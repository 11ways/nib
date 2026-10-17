import { afterAll, afterEach, describe, expect, spyOn, test } from "bun:test"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { Database } from "bun:sqlite"
import { chmod, lstat, mkdir, mkdtemp, readdir, readFile, readlink, rm, writeFile } from "node:fs/promises"
import { realpathSync } from "node:fs"
import os, { tmpdir } from "node:os"
import path from "node:path"
import { gunzipSync } from "node:zlib"
import { applyChanges, changedFiles, copyFixture, hashTree, linkModules, pristineFixture, REPO, writeFixture } from "../bench/fixture"
import { confined, jobKey, recheck, reparse, verify } from "../bench/jobs"
import { MODELS, selectModels } from "../bench/models"
import { isCurrent, renderReport, writeReport, type Current } from "../bench/report"
import {
  addUsage, claudeSetup, classifyNotes, DECIDER_KINDS, DECIDER_MODEL, deciderArm, deciderOf, DEFAULT_BUDGET, DEFAULT_DECIDER, emptyUsage, Group, harnessEnvironment, MAX_TOOL_CALLS, MESSAGES, nibBinary,
  nibEnvironment, NOTE_PATTERNS, opencodePrompt, opencodeSetup, openrouterKey, openSession, OUTPUT_LOG, parseClaude, parseOpencode, providerLogin, readLines, SETUP, SHARED, toolCallsIn, toolName, type Mode, type Model,
} from "../bench/runners"
import { checkHash, eligible, fits, hash, identity, identityKey, loadRuns, samples, saveRun, taskHash, type Run } from "../bench/store"
import { promptOf, sourceWithUses, TASKS, taskKind } from "../bench/tasks"
import { binary } from "./support/engine"

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
      logReads: 0,
      toolChars: {
        "nib.read": { written: JSON.stringify(args.read).length, returned: failed.length },
        "nib.edit": { written: JSON.stringify(args.patch).length, returned: edited.length },
        edit: { written: JSON.stringify(args.edit).length, returned: recovered.length },
      },
    })
    expect(parseClaude(lines({ type: "result", subtype: "error_during_execution", is_error: true, result: "overloaded" })).error).toBe("error_during_execution: overloaded")
    expect(parseClaude("").error).toBe("no result event")
  })

  test("OpenCode 1 run --format json: step_finish usage summed, tool_use states counted, nib's notes in completed output, tool characters, the last step's context, error events kept", () => {
    const lesson = "The - rows matched the file, so the edit was applied; but nib never needs the old text, since the line numbers already say what a hunk replaces."
    const output = `[a.txt#1A2B]\n2:TWO\n\nWarnings:\n${lesson}\nLeave - rows out next time. This hunk is just:\nPUT 2:\n+TWO`
    const error = "Ripgrep JSON record exceeded 65536 bytes"
    const args = { patch: { input: "[a.txt#1A2B]\nPUT 2:\n-two\n+TWO" }, grep: { pattern: "x" } }
    // Shapes copied from a real OpenCode 1.18 run; stored runs keep such transcripts, and --reparse reads them.
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
      logReads: 0,
      toolChars: { "nib.edit": { written: JSON.stringify(args.patch).length, returned: output.length }, grep: { written: JSON.stringify(args.grep).length, returned: error.length } },
    })
    expect(parseOpencode(lines({ type: "error", sessionID: "ses_1", error: { name: "APIError", data: { message: "free tier refused" } } })).error).toBe("free tier refused")
    expect(parseOpencode("").error).toBe("no model call finished")
  })

  test("OpenCode 2: usage and tool calls come from the server's messages, which hold the model calls the client's events missed; code mode calls count as the tools they ran", () => {
    const warning = "[nope.txt does not exist]"
    const read = `[a.txt#5BF9]\n1:hello\n\n${warning}`
    const refused = "external_directory denied"
    const script = { code: "return await tools.nib.grep({ pattern: \"x\" })" }
    const tokens = (input: number, read: number) => ({ input, output: 10, reasoning: 2, cache: { read, write: 0 } })
    // Shapes copied from a real OpenCode 2.0.30 run: the client's events end before the last model call, which only the messages hold.
    const session = parseOpencode(lines(
      { type: "step_start", sessionID: "ses_2", part: { type: "step-start" } },
      { type: "tool_use", sessionID: "ses_2", part: { type: "tool", tool: "nib_read", state: { status: "completed", input: { paths: ["a.txt", "nope.txt"] }, output: read } } },
      { type: "step_finish", sessionID: "ses_2", part: { type: "step-finish", reason: "tool-calls", cost: 0, tokens: tokens(902, 100) } },
      { type: "text", sessionID: "ses_2", part: { type: "text", text: "done" } },
      { type: MESSAGES, sessionID: "ses_2", messages: [
        { id: "msg_1", type: "user", text: "Do it" },
        { id: "msg_2", type: "assistant", cost: 0.002, tokens: tokens(902, 100), content: [
          { type: "tool", id: "call_1", name: "nib_read", state: { status: "completed", input: { paths: ["a.txt", "nope.txt"] }, content: [{ type: "text", text: read }] } },
          { type: "tool", id: "call_2", name: "read", state: { status: "error", input: { filePath: "/etc/passwd" }, error: { message: refused } } },
        ] },
        { id: "msg_3", type: "assistant", cost: 0.001, tokens: tokens(50, 1000), content: [
          { type: "tool", id: "call_3", name: "execute", state: { status: "completed", input: script, content: [{ type: "text", text: "1:x" }], metadata: { toolCalls: [{ tool: "nib.grep", status: "completed" }, { tool: "nib.read", status: "error" }] } } },
        ] },
        { id: "msg_4", type: "assistant", cost: 0.001, tokens: tokens(5, 1100), content: [{ type: "text", text: "done" }] },
        { id: "msg_5", type: "idle", outcome: "succeeded" },
      ] },
    ))
    expect(session).toEqual({
      inputTokens: 957, outputTokens: 30, reasoningTokens: 6, cacheRead: 2200, cacheWrite: 0, cost: 0.004, turns: 3, toolCalls: 4, toolErrors: 2, tools: { "nib.read": 2, read: 1, "nib.grep": 1 },
      failures: [{ tool: "read", message: refused }, { tool: "nib.read", message: "1:x" }], notes: [{ kind: "warned", tool: "nib.read", message: warning }], sessionId: "ses_2",
      startContextTokens: 1002,
      contextTokens: 1105,
      logReads: 0,
      toolChars: {
        "nib.read": { written: JSON.stringify({ paths: ["a.txt", "nope.txt"] }).length, returned: read.length },
        read: { written: JSON.stringify({ filePath: "/etc/passwd" }).length, returned: refused.length },
        execute: { written: JSON.stringify(script).length, returned: 3 },
      },
    })
    // Without the messages, as when the server could not be asked, the client's events count.
    expect(parseOpencode(lines({ type: "step_finish", sessionID: "ses_2", part: { cost: 0, tokens: tokens(902, 100) } })).turns).toBe(1)
    expect(parseOpencode(lines({ type: MESSAGES, sessionID: "ses_2", messages: [{ id: "msg_1", type: "user" }] })).error).toBe("no model call finished")
  })

  test("tool names line up across harnesses", () => {
    expect(["mcp__nib__grep", "Read", "MultiEdit"].map((name) => toolName("claude", name))).toEqual(["nib.grep", "read", "multiedit"])
    expect(["nib_grep", "read", "patch", "nib.read"].map((name) => toolName("opencode", name))).toEqual(["nib.grep", "read", "patch", "nib.read"])
  })

  test("calls whose arguments name a saved output log count as log reads, in both harnesses; job logs and the project's own logs do not", () => {
    const log = "/tmp/nib-1000/4242-output-3.log"
    const claude = parseClaude(lines(
      { type: "assistant", message: { id: "m1", content: [
        { type: "tool_use", id: "t1", name: "mcp__nib__nibash", input: { command: `sed -n 31,40p ${log}` } },
        { type: "tool_use", id: "t2", name: "mcp__nib__read", input: { paths: [`${log}:31-40`], maxLineLength: 30000 } },
        { type: "tool_use", id: "t3", name: "mcp__nib__read", input: { paths: ["/run/user/1000/nib/4242-job-1.log", "logs/api-2026-10-13.log"] } },
      ] } },
      { type: "result", subtype: "success", num_turns: 1, total_cost_usd: 0, usage: {} },
    ))
    const opencode = parseOpencode(lines({ type: MESSAGES, sessionID: "s", messages: [{ id: "m", type: "assistant", cost: 0, tokens: { input: 1, output: 1 }, content: [
      { type: "tool", name: "nib_grep", state: { status: "completed", input: { pattern: "error", paths: ["/run/user/1000/nib/77-output-12.log"] }, content: [] } },
    ] }] }))
    expect([claude.logReads, opencode.logReads]).toEqual([2, 1])
    const total = emptyUsage()
    addUsage(total, claude)
    addUsage(total, opencode)
    expect(total.logReads).toBe(3)
  })

  test("every note nib prints is classified by kind, and file lines that read like notes are not", async () => {
    const directory = await scratch()
    const file = path.join(directory, "a.txt")
    await writeFile(file, "one\ntwo\nthree\nfour\nfive\n")
    const client = new Client({ name: "test", version: "1" })
    // The shell is on for nibash's notes on cut output, and its logs go to the scratch directory.
    await client.connect(new StdioClientTransport({ command: await binary(), cwd: directory, env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", XDG_STATE_HOME: process.env.XDG_STATE_HOME ?? "", XDG_DATA_HOME: process.env.XDG_DATA_HOME ?? "", XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME ?? "", TMPDIR: directory, NIB_SHELL: "1" }, stderr: "pipe" }))
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
      "warned: Lines 2 and 3 now nearly match: line 2 was kept just above an edit and line 3 is the edit's first new line. If the edit was meant to replace line 2, its range started one line late and overwrote the line below it instead; these hunks, with the new tag, delete line 2 and put that line back:",
    ])
    await writeFile(path.join(directory, "code.ts"), "function f() {\n  return 1\n}\n")
    const code = /#([0-9A-F]{4})\]/.exec(await call("read", { paths: ["code.ts"] }))![1]
    expect(await notes("edit", { input: `[code.ts#${code}]\nPUT 1.=2:\n+function f() {\n+  return 2\n+}` })).toEqual([
      "warned: The edit of lines 1-2 replaced lines 1-3: its last new row repeats line 3, which would have stayed below it with the brackets unbalanced, so the range ended one line early.",
    ])
    const stray = /#([0-9A-F]{4})\]/.exec(await call("read", { paths: ["code.ts"] }))![1]
    expect(await notes("edit", { input: `[code.ts#${stray}]\nPUT 2:\n+  return 3 }` })).toEqual([
      "warned: Brackets no longer balance: before this edit they did, and now the file has 1 more } than {. The new line 2 has 1 more } than {, counted against the lines they replaced; check them and the lines just outside them.",
    ])
    // nibash's notes on cut output: keywords choosing the middle, the fixed cut when they find nothing, and the footer of a row cut for length.
    const decided = await notes("nibash", { command: "seq 1 3000; echo 'error: boom'; seq 5001 5200" })
    expect(decided).toEqual([
      "cut: [nibash: log lines 31-2997 left out]",
      "cut: [nibash: log lines 3005-3101 left out]",
      expect.stringMatching(/^cut: \[nibash: 137 rows shown of 3,201 log lines; keywords chose the middle; whole output: \S+\]$/),
    ])
    // The log path is the form the benchmark counts follow-up reads of.
    const log = /whole output: (\S+)\]$/.exec(decided[2]!)![1]!
    expect([OUTPUT_LOG.test(log), log.startsWith(directory)]).toEqual([true, true])
    expect(await notes("nibash", { command: "seq 1 5000" })).toEqual([expect.stringMatching(/^cut: \[nibash: 300 rows shown of 5,000 log lines; log lines 101-4800 left out; keywords found nothing in them; whole output: \S+\]$/)])
    expect(await notes("nibash", { command: "seq -s x 1 1000" })).toEqual([expect.stringMatching(/^cut: \[cut rows: N is the row's line in the whole output; read \S+:N with maxLineLength to see one whole\]$/)])
    // read gives long file lines a footer of its own, which is no note.
    await writeFile(path.join(directory, "wide.txt"), "x".repeat(2500) + "\n")
    const wide = await call("read", { paths: ["wide.txt"] })
    expect([wide.includes("\n[cut rows: "), classifyNotes("nib.read", wide)]).toEqual([true, []])
    expect(NOTE_PATTERNS.filter(({ pattern }) => !seen.some((note) => new RegExp(pattern.source).test(note.replace(/^\w+: /, ""))))).toEqual([])

    // Read and grep rows start with their line number, so notes quoted in a file never count.
    const quoted = seen.map((note) => note.replace(/^\w+: /, ""))
    await writeFile(path.join(directory, "notes.txt"), quoted.join("\n") + "\n")
    expect(classifyNotes("nib.read", await call("read", { paths: ["notes.txt"] }))).toEqual([])
    expect(classifyNotes("nib.grep", await call("grep", { pattern: "stale", paths: ["notes.txt"] }))).toEqual([])
    await client.close()
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
// OpenCode 2's server answers the API calls the benchmark makes, and keeps each session's messages; its client prints a prompt's events but not
// its usage, which only the messages carry, as when the real client stops listening early. The server keeps what it was sent in $HOME/state.json.
const FAKE_OPENCODE = `#!/usr/bin/env bun
const [command, ...args] = process.argv.slice(2)
const authorization = "Basic " + btoa("opencode:" + process.env.OPENCODE_SERVER_PASSWORD)
const option = (name) => args.includes(name) ? args[args.indexOf(name) + 1] : undefined
if (command === "--version") console.log("opencode v0.0.0-fake")
else if (command === "serve") {
  let turns = 0
  const sessions = new Map()
  const state = { credentials: [], polls: 0, locations: [] }
  const save = () => Bun.write(process.env.HOME + "/state.json", JSON.stringify(state))
  const server = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: async (request) => {
    if (request.headers.get("authorization") !== authorization) return new Response("no", { status: 401 })
    const url = new URL(request.url)
    if (url.pathname === "/api/credential") state.credentials.push(await request.json())
    if (url.pathname === "/api/mcp") {
      state.locations.push(url.searchParams.get("location[directory]"))
      state.polls++
      await save()
      const status = url.searchParams.get("location[directory]").endsWith("broken") ? { status: "failed", error: "spawn ENOENT" } : { status: state.polls < 3 ? "pending" : "connected" }
      return Response.json({ data: [{ name: "nib", status }] })
    }
    if (url.pathname === "/fake/prompt") {
      const { session, prompt } = await request.json()
      const messages = sessions.get(session) ?? []
      sessions.set(session, messages)
      turns++
      messages.push({ id: "msg_u" + turns, type: "user", text: prompt }, { id: "msg_a" + turns, type: "assistant", cost: 0.01, tokens: { input: turns, output: 1, reasoning: 0, cache: { read: 0, write: 0 } }, content: [{ type: "text", text: "turn " + turns }] })
      return Response.json({ pid: process.pid, turn: turns })
    }
    const listed = /^\\/api\\/session\\/([^/]+)\\/message$/.exec(url.pathname)
    if (listed && (sessions.get(listed[1]) ?? []).some((message) => message.text?.startsWith("lost"))) return new Response("gone", { status: 500 })
    if (listed) return Response.json({ data: sessions.get(listed[1]) ?? [], cursor: { next: "end" } })
    await save()
    return Response.json({})
  } })
  await save()
  console.log("server listening on http://127.0.0.1:" + server.port)
} else {
  const session = option("--session") ?? "ses_" + process.pid
  const prompt = args.at(-1)
  const state = await (await fetch(option("--server") + "/fake/prompt", { method: "POST", headers: { authorization }, body: JSON.stringify({ session, prompt }) })).json()
  console.log(JSON.stringify({ type: "step_start", sessionID: session, part: { type: "step-start" } }))
  if (prompt.startsWith("loop")) for (let call = 0; call < 200; call++) console.log(JSON.stringify({ type: "tool_use", sessionID: session, part: { tool: "read", state: { status: "completed", input: {}, output: "x" } } }))
  if (prompt.startsWith("hang")) await Bun.sleep(300_000)
  console.log(JSON.stringify({ type: "text", sessionID: session, part: { text: "server " + state.pid + " turn " + state.turn } }))
}
`

/** A directory holding the fake harnesses: Claude Code to put first on PATH, OpenCode to name in NIB_BENCH_OPENCODE. */
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
    expect([server.command, server.args, server.env]).toEqual([await nibBinary(), [], { NIB_SANDBOX: "1", NIB_DIRECTORY: "/work", NIB_SHELL: "0" }])
    expect(JSON.parse(nib.args[nib.args.indexOf("--settings") + 1]!)).toEqual({ permissions: { blockReadsOutsideWorkingDirectories: true } })
    const builtin = await claudeSetup({ ...claude, shell: true, variant: "high" }, "builtin", "/work", home)
    expect(builtin.args.slice(builtin.args.indexOf("--allowedTools") + 1)).toEqual(["Edit(./**)", "Bash", "--effort", "high"])
    expect(builtin.args[builtin.args.indexOf("--settings") + 1]).toBe(nib.args[nib.args.indexOf("--settings") + 1])
    expect(builtin.args.slice(builtin.args.indexOf("--disallowedTools") + 1, builtin.args.indexOf("--allowedTools"))).toEqual(["Task", "Agent", "WebFetch", "WebSearch"])
    // In the shell series nib mode has nibash as its shell, and the harness's stays off.
    const shelled = await claudeSetup({ ...claude, shell: true }, "nib", "/work", home)
    expect(shelled.args.slice(shelled.args.indexOf("--disallowedTools") + 1, shelled.args.indexOf("--allowedTools"))).toContain("Bash")
    expect(shelled.args.slice(shelled.args.indexOf("--allowedTools") + 1)).toEqual(["mcp__nib__*"])
    expect(JSON.parse(await readFile(path.join(home, "mcp.json"), "utf8")).mcpServers.nib.env.NIB_SHELL).toBe("1")
  })

  test("tool calls are counted per line, and lines are whole however the chunks split them", async () => {
    const call = JSON.stringify({ type: "tool_use", part: { tool: "nib_read" } })
    const calls = JSON.stringify({ type: "assistant", message: { content: [{ type: "text" }, { type: "tool_use" }, { type: "tool_use" }] } })
    expect([toolCallsIn("opencode", call), toolCallsIn("claude", calls), toolCallsIn("claude", call), toolCallsIn("opencode", "not json")]).toEqual([1, 2, 0, 0])
    // An OpenCode code mode call counts as the calls its script made, as the run output nests them, and as one when it made none.
    const script = (toolCalls: object[]) => JSON.stringify({ type: "tool_use", part: { tool: "execute", state: { status: "completed", metadata: { metadata: { toolCalls } } } } })
    expect([toolCallsIn("opencode", script([{ tool: "nib.read" }, { tool: "nib.grep" }, { tool: "nib.read" }])), toolCallsIn("opencode", script([]))]).toEqual([3, 1])
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

  test("OpenCode: one server per job behind a password, nib connected before the first prompt, a client per prompt continuing the first prompt's session, usage from the server's messages, and a limit stops the server too", async () => {
    const bin = await fakeHarnesses()
    await withEnvironment({ NIB_BENCH_OPENCODE: path.join(bin, "opencode"), OPENROUTER_API_KEY: "sk-test" }, async () => {
      const work = await scratch()
      const home = await scratch()
      const session = await openSession(opencode, "nib", work, home, 10_000, 2)
      // The server said nib was pending twice, so the harness asked again until it was connected, about the work directory each time.
      expect(JSON.parse(await readFile(path.join(home, "state.json"), "utf8"))).toEqual({ credentials: [], polls: 3, locations: [work, work, work] })
      const first = await session.prompt("first", 10_000)
      const second = await session.prompt("second", 10_000)
      const server = /server (\d+) turn 1/.exec(first.stdout)?.[1]
      expect(server).toBeDefined()
      expect(second.stdout).toContain(`server ${server} turn 2`)
      expect(first.session.sessionId).toMatch(/^ses_\d+$/)
      expect(second.session.sessionId).toBe(first.session.sessionId)
      // Each prompt reads only its own messages: the client printed no usage, the messages carry it.
      expect([first.session.cost, first.session.inputTokens, second.session.inputTokens, second.session.turns, second.session.error]).toEqual([0.01, 1, 2, 1, undefined])
      expect(JSON.parse(second.stdout.split("\n").at(-1)!)).toMatchObject({ type: MESSAGES, messages: [{ id: "msg_u2" }, { id: "msg_a2" }] })
      await session.close()
      expect(alive(Number(server))).toBe(false)

      const looping = await openSession(opencode, "nib", work, home, 10_000, 1)
      expect((await looping.prompt("loop", 10_000)).capped).toBe(true)
      await looping.close()
      const hanging = await openSession(opencode, "builtin", work, home, 1000, 1)
      const hung = await hanging.prompt("hang", 1000)
      await hanging.close()
      // A stopped prompt still gets the messages the server holds, read before the server is stopped.
      expect([hung.timedOut, hung.session.turns]).toEqual([true, 1])
      // Without the server's messages the counts could be short, so a finished prompt whose messages cannot be read is a harness error.
      const losing = await openSession(opencode, "builtin", work, home, 10_000, 1)
      expect((await losing.prompt("lost", 10_000)).session.error).toStartWith("could not read the session's messages: OpenCode's /api/session/")
      await losing.close()
      // A nib that fails to connect fails the job before any prompt.
      const broken = path.join(await scratch(), "broken")
      await mkdir(broken)
      await expect(openSession(opencode, "nib", broken, home, 10_000, 1)).rejects.toThrow('OpenCode did not connect nib: {"status":"failed","error":"spawn ENOENT"}')
    })
  }, 30_000)

  test("OpenCode: config through the environment, its own HOME and XDG directories, built-in tools and code mode off by permission, nib offered directly, and OpenRouter's key", async () => {
    const home = await scratch()
    await withEnvironment({ NIB_BENCH_OPENCODE: "/fake/opencode", OPENROUTER_API_KEY: "sk-test" }, async () => {
      const nib = await opencodeSetup(opencode, "nib", "/work", home, 60_000)
      expect([nib.args, nib.login]).toEqual([["/fake/opencode", "serve", "--port", "0"], undefined])
      const config = JSON.parse(nib.env.OPENCODE_CONFIG_CONTENT!)
      const denied = (permissions: { action: string; resource: string; effect: string }[]) => {
        // Everything is allowed first, and each later rule denies one action outright, which is what makes OpenCode drop its tool.
        expect(permissions[0]).toEqual({ action: "*", resource: "*", effect: "allow" })
        expect(permissions.slice(1).every((rule) => rule.resource === "*" && rule.effect === "deny")).toBe(true)
        return permissions.slice(1).map((rule) => rule.action)
      }
      expect(denied(config.permissions)).toEqual(["shell", "subagent", "webfetch", "websearch", "question", "skill", "execute", "external_directory", "read", "grep", "glob", "edit"])
      expect(config.mcp.servers.nib).toEqual({ type: "local", command: [await nibBinary()], environment: { NIB_SANDBOX: "1", NIB_DIRECTORY: "/work", NIB_SHELL: "0", HOME: os.homedir() }, codemode: false })
      expect([nib.env.PWD, nib.env.OPENROUTER_API_KEY, nib.env.HOME, nib.env.XDG_CONFIG_HOME, nib.env.XDG_DATA_HOME, nib.env.OPENCODE_SERVER_PASSWORD?.length]).toEqual(["/work", "sk-test", home, path.join(home, "config"), path.join(home, "data"), 36])
      expect(nib.env.OPENCODE_DISABLE_PROJECT_CONFIG).toBe("1")
      const client = opencodePrompt("/fake/opencode", opencode, "http://127.0.0.1:4096", "Do it")
      expect(client.slice(0, -1)).toEqual(["/fake/opencode", "run", "--server", "http://127.0.0.1:4096", "--format", "json", "--model", opencode.model, "--auto"])
      // The task already holds the whole prompt; the runner sends it unchanged.
      expect(client.at(-1)).toBe("Do it")
      expect(opencodePrompt("/fake/opencode", { ...opencode, variant: "high" }, "http://x", "Do it", "ses_1").slice(-5, -1)).toEqual([`${opencode.model}#high`, "--auto", "--session", "ses_1"])
      const builtin = await opencodeSetup({ ...opencode, shell: true }, "builtin", "/work", home, 60_000)
      const builtinConfig = JSON.parse(builtin.env.OPENCODE_CONFIG_CONTENT!)
      expect([denied(builtinConfig.permissions), builtinConfig.mcp]).toEqual([["subagent", "webfetch", "websearch", "question", "skill", "execute", "external_directory"], {}])
      const shelled = JSON.parse((await opencodeSetup({ ...opencode, shell: true }, "nib", "/work", home, 60_000)).env.OPENCODE_CONFIG_CONTENT!)
      expect([denied(shelled.permissions).includes("shell"), shelled.mcp.servers.nib.environment.NIB_SHELL]).toEqual([true, "1"])
    })
  })

  test("OpenCode logins: only the model's provider goes along, from OpenCode 2's database once it has taken in auth.json and from auth.json before, and an OAuth login must outlive the whole job", async () => {
    const data = await scratch()
    await mkdir(path.join(data, "opencode"))
    const later = Date.now() + 86_400_000
    await writeFile(path.join(data, "opencode", "auth.json"), JSON.stringify({ openai: { type: "oauth", access: "a", refresh: "r", expires: later, accountId: "acc" }, openrouter: { type: "api", key: "sk-saved" }, anthropic: { type: "bogus" } }))
    const luna: Model = { harness: "opencode", model: "openai/gpt-6-luna", label: "Luna" }
    await withEnvironment({ NIB_BENCH_OPENCODE: "/fake/opencode", XDG_DATA_HOME: data, OPENAI_API_KEY: "sk-billed", OPENROUTER_API_KEY: undefined }, async () => {
      // OpenCode 2 has not opened the database yet, so auth.json holds the logins, in the form its first start gives them.
      const gpt = await opencodeSetup(luna, "nib", "/work", await scratch(), 60_000)
      expect(gpt.login).toEqual({ integrationID: "openai", label: "OAuth", value: { type: "oauth", methodID: "chatgpt-browser", refresh: "r", access: "a", expires: later, metadata: { accountID: "acc" } }, activate: true })
      expect(gpt.env.OPENAI_API_KEY).toBeUndefined()
      expect((await opencodeSetup(opencode, "nib", "/work", await scratch(), 60_000)).login).toEqual({ integrationID: "openrouter", label: "API key", value: { type: "key", key: "sk-saved" }, activate: true })
      await expect(opencodeSetup(luna, "nib", "/work", await scratch(), 2 * 86_400_000)).rejects.toThrow("before this run could end")
      await expect(providerLogin("google", 60_000)).rejects.toThrow("OpenCode has no google login")
      await expect(providerLogin("anthropic", 60_000)).rejects.toThrow("OpenCode has no anthropic login")
      // OpenCode's own models run their free ones without a login.
      expect(await providerLogin("opencode", 60_000)).toBeUndefined()

      // Once OpenCode 2 has taken auth.json in, its credential table holds the logins, and auth.json may be stale.
      const database = new Database(path.join(data, "opencode", "opencode.db"))
      database.run("CREATE TABLE migration (id TEXT PRIMARY KEY, time_completed INTEGER NOT NULL)")
      database.run("CREATE TABLE credential (id TEXT PRIMARY KEY, integration_id TEXT, label TEXT NOT NULL, value TEXT NOT NULL, active INTEGER, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL)")
      expect((await providerLogin("openrouter", 60_000))?.value.key).toBe("sk-saved")
      database.run("INSERT INTO migration VALUES ('20260805200742_import_legacy_credentials', 1)")
      const credential = (id: string, integration: string, value: object, active: number, updated: number) =>
        database.run("INSERT INTO credential VALUES (?, ?, ?, ?, ?, 1, ?)", [id, integration, "Saved", JSON.stringify(value), active, updated])
      const fresh = { type: "oauth", methodID: "chatgpt-headless", refresh: "r2", access: "a2", expires: later }
      credential("cred_1", "openai", { ...fresh, refresh: "old" }, 0, 5)
      credential("cred_2", "openai", fresh, 1, 2)
      credential("cred_3", "openrouter", { type: "external", methodID: "x" }, 1, 1)
      database.close()
      expect(await providerLogin("openai", 60_000)).toEqual({ integrationID: "openai", label: "Saved", value: fresh, activate: true })
      await expect(providerLogin("openrouter", 60_000)).rejects.toThrow("login is of type external")
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
    const cases = [
      [["--modes", "nbi"], "--modes takes builtin, nib or both"], [["--concurrency", "0"], "--concurrency takes a whole number above 0"], [["--repetitions", "1.5"], "--repetitions takes a whole number above 0"],
      [["--timeout=-3"], "--timeout takes a number above 0"], [["--modez", "nib"], "Unknown option '--modez'"],
      [["--decider", "keywords"], "--decider chooses how nibash cuts output, so it needs --shell"], [["--shell", "--decider", "magic"], '--decider: unknown decider "magic"'],
    ] as const
    for (const [option, message] of cases) {
      const child = Bun.spawn([process.execPath, path.join(REPO, "bench/run.ts"), ...option, "--plan"], { stdout: "pipe", stderr: "pipe" })
      const [stderr, exit] = await Promise.all([new Response(child.stderr).text(), child.exited])
      expect([exit, stderr.split("\n")[0]]).toEqual([2, expect.stringContaining(message)])
    }
  })
})

describe("decider arms", () => {
  const shelled: Model = { harness: "claude", model: "claude-haiku-5-5", label: "Haiku + shell", shell: true }

  test("the decider vocabulary is nib's: the kinds its binary takes, its default kind and its pinned OpenRouter model", async () => {
    const child = Bun.spawn([await binary()], { env: { PATH: process.env.PATH ?? "", NIB_DECIDER: "none" }, stdin: "ignore", stdout: "pipe", stderr: "pipe" })
    const [stderr, exit] = await Promise.all([new Response(child.stderr).text(), child.exited])
    expect([exit, stderr.trim()]).toEqual([1, `nib: NIB_DECIDER is "none"; use one of ${DECIDER_KINDS.join(", ")}`])
    const config = await readFile(path.join(REPO, "internal/decider/config.go"), "utf8")
    const kinds = Object.fromEntries([...config.matchAll(/^\t(\w+)\s+Kind = "(\w+)"$/gm)].map((match) => [match[1]!, match[2]!]))
    expect(Object.values(kinds)).toEqual([...DECIDER_KINDS])
    expect(kinds[/^\tDefaultKind\s+= (\w+)$/m.exec(config)![1]!]).toBe(DEFAULT_DECIDER)
    expect(/^\tDefaultModel\s+= "([^"]+)"$/m.exec(config)![1]).toBe(DECIDER_MODEL)
  })

  test("an arm is a kind, openrouter with the model it asks; only nib mode with a shell has one, so built-in and shell-less jobs are shared by every arm", () => {
    expect(["fixed", "keywords", "openrouter", "openrouter:other/model"].map(deciderArm)).toEqual(["fixed", "keywords", "openrouter:typesafe/jev-1.14", "openrouter:other/model"])
    expect(() => deciderArm("magic")).toThrow('unknown decider "magic"; use one of fixed, keywords, openrouter')
    expect(() => deciderArm("keywords:x")).toThrow("the keywords decider asks no model")
    const fixed = { ...shelled, decider: "fixed" }
    expect([deciderOf(shelled, "nib"), deciderOf(fixed, "nib"), deciderOf(fixed, "builtin"), deciderOf({ ...fixed, shell: false }, "nib")]).toEqual(["keywords", "fixed", undefined, undefined])
    const task = TASKS.find((item) => item.name === "build-log")!
    const key = (mode: Mode, model: Model) => jobKey({ task, model, mode, nib: "n", fixture: "f" })
    expect(key("builtin", fixed)).toBe(key("builtin", { ...shelled, decider: "openrouter" }))
    expect(key("nib", { ...fixed, shell: false })).toBe(key("nib", { ...shelled, shell: false }))
    // The default arm is keywords whether or not the model names it, and every arm is a job of its own.
    expect(key("nib", shelled)).toBe(key("nib", { ...shelled, decider: "keywords" }))
    expect(new Set([key("nib", shelled), key("nib", fixed), key("nib", { ...shelled, decider: "openrouter" })]).size).toBe(3)
  })

  test("nib is told its arm's decider; openrouter also gets its model, consent under the sandbox, the key and an answer cache shared by runs", async () => {
    const plain = { NIB_SANDBOX: "1", NIB_DIRECTORY: "/work", NIB_SHELL: "1" }
    await withEnvironment({ OPENROUTER_API_KEY: "sk-or" }, async () => {
      expect(await nibEnvironment("/work", shelled, "nib")).toEqual({ ...plain, NIB_DECIDER: "keywords" })
      expect(await nibEnvironment("/work", { ...shelled, decider: "fixed" }, "nib")).toEqual({ ...plain, NIB_DECIDER: "fixed" })
      expect(await nibEnvironment("/work", { ...shelled, decider: "openrouter" }, "nib")).toEqual({
        ...plain, NIB_DECIDER: "openrouter", NIB_DECIDER_MODEL: "typesafe/jev-1.14", NIB_DECIDER_SANDBOX: "1", NIB_DECIDER_KEY: "sk-or", NIB_DECIDER_CACHE: path.join(SHARED, "decider"),
      })
      expect(await nibEnvironment("/work", { ...shelled, shell: false, decider: "openrouter" }, "nib")).toEqual({ ...plain, NIB_SHELL: "0" })
      const home = path.join(await scratch(), "home")
      await claudeSetup({ ...shelled, decider: "fixed" }, "nib", "/work", home)
      expect(JSON.parse(await readFile(path.join(home, "mcp.json"), "utf8")).mcpServers.nib.env).toEqual({ ...plain, NIB_DECIDER: "fixed" })
    })
    // Without OPENROUTER_API_KEY the key is OpenCode's OpenRouter login, the one OpenCode runs on OpenRouter use.
    const data = await scratch()
    await mkdir(path.join(data, "opencode"))
    await writeFile(path.join(data, "opencode", "auth.json"), JSON.stringify({ openrouter: { type: "api", key: "sk-saved" } }))
    const mimo: Model = { harness: "opencode", model: "openrouter/xiaomi/mimo-v2.6-flash", label: "MiMo + shell", shell: true, decider: "openrouter" }
    await withEnvironment({ NIB_BENCH_OPENCODE: "/fake/opencode", OPENROUTER_API_KEY: undefined, XDG_DATA_HOME: data }, async () => {
      expect(await openrouterKey()).toBe("sk-saved")
      const setup = await opencodeSetup(mimo, "nib", "/work", await scratch(), 60_000)
      expect(JSON.parse(setup.env.OPENCODE_CONFIG_CONTENT!).mcp.servers.nib.environment).toMatchObject({ NIB_DECIDER: "openrouter", NIB_DECIDER_KEY: "sk-saved" })
    })
    await withEnvironment({ OPENROUTER_API_KEY: undefined, XDG_DATA_HOME: await scratch() }, () => expect(openrouterKey()).rejects.toThrow("OpenCode has no openrouter login"))
  })
})

describe("stored runs", () => {
  const base = { task: "rename", taskHash: "t", fixture: "f", harness: "opencode" as const, model: "m", mode: "nib" as const, nib: "n1" }
  const stored = (id: string, status: Run["status"]): Run => ({
    ...identity(base), id, key: "k", label: "M", status, checkHash: "c", harnessVersion: "1", started: `2026-10-14T12:00:0${id}Z`, seconds: 1, timeout: 900_000, maxToolCalls: MAX_TOOL_CALLS,
    inputTokens: 0, outputTokens: 0, reasoningTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0, turns: 1, toolCalls: 0, toolErrors: 0, tools: {}, failures: [], notes: [], startContextTokens: 0, contextTokens: 0, toolChars: {}, logReads: 0,
  })

  test("identity keys split on everything a model sees, and a builtin job ignores the nib version", async () => {
    const key = identityKey(identity(base))
    expect(identityKey(identity({ ...base, nib: "n2" }))).not.toBe(key)
    expect(identityKey(identity({ ...base, shell: true }))).not.toBe(key)
    expect(identityKey(identity({ ...base, variant: "high" }))).not.toBe(key)
    // The decider joins the key only when set, so the keys of runs without one stay as they were before deciders existed.
    expect(key).toBe(hash(JSON.stringify([base.task, base.taskHash, base.fixture, base.harness, base.model, "", "", base.mode, base.nib, SETUP])))
    const keywords = identityKey(identity({ ...base, shell: true, decider: "keywords" }))
    expect(new Set([identityKey(identity({ ...base, shell: true })), keywords, identityKey(identity({ ...base, shell: true, decider: "fixed" }))]).size).toBe(3)
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

  test("noisy-service and long-run print the offline check's captures byte for byte at their start, and stop failing once solved", async () => {
    const captures = path.join(REPO, "internal/nibash/testdata/captures")
    const cases = [["noisy-service", "scripts/serve.ts", 0, /POST \/v1\/payments 503/], ["long-run", "scripts/check.ts", 1, /✕/]] as const
    for (const [name, script, exit, failure] of cases) {
      const task = TASKS.find((item) => item.name === name)!
      const work = await scratch()
      await mkdir(path.join(work, "config"))
      await mkdir(path.join(work, "src"))
      await task.steps[0]!.before!(work)
      const run = async () => {
        const child = Bun.spawn([process.execPath, script], { cwd: work, stdout: "pipe", stderr: "pipe" })
        const [stdout, stderr, code] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
        return { stdout, stderr, code }
      }
      const start = await run()
      expect([start.code, start.stderr]).toEqual([exit, ""])
      expect(start.stdout === gunzipSync(await readFile(path.join(captures, `${name}.out.gz`))).toString()).toBe(true)
      await task.steps[0]!.solve(work)
      const solved = await run()
      expect([solved.code, failure.test(solved.stdout)]).toEqual([0, false])
    }
  }, 30_000)

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
    // HOME is empty but for the folders leading to the repository and to Bun, ripgrep and git, which come back into view.
    const shown = [REPO, process.execPath, Bun.which("rg"), Bun.which("git")].flatMap((place) => {
      const inside = place ? path.relative(os.homedir(), realpathSync(place)) : ".."
      return inside.startsWith("..") ? [] : [inside.split(path.sep)[0]!]
    })
    const home = [...new Set(shown)].sort().join("|") || "-"
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
    inputTokens: 1000, outputTokens: 200, reasoningTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0.5, turns: 2, toolCalls: 4, toolErrors: 1, tools: { read: 2 }, failures: [], notes: [], startContextTokens: 2000, contextTokens: 5000, toolChars: { read: { written: 80, returned: 4000 } }, logReads: 0, ...fields,
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
    expect(renderReport([run({})], current)).toContain("<h2>Notes by message</h2>\n<p class=\"muted\">nib's notes in calls that worked (corrections, warnings and nibash's notes on output it cut), masked and grouped like the failed calls, saved logs as &lt;log&gt;; the most common 40.</p>\n<p class=\"muted\">None.</p>")
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
    expect(html).toContain('<tr><td>GPT-6 Luna</td><td><i class="key builtin"></i>Built-in tools</td><td>2 (1 solved)</td><td>10</td><td>50%</td>')
    expect(html).toContain('<tr><td>GPT-6 Luna</td><td><i class="key nib"></i>nib</td><td>2 (1 solved)</td><td>10</td><td>50%</td>')
  })

  test("token, cost and tool figures count passed runs only, over the pairs both modes solved", () => {
    const both: Current = { ...current, tasks: { rename: "t", minified: "m" } }
    const html = renderReport([
      run({ contextTokens: 9000 }),
      // A run that broke down in its first response adds almost nothing; it must not make its side look cheaper.
      run({ mode: "nib", nib: "n1", contextTokens: 2100, status: "failed", failure: "stopped after 150 tool calls" }),
      run({ mode: "nib", nib: "n1", contextTokens: 6000 }),
      // Neither mode solved minified, so only its pass rate counts.
      run({ task: "minified", taskHash: "m", status: "failed", contextTokens: 50_000 }),
      run({ task: "minified", taskHash: "m", mode: "nib", nib: "n1", status: "failed", contextTokens: 2000 }),
    ], both)
    expect(html).toContain('<div class="tile-label">Added during the run</div><div class="tile-values"><span><i class="key builtin"></i>7.0K</span><span><i class="key nib"></i>4.0K</span><span class="change">-43%</span></div>')
    expect(html).toContain('<div class="tile-label">Tasks passed</div><div class="tile-values"><span><i class="key builtin"></i>50%</span><span><i class="key nib"></i>25%</span></div>')
    expect(html).toContain("Everything but the pass rate counts passed runs only, over the 1 pair both tools solved at least once")
    const unsolved = renderReport([run({ status: "failed" }), run({ mode: "nib", nib: "n1", status: "failed" })], current)
    expect(unsolved).toContain('<div class="tile-label">Cost per run</div><div class="tile-values"><span><i class="key builtin"></i>-</span><span><i class="key nib"></i>-</span></div>')
    expect(unsolved).not.toContain("NaN")
  })

  test("nib against built-in tools: output tokens first, per setup and model, with nib's change; and per task in numbers", () => {
    const both: Current = { ...current, tasks: { rename: "t", "log-stats": "s" } }
    const html = renderReport([
      run({ outputTokens: 400 }),
      run({ mode: "nib", nib: "n1", outputTokens: 300, contextTokens: 4000 }),
      run({ task: "log-stats", taskHash: "s", shell: true, label: "GPT-6 Luna + shell", outputTokens: 1000 }),
      run({ task: "log-stats", taskHash: "s", shell: true, label: "GPT-6 Luna + shell", mode: "nib", nib: "n1", outputTokens: 500 }),
    ], both)
    expect(html).toContain('<div class="tile-label">Output tokens per run</div><div class="tile-values"><span><i class="key builtin"></i>400</span><span><i class="key nib"></i>300</span><span class="change">-25%</span></div>')
    expect(html).toContain('<tr><td>All models</td><td>100% / 100%</td><td>400 / 300 <span class="change">-25%</span></td><td>3.0K / 2.0K <span class="change">-33%</span></td>')
    expect(html).toContain('<tr><td>All models, with a shell</td><td>100% / 100%</td><td>1.0K / 500 <span class="change">-50%</span></td>')
    expect(html).toContain('<tr><td>GPT-6 Luna + shell</td><td>100% / 100%</td><td>1.0K / 500 <span class="change">-50%</span></td>')
    // Program tasks run only with a shell, and the kind table says so.
    expect(html).toContain('<tr><td>Program output, with a shell</td><td><i class="key builtin"></i>Built-in tools</td>')
    expect(html).not.toContain("<tr><td>Program output</td>")
    expect(html).toContain('<tr><td>log-stats</td><td>GPT-6 Luna + shell</td><td>1/1 / 1/1</td><td>1.0K / 500 <span class="change">-50%</span></td>')
    expect(html).not.toMatch(/[^\x00-\x7f]/)
  })

  test("program tasks need a shell and form their own kind", () => {
    const program = TASKS.filter((task) => task.shell)
    expect(program.map((task) => task.name)).toEqual(["type-errors", "failing-test", "build-log", "log-stats", "noisy-service", "long-run"])
    expect(program.every((task) => taskKind(task) === "program")).toBe(true)
    expect(TASKS.filter((task) => !task.shell).every((task) => taskKind(task) !== "program")).toBe(true)
  })

  test("edit task prompts rule out running programs; program task prompts, which need them, do not", () => {
    const prompts = (shell: boolean) => TASKS.filter((task) => !!task.shell === shell).flatMap((task) => task.steps.map((step) => promptOf(task, step)))
    expect(prompts(false).every((prompt) => prompt.endsWith("Do not run tests, type checks, builds or git commands; just make the edits and stop."))).toBe(true)
    expect(prompts(true).some((prompt) => prompt.includes("Do not run"))).toBe(false)
  })

  test("the tasks with several prompts are the multi-step ones", () => {
    expect(TASKS.filter((task) => taskKind(task) === "multi-step").map((task) => task.name)).toEqual(["followup", "staged-refactor", "parser-rounds"])
  })

  test("decider arms are series of their own, each compared with the same built-in shell runs, which count once, and program tasks show log reads per arm", () => {
    const both: Current = { ...current, tasks: { rename: "t", "build-log": "b" } }
    const shelled = { task: "build-log", taskHash: "b", shell: true, label: "GPT-6 Luna + shell" }
    const closing = "[nibash: 137 rows shown of 3,013 log lines; keywords chose the middle; whole output: /tmp/nib-1000/42-output-1.log]"
    const html = renderReport([
      run({ ...shelled, outputTokens: 1000 }),
      run({ ...shelled, mode: "nib", nib: "n1", decider: "keywords", outputTokens: 500, logReads: 1, notes: [{ kind: "cut", tool: "nib.nibash", message: closing }], toolChars: { "nib.nibash": { written: 50, returned: 6000 } } }),
      run({ ...shelled, mode: "nib", nib: "n1", decider: "openrouter:typesafe/jev-1.14", outputTokens: 250 }),
    ], both)
    expect(html).toContain("3 runs on fixture <code>f</code>")
    expect(html).toContain('<tr><td>All models, with a shell, keywords</td><td>100% / 100%</td><td>1.0K / 500 <span class="change">-50%</span></td>')
    expect(html).toContain('<tr><td>All models, with a shell, openrouter:typesafe/jev-1.14</td><td>100% / 100%</td><td>1.0K / 250 <span class="change">-75%</span></td>')
    expect(html).toContain('<tr><td>GPT-6 Luna + shell, keywords</td><td><i class="key builtin"></i>Built-in tools</td><td>1</td><td>1</td><td>100%</td>')
    expect(html).toContain('<tr><td>Program output, with a shell, openrouter:typesafe/jev-1.14</td><td><i class="key nib"></i>nib</td>')
    // Pairs, runs, pass rate, log reads, cut notes, nibash characters returned and cost per run.
    expect(html).toContain("<tr><td>All models, keywords</td><td>1</td><td>1</td><td>100%</td><td>1.00</td><td>1.0</td><td>6.0K</td><td>$0.500</td></tr>")
    expect(html).toContain("<tr><td>GPT-6 Luna + shell, openrouter:typesafe/jev-1.14</td><td>1</td><td>1</td><td>100%</td><td>0.00</td><td>0.0</td><td>0</td><td>$0.500</td></tr>")
    expect(html).toContain('<tr><td>Cut output</td><td><code>nib.nibash</code></td><td class="failure">[nibash: N rows shown of N,N log lines; keywords chose the middle; whole output: &lt;log&gt;]</td><td>1</td><td>GPT-6 Luna + shell</td></tr>')
    // Without nib runs of any arm there is no table of arms.
    expect(renderReport([run({})], current)).not.toContain("Program output by decider")
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
