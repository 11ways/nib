/** Runs a job's prompts through one Claude Code or OpenCode v2 process in an isolated setup and reads usage and tool calls from its output. */
import { Database } from "bun:sqlite"
import { randomUUID } from "node:crypto"
import { access, mkdir, readFile, writeFile } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import type { FileSink, Subprocess } from "bun"
import { REPO } from "./fixture"

export type Harness = "claude" | "opencode"
export type Mode = "builtin" | "nib"
/**
 * `variant` is Claude Code's --effort or OpenCode's model variant, unset for the harness default; `shell` keeps the shell tool, which the benchmark
 * otherwise turns off; `decider` is the arm (see deciderArm) nibash cuts long output with, which matters only in nib mode with a shell.
 */
export type Model = { harness: Harness; model: string; label: string; variant?: string; shell?: boolean; decider?: string }
export type Usage = {
  inputTokens: number
  outputTokens: number
  reasoningTokens: number
  cacheRead: number
  cacheWrite: number
  cost: number
  turns: number
  toolCalls: number
  toolErrors: number
  tools: Record<string, number>
  /** Each failed tool call's tool and message, cut at FAILURE_CHARS. */
  failures: { tool: string; message: string }[]
  /** nib's notes in tool calls that worked: a form it accepted and translated (corrected), or something it flagged (warned). */
  notes: Note[]
  /** Input tokens, cache included, of the first model call: system prompt, tool definitions and prompt, before any tool ran. */
  startContextTokens: number
  /** Input tokens, cache included, of the last model call: the context the run ended with. A later prompt's calls carry the earlier prompts, so steps are not added up. */
  contextTokens: number
  /** Characters of tool arguments the model wrote and of results it got back, per tool; characters count the same for every model, tokens do not. */
  toolChars: Record<string, ToolChars>
  /** Tool calls whose arguments name a saved nibash output log (OUTPUT_LOG): follow-up reads of output that was cut. */
  logReads: number
}
/** `cut` notes are nibash's notes on output it cut and the footer of rows it cut, which no call corrected or warned about. */
export type Note = { kind: "corrected" | "warned" | "cut"; tool: string; message: string }
export type ToolChars = { written: number; returned: number }
/** One prompt's output; `error` is set when the harness or provider failed, as opposed to the model doing the task wrong. */
export type Session = Usage & { sessionId?: string; error?: string }
/** A job's limits: each prompt gets `timeout` milliseconds and at most `maxToolCalls` tool calls. */
export type Budget = { timeout: number; maxToolCalls: number }

/**
 * Bumped whenever the harness setup below changes what a model sees, so older results stop counting as cached; 2 turned the harness's own readers off
 * in nib mode; 3 sandboxed both modes to the work directory: nib through NIB_SANDBOX, Claude Code's own tools through its permissions; 4 kept one
 * harness process, and so one nib server, through all prompts of a job instead of resuming the conversation in a new process per prompt; 5 ran nib
 * as its Go binary, and in the shell series gave nib mode nibash in place of the harness's shell; 6 moved OpenCode to v2: its permission names,
 * nib's tools offered directly instead of through code mode, the model's first call made only once nib's server is connected, and HOME isolated;
 * 7 gave nibash a decider, named explicitly in NIB_DECIDER (keywords by default), with notes on cut output by log line and long rows cut; 8
 * offered nib's skim tool beside nibash.
 */
export const SETUP = 8
const BINARY = path.join(import.meta.dir, ".cache", "nib")
let built: Promise<string> | undefined

/** The nib binary the runs start, built from the working tree once per benchmark process. */
export function nibBinary() {
  return built ??= (async () => {
    const child = Bun.spawn(["go", "build", "-o", BINARY, "./cmd/nib"], { cwd: REPO, stdout: "pipe", stderr: "pipe" })
    const [exit, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()])
    if (exit !== 0) throw new Error(`go build failed: ${stderr.trim()}`)
    return BINARY
  })()
}

/** The OpenCode v2 release the benchmark runs; it lives in bench/.cache, apart from any OpenCode on PATH, so a setup always means one harness. */
export const OPENCODE_VERSION = "2.0.30"
let opencodeInstalled: Promise<string> | undefined

/**
 * The OpenCode binary the runs start: NIB_BENCH_OPENCODE when set (a stand-in or a local build), else OPENCODE_VERSION, installed on first use.
 * @throws Error when npm cannot install that version
 */
export function opencodeBinary() {
  const chosen = process.env.NIB_BENCH_OPENCODE
  if (chosen) return Promise.resolve(chosen)
  return opencodeInstalled ??= (async () => {
    const prefix = path.join(SHARED, `opencode-${OPENCODE_VERSION}`)
    const binary = path.join(prefix, "node_modules", ".bin", "opencode")
    const wanted = `opencode v${OPENCODE_VERSION}`
    if (await commandVersion(binary) === wanted) return binary
    // npm, unlike bun, runs the package's postinstall, which puts the binary for this platform in place.
    const child = Bun.spawn(["npm", "install", "--prefix", prefix, "--no-save", "--no-package-lock", "--no-audit", "--no-fund", `@opencode/cli@${OPENCODE_VERSION}`], { stdout: "pipe", stderr: "pipe" })
    const [exit, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()])
    if (exit !== 0) throw new Error(`npm install @opencode/cli@${OPENCODE_VERSION} failed: ${tail(stderr)}`)
    const installed = await commandVersion(binary)
    if (installed !== wanted) throw new Error(`${binary} reports ${installed}, not ${wanted}`)
    return binary
  })()
}
const EDIT_TOOLS = ["Edit", "Write", "MultiEdit", "NotebookEdit"]
// Claude Code refuses reads outside the work directory, and an Edit rule scoped to it covers every editing tool, so built-in edits elsewhere need a prompt that -p denies.
const CLAUDE_SANDBOX = JSON.stringify({ permissions: { blockReadsOutsideWorkingDirectories: true } })
const CLAUDE_EDITS = ["Edit(./**)"]

/** nib's decider kinds as NIB_DECIDER spells them, its default kind and its pinned OpenRouter model; internal/decider is their home, and a test holds these to it. */
export const DECIDER_KINDS = ["fixed", "keywords", "openrouter"] as const
export type DeciderKind = (typeof DECIDER_KINDS)[number]
export const DEFAULT_DECIDER: DeciderKind = "keywords"
export const DECIDER_MODEL = "typesafe/jev-1.14"

/**
 * A decider arm as runs record it: a kind, and for openrouter the model it asks, so `openrouter` is `openrouter:typesafe/jev-1.14`.
 * @throws Error for an unknown kind, or a model given to a kind that asks none
 */
export function deciderArm(name: string) {
  const [kind, ...rest] = name.trim().split(":")
  const model = rest.join(":")
  if (!DECIDER_KINDS.includes(kind as DeciderKind)) throw new Error(`unknown decider ${JSON.stringify(name)}; use one of ${DECIDER_KINDS.join(", ")}`)
  if (kind === "openrouter") return `openrouter:${model || DECIDER_MODEL}`
  if (rest.length) throw new Error(`the ${kind} decider asks no model, so ${JSON.stringify(name)} means nothing`)
  return kind
}

/** The decider arm a job's nib cuts output with: only nibash cuts, so only nib mode in the shell series has one, the default unless the model names another. */
export function deciderOf(model: Model, mode: Mode) {
  return mode === "nib" && model.shell ? deciderArm(model.decider ?? DEFAULT_DECIDER) : undefined
}

/**
 * The OpenRouter key the openrouter decider pays with: OPENROUTER_API_KEY, else OpenCode's OpenRouter login, as OpenCode runs find it.
 * @throws Error when there is neither, or the login is not a key
 */
export async function openrouterKey() {
  if (process.env.OPENROUTER_API_KEY) return process.env.OPENROUTER_API_KEY
  const login = await providerLogin("openrouter", 0)
  if (login?.value.type !== "key" || typeof login.value.key !== "string") throw new Error("the openrouter decider needs OPENROUTER_API_KEY or an OpenRouter API key login in OpenCode")
  return login.value.key as string
}

/**
 * nib's environment in a run. OpenCode's external_directory permission, off below, keeps its own tools in the work directory; nib keeps itself
 * there, named explicitly. In the shell series nib mode has nibash as its shell, in place of the harness's; otherwise nib offers no shell, so both
 * modes have none. The openrouter decider is allowed under the sandbox explicitly, and its answers are cached across runs, so the same output
 * is not paid for twice.
 */
export async function nibEnvironment(work: string, model: Model, mode: Mode): Promise<Record<string, string>> {
  const env: Record<string, string> = { NIB_SANDBOX: "1", NIB_DIRECTORY: work, NIB_SHELL: model.shell ? "1" : "0" }
  const arm = deciderOf(model, mode)
  if (!arm) return env
  const [kind, ...rest] = arm.split(":")
  env.NIB_DECIDER = kind!
  if (kind === "openrouter") Object.assign(env, { NIB_DECIDER_MODEL: rest.join(":"), NIB_DECIDER_SANDBOX: "1", NIB_DECIDER_KEY: await openrouterKey(), NIB_DECIDER_CACHE: path.join(SHARED, "decider") })
  return env
}

// nib mode turns all of the harness's own file tools off, readers too, so a model cannot read a file without a tag and then edit it through nib.
const CLAUDE_FILE_TOOLS = [...EDIT_TOOLS, "Read", "Grep", "Glob"]
// OpenCode v2 gates write and patch by the edit permission, and has no list tool.
const OPENCODE_FILE_TOOLS = ["read", "grep", "glob", "edit"]
// Shell, web and subagent tools are off in both modes and both harnesses, so only the file tools differ. OpenCode's execute is its code mode,
// which runs scripts that call tools and fetch URLs, so it is off too; with it go the opencode_* tools that live only inside it.
const CLAUDE_OFF = ["Bash", "Task", "Agent", "WebFetch", "WebSearch"]
const OPENCODE_OFF = ["shell", "subagent", "webfetch", "websearch", "question", "skill", "execute", "external_directory"]
export const SHARED = path.join(import.meta.dir, ".cache")
const FAILURE_CHARS = 300

/**
 * How nib words its notes, each on a line of its own: these patterns must match what nib prints, and a test makes nib print every one of them.
 * They are anchored at line starts, so file content in read and grep rows, which starts with a line number, never matches.
 * Plain reads drop the numbers, so there a file line worded like a note would count; the fixture has no such line.
 */
export const NOTE_PATTERNS: { kind: Note["kind"]; pattern: RegExp }[] = [
  { kind: "corrected", pattern: /^\[read takes paths, like .*\]$/gm },
  { kind: "corrected", pattern: /^\[grep takes paths, a list like .*\]$/gm },
  { kind: "corrected", pattern: /^\[\S+ \d+ is over its limit of \d+, so \d+ was used\]$/gm },
  { kind: "corrected", pattern: /^The - rows matched the file, so the edit was applied;.*$/gm },
  { kind: "corrected", pattern: /^\[\w+ was a string holding a JSON list;.*\]$/gm },
  { kind: "corrected", pattern: /^\[the patch came as one line with escaped line breaks.*\]$/gm },
  { kind: "corrected", pattern: /^\[mode \S+ was read as \S+, nib's name for it\]$/gm },
  { kind: "corrected", pattern: /^PUT \S+ was read as PUT \S+; a range is written N\.=M, with a dot\.$/gm },
  { kind: "warned", pattern: /^Recovered (by remapping stale line numbers|from a stale tag).*$/gm },
  { kind: "warned", pattern: /^Tag #[0-9A-F]{4} was never issued for this file;.*$/gm },
  { kind: "warned", pattern: /^Applied the end-of-file insert despite a stale tag.*$/gm },
  { kind: "warned", pattern: /^Lines \d+ and \d+ now nearly match:.*$/gm },
  { kind: "warned", pattern: /^Brackets no longer balance:.*$/gm },
  { kind: "warned", pattern: /^The edit of lines? [\d-]+ replaced lines \d+-\d+:.*$/gm },
  { kind: "warned", pattern: /^The edit of lines \d+-\d+ dropped line \d+, .*$/gm },
  // A path that names nothing, noted while the call serves its other paths.
  { kind: "warned", pattern: /^\[.* does not exist.*\]$/gm },
  // nibash's gaps in cut output and the closing note that ends it (log lines, or rows for output without log lines), and its footer for long rows,
  // whose start tells it from the footer read and grep give long file lines.
  { kind: "cut", pattern: /^\[nibash: (?:[\d,]+ rows shown of [\d,]+ log lines|(?:log lines? \d+(?:-\d+)?|[\d,]+ rows) left out)[^\n]*\]$/gm },
  { kind: "cut", pattern: /^\[cut rows: N is the row's line in the whole output[^\n]*\]$/gm },
]

/** A saved nibash output log as its notes name it, PID-output-N.log in nib's log directory; job logs are left out, since reading one is how a background job's output is seen at all. */
export const OUTPUT_LOG = /\/nib(?:-\d+)?\/\d+-output-\d+\.log\b/

/** Whether a tool call's arguments name a saved output log, so the call follows up on output that was cut. */
const namesLog = (input: unknown) => OUTPUT_LOG.test(JSON.stringify(input ?? {}))

/** The notes in one nib tool response; responses of other tools never carry them. */
export function classifyNotes(tool: string, text: string): Note[] {
  if (!tool.startsWith("nib.")) return []
  return NOTE_PATTERNS.flatMap(({ kind, pattern }) => [...text.matchAll(pattern)].map((match) => ({ kind, tool, message: match[0].slice(0, FAILURE_CHARS) })))
}

export function emptyUsage(): Usage {
  return { inputTokens: 0, outputTokens: 0, reasoningTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0, turns: 0, toolCalls: 0, toolErrors: 0, tools: {}, failures: [], notes: [], startContextTokens: 0, contextTokens: 0, toolChars: {}, logReads: 0 }
}

function addChars(usage: Usage, tool: string, kind: keyof ToolChars, count: number) {
  const entry = usage.toolChars[tool] ??= { written: 0, returned: 0 }
  entry[kind] += count
}

export function addUsage(total: Usage, part: Usage) {
  for (const key of ["inputTokens", "outputTokens", "reasoningTokens", "cacheRead", "cacheWrite", "cost", "turns", "toolCalls", "toolErrors", "logReads"] as const) total[key] += part[key] ?? 0
  for (const [name, count] of Object.entries(part.tools)) total.tools[name] = (total.tools[name] ?? 0) + count
  total.failures.push(...part.failures)
  total.notes.push(...part.notes ?? [])
  total.startContextTokens ||= part.startContextTokens ?? 0
  if (part.contextTokens) total.contextTokens = part.contextTokens
  for (const [name, chars] of Object.entries(part.toolChars ?? {})) {
    addChars(total, name, "written", chars.written)
    addChars(total, name, "returned", chars.returned)
  }
}

/** One name per tool across harnesses: built-ins lowercased (read, edit), MCP tools as server.tool (nib.read). */
export function toolName(harness: Harness, name: string) {
  const mcp = harness === "claude" ? /^mcp__([^_]+)__(.+)$/.exec(name) : /^(nib)_(.+)$/.exec(name)
  return mcp ? `${mcp[1]}.${mcp[2]}` : name.toLowerCase()
}

const json = (line: string): any => {
  try { return JSON.parse(line) } catch { return undefined }
}

/**
 * Claude Code's stream-json for one prompt: assistant messages with each call's usage and tool_use blocks, tool results (matched to their call by id),
 * and a closing result with that prompt's tokens. Its total_cost_usd counts the whole process so far, so `spent` is what the earlier prompts cost.
 */
export function parseClaude(stdout: string, spent = 0): Session {
  const session: Session = emptyUsage()
  const names = new Map<string, string>()
  // Claude Code sends one assistant event per content block, so a model call is one message id; its num_turns counts parallel tool calls apart.
  const calls = new Set<string>()
  let finished = false
  for (const event of stdout.split("\n").map(json)) {
    if (!event) continue
    if (event.session_id) session.sessionId = event.session_id
    if (event.type === "assistant") {
      if (event.message?.id) calls.add(event.message.id)
      const usage = event.message?.usage
      if (usage) session.contextTokens = (usage.input_tokens ?? 0) + (usage.cache_read_input_tokens ?? 0) + (usage.cache_creation_input_tokens ?? 0)
      session.startContextTokens ||= session.contextTokens
      for (const part of event.message?.content ?? []) {
        if (part.type !== "tool_use") continue
        session.toolCalls++
        const name = toolName("claude", part.name)
        names.set(part.id, name)
        session.tools[name] = (session.tools[name] ?? 0) + 1
        addChars(session, name, "written", JSON.stringify(part.input ?? {}).length)
        if (namesLog(part.input)) session.logReads++
      }
    } else if (event.type === "user" && Array.isArray(event.message?.content)) {
      for (const part of event.message.content) {
        if (part.type !== "tool_result") continue
        const tool = names.get(part.tool_use_id) ?? "unknown"
        const text = typeof part.content === "string" ? part.content : (part.content ?? []).map((item: { text?: string }) => item.text ?? "").join("\n")
        addChars(session, tool, "returned", text.length)
        if (!part.is_error) {
          session.notes.push(...classifyNotes(tool, text))
          continue
        }
        session.toolErrors++
        session.failures.push({ tool, message: text.slice(0, FAILURE_CHARS) })
      }
    } else if (event.type === "result") {
      finished = true
      session.turns = event.num_turns ?? 0
      session.cost = event.total_cost_usd === undefined ? 0 : event.total_cost_usd - spent
      session.inputTokens = event.usage?.input_tokens ?? 0
      session.outputTokens = event.usage?.output_tokens ?? 0
      session.cacheRead = event.usage?.cache_read_input_tokens ?? 0
      session.cacheWrite = event.usage?.cache_creation_input_tokens ?? 0
      if (event.is_error || event.subtype !== "success") session.error = `${event.subtype ?? "error"}: ${String(event.result ?? "").slice(0, 500)}`
    }
  }
  if (calls.size) session.turns = calls.size
  if (!finished) session.error ??= "no result event"
  return session
}

/** The line a prompt's OpenCode output ends with: that prompt's messages of the session, read from the server; see opencodeSession. */
export const MESSAGES = "nib-bench-messages"

/** The calls an OpenCode code mode script made, as server.tool names, from its execute call's metadata, which the run output nests once more. */
function codeModeCalls(metadata: any): { tool: string; status?: string }[] {
  const calls = metadata?.toolCalls ?? metadata?.metadata?.toolCalls
  return Array.isArray(calls) ? calls.filter((call) => typeof call?.tool === "string") : []
}

/** One model call's tokens and cost; a call cut off before it reported them still counts as a turn. */
function addOpencodeStep(session: Session, tokens: any, cost: unknown) {
  session.turns++
  session.cost += Number(cost) || 0
  if (!tokens) return
  session.inputTokens += tokens.input ?? 0
  session.outputTokens += tokens.output ?? 0
  session.reasoningTokens += tokens.reasoning ?? 0
  session.cacheRead += tokens.cache?.read ?? 0
  session.cacheWrite += tokens.cache?.write ?? 0
  session.contextTokens = (tokens.input ?? 0) + (tokens.cache?.read ?? 0) + (tokens.cache?.write ?? 0)
  session.startContextTokens ||= session.contextTokens
}

/**
 * One OpenCode tool call. A code mode execute call counts as the calls its script made, each under its own tool, while its characters stay
 * execute's and its notes are not read, since one output serves all of its calls.
 */
function addOpencodeCall(session: Session, call: { tool: string; failed: boolean; input: unknown; result: string; metadata?: unknown }) {
  const name = toolName("opencode", call.tool)
  addChars(session, name, "written", JSON.stringify(call.input ?? {}).length)
  if (namesLog(call.input)) session.logReads++
  addChars(session, name, "returned", call.result.length)
  const ran = name === "execute" ? codeModeCalls(call.metadata).map((inner) => ({ tool: toolName("opencode", inner.tool), failed: inner.status === "error" })) : []
  for (const { tool, failed } of ran.length ? ran : [{ tool: name, failed: call.failed }]) {
    session.toolCalls++
    session.tools[tool] = (session.tools[tool] ?? 0) + 1
    if (!failed) continue
    session.toolErrors++
    session.failures.push({ tool, message: call.result.slice(0, FAILURE_CHARS) })
  }
  if (!call.failed && !ran.length) session.notes.push(...classifyNotes(name, call.result))
}

/**
 * One prompt's OpenCode output: run --format json events, then the MESSAGES line, whose assistant messages are the prompt's model calls with
 * their tokens, cost and tool calls. OpenCode 2's client stops listening once the session is idle, so its step_finish and tool_use events can
 * miss the last calls; they count only without that line, as in OpenCode 1 transcripts. Error events count either way.
 */
export function parseOpencode(stdout: string): Session {
  const session: Session = emptyUsage()
  const events = stdout.split("\n").map(json).filter(Boolean)
  const messages = events.findLast((event) => event.type === MESSAGES)?.messages
  for (const event of events) {
    if (event.sessionID) session.sessionId = event.sessionID
    const part = event.part
    if (event.type === "error") {
      const error = event.error ?? {}
      session.error = String(error.data?.message ?? error.message ?? error.name ?? "error").slice(0, 500)
    } else if (messages) continue
    else if (event.type === "step_finish") addOpencodeStep(session, part?.tokens, part?.cost)
    else if (event.type === "tool_use") {
      const failed = part?.state?.status === "error"
      addOpencodeCall(session, { tool: part?.tool ?? "unknown", failed, input: part?.state?.input, result: String((failed ? part.state.error : part?.state?.output) ?? ""), metadata: part?.state?.metadata })
    }
  }
  for (const message of Array.isArray(messages) ? messages : []) {
    if (message?.type !== "assistant") continue
    addOpencodeStep(session, message.tokens, message.cost)
    for (const item of message.content ?? []) {
      if (item?.type !== "tool") continue
      const state = item.state ?? {}
      const failed = state.status === "error"
      const text = (state.content ?? []).filter((content: any) => content?.type === "text").map((content: any) => content.text).join("\n")
      addOpencodeCall(session, { tool: item.name ?? "unknown", failed, input: state.input, result: failed ? String(state.error?.message ?? text) : text, metadata: state.metadata })
    }
  }
  if (!session.turns) session.error ??= "no model call finished"
  return session
}

// Only these reach a harness: what it needs to run, find programs, reach the network and keep its login. Inherited overrides such as
// OPENCODE_PERMISSION, OPENCODE_CONFIG_DIR or NIB_DIRECTORY, a surrounding Claude Code session's variables, and API keys that would bill per token
// instead of a subscription login never do; whatever else a harness needs is set explicitly.
const PASSED = /^(PATH|HOME|USER|LOGNAME|SHELL|LANG|LC_[A-Z]+|TERM|TZ|TMPDIR|(HTTPS?|NO|ALL)_PROXY|(https?|no|all)_proxy|SSL_CERT_(FILE|DIR)|NODE_EXTRA_CA_CERTS|CLAUDE_CONFIG_DIR)$/

/** The variables a harness inherits from this process, see PASSED. */
export function harnessEnvironment() {
  return Object.fromEntries(Object.entries(process.env).filter((entry): entry is [string, string] => entry[1] !== undefined && PASSED.test(entry[0])))
}

/** OpenCode's real data directory, where its logins live; resolved on use, so a later XDG_DATA_HOME counts. */
function opencodeData() {
  return path.join(process.env.XDG_DATA_HOME ?? path.join(os.homedir(), ".local/share"), "opencode")
}

/** A refresh inside a run would rotate an OAuth login and leave OpenCode's own copy stale, so a login must outlive the run by this much. */
const LOGIN_MARGIN = 15 * 60_000

/** A login as OpenCode 2's credential API takes it. */
export type Login = { integrationID: string; label: string; value: Record<string, any>; activate: true }

// The migration that takes auth.json into OpenCode 2's database the first time OpenCode 2 opens it; until then auth.json holds the logins.
const LEGACY_IMPORT = "20260805200742_import_legacy_credentials"
// The sign-in method that migration gives an auth.json OAuth login, by provider; the others get "oauth".
const LEGACY_METHODS: Record<string, string> = { openai: "chatgpt-browser", "github-copilot": "device", opencode: "device", xai: "device" }

/** OpenCode 2's credentials of `provider`, the active one first, or undefined while OpenCode 2 has not yet opened its database. */
function storedLogins(provider: string): { label: string; value: Record<string, any> }[] | undefined {
  let database: Database
  try {
    database = new Database(path.join(opencodeData(), "opencode.db"), { readonly: true })
  } catch {
    return undefined
  }
  try {
    if (!database.query("SELECT 1 FROM migration WHERE id = ?").get(LEGACY_IMPORT)) return undefined
    const rows = database.query("SELECT label, value FROM credential WHERE integration_id = ? ORDER BY active DESC, time_updated DESC").all(provider) as { label: string; value: string }[]
    return rows.map((row) => ({ label: row.label, value: JSON.parse(row.value) }))
  } catch {
    return undefined
  } finally {
    database.close()
  }
}

/** An auth.json entry as that migration turns it into a credential, or undefined for one it skips. */
function legacyLogin(provider: string, entry: any): { label: string; value: Record<string, any> } | undefined {
  if (entry?.type === "api") return { label: "API key", value: { type: "key", key: entry.key, ...entry.metadata ? { metadata: entry.metadata } : {} } }
  if (entry?.type === "wellknown") return { label: "API key", value: { type: "key", key: entry.token } }
  if (entry?.type !== "oauth") return undefined
  const metadata = { ...entry.accountId ? { accountID: entry.accountId } : {}, ...entry.enterpriseUrl ? { enterpriseUrl: entry.enterpriseUrl } : {} }
  const value = { type: "oauth", methodID: LEGACY_METHODS[provider] ?? "oauth", refresh: entry.refresh, access: entry.access, expires: entry.expires }
  return { label: "OAuth", value: Object.keys(metadata).length ? { ...value, metadata } : value }
}

/**
 * The saved login of one provider, so no other provider's login is passed: OpenCode 2's own once it has opened OpenCode's database, else the
 * auth.json entry it would take in then. Undefined for OpenCode's own models without a login, which then offer only their free models.
 * @throws Error when OpenCode has no such login, or an OAuth login would expire before a run of `duration` milliseconds ends
 */
export async function providerLogin(provider: string, duration: number): Promise<Login | undefined> {
  const stored = storedLogins(provider)
  const saved = stored ? stored[0] : legacyLogin(provider, JSON.parse(await readFile(path.join(opencodeData(), "auth.json"), "utf8").catch(() => "{}"))[provider])
  if (!saved) {
    if (provider === "opencode") return undefined
    throw new Error(`OpenCode has no ${provider} login; run opencode auth login`)
  }
  const { type, expires } = saved.value
  if (type !== "oauth" && type !== "key") throw new Error(`OpenCode's ${provider} login is of type ${type}, which a run cannot take along`)
  if (type === "oauth" && Number(expires) - Date.now() < duration + LOGIN_MARGIN) {
    throw new Error(`OpenCode's ${provider} login expires at ${new Date(Number(expires)).toISOString()}, before this run could end; use OpenCode once so it refreshes the login, then run again`)
  }
  return { integrationID: provider, label: saved.label, value: saved.value, activate: true }
}

/** The first line `command --version` prints, or "unknown" when it does not run. */
async function commandVersion(command: string) {
  try {
    const child = Bun.spawn([command, "--version"], { stdout: "pipe", stderr: "ignore", env: harnessEnvironment() })
    return (await new Response(child.stdout).text()).trim().split("\n")[0] || "unknown"
  } catch {
    return "unknown"
  }
}

const versions = new Map<Harness, Promise<string>>()

/** The version of a harness the runs use, or "unknown" when it does not run. */
export function harnessVersion(harness: Harness) {
  let version = versions.get(harness)
  if (!version) {
    version = (harness === "claude" ? Promise.resolve("claude") : opencodeBinary()).then(commandVersion, () => "unknown")
    versions.set(harness, version)
  }
  return version
}

/** A harness command and its whole environment. */
export type Setup = { args: string[]; env: Record<string, string> }
/** OpenCode's server and the login it gets through its API once it listens. */
export type OpencodeSetup = Setup & { login?: Login }

/** Claude Code's one process for a whole job, reading each prompt from stdin as a stream-json user message; `home` is a scratch directory outside the agent's project. */
export async function claudeSetup(model: Model, mode: Mode, work: string, home: string): Promise<Setup> {
  const nib = mode === "nib"
  await mkdir(home, { recursive: true })
  const config = path.join(home, "mcp.json")
  await writeFile(config, JSON.stringify({ mcpServers: nib ? { nib: { type: "stdio", command: await nibBinary(), args: [], env: await nibEnvironment(work, model, mode), alwaysLoad: true } } : {} }))
  // User settings and CLAUDE.md are skipped so personal rules (such as a deny of Edit) cannot change either setup.
  const harnessShell = model.shell && !nib
  const off = harnessShell ? CLAUDE_OFF.filter((tool) => tool !== "Bash") : CLAUDE_OFF
  const allowed = [...nib ? ["mcp__nib__*"] : CLAUDE_EDITS, ...harnessShell ? ["Bash"] : []]
  const args = ["claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--model", model.model, "--mcp-config", config, "--strict-mcp-config",
    "--setting-sources", "project,local", "--settings", CLAUDE_SANDBOX, "--no-session-persistence", "--disallowedTools", ...off, ...nib ? CLAUDE_FILE_TOOLS : [], "--allowedTools", ...allowed]
  if (model.variant) args.push("--effort", model.variant)
  return { args, env: { ...harnessEnvironment(), PWD: work } }
}

/**
 * OpenCode's server for a whole job, on a free port behind a random password; each prompt's client (see opencodePrompt) gets the same environment.
 * `home` is the run's scratch directory, outside the work directory; `duration` is how long the whole job may take, which a login must outlive.
 */
export async function opencodeSetup(model: Model, mode: Mode, work: string, home: string, duration: number): Promise<OpencodeSetup> {
  const nib = mode === "nib"
  await mkdir(home, { recursive: true })
  const harnessShell = model.shell && !nib
  const denied = [...OPENCODE_OFF.filter((tool) => !(harnessShell && tool === "shell")), ...nib ? OPENCODE_FILE_TOOLS : []]
  // The last matching rule wins, so the denials after the allow-all do; a tool whose permission is denied outright is not offered at all.
  const permissions = [{ action: "*", resource: "*", effect: "allow" }, ...denied.map((action) => ({ action, resource: "*", effect: "deny" }))]
  // OpenCode 2 offers an MCP server's tools only inside code mode scripts unless codemode is false. nib keeps the real HOME, so its sandbox
  // still hides the user's files from the programs nibash runs.
  const server = { type: "local", command: [await nibBinary()], environment: { ...await nibEnvironment(work, model, mode), HOME: os.homedir() }, codemode: false }
  const config = { $schema: "https://opencode.ai/config.json", share: "disabled", permissions, mcp: nib ? { servers: { nib: server } } : {} }
  // OpenCode takes its project directory from PWD rather than the process's own directory, so an inherited PWD would point it at the caller's project.
  const env: Record<string, string> = { ...harnessEnvironment(), PWD: work, OPENCODE_SERVER_PASSWORD: randomUUID() }
  const provider = model.model.split("/")[0]!
  let login: Login | undefined
  if (provider === "openrouter" && process.env.OPENROUTER_API_KEY) env.OPENROUTER_API_KEY = process.env.OPENROUTER_API_KEY
  else login = await providerLogin(provider, duration)
  // OpenCode reads config, agents, skills and instructions from HOME (~/.agents, ~/.claude) and the XDG directories, so the run gets its own,
  // empty ones; its database, and with it the login, starts empty in the run's data directory. Only the cache (model lists and such) is shared.
  Object.assign(env, {
    HOME: home,
    XDG_CONFIG_HOME: path.join(home, "config"),
    XDG_CACHE_HOME: path.join(SHARED, "opencode-v2-cache"),
    XDG_DATA_HOME: path.join(home, "data"),
    XDG_STATE_HOME: path.join(home, "state"),
    OPENCODE_CONFIG_CONTENT: JSON.stringify(config),
    OPENCODE_DISABLE_CLAUDE_CODE: "1",
    // An opencode.json the model wrote in the work directory would merge before the inline config and could reorder its permissions for the next step.
    OPENCODE_DISABLE_PROJECT_CONFIG: "1",
    OPENCODE_DISABLE_AUTOUPDATE: "1",
  })
  return { args: [await opencodeBinary(), "serve", "--port", "0"], env, login }
}

/** The client that sends one prompt to a job's OpenCode server at `url` from the work directory it runs in, continuing `session` after the first prompt. */
export function opencodePrompt(binary: string, model: Model, url: string, prompt: string, session?: string) {
  const args = [binary, "run", "--server", url, "--format", "json", "--model", model.variant ? `${model.model}#${model.variant}` : model.model, "--auto"]
  if (session) args.push("--session", session)
  args.push(prompt)
  return args
}

// A prompt past this many tool calls is stopped and counts as failed: a model stuck repeating a call would otherwise go on paying for each one.
// Prompts that do not loop stay far below it; the busiest seen made 103 calls, the loops 626 and 1694.
export const MAX_TOOL_CALLS = 150
export const DEFAULT_BUDGET: Budget = { timeout: 900_000, maxToolCalls: MAX_TOOL_CALLS }

/** How many tool calls one line of a harness's output starts; an OpenCode code mode call starts the calls its script made. */
export function toolCallsIn(harness: Harness, line: string) {
  const event = json(line)
  if (harness === "claude") return event?.type === "assistant" ? (event.message?.content ?? []).filter((part: { type?: string }) => part.type === "tool_use").length : 0
  if (event?.type !== "tool_use") return 0
  return event.part?.tool === "execute" ? Math.max(1, codeModeCalls(event.part?.state?.metadata).length) : 1
}

/** `promise`'s value, or "timedOut" once `ms` milliseconds pass first. */
export function within<T>(promise: Promise<T>, ms: number): Promise<T | "timedOut"> {
  let timer: ReturnType<typeof setTimeout> | undefined
  const expiry = new Promise<"timedOut">((resolve) => { timer = setTimeout(() => resolve("timedOut"), ms) })
  return Promise.race([promise, expiry]).finally(() => clearTimeout(timer))
}

/** Calls `line` with each line of `reader` as it streams in, however the chunks split them; a cancel ends it as the end of the stream does. */
export async function readLines(reader: ReadableStreamDefaultReader<Uint8Array>, line: (text: string) => void) {
  const decoder = new TextDecoder()
  let pending = ""
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    const parts = (pending + decoder.decode(value, { stream: true })).split("\n")
    pending = parts.pop()!
    for (const part of parts) line(part)
  }
  pending += decoder.decode()
  if (pending) line(pending)
}

// A stopped harness gets this long after SIGTERM before its whole process group is killed, and its output this long after its exit before it is cut off.
const STOP_GRACE = 5_000
const DRAIN = 2_000

/** A harness process in a process group of its own, its stdout read line by line; stopping it reaches everything it started, even what ignores SIGTERM. */
export class Group {
  readonly child: Subprocess<"pipe" | "ignore", "pipe", "pipe">
  /** Settles once stdout ended or was cut off. */
  readonly output: Promise<void>
  readonly #errors: Promise<void>
  readonly #readers: ReadableStreamDefaultReader<Uint8Array>[]
  #stderr = ""

  constructor(args: string[], options: { cwd: string; env: Record<string, string>; stdin?: boolean }, line: (text: string) => void) {
    this.child = Bun.spawn(args, { cwd: options.cwd, env: options.env, stdin: options.stdin ? "pipe" : "ignore", stdout: "pipe", stderr: "pipe", detached: true })
    const [out, err] = [this.child.stdout.getReader(), this.child.stderr.getReader()]
    this.#readers = [out, err]
    this.output = readLines(out, line).catch(() => {})
    this.#errors = readLines(err, (text) => { this.#stderr += `${text}\n` }).catch(() => {})
  }

  get stderr() {
    return this.#stderr
  }

  /** Sends `text` to stdin; false once the process can no longer take it. */
  write(text: string) {
    try {
      const stdin = this.child.stdin as FileSink
      stdin.write(text)
      stdin.flush()
      return true
    } catch {
      return false
    }
  }

  /**
   * SIGTERM to the whole group, SIGKILL once STOP_GRACE ms pass with any of it left, then the output, which gets at most DRAIN ms more; once it
   * returns, nothing of the group is left, zombies included, unless their reaping took longer than STOP_GRACE.
   */
  async stop() {
    this.#signal("SIGTERM")
    await this.#vanish()
    if (this.#alive()) {
      this.#signal("SIGKILL")
      // Killed members stay in the group as zombies until their new parent reaps them.
      await this.#vanish()
    }
    await this.child.exited
    await within(Promise.all([this.output, this.#errors]), DRAIN)
    await Promise.all(this.#readers.map((reader) => reader.cancel().catch(() => {})))
  }

  /** Waits up to STOP_GRACE ms for the group to be gone. */
  async #vanish() {
    const deadline = Date.now() + STOP_GRACE
    while (this.#alive() && Date.now() < deadline) await Bun.sleep(50)
  }

  /** Closes stdin, gives the process STOP_GRACE ms to exit by itself, then stops whatever is left of the group. */
  async finish() {
    try {
      await this.child.stdin?.end()
    } catch {
      // The process is gone already.
    }
    await within(this.child.exited, STOP_GRACE)
    await this.stop()
  }

  #signal(signal: NodeJS.Signals) {
    try {
      process.kill(-this.child.pid, signal)
    } catch {
      // The group is gone.
    }
  }

  /** Whether any process of the group is left; the leader counts until it is reaped. */
  #alive() {
    try {
      process.kill(-this.child.pid, 0)
      return true
    } catch {
      return false
    }
  }
}

/** A prompt's output, parsed, and whether a limit stopped it. */
export type StepResult = { stdout: string; session: Session; timedOut: boolean; capped: boolean }

/** The harness of one job: every prompt goes to the same harness process and nib server, as a user's follow-ups would. */
export type HarnessSession = {
  /** Sends one prompt and resolves once the model is done with it; past `timeout` ms or MAX_TOOL_CALLS tool calls the whole harness is stopped. */
  prompt(text: string, timeout: number): Promise<StepResult>
  /** Ends the harness and everything it started, and returns what it wrote to stderr. */
  close(): Promise<string>
}

/** The last lines of a harness's stderr, for an error message. */
const tail = (text: string) => text.trim().split("\n").slice(-3).join(" ").slice(0, 500)

/** Starts the harness for a job of `steps` prompts in `work`, each allowed `timeout` ms. */
export function openSession(model: Model, mode: Mode, work: string, home: string, timeout: number, steps: number): Promise<HarnessSession> {
  return model.harness === "claude" ? claudeSession(model, mode, work, home) : opencodeSession(model, mode, work, home, timeout, steps)
}

type Ending = "finished" | "exited" | "capped" | "timedOut"

async function claudeSession(model: Model, mode: Mode, work: string, home: string): Promise<HarnessSession> {
  const { args, env } = await claudeSetup(model, mode, work, home)
  // Lines that come between prompts, such as a session's opening events, go to the next prompt's output.
  const collector = () => ({ lines: [] as string[], calls: 0, end: (_: Ending) => {} })
  let step = collector()
  let exited = false
  const group = new Group(args, { cwd: work, env, stdin: true }, (line) => {
    step.lines.push(line)
    step.calls += toolCallsIn("claude", line)
    if (step.calls > MAX_TOOL_CALLS) step.end("capped")
    else if (json(line)?.type === "result") step.end("finished")
  })
  void group.output.then(() => {
    exited = true
    step.end("exited")
  })
  let spent = 0
  return {
    async prompt(text, timeout) {
      const ended = new Promise<Ending>((resolve) => { step = { lines: step.lines, calls: 0, end: resolve } })
      const message = JSON.stringify({ type: "user", message: { role: "user", content: text } })
      if (exited || !group.write(`${message}\n`)) step.end("exited")
      const ending = await within(ended, timeout)
      if (ending === "timedOut" || ending === "capped") await group.stop()
      const stdout = step.lines.join("\n")
      step = collector()
      const session = parseClaude(stdout, spent)
      spent += session.cost
      if (ending === "exited") session.error = `${session.error ?? "exited"}; exit ${await group.child.exited}: ${tail(group.stderr)}`
      return { stdout, session, timedOut: ending === "timedOut", capped: ending === "capped" }
    },
    async close() {
      await group.finish()
      return group.stderr.trim()
    },
  }
}

// OpenCode's server announces its address once it listens, and connects nib once asked about the work directory; one that has not by then counts
// as a harness error.
const SERVE_START = 60_000
// How long one request to OpenCode's API may take.
const API_TIMEOUT = 30_000

/**
 * One request to OpenCode 2's HTTP API, behind the server's password, as its own clients make it.
 * @returns the parsed JSON answer, undefined for none
 * @throws Error when the server cannot be reached or answers with an error status
 */
async function opencodeApi(url: string, password: string, route: string, body?: unknown) {
  const response = await fetch(new URL(route, url), {
    method: body === undefined ? "GET" : "POST",
    headers: { authorization: `Basic ${btoa(`opencode:${password}`)}`, ...body === undefined ? {} : { "content-type": "application/json" } },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(API_TIMEOUT),
  })
  const text = await response.text()
  if (!response.ok) throw new Error(`OpenCode's ${route.split("?")[0]} answered ${response.status}: ${text.slice(0, 300)}`)
  return text ? JSON.parse(text) : undefined
}

/**
 * Waits until the server has connected nib for `work`: OpenCode starts MCP servers only once a request names the directory, and does not wait for
 * them before a model call, so a first prompt sent sooner goes out without nib's tools.
 * @throws Error when nib fails to connect, or is not connected within SERVE_START ms
 */
async function nibConnected(url: string, password: string, work: string) {
  const deadline = Date.now() + SERVE_START
  for (;;) {
    const listed = await opencodeApi(url, password, `/api/mcp?${new URLSearchParams({ "location[directory]": work })}`)
    const status = listed?.data?.find((server: { name?: string }) => server.name === "nib")?.status
    if (status?.status === "connected") return
    if ((status && status.status !== "pending") || Date.now() > deadline) throw new Error(`OpenCode did not connect nib: ${JSON.stringify(status ?? listed)}`)
    await Bun.sleep(100)
  }
}

// OpenCode's own client asks for messages in pages of this many.
const PAGE = 200

/** The session's messages on the server, oldest first, without those in `seen`; `seen` takes in the ones returned. */
async function newMessages(url: string, password: string, session: string, seen: Set<string>) {
  const messages: { id: string }[] = []
  let cursor: string | undefined
  for (;;) {
    const query = new URLSearchParams(cursor ? { limit: String(PAGE), cursor } : { limit: String(PAGE), order: "asc" })
    const page = await opencodeApi(url, password, `/api/session/${encodeURIComponent(session)}/message?${query}`)
    const data = page?.data ?? []
    messages.push(...data)
    cursor = page?.cursor?.next
    if (data.length < PAGE || !cursor) break
  }
  const fresh = messages.filter((message) => !seen.has(message.id))
  for (const message of fresh) seen.add(message.id)
  return fresh
}

async function opencodeSession(model: Model, mode: Mode, work: string, home: string, timeout: number, steps: number): Promise<HarnessSession> {
  const { args, env, login } = await opencodeSetup(model, mode, work, home, timeout * steps)
  let announce = (_: string) => {}
  const announced = new Promise<string>((resolve) => { announce = resolve })
  const server = new Group(args, { cwd: work, env }, (line) => {
    const url = /listening on (http:\/\/\S+)/.exec(line)?.[1]
    if (url) announce(url)
  })
  const url = await within(Promise.race([announced, server.child.exited.then(() => "exited" as const)]), SERVE_START)
  if (url === "timedOut" || url === "exited") {
    await server.stop()
    throw new Error(`opencode serve ${url === "exited" ? "exited" : `did not listen within ${SERVE_START / 1000}s`}: ${tail(server.stderr)}`)
  }
  const password = env.OPENCODE_SERVER_PASSWORD!
  try {
    // Through the API the login lands only in the run's own database.
    if (login) await opencodeApi(url, password, "/api/credential", login)
    if (mode === "nib") await nibConnected(url, password, work)
  } catch (error) {
    await server.stop()
    throw new Error(`${error instanceof Error ? error.message : String(error)}; ${tail(server.stderr)}`)
  }
  const stderrs: string[] = []
  const seen = new Set<string>()
  let id: string | undefined
  return {
    async prompt(text, timeout) {
      const lines: string[] = []
      let calls = 0
      let cap = () => {}
      const capped = new Promise<"capped">((resolve) => { cap = () => resolve("capped") })
      const client = new Group(opencodePrompt(args[0]!, model, url, text, id), { cwd: work, env }, (line) => {
        lines.push(line)
        calls += toolCallsIn("opencode", line)
        if (calls > MAX_TOOL_CALLS) cap()
      })
      const ending = await within(Promise.race([client.child.exited.then(() => "finished" as const), capped]), timeout)
      if (ending === "finished") await client.finish()
      else await client.stop()
      if (client.stderr.trim()) stderrs.push(client.stderr.trim())
      const current = id ?? parseOpencode(lines.join("\n")).sessionId
      let unread: string | undefined
      if (current) {
        try {
          lines.push(JSON.stringify({ type: MESSAGES, sessionID: current, messages: await newMessages(url, password, current, seen) }))
        } catch (error) {
          unread = error instanceof Error ? error.message : String(error)
        }
      }
      // The server runs the model, so a stopped prompt stops it too; the job ends there.
      if (ending !== "finished") await server.stop()
      const stdout = lines.join("\n")
      const session = parseOpencode(stdout)
      id ??= session.sessionId
      const exit = await client.child.exited
      if (ending === "finished" && exit !== 0) session.error ??= `exit ${exit}: ${tail(client.stderr)}`
      // Without the server's messages the client's output can miss model calls, so the counts would be short; this cause comes first.
      if (ending === "finished" && unread) session.error = `could not read the session's messages: ${unread}${session.error ? `; ${session.error}` : ""}`
      return { stdout, session, timedOut: ending === "timedOut", capped: ending === "capped" }
    },
    async close() {
      await server.stop()
      return [server.stderr.trim(), ...stderrs].filter(Boolean).join("\n")
    },
  }
}
