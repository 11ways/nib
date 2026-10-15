#!/usr/bin/env bun
/**
 * Runs the benchmark tasks with each harness's own file tools and with nib, for every selected model, and stores each run.
 * A job that stored runs already cover is skipped, so a repeated call only runs what is new or missing.
 */
import path from "node:path"
import { parseArgs } from "node:util"
import { pristineFixture, REPO } from "./fixture"
import { jobKey, recheck, reparse, repoFingerprint, runJob, validate, type Job } from "./jobs"
import { selectModels } from "./models"
import { writeReport } from "./report"
import { harnessVersion, MAX_TOOL_CALLS, type Budget, type Harness, type Mode } from "./runners"
import { currentNib, eligible, loadRuns, RESULTS, samples, type Run } from "./store"
import { TASKS } from "./tasks"

const USAGE = `Usage: bun bench/run.ts [options]

  --models LIST       labels, ids or harness names (claude, opencode), comma-separated; default all of bench/models.ts
  --tasks LIST        task names, comma-separated; default all
  --modes LIST        builtin, nib or both (default)
  --repetitions N     samples wanted per task, model and mode (default 2); stored runs count
  --concurrency N     sessions at once (default 4)
  --timeout SECONDS   per prompt (default 900); a timed-out run counts as failed. Stored runs count when this budget would not have changed them
  --any-harness-version  let stored runs of other Claude Code and OpenCode versions count too (by default only the installed versions' runs do)
  --plan              list what would run and what is stored, run nothing
  --validate          check every task with its reference solution, no model calls
  --recheck           re-run changed checks on stored runs, no model calls
  --reparse           re-read tokens, cost, tool calls, failed calls and notes of stored runs from their transcripts, no model calls
  --report            only rewrite bench/results/report.html
  --keep              keep each run's working directory in the temp directory
  --shell             keep the shell tool on (off by default in both modes); such runs are their own series
  --results DIR       where runs and the report live (default bench/results)`

/** Prints why the options are wrong and exits before anything runs. */
function refuse(message: string): never {
  console.error(`${message}\n\n${USAGE}`)
  process.exit(2)
}

function parseOptions() {
  try {
    return parseArgs({
      options: {
        models: { type: "string" },
        tasks: { type: "string" },
        modes: { type: "string" },
        repetitions: { type: "string", default: "2" },
        concurrency: { type: "string", default: "4" },
        timeout: { type: "string", default: "900" },
        plan: { type: "boolean" },
        validate: { type: "boolean" },
        recheck: { type: "boolean" },
        reparse: { type: "boolean" },
        report: { type: "boolean" },
        keep: { type: "boolean" },
        shell: { type: "boolean" },
        "any-harness-version": { type: "boolean" },
        results: { type: "string" },
        help: { type: "boolean" },
      },
    })
  } catch (error) {
    refuse(error instanceof Error ? error.message : String(error))
  }
}

const { values } = parseOptions()
if (values.help) {
  console.log(USAGE)
  process.exit(0)
}
const list = (value?: string) => value?.split(",").map((item) => item.trim()).filter(Boolean)
const positive = (name: string, value: string, whole: boolean) => {
  const number = Number(value)
  if (!(number > 0) || !Number.isFinite(number) || (whole && !Number.isInteger(number))) refuse(`--${name} takes a ${whole ? "whole " : ""}number above 0, not ${JSON.stringify(value)}`)
  return number
}
const results = values.results ?? RESULTS
const tasks = list(values.tasks) ? TASKS.filter((task) => list(values.tasks)!.includes(task.name)) : TASKS
const unknown = list(values.tasks)?.filter((name) => !TASKS.some((task) => task.name === name)) ?? []
if (unknown.length) refuse(`Unknown tasks: ${unknown.join(", ")}; known: ${TASKS.map((task) => task.name).join(", ")}`)
const modes = [...new Set(list(values.modes) ?? ["builtin", "nib"])] as Mode[]
const wrongModes = modes.filter((mode) => mode !== "builtin" && mode !== "nib")
if (wrongModes.length || !modes.length) refuse(`--modes takes builtin, nib or both, not ${JSON.stringify(values.modes)}`)
const repetitions = positive("repetitions", values.repetitions, true)
const concurrency = positive("concurrency", values.concurrency, true)
const budget: Budget = { timeout: positive("timeout", values.timeout, false) * 1000, maxToolCalls: MAX_TOOL_CALLS }
const anyHarnessVersion = !!values["any-harness-version"]

if (values.validate) {
  const problems = await validate(tasks)
  console.log(problems.length ? `\n${problems.join("\n\n")}` : `\nAll ${tasks.length} tasks fail untouched and pass with their reference solution.`)
  process.exit(problems.length ? 1 : 0)
}
const fixture = (await pristineFixture()).hash
if (values.reparse) console.log(`Reparsed ${await reparse(results)} stored runs.`)
if (values.recheck) console.log(`Rechecked ${await recheck(TASKS, fixture, results)} stored runs.`)
if (values.reparse || values.recheck || values.report) {
  console.log(`Report: ${await writeReport(results, { budget, anyHarnessVersion })}`)
  process.exit(0)
}

const models = selectModels(list(values.models)).map((model) => values.shell ? { ...model, shell: true, label: `${model.label} + shell` } : model)
const harnesses = [...new Set(models.map((model) => model.harness))]
const versions = anyHarnessVersion ? undefined : Object.fromEntries(await Promise.all(harnesses.map(async (harness) => [harness, await harnessVersion(harness)]))) as Partial<Record<Harness, string>>
const nib = await currentNib()
const stored = await loadRuns(results)
const counts = samples(stored.filter((run) => eligible(run, budget, versions)))
// Repetition-major order spreads concurrent sessions over models and providers.
const jobs: Job[] = []
for (let repetition = 1; repetition <= repetitions; repetition++) {
  for (const task of tasks) {
    for (const model of models) {
      for (const mode of modes) {
        const job = { task, model, mode, nib, fixture }
        if ((counts.get(jobKey(job)) ?? 0) < repetition) jobs.push(job)
      }
    }
  }
}

/** Average cost of stored runs like this job (same model, task and mode, any nib version), for estimates. */
function estimate(job: Job) {
  const like = stored.filter((run) => run.model === job.model.model && run.task === job.task.name && run.mode === job.mode && run.status !== "error")
  return like.length ? like.reduce((sum, run) => sum + run.cost, 0) / like.length : undefined
}

console.log(`Fixture ${fixture}, nib ${nib}, ${versions ? Object.entries(versions).map(([harness, version]) => `${harness} ${version}`).join(", ") : "any harness version"}; ${tasks.length} tasks x ${models.length} models x ${modes.length} modes x ${repetitions} repetitions.`)
console.log("| model | harness | to run | stored samples | estimated cost |\n| --- | --- | --- | --- | --- |")
for (const model of models) {
  const mine = jobs.filter((job) => job.model === model)
  const known = mine.map(estimate).filter((cost): cost is number => cost !== undefined)
  const have = tasks.reduce((sum, task) => sum + modes.reduce((inner, mode) => inner + Math.min(repetitions, counts.get(jobKey({ task, model, mode, nib, fixture })) ?? 0), 0), 0)
  const cost = known.length ? `$${known.reduce((sum, value) => sum + value, 0).toFixed(2)}${known.length < mine.length ? ` + ${mine.length - known.length} unknown` : ""}` : mine.length ? "unknown" : "-"
  console.log(`| ${model.label} | ${model.harness} | ${mine.length} | ${have} | ${cost} |`)
}
if (values.plan || !jobs.length) {
  if (!jobs.length) console.log("\nNothing to run; every job has its samples.")
  console.log(`Report: ${await writeReport(results, { budget, anyHarnessVersion })}`)
  process.exit(0)
}

const finished: Run[] = []
let next = 0
let tampered = false
const timeout = budget.timeout
const fingerprint = await repoFingerprint()
await Promise.all(Array.from({ length: Math.min(concurrency, jobs.length) }, async () => {
  while (next < jobs.length && !tampered) {
    const job = jobs[next++]!
    const run = await runJob(job, { timeout, keep: !!values.keep, results })
    finished.push(run)
    const mark = run.status === "passed" ? "ok" : run.status === "failed" ? "FAILED" : "ERROR"
    console.error(`[${finished.length}/${jobs.length}] ${run.task} ${run.label} ${run.mode}: ${mark} out=${run.outputTokens} calls=${run.toolCalls} errors=${run.toolErrors} $${run.cost.toFixed(3)} ${run.seconds.toFixed(0)}s${run.status === "error" ? ` (${run.failure?.slice(0, 160)})` : ""}`)
    // A run must only touch its own copy; a change to nib itself stops everything before more damage is done.
    if (!tampered && await repoFingerprint() !== fingerprint) {
      tampered = true
      console.error(`\nSTOPPED: nib's own working tree changed during the benchmark (last run: ${run.id}). Check git status in ${path.relative(process.cwd(), REPO) || "."}; if you edited nib yourself meanwhile, run again.`)
    }
  }
}))
const errors = finished.filter((run) => run.status === "error").length
console.log(`\n${finished.length} runs: ${finished.filter((run) => run.status === "passed").length} passed, ${finished.filter((run) => run.status === "failed").length} failed, ${errors} errors${errors ? " (errors run again next time)" : ""}; $${finished.reduce((sum, run) => sum + run.cost, 0).toFixed(2)}.`)
console.log(`Report: ${await writeReport(results, { budget, anyHarnessVersion })}`)
if (tampered) process.exit(1)
