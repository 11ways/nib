/** Benchmark tasks: prompts a model works through, a check of the finished tree, and a reference solution that proves the check passes. */
import { diffArrays } from "diff"
import { readFileSync, rmSync } from "node:fs"
import { mkdir, mkdtemp, readdir, readFile, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { API_CONFIG, DATA_FILES, NEW_HOST, OLD_HOST } from "./fixture"

/** One prompt of a task; `before` changes the tree first, as a teammate would, and `solve` is the reference for this step. */
export type Step = { prompt: string; before?: (work: string) => Promise<void>; solve: (work: string) => Promise<void> }
/**
 * `check` throws with the reason when the finished tree in `work` is wrong; `pristine` is the untouched fixture. `tests` names the test file the
 * prompts ask for: verification runs it, and it must pass with more tests and assertions than the untouched fixture's copy. `shell` marks a task
 * that needs a shell to run programs; it runs only in the shell series, since the default series offers no shell.
 */
export type Task = { name: string; about: string; steps: Step[]; check: (work: string, pristine: string) => Promise<void>; tests?: string; shell?: boolean }
/** One-shot tasks take one prompt; multi-step ones take several on the same files, the repeated editing nib is built for; program tasks need a shell. */
export type TaskKind = "one-shot" | "multi-step" | "program"
// Edit tasks are judged on their edits alone, so their prompts rule out runs that would only cost time; program tasks are about running programs.
const EDITS_ONLY = "\n\nDo not run tests, type checks, builds or git commands; just make the edits and stop."
/** The prompt a model gets for `step` of `task`. */
export const promptOf = (task: Task, step: Step) => task.shell ? step.prompt : step.prompt + EDITS_ONLY
export const taskKind = (task: Task): TaskKind => task.shell ? "program" : task.steps.length > 1 ? "multi-step" : "one-shot"

const read = (directory: string, file: string) => readFile(path.join(directory, file), "utf8")
const load = (directory: string, file: string) => import(path.join(directory, file))
async function change(directory: string, file: string, edit: (text: string) => string) {
  const before = await read(directory, file)
  const after = edit(before)
  if (after === before) throw new Error(`reference left ${file} unchanged`)
  await writeFile(path.join(directory, file), after)
}
function fail(message: string): never {
  throw new Error(message)
}
async function codeFiles(directory: string) {
  const files: string[] = []
  for (const root of ["src", "test"]) {
    for (const entry of await readdir(path.join(directory, root), { recursive: true })) if (String(entry).endsWith(".ts")) files.push(path.join(root, String(entry)))
  }
  return files
}

/** An engine of the finished tree, working in a fresh temporary directory. */
async function engineIn(work: string) {
  const { Engine } = await load(work, "src/engine.ts")
  const directory = await mkdtemp(path.join(tmpdir(), "nib-bench-check-"))
  process.on("exit", () => rmSync(directory, { recursive: true, force: true }))
  const ctx = { directory, session: "check", signal: new AbortController().signal }
  return { engine: new Engine(), ctx, directory }
}

const lines20 = Array.from({ length: 20 }, (_, index) => `l${index + 1}`).join("\n") + "\n"
const TEAMMATE_HEADER = "// Patch parsing for nib's edit tool; keep it free of engine imports.\n"
const OLD_HINT = '"- rows are not accepted; PUT replaces the range with + rows, CUT deletes"'
const NEW_HINT = '"- rows are not accepted: PUT replaces the range with + rows, and CUT deletes it"'
const FUNCTION = /^(export )?(async )?function /
const COMMENTED = /^\s*(\/\/|\*|\/\*\*.*\*\/)|\*\/\s*$/
const DATA_ENDINGS: Record<string, string> = { "a.csv": "", "b.csv": "\r\n", "c.csv": "\n", "d.csv": "", "e.csv": "\n", "f.csv": "\r\n" }
const AGE_EXPECT = 'expect(describeAge(now - 90_000, now)).toBe("1m ago")'
const AGE_TEST = `\ndescribe("describeAge", () => {\n  test("names an age in its largest whole unit", () => {\n    const now = Date.now()\n    ${AGE_EXPECT}\n    expect(describeAge(now - 3 * 3_600_000, now)).toBe("3h ago")\n  })\n})\n`
const CUT_HINT = "a CUT line ends at its line numbers; delete text inside a line with SUB"
const CUT_CASE = `  test("CUT N deletes one line and CUT N-M a range", () => {\n    const [section] = parsePatch("[a.ts#3C4D]\\nCUT 3\\nCUT 5-7")\n    expect(section).toMatchObject({ edits: [{ start: 3, end: 3, operation: "replace" }, { start: 5, end: 7, operation: "replace" }] })\n  })\n`
const CUT_TEXT_CASE = `  test("a CUT line ends at its line numbers", () => {\n    expect(() => parsePatch("[a.ts#3C4D]\\nCUT 3 old text")).toThrow("${CUT_HINT}")\n  })\n`
const SEEDED_TAG = "export function fileTag(text: string, seed: number) {\n  return (Bun.hash.xxHash32(text.split(\"\\n\").map(trimEnd).join(\"\\n\"), seed)"
const PLAIN_TAG = "export function fileTag(text: string) {\n  return (Bun.hash.xxHash32(text.split(\"\\n\").map(trimEnd).join(\"\\n\"), 0)"
const ROW = "if (!colored && !cut) return { text: `${line}:${content}`, anchor: true }"
const TRIMMED_ROW = "if (!colored && !cut) return { text: `${line}:${content.trimEnd()}`, anchor: true }"
const BUILD_FORMATS = ["esm", "iife"]
const BUILD_CONFIG = {
  targets: [
    { name: "core", entry: "src/engine.ts", outDir: "dist/core", format: "esm" },
    { name: "patch", entry: "src/patch.ts", outDir: "dist/patch", format: "esm" },
    { name: "worker", entry: "src/snapshots.ts", outDir: "dist/worker", format: "cjs" },
    { name: "server", entry: "src/server.ts", outDir: "dist/server", format: "esm" },
    { name: "cli", entry: "src/main.ts", outDir: "dist/cli", format: "iife" },
    { name: "browser", entry: "src/patch.ts", outDir: "dist/browser", format: "iife" },
  ],
}
// A bundler-like build: hundreds of colored lines per target, the one real error in the middle, and a summary at the end.
const BUILD_SCRIPT = `// Builds every target in config/build.json.
const config = JSON.parse(await Bun.file(new URL("../config/build.json", import.meta.url)).text())
const FORMATS = ${JSON.stringify(BUILD_FORMATS)}
let seed = 42
const next = () => (seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648
const paint = (code, text) => "\\x1b[" + code + "m" + text + "\\x1b[0m"
let failed = 0
for (const target of config.targets) {
  console.log(paint("1", "[" + target.name + "]") + " building " + target.entry + " -> " + target.outDir + " (" + target.format + ")")
  if (!FORMATS.includes(target.format)) {
    console.log(paint("31", "[" + target.name + "] error: unsupported format \\"" + target.format + "\\" in config/build.json (supported: " + FORMATS.join(", ") + ")"))
    failed++
    continue
  }
  for (let index = 0; index < 600; index++) {
    const module = "src/module" + Math.floor(next() * 400) + ".ts"
    if (next() < 0.03) console.log(paint("33", "[" + target.name + "] warning: " + module + " has an unused import"))
    else console.log("[" + target.name + "] " + paint("2", "transform") + " " + module + " " + Math.floor(next() * 90) + "ms")
  }
  console.log(paint("32", "[" + target.name + "] done: " + (200 + Math.floor(next() * 400)) + " modules, " + Math.floor(next() * 900) + " KiB"))
}
console.log(failed ? paint("31", "build failed: " + failed + " of " + config.targets.length + " targets failed") : paint("32", "build ok"))
process.exit(failed ? 1 : 0)
`
const SLOW_ORDERS = /GET \/v1\/orders\/\d+ 200 (\d+)ms/g
async function slowOrders(directory: string) {
  const log = await read(directory, "logs/api-2026-10-13.log")
  return [...log.matchAll(SLOW_ORDERS)].filter((match) => Number(match[1]) >= 50).length
}
async function unchangedTests(work: string, pristine: string) {
  for (const file of (await codeFiles(pristine)).filter((name) => name.startsWith("test"))) if (await read(work, file) !== await read(pristine, file)) fail(`${file} changed`)
}
// A gateway's routes halfway through a migration: /v1/payments was moved to payments-v2, whose hosts were never added. 42 routes, 7 upstreams,
// as the replayed log's first line says.
const ROUTE_TABLE: [string, string[]][] = [
  ["orders", ["/v1/orders", "/v1/orders/:id", "/v1/orders/:id/items", "/v1/orders/:id/cancel", "/v1/orders/:id/history", "/v1/orders/:id/invoice", "/v1/orders/export", "/v1/orders/search"]],
  ["users", ["/v1/users", "/v1/users/:id", "/v1/users/:id/addresses", "/v1/users/:id/preferences", "/v1/users/:id/sessions", "/v1/users/me", "/v1/users/invite", "/v1/users/search"]],
  ["carts", ["/v1/carts", "/v1/carts/:id", "/v1/carts/:id/items", "/v1/carts/:id/items/:item", "/v1/carts/:id/checkout", "/v1/carts/:id/coupons"]],
  ["payments", ["/v1/payments", "/v1/payments/:id", "/v1/payments/:id/capture", "/v1/payments/:id/refunds", "/v1/payments/methods", "/v1/payments/webhooks", "/v1/refunds/:id"]],
  ["search", ["/v1/search", "/v1/search/suggest", "/v1/search/facets", "/v1/catalog", "/v1/catalog/:id", "/v1/catalog/:id/related"]],
  ["auth", ["/v1/auth/login", "/v1/auth/logout", "/v1/auth/refresh", "/v1/auth/reset", "/v1/auth/verify", "/v1/auth/mfa", "/v1/auth/keys"]],
]
const upstreamHosts = (subnet: number, count: number) => Array.from({ length: count }, (_, index) => `10.0.${subnet}.${11 + index}:8080`)
const ROUTES_CONFIG = {
  routes: Object.fromEntries(ROUTE_TABLE.flatMap(([upstream, paths]) => paths.map((route) => [route, route === "/v1/payments" ? "payments-v2" : upstream]))),
  upstreams: { orders: upstreamHosts(1, 4), users: upstreamHosts(2, 3), carts: upstreamHosts(3, 2), payments: upstreamHosts(4, 3), "payments-v2": [], search: upstreamHosts(5, 2), auth: upstreamHosts(6, 2) },
}
// noisyService() of internal/nibash/testdata/captures/generate.ts, reading the route the reload sets from config/routes.json: with the starting
// config it prints that capture byte for byte (a test holds it there), and once /v1/payments has hosts again its POSTs stop failing.
const SERVE_SCRIPT = `// Replays a recorded day of the gateway's traffic; partway through, the gateway reloads config/routes.json.
const config = JSON.parse(await Bun.file(new URL("../config/routes.json", import.meta.url)).text())
let seed = 7
const next = () => (seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648
const pick = (list) => list[Math.floor(next() * list.length)]
const time = (second) => new Date(Date.UTC(2026, 9, 13, 9, 0, 0) + second * 1000 + Math.floor(next() * 990)).toISOString()
const upstream = config.routes["/v1/payments"]
const healthy = (config.upstreams[upstream] ?? []).length
const lines = []
const reloadAt = 1700
let second = 0
let reloaded = ""
lines.push(time(second) + " INFO  server: listening on :8080 (routes: 42, upstreams: 7)")
lines.push(time(second) + ' INFO  routes: /v1/payments -> upstream "payments" (3 healthy hosts of 3)')
while (lines.length < 3200) {
  second += 1
  if (lines.length === reloadAt) {
    reloaded = time(second)
    lines.push(reloaded + " INFO  config: reloaded config/routes.json (" + Object.keys(config.routes).length + " routes, " + Object.keys(config.upstreams).length + " upstreams)")
    lines.push(time(second) + " INFO  routes: /v1/payments -> upstream " + JSON.stringify(upstream) + " (" + healthy + " healthy hosts of " + healthy + ")")
    continue
  }
  const failing = lines.length > reloadAt && healthy === 0
  const roll = next()
  if (roll < 0.09) lines.push(time(second) + " ERROR pool: connection pool exhausted (64/64 in use), retrying in 50ms")
  else if (roll < 0.16) lines.push(time(second) + " DEBUG health: ok (db " + (1 + Math.floor(next() * 4)) + "ms, cache 1ms)")
  else if (roll < 0.19) lines.push(time(second) + " WARN  gc: pause " + (80 + Math.floor(next() * 90)) + "ms")
  else if (roll < 0.34) lines.push(failing ? time(second) + " INFO  http: POST /v1/payments 503 0ms" : time(second) + " INFO  http: POST /v1/payments 201 " + (30 + Math.floor(next() * 40)) + "ms")
  else lines.push(time(second) + " INFO  http: " + pick(["GET", "GET", "GET", "PUT"]) + " /v1/" + pick(["orders", "users", "carts"]) + "/" + (10000 + Math.floor(next() * 90000)) + " 200 " + (2 + Math.floor(next() * 60)) + "ms")
}
lines.push(time(second + 1) + " INFO  server: draining (SIGTERM)")
lines.push(time(second + 1) + " INFO  server: stopped; " + lines.filter((line) => line.includes(" 503 ")).length + " POST /v1/payments answered 503 since " + reloaded.slice(11, 19))
console.log(lines.join("\\n"))
`
// parseDuration drops the sign; line 41 is the bug, as the check's failure says.
const DURATION_SOURCE = `// Durations as people write them in config files and flags: "90s", "15m", "1h30m", "250ms", and "-2h" for a time back.

const UNITS: Record<string, number> = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400 }
const PART = /(\\d+(?:\\.\\d+)?)(ms|s|m|h|d)/g
const DURATION = /^([+-])?((?:\\d+(?:\\.\\d+)?(?:ms|s|m|h|d))+)$/

/** Seconds as the largest units that divide them, like 5400 as "1h30m"; 0 is "0s". */
export function formatDuration(seconds: number): string {
  if (seconds === 0) return "0s"
  const sign = seconds < 0 ? "-" : ""
  let rest = Math.abs(seconds)
  const parts: string[] = []
  for (const unit of ["d", "h", "m", "s"]) {
    const size = UNITS[unit]!
    const count = Math.floor(rest / size)
    if (count > 0) {
      parts.push(count + unit)
      rest -= count * size
    }
  }
  const millis = Math.round(rest * 1000)
  if (millis > 0) parts.push(millis + "ms")
  return sign + parts.join("")
}

/** Whether text is a duration parseDuration takes. */
export function isDuration(text: string): boolean {
  return DURATION.test(text.trim())
}

/**
 * Seconds in a duration; a leading - counts back, as in "-90s" for a minute and a half ago.
 * @throws Error when text is not a duration
 */
export function parseDuration(text: string): number {
  const match = DURATION.exec(text.trim())
  if (!match) throw new Error("not a duration: " + JSON.stringify(text))
  let total = 0
  for (const [, amount, unit] of match[2]!.matchAll(PART)) total += Number(amount) * UNITS[unit!]!
  const sign = match[1] === "-" ? -1 : 1
  return Math.abs(sign * total)
}
`
// The values the long-run check holds parseDuration to.
const DURATIONS: [string, number][] = [["-90s", -90], ["90s", 90], ["1h30m", 5400], ["-2h", -7200], ["15m", 900], ["+45s", 45], ["0s", 0], ["2d", 172800]]
// longRun() of internal/nibash/testdata/captures/generate.ts, from the generator state that noisyService() leaves, with its one real check run on
// src/duration.ts: while that fails it prints that capture byte for byte (a test holds it there).
const CHECK_SCRIPT = `// Runs the project's checks.
import { parseDuration } from "../src/duration.ts"

let seed = 1467546745
const next = () => (seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648
const pick = (list) => list[Math.floor(next() * list.length)]
const AREAS = ["durations", "parser", "router", "cache", "errors", "retry", "auth", "orders", "money", "locale"]
const VERBS = ["parses", "formats", "rounds", "keeps", "merges", "sorts", "rejects", "accepts", "reports", "skips"]
const THINGS = ["empty input", "a trailing comma", "unicode names", "large numbers", "nested lists", "a missing field", "duplicate keys", "negative values", "an expired token", "a slow upstream"]
const NOISY = ["reports an error for {}", "fails fast on {}", "throws a TypeError for {}", "keeps the failure count for {}", "returns errors for {}"]
const lines = []
const failAt = 1400
let count = 0
let failed = 0
for (let file = 0; file < 38; file++) {
  lines.push("src/" + AREAS[file % AREAS.length] + file + ".test.ts:")
  for (let n = 0; n < 63; n++) {
    count++
    const thing = pick(THINGS)
    const name = next() < 0.11 ? pick(NOISY).replace("{}", thing) : pick(VERBS) + " " + thing
    const ms = (next() * 0.9).toFixed(2)
    if (count === failAt) {
      let got
      try {
        got = parseDuration("-90s")
      } catch (error) {
        got = String(error)
      }
      if (got === -90) lines.push('\\u2713 durations > parses "-90s" [0.31ms]')
      else {
        failed++
        lines.push('\\u2715 durations > parses "-90s" [0.31ms]', "    got " + got + ", want -90 (src/duration.ts:41)", "    at parseDuration (src/duration.ts:41:12)")
      }
      continue
    }
    lines.push("\\u2713 " + AREAS[file % AREAS.length] + " > " + name + " [" + ms + "ms]")
  }
  lines.push("")
}
lines.push(" " + (count - failed) + " pass", " " + failed + " fail", " " + count * 3 + " expect() calls", "Ran " + count + " tests across 38 files. [1.84s]")
console.log(lines.join("\\n"))
process.exit(failed ? 1 : 0)
`

export const TASKS: Task[] = [
  {
    name: "rename",
    about: "rename a function used 48 times across source and tests",
    steps: [{
      prompt: "Rename the function fileTag to contentTag everywhere in this project: its definition and every use, including the tests.",
      solve: async (work) => {
        for (const file of await codeFiles(work)) {
          const text = await read(work, file)
          if (/\bfileTag\b/.test(text)) await writeFile(path.join(work, file), text.replace(/\bfileTag\b/g, "contentTag"))
        }
      },
    }],
    check: async (work) => {
      let left = 0
      let used = 0
      for (const file of await codeFiles(work)) {
        const text = await read(work, file)
        left += text.match(/\bfileTag\b/g)?.length ?? 0
        used += text.match(/\bcontentTag\b/g)?.length ?? 0
      }
      if (left) fail(`fileTag still occurs ${left} times`)
      if (used < 48) fail(`contentTag occurs only ${used} times, expected 48`)
    },
  },
  {
    name: "one-line",
    about: "change one expression in a 2200-line file",
    steps: [{
      prompt: "In src/engine.ts, make formatSize show sizes of 1 MiB and above with two decimals, like 2.38 MiB. Smaller sizes stay as they are.",
      solve: (work) => change(work, "src/engine.ts", (text) => text.replace("KiB` : `${(bytes / 1048576).toFixed(1)} MiB`", "KiB` : `${(bytes / 1048576).toFixed(2)} MiB`")),
    }],
    check: async (work) => {
      const { engine, ctx, directory } = await engineIn(work)
      await writeFile(path.join(directory, "big.txt"), "a".repeat(2_500_000))
      await writeFile(path.join(directory, "mib.txt"), "a".repeat(1_048_575) + "\n")
      await writeFile(path.join(directory, "small.txt"), "a".repeat(1535) + "\n")
      const text = (await engine.find(ctx, {})).text
      for (const row of ["big.txt (2.38 MiB, ", "mib.txt (1 line, 1.00 MiB, ", "small.txt (1 line, 1.5 KiB, "]) if (!text.includes(row)) fail(`find output lacks "${row}":\n${text}`)
    },
  },
  {
    name: "cross-file",
    about: "a parser change with a test and a docs row",
    tests: "test/patch.test.ts",
    steps: [{
      prompt: "Make the patch language accept DEL as an alias for REM (delete the file). Update the parser in src/patch.ts, add a test for it to test/patch.test.ts following that file's style, and mention DEL in README.md's table of hunks.",
      solve: async (work) => {
        await change(work, "src/patch.ts", (text) => text.replace('if (line.trim() === "REM") {', 'if (line.trim() === "REM" || line.trim() === "DEL") {'))
        await change(work, "test/patch.test.ts", (text) => text + '\ndescribe("DEL", () => {\n  test("is an alias for REM", () => {\n    expect(parsePatch("[a.ts#ABCD]\\nDEL")[0]).toMatchObject({ remove: true })\n  })\n})\n')
        await change(work, "README.md", (text) => text.replace("| `MV DEST` |", "| `DEL` | Same as `REM`. |\n| `MV DEST` |"))
      },
    }],
    check: async (work) => {
      const { parsePatch } = await load(work, "src/patch.ts")
      if (parsePatch("[a.ts#ABCD]\nDEL")[0]?.remove !== true) fail("DEL does not delete")
      if (parsePatch("[a.ts#ABCD]\nREM")[0]?.remove !== true) fail("REM no longer deletes")
      let combined = false
      try { parsePatch("[a.ts#ABCD]\nDEL\nPUT 1.=1:\n+x") } catch { combined = true }
      if (!combined) fail("DEL followed by PUT is accepted")
      if (!(await read(work, "test/patch.test.ts")).includes("DEL")) fail("no test for DEL")
      if (!(await read(work, "README.md")).includes("`DEL`")) fail("README.md does not mention `DEL`")
    },
  },
  {
    name: "large-file",
    about: "two separate changes deep in a 2200-line file",
    steps: [{
      prompt: "In src/engine.ts, make edit previews show 3 lines of context around each change instead of 2, and make the mime detection also recognize BMP images (files starting with the bytes \"BM\") as image/bmp.",
      solve: (work) => change(work, "src/engine.ts", (text) => text
        .replace("const PREVIEW_CONTEXT = 2", "const PREVIEW_CONTEXT = 3")
        .replace('function mime(bytes: Buffer) {\n', 'function mime(bytes: Buffer) {\n  if (bytes[0] === 0x42 && bytes[1] === 0x4d) return "image/bmp"\n')),
    }],
    check: async (work) => {
      const { engine, ctx, directory } = await engineIn(work)
      await writeFile(path.join(directory, "x.bmp"), Buffer.from([0x42, 0x4d, 0, 0, 0, 0, 1, 2, 3]))
      const image = await engine.read(ctx, { paths: ["x.bmp"] })
      if (image.images?.[0]?.mimeType !== "image/bmp") fail(`a BMP file reads as ${JSON.stringify(image.text)}`)
      await writeFile(path.join(directory, "t.txt"), lines20)
      const tag = /#([0-9A-F]{4})\]/.exec((await engine.read(ctx, { paths: ["t.txt"] })).text)![1]
      const preview = (await engine.patch(ctx, `[t.txt#${tag}]\nPUT 10.=10:\n+TEN`)).text
      if (!preview.includes("\n7:l7\n") || !preview.includes("\n13:l13") || preview.includes("\n6:l6") || preview.includes("14:l14")) fail(`the preview does not show 3 lines of context:\n${preview}`)
    },
  },
  {
    name: "whole-file",
    about: "19 insertions spread over a 2200-line file",
    steps: [{
      prompt: "In src/engine.ts, add a one-line /** ... */ doc comment directly above every top-level function declaration (function or async function, exported or not) that does not already have a comment on the line directly above it. Do not change anything else.",
      solve: (work) => change(work, "src/engine.ts", (text) => {
        const lines = text.split("\n")
        return lines.flatMap((line, index) => FUNCTION.test(line) && !COMMENTED.test(lines[index - 1] ?? "") ? ["/** Does what its name says. */", line] : [line]).join("\n")
      }),
    }],
    check: async (work, pristine) => {
      const lines = (await read(work, "src/engine.ts")).split("\n")
      const missing = lines.flatMap((line, index) => FUNCTION.test(line) && !COMMENTED.test(lines[index - 1] ?? "") ? [line] : [])
      if (missing.length) fail(`undocumented: ${missing.join(" | ")}`)
      const changes = diffArrays((await read(pristine, "src/engine.ts")).split("\n"), lines)
      const removed = changes.filter((part) => part.removed).flatMap((part) => part.value)
      const added = changes.filter((part) => part.added).flatMap((part) => part.value).filter((line) => !/^\s*\/\*\*.*\*\/\s*$/.test(line))
      if (removed.length || added.length) fail(`changed more than doc comments: removed ${JSON.stringify(removed.slice(0, 3))}, added ${JSON.stringify(added.slice(0, 3))}`)
    },
  },
  {
    name: "log-hunt",
    about: "find why a service crashed in a 3 MiB colored log, then fix a config file",
    steps: [{
      prompt: "The API service crashed on its most recent run. Its logs are in logs/. Find out from the newest log why it crashed, and fix config/api.json so it starts again.",
      solve: (work) => change(work, "config/api.json", (text) => JSON.stringify({ ...JSON.parse(text), retryLimit: 7 }, null, 2) + "\n"),
    }],
    check: async (work) => {
      const config = JSON.parse(await read(work, "config/api.json"))
      if (config.retryLimit !== 7) fail(`retryLimit is ${JSON.stringify(config.retryLimit)}`)
      for (const [key, value] of Object.entries(API_CONFIG)) if (config[key] !== value) fail(`${key} changed to ${JSON.stringify(config[key])}`)
    },
  },
  {
    name: "minified",
    about: "replace a host inside a 90 KiB one-line bundle",
    steps: [{
      prompt: `public/vendor.min.js still points at the old API host ${OLD_HOST}. Change every occurrence to ${NEW_HOST} without changing anything else in the file.`,
      solve: (work) => change(work, "public/vendor.min.js", (text) => text.replaceAll(OLD_HOST, NEW_HOST)),
    }],
    check: async (work, pristine) => {
      const expected = (await read(pristine, "public/vendor.min.js")).replaceAll(OLD_HOST, NEW_HOST)
      const actual = await read(work, "public/vendor.min.js")
      if (actual !== expected) fail(`the bundle differs from the expected one (${actual.length} chars, expected ${expected.length}; old host left ${actual.split(OLD_HOST).length - 1} times)`)
    },
  },
  {
    name: "copy-variant",
    about: "copy a file and change two things in the copy only",
    steps: [{
      prompt: "Create src/snapshots-legacy.ts as a copy of src/snapshots.ts. In the copy only, change MAX_VERSIONS from 4 to 8 and rename the class SnapshotStore to LegacySnapshotStore. Leave src/snapshots.ts as it is.",
      solve: async (work) => {
        const text = await read(work, "src/snapshots.ts")
        await writeFile(path.join(work, "src/snapshots-legacy.ts"), text.replace("const MAX_VERSIONS = 4", "const MAX_VERSIONS = 8").replace("class SnapshotStore", "class LegacySnapshotStore"))
      },
    }],
    check: async (work, pristine) => {
      const original = await read(pristine, "src/snapshots.ts")
      if (await read(work, "src/snapshots.ts") !== original) fail("src/snapshots.ts changed")
      const copy = await read(work, "src/snapshots-legacy.ts").catch(() => fail("src/snapshots-legacy.ts is missing"))
      if (copy !== original.replace("const MAX_VERSIONS = 4", "const MAX_VERSIONS = 8").replace("class SnapshotStore", "class LegacySnapshotStore")) fail("src/snapshots-legacy.ts is not the original with exactly those two changes")
    },
  },
  {
    name: "followup",
    about: "two prompts on one file, which a teammate changes in between",
    steps: [
      {
        prompt: "In src/patch.ts, make unquote also strip backticks around a path, so a section header like [`src/a.ts`#3C4D] names src/a.ts.",
        solve: (work) => change(work, "src/patch.ts", (text) => text.replace("/^([\"']).*\\1$/", "/^([\"'`]).*\\1$/")),
      },
      {
        prompt: "While you worked, a teammate changed src/patch.ts. Keep their changes, and now also make the parser accept PUT N: as a single-line replace, the same as PUT N.=N:.",
        before: (work) => change(work, "src/patch.ts", (text) => TEAMMATE_HEADER + text.replace(OLD_HINT, NEW_HINT)),
        solve: (work) => change(work, "src/patch.ts", (text) => text.replace("const range = RANGE_PUT.exec(line)", "const single = /^PUT ([1-9][0-9]*):$/.exec(line)\n      const range = RANGE_PUT.exec(line) ?? (single ? [line, single[1]!, single[1]!] : null)")),
      },
    ],
    check: async (work) => {
      const text = await read(work, "src/patch.ts")
      if (!text.startsWith(TEAMMATE_HEADER) || !text.includes(NEW_HINT)) fail("the teammate's changes are gone")
      const { parsePatch } = await load(work, "src/patch.ts")
      if (parsePatch("[`src/a.ts`#ABCD]\nREM")[0]?.path !== "src/a.ts") fail("backticks are not stripped")
      const single = parsePatch("[a.ts#ABCD]\nPUT 3:\n+x")[0]?.edits?.[0]
      if (single?.start !== 3 || single.end !== 3 || single.operation !== "replace" || single.content?.[0] !== "x") fail(`PUT 3: parses as ${JSON.stringify(single)}`)
      const range = parsePatch("[a.ts#ABCD]\nPUT 2.=4:\n+x")[0]?.edits?.[0]
      if (range?.start !== 2 || range.end !== 4) fail("PUT 2.=4: broke")
    },
  },
  {
    name: "staged-refactor",
    about: "four prompts reworking one function of a 2200-line file, its callers and a test",
    tests: "test/engine.test.ts",
    steps: [
      {
        prompt: "In src/engine.ts, rename the function formatAge to describeAge, including every use.",
        solve: (work) => change(work, "src/engine.ts", (text) => text.replace(/\bformatAge\b/g, "describeAge")),
      },
      {
        prompt: "Give describeAge a second parameter, now, in epoch milliseconds with Date.now() as its default, and measure the age from it instead of calling Date.now() inside the function. Export describeAge.",
        solve: (work) => change(work, "src/engine.ts", (text) => text.replace(
          "function describeAge(time: number) {\n  const seconds = Math.max(0, Math.floor((Date.now() - time) / 1000))",
          "export function describeAge(time: number, now = Date.now()) {\n  const seconds = Math.max(0, Math.floor((now - time) / 1000))",
        )),
      },
      {
        prompt: "Add a test for describeAge to test/engine.test.ts, following that file's style: 90 seconds before now is \"1m ago\" and 3 hours before now is \"3h ago\".",
        solve: (work) => change(work, "test/engine.test.ts", (text) => text.replace('import { Engine, type Context } from "../src/engine"', 'import { describeAge, Engine, type Context } from "../src/engine"') + AGE_TEST),
      },
      {
        prompt: "Make describeAge say \"just now\" for ages under 10 seconds, and cover that in your test too.",
        solve: async (work) => {
          await change(work, "src/engine.ts", (text) => text.replace("  if (seconds < 60) return `${seconds}s ago`", "  if (seconds < 10) return \"just now\"\n  if (seconds < 60) return `${seconds}s ago`"))
          await change(work, "test/engine.test.ts", (text) => text.replace(AGE_EXPECT, `expect(describeAge(now - 5_000, now)).toBe("just now")\n    ${AGE_EXPECT}`))
        },
      },
    ],
    check: async (work) => {
      if (/\bformatAge\b/.test(await read(work, "src/engine.ts"))) fail("formatAge is still in src/engine.ts")
      const { describeAge } = await load(work, "src/engine.ts")
      if (typeof describeAge !== "function") fail("src/engine.ts exports no describeAge")
      const now = Date.now()
      for (const [ago, expected] of [[5_000, "just now"], [30_000, "30s ago"], [90_000, "1m ago"], [3 * 3_600_000, "3h ago"], [3 * 86_400_000, "3d ago"]] as const) {
        const actual = describeAge(now - ago, now)
        if (actual !== expected) fail(`describeAge(now - ${ago}, now) is ${JSON.stringify(actual)}, expected ${JSON.stringify(expected)}`)
      }
      if (describeAge(Date.now() - 120_000) !== "2m ago") fail("describeAge without now does not measure from the current time")
      // The callers still name ages: find shows a file's.
      const { engine, ctx, directory } = await engineIn(work)
      await writeFile(path.join(directory, "a.txt"), "x\n")
      const time = Date.now() / 1000 - 300
      await utimes(path.join(directory, "a.txt"), time, time)
      const listed = (await engine.find(ctx, {})).text
      if (!listed.includes("a.txt (1 line, 2 bytes, 5m ago)")) fail(`find no longer shows ages:\n${listed}`)
      const tests = await read(work, "test/engine.test.ts")
      if (!tests.includes("describeAge") || !tests.includes("just now")) fail("no test for describeAge that covers \"just now\"")
    },
  },
  {
    name: "parser-rounds",
    about: "four prompts growing one parser rule and its tests",
    tests: "test/patch.test.ts",
    steps: [
      {
        prompt: "In src/patch.ts, make the parser accept CUT N as a one-line delete, the same as CUT N.=N.",
        solve: (work) => change(work, "src/patch.ts", (text) => text
          .replace("const CUT = /^CUT ([1-9][0-9]*)\\.=([1-9][0-9]*)$/", "const CUT = /^CUT ([1-9][0-9]*)(?:\\.=([1-9][0-9]*))?$/")
          .replace("end: Number(cut[2])", "end: Number(cut[2] ?? cut[1])")),
      },
      {
        prompt: "Also accept CUT ranges written with a dash, like CUT 3-5, the same as CUT 3.=5. PUT stays as it is: PUT 3-5: must still be refused with its hint.",
        solve: (work) => change(work, "src/patch.ts", (text) => text.replace("(?:\\.=([1-9][0-9]*))?$/", "(?:(?:\\.=|-)([1-9][0-9]*))?$/")),
      },
      {
        prompt: "Add a test for both new CUT forms to test/patch.test.ts, following that file's style.",
        solve: (work) => change(work, "test/patch.test.ts", (text) => `${text}\ndescribe("CUT forms", () => {\n${CUT_CASE}})\n`),
      },
      {
        prompt: `A CUT line with anything after its line numbers, like CUT 3 old text, should be refused with the hint "${CUT_HINT}". Add that case to your test.`,
        solve: async (work) => {
          await change(work, "src/patch.ts", (text) => text.replace(
            '  [/^PUT ([1-9][0-9]*):$/, "a single-line replace is written PUT N.=N:"],\n',
            `  [/^PUT ([1-9][0-9]*):$/, "a single-line replace is written PUT N.=N:"],\n  [/^CUT [1-9][0-9]*\\S*\\s+\\S/, "${CUT_HINT}"],\n`,
          ))
          await change(work, "test/patch.test.ts", (text) => text.replace(CUT_CASE, CUT_CASE + CUT_TEXT_CASE))
        },
      },
    ],
    check: async (work) => {
      const { parsePatch } = await load(work, "src/patch.ts")
      const refusal = (input: string) => {
        try { parsePatch(input) } catch (error) { return error instanceof Error ? error.message : String(error) }
      }
      for (const [line, start, end] of [["CUT 3", 3, 3], ["CUT 3-5", 3, 5], ["CUT 3.=5", 3, 5], ["CUT 3*", 3, 3]] as const) {
        const refused = refusal(`[a.ts#ABCD]\n${line}`)
        if (refused) fail(`${line} is refused: ${refused}`)
        const edit = parsePatch(`[a.ts#ABCD]\n${line}`)[0]?.edits?.[0]
        if (edit?.start !== start || edit.end !== end || edit.operation !== "replace" || edit.content?.length || !!edit.block !== line.endsWith("*")) fail(`${line} parses as ${JSON.stringify(edit)}`)
      }
      for (const [input, hint] of [["[a.ts#ABCD]\nPUT 3-5:\n+x", "ranges are written N.=M"], ["[a.ts#ABCD]\nCUT 3 old text", CUT_HINT]] as const) {
        const refused = refusal(input) ?? "accepted"
        if (!refused.includes(hint)) fail(`${JSON.stringify(input)} gives: ${refused}`)
      }
      const tests = await read(work, "test/patch.test.ts")
      if (!/CUT [0-9]+-[0-9]+/.test(tests) || !tests.includes("ends at its line numbers")) fail("no tests for CUT N-M and for text after a CUT line")
    },
  },
  {
    name: "block-rewrite",
    about: "rewrite one whole function in a 2200-line file",
    steps: [{
      prompt: "In src/engine.ts, rewrite formatAge so an age of 14 days or more shows whole weeks (like 2w ago) and an age of 60 days or more shows whole months of 30 days (like 3mo ago). Shorter ages stay as they are.",
      solve: (work) => change(work, "src/engine.ts", (text) => text.replace(
        "  return seconds < 172800 ? `${Math.floor(seconds / 3600)}h ago` : `${Math.floor(seconds / 86400)}d ago`\n",
        "  if (seconds < 172800) return `${Math.floor(seconds / 3600)}h ago`\n  const days = Math.floor(seconds / 86400)\n  if (days >= 60) return `${Math.floor(days / 30)}mo ago`\n  return days >= 14 ? `${Math.floor(days / 7)}w ago` : `${days}d ago`\n",
      )),
    }],
    check: async (work) => {
      const { engine, ctx, directory } = await engineIn(work)
      const ages: [string, number, string][] = [["a.txt", 2 * 3600 + 60, "2h ago"], ["b.txt", 3 * 86400 + 60, "3d ago"], ["c.txt", 13 * 86400 + 60, "13d ago"], ["d.txt", 20 * 86400 + 60, "2w ago"], ["e.txt", 100 * 86400 + 60, "3mo ago"]]
      for (const [name, seconds] of ages) {
        await writeFile(path.join(directory, name), "x\n")
        const time = Date.now() / 1000 - seconds
        await utimes(path.join(directory, name), time, time)
      }
      const text = (await engine.find(ctx, {})).text
      for (const [name, , age] of ages) if (!text.includes(`${name} (1 line, 2 bytes, ${age})`)) fail(`${name} should be ${age}:\n${text}`)
    },
  },
  {
    name: "line-endings",
    about: "add missing final newlines while keeping each file's CRLF or LF",
    steps: [{
      prompt: "Some files in data/ use Windows line endings (CRLF) and some have no newline at the end. Make sure every file in data/ ends with a newline, using the line ending that file already uses, and change nothing else.",
      solve: async (work) => {
        for (const [name, ending] of Object.entries(DATA_ENDINGS)) if (ending) await writeFile(path.join(work, "data", name), DATA_FILES[name] + ending)
      },
    }],
    check: async (work) => {
      for (const [name, ending] of Object.entries(DATA_ENDINGS)) {
        const actual = await read(work, path.join("data", name))
        if (actual !== DATA_FILES[name] + ending) fail(`data/${name} is ${JSON.stringify(actual)}, expected ${JSON.stringify(DATA_FILES[name] + ending)}`)
      }
    },
  },
  {
    name: "type-errors",
    about: "a signature change breaks the type check in 45 places; run it and fix them",
    shell: true,
    steps: [{
      prompt: "The type check fails since a teammate changed src/snapshots.ts. Run bun typecheck and fix it so it passes, without changing what any existing call computes.",
      before: (work) => change(work, "src/snapshots.ts", (text) => text.replace(PLAIN_TAG, SEEDED_TAG)),
      solve: (work) => change(work, "src/snapshots.ts", (text) => text.replace("fileTag(text: string, seed: number)", "fileTag(text: string, seed = 0)")),
    }],
    check: async (work) => {
      // The fixture's own tests pin this vector: oh-my-pi's file_hash of "hello\n".
      const { engine, ctx, directory } = await engineIn(work)
      await writeFile(path.join(directory, "t.txt"), "hello\n")
      const shown = (await engine.read(ctx, { paths: ["t.txt"] })).text
      if (!shown.includes("#5BF9]")) fail(`reads no longer show the tag 5BF9 for "hello\\n":\n${shown}`)
    },
  },
  {
    name: "failing-test",
    about: "a one-character bug fails a test with a long diff; run the tests and fix the source",
    shell: true,
    steps: [{
      prompt: "The test suite fails. Run bun test, find the cause and fix it in src/. Do not change any test.",
      before: (work) => change(work, "src/engine.ts", (text) => text.replace(ROW, TRIMMED_ROW)),
      solve: (work) => change(work, "src/engine.ts", (text) => text.replace(TRIMMED_ROW, ROW)),
    }],
    check: async (work, pristine) => {
      await unchangedTests(work, pristine)
      const { engine, ctx, directory } = await engineIn(work)
      await writeFile(path.join(directory, "t.txt"), "keep  \nthese\t\n")
      const shown = (await engine.read(ctx, { paths: ["t.txt"] })).text
      if (!shown.includes("\n1:keep  \n2:these\t")) fail(`reads drop trailing whitespace:\n${JSON.stringify(shown)}`)
    },
  },
  {
    name: "build-log",
    about: "a build prints 3000 colored lines with its one error in the middle; find it and fix the config",
    shell: true,
    steps: [{
      prompt: "bun scripts/build.ts fails. Find out why and fix config/build.json so the build succeeds. Do not change the build script.",
      before: async (work) => {
        await mkdir(path.join(work, "scripts"), { recursive: true })
        await writeFile(path.join(work, "scripts", "build.ts"), BUILD_SCRIPT)
        await writeFile(path.join(work, "config", "build.json"), JSON.stringify(BUILD_CONFIG, null, 2) + "\n")
      },
      solve: (work) => change(work, "config/build.json", (text) => text.replace('"format": "cjs"', '"format": "esm"')),
    }],
    check: async (work) => {
      if (await read(work, "scripts/build.ts") !== BUILD_SCRIPT) fail("scripts/build.ts changed")
      const config = JSON.parse(await read(work, "config/build.json"))
      const targets: { name: string; format: string }[] = config.targets ?? []
      if (targets.length !== BUILD_CONFIG.targets.length) fail(`config/build.json has ${targets.length} targets, expected ${BUILD_CONFIG.targets.length}`)
      // Only the worker's format may change, to one the build supports.
      for (const [index, target] of BUILD_CONFIG.targets.entries()) {
        const actual = targets[index]!
        if (!BUILD_FORMATS.includes(actual.format)) fail(`target ${actual.name} has format ${JSON.stringify(actual.format)}`)
        const expected = target.name === "worker" ? { ...target, format: actual.format } : target
        if (JSON.stringify(actual) !== JSON.stringify(expected)) fail(`target ${target.name} changed beyond the worker's format`)
      }
    },
  },
  {
    name: "log-stats",
    about: "count slow requests in a 9000-line log and write the number down",
    shell: true,
    steps: [{
      prompt: "How many GET requests to /v1/orders in logs/api-2026-10-13.log took 50 ms or longer? Write only that number to answer.txt in the project root.",
      solve: async (work) => writeFile(path.join(work, "answer.txt"), `${await slowOrders(work)}\n`),
    }],
    check: async (work, pristine) => {
      const expected = await slowOrders(pristine)
      const answer = (await read(work, "answer.txt").catch(() => fail("answer.txt is missing"))).trim()
      if (answer !== String(expected)) fail(`answer.txt says ${JSON.stringify(answer)}, expected ${expected}`)
    },
  },
  {
    name: "noisy-service",
    about: "a 3200-line service log hides the cause, a route reload, among hundreds of unrelated ERROR lines; fix the route",
    shell: true,
    steps: [{
      prompt: "POST /v1/payments starts answering 503 partway through bun scripts/serve.ts, which replays a recorded day of traffic and exits. Find out why and fix config/routes.json so payments keep working. Do not change the script.",
      before: async (work) => {
        await mkdir(path.join(work, "scripts"), { recursive: true })
        await writeFile(path.join(work, "scripts", "serve.ts"), SERVE_SCRIPT)
        await writeFile(path.join(work, "config", "routes.json"), JSON.stringify(ROUTES_CONFIG, null, 2) + "\n")
      },
      solve: (work) => change(work, "config/routes.json", (text) => text.replace('"/v1/payments": "payments-v2"', '"/v1/payments": "payments"')),
    }],
    check: async (work) => {
      if (await read(work, "scripts/serve.ts") !== SERVE_SCRIPT) fail("scripts/serve.ts changed")
      const config = JSON.parse(await read(work, "config/routes.json"))
      const upstream = config.routes?.["/v1/payments"]
      // Only payments, the upstream that served the route before the reload, serves payments; the others with hosts serve other routes.
      if (upstream !== "payments") fail(`/v1/payments goes to ${JSON.stringify(upstream)}, not to "payments", the upstream that served it before the reload`)
      if (!Bun.deepEquals(config, { ...ROUTES_CONFIG, routes: { ...ROUTES_CONFIG.routes, "/v1/payments": "payments" } })) fail("config/routes.json changed beyond the /v1/payments route")
    },
  },
  {
    name: "long-run",
    about: "2394 checks, many named for errors, print one failure marked only with a cross; find it and fix the source",
    shell: true,
    steps: [{
      prompt: "bun scripts/check.ts runs the project's 2,394 checks and one fails. Find the failing check and fix the source so every check passes. Do not change the checks.",
      before: async (work) => {
        await mkdir(path.join(work, "scripts"), { recursive: true })
        await writeFile(path.join(work, "scripts", "check.ts"), CHECK_SCRIPT)
        await writeFile(path.join(work, "src", "duration.ts"), DURATION_SOURCE)
      },
      solve: (work) => change(work, "src/duration.ts", (text) => text.replace("return Math.abs(sign * total)", "return sign * total")),
    }],
    check: async (work) => {
      if (await read(work, "scripts/check.ts") !== CHECK_SCRIPT) fail("scripts/check.ts changed")
      const { parseDuration } = await load(work, "src/duration.ts")
      for (const [text, seconds] of DURATIONS) {
        const actual = parseDuration(text)
        if (actual !== seconds) fail(`parseDuration(${JSON.stringify(text)}) is ${actual}, expected ${seconds}`)
      }
    },
  },
]

// This file's top-level declarations by name, each without the comments that follow it, so a hash of a task's function can take in what it uses.
const DECLARATIONS = new Map<string, string>()
{
  const source = readFileSync(import.meta.path, "utf8")
  const starts = [...source.matchAll(/^(?:export )?(?:const|let|function|async function) (\w+)/gm)]
  starts.forEach((match, index) => DECLARATIONS.set(match[1]!, source.slice(match.index, starts[index + 1]?.index).replace(/(\n(\/\/.*|\/\*\*.*| \*.*)?)+$/, "")))
  DECLARATIONS.delete("TASKS")
}

/** `fn`'s source and that of every top-level declaration here it uses, directly or through another; Function.toString leaves out captured values. */
export function sourceWithUses(fn: Function | undefined) {
  if (!fn) return ""
  const used = new Set<string>()
  const visit = (text: string) => {
    for (const [name, declaration] of DECLARATIONS) {
      if (used.has(name) || !new RegExp(`\\b${name}\\b`).test(text)) continue
      used.add(name)
      visit(declaration)
    }
  }
  const text = fn.toString()
  visit(text)
  return [text, ...[...used].sort().map((name) => DECLARATIONS.get(name)!)].join("\n")
}
