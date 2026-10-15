/** Runs a job's prompts through one Claude Code or OpenCode process in an isolated setup and reads usage and tool calls from its JSON output. */
import { randomUUID } from "node:crypto"
import { copyFile, mkdir, readFile, writeFile } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import type { FileSink, Subprocess } from "bun"
import { REPO } from "./fixture"

export type Harness = "claude" | "opencode"
export type Mode = "builtin" | "nib"
/** `variant` is Claude Code's --effort or OpenCode's --variant, unset for the harness default; `shell` keeps the shell tool, which the benchmark otherwise turns off. */
export type Model = { harness: Harness; model: string; label: string; variant?: string; shell?: boolean }
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
}
export type Note = { kind: "corrected" | "warned"; tool: string; message: string }
export type ToolChars = { written: number; returned: number }
/** One prompt's output; `error` is set when the harness or provider failed, as opposed to the model doing the task wrong. */
export type Session = Usage & { sessionId?: string; error?: string }
/** A job's limits: each prompt gets `timeout` milliseconds and at most `maxToolCalls` tool calls. */
export type Budget = { timeout: number; maxToolCalls: number }

/**
 * Bumped whenever the harness setup below changes what a model sees, so older results stop counting as cached; 2 turned the harness's own readers off
 * in nib mode; 3 sandboxed both modes to the work directory: nib through NIB_SANDBOX, Claude Code's own tools through its permissions; 4 kept one
 * harness process, and so one nib server, through all prompts of a job instead of resuming the conversation in a new process per prompt.
 */
export const SETUP = 4
const MAIN = path.join(REPO, "src/main.ts")
const SUFFIX = "\n\nDo not run tests, type checks, builds or git commands; just make the edits and stop."
const EDIT_TOOLS = ["Edit", "Write", "MultiEdit", "NotebookEdit"]
// Claude Code refuses reads outside the work directory, and an Edit rule scoped to it covers every editing tool, so built-in edits elsewhere need a prompt that -p denies.
const CLAUDE_SANDBOX = JSON.stringify({ permissions: { blockReadsOutsideWorkingDirectories: true } })
const CLAUDE_EDITS = ["Edit(./**)"]
// OpenCode's external_directory permission, off below, keeps its own tools in the work directory; nib keeps itself there, named explicitly.
const nibEnvironment = (work: string) => ({ NIB_SANDBOX: "1", NIB_DIRECTORY: work })
// nib mode turns all of the harness's own file tools off, readers too, so a model cannot read a file without a tag and then edit it through nib.
const CLAUDE_FILE_TOOLS = [...EDIT_TOOLS, "Read", "Grep", "Glob"]
const OPENCODE_FILE_TOOLS = ["edit", "read", "grep", "glob", "list"]
// Shell, web and subagent tools are off in both modes and both harnesses, so only the file tools differ.
const CLAUDE_OFF = ["Bash", "Task", "Agent", "WebFetch", "WebSearch"]
const OPENCODE_OFF = ["bash", "task", "webfetch", "websearch", "question", "skill", "external_directory"]
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
  // A path that names nothing, noted while the call serves its other paths.
  { kind: "warned", pattern: /^\[.* does not exist.*\]$/gm },
]

/** The notes in one nib tool response; responses of other tools never carry them. */
export function classifyNotes(tool: string, text: string): Note[] {
  if (!tool.startsWith("nib.")) return []
  return NOTE_PATTERNS.flatMap(({ kind, pattern }) => [...text.matchAll(pattern)].map((match) => ({ kind, tool, message: match[0].slice(0, FAILURE_CHARS) })))
}

export function emptyUsage(): Usage {
  return { inputTokens: 0, outputTokens: 0, reasoningTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0, turns: 0, toolCalls: 0, toolErrors: 0, tools: {}, failures: [], notes: [], startContextTokens: 0, contextTokens: 0, toolChars: {} }
}

function addChars(usage: Usage, tool: string, kind: keyof ToolChars, count: number) {
  const entry = usage.toolChars[tool] ??= { written: 0, returned: 0 }
  entry[kind] += count
}

export function addUsage(total: Usage, part: Usage) {
  for (const key of ["inputTokens", "outputTokens", "reasoningTokens", "cacheRead", "cacheWrite", "cost", "turns", "toolCalls", "toolErrors"] as const) total[key] += part[key]
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

/** OpenCode's run --format json: step_finish parts carry tokens and cost per model call, tool_use parts end in completed (with output) or error. */
export function parseOpencode(stdout: string): Session {
  const session: Session = emptyUsage()
  for (const event of stdout.split("\n").map(json)) {
    if (!event) continue
    if (event.sessionID) session.sessionId = event.sessionID
    const part = event.part
    if (event.type === "step_finish") {
      const tokens = part?.tokens
      session.turns++
      session.inputTokens += tokens?.input ?? 0
      session.outputTokens += tokens?.output ?? 0
      session.reasoningTokens += tokens?.reasoning ?? 0
      session.cacheRead += tokens?.cache?.read ?? 0
      session.cacheWrite += tokens?.cache?.write ?? 0
      session.cost += part?.cost ?? 0
      session.contextTokens = (tokens?.input ?? 0) + (tokens?.cache?.read ?? 0) + (tokens?.cache?.write ?? 0)
      session.startContextTokens ||= session.contextTokens
    } else if (event.type === "tool_use") {
      session.toolCalls++
      const name = toolName("opencode", part?.tool ?? "unknown")
      session.tools[name] = (session.tools[name] ?? 0) + 1
      const failed = part?.state?.status === "error"
      const result = String((failed ? part.state.error : part?.state?.output) ?? "")
      addChars(session, name, "written", JSON.stringify(part?.state?.input ?? {}).length)
      addChars(session, name, "returned", result.length)
      if (failed) {
        session.toolErrors++
        session.failures.push({ tool: name, message: result.slice(0, FAILURE_CHARS) })
      } else session.notes.push(...classifyNotes(name, result))
    } else if (event.type === "error") {
      const error = event.error ?? {}
      session.error = String(error.data?.message ?? error.message ?? error.name ?? "error").slice(0, 500)
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

/**
 * The saved login of one provider, as OPENCODE_AUTH_CONTENT, so no copy of it lands on disk and no other provider's login is passed.
 * @throws Error when OpenCode has no such login, or an OAuth login would expire before a run of `duration` milliseconds ends
 */
export async function providerLogin(provider: string, duration: number) {
  const saved = JSON.parse(await readFile(path.join(opencodeData(), "auth.json"), "utf8").catch(() => "{}"))
  const entry = saved[provider]
  if (!entry) throw new Error(`OpenCode has no ${provider} login; run opencode auth login`)
  if (entry.type === "oauth" && Number(entry.expires) - Date.now() < duration + LOGIN_MARGIN) {
    throw new Error(`OpenCode's ${provider} login expires at ${new Date(Number(entry.expires)).toISOString()}, before this run could end; use OpenCode once so it refreshes the login, then run again`)
  }
  return JSON.stringify({ [provider]: entry })
}

const versions = new Map<Harness, Promise<string>>()

/** The installed version of a harness, or "unknown" when it does not run. */
export function harnessVersion(harness: Harness) {
  let version = versions.get(harness)
  if (!version) {
    version = (async () => {
      const child = Bun.spawn([harness, "--version"], { stdout: "pipe", stderr: "ignore", env: harnessEnvironment() })
      return (await new Response(child.stdout).text()).trim().split("\n")[0] || "unknown"
    })().catch(() => "unknown")
    versions.set(harness, version)
  }
  return version
}

/** A harness command and its whole environment. */
export type Setup = { args: string[]; env: Record<string, string> }

/** Claude Code's one process for a whole job, reading each prompt from stdin as a stream-json user message; `home` is a scratch directory outside the agent's project. */
export async function claudeSetup(model: Model, mode: Mode, work: string, home: string): Promise<Setup> {
  const nib = mode === "nib"
  await mkdir(home, { recursive: true })
  const config = path.join(home, "mcp.json")
  await writeFile(config, JSON.stringify({ mcpServers: nib ? { nib: { type: "stdio", command: "bun", args: [MAIN], env: nibEnvironment(work), alwaysLoad: true } } : {} }))
  // User settings and CLAUDE.md are skipped so personal rules (such as a deny of Edit) cannot change either setup.
  const off = model.shell ? CLAUDE_OFF.filter((tool) => tool !== "Bash") : CLAUDE_OFF
  const allowed = [...nib ? ["mcp__nib__*"] : CLAUDE_EDITS, ...model.shell ? ["Bash"] : []]
  const args = ["claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--model", model.model, "--mcp-config", config, "--strict-mcp-config",
    "--setting-sources", "project,local", "--settings", CLAUDE_SANDBOX, "--no-session-persistence", "--disallowedTools", ...off, ...nib ? CLAUDE_FILE_TOOLS : [], "--allowedTools", ...allowed]
  if (model.variant) args.push("--effort", model.variant)
  return { args, env: { ...harnessEnvironment(), PWD: work } }
}

/**
 * OpenCode's server for a whole job, on a free port behind a random password; each prompt's client (see opencodePrompt) gets the same environment.
 * `duration` is how long the whole job may take, which a login must outlive.
 */
export async function opencodeSetup(model: Model, mode: Mode, work: string, home: string, duration: number): Promise<Setup> {
  const nib = mode === "nib"
  await mkdir(home, { recursive: true })
  const permission: Record<string, string> = { "*": "allow", ...Object.fromEntries(OPENCODE_OFF.filter((tool) => !(model.shell && tool === "bash")).map((tool) => [tool, "deny"])) }
  if (nib) for (const tool of OPENCODE_FILE_TOOLS) permission[tool] = "deny"
  const config = { $schema: "https://opencode.ai/config.json", autoupdate: false, share: "disabled", permission, mcp: nib ? { nib: { type: "local", command: ["bun", MAIN], environment: nibEnvironment(work), enabled: true } } : {} }
  await mkdir(path.join(SHARED, "opencode-config"), { recursive: true })
  // OpenCode takes its project directory from PWD rather than the process's own directory, so an inherited PWD would point it at the caller's project.
  const env: Record<string, string> = { ...harnessEnvironment(), PWD: work, OPENCODE_SERVER_PASSWORD: randomUUID() }
  const provider = model.model.split("/")[0]!
  if (provider === "opencode") {
    // OpenCode's own models (the free ones included) need the OpenCode account; the copy lives in the run's scratch directory, removed with it.
    await mkdir(path.join(home, "data", "opencode"), { recursive: true })
    await copyFile(path.join(opencodeData(), "account.json"), path.join(home, "data", "opencode", "account.json")).catch(() => {
      throw new Error(`OpenCode's own models need ${path.join(opencodeData(), "account.json")}; log in to OpenCode first`)
    })
  } else if (provider === "openrouter" && process.env.OPENROUTER_API_KEY) env.OPENROUTER_API_KEY = process.env.OPENROUTER_API_KEY
  else env.OPENCODE_AUTH_CONTENT = await providerLogin(provider, duration)
  // Global config, plugins, agents and instructions live under the real XDG_CONFIG_HOME, so an empty one keeps them out; data is per run.
  Object.assign(env, {
    XDG_CONFIG_HOME: path.join(SHARED, "opencode-config"),
    XDG_CACHE_HOME: path.join(SHARED, "opencode-cache"),
    XDG_DATA_HOME: path.join(home, "data"),
    XDG_STATE_HOME: path.join(home, "state"),
    OPENCODE_CONFIG_CONTENT: JSON.stringify(config),
    OPENCODE_DISABLE_CLAUDE_CODE: "1",
    // An opencode.json the model wrote in the work directory would merge before the inline config and could reorder its permissions for the next step.
    OPENCODE_DISABLE_PROJECT_CONFIG: "1",
    OPENCODE_DISABLE_AUTOUPDATE: "1",
    OPENCODE_DISABLE_SHARE: "1",
  })
  return { args: ["opencode", "serve", "--port", "0"], env }
}

/** The client that sends one prompt to a job's OpenCode server at `url`, continuing `session` after the first prompt. */
export function opencodePrompt(model: Model, url: string, work: string, prompt: string, session?: string) {
  const args = ["opencode", "run", "--attach", url, "--format", "json", "--dir", work, "--model", model.model, "--auto"]
  if (model.variant) args.push("--variant", model.variant)
  if (session) args.push("--session", session)
  args.push(prompt + SUFFIX)
  return args
}

// A prompt past this many tool calls is stopped and counts as failed: a model stuck repeating a call would otherwise go on paying for each one.
// Prompts that do not loop stay far below it; the busiest seen made 103 calls, the loops 626 and 1694.
export const MAX_TOOL_CALLS = 150
export const DEFAULT_BUDGET: Budget = { timeout: 900_000, maxToolCalls: MAX_TOOL_CALLS }

/** How many tool calls one line of a harness's output starts. */
export function toolCallsIn(harness: Harness, line: string) {
  const event = json(line)
  if (harness === "claude") return event?.type === "assistant" ? (event.message?.content ?? []).filter((part: { type?: string }) => part.type === "tool_use").length : 0
  return event?.type === "tool_use" ? 1 : 0
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
      const message = JSON.stringify({ type: "user", message: { role: "user", content: text + SUFFIX } })
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

// OpenCode's server announces its address once it listens; one that has not by then counts as a harness error.
const SERVE_START = 60_000

async function opencodeSession(model: Model, mode: Mode, work: string, home: string, timeout: number, steps: number): Promise<HarnessSession> {
  const { args, env } = await opencodeSetup(model, mode, work, home, timeout * steps)
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
  const stderrs: string[] = []
  let id: string | undefined
  return {
    async prompt(text, timeout) {
      const lines: string[] = []
      let calls = 0
      let cap = () => {}
      const capped = new Promise<"capped">((resolve) => { cap = () => resolve("capped") })
      const client = new Group(opencodePrompt(model, url, work, text, id), { cwd: work, env }, (line) => {
        lines.push(line)
        calls += toolCallsIn("opencode", line)
        if (calls > MAX_TOOL_CALLS) cap()
      })
      const ending = await within(Promise.race([client.child.exited.then(() => "finished" as const), capped]), timeout)
      // The server runs the model, so a stopped prompt stops it too; the job ends there.
      if (ending === "finished") await client.finish()
      else await Promise.all([client.stop(), server.stop()])
      if (client.stderr.trim()) stderrs.push(client.stderr.trim())
      const stdout = lines.join("\n")
      const session = parseOpencode(stdout)
      id ??= session.sessionId
      const exit = await client.child.exited
      if (ending === "finished" && exit !== 0) session.error ??= `exit ${exit}: ${tail(client.stderr)}`
      return { stdout, session, timedOut: ending === "timedOut", capped: ending === "capped" }
    },
    async close() {
      await server.stop()
      return [server.stderr.trim(), ...stderrs].filter(Boolean).join("\n")
    },
  }
}
