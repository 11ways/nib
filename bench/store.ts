/** Stored runs, one directory each, and the identity that decides whether a run already covers a job. */
import { createHash, randomUUID } from "node:crypto"
import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises"
import path from "node:path"
import { REPO } from "./fixture"
import { SETUP, type Budget, type Harness, type Mode, type Usage } from "./runners"
import { promptOf, sourceWithUses, type Task } from "./tasks"

export const RESULTS = path.join(import.meta.dir, "results")

/**
 * Everything that shapes what a model sees in a run; runs with equal identities are samples of the same job. `decider` is set only where nibash
 * cuts output (nib mode in the shell series, see deciderOf); runs from before deciders have none and match no arm.
 */
export type Identity = { task: string; taskHash: string; fixture: string; harness: Harness; model: string; variant?: string; shell?: boolean; mode: Mode; nib?: string; setup: number; decider?: string }
/** `failed` means the model's work did not pass; `error` means the harness or provider failed, which never counts as a sample. */
export type Status = "passed" | "failed" | "error"
export type Run = Identity & Usage & {
  id: string
  key: string
  label: string
  status: Status
  failure?: string
  /** Which limit stopped the run, which then failed without its work being checked. */
  stopped?: "timeout" | "cap"
  /** The run's budget; older runs recorded none. */
  timeout?: number
  maxToolCalls?: number
  checkHash: string
  harnessVersion: string
  started: string
  seconds: number
}

/** Bumped whenever verification changes in a way that can turn a verdict, so recheck checks every stored run again; 2 confined it and ran the tests a task asks for. */
export const VERIFY = 2

export function hash(...parts: string[]) {
  const digest = createHash("sha256")
  for (const part of parts) digest.update(part).update("\0")
  return digest.digest("hex").slice(0, 16)
}

/** What the model is given: the prompts and the outside changes between them, constants they use included; a changed check alone keeps results, see recheck. */
export function taskHash(task: Task) {
  return hash(...task.steps.flatMap((step) => [promptOf(task, step), sourceWithUses(step.before)]))
}

export function checkHash(task: Task) {
  return hash(sourceWithUses(task.check), task.tests ?? "", String(VERIFY))
}

let nibHash: Promise<string> | undefined

/** nib's Go source as the model meets it, tool descriptions included, and its module versions; any change makes nib runs new jobs. Tests are left out. */
export function currentNib() {
  return nibHash ??= (async () => {
    const files: string[] = []
    const walk = async (directory: string) => {
      for (const entry of await readdir(path.join(REPO, directory), { withFileTypes: true })) {
        const relative = path.join(directory, entry.name)
        if (entry.isDirectory() && entry.name !== "testdata") await walk(relative)
        else if (entry.isFile() && entry.name.endsWith(".go") && !entry.name.endsWith("_test.go")) files.push(relative)
      }
    }
    await walk("cmd")
    await walk("internal")
    files.sort()
    files.push("go.mod", "go.sum")
    return hash(...await Promise.all(files.map(async (file) => file + "\0" + await readFile(path.join(REPO, file), "utf8"))))
  })()
}

/** A run's key; the decider is appended only when set, so the keys of runs without one stay as they were. */
export function identityKey(identity: Identity) {
  return hash(JSON.stringify([identity.task, identity.taskHash, identity.fixture, identity.harness, identity.model, identity.variant ?? "", identity.shell ? "shell" : "", identity.mode, identity.nib ?? "", identity.setup, ...identity.decider ? [identity.decider] : []]))
}

export function identity(fields: Omit<Identity, "setup">): Identity {
  return { ...fields, setup: SETUP }
}

/** Whether the run's limits stopped it, so its tree is not the model's finished work; older runs say so only in their failure. */
export function stoppedEarly(run: Run) {
  return run.stopped !== undefined || /^(timed out after \d+(\.\d+)?s|stopped after \d+ tool calls) in step \d+$/.test(run.failure ?? "")
}

/**
 * Whether a stored run is a fair sample under `budget`: a run its limits stopped would have stopped under any budget no larger, and any other run
 * would have gone the same under a budget no smaller. Runs without a recorded budget count for none.
 */
export function fits(run: Run, budget: Budget) {
  if (run.timeout === undefined || run.maxToolCalls === undefined) return false
  if (stoppedEarly(run)) return budget.timeout <= run.timeout && budget.maxToolCalls <= run.maxToolCalls
  return budget.timeout >= run.timeout && budget.maxToolCalls >= run.maxToolCalls
}

/** Whether a run counts as a sample now: it fits `budget` and ran on the installed harness version, unless `versions` is unset to take any. */
export function eligible(run: Run, budget: Budget, versions?: Partial<Record<Harness, string>>) {
  return fits(run, budget) && (!versions || versions[run.harness] === run.harnessVersion)
}

/** Every readable run; a damaged run.json is reported and skipped, so the others stay usable. */
export async function loadRuns(results = RESULTS): Promise<Run[]> {
  const directory = path.join(results, "runs")
  const names = await readdir(directory).catch(() => [] as string[])
  const runs = await Promise.all(names.map(async (name) => {
    const text = await readFile(path.join(directory, name, "run.json"), "utf8").catch(() => undefined)
    if (text === undefined) return
    try {
      return JSON.parse(text) as Run
    } catch (error) {
      console.warn(`Skipped ${path.join(directory, name, "run.json")}: ${error instanceof Error ? error.message : String(error)}`)
    }
  }))
  return runs.filter((run): run is Run => run !== undefined).sort((a, b) => a.started.localeCompare(b.started))
}

/** Writes `content` through a temporary sibling and a rename, so an interrupted write never leaves half a file. */
async function publish(file: string, content: string) {
  const staged = `${file}.${randomUUID()}.tmp`
  try {
    await writeFile(staged, content, { flag: "wx" })
    await rename(staged, file)
  } catch (error) {
    await rm(staged, { force: true })
    throw error
  }
}

/** Writes a run and its artifacts, run.json last; `files` maps names like transcript.jsonl to their contents. */
export async function saveRun(run: Run, files: Record<string, string> = {}, results = RESULTS) {
  const directory = path.join(results, "runs", run.id)
  await mkdir(directory, { recursive: true })
  for (const [name, content] of Object.entries(files)) await publish(path.join(directory, name), content)
  await publish(path.join(directory, "run.json"), JSON.stringify(run, null, 2) + "\n")
}

export function runDirectory(run: Pick<Run, "id">, results = RESULTS) {
  return path.join(results, "runs", run.id)
}

/** Samples per key: passed and failed runs; errors are left out so their jobs run again. */
export function samples(runs: Run[]) {
  const counts = new Map<string, number>()
  for (const run of runs) if (run.status !== "error") counts.set(run.key, (counts.get(run.key) ?? 0) + 1)
  return counts
}
