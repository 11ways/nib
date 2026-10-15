/** The project every task starts from: nib's own source at a pinned commit, plus generated logs, data files and a minified bundle. */
import { createHash } from "node:crypto"
import { rmSync } from "node:fs"
import { cp, mkdir, mkdtemp, readdir, readFile, rm, symlink, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"

export const REPO = path.join(import.meta.dir, "..")
export const FIXTURE_COMMIT = "9f3603fe2993cb97667128bcda2d7bed17f0366b"
const ARCHIVED = ["src", "test", "package.json", "tsconfig.json", "README.md"]
export const OLD_HOST = "api.old-example.com"
export const NEW_HOST = "api.example.com"

/** A deterministic random source, so every fixture build is byte-identical. */
function random(seed: number) {
  let state = seed
  return () => {
    state = (state + 0x6d2b79f5) | 0
    let value = Math.imul(state ^ (state >>> 15), 1 | state)
    value = (value + Math.imul(value ^ (value >>> 7), 61 | value)) ^ value
    return ((value ^ (value >>> 14)) >>> 0) / 4294967296
  }
}

const COLORS = { INFO: "\x1b[32mINFO\x1b[0m", WARN: "\x1b[33mWARN\x1b[0m", ERROR: "\x1b[31mERROR\x1b[0m", FATAL: "\x1b[41mFATAL\x1b[0m" }

/** One day of API service log: requests, heartbeats, slow-query warnings and a recurring stack trace, ending in `ending`. */
function apiLog(day: string, lines: number, seed: number, ending: string[]) {
  const next = random(seed)
  const out: string[] = []
  let millis = 0
  const stamp = () => {
    millis += 1 + Math.floor(next() * 900)
    const date = new Date(Date.UTC(2026, 9, Number(day.slice(8)), 6) + millis)
    return date.toISOString()
  }
  const paths = ["/v1/items", "/v1/orders", "/v1/users", "/v1/search", "/v1/health"]
  const trace = ["    at Pool.acquire (src/db/pool.ts:88:11)", "    at Repository.find (src/db/repository.ts:41:20)", "    at OrdersHandler.list (src/http/orders.ts:132:9)", "    at Router.dispatch (src/http/router.ts:57:14)"]
  out.push(`${stamp()} ${COLORS.INFO} api starting (release 4.2.0, pid ${4000 + Math.floor(next() * 5000)})`, `${stamp()} ${COLORS.INFO} config loaded from config/api.json`)
  while (out.length < lines) {
    const roll = next()
    if (roll < 0.35) out.push(`${stamp()} ${COLORS.INFO} heartbeat ok`)
    else if (roll < 0.9) {
      const route = paths[Math.floor(next() * paths.length)]!
      out.push(`${stamp()} ${COLORS.INFO} GET ${route}/${Math.floor(next() * 90000)} 200 ${3 + Math.floor(next() * 80)}ms`)
    } else if (roll < 0.98) out.push(`${stamp()} ${COLORS.WARN} slow query on orders: ${200 + Math.floor(next() * 900)}ms (retry ${1 + Math.floor(next() * 3)}/5)`)
    else out.push(`${stamp()} ${COLORS.ERROR} pool exhausted while listing orders`, ...trace)
  }
  out.push(...ending.map((line) => `${stamp()} ${line}`))
  return out.join("\n") + "\n"
}

/** A one-line bundle shaped like minifier output, holding the old API host three times. */
function bundle() {
  const next = random(7)
  const name = () => String.fromCharCode(97 + Math.floor(next() * 26)) + Math.floor(next() * 99)
  const parts: string[] = []
  for (let index = 0; index < 1400; index++) {
    const [a, b, c] = [name(), name(), name()]
    const roll = next()
    if (roll < 0.4) parts.push(`function ${a}(${b},${c}){return ${b}.map(function(e){return e*${c}+${Math.floor(next() * 100)}})}`)
    else if (roll < 0.7) parts.push(`var ${a}={${b}:${Math.floor(next() * 1000)},${c}:"${name()}-${name()}",n:!${Math.floor(next() * 2)}}`)
    else parts.push(`${a}.prototype.${b}=function(t,r){if(!t)throw new Error("${c} missing");return this.${c}(t,r||{})}`)
    if (index === 300) parts.push(`var API_BASE="https://${OLD_HOST}/v1"`)
    if (index === 700) parts.push(`var CSP={connectSrc:["'self'","https://${OLD_HOST}"]}`)
    if (index === 1100) parts.push(`function health(){return fetch("//${OLD_HOST}/health")}`)
  }
  return `/*! vendor bundle 3.2.1 | MIT */\n!function(){"use strict";${parts.join(";")}}();\n`
}

/** Line-ending samples: each file keeps CRLF or LF, and some lack a final newline. */
export const DATA_FILES: Record<string, string> = {
  "a.csv": "id,name\n1,alpha\n2,beta\n",
  "b.csv": "id,qty\r\n1,4\r\n2,9",
  "c.csv": "x,y\n1,2\n3,4",
  "d.csv": "key,value\r\nmode,fast\r\n",
  "e.csv": "m,n\n5,6",
  "f.csv": "h1,h2\r\nv1,v2\r\nv3,v4",
}

export const API_CONFIG = { host: "0.0.0.0", port: 8080, timeoutMs: 5000, upstream: "postgres://db:5432/api" }

async function writeExtras(directory: string) {
  await mkdir(path.join(directory, "logs"))
  const logs: [string, string][] = [
    ["2026-10-12", apiLog("2026-10-12", 4000, 12, [`${COLORS.ERROR} config: missing key "timeoutMs" in config/api.json, using 30000`, `${COLORS.INFO} shutting down`])],
    ["2026-10-13", apiLog("2026-10-13", 9000, 13, [`${COLORS.ERROR} upstream: connection refused (db:5432)`, `${COLORS.INFO} reconnected to db:5432`, `${COLORS.INFO} shutting down`])],
    ["2026-10-14", apiLog("2026-10-14", 40000, 14, [`${COLORS.FATAL} config: missing required key "retryLimit" in config/api.json (a number; the previous release used 7)`, `${COLORS.INFO} exiting with code 78`])],
  ]
  for (const [day, content] of logs) {
    const file = path.join(directory, "logs", `api-${day}.log`)
    await writeFile(file, content)
    // Each log was last written the morning it ran, so the newest file is also the latest by time.
    const time = Date.UTC(2026, 9, Number(day.slice(8)), 11) / 1000
    await utimes(file, time, time)
  }
  await mkdir(path.join(directory, "config"))
  await writeFile(path.join(directory, "config", "api.json"), JSON.stringify(API_CONFIG, null, 2) + "\n")
  await mkdir(path.join(directory, "public"))
  await writeFile(path.join(directory, "public", "vendor.min.js"), bundle())
  await mkdir(path.join(directory, "data"))
  for (const [name, content] of Object.entries(DATA_FILES)) await writeFile(path.join(directory, "data", name), content)
}

/** Builds the fixture into the empty `directory`, without node_modules. */
export async function writeFixture(directory: string) {
  const archive = path.join(directory, ".fixture.tar")
  const run = async (command: string[]) => {
    const child = Bun.spawn(command, { stdout: "ignore", stderr: "pipe" })
    if (await child.exited) throw new Error(`${command.join(" ")} failed: ${await new Response(child.stderr).text()}`)
  }
  await run(["git", "-C", REPO, "archive", "--format=tar", "-o", archive, FIXTURE_COMMIT, ...ARCHIVED])
  await run(["tar", "-xf", archive, "-C", directory])
  await rm(archive)
  await writeExtras(directory)
}

/** Every file below `directory` except node_modules, as sorted relative paths. */
export async function listFiles(directory: string): Promise<string[]> {
  const entries = await readdir(directory, { recursive: true, withFileTypes: true })
  return entries
    .filter((entry) => entry.isFile() && !path.relative(directory, path.join(entry.parentPath, entry.name)).split(path.sep).includes("node_modules"))
    .map((entry) => path.relative(directory, path.join(entry.parentPath, entry.name)))
    .sort()
}

/** A content hash of a prepared fixture, so results from another fixture are never mixed in. */
export async function hashTree(directory: string) {
  const hash = createHash("sha256")
  for (const file of await listFiles(directory)) hash.update(file).update("\0").update(await readFile(path.join(directory, file))).update("\0")
  return hash.digest("hex").slice(0, 16)
}

let pristine: Promise<{ directory: string; hash: string }> | undefined

/** The untouched fixture for this process, built once; runs copy it and diff against it. */
export function pristineFixture() {
  return pristine ??= (async () => {
    const directory = await mkdtemp(path.join(tmpdir(), "nib-bench-pristine-"))
    process.on("exit", () => rmSync(directory, { recursive: true, force: true }))
    await writeFixture(directory)
    return { directory, hash: await hashTree(directory) }
  })()
}

/** Copies the pristine fixture into `work`, keeping file times. */
export async function copyFixture(work: string) {
  const { directory } = await pristineFixture()
  await cp(directory, work, { recursive: true, preserveTimestamps: true })
}

/**
 * Links nib's node_modules into `work` so checks and type checks resolve; only once the model is done, since a harness that judges paths as written
 * (OpenCode does) would let it read and write nib's own dependencies through the link.
 */
export async function linkModules(work: string) {
  await symlink(path.join(REPO, "node_modules"), path.join(work, "node_modules")).catch((error: NodeJS.ErrnoException) => {
    if (error.code !== "EEXIST") throw error
  })
}

/** The files a run added, changed or deleted, with their final contents (null when deleted). */
export async function changedFiles(work: string) {
  const { directory } = await pristineFixture()
  const before = new Set(await listFiles(directory))
  const after = await listFiles(work)
  const changes: Record<string, string | null> = {}
  for (const file of after) {
    const content = await readFile(path.join(work, file), "utf8")
    if (!before.has(file) || content !== await readFile(path.join(directory, file), "utf8")) changes[file] = content
  }
  for (const file of before) if (!after.includes(file)) changes[file] = null
  return changes
}

/** Rebuilds a finished run's tree in `work` from the pristine fixture and its stored changes, for checking it again. */
export async function applyChanges(work: string, changes: Record<string, string | null>) {
  await copyFixture(work)
  for (const [file, content] of Object.entries(changes)) {
    const target = path.join(work, file)
    if (content === null) await rm(target, { force: true })
    else {
      await mkdir(path.dirname(target), { recursive: true })
      await writeFile(target, content)
    }
  }
}
