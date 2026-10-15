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
import { DEFAULT_DECIDER, deciderArm, harnessVersion, MAX_TOOL_CALLS, openrouterKey, type Budget, type Harness, type Mode, type Model } from "./runners"
import { currentNib, eligible, loadRuns, RESULTS, samples, type Run } from "./store"
import { TASKS, taskKind } from "./tasks"

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
  --decider LIST      with --shell: the decider arms nib mode runs, comma-separated: fixed, keywords, openrouter (typesafe/jev-1.14) or
                      openrouter:MODEL; default ${DEFAULT_DECIDER}, nib's default. Each arm is its own series; built-in runs are shared by all arms
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
        decider: { type: "string" },
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
const shellOnly = tasks.filter((task) => task.shell && list(values.tasks)?.includes(task.name)).map((task) => task.name)
if (shellOnly.length && !values.shell && !values.validate) refuse(`${shellOnly.join(", ")} need a shell, so they run only with --shell`)
const modes = [...new Set(list(values.modes) ?? ["builtin", "nib"])] as Mode[]
const wrongModes = modes.filter((mode) => mode !== "builtin" && mode !== "nib")
if (wrongModes.length || !modes.length) refuse(`--modes takes builtin, nib or both, not ${JSON.stringify(values.modes)}`)
const repetitions = positive("repetitions", values.repetitions, true)
const concurrency = positive("concurrency", values.concurrency, true)
const budget: Budget = { timeout: positive("timeout", values.timeout, false) * 1000, maxToolCalls: MAX_TOOL_CALLS }
const anyHarnessVersion = !!values["any-harness-version"]
if (values.decider !== undefined && !values.shell && !values.validate) refuse("--decider chooses how nibash cuts output, so it needs --shell")
const arms = [...new Set((list(values.decider) ?? [DEFAULT_DECIDER]).map((name) => {
  try {
    return deciderArm(name)
  } catch (error) {
    refuse(`--decider: ${error instanceof Error ? error.message : String(error)}`)
  }
}))]
if (!arms.length) refuse("--decider takes at least one decider")

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
/** The jobs one task and model setup take: one per mode, and in nib mode with a shell one per decider arm, since only nibash cuts output. */
const variants = (model: Model) => modes.flatMap((mode) => mode === "nib" && model.shell ? arms.map((decider) => ({ mode, model: { ...model, decider } })) : [{ mode, model }])
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
      // The default series offers no shell, so a task that runs programs has no fair run there.
      if (task.shell && !model.shell) continue
      for (const variant of variants(model)) {
        const job = { task, ...variant, nib, fixture }
        if ((counts.get(jobKey(job)) ?? 0) < repetition) jobs.push(job)
      }
    }
  }
}

/** Average cost of stored runs like this job (same model, task and mode, any nib version), else of the same model and mode on tasks of the same kind, for estimates. */
function estimate(job: Job) {
  const average = (runs: Run[]) => runs.length ? runs.reduce((sum, run) => sum + run.cost, 0) / runs.length : undefined
  const like = stored.filter((run) => run.model === job.model.model && run.mode === job.mode && run.status !== "error")
  const kind = taskKind(job.task)
  return average(like.filter((run) => run.task === job.task.name)) ?? average(like.filter((run) => TASKS.some((task) => task.name === run.task && taskKind(task) === kind)))
}

const armsNote = models.some((model) => model.shell) && modes.includes("nib") ? `; nib mode with ${arms.length === 1 ? `the ${arms[0]} decider` : `decider arms ${arms.join(", ")}`}` : ""
console.log(`Fixture ${fixture}, nib ${nib}, ${versions ? Object.entries(versions).map(([harness, version]) => `${harness} ${version}`).join(", ") : "any harness version"}; ${tasks.length} tasks x ${models.length} models x ${modes.length} modes x ${repetitions} repetitions${armsNote}.`)
console.log("| model | harness | to run | stored samples | estimated cost |\n| --- | --- | --- | --- | --- |")
for (const model of models) {
  const mine = jobs.filter((job) => job.model.model === model.model && job.model.harness === model.harness && job.model.variant === model.variant)
  const known = mine.map(estimate).filter((cost): cost is number => cost !== undefined)
  const have = tasks.reduce((sum, task) => task.shell && !model.shell ? sum : sum + variants(model).reduce((inner, variant) => inner + Math.min(repetitions, counts.get(jobKey({ task, ...variant, nib, fixture })) ?? 0), 0), 0)
  const cost = known.length ? `$${known.reduce((sum, value) => sum + value, 0).toFixed(2)}${known.length < mine.length ? ` + ${mine.length - known.length} unknown` : ""}` : mine.length ? "unknown" : "-"
  console.log(`| ${model.label} | ${model.harness} | ${mine.length} | ${have} | ${cost} |`)
}
if (values.plan || !jobs.length) {
  if (!jobs.length) console.log("\nNothing to run; every job has its samples.")
  console.log(`Report: ${await writeReport(results, { budget, anyHarnessVersion })}`)
  process.exit(0)
}

// The openrouter decider fails at nib's start without a key, which would turn every such job into an error; this stops before any of them runs.
if (jobs.some((job) => job.model.decider?.startsWith("openrouter:"))) {
  try {
    await openrouterKey()
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exit(2)
  }
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
    console.error(`[${finished.length}/${jobs.length}] ${run.task} ${run.label}${run.decider ? ` (${run.decider})` : ""} ${run.mode}: ${mark} out=${run.outputTokens} calls=${run.toolCalls} log reads=${run.logReads} errors=${run.toolErrors} $${run.cost.toFixed(3)} ${run.seconds.toFixed(0)}s${run.status === "error" ? ` (${run.failure?.slice(0, 160)})` : ""}`)
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
