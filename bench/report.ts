#!/usr/bin/env bun
/** Turns the stored runs into one self-contained HTML report: bun bench/report.ts [results directory]. */
import { mkdir, writeFile } from "node:fs/promises"
import path from "node:path"
import { pristineFixture } from "./fixture"
import { MODELS } from "./models"
import { DEFAULT_BUDGET, harnessVersion, OUTPUT_LOG, SETUP, type Budget, type Harness, type Mode, type Note, type ToolChars } from "./runners"
import { currentNib, eligible, loadRuns, RESULTS, taskHash, type Run } from "./store"
import { TASKS, taskKind, type Task, type TaskKind } from "./tasks"

const MODES: Mode[] = ["builtin", "nib"]
const MODE_NAMES: Record<Mode, string> = { builtin: "Built-in tools", nib: "nib" }
// Order matters: the tables list kinds in this order, and naming every kind here makes a new one fail to build until it has a name.
const KIND_NAMES: Record<TaskKind, string> = { "multi-step": "Multi-step", "one-shot": "One-shot", program: "Program output" }
const KINDS = Object.keys(KIND_NAMES) as TaskKind[]
// Naming every note kind here makes a new one fail to build until it has a name.
const NOTE_NAMES: Record<Note["kind"], string> = { corrected: "Corrected", warned: "Warning", cut: "Cut output" }
// The per-run columns that the per-kind and per-model tables share.
const NUMBER_HEADS = "<th>Pairs</th><th>Runs</th><th>Pass rate</th><th>Cost per run</th><th>Output tokens</th><th>Context at the start</th><th>Context at the end</th><th>Added during the run</th><th>Tool characters written / returned</th><th>Turns</th><th>Tool calls</th><th>Failed tool calls</th><th>Corrections</th><th>Warnings</th><th>Seconds</th>"

/** What a run must match to count: the current fixture, tasks, harness setup and nib source, a budget it fits and, unless unset, the installed harness versions. */
export type Current = { fixture: string; nib: string; tasks: Record<string, string>; budget: Budget; versions?: Partial<Record<Harness, string>> }
export type ReportOptions = { budget?: Budget; anyHarnessVersion?: boolean }

export async function currentIdentity(options: ReportOptions = {}): Promise<Current> {
  const versions = options.anyHarnessVersion ? undefined : { claude: await harnessVersion("claude"), opencode: await harnessVersion("opencode") }
  return { fixture: (await pristineFixture()).hash, nib: await currentNib(), tasks: Object.fromEntries(TASKS.map((task) => [task.name, taskHash(task)])), budget: options.budget ?? DEFAULT_BUDGET, versions }
}

export function isCurrent(run: Run, current: Current) {
  return run.fixture === current.fixture && run.setup === SETUP && current.tasks[run.task] === run.taskHash && (run.mode === "builtin" || run.nib === current.nib) && eligible(run, current.budget, current.versions)
}

/**
 * `rate` is the pass rate: passed out of runs for one group of runs, the average of the pairs' rates in a comparison. Every other number covers
 * passed runs only, NaN without one: a run that broke down early adds almost nothing, which would make the side that failed more look cheaper.
 * `logReads` are calls naming a saved output log, `cut` nibash's notes on output it cut, and `nibash` the characters nibash returned.
 */
type Stats = { runs: number; passed: number; rate: number; cost: number; output: number; start: number; context: number; written: number; returned: number; turns: number; calls: number; errors: number; corrected: number; warned: number; seconds: number; logReads: number; cut: number; nibash: number }

// Tool text stays in characters, which are exact for every model; tokens per character depend on each model's tokenizer.
const toolChars = (run: Run, kind: keyof ToolChars) => Object.values(run.toolChars ?? {}).reduce((sum, chars) => sum + chars[kind], 0)

function stats(runs: Run[]): Stats | undefined {
  const counted = runs.filter((run) => run.status !== "error")
  if (!counted.length) return
  const succeeded = counted.filter((run) => run.status === "passed")
  const average = (pick: (run: Run) => number) => succeeded.reduce((sum, run) => sum + pick(run), 0) / succeeded.length
  return {
    runs: counted.length,
    passed: succeeded.length,
    rate: succeeded.length / counted.length,
    cost: average((run) => run.cost),
    output: average((run) => run.outputTokens),
    start: average((run) => run.startContextTokens ?? 0),
    context: average((run) => run.contextTokens ?? 0),
    written: average((run) => toolChars(run, "written")),
    returned: average((run) => toolChars(run, "returned")),
    turns: average((run) => run.turns),
    calls: average((run) => run.toolCalls),
    errors: average((run) => run.toolErrors),
    corrected: average((run) => (run.notes ?? []).filter((note) => note.kind === "corrected").length),
    warned: average((run) => (run.notes ?? []).filter((note) => note.kind === "warned").length),
    seconds: average((run) => run.seconds),
    logReads: average((run) => run.logReads ?? 0),
    cut: average((run) => (run.notes ?? []).filter((note) => note.kind === "cut").length),
    nibash: average((run) => run.toolChars?.["nib.nibash"]?.returned ?? 0),
  }
}

// A row per model setup: the same model with another effort, with the shell on or with another decider arm is its own series.
const series = (run: Run) => [run.model, run.variant ?? "", run.shell ? "shell" : "", run.decider ?? ""].join("|")
const seriesLabel = (run: Run) => run.decider ? `${run.label}, ${run.decider}` : run.label
// The shell setups split by decider arm; the default setup has no shell and so no arm.
const setupOf = (run: Run) => run.shell ? `shell|${run.decider ?? ""}` : ""
const setupName = (setup: string) => setup ? `with a shell${setup.slice(6) ? `, ${setup.slice(6)}` : ""}` : ""

/**
 * Built-in shell runs, which no decider touches, once for each decider arm that nib runs of their model setup have, so every arm is compared
 * with the same built-in runs; other runs as they are.
 */
export function shareBuiltin(runs: Run[]): Run[] {
  const arms = new Map<string, Set<string>>()
  for (const run of runs) {
    if (run.mode !== "nib" || !run.decider) continue
    const base = series({ ...run, decider: undefined })
    arms.set(base, (arms.get(base) ?? new Set()).add(run.decider))
  }
  return runs.flatMap((run) => {
    const shared = run.mode === "builtin" && run.shell ? arms.get(series(run)) : undefined
    return shared ? [...shared].map((decider) => ({ ...run, decider })) : [run]
  })
}

/** The average of every number in `list`, each entry weighing the same, except runs and passed, which are totals. */
function mean(list: Stats[]): Stats {
  const result = {} as Stats
  for (const key of Object.keys(list[0]!) as (keyof Stats)[]) result[key] = list.reduce((sum, value) => sum + value[key], 0) / list.length
  return { ...result, runs: list.reduce((sum, value) => sum + value.runs, 0), passed: list.reduce((sum, value) => sum + value.passed, 0) }
}

/** Groups' numbers as one: the run counts and pass rate over `all`, every other number over `solved`, the groups with a passed run (NaN without one). */
function combine(all: Stats[], solved: Stats[]): Stats {
  const total = mean(all)
  const efficiency = solved.length ? mean(solved) : Object.fromEntries(Object.keys(total).map((key) => [key, NaN])) as Stats
  return { ...efficiency, runs: total.runs, passed: total.passed, rate: total.rate }
}

/** `pairs` have runs in both modes and give the pass rates; `solved` have a passed run in both modes and give every other number. */
type Comparison = { builtin?: Stats; nib?: Stats; pairs: number; solved: number; unpaired: number }

/**
 * Each mode's numbers over the pairs of task and model setup with counted runs in both modes, every pair weighing the same, so neither mode looks
 * better only for having more runs of the easier tasks; `unpaired` counts the pairs left out for having runs in one mode only.
 */
function compare(runs: Run[]): Comparison {
  const groups = new Map<string, Record<Mode, Run[]>>()
  for (const run of runs) {
    if (run.status === "error") continue
    const key = `${run.task}\0${series(run)}`
    const group = groups.get(key) ?? { builtin: [], nib: [] }
    group[run.mode].push(run)
    groups.set(key, group)
  }
  const paired = [...groups.values()].filter((group) => group.builtin.length && group.nib.length)
  const solved = paired.filter((group) => MODES.every((mode) => group[mode].some((run) => run.status === "passed")))
  const side = (mode: Mode): Stats | undefined => paired.length ? combine(paired.map((group) => stats(group[mode])!), solved.map((group) => stats(group[mode])!)) : undefined
  return { builtin: side("builtin"), nib: side("nib"), pairs: paired.length, solved: solved.length, unpaired: groups.size - paired.length }
}

const escape = (text: string) => text.replace(/[&<>"]/g, (char) => `&${({ "&": "amp", "<": "lt", ">": "gt", '"': "quot" } as Record<string, string>)[char]};`)
const percent = (value: number) => `${Math.round(value * 100)}%`
const money = (value: number) => Number.isNaN(value) ? "-" : value >= 1 ? `$${value.toFixed(2)}` : `$${value.toFixed(3)}`
const compact = (value: number) => Number.isNaN(value) ? "-" : value >= 10_000 ? `${Math.round(value / 1000)}K` : value >= 1000 ? `${(value / 1000).toFixed(1)}K` : `${Math.round(value)}`
const fixed = (value: number, digits: number) => Number.isNaN(value) ? "-" : value.toFixed(digits)
/** nib's change against built-in tools as a signed percentage, like -43%; empty when either side is unknown or built-in is zero. */
const delta = (builtin: number, nib: number) => Number.isNaN(builtin) || Number.isNaN(nib) || builtin === 0 ? "" : `${nib >= builtin ? "+" : "-"}${Math.round(Math.abs(nib / builtin - 1) * 100)}%`

/** Rounds a maximum up to a clean axis end with four ticks. */
function niceMax(value: number) {
  if (value <= 0) return 1
  const step = 10 ** Math.floor(Math.log10(value / 4))
  const unit = [1, 2, 2.5, 5, 10].map((factor) => factor * step).find((candidate) => candidate * 4 >= value)!
  return unit * 4
}

/** Horizontal bars, two per model (built-in, then nib), on one axis from zero; `tick` gets the distance between ticks, so it can show the decimals the steps need. */
function barChart(title: string, rows: { label: string; values: Partial<Record<Mode, number>>; tips: Partial<Record<Mode, string>> }[], format: (value: number) => string, fixedMax?: number, tick: (value: number, step: number) => string = format) {
  const labelWidth = 170
  const plotWidth = 380
  const bar = 12
  const band = 2 * bar + 2 + 14
  const top = 22
  const height = top + rows.length * band + 4
  const max = fixedMax ?? niceMax(Math.max(0, ...rows.flatMap((row) => Object.values(row.values).filter((value): value is number => value !== undefined))))
  const x = (value: number) => labelWidth + (value / max) * plotWidth
  const parts: string[] = []
  for (let step = 0; step <= 4; step++) {
    const value = (max / 4) * step
    parts.push(`<line class="grid" x1="${x(value)}" x2="${x(value)}" y1="${top - 4}" y2="${height - 4}"/>`, `<text class="tick" x="${x(value)}" y="12" text-anchor="middle">${escape(tick(value, max / 4))}</text>`)
  }
  rows.forEach((row, index) => {
    const y = top + index * band + 7
    parts.push(`<text class="label" x="${labelWidth - 10}" y="${y + bar + 2}" text-anchor="end">${escape(row.label)}</text>`)
    MODES.forEach((mode, offset) => {
      const value = row.values[mode]
      const barTop = y + offset * (bar + 2)
      if (value === undefined) {
        parts.push(`<text class="value" x="${labelWidth + 4}" y="${barTop + bar - 2}">no runs</text>`)
        return
      }
      const width = Math.max(0, x(value) - labelWidth)
      const radius = Math.min(4, width / 2)
      // Rounded at the data end, square at the baseline.
      const shape = width > 0
        ? `<path class="mark ${mode}" d="M${labelWidth},${barTop}h${width - radius}a${radius},${radius} 0 0 1 ${radius},${radius}v${bar - 2 * radius}a${radius},${radius} 0 0 1 -${radius},${radius}h-${width - radius}z"/>`
        : ""
      parts.push(`<g class="hit" data-tip="${escape(row.tips[mode] ?? "")}"><rect x="${labelWidth}" y="${barTop - 1}" width="${plotWidth + 60}" height="${bar + 2}" fill="transparent"/>${shape}<text class="value" x="${x(value) + 6}" y="${barTop + bar - 2}">${escape(format(value))}</text></g>`)
    })
  })
  parts.push(`<line class="axis" x1="${labelWidth}" x2="${labelWidth}" y1="${top - 4}" y2="${height - 4}"/>`)
  return `<figure class="chart"><figcaption>${escape(title)}</figcaption><svg viewBox="0 0 ${labelWidth + plotWidth + 70} ${height}" role="img" aria-label="${escape(title)}">${parts.join("")}</svg></figure>`
}

/** Two thin bars in a table cell, the same encoding as the charts, with the fractions as text. */
function miniBars(builtin?: Stats, nib?: Stats) {
  const line = (mode: Mode, value?: Stats) => value
    ? `<span class="mini"><span class="track"><span class="mark ${mode}" style="width:${Math.round((value.passed / value.runs) * 100)}%"></span></span>${value.passed}/${value.runs}</span>`
    : `<span class="mini muted">-</span>`
  return `<td class="cell" data-tip="${escape(`built-in ${builtin ? `${builtin.passed}/${builtin.runs}` : "no runs"}, nib ${nib ? `${nib.passed}/${nib.runs}` : "no runs"}`)}">${line("builtin", builtin)}${line("nib", nib)}</td>`
}

/** The report for `runs`, comparing built-in tools and nib per model under the `current` identity. */
export function renderReport(runs: Run[], current: Current, generated = new Date()) {
  const real = runs.filter((run) => isCurrent(run, current))
  // Comparisons see each built-in shell run once per decider arm; counts of runs, tools and notes see each run once.
  const live = shareBuiltin(real)
  const counted = real.filter((run) => run.status !== "error")
  const known = MODELS.map((model) => model.model)
  const order = (key: string) => (known.indexOf(key.split("|")[0]!) + known.length + 1) % (known.length + 1)
  const models = [...new Set(live.map(series))].sort((a, b) => order(a) - order(b) || a.localeCompare(b))
  const label = (key: string) => {
    const run = live.find((item) => series(item) === key)
    return run ? seriesLabel(run) : key
  }
  const pick = (filter: (run: Run) => boolean) => stats(live.filter(filter))
  const byModel = models.map((model) => ({ model, ...compare(live.filter((run) => series(run) === model)) }))
  // Shell runs are a setup of their own, so the headline compares the default setup only; each shell setup, one per decider arm, gets its own row below.
  const overall = compare(live.filter((run) => !run.shell))
  const setups = [...new Set(live.map(setupOf))].sort()
  const errors = real.length - counted.length
  // The pass rate covers every pair with runs in both modes; the other charts cover the pairs both modes solved, and skip a model with none.
  const chartRows = (value: (stats: Stats) => number, tip: (stats: Stats) => string) => byModel.map((row) => ({
    label: label(row.model),
    values: Object.fromEntries(MODES.flatMap((mode) => row[mode] && !Number.isNaN(value(row[mode]!)) ? [[mode, value(row[mode]!)]] : [])),
    tips: Object.fromEntries(MODES.flatMap((mode) => row[mode] ? [[mode, `${label(row.model)}, ${MODE_NAMES[mode]}: ${tip(row[mode]!)} over ${row.pairs} tasks, ${row[mode]!.runs} runs`]] : [])),
  }))
  const tile = (name: string, builtin: string, nib: string, change = "") => `<div class="tile"><div class="tile-label">${name}</div><div class="tile-values"><span><i class="key builtin"></i>${builtin}</span><span><i class="key nib"></i>${nib}</span>${change ? `<span class="change">${change}</span>` : ""}</div></div>`
  const show = (value: Stats | undefined, format: (stats: Stats) => string) => value ? format(value) : "-"
  const shift = (comparison: Comparison, pick: (stats: Stats) => number) => comparison.builtin && comparison.nib ? delta(pick(comparison.builtin), pick(comparison.nib)) : ""
  // Only nib's notes are detected, so built-in tools get no count rather than a misleading zero.
  const cells = (value: Stats | undefined, mode: Mode, comparison: Comparison) => {
    const note = (count: number) => mode === "nib" ? fixed(count, 1) : "-"
    const pairs = comparison.solved === comparison.pairs ? `${comparison.pairs}` : `${comparison.pairs} (${comparison.solved} solved)`
    return value
      ? `<td>${pairs}</td><td>${value.runs}</td><td>${percent(value.rate)}</td><td>${money(value.cost)}</td><td>${compact(value.output)}</td><td>${compact(value.start)}</td><td>${compact(value.context)}</td><td>${compact(value.context - value.start)}</td><td>${compact(value.written)} / ${compact(value.returned)}</td><td>${fixed(value.turns, 1)}</td><td>${fixed(value.calls, 1)}</td><td>${fixed(value.errors, 1)}</td><td>${note(value.corrected)}</td><td>${note(value.warned)}</td><td>${fixed(value.seconds, 0)}</td>`
      : `<td colspan="15" class="muted">no task with runs in both modes</td>`
  }
  const summaryRows = byModel.flatMap((row) => MODES.map((mode) => `<tr><td>${escape(label(row.model))}</td><td><i class="key ${mode}"></i>${MODE_NAMES[mode]}</td>${cells(row[mode], mode, row)}</tr>`))
  const kinds = new Map(TASKS.map((task) => [task.name, taskKind(task)]))
  const kindRows = KINDS.flatMap((kind) => setups.flatMap((setup) => {
    const runsOf = live.filter((run) => setupOf(run) === setup && kinds.get(run.task) === kind)
    if (!runsOf.length) return []
    const comparison = compare(runsOf)
    return MODES.map((mode) => `<tr><td>${KIND_NAMES[kind]}${setup ? `, ${escape(setupName(setup))}` : ""}</td><td><i class="key ${mode}"></i>${MODE_NAMES[mode]}</td>${cells(comparison[mode], mode, comparison)}</tr>`)
  }))
  // nib's change against built-in tools, the main question, per setup and per model; output tokens first, since they cost the most.
  const versusRow = (name: string, comparison: Comparison) => {
    const { builtin, nib } = comparison
    if (!builtin || !nib) return `<tr><td>${escape(name)}</td><td colspan="7" class="muted">no task with runs in both modes</td></tr>`
    const pair = (pick: (stats: Stats) => number, format: (value: number) => string) => `<td>${format(pick(builtin))} / ${format(pick(nib))} <span class="change">${delta(pick(builtin), pick(nib))}</span></td>`
    return `<tr><td>${escape(name)}</td><td>${percent(builtin.rate)} / ${percent(nib.rate)}</td>${pair((value) => value.output, compact)}${pair((value) => value.context - value.start, compact)}${pair((value) => value.context, compact)}${pair((value) => value.cost, money)}${pair((value) => value.calls, (value) => fixed(value, 1))}${pair((value) => value.errors, (value) => fixed(value, 2))}</tr>`
  }
  const versusRows = [
    ...setups.map((setup) => versusRow(setup ? `All models, ${setupName(setup)}` : "All models", setup ? compare(live.filter((run) => setupOf(run) === setup)) : overall)),
    ...byModel.map((row) => versusRow(label(row.model), row)),
  ]
  // Per task and model setup, the numbers that decide the comparison, over passed runs.
  const taskNumberRows = TASKS.flatMap((task) => models.flatMap((model) => {
    const side = (mode: Mode) => pick((run) => run.task === task.name && series(run) === model && run.mode === mode)
    const builtin = side("builtin")
    const nib = side("nib")
    if (!builtin && !nib) return []
    const pass = (value?: Stats) => value ? `${value.passed}/${value.runs}` : "-"
    const both = (pick: (stats: Stats) => number) => `<td>${builtin ? compact(pick(builtin)) : "-"} / ${nib ? compact(pick(nib)) : "-"} <span class="change">${builtin && nib ? delta(pick(builtin), pick(nib)) : ""}</span></td>`
    return [`<tr><td>${escape(task.name)}</td><td>${escape(label(model))}</td><td>${pass(builtin)} / ${pass(nib)}</td>${both((value) => value.output)}${both((value) => value.context - value.start)}${both((value) => value.returned)}</tr>`]
  }))
  const taskRow = (task: Task) => `<tr><th scope="row"><span class="task">${escape(task.name)}</span><span class="muted">${escape(task.about)}</span></th>${models.map((model) => miniBars(
    pick((run) => run.task === task.name && series(run) === model && run.mode === "builtin"),
    pick((run) => run.task === task.name && series(run) === model && run.mode === "nib"),
  )).join("")}</tr>`
  const taskRows = KINDS.flatMap((kind) => {
    const tasks = TASKS.filter((task) => taskKind(task) === kind)
    return tasks.length ? [`<tr class="group"><th colspan="${models.length + 1}">${KIND_NAMES[kind]} tasks</th></tr>`, ...tasks.map(taskRow)] : []
  })
  // Program tasks per decider arm, nib runs only: how often a model went back to a saved log for what a cut left out, against what nibash returned.
  const armRuns = live.filter((run) => run.mode === "nib" && run.decider && kinds.get(run.task) === "program")
  const armRow = (name: string, items: Run[]) => {
    const groups = new Map<string, Run[]>()
    for (const run of items) groups.set(`${run.task}\0${series(run)}`, [...groups.get(`${run.task}\0${series(run)}`) ?? [], run])
    const all = [...groups.values()].map(stats).filter((value): value is Stats => value !== undefined)
    if (!all.length) return []
    const value = combine(all, all.filter((item) => item.passed > 0))
    return [`<tr><td>${escape(name)}</td><td>${all.length}</td><td>${value.runs}</td><td>${percent(value.rate)}</td><td>${fixed(value.logReads, 2)}</td><td>${fixed(value.cut, 1)}</td><td>${compact(value.nibash)}</td><td>${money(value.cost)}</td></tr>`]
  }
  const armRows = [...new Set(armRuns.map((run) => run.decider!))].sort().flatMap((arm) => {
    const mine = armRuns.filter((run) => run.decider === arm)
    return [...armRow(`All models, ${arm}`, mine), ...models.flatMap((model) => armRow(label(model), mine.filter((run) => series(run) === model)))]
  })
  const tools = new Map<string, { calls: Record<Mode, number> } & ToolChars>()
  for (const run of counted) for (const [name, count] of Object.entries(run.tools)) {
    const entry = tools.get(name) ?? { calls: { builtin: 0, nib: 0 }, written: 0, returned: 0 }
    entry.calls[run.mode] += count
    entry.written += run.toolChars?.[name]?.written ?? 0
    entry.returned += run.toolChars?.[name]?.returned ?? 0
    tools.set(name, entry)
  }
  const runsIn = (mode: Mode) => counted.filter((run) => run.mode === mode).length
  const perRun = (mode: Mode, count: number) => runsIn(mode) ? (count / runsIn(mode)).toFixed(2) : "-"
  const perCall = (chars: number, calls: number) => compact(chars / calls)
  const toolRows = [...tools].sort((a, b) => b[1].calls.builtin + b[1].calls.nib - a[1].calls.builtin - a[1].calls.nib).map(([name, entry]) => {
    const calls = entry.calls.builtin + entry.calls.nib
    return `<tr><td><code>${escape(name)}</code></td><td>${perRun("builtin", entry.calls.builtin)}</td><td>${perRun("nib", entry.calls.nib)}</td><td>${perCall(entry.written, calls)}</td><td>${perCall(entry.returned, calls)}</td></tr>`
  })
  // The same message in different runs: paths into a run's copy, quoted path lists and numbers are masked, so only the kind of message counts.
  const shape = (message: string) => message.replace(/\/tmp\/nib-bench-[^/\s]+\/work/g, "<work>").replace(new RegExp(`\\S*${OUTPUT_LOG.source}`, "g"), "<log>").replace(/\["[^\]]*"\]/g, '["..."]').replace(/\d+/g, "N").replace(/\s+/g, " ").trim().slice(0, 200)
  const tally = (pick: (run: Run) => { kind?: Note["kind"]; tool: string; message: string }[]) => {
    const groups = new Map<string, { kind?: Note["kind"]; tool: string; message: string; counts: Record<Mode, number>; models: Set<string> }>()
    for (const run of counted) for (const item of pick(run)) {
      const message = shape(item.message)
      const key = `${item.kind}\0${item.tool}\0${message}`
      const group = groups.get(key) ?? { kind: item.kind, tool: item.tool, message, counts: { builtin: 0, nib: 0 }, models: new Set<string>() }
      group.counts[run.mode]++
      group.models.add(run.label)
      groups.set(key, group)
    }
    return [...groups.values()].sort((a, b) => b.counts.builtin + b.counts.nib - a.counts.builtin - a.counts.nib).slice(0, 40)
  }
  const mistakeRows = tally((run) => run.failures ?? [])
    .map((group) => `<tr><td><code>${escape(group.tool)}</code></td><td class="failure">${escape(group.message)}</td><td>${group.counts.builtin}</td><td>${group.counts.nib}</td><td>${escape([...group.models].join(", "))}</td></tr>`)
  const noteRows = tally((run) => run.notes ?? [])
    .map((group) => `<tr><td>${NOTE_NAMES[group.kind!]}</td><td><code>${escape(group.tool)}</code></td><td class="failure">${escape(group.message)}</td><td>${group.counts.nib}</td><td>${escape([...group.models].join(", "))}</td></tr>`)
  const failed = real.filter((run) => run.status !== "passed").sort((a, b) => b.started.localeCompare(a.started)).slice(0, 100)
  const failureRows = failed.map((run) => `<tr><td>${escape(run.task)}</td><td>${escape(run.label)}</td><td><i class="key ${run.mode}"></i>${MODE_NAMES[run.mode]}</td><td>${run.status}</td><td class="failure">${escape(run.failure ?? "")}</td><td class="links"><a href="runs/${encodeURIComponent(run.id)}/transcript.jsonl">transcript</a> <a href="runs/${encodeURIComponent(run.id)}/changes.diff">diff</a></td></tr>`)
  const history = new Map<string, Run[]>()
  for (const run of runs) {
    if (run.mode !== "nib" || run.fixture !== current.fixture || run.setup !== SETUP || current.tasks[run.task] !== run.taskHash || !run.nib) continue
    history.set(run.nib, [...history.get(run.nib) ?? [], run])
  }
  const historyRows = [...history].map(([nib, items]) => ({ nib, items, first: items.map((run) => run.started).sort()[0]!, value: stats(items) }))
    .sort((a, b) => a.first.localeCompare(b.first))
    .map(({ nib, items, first, value }) => `<tr><td><code>${nib}</code>${nib === current.nib ? " (current)" : ""}</td><td>${first.slice(0, 10)}</td><td>${new Set(items.map((run) => run.model)).size}</td>${value ? `<td>${value.runs}</td><td>${percent(value.rate)}</td><td>${money(value.cost)}</td><td>${compact(value.output)}</td>` : `<td colspan="4" class="muted">only errors</td>`}</tr>`)
  const versions = [...new Set(real.map((run) => `${run.harness} ${run.harnessVersion}`))].sort()
  const legend = `<div class="legend"><span><i class="key builtin"></i>${MODE_NAMES.builtin}</span><span><i class="key nib"></i>${MODE_NAMES.nib}</span></div>`
  const body = !live.length
    ? `<p class="empty">No runs for the current fixture, tasks and nib yet. Run <code>bun bench/run.ts --plan</code> to see what a run would cover.</p>`
    : `<section class="tiles">
  ${tile("Tasks passed", show(overall.builtin, (value) => percent(value.rate)), show(overall.nib, (value) => percent(value.rate)))}
  ${tile("Output tokens per run", show(overall.builtin, (value) => compact(value.output)), show(overall.nib, (value) => compact(value.output)), shift(overall, (value) => value.output))}
  ${tile("Added during the run", show(overall.builtin, (value) => compact(value.context - value.start)), show(overall.nib, (value) => compact(value.context - value.start)), shift(overall, (value) => value.context - value.start))}
  ${tile("Context at the end", show(overall.builtin, (value) => compact(value.context)), show(overall.nib, (value) => compact(value.context)), shift(overall, (value) => value.context))}
  ${tile("Cost per run", show(overall.builtin, (value) => money(value.cost)), show(overall.nib, (value) => money(value.cost)), shift(overall, (value) => value.cost))}
  ${tile("Failed tool calls per run", show(overall.builtin, (value) => fixed(value.errors, 1)), show(overall.nib, (value) => fixed(value.errors, 1)))}
</section>
<p class="muted">These average the ${overall.pairs === 1 ? "1 pair" : `${overall.pairs} pairs`} of task and model with runs with both tools, each pair weighing the same, so more runs of an easy task never lift a side${overall.unpaired ? `; ${overall.unpaired === 1 ? "1 pair with runs of one side only is" : `${overall.unpaired} pairs with runs of one side only are`} left out` : ""}. Shell runs are left out too; they have rows of their own below. Everything but the pass rate counts passed runs only, over the ${overall.solved === 1 ? "1 pair" : `${overall.solved} pairs`} both tools solved at least once, since a run that broke down early adds almost no tokens; "solved" in the tables counts those pairs where it differs.</p>
${legend}
<section class="charts">
  ${barChart("Tasks passed", chartRows((value) => value.rate, (value) => `${percent(value.rate)} passed`), percent, 1)}
  ${barChart("Cost per run", chartRows((value) => value.cost, (value) => `${money(value.cost)} per run`), money, undefined, (value, step) => `$${value.toFixed(Math.max(2, Math.ceil(-Math.log10(step))))}`)}
  ${barChart("Output tokens per run", chartRows((value) => value.output, (value) => `${compact(value.output)} output tokens per run`), compact)}
  ${barChart("Context at the end", chartRows((value) => value.context, (value) => `${compact(value.context)} tokens in the last model call, ${compact(value.start)} of them before any tool ran`), compact)}
  ${barChart("Added during the run", chartRows((value) => value.context - value.start, (value) => `${compact(value.context - value.start)} tokens added by tool calls, results and messages`), compact)}
  ${barChart("Failed tool calls per run", chartRows((value) => value.errors, (value) => `${fixed(value.errors, 2)} failed tool calls per run`), (value) => value.toFixed(1))}
</section>
<h2>nib against built-in tools</h2>
<p class="muted">Built-in tools / nib, then nib's change. Output tokens come first: they are the model's own writing, the slowest and most expensive tokens. Everything but the pass rate covers passed runs of the pairs both tools solved, per run.</p>
<div class="scroll"><table class="numbers"><thead><tr><th>Models</th><th>Pass rate</th><th>Output tokens</th><th>Added during the run</th><th>Context at the end</th><th>Cost per run</th><th>Tool calls</th><th>Failed tool calls</th></tr></thead><tbody>${versusRows.join("")}</tbody></table></div>
<h2>By task kind</h2>
<p class="muted">Multi-step tasks give several prompts on the same files, each building on the last: the repeated editing nib is built for. One-shot tasks need a single prompt. Program-output tasks run builds, tests and scripts and read their output, so they run only with a shell, where nib mode's shell is nibash. The columns are those of the per-model table below.</p>
<div class="scroll"><table class="numbers"><thead><tr><th>Tasks</th><th>Tools</th>${NUMBER_HEADS}</tr></thead><tbody>${kindRows.join("")}</tbody></table></div>
<h2>Per model</h2>
<div class="scroll"><table class="numbers"><thead><tr><th>Model</th><th>Tools</th>${NUMBER_HEADS}</tr></thead><tbody>${summaryRows.join("")}</tbody></table></div>
<p class="muted">A pair is one task with one model setup. Only pairs with runs with both tools count, each the same: runs is their total, and every other number averages the pairs' own averages, the pass rate included. All columns after the pass rate are per run, and a turn is one model call. Context at the start is the input of the first model call, cache included: system prompt, tool definitions and prompt. Context at the end is the input of the last call: everything the model had in front of it by then, counted once, so an extra call is no penalty in itself; the cost column still pays for every call. Added during the run is the difference: tool calls, tool results and the model's own messages. Tool characters are the arguments the model wrote to tools and the results it got back, exact for every model; a token is about 2 to 3.5 characters of code. A correction is a call nib accepted after fixing its arguments or form, saying how to write it next time; a warning flags something in a call that worked, like an edit recovered from a stale tag. These count notes, so one call can add more than one.</p>
<h2>Per task</h2>
<p class="muted">Passed runs out of all runs; the upper bar is built-in tools, the lower one nib.</p>
<div class="scroll"><table class="grid-table"><thead><tr><th>Task</th>${models.map((model) => `<th>${escape(label(model))}</th>`).join("")}</tr></thead><tbody>${taskRows.join("")}</tbody></table></div>
<h2>Per task, in numbers</h2>
<p class="muted">Built-in tools / nib per task and model setup, then nib's change: passed out of all runs, and per passed run the output tokens, the context added during the run and the characters tools returned.</p>
<div class="scroll"><table class="numbers"><thead><tr><th>Task</th><th>Model</th><th>Passed</th><th>Output tokens</th><th>Added during the run</th><th>Characters returned</th></tr></thead><tbody>${taskNumberRows.join("")}</tbody></table></div>
${armRows.length ? `<h2>Program output by decider</h2>
<p class="muted">nib runs of program tasks, per decider arm: the decider chooses which rows of a long output nibash shows. Log reads are calls naming a saved output log, the follow-up a model makes when a cut left out what it needed, so fewer is better; cut notes are nibash's notes on output it cut; nibash characters are what the shell returned. The pass rate covers all runs, the rest passed runs, averaged over pairs of task and model setup.</p>
<div class="scroll"><table class="numbers"><thead><tr><th>Models and decider</th><th>Pairs</th><th>Runs</th><th>Pass rate</th><th>Log reads</th><th>Cut notes</th><th>nibash characters returned</th><th>Cost per run</th></tr></thead><tbody>${armRows.join("")}</tbody></table></div>` : ""}
<h2>Tools</h2>
<p class="muted">Calls per run in each mode, and the characters each call wrote to the tool and got back from it.</p>
<div class="scroll"><table class="numbers narrow"><thead><tr><th>Tool</th><th><i class="key builtin"></i>${MODE_NAMES.builtin}</th><th><i class="key nib"></i>${MODE_NAMES.nib}</th><th>Characters written per call</th><th>Characters returned per call</th></tr></thead><tbody>${toolRows.join("")}</tbody></table></div>
<h2>Failed tool calls by message</h2>
<p class="muted">Paths into a run's copy, quoted path lists and numbers are masked, so the same mistake groups together; the most common 40.</p>
${mistakeRows.length ? `<div class="scroll"><table class="numbers"><thead><tr><th>Tool</th><th>Message</th><th><i class="key builtin"></i>${MODE_NAMES.builtin}</th><th><i class="key nib"></i>${MODE_NAMES.nib}</th><th>Models</th></tr></thead><tbody>${mistakeRows.join("")}</tbody></table></div>` : `<p class="muted">None.</p>`}
<h2>Notes by message</h2>
<p class="muted">nib's notes in calls that worked (corrections, warnings and nibash's notes on output it cut), masked and grouped like the failed calls, saved logs as &lt;log&gt;; the most common 40.</p>
${noteRows.length ? `<div class="scroll"><table class="numbers"><thead><tr><th>Kind</th><th>Tool</th><th>Message</th><th>Times</th><th>Models</th></tr></thead><tbody>${noteRows.join("")}</tbody></table></div>` : `<p class="muted">None.</p>`}
<h2>nib versions</h2>
<p class="muted">nib runs on this fixture by nib source version; the model mix can differ between versions.</p>
<div class="scroll"><table class="numbers narrow"><thead><tr><th>nib source</th><th>First run</th><th>Models</th><th>Runs</th><th>Pass rate</th><th>Cost per run</th><th>Output tokens</th></tr></thead><tbody>${historyRows.join("")}</tbody></table></div>
<h2>Failed runs</h2>
${failureRows.length ? `<div class="scroll"><table class="failures"><thead><tr><th>Task</th><th>Model</th><th>Tools</th><th>Status</th><th>Why</th><th></th></tr></thead><tbody>${failureRows.join("")}</tbody></table></div>` : `<p class="muted">None.</p>`}`
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>nib benchmark</title>
<style>
.viz-root {
  color-scheme: light;
  --page: #f9f9f7; --surface: #fcfcfb; --ink: #0b0b0b; --ink-2: #52514e; --muted: #898781;
  --grid: #e1e0d9; --axis: #c3c2b7; --ring: rgba(11, 11, 11, 0.10);
  --builtin: #2a78d6; --nib: #eb6834;
}
@media (prefers-color-scheme: dark) {
  :root:where(:not([data-theme="light"])) .viz-root {
    color-scheme: dark;
    --page: #0d0d0d; --surface: #1a1a19; --ink: #f0efec; --ink-2: #c3c2b7; --muted: #898781;
    --grid: #2c2c2a; --axis: #383835; --ring: rgba(255, 255, 255, 0.10);
    --builtin: #3987e5; --nib: #d95926;
  }
}
:root[data-theme="dark"] .viz-root {
  color-scheme: dark;
  --page: #0d0d0d; --surface: #1a1a19; --ink: #f0efec; --ink-2: #c3c2b7; --muted: #898781;
  --grid: #2c2c2a; --axis: #383835; --ring: rgba(255, 255, 255, 0.10);
  --builtin: #3987e5; --nib: #d95926;
}
body { margin: 0; }
.viz-root { background: var(--page); color: var(--ink); font: 14px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; min-height: 100vh; padding: 32px; box-sizing: border-box; }
h1 { font-size: 22px; font-weight: 600; margin: 0 0 4px; }
h2 { font-size: 16px; font-weight: 600; margin: 36px 0 8px; }
.sub, .muted { color: var(--ink-2); }
.muted { font-size: 13px; }
header { display: flex; justify-content: space-between; align-items: start; gap: 16px; }
button { font: inherit; color: var(--ink-2); background: var(--surface); border: 1px solid var(--ring); border-radius: 6px; padding: 4px 10px; cursor: pointer; }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 12px; margin: 24px 0 16px; }
.tile { background: var(--surface); border: 1px solid var(--ring); border-radius: 8px; padding: 14px 16px; }
.tile-label { color: var(--ink-2); font-size: 13px; }
.tile-values { display: flex; flex-wrap: wrap; column-gap: 14px; font-size: 20px; font-weight: 600; margin-top: 4px; }
.tile-values span { white-space: nowrap; }
.change { color: var(--ink-2); font-size: 12px; font-weight: 400; white-space: nowrap; }
.tile-values .change { font-size: 14px; align-self: center; }
.key { display: inline-block; width: 10px; height: 10px; border-radius: 2px; margin-right: 6px; vertical-align: baseline; }
.key.builtin, .mark.builtin { background: var(--builtin); fill: var(--builtin); }
.key.nib, .mark.nib { background: var(--nib); fill: var(--nib); }
.legend { display: flex; gap: 18px; color: var(--ink-2); margin: 8px 0; }
.charts { display: grid; grid-template-columns: repeat(auto-fit, minmax(460px, 1fr)); gap: 12px; }
.chart { margin: 0; background: var(--surface); border: 1px solid var(--ring); border-radius: 8px; padding: 14px 16px 8px; }
figcaption { font-weight: 600; margin-bottom: 6px; }
svg { width: 100%; height: auto; display: block; }
svg text { font: 12px system-ui, -apple-system, "Segoe UI", sans-serif; }
.label { fill: var(--ink); }
.value { fill: var(--ink-2); font-variant-numeric: tabular-nums; }
.tick { fill: var(--muted); font-variant-numeric: tabular-nums; }
.grid { stroke: var(--grid); stroke-width: 1; }
.axis { stroke: var(--axis); stroke-width: 1; }
.hit:hover .mark { opacity: 0.85; }
.scroll { overflow-x: auto; }
table { border-collapse: collapse; background: var(--surface); border: 1px solid var(--ring); border-radius: 8px; }
th, td { padding: 6px 10px; text-align: left; border-bottom: 1px solid var(--grid); vertical-align: top; }
thead th { color: var(--ink-2); font-weight: 600; font-size: 13px; white-space: nowrap; }
/* Headers of number tables may wrap, so many columns still fit next to their unwrapped values. */
.numbers thead th { white-space: normal; vertical-align: bottom; }
.numbers td { font-variant-numeric: tabular-nums; white-space: nowrap; }
.task { display: block; font-weight: 600; }
.grid-table th[scope="row"] { min-width: 220px; font-weight: normal; }
.grid-table .group th { font-size: 12px; font-weight: 600; color: var(--ink-2); background: var(--page); }
.cell { min-width: 96px; }
.mini { display: flex; align-items: center; gap: 6px; font-size: 12px; font-variant-numeric: tabular-nums; color: var(--ink-2); }
.track { display: inline-block; width: 56px; height: 6px; background: var(--grid); border-radius: 3px; overflow: hidden; }
.track .mark { display: block; height: 100%; border-radius: 0 3px 3px 0; }
/* Messages wrap even inside .numbers tables, whose other cells do not. */
.failure, .numbers .failure { max-width: 560px; white-space: pre-wrap; font-size: 12px; color: var(--ink-2); }
.links a { color: var(--ink); margin-right: 8px; }
.empty { background: var(--surface); border: 1px solid var(--ring); border-radius: 8px; padding: 24px; }
#tip { position: fixed; pointer-events: none; background: var(--surface); color: var(--ink); border: 1px solid var(--ring); border-radius: 6px; padding: 6px 10px; font-size: 12px; box-shadow: 0 2px 8px rgba(0, 0, 0, 0.12); display: none; max-width: 320px; }
</style>
</head>
<body>
<div class="viz-root">
<header>
  <div>
    <h1>nib benchmark</h1>
    <div class="sub">${counted.length} runs on fixture <code>${current.fixture}</code> with nib <code>${current.nib}</code>${errors ? `; ${errors} harness or provider errors left out` : ""}. Generated ${escape(generated.toISOString().slice(0, 16).replace("T", " "))} UTC.</div>
    <div class="muted">${escape(versions.join(", ") || "no harness versions yet")}</div>
  </div>
  <button type="button" id="theme">Toggle theme</button>
</header>
${body}
<div id="tip" role="tooltip"></div>
</div>
<script>
const tip = document.getElementById("tip")
document.addEventListener("mousemove", (event) => {
  const target = event.target.closest && event.target.closest("[data-tip]")
  if (!target || !target.dataset.tip) { tip.style.display = "none"; return }
  tip.textContent = target.dataset.tip
  tip.style.display = "block"
  tip.style.left = Math.min(event.clientX + 14, innerWidth - tip.offsetWidth - 8) + "px"
  tip.style.top = (event.clientY + 14) + "px"
})
document.getElementById("theme").addEventListener("click", () => {
  const root = document.documentElement
  const dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches
  root.dataset.theme = dark ? "light" : "dark"
})
</script>
</body>
</html>
`
}

/** Writes report.html into the results directory and returns its path. */
export async function writeReport(results = RESULTS, options: ReportOptions = {}) {
  const file = path.join(results, "report.html")
  await mkdir(results, { recursive: true })
  await writeFile(file, renderReport(await loadRuns(results), await currentIdentity(options)))
  return file
}

if (import.meta.main) console.log(await writeReport(process.argv[2] ?? RESULTS))
