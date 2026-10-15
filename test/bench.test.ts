import { afterAll, afterEach, describe, expect, spyOn, test } from "bun:test"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js"
import { chmod, lstat, mkdir, mkdtemp, readdir, readFile, readlink, rm, writeFile } from "node:fs/promises"
import os, { tmpdir } from "node:os"
import path from "node:path"
import { applyChanges, changedFiles, copyFixture, hashTree, linkModules, pristineFixture, REPO, writeFixture } from "../bench/fixture"
import { confined, jobKey, recheck, reparse, verify } from "../bench/jobs"
import { MODELS, selectModels } from "../bench/models"
import { isCurrent, renderReport, writeReport, type Current } from "../bench/report"
import { claudeSetup, classifyNotes, DEFAULT_BUDGET, Group, harnessEnvironment, MAX_TOOL_CALLS, NOTE_PATTERNS, opencodePrompt, opencodeSetup, openSession, parseClaude, parseOpencode, providerLogin, readLines, SETUP, toolCallsIn, toolName, type Model } from "../bench/runners"
import { checkHash, eligible, fits, identity, identityKey, loadRuns, samples, saveRun, taskHash, type Run } from "../bench/store"
import { sourceWithUses, TASKS, taskKind } from "../bench/tasks"
import { createServer } from "../src/server"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })
// The test runner fires no exit handlers, so the pristine fixture the tests built is removed here.
afterAll(async () => { await rm((await pristineFixture()).directory, { recursive: true, force: true }) })

async function scratch() {
  const directory = await mkdtemp(path.join(tmpdir(), "nib-bench-test-"))
  directories.push(directory)
  return directory
}

const lines = (...events: object[]) => events.map((event) => JSON.stringify(event)).join("\n")

describe("harness output", () => {
  test("Claude Code stream-json: tool calls by name, failed tool results, nib's notes in the others, tool characters, model calls, the last call's context and usage from the result", () => {
    const recovered = "Recovered by remapping stale line numbers to unchanged current lines (file changed since the tagged read). Verify the diff matches your intent."
    const failed = "MCP error -32602: Input validation error"
    const edited = `[a.txt#1A2B]\n1:x\n\nWarnings:\n${recovered}`
    const args = { read: { paths: ["a.txt"] }, patch: { input: "[a.txt#1A2B]\nPUT 1:\n+x" }, edit: { file_path: "a.txt", old_string: "y", new_string: "x" } }
    const session = parseClaude(lines(
      { type: "system", subtype: "init", session_id: "s1", model: "claude-haiku-5-5" },
      { type: "assistant", session_id: "s1", message: { id: "m1", usage: { input_tokens: 2, cache_creation_input_tokens: 15000, cache_read_input_tokens: 0 }, content: [{ type: "text", text: "Reading" }, { type: "tool_use", id: "t1", name: "mcp__nib__read", input: args.read }] } },
      { type: "user", session_id: "s1", message: { content: [{ type: "tool_result", tool_use_id: "t1", is_error: true, content: [{ type: "text", text: failed }] }] } },
      // Claude Code sends each content block of one model call as its own event.
      { type: "assistant", session_id: "s1", message: { id: "m2", usage: { input_tokens: 3, cache_creation_input_tokens: 500, cache_read_input_tokens: 15000 }, content: [{ type: "tool_use", id: "t2", name: "mcp__nib__edit", input: args.patch }] } },
      { type: "assistant", session_id: "s1", message: { id: "m2", usage: { input_tokens: 3, cache_creation_input_tokens: 500, cache_read_input_tokens: 15000 }, content: [{ type: "tool_use", id: "t3", name: "Edit", input: args.edit }] } },
      { type: "user", session_id: "s1", message: { content: [
        { type: "tool_result", tool_use_id: "t2", content: [{ type: "text", text: edited }] },
        // Only nib's responses carry notes, so the same words from another tool count for nothing.
        { type: "tool_result", tool_use_id: "t3", content: recovered },
      ] } },
      { type: "assistant", session_id: "s1", message: { id: "m3", usage: { input_tokens: 4, cache_creation_input_tokens: 300, cache_read_input_tokens: 15500 }, content: [{ type: "text", text: "Done" }] } },
      // num_turns counts the two parallel tool calls apart; the run made three model calls.
      { type: "result", subtype: "success", is_error: false, session_id: "s1", num_turns: 4, total_cost_usd: 0.25, usage: { input_tokens: 100, output_tokens: 40, cache_read_input_tokens: 900, cache_creation_input_tokens: 50 } },
    ) + "\nnot json")
    expect(session).toEqual({
      inputTokens: 100, outputTokens: 40, reasoningTokens: 0, cacheRead: 900, cacheWrite: 50, cost: 0.25, turns: 3, toolCalls: 3, toolErrors: 1, tools: { "nib.read": 1, "nib.edit": 1, edit: 1 },
      failures: [{ tool: "nib.read", message: failed }], notes: [{ kind: "warned", tool: "nib.edit", message: recovered }], sessionId: "s1",
      // The first and the last call's input, not the result's totals over all calls.
      startContextTokens: 15002,
      contextTokens: 15804,
      toolChars: {
        "nib.read": { written: JSON.stringify(args.read).length, returned: failed.length },
        "nib.edit": { written: JSON.stringify(args.patch).length, returned: edited.length },
        edit: { written: JSON.stringify(args.edit).length, returned: recovered.length },
      },
    })
    expect(parseClaude(lines({ type: "result", subtype: "error_during_execution", is_error: true, result: "overloaded" })).error).toBe("error_during_execution: overloaded")
    expect(parseClaude("").error).toBe("no result event")
  })

  test("OpenCode run --format json: step_finish usage summed, tool_use states counted, nib's notes in completed output, tool characters, the last step's context, error events kept", () => {
    const lesson = "The - rows matched the file, so the edit was applied; but nib never needs the old text, since the line numbers already say what a hunk replaces."
    const output = `[a.txt#1A2B]\n2:TWO\n\nWarnings:\n${lesson}\nLeave - rows out next time. This hunk is just:\nPUT 2:\n+TWO`
    const error = "Ripgrep JSON record exceeded 65536 bytes"
    const args = { patch: { input: "[a.txt#1A2B]\nPUT 2:\n-two\n+TWO" }, grep: { pattern: "x" } }
    // Shapes copied from a real OpenCode 1.18 run.
    const session = parseOpencode(lines(
      { type: "step_start", sessionID: "ses_1", part: { type: "step-start" } },
      { type: "tool_use", sessionID: "ses_1", part: { type: "tool", tool: "nib_edit", state: { status: "completed", input: args.patch, output } } },
      { type: "tool_use", sessionID: "ses_1", part: { type: "tool", tool: "grep", state: { status: "error", input: args.grep, error } } },
      { type: "step_finish", sessionID: "ses_1", part: { type: "step-finish", reason: "tool-calls", tokens: { total: 8086, input: 1867, output: 59, reasoning: 16, cache: { write: 0, read: 6144 } }, cost: 0.002 } },
      { type: "step_finish", sessionID: "ses_1", part: { type: "step-finish", reason: "stop", tokens: { total: 100, input: 10, output: 5, reasoning: 0, cache: { write: 3, read: 80 } }, cost: 0.001 } },
    ))
    expect(session).toEqual({
      inputTokens: 1877, outputTokens: 64, reasoningTokens: 16, cacheRead: 6224, cacheWrite: 3, cost: 0.003, turns: 2, toolCalls: 2, toolErrors: 1, tools: { "nib.edit": 1, grep: 1 },
      failures: [{ tool: "grep", message: error }], notes: [{ kind: "corrected", tool: "nib.edit", message: lesson }], sessionId: "ses_1",
      startContextTokens: 8011,
      contextTokens: 93,
      toolChars: { "nib.edit": { written: JSON.stringify(args.patch).length, returned: output.length }, grep: { written: JSON.stringify(args.grep).length, returned: error.length } },
    })
    expect(parseOpencode(lines({ type: "error", sessionID: "ses_1", error: { name: "APIError", data: { message: "free tier refused" } } })).error).toBe("free tier refused")
    expect(parseOpencode("").error).toBe("no model call finished")
  })

  test("tool names line up across harnesses", () => {
    expect(["mcp__nib__grep", "Read", "MultiEdit"].map((name) => toolName("claude", name))).toEqual(["nib.grep", "read", "multiedit"])
    expect(["nib_grep", "read", "apply_patch"].map((name) => toolName("opencode", name))).toEqual(["nib.grep", "read", "apply_patch"])
  })

  test("every note nib prints is classified by kind, and file lines that read like notes are not", async () => {
    const directory = await scratch()
    const file = path.join(directory, "a.txt")
    await writeFile(file, "one\ntwo\nthree\nfour\nfive\n")
    const client = new Client({ name: "test", version: "1" })
    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair()
    await Promise.all([createServer({ directory }).connect(serverTransport), client.connect(clientTransport)])
    const call = async (name: string, args: Record<string, unknown>) => {
      const result = await client.callTool({ name, arguments: args }) as { isError?: boolean; content: { text?: string }[] }
      if (result.isError) throw new Error(`${name} failed: ${result.content[0]?.text}`)
      return result.content[0]?.text ?? ""
    }
    const seen: string[] = []
    const notes = async (name: string, args: Record<string, unknown>) => {
      const found = classifyNotes(`nib.${name}`, await call(name, args)).map((note) => `${note.kind}: ${note.message}`)
      seen.push(...found)
      return found
    }
    const tag = async () => /#([0-9A-F]{4})\]/.exec(await call("read", { paths: ["a.txt"] }))![1]
    const change = async (update: (text: string) => string) => writeFile(file, update(await readFile(file, "utf8")))

    expect(await notes("read", { filePath: "a.txt" })).toEqual(['corrected: [read takes paths, like ["a.txt"]; filePath was read that way]'])
    expect(await notes("grep", { pattern: "two", path: "a.txt", after: 30 })).toEqual([
      'corrected: [grep takes paths, a list like ["a.txt"]; path was read that way]',
      "corrected: [after 30 is over its limit of 20, so 20 was used]",
    ])
    expect(await notes("read", { paths: '["a.txt"]' })).toEqual(['corrected: [paths was a string holding a JSON list; it was read as the list ["a.txt"], so send the list itself]'])
    expect(await notes("read", { paths: '["a.txt"]>' })).toEqual(['corrected: [paths was a string holding a JSON list; it was read as the list ["a.txt"], and the tool-call markup after it (">") was dropped, so send the list itself]'])
    const first = await tag()
    expect(await notes("edit", { input: `[a.txt#${first}]\nPUT 2.=2:\n-two\n+TWO` })).toEqual([
      "corrected: The - rows matched the file, so the edit was applied; but nib never needs the old text, since the line numbers already say what a hunk replaces.",
    ])
    const escaped = await tag()
    expect(await notes("edit", { input: `[a.txt#${escaped}]\\nPUT 2:\\n+TWO` })).toEqual(["corrected: [the patch came as one line with escaped line breaks (\\n); it was read with real line breaks, so send those]"])
    // A stale tag is rebased only when every line it touches keeps an unchanged neighbour, so these edits stay clear of the lines changed before them.
    expect(await notes("edit", { input: `[a.txt#${first}]\nPUT 4.=4:\n+FOUR` })).toEqual(["warned: Recovered from a stale tag using an earlier read (a prior edit in this session advanced the tag)."])
    let read = await tag()
    await change((text) => text + "six\n")
    expect(await notes("edit", { input: `[a.txt#${read}]\nPUT 1.=1:\n+ONE` })).toEqual(["warned: Recovered from a stale tag using the tagged read (file changed externally between read and edit)."])
    read = await tag()
    await change((text) => "zero\n" + text)
    expect(await notes("edit", { input: `[a.txt#${read}]\nPUT 3.=3:\n+THREE` })).toEqual([
      "warned: Recovered by remapping stale line numbers to unchanged current lines (file changed since the tagged read). Verify the diff matches your intent.",
    ])
    read = await tag()
    await change((text) => text + "seven\n")
    expect(await notes("edit", { input: `[a.txt#${read}]\nPUT >$:\n+eight` })).toEqual([
      "warned: Applied the end-of-file insert despite a stale tag (file changed since the tagged read). Read again if that change was unexpected.",
    ])
    const invented = (await tag()) === "0000" ? "0001" : "0000"
    expect(await notes("edit", { input: `[a.txt#${invented}]\nSUB 1: zero\n+ZERO` })).toEqual([
      `warned: Tag #${invented} was never issued for this file; never invent tags. The edit was applied only because every hunk is safe without one: SUB and matching - rows quote what they replace, and PUT >$: only appends. Any other hunk would have been refused; use the tag in the header above.`,
    ])
    expect(await readFile(file, "utf8")).toBe("ZERO\nONE\nTWO\nTHREE\nFOUR\nfive\nsix\nseven\neight\n")
    const ranged = await tag()
    expect(await notes("edit", { input: `[a.txt#${ranged}]\nPUT 6:=7:\n+five and six` })).toEqual(["corrected: PUT 6:=7: was read as PUT 6.=7:; a range is written N.=M, with a dot."])
    expect(await notes("grep", { pattern: "ZERO", paths: ["a.txt"], mode: "content" })).toEqual(["corrected: [mode content was read as matches, nib's name for it]"])
    expect(await notes("read", { paths: ["a.txt", "nope.txt"] })).toEqual(["warned: [nope.txt does not exist]"])
    expect(await notes("grep", { pattern: "ZERO", paths: ["a.txt", "nope.txt"] })).toEqual(["warned: [nope.txt does not exist; searched the other paths]"])
    expect(await notes("find", { paths: ["a.txt", "nope"] })).toEqual(["warned: [nope does not exist; listed the other paths]"])
    await writeFile(path.join(directory, "plan.md"), "# Plan\n- **Name** of a peer, in precedence order, decided ONCE at server start:\n  1. an explicit name\n  2. the pane session\n")
    const plan = /#([0-9A-F]{4})\]/.exec(await call("read", { paths: ["plan.md"] }))![1]
    expect(await notes("edit", { input: `[plan.md#${plan}]\nPUT 3.=4:\n+- **Name** of a peer, in precedence order, decided ONCE at registration:\n+  1. an explicit name or SKETERM_PEER_NAME` })).toEqual([
      "warned: Lines 2 and 3 now nearly match: line 2 was kept just above an edit and line 3 is the edit's first new line. If the edit was meant to replace line 2, its range started one line late; fix that now.",
    ])
    expect(NOTE_PATTERNS.filter(({ pattern }) => !seen.some((note) => new RegExp(pattern.source).test(note.replace(/^\w+: /, ""))))).toEqual([])

    // Read and grep rows start with their line number, so notes quoted in a file never count.
    const quoted = seen.map((note) => note.replace(/^\w+: /, ""))
    await writeFile(path.join(directory, "notes.txt"), quoted.join("\n") + "\n")
    expect(classifyNotes("nib.read", await call("read", { paths: ["notes.txt"] }))).toEqual([])
    expect(classifyNotes("nib.grep", await call("grep", { pattern: "stale", paths: ["notes.txt"] }))).toEqual([])
  })
})

/** Runs `body` with `changes` applied to process.env, undefined deleting a variable, and puts the old values back after. */
async function withEnvironment<T>(changes: Record<string, string | undefined>, body: () => Promise<T> | T) {
  const saved = Object.fromEntries(Object.keys(changes).map((key) => [key, process.env[key]]))
  const apply = (values: Record<string, string | undefined>) => {
    for (const [key, value] of Object.entries(values)) {
      if (value === undefined) delete process.env[key]
      else process.env[key] = value
    }
  }
  apply(changes)
  try {
    return await body()
  } finally {
    apply(saved)
  }
}

// Stand-ins for the harnesses, put first on PATH: Claude Code's stream-json mode answers each stdin message with a result and a cumulative cost,
// OpenCode's server counts the prompts its clients send; both print their pid, so a test sees whether the prompts of a job share one process.
const FAKE_CLAUDE = `#!/usr/bin/env bun
let turn = 0
let pending = ""
const emit = (event) => process.stdout.write(JSON.stringify(event) + "\\n")
emit({ type: "system", subtype: "init", session_id: "s1" })
for await (const chunk of Bun.stdin.stream()) {
  pending += new TextDecoder().decode(chunk)
  let at
  while ((at = pending.indexOf("\\n")) >= 0) {
    const text = JSON.parse(pending.slice(0, at)).message.content
    pending = pending.slice(at + 1)
    turn++
    if (text.startsWith("hang")) {
      emit({ type: "system", subtype: "child", pid: Bun.spawn(["sleep", "300"]).pid })
      await Bun.sleep(300_000)
    }
    if (text.startsWith("loop")) for (let call = 0; call < 200; call++) emit({ type: "assistant", message: { id: "m" + call, content: [{ type: "tool_use", id: "t" + call, name: "Read", input: {} }] } })
    emit({ type: "assistant", session_id: "s1", message: { id: "a" + turn, usage: { input_tokens: 1 }, content: [{ type: "text", text: "pid " + process.pid + " turn " + turn }] } })
    emit({ type: "result", subtype: "success", is_error: false, session_id: "s1", num_turns: 1, total_cost_usd: turn / 10, usage: { input_tokens: 10, output_tokens: 5 } })
  }
}
`
const FAKE_OPENCODE = `#!/usr/bin/env bun
const [command, ...args] = process.argv.slice(2)
const authorization = "Basic " + btoa("opencode:" + process.env.OPENCODE_SERVER_PASSWORD)
if (command === "serve") {
  let turns = 0
  const server = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: (request) => request.headers.get("authorization") === authorization ? Response.json({ pid: process.pid, turn: ++turns }) : new Response("no", { status: 401 }) })
  console.log("opencode server listening on http://127.0.0.1:" + server.port)
} else {
  const session = args.includes("--session") ? args[args.indexOf("--session") + 1] : "ses_" + process.pid
  const state = await (await fetch(args[args.indexOf("--attach") + 1], { headers: { authorization } })).json()
  const prompt = args.at(-1)
  if (prompt.startsWith("loop")) for (let call = 0; call < 200; call++) console.log(JSON.stringify({ type: "tool_use", sessionID: session, part: { tool: "read", state: { status: "completed", input: {}, output: "x" } } }))
  if (prompt.startsWith("hang")) await Bun.sleep(300_000)
  console.log(JSON.stringify({ type: "text", sessionID: session, part: { text: "server " + state.pid + " turn " + state.turn } }))
  console.log(JSON.stringify({ type: "step_finish", sessionID: session, part: { tokens: { input: 1, output: 1, cache: { read: 0, write: 0 } }, cost: 0.01 } }))
}
`

/** A directory holding the fake harnesses, to put first on PATH. */
async function fakeHarnesses() {
  const bin = await scratch()
  for (const [name, script] of [["claude", FAKE_CLAUDE], ["opencode", FAKE_OPENCODE]]) {
    await writeFile(path.join(bin, name!), script!)
    await chmod(path.join(bin, name!), 0o755)
  }
  return bin
}

const alive = (pid: number) => {
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

describe("harness setup", () => {
  const claude: Model = { harness: "claude", model: "claude-haiku-5-5", label: "Haiku" }
  const opencode: Model = { harness: "opencode", model: "openrouter/xiaomi/mimo-v2.6-flash", label: "MiMo" }

  test("a harness inherits only what it needs to run and log in: no overrides, no surrounding Claude Code session, no per-token API keys", async () => {
    const inherited = { CLAUDECODE: "1", CLAUDE_CODE_SESSION_ID: "x", CLAUDE_CONFIG_DIR: "/config", OPENCODE_PERMISSION: '{"*":"allow"}', OPENCODE_CONFIG_DIR: "/plugins", NIB_DIRECTORY: "/elsewhere", OPENAI_API_KEY: "sk", ANTHROPIC_API_KEY: "sk", HTTPS_PROXY: "http://proxy:3128", LC_TIME: "C" }
    await withEnvironment(inherited, () => {
      const env = harnessEnvironment()
      expect(Object.keys(inherited).filter((key) => key in env).sort()).toEqual(["CLAUDE_CONFIG_DIR", "HTTPS_PROXY", "LC_TIME"])
      expect(env.PATH).toBe(process.env.PATH!)
    })
  })

  test("Claude Code: one stream-json process per job, file tools by mode, shell and subagents off unless asked, and nib told its directory", async () => {
    // The scratch home does not exist yet when a run starts.
    const home = path.join(await scratch(), "home")
    const nib = await claudeSetup(claude, "nib", "/work", home)
    const disallowed = nib.args.slice(nib.args.indexOf("--disallowedTools") + 1, nib.args.indexOf("--allowedTools"))
    expect(disallowed).toEqual(["Bash", "Task", "Agent", "WebFetch", "WebSearch", "Edit", "Write", "MultiEdit", "NotebookEdit", "Read", "Grep", "Glob"])
    expect(nib.args.slice(nib.args.indexOf("--allowedTools") + 1)).toEqual(["mcp__nib__*"])
    expect(nib.args.slice(0, 6)).toEqual(["claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json"])
    expect(nib.args).toContain("--no-session-persistence")
    expect(nib.env.PWD).toBe("/work")
    const server = JSON.parse(await readFile(path.join(home, "mcp.json"), "utf8")).mcpServers.nib
    expect([server.args[0].endsWith("src/main.ts"), server.env]).toEqual([true, { NIB_SANDBOX: "1", NIB_DIRECTORY: "/work" }])
    expect(JSON.parse(nib.args[nib.args.indexOf("--settings") + 1]!)).toEqual({ permissions: { blockReadsOutsideWorkingDirectories: true } })
    const builtin = await claudeSetup({ ...claude, shell: true, variant: "high" }, "builtin", "/work", home)
    expect(builtin.args.slice(builtin.args.indexOf("--allowedTools") + 1)).toEqual(["Edit(./**)", "Bash", "--effort", "high"])
    expect(builtin.args[builtin.args.indexOf("--settings") + 1]).toBe(nib.args[nib.args.indexOf("--settings") + 1])
    expect(builtin.args.slice(builtin.args.indexOf("--disallowedTools") + 1, builtin.args.indexOf("--allowedTools"))).toEqual(["Task", "Agent", "WebFetch", "WebSearch"])
  })

  test("tool calls are counted per line, and lines are whole however the chunks split them", async () => {
    const call = JSON.stringify({ type: "tool_use", part: { tool: "nib_read" } })
    const calls = JSON.stringify({ type: "assistant", message: { content: [{ type: "text" }, { type: "tool_use" }, { type: "tool_use" }] } })
    expect([toolCallsIn("opencode", call), toolCallsIn("claude", calls), toolCallsIn("claude", call), toolCallsIn("opencode", "not json")]).toEqual([1, 2, 0, 0])
    const text = `${call}\n${calls}\nlast line without a newline`
    // Chunks of 7 bytes split the lines anywhere.
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        const bytes = new TextEncoder().encode(text)
        for (let at = 0; at < bytes.length; at += 7) controller.enqueue(bytes.subarray(at, at + 7))
        controller.close()
      },
    })
    const lines: string[] = []
    await readLines(stream.getReader(), (line) => lines.push(line))
    expect(lines).toEqual(text.split("\n"))
  })

  test("a stop reaches the whole process group, SIGTERM or not, and output held open by an escaped process is cut off", async () => {
    const cwd = await scratch()
    const env = { PATH: process.env.PATH! }
    const pids: string[] = []
    const stubborn = new Group(["bash", "-c", "trap '' TERM; sleep 300 & echo $!; wait"], { cwd, env }, (line) => pids.push(line))
    while (!pids.length) await Bun.sleep(20)
    let started = Date.now()
    await stubborn.stop()
    // The shell and its child ignore SIGTERM, so only the SIGKILL after the grace period ends them.
    expect(Date.now() - started).toBeGreaterThanOrEqual(4500)
    expect(alive(Number(pids[0]))).toBe(false)
    const escaped: string[] = []
    // The shell exits only once its child leads a session of its own, so the child has left the group before the stop.
    const leaver = new Group(["bash", "-c", 'setsid sleep 300 & pid=$!; until [ "$(ps -o sid= -p $pid | tr -d " ")" = "$pid" ]; do sleep 0.01; done; echo $pid'], { cwd, env }, (line) => escaped.push(line))
    started = Date.now()
    await leaver.finish()
    try {
      expect(Date.now() - started).toBeLessThan(4000)
      expect(alive(Number(escaped[0]))).toBe(true)
    } finally {
      process.kill(Number(escaped[0]), "SIGKILL")
    }
  }, 20_000)

  test("Claude Code: every prompt of a job goes to the same process, each costs what the session added, and a limit stops it all", async () => {
    const bin = await fakeHarnesses()
    await withEnvironment({ PATH: `${bin}:${process.env.PATH}` }, async () => {
      const work = await scratch()
      const session = await openSession(claude, "nib", work, await scratch(), 10_000, 2)
      const first = await session.prompt("first", 10_000)
      const second = await session.prompt("second", 10_000)
      const pid = /pid (\d+) turn 1/.exec(first.stdout)?.[1]
      expect(pid).toBeDefined()
      expect(second.stdout).toContain(`pid ${pid} turn 2`)
      expect([first.session.cost, second.session.cost].map((cost) => cost.toFixed(3))).toEqual(["0.100", "0.100"])
      expect([first.timedOut, first.capped, second.session.error]).toEqual([false, false, undefined])
      // The init event came before any prompt was sent, and still lands in the first prompt's output.
      expect([first.stdout.includes('"subtype":"init"'), second.stdout.includes('"subtype":"init"')]).toEqual([true, false])
      await session.close()
      expect(alive(Number(pid))).toBe(false)

      const looping = await openSession(claude, "nib", work, await scratch(), 10_000, 1)
      expect((await looping.prompt("loop", 10_000)).capped).toBe(true)
      await looping.close()
      const hanging = await openSession(claude, "nib", work, await scratch(), 1000, 1)
      const hung = await hanging.prompt("hang", 1000)
      await hanging.close()
      const child = Number(/"pid":(\d+)/.exec(hung.stdout)?.[1])
      expect([hung.timedOut, child > 0, alive(child)]).toEqual([true, true, false])
    })
  }, 30_000)

  test("OpenCode: one server per job behind a password, a client per prompt continuing the first prompt's session, and a limit stops the server too", async () => {
    const bin = await fakeHarnesses()
    await withEnvironment({ PATH: `${bin}:${process.env.PATH}`, OPENROUTER_API_KEY: "sk-test" }, async () => {
      const work = await scratch()
      const home = await scratch()
      const session = await openSession(opencode, "nib", work, home, 10_000, 2)
      const first = await session.prompt("first", 10_000)
      const second = await session.prompt("second", 10_000)
      const server = /server (\d+) turn 1/.exec(first.stdout)?.[1]
      expect(server).toBeDefined()
      expect(second.stdout).toContain(`server ${server} turn 2`)
      expect(first.session.sessionId).toMatch(/^ses_\d+$/)
      expect(second.session.sessionId).toBe(first.session.sessionId)
      expect([first.session.cost, second.session.error]).toEqual([0.01, undefined])
      await session.close()
      expect(alive(Number(server))).toBe(false)

      const looping = await openSession(opencode, "nib", work, home, 10_000, 1)
      expect((await looping.prompt("loop", 10_000)).capped).toBe(true)
      await looping.close()
      const hanging = await openSession(opencode, "nib", work, home, 1000, 1)
      expect((await hanging.prompt("hang", 1000)).timedOut).toBe(true)
      await hanging.close()
    })
  }, 30_000)

  test("OpenCode: config through the environment, isolated XDG directories, the work directory twice, and OpenRouter's key", async () => {
    const home = await scratch()
    await withEnvironment({ OPENROUTER_API_KEY: "sk-test" }, async () => {
      const nib = await opencodeSetup(opencode, "nib", "/work", home, 60_000)
      expect(nib.args).toEqual(["opencode", "serve", "--port", "0"])
      const config = JSON.parse(nib.env.OPENCODE_CONFIG_CONTENT!)
      expect(config.permission).toMatchObject({ "*": "allow", bash: "deny", task: "deny", edit: "deny", read: "deny", grep: "deny", glob: "deny", list: "deny" })
      expect([config.mcp.nib.command[1].endsWith("src/main.ts"), config.mcp.nib.environment, config.permission.external_directory]).toEqual([true, { NIB_SANDBOX: "1", NIB_DIRECTORY: "/work" }, "deny"])
      expect([nib.env.PWD, nib.env.OPENROUTER_API_KEY, nib.env.XDG_DATA_HOME, nib.env.OPENCODE_SERVER_PASSWORD?.length]).toEqual(["/work", "sk-test", path.join(home, "data"), 36])
      expect("OPENCODE_AUTH_CONTENT" in nib.env).toBe(false)
      expect(nib.env.OPENCODE_DISABLE_PROJECT_CONFIG).toBe("1")
      const client = opencodePrompt(opencode, "http://127.0.0.1:4096", "/work", "Do it")
      expect(client.slice(0, 9)).toEqual(["opencode", "run", "--attach", "http://127.0.0.1:4096", "--format", "json", "--dir", "/work", "--model"])
      expect(client).not.toContain("--session")
      expect(opencodePrompt({ ...opencode, variant: "high" }, "http://x", "/work", "Do it", "ses_1").slice(-5, -1)).toEqual(["--variant", "high", "--session", "ses_1"])
      const builtin = await opencodeSetup({ ...opencode, shell: true }, "builtin", "/work", home, 60_000)
      const permission = JSON.parse(builtin.env.OPENCODE_CONFIG_CONTENT!).permission
      expect([permission.bash, permission.edit, permission.read, permission.external_directory, JSON.parse(builtin.env.OPENCODE_CONFIG_CONTENT!).mcp]).toEqual([undefined, undefined, undefined, "deny", {}])
    })
  })

  test("OpenCode logins: only the model's provider goes along, through the environment, and an OAuth login must outlive the whole job", async () => {
    const data = await scratch()
    await mkdir(path.join(data, "opencode"))
    const later = Date.now() + 86_400_000
    const openai = { type: "oauth", access: "a", refresh: "r", expires: later }
    await writeFile(path.join(data, "opencode", "auth.json"), JSON.stringify({ openai, openrouter: { type: "api", key: "sk-saved" }, anthropic: { type: "oauth" } }))
    await withEnvironment({ XDG_DATA_HOME: data, OPENAI_API_KEY: "sk-billed", OPENROUTER_API_KEY: undefined }, async () => {
      const gpt = await opencodeSetup({ harness: "opencode", model: "openai/gpt-6-luna", label: "Luna" }, "nib", "/work", await scratch(), 60_000)
      expect(JSON.parse(gpt.env.OPENCODE_AUTH_CONTENT!)).toEqual({ openai })
      expect(gpt.env.OPENAI_API_KEY).toBeUndefined()
      const mimo = await opencodeSetup(opencode, "nib", "/work", await scratch(), 60_000)
      expect(JSON.parse(mimo.env.OPENCODE_AUTH_CONTENT!)).toEqual({ openrouter: { type: "api", key: "sk-saved" } })
      await expect(opencodeSetup({ harness: "opencode", model: "openai/gpt-6-luna", label: "Luna" }, "nib", "/work", await scratch(), 2 * 86_400_000)).rejects.toThrow("before this run could end")
      await expect(providerLogin("google", 60_000)).rejects.toThrow("OpenCode has no google login")
    })
  })

  test("models resolve by label, id or harness, unknown ids pick their harness by name, and a model named twice runs once", () => {
    expect(selectModels()).toBe(MODELS)
    expect(selectModels(["claude"]).map((model) => model.model)).toEqual(["claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5"])
    expect(selectModels(["gpt-6 luna"])[0]!.model).toBe("openai/gpt-6-luna")
    expect(selectModels(["claude-fable-5-1", "opencode/big-pickle"]).map((model) => model.harness)).toEqual(["claude", "opencode"])
    expect(selectModels(["GPT-6 Luna", "openai/gpt-6-luna", "opencode", "claude-haiku-5-5", "Claude Haiku 5.5"]).map((model) => model.model)).toEqual([
      "openai/gpt-6-luna", "openai/gpt-6.1-sol", "openai/gpt-6-astra", "openrouter/xiaomi/mimo-v2.6-flash", "openrouter/deepseek/deepseek-v4.1-flash", "claude-haiku-5-5",
    ])
  })

  test("bench/run.ts refuses invalid options before anything runs", async () => {
    for (const [option, message] of [[["--modes", "nbi"], "--modes takes builtin, nib or both"], [["--concurrency", "0"], "--concurrency takes a whole number above 0"], [["--repetitions", "1.5"], "--repetitions takes a whole number above 0"], [["--timeout=-3"], "--timeout takes a number above 0"], [["--modez", "nib"], "Unknown option '--modez'"]] as const) {
      const child = Bun.spawn([process.execPath, path.join(REPO, "bench/run.ts"), ...option, "--plan"], { stdout: "pipe", stderr: "pipe" })
      const [stderr, exit] = await Promise.all([new Response(child.stderr).text(), child.exited])
      expect([exit, stderr.split("\n")[0]]).toEqual([2, expect.stringContaining(message)])
    }
  })
})

describe("stored runs", () => {
  const base = { task: "rename", taskHash: "t", fixture: "f", harness: "opencode" as const, model: "m", mode: "nib" as const, nib: "n1" }
  const stored = (id: string, status: Run["status"]): Run => ({
    ...identity(base), id, key: "k", label: "M", status, checkHash: "c", harnessVersion: "1", started: `2026-10-14T12:00:0${id}Z`, seconds: 1, timeout: 900_000, maxToolCalls: MAX_TOOL_CALLS,
    inputTokens: 0, outputTokens: 0, reasoningTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0, turns: 1, toolCalls: 0, toolErrors: 0, tools: {}, failures: [], notes: [], startContextTokens: 0, contextTokens: 0, toolChars: {},
  })

  test("identity keys split on everything a model sees, and a builtin job ignores the nib version", async () => {
    const key = identityKey(identity(base))
    expect(identityKey(identity({ ...base, nib: "n2" }))).not.toBe(key)
    expect(identityKey(identity({ ...base, shell: true }))).not.toBe(key)
    expect(identityKey(identity({ ...base, variant: "high" }))).not.toBe(key)
    const task = TASKS[0]!
    const model = MODELS[3]!
    expect(jobKey({ task, model, mode: "builtin", nib: "a", fixture: "f" })).toBe(jobKey({ task, model, mode: "builtin", nib: "b", fixture: "f" }))
    expect(jobKey({ task, model, mode: "nib", nib: "a", fixture: "f" })).not.toBe(jobKey({ task, model, mode: "nib", nib: "b", fixture: "f" }))
  })

  test("runs round-trip through the results directory; errors are not samples", async () => {
    const results = await scratch()
    await saveRun(stored("1", "passed"), { "transcript.jsonl": "{}\n" }, results)
    await saveRun(stored("2", "error"), {}, results)
    await saveRun(stored("3", "failed"), {}, results)
    const loaded = await loadRuns(results)
    expect(loaded.map((item) => item.id)).toEqual(["1", "2", "3"])
    expect(samples(loaded).get("k")).toBe(2)
    expect(await readFile(path.join(results, "runs", "1", "transcript.jsonl"), "utf8")).toBe("{}\n")
    expect((await readdir(path.join(results, "runs", "1"))).sort()).toEqual(["run.json", "transcript.jsonl"])
  })

  test("a damaged run.json is skipped with a warning, the others still load, and a failed save leaves the stored run as it was", async () => {
    const results = await scratch()
    await saveRun(stored("1", "passed"), {}, results)
    await saveRun(stored("2", "passed"), {}, results)
    await writeFile(path.join(results, "runs", "2", "run.json"), '{"id": "2", "status": "pass')
    const warn = spyOn(console, "warn").mockImplementation(() => {})
    try {
      expect((await loadRuns(results)).map((item) => item.id)).toEqual(["1"])
      expect(warn.mock.calls.map((call) => String(call[0]))).toEqual([expect.stringContaining(path.join("runs", "2", "run.json"))])
      // An artifact that cannot be written stops the save before run.json, so the run stays as it was and no staged file is left behind.
      await mkdir(path.join(results, "runs", "1", "blocked"))
      await expect(saveRun(stored("1", "failed"), { blocked: "x" }, results)).rejects.toThrow()
      expect((await loadRuns(results))[0]!.status).toBe("passed")
      expect((await readdir(path.join(results, "runs", "1"))).sort()).toEqual(["blocked", "run.json"])
    } finally {
      warn.mockRestore()
    }
  })

  test("a stored run counts under a budget it would have gone the same under, and only on the installed harness version unless any is fine", () => {
    const finished = stored("1", "passed")
    const timedOut: Run = { ...stored("2", "failed"), stopped: "timeout", failure: "timed out after 900s in step 1" }
    const smaller = { timeout: 300_000, maxToolCalls: MAX_TOOL_CALLS }
    const larger = { timeout: 1_800_000, maxToolCalls: MAX_TOOL_CALLS }
    // A run that finished within its limits would have finished under larger ones; one they stopped would have stopped under smaller ones.
    expect([fits(finished, DEFAULT_BUDGET), fits(finished, larger), fits(finished, smaller)]).toEqual([true, true, false])
    expect([fits(timedOut, DEFAULT_BUDGET), fits(timedOut, larger), fits(timedOut, smaller)]).toEqual([true, false, true])
    expect(fits({ ...finished, maxToolCalls: 50 }, DEFAULT_BUDGET)).toBe(true)
    expect(fits({ ...timedOut, maxToolCalls: 50 }, DEFAULT_BUDGET)).toBe(false)
    // Runs from before budgets were recorded count for none.
    expect(fits({ ...finished, timeout: undefined, maxToolCalls: undefined }, DEFAULT_BUDGET)).toBe(false)
    expect([eligible(finished, DEFAULT_BUDGET, { opencode: "1" }), eligible(finished, DEFAULT_BUDGET, { opencode: "2" }), eligible(finished, DEFAULT_BUDGET)]).toEqual([true, false, true])
  })

  test("a task's hashes take in the constants and helpers its functions use, comments aside, and stay put for steps without outside changes", () => {
    const followup = TASKS.find((task) => task.name === "followup")!
    const before = sourceWithUses(followup.steps[1]!.before)
    // The teammate's change uses change(), which uses read(): all three, and the constants, are in.
    for (const part of ['const TEAMMATE_HEADER = "// Patch parsing', "const OLD_HINT =", "const NEW_HINT =", "async function change(", "const read = "]) expect(before).toContain(part)
    expect(sourceWithUses(TASKS.find((task) => task.name === "rename")!.check)).toContain("async function codeFiles(")
    // The doc comment between codeFiles and engineIn belongs to neither, so editing it changes no hash.
    expect(sourceWithUses(TASKS.find((task) => task.name === "rename")!.check)).not.toContain("/** An engine of the finished tree")
    expect(sourceWithUses(undefined)).toBe("")
    const rename = TASKS.find((task) => task.name === "rename")!
    expect(taskHash(rename)).toBe(taskHash({ ...rename, steps: rename.steps.map((step) => ({ ...step, solve: async () => {} })) }))
    expect(checkHash(rename)).not.toBe(checkHash({ ...rename, tests: "test/patch.test.ts" }))
    expect(sourceWithUses(TASKS.find((task) => task.name === "log-hunt")!.check)).not.toContain("const TEAMMATE_HEADER")
  })

  test("reparse reads each step of a stored transcript again, keeping everything else; runs without one stay as stored", async () => {
    const results = await scratch()
    const step = (index: number) => JSON.stringify({ type: "nib-bench-step", step: index, prompt: "p" })
    const result = (turns: number, cost: number) => ({ type: "result", subtype: "success", is_error: false, session_id: "s1", num_turns: turns, total_cost_usd: cost, usage: { input_tokens: 10, output_tokens: 5 } })
    const context = (tokens: number) => ({ input_tokens: 1, cache_read_input_tokens: tokens - 1, cache_creation_input_tokens: 0 })
    const note = '[read takes paths, like ["a.txt"]; filePath was read that way]'
    const transcript = [
      step(1),
      lines(
        { type: "assistant", session_id: "s1", message: { usage: context(1000), content: [{ type: "tool_use", id: "t1", name: "mcp__nib__read", input: {} }] } },
        { type: "user", session_id: "s1", message: { content: [{ type: "tool_result", tool_use_id: "t1", content: [{ type: "text", text: `${note}\n\n[a.txt#1A2B]\n1:x` }] }] } },
        result(2, 0.25),
      ),
      step(2),
      lines(
        { type: "assistant", session_id: "s1", message: { usage: context(1500), content: [{ type: "tool_use", id: "t2", name: "mcp__nib__edit", input: {} }] } },
        { type: "user", session_id: "s1", message: { content: [{ type: "tool_result", tool_use_id: "t2", is_error: true, content: "stale" }] } },
        result(3, 0.5),
      ),
    ].join("\n") + "\n"
    await saveRun({ ...stored("1", "failed"), harness: "claude", failure: "check: wrong" }, { "transcript.jsonl": transcript }, results)
    await saveRun(stored("2", "passed"), {}, results)
    expect(await reparse(results)).toBe(1)
    const [first, second] = await loadRuns(results)
    // Each step's result event carries that prompt's tokens, so the steps add up; Claude Code's cost counts the whole session, so each step adds
    // what it cost beyond the step before. The second step continued the first, so its last call's context already holds the first step's, and that one is the run's.
    expect(first).toMatchObject({
      status: "failed", failure: "check: wrong", turns: 5, cost: 0.5, inputTokens: 20, outputTokens: 10, toolCalls: 2, toolErrors: 1, tools: { "nib.read": 1, "nib.edit": 1 },
      failures: [{ tool: "nib.edit", message: "stale" }], notes: [{ kind: "corrected", tool: "nib.read", message: note }],
      startContextTokens: 1000, contextTokens: 1500, toolChars: { "nib.read": { written: 2, returned: `${note}\n\n[a.txt#1A2B]\n1:x`.length }, "nib.edit": { written: 2, returned: 5 } },
    })
    expect(second).toEqual(stored("2", "passed"))
  })

  test("recheck leaves runs that a limit stopped failed, also older ones that say so only in their failure", async () => {
    const results = await scratch()
    const fixture = (await pristineFixture()).hash
    const timedOut: Run = { ...stored("1", "failed"), fixture, stopped: "timeout", failure: "timed out after 900s in step 1" }
    const legacy: Run = { ...stored("2", "failed"), fixture, failure: "stopped after 150 tool calls in step 2" }
    await saveRun(timedOut, {}, results)
    await saveRun(legacy, {}, results)
    expect(await recheck(TASKS, fixture, results)).toBe(0)
    expect(await loadRuns(results)).toEqual([timedOut, legacy])
  })
})

describe("fixture", () => {
  test("builds byte-identical every time, and a run's changes rebuild its tree", async () => {
    const [one, two] = [await scratch(), await scratch()]
    await writeFixture(one)
    await writeFixture(two)
    expect(await hashTree(one)).toBe(await hashTree(two))
    expect(await hashTree(one)).toBe((await pristineFixture()).hash)
    const work = path.join(await scratch(), "work")
    await applyChanges(work, {})
    // The model works without nib's node_modules link, which only the check gets, and linking twice is harmless.
    await expect(lstat(path.join(work, "node_modules"))).rejects.toThrow()
    await linkModules(work)
    await linkModules(work)
    expect(await readlink(path.join(work, "node_modules"))).toBe(path.join(REPO, "node_modules"))
    await writeFile(path.join(work, "config", "api.json"), "{}\n")
    await writeFile(path.join(work, "notes.txt"), "new\n")
    await rm(path.join(work, "data", "a.csv"))
    const changes = await changedFiles(work)
    expect(changes).toEqual({ "config/api.json": "{}\n", "notes.txt": "new\n", "data/a.csv": null })
    const rebuilt = path.join(await scratch(), "work")
    await applyChanges(rebuilt, changes)
    expect(await hashTree(rebuilt)).toBe(await hashTree(work))
  }, 30_000)
})

describe("verification", () => {
  /** A fresh copy of the fixture with the reference solution of `name` applied, then `edit`. */
  async function solved(name: string, edit: (work: string) => Promise<void> = async () => {}) {
    const work = path.join(await scratch(), "work")
    await copyFixture(work)
    for (const step of TASKS.find((task) => task.name === name)!.steps) {
      await step.before?.(work)
      await step.solve(work)
    }
    await edit(work)
    return work
  }
  const append = (work: string, file: string, text: string) => readFile(path.join(work, file), "utf8").then((before) => writeFile(path.join(work, file), before + text))

  test("confined code sees no network, no HOME and no variables, and can write nowhere it was not given", async () => {
    const writable = await scratch()
    const script = [
      'console.log(await fetch("https://example.com").then(() => "online", () => "offline"))',
      `for (const file of [${JSON.stringify(path.join(REPO, "nib-confined.txt"))}, ${JSON.stringify(path.join(writable, "out.txt"))}]) console.log(await Bun.write(file, "x").then(() => "wrote", () => "refused"))`,
      `console.log((await Array.fromAsync(new Bun.Glob("*").scan({ cwd: ${JSON.stringify(os.homedir())}, onlyFiles: false, dot: true }))).join("|") || "-", Object.keys(process.env).sort().join(","))`,
      "console.log(await Bun.stdin.text())",
    ].join("\n")
    await writeFile(path.join(writable, "probe.ts"), script)
    const child = Bun.spawn(confined([process.execPath, path.join(writable, "probe.ts")], writable, [], [writable]), { stdin: new Blob(["from stdin"]), stdout: "pipe", stderr: "pipe" })
    const [stdout, exit] = await Promise.all([new Response(child.stdout).text(), child.exited])
    // HOME is empty but for the folders leading to the repository, which comes back into view.
    const inside = path.relative(os.homedir(), REPO)
    const home = inside.startsWith("..") ? "-" : inside.split(path.sep)[0]
    expect([exit, stdout.trim().split("\n")]).toEqual([0, ["offline", "refused", "wrote", `${home} HOME,PATH,PWD,TMPDIR`, "from stdin"]])
  })

  test("a tree whose code exits with status 0 before its check finishes fails", async () => {
    const work = await solved("cross-file", (work) => append(work, "src/patch.ts", "\nprocess.exit(0)\n"))
    const verdict = await verify(TASKS.find((task) => task.name === "cross-file")!, work)
    expect(verdict.ok).toBe(false)
    expect(verdict.failure).toContain("check: exited 0 before the check finished")
  }, 60_000)

  test("the tests a task asks for must have a new one that runs, asserts and passes: a reference does, a failing, skipped or empty test does not", async () => {
    const task = TASKS.find((item) => item.name === "cross-file")!
    const reference = (change: (text: string) => string) => solved("cross-file", async (work) => {
      const file = path.join(work, "test/patch.test.ts")
      await writeFile(file, change(await readFile(file, "utf8")))
    })
    expect(await verify(task, await solved("cross-file"))).toEqual({ ok: true })
    const failing = await verify(task, await reference((text) => `${text}\ntest("DEL never passes", () => {\n  expect(1).toBe(2)\n})\n`))
    expect(failing.failure).toContain("tests: test/patch.test.ts has new tests that fail:  > DEL never passes")
    const skipped = await verify(task, await reference((text) => text.replace('test("is an alias for REM"', 'test.skip("is an alias for REM"')))
    expect(skipped.failure).toBe("tests: test/patch.test.ts has no new test that runs and asserts something (1 new, 1 skipped)")
    const empty = await verify(task, await reference((text) => text.replace('expect(parsePatch("[a.ts#ABCD]\\nDEL")[0]).toMatchObject({ remove: true })', "")))
    expect(empty.failure).toBe("tests: test/patch.test.ts has no new test that runs and asserts something (1 new, 0 skipped)")
  }, 120_000)
})

describe("report", () => {
  const current: Current = { fixture: "f", nib: "n1", tasks: { rename: "t" }, budget: DEFAULT_BUDGET, versions: { opencode: "1.18.29" } }
  const run = (fields: Partial<Run>): Run => ({
    task: "rename", taskHash: "t", fixture: "f", harness: "opencode", model: "openrouter/openai/gpt-6-luna", mode: "builtin", setup: SETUP,
    id: Math.random().toString(36).slice(2), key: "k", label: "GPT-6 Luna", status: "passed", checkHash: "c", harnessVersion: "1.18.29", started: "2026-10-14T12:00:00Z", seconds: 10, timeout: 900_000, maxToolCalls: MAX_TOOL_CALLS,
    inputTokens: 1000, outputTokens: 200, reasoningTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0.5, turns: 2, toolCalls: 4, toolErrors: 1, tools: { read: 2 }, failures: [], notes: [], startContextTokens: 2000, contextTokens: 5000, toolChars: { read: { written: 80, returned: 4000 } }, ...fields,
  })

  test("only runs of the current fixture, tasks, setup, nib, budget and harness version count; shell runs are their own row", () => {
    const runs = [
      run({}),
      run({ mode: "nib", nib: "n1", status: "failed", failure: "check: <wrong>" }),
      run({ mode: "nib", nib: "old" }),
      run({ fixture: "other" }),
      run({ status: "error" }),
      run({ shell: true, label: "GPT-6 Luna + shell" }),
      run({ harnessVersion: "1.17.0" }),
      run({ status: "failed", stopped: "timeout", timeout: 60_000, failure: "timed out after 60s in step 1" }),
    ]
    expect(runs.map((item) => isCurrent(item, current))).toEqual([true, true, false, false, true, true, false, false])
    expect(isCurrent(runs[6]!, { ...current, versions: undefined })).toBe(true)
    const html = renderReport(runs, current, new Date("2026-10-14T12:30:00Z"))
    expect(html).toContain("3 runs on fixture <code>f</code> with nib <code>n1</code>; 1 harness or provider errors left out.")
    expect(html).toContain("<td>GPT-6 Luna + shell</td>")
    expect(html).toContain("check: &lt;wrong&gt;")
    expect(html).toContain('<td><code>old</code></td><td>2026-10-14</td>')
    expect(html).not.toMatch(/[^\x00-\x7f]/)
  })

  test("failed tool calls group by message, with run paths and numbers masked", () => {
    const runs = [
      run({ mode: "nib", nib: "n1", label: "MiMo", failures: [{ tool: "nib.edit", message: 'Patch line 2: a single-line replace; got "PUT 1237:"' }] }),
      run({ mode: "nib", nib: "n1", label: "Haiku", failures: [{ tool: "nib.edit", message: 'Patch line 2: a single-line replace; got "PUT 12:"' }] }),
      run({ label: "MiMo", failures: [{ tool: "read", message: "File not found: /tmp/nib-bench-AbC123/work/src/x.ts" }] }),
    ]
    const html = renderReport(runs, current)
    expect(html).toContain("<td><code>nib.edit</code></td><td class=\"failure\">Patch line N: a single-line replace; got &quot;PUT N:&quot;</td><td>0</td><td>2</td><td>MiMo, Haiku</td>")
    expect(html).toContain("File not found: &lt;work&gt;/src/x.ts</td><td>1</td><td>0</td><td>MiMo</td>")
  })

  test("token columns: the first and last call's context once per run, what the run added, and tool characters per run and per call", () => {
    const runs = [
      run({}),
      run({ mode: "nib", nib: "n1", contextTokens: 3000, tools: { "nib.read": 1, "nib.edit": 1 }, toolChars: { "nib.read": { written: 40, returned: 800 }, "nib.edit": { written: 120, returned: 200 } } }),
    ]
    const html = renderReport(runs, current)
    // Output tokens, context at the start and at the end, added during the run, tool characters written / returned, turns, then tool calls.
    expect(html).toContain("<td>200</td><td>2.0K</td><td>5.0K</td><td>3.0K</td><td>80 / 4.0K</td><td>2.0</td><td>4.0</td>")
    expect(html).toContain("<td>200</td><td>2.0K</td><td>3.0K</td><td>1.0K</td><td>160 / 1.0K</td><td>2.0</td><td>4.0</td>")
    expect(html).toContain("<tr><td><code>read</code></td><td>2.00</td><td>0.00</td><td>40</td><td>2.0K</td></tr>")
    expect(html).toContain("<tr><td><code>nib.edit</code></td><td>0.00</td><td>1.00</td><td>120</td><td>200</td></tr>")
  })

  test("corrections and warnings: per-run columns for nib only, and a table grouped by masked message", () => {
    const recovered = "Recovered by remapping stale line numbers to unchanged current lines (file changed since the tagged read). Verify the diff matches your intent."
    const runs = [
      run({}),
      run({ mode: "nib", nib: "n1", label: "MiMo", notes: [
        { kind: "corrected", tool: "nib.read", message: '[read takes paths, like ["src/a.ts:10+5"]; filePath, offset, limit were read that way]' },
        { kind: "warned", tool: "nib.edit", message: recovered },
      ] }),
      run({ mode: "nib", nib: "n1", label: "Haiku", notes: [{ kind: "corrected", tool: "nib.read", message: '[read takes paths, like ["b.ts"]; filePath, offset, limit were read that way]' }] }),
      // A run.json written before notes were recorded.
      { ...run({ mode: "nib", nib: "n1", label: "Haiku" }), notes: undefined } as unknown as Run,
    ]
    const html = renderReport(runs, current)
    expect(html).toContain("<td>4.0</td><td>1.0</td><td>-</td><td>-</td><td>10</td>")
    expect(html).toContain("<td>4.0</td><td>1.0</td><td>0.7</td><td>0.3</td><td>10</td>")
    expect(html).toContain('<tr><td>Corrected</td><td><code>nib.read</code></td><td class="failure">[read takes paths, like [&quot;...&quot;]; filePath, offset, limit were read that way]</td><td>2</td><td>MiMo, Haiku</td></tr>')
    expect(html).toContain(`<tr><td>Warning</td><td><code>nib.edit</code></td><td class="failure">${recovered}</td><td>1</td><td>MiMo</td></tr>`)
    expect(renderReport([run({})], current)).toContain("<h2>Corrections and warnings by message</h2>\n<p class=\"muted\">nib's notes in calls that worked, masked and grouped like the failed calls; the most common 40.</p>\n<p class=\"muted\">None.</p>")
  })

  test("runs group by task kind, and the per-task grid lists the multi-step tasks first", () => {
    const both: Current = { ...current, tasks: { rename: "t", followup: "u" } }
    const html = renderReport([
      run({}),
      run({ task: "followup", taskHash: "u", contextTokens: 9000 }),
      run({ task: "followup", taskHash: "u", mode: "nib", nib: "n1", contextTokens: 7000 }),
    ], both)
    // Pairs, runs, pass rate, cost, output tokens, context at the start, then at the end.
    expect(html).toContain('<tr><td>Multi-step</td><td><i class="key builtin"></i>Built-in tools</td><td>1</td><td>1</td><td>100%</td><td>$0.500</td><td>200</td><td>2.0K</td><td>9.0K</td>')
    expect(html).toContain('<tr><td>Multi-step</td><td><i class="key nib"></i>nib</td><td>1</td><td>1</td><td>100%</td><td>$0.500</td><td>200</td><td>2.0K</td><td>7.0K</td>')
    // The one-shot task has built-in runs only, so there is nothing to compare.
    expect(html).toContain('<tr><td>One-shot</td><td><i class="key builtin"></i>Built-in tools</td><td colspan="15" class="muted">no task with runs in both modes</td></tr>')
    expect(html).toContain('<tr><td>One-shot</td><td><i class="key nib"></i>nib</td><td colspan="15" class="muted">no task with runs in both modes</td></tr>')
    const grid = html.slice(html.indexOf("<h2>Per task</h2>"))
    const at = (text: string) => grid.indexOf(text)
    expect([at("Multi-step tasks</th>"), at(">followup<"), at("One-shot tasks</th>"), at(">rename<")].every((value, index, all) => value > (all[index - 1] ?? -1))).toBe(true)
  })

  test("aggregates weigh every task and model pair the same and leave out pairs with one mode only and shell runs, so uneven reruns cannot tip them", () => {
    const both: Current = { ...current, tasks: { rename: "t", minified: "m", followup: "u" } }
    const repeat = (count: number, fields: Partial<Run>) => Array.from({ length: count }, () => run(fields))
    // rename always passes and minified always fails, whichever the tools; only how often each ran differs.
    const html = renderReport([
      ...repeat(1, {}), ...repeat(9, { task: "minified", taskHash: "m", status: "failed" }),
      ...repeat(9, { mode: "nib", nib: "n1" }), ...repeat(1, { task: "minified", taskHash: "m", mode: "nib", nib: "n1", status: "failed" }),
      run({ task: "followup", taskHash: "u", status: "failed" }),
      ...repeat(3, { mode: "nib", nib: "n1", shell: true, label: "GPT-6 Luna + shell", status: "failed" }),
    ], both)
    expect(html).toContain('<div class="tile-label">Tasks passed</div><div class="tile-values"><span><i class="key builtin"></i>50%</span><span><i class="key nib"></i>50%</span></div>')
    expect(html).toContain("These average the 2 pairs of task and model with runs with both tools, each pair weighing the same, so more runs of an easy task never lift a side; 1 pair with runs of one side only is left out.")
    expect(html).toContain('<tr><td>GPT-6 Luna</td><td><i class="key builtin"></i>Built-in tools</td><td>2</td><td>10</td><td>50%</td>')
    expect(html).toContain('<tr><td>GPT-6 Luna</td><td><i class="key nib"></i>nib</td><td>2</td><td>10</td><td>50%</td>')
  })

  test("the tasks with several prompts are the multi-step ones", () => {
    expect(TASKS.filter((task) => taskKind(task) === "multi-step").map((task) => task.name)).toEqual(["followup", "staged-refactor", "parser-rounds"])
  })

  test("cost ticks show as many decimals as their steps need", () => {
    const html = renderReport([run({ cost: 0.006 }), run({ mode: "nib", nib: "n1", cost: 0.002 })], current)
    for (const tick of ["$0.000", "$0.002", "$0.004", "$0.006", "$0.008"]) expect(html).toContain(`text-anchor="middle">${tick}</text>`)
  })

  test("without current runs it says how to start, also into a results directory that does not exist yet", async () => {
    expect(renderReport([], current)).toContain("No runs for the current fixture, tasks and nib yet.")
    const file = await writeReport(path.join(await scratch(), "fresh", "results"))
    expect(await readFile(file, "utf8")).toContain("No runs for the current fixture, tasks and nib yet.")
  })
})
