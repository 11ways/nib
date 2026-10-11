#!/usr/bin/env bun
/**
 * Runs the same editing tasks through headless Claude Code with the built-in file tools and with nib, then checks and compares them.
 * Usage: NIB_BENCH_SOURCE=/path/to/project bun bench/run.ts [repetitions] [concurrency] [comma-separated task names]
 */
import { mkdtemp, readFile, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"

const SOURCE = process.env.NIB_BENCH_SOURCE ?? (() => { throw new Error("Set NIB_BENCH_SOURCE to the fixture project to copy") })()
const MAIN = path.join(import.meta.dir, "../src/main.ts")
const repetitions = Number(process.argv[2] ?? 2)
const concurrency = Number(process.argv[3] ?? 4)
const only = process.argv[4]?.split(",")
const SUFFIX = "\n\nDo not run tests, type checks, builds or git commands; just make the edits and stop."

const TASK_FIXTURE = `{ id: "t", directory: "/d", description: "x", background: false, state: "completed", run: 1, started: 0, pending: [],
  outcome: { state: "completed", text: "done", usage: { input: 10, output: 5, reasoning: 7, cacheRead: 20, cacheWrite: 3, cost: 0.5 } } }`

type Task = { name: string; prompt: string; check: string }

const TASKS: Task[] = [
  {
    name: "rename",
    prompt: "Rename the function describeError to formatError everywhere in this project (definition and every use).",
    check: `
      const { $ } = await import("bun")
      const left = await $\`rg -c describeError src test\`.nothrow().text()
      if (left.trim()) throw new Error("describeError remains: " + left)
      const used = await $\`rg -o formatError src test\`.nothrow().text()
      if (used.trim().split("\\n").length < 10) throw new Error("formatError occurs too rarely: " + used)
    `,
  },
  {
    name: "format-line",
    prompt: "In src/mcp/server.ts, make formatTask's tokens line also report reasoning tokens, right after output, so it reads like: tokens: input 33 (cached 20), output 5, reasoning 7, cost $0.5000",
    check: `
      const { formatTask } = await import(process.cwd() + "/src/mcp/server.ts")
      const out = formatTask(${TASK_FIXTURE})
      if (!out.includes("tokens: input 33 (cached 20), output 5, reasoning 7, cost $0.5000\\n")) throw new Error(out)
    `,
  },
  {
    name: "cross-file",
    prompt: "Add an exported function cacheHitRatio(usage: Usage): number to src/mcp/tasks.ts that returns cacheRead / (input + cacheRead + cacheWrite), or 0 when that denominator is 0. Then use it in formatTask in src/mcp/server.ts: append \" (cache hit N%)\" to the end of the tokens line, with N = Math.round(ratio * 100). Also add a test for cacheHitRatio to test/mcp-tasks.test.ts, following that file's existing style.",
    check: `
      const { cacheHitRatio } = await import(process.cwd() + "/src/mcp/tasks.ts")
      const { formatTask } = await import(process.cwd() + "/src/mcp/server.ts")
      const usage = { input: 10, output: 0, reasoning: 0, cacheRead: 30, cacheWrite: 0, cost: 0 }
      if (cacheHitRatio(usage) !== 0.75) throw new Error("ratio " + cacheHitRatio(usage))
      if (cacheHitRatio({ ...usage, input: 0, cacheRead: 0 }) !== 0) throw new Error("zero denominator")
      const out = formatTask(${TASK_FIXTURE})
      if (!out.includes("cost $0.5000 (cache hit 61%)\\n")) throw new Error(out)
      const test = await Bun.file("test/mcp-tasks.test.ts").text()
      if (!test.includes("cacheHitRatio")) throw new Error("no test added")
    `,
  },
  {
    name: "large-file",
    prompt: "In src/hashline.ts, make edit previews show 3 lines of context around each change instead of 2, and make the mime detection also recognize BMP images (files starting with the bytes \"BM\") as image/bmp.",
    check: `
      const source = await Bun.file("src/hashline.ts").text()
      if (!/const PREVIEW_CONTEXT = 3\\b/.test(source)) throw new Error("PREVIEW_CONTEXT is not 3")
      if (!source.includes("image/bmp")) throw new Error("no image/bmp")
      if (!/"BM"|0x42, 0x4d|66, 77|0x42.*0x4[dD]/.test(source)) throw new Error("no BM signature check")
    `,
  },
  {
    name: "whole-file",
    prompt: "In src/hashline.ts, add a one-line /** ... */ doc comment directly above every top-level function declaration (function or async function, exported or not) that does not already have a comment on the line directly above it. Do not change anything else.",
    check: `
      const lines = (await Bun.file("src/hashline.ts").text()).split("\\n")
      const missing = lines.flatMap((line, index) => /^(export )?(async )?function /.test(line) && !/^\\s*(\\/\\/|\\*|\\/\\*\\*.*\\*\\/)|\\*\\/\\s*$/.test(lines[index - 1] ?? "") ? [line] : [])
      if (missing.length) throw new Error("undocumented: " + missing.join(" | "))
    `,
  },
]

type Mode = "builtin" | "nib"
type Job = { task: Task; mode: Mode; repetition: number }
type Metrics = {
  task: string; mode: Mode; repetition: number; ok: boolean; failure?: string; seconds: number
  outputTokens: number; inputTokens: number; cacheRead: number; cacheWrite: number; cost: number; turns: number
  toolCalls: number; toolErrors: number; tools: Record<string, number>; directory: string
}

async function prepare() {
  const directory = await mkdtemp(path.join(tmpdir(), "nib-bench-"))
  const copy = Bun.spawn(["rsync", "-a", "--exclude", "node_modules", "--exclude", ".git", `${SOURCE}/`, `${directory}/`])
  if (await copy.exited) throw new Error("rsync failed")
  await symlink(path.join(SOURCE, "node_modules"), path.join(directory, "node_modules"))
  return directory
}

async function run(job: Job): Promise<Metrics> {
  const directory = await prepare()
  const config = path.join(directory, "..", `${path.basename(directory)}.mcp.json`)
  await writeFile(config, JSON.stringify({ mcpServers: job.mode === "nib" ? { nib: { type: "stdio", command: "bun", args: [MAIN], alwaysLoad: true } } : {} }))
  // User settings are skipped so a personal deny of Edit/Write (as in the nib setup itself) cannot disable the baseline.
  const args = ["claude", "-p", job.task.prompt + SUFFIX, "--mcp-config", config, "--strict-mcp-config", "--setting-sources", "project,local", "--output-format", "stream-json", "--verbose", "--no-session-persistence"]
  if (job.mode === "nib") args.push("--disallowedTools", "Edit", "Write", "MultiEdit", "NotebookEdit", "--allowedTools", "mcp__nib__*")
  else args.push("--allowedTools", "Edit", "Write", "MultiEdit")
  const started = performance.now()
  const child = Bun.spawn(args, { cwd: directory, stdin: "ignore", stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
  const seconds = (performance.now() - started) / 1000
  await writeFile(`${directory}.jsonl`, stdout + (stderr ? `\n${stderr}` : ""))
  const metrics: Metrics = {
    task: job.task.name, mode: job.mode, repetition: job.repetition, ok: false, seconds,
    outputTokens: 0, inputTokens: 0, cacheRead: 0, cacheWrite: 0, cost: 0, turns: 0, toolCalls: 0, toolErrors: 0, tools: {}, directory,
  }
  for (const line of stdout.split("\n")) {
    let message: any
    try { message = JSON.parse(line) } catch { continue }
    if (message.type === "assistant") {
      for (const part of message.message.content) {
        if (part.type !== "tool_use") continue
        metrics.toolCalls++
        metrics.tools[part.name] = (metrics.tools[part.name] ?? 0) + 1
      }
    } else if (message.type === "user" && Array.isArray(message.message.content)) {
      for (const part of message.message.content) if (part.type === "tool_result" && part.is_error) metrics.toolErrors++
    } else if (message.type === "result") {
      metrics.turns = message.num_turns ?? 0
      metrics.cost = message.total_cost_usd ?? 0
      metrics.outputTokens = message.usage?.output_tokens ?? 0
      metrics.inputTokens = message.usage?.input_tokens ?? 0
      metrics.cacheRead = message.usage?.cache_read_input_tokens ?? 0
      metrics.cacheWrite = message.usage?.cache_creation_input_tokens ?? 0
    }
  }
  const checks = [
    Bun.spawn(["bun", "--eval", job.task.check], { cwd: directory, stdout: "pipe", stderr: "pipe" }),
    Bun.spawn(["bunx", "tsgo", "--noEmit"], { cwd: directory, stdout: "pipe", stderr: "pipe" }),
  ]
  const outcomes = await Promise.all(checks.map(async (check) => ({ exit: await check.exited, output: await new Response(check.stdout).text() + await new Response(check.stderr).text() })))
  metrics.ok = outcomes.every((outcome) => outcome.exit === 0)
  if (!metrics.ok) metrics.failure = outcomes.map((outcome) => outcome.output.trim()).filter(Boolean).join("\n").slice(0, 600)
  return metrics
}

const jobs: Job[] = []
for (let repetition = 1; repetition <= repetitions; repetition++) {
  for (const task of TASKS.filter((item) => !only || only.includes(item.name))) for (const mode of ["builtin", "nib"] as const) jobs.push({ task, mode, repetition })
}
const results: Metrics[] = []
let next = 0
await Promise.all(Array.from({ length: concurrency }, async () => {
  while (next < jobs.length) {
    const job = jobs[next++]!
    const result = await run(job)
    results.push(result)
    console.error(`${result.task}/${result.mode}#${result.repetition}: ${result.ok ? "ok" : "FAILED"} out=${result.outputTokens} calls=${result.toolCalls} errors=${result.toolErrors} $${result.cost.toFixed(3)} ${result.seconds.toFixed(0)}s`)
  }
}))
const file = path.join(import.meta.dir, `results-${new Date().toISOString().replaceAll(":", "-")}.json`)
await writeFile(file, JSON.stringify(results, null, 2))

const average = (values: number[]) => values.reduce((sum, value) => sum + value, 0) / Math.max(1, values.length)
console.log(`| task | mode | passed | output tokens | input tokens (incl. cache) | tool calls | tool errors | turns | cost $ | seconds |`)
console.log(`| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |`)
for (const task of [...TASKS.map((item) => item.name).filter((name) => results.some((item) => item.task === name)), "all"]) {
  for (const mode of ["builtin", "nib"] as const) {
    const rows = results.filter((item) => item.mode === mode && (task === "all" || item.task === task))
    console.log(`| ${task} | ${mode} | ${rows.filter((row) => row.ok).length}/${rows.length} | ${average(rows.map((row) => row.outputTokens)).toFixed(0)} | ${average(rows.map((row) => row.inputTokens + row.cacheRead + row.cacheWrite)).toFixed(0)} | ${average(rows.map((row) => row.toolCalls)).toFixed(1)} | ${average(rows.map((row) => row.toolErrors)).toFixed(1)} | ${average(rows.map((row) => row.turns)).toFixed(1)} | ${average(rows.map((row) => row.cost)).toFixed(3)} | ${average(rows.map((row) => row.seconds)).toFixed(0)} |`)
  }
}
console.log(`\nRaw results: ${file}`)
for (const failed of results.filter((item) => !item.ok)) console.log(`\nFAILED ${failed.task}/${failed.mode}#${failed.repetition} (${failed.directory}):\n${failed.failure}`)
