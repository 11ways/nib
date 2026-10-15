/** Running, checking and re-checking benchmark jobs. */
import { createTwoFilesPatch } from "diff"
import { randomUUID } from "node:crypto"
import { realpathSync } from "node:fs"
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises"
import os, { tmpdir } from "node:os"
import path from "node:path"
import { applyChanges, changedFiles, copyFixture, linkModules, pristineFixture, REPO } from "./fixture"
import { addUsage, emptyUsage, harnessVersion, MAX_TOOL_CALLS, openSession, parseClaude, parseOpencode, type HarnessSession, type Mode, type Model } from "./runners"
import { checkHash, hash, identity, identityKey, loadRuns, runDirectory, saveRun, stoppedEarly, taskHash, type Run, type Status } from "./store"
import type { Task } from "./tasks"

export type Job = { task: Task; model: Model; mode: Mode; nib: string; fixture: string }

const CHECK = path.join(import.meta.dir, "check.ts")
const CHECK_TIMEOUT = 180_000

function jobFields(job: Job) {
  return { task: job.task.name, taskHash: taskHash(job.task), fixture: job.fixture, harness: job.model.harness, model: job.model.model, variant: job.model.variant, shell: job.model.shell, mode: job.mode, nib: job.mode === "nib" ? job.nib : undefined }
}

export function jobKey(job: Job) {
  return identityKey(identity(jobFields(job)))
}

/** A fingerprint of nib's own working tree (tracked changes and untracked files), to stop the benchmark if a run ever edits it instead of its copy. */
export async function repoFingerprint() {
  const git = async (...args: string[]) => new Response(Bun.spawn(["git", "-C", REPO, ...args], { stdout: "pipe", stderr: "ignore" }).stdout).text()
  const untracked = (await git("ls-files", "--others", "--exclude-standard", "-z")).split("\0").filter(Boolean)
  return hash(await git("diff", "HEAD", "--binary"), ...untracked, untracked.length ? await git("hash-object", "--", ...untracked) : "")
}

/**
 * `args` as a bwrap command in `cwd`, since checks run code the model wrote: the whole filesystem read-only, HOME, /tmp and /run empty (so no
 * logins and no sockets), no network and no inherited variables; only `writable` can change, and `readable` comes back into view after the empty ones.
 * @throws Error when bwrap is not installed
 */
export function confined(args: string[], cwd: string, readable: string[], writable: string[] = []) {
  if (!Bun.which("bwrap")) throw new Error("benchmark checks run in bwrap (bubblewrap), which is not installed")
  // Bun, ripgrep and git may live in HOME, which is hidden, so each comes back by itself.
  const tools = [process.execPath, Bun.which("rg"), Bun.which("git")].filter((tool): tool is string => !!tool).map((tool) => realpathSync(tool))
  const bind = (flag: string, paths: string[]) => paths.flatMap((item) => [flag, item, item])
  return ["bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run", "--tmpfs", os.homedir(),
    ...bind("--ro-bind", [REPO, ...tools, ...readable]), ...bind("--bind", writable), "--unshare-all", "--die-with-parent", "--new-session", "--clearenv",
    "--setenv", "PATH", [...new Set(tools.map((tool) => path.dirname(tool))), "/usr/local/bin", "/usr/bin", "/bin"].join(":"), "--setenv", "HOME", os.homedir(), "--setenv", "TMPDIR", "/tmp",
    "--chdir", cwd, "--", ...args]
}

async function runConfined(args: string[], cwd: string, readable: string[], writable: string[] = [], input = "") {
  const child = Bun.spawn(confined(args, cwd, readable, writable), { stdin: new Blob([input]), stdout: "pipe", stderr: "pipe", signal: AbortSignal.timeout(CHECK_TIMEOUT) })
  const [stdout, stderr, exit] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
  return { exit, stdout, stderr }
}

/** One test of a junit report, named by its suite path and its own name. */
type TestCase = { id: string; assertions: number; failed: boolean; skipped: boolean }

/** Runs one test file of `tree` confined, through Bun's junit reporter into `out`; no cases when the run wrote no report, as when it never finished. */
async function runTests(tree: string, file: string, out: string) {
  const report = path.join(out, `${randomUUID()}.xml`)
  const run = await runConfined([process.execPath, "test", "--reporter=junit", `--reporter-outfile=${report}`, file], tree, [tree], [out])
  const xml = await readFile(report, "utf8").catch(() => undefined)
  const cases = xml === undefined ? undefined : [...xml.matchAll(/<testcase\b([^>]*?)(?:\/>|>([\s\S]*?)<\/testcase>)/g)].map((match): TestCase => {
    const attribute = (name: string) => new RegExp(`\\b${name}="([^"]*)"`).exec(match[1]!)?.[1] ?? ""
    return { id: `${attribute("classname")} > ${attribute("name")}`, assertions: Number(attribute("assertions")) || 0, failed: /<(failure|error)\b/.test(match[2] ?? ""), skipped: /<skipped\b/.test(match[2] ?? "") }
  })
  return { ...run, cases }
}

const pristineTests = new Map<string, Promise<Set<string>>>()

/** The tests one test file holds in an untouched copy of the fixture, once per process. */
function pristineCases(file: string) {
  let cases = pristineTests.get(file)
  if (!cases) {
    cases = (async () => {
      const base = await mkdtemp(path.join(tmpdir(), "nib-bench-pristine-tests-"))
      try {
        const work = path.join(base, "work")
        await copyFixture(work)
        await linkModules(work)
        await mkdir(path.join(base, "out"))
        const run = await runTests(work, file, path.join(base, "out"))
        if (!run.cases?.length) throw new Error(`${file} runs no tests in the untouched fixture: ${(run.stdout + run.stderr).trim().slice(-1000)}`)
        return new Set(run.cases.map((item) => item.id))
      } finally { await rm(base, { recursive: true, force: true }) }
    })()
    pristineTests.set(file, cases)
  }
  return cases
}

/**
 * Why the tests a task asks for do not hold up in `work`, or empty when they do: the file must have a new test, by name, that runs and asserts
 * something, and no new test may fail. The fixture's own tests are not judged, since one of them races and can fail under load.
 */
async function testFailure(work: string, file: string, out: string) {
  const [mine, before] = await Promise.all([runTests(work, file, out), pristineCases(file)])
  const output = () => (mine.stdout + mine.stderr).trim().slice(-1500)
  if (!mine.cases?.length) return `tests: ${file} did not run:\n${output()}`
  const added = mine.cases.filter((item) => !before.has(item.id))
  const failed = added.filter((item) => item.failed)
  if (failed.length) return `tests: ${file} has new tests that fail: ${failed.map((item) => item.id).join(", ")}\n${output()}`
  if (!added.some((item) => !item.skipped && item.assertions > 0)) {
    return `tests: ${file} has no new test that runs and asserts something (${added.length} new, ${added.filter((item) => item.skipped).length} skipped)`
  }
  return ""
}

/**
 * Runs the task's check, the tests it asks for and a type check on a finished tree, each confined; check.ts prints a nonce it reads from stdin
 * only once the check returned, so a tree that makes the process exit early with status 0 does not pass.
 */
export async function verify(task: Task, work: string) {
  const { directory: pristine } = await pristineFixture()
  await linkModules(work)
  const out = await mkdtemp(path.join(tmpdir(), "nib-bench-verify-"))
  try {
    const nonce = randomUUID()
    const [check, types, tests] = await Promise.all([
      runConfined([process.execPath, CHECK, task.name, work, pristine], work, [work, pristine], [], nonce),
      runConfined([path.join(work, "node_modules/.bin/tsgo"), "--noEmit"], work, [work]),
      task.tests ? testFailure(work, task.tests, out) : "",
    ])
    const checked = !check.exit && check.stdout.split("\n").includes(nonce)
    const checkOutput = (check.stdout.replace(nonce, "") + check.stderr).trim() || `exited ${check.exit} before the check finished`
    const failures = [checked ? "" : `check: ${checkOutput}`, types.exit ? `types: ${(types.stdout + types.stderr).trim()}` : "", tests].filter(Boolean)
    return failures.length ? { ok: false, failure: failures.join("\n").slice(0, 3000) } : { ok: true }
  } finally { await rm(out, { recursive: true, force: true }) }
}

/** A readable diff of a run's changes against the pristine fixture. */
async function diffOf(changes: Record<string, string | null>) {
  const { directory } = await pristineFixture()
  const parts: string[] = []
  for (const [file, content] of Object.entries(changes)) {
    const before = await readFile(path.join(directory, file), "utf8").catch(() => "")
    parts.push(createTwoFilesPatch(`a/${file}`, content === null ? "/dev/null" : `b/${file}`, before, content ?? "", "", "", { context: 2 }))
  }
  return parts.join("\n")
}

/** Runs every step of a job through one harness session in a fresh copy of the fixture, checks the result and stores it. */
export async function runJob(job: Job, options: { timeout: number; keep: boolean; results?: string }): Promise<Run> {
  const base = await mkdtemp(path.join(tmpdir(), "nib-bench-"))
  const work = path.join(base, "work")
  const home = path.join(base, "home")
  await copyFixture(work)
  const usage = emptyUsage()
  const transcript: string[] = []
  const started = new Date()
  let stderr = ""
  let status: Status | undefined
  let failure: string | undefined
  let stopped: Run["stopped"]
  let changes: Record<string, string | null> | undefined
  try {
    let harness: HarnessSession | undefined
    try {
      for (const [index, step] of job.task.steps.entries()) {
        await step.before?.(work)
        harness ??= await openSession(job.model, job.mode, work, home, options.timeout, job.task.steps.length)
        const result = await harness.prompt(step.prompt, options.timeout)
        transcript.push(JSON.stringify({ type: "nib-bench-step", step: index + 1, prompt: step.prompt }), result.stdout.trim())
        addUsage(usage, result.session)
        if (result.timedOut) [status, failure, stopped] = ["failed", `timed out after ${options.timeout / 1000}s in step ${index + 1}`, "timeout"]
        else if (result.capped) [status, failure, stopped] = ["failed", `stopped after ${MAX_TOOL_CALLS} tool calls in step ${index + 1}`, "cap"]
        else if (result.session.error) [status, failure] = ["error", `step ${index + 1}: ${result.session.error}`]
        if (status) break
      }
    } finally {
      if (harness) stderr = await harness.close()
    }
    // The harness is gone, so the changes are the model's alone; they are taken before any check runs code from the tree.
    changes = await changedFiles(work)
    if (!status) {
      const verdict = await verify(job.task, work)
      status = verdict.ok ? "passed" : "failed"
      failure = verdict.failure
    }
  } catch (error) {
    status = "error"
    failure = error instanceof Error ? error.message : String(error)
  }
  changes ??= await changedFiles(work).catch(() => ({}))
  const fields = jobFields(job)
  const run: Run = {
    ...identity(fields),
    ...usage,
    id: `${started.toISOString().replaceAll(":", "-").slice(0, 19)}-${job.task.name}-${job.model.model.replaceAll("/", "_")}-${job.mode}-${Math.random().toString(36).slice(2, 6)}`,
    key: jobKey(job),
    label: job.model.label,
    status,
    ...failure && { failure },
    ...stopped && { stopped },
    timeout: options.timeout,
    maxToolCalls: MAX_TOOL_CALLS,
    checkHash: checkHash(job.task),
    harnessVersion: await harnessVersion(job.model.harness),
    started: started.toISOString(),
    seconds: (Date.now() - started.getTime()) / 1000,
  }
  await saveRun(run, {
    "transcript.jsonl": transcript.join("\n") + "\n",
    "changes.json": JSON.stringify(changes),
    "changes.diff": await diffOf(changes),
    ...stderr && { "stderr.txt": stderr + "\n" },
  }, options.results)
  if (!options.keep) await rm(base, { recursive: true, force: true })
  return run
}

/** Proves every task fair without a model: the untouched fixture fails its check, and the reference solution passes it. */
export async function validate(tasks: Task[]) {
  const problems: string[] = []
  await Promise.all(tasks.map(async (task) => {
    const base = await mkdtemp(path.join(tmpdir(), "nib-bench-validate-"))
    const work = path.join(base, "work")
    try {
      await copyFixture(work)
      const untouched = await verify(task, work)
      if (untouched.ok) problems.push(`${task.name}: the untouched fixture already passes`)
      else if (untouched.failure?.includes("types:")) problems.push(`${task.name}: the untouched fixture fails its type check:\n${untouched.failure}`)
      for (const step of task.steps) {
        await step.before?.(work)
        await step.solve(work)
      }
      const solved = await verify(task, work)
      if (!solved.ok) problems.push(`${task.name}: the reference solution fails:\n${solved.failure}`)
      console.error(`${task.name}: untouched fails (${untouched.failure?.split("\n")[0]?.slice(0, 120)}); reference ${solved.ok ? "passes" : "FAILS"}`)
    } finally { await rm(base, { recursive: true, force: true }) }
  }))
  return problems
}

/** Re-derives tokens, cost, tool calls, failed calls and notes of every stored run from its transcript, as the parsers read them now; no model is called. */
export async function reparse(results?: string) {
  let updated = 0
  for (const run of await loadRuns(results)) {
    const transcript = await readFile(path.join(runDirectory(run, results), "transcript.jsonl"), "utf8").catch(() => undefined)
    if (transcript === undefined) continue
    // Each prompt's output follows its step marker; parsing them apart keeps per-prompt totals, such as Claude Code's result tokens, from overwriting each other.
    const steps: string[][] = []
    for (const line of transcript.split("\n")) {
      if (line.startsWith('{"type":"nib-bench-step"')) steps.push([])
      else steps.at(-1)?.push(line)
    }
    const usage = emptyUsage()
    // Claude Code's cost counts the whole session, also across the resumed processes of older runs, so each step adds what it cost beyond the last.
    let spent = 0
    for (const step of steps) {
      const session = run.harness === "claude" ? parseClaude(step.join("\n"), spent) : parseOpencode(step.join("\n"))
      spent += session.cost
      addUsage(usage, session)
    }
    await saveRun({ ...run, ...usage }, {}, results)
    updated++
  }
  return updated
}

/** Checks stored runs again whose check changed since, by rebuilding their trees from the stored changes; no model is called. */
export async function recheck(tasks: Task[], fixture: string, results?: string) {
  let updated = 0
  for (const run of await loadRuns(results)) {
    const task = tasks.find((item) => item.name === run.task)
    // A run its limits stopped left a tree that is not the model's finished work, so it stays failed whatever a check says of it.
    if (!task || run.status === "error" || stoppedEarly(run) || run.fixture !== fixture || run.checkHash === checkHash(task)) continue
    const directory = runDirectory(run, results)
    const changes = JSON.parse(await readFile(path.join(directory, "changes.json"), "utf8"))
    const base = await mkdtemp(path.join(tmpdir(), "nib-bench-recheck-"))
    try {
      const work = path.join(base, "work")
      await applyChanges(work, changes)
      const verdict = await verify(task, work)
      const next: Run = { ...run, status: verdict.ok ? "passed" : "failed", checkHash: checkHash(task) }
      if (verdict.ok) delete next.failure
      else next.failure = verdict.failure
      await saveRun(next, {}, results)
      updated++
    } finally { await rm(base, { recursive: true, force: true }) }
  }
  return updated
}
