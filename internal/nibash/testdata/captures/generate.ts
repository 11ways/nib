// Writes the two made-up captures of the offline check, where keywords cannot find the cause: bun internal/nibash/testdata/captures/generate.ts
// The output is deterministic; the benchmark's program tasks for these cases can print the same text.
import { gzipSync } from "node:zlib"
import { writeFileSync } from "node:fs"
import path from "node:path"

let seed = 7
const next = () => (seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648
const pick = <T>(list: T[]) => list[Math.floor(next() * list.length)]!

function time(second: number) {
  const date = new Date(Date.UTC(2026, 9, 13, 9, 0, 0) + second * 1000 + Math.floor(next() * 990))
  return date.toISOString()
}

/** A payments service whose route reload points /v1/payments at an upstream with no hosts; hundreds of unrelated pool errors flood keywords. */
export function noisyService() {
  const lines: string[] = []
  const reloadAt = 1700
  let second = 0
  let reloaded = ""
  lines.push(`${time(second)} INFO  server: listening on :8080 (routes: 42, upstreams: 7)`)
  lines.push(`${time(second)} INFO  routes: /v1/payments -> upstream "payments" (3 healthy hosts of 3)`)
  for (let n = 0; lines.length < 3200; n++) {
    second += 1
    if (lines.length === reloadAt) {
      reloaded = time(second)
      lines.push(`${reloaded} INFO  config: reloaded config/routes.json (42 routes, 7 upstreams)`)
      lines.push(`${time(second)} INFO  routes: /v1/payments -> upstream "payments-v2" (0 healthy hosts of 0)`)
      continue
    }
    const after = lines.length > reloadAt
    const roll = next()
    if (roll < 0.09) lines.push(`${time(second)} ERROR pool: connection pool exhausted (64/64 in use), retrying in 50ms`)
    else if (roll < 0.16) lines.push(`${time(second)} DEBUG health: ok (db ${1 + Math.floor(next() * 4)}ms, cache 1ms)`)
    else if (roll < 0.19) lines.push(`${time(second)} WARN  gc: pause ${80 + Math.floor(next() * 90)}ms`)
    else if (roll < 0.34) lines.push(after ? `${time(second)} INFO  http: POST /v1/payments 503 0ms` : `${time(second)} INFO  http: POST /v1/payments 201 ${30 + Math.floor(next() * 40)}ms`)
    else lines.push(`${time(second)} INFO  http: ${pick(["GET", "GET", "GET", "PUT"])} /v1/${pick(["orders", "users", "carts"])}/${10000 + Math.floor(next() * 90000)} 200 ${2 + Math.floor(next() * 60)}ms`)
  }
  lines.push(`${time(second + 1)} INFO  server: draining (SIGTERM)`)
  lines.push(`${time(second + 1)} INFO  server: stopped; ${lines.filter((line) => line.includes(" 503 ")).length} POST /v1/payments answered 503 since ${reloaded.slice(11, 19)}`)
  return lines.join("\n") + "\n"
}

const AREAS = ["durations", "parser", "router", "cache", "errors", "retry", "auth", "orders", "money", "locale"]
const VERBS = ["parses", "formats", "rounds", "keeps", "merges", "sorts", "rejects", "accepts", "reports", "skips"]
const THINGS = ["empty input", "a trailing comma", "unicode names", "large numbers", "nested lists", "a missing field", "duplicate keys", "negative values", "an expired token", "a slow upstream"]
const NOISY = ["reports an error for {}", "fails fast on {}", "throws a TypeError for {}", "keeps the failure count for {}", "returns errors for {}"]

/** A test run of 2,400 passing tests, a tenth of them named for errors and failures, and one failure in the middle marked only with a cross. */
export function longRun() {
  const lines: string[] = []
  const failAt = 1400
  let count = 0
  for (let file = 0; file < 38; file++) {
    lines.push(`src/${AREAS[file % AREAS.length]}${file}.test.ts:`)
    for (let n = 0; n < 63; n++) {
      count++
      const thing = pick(THINGS)
      const name = next() < 0.11 ? pick(NOISY).replace("{}", thing) : `${pick(VERBS)} ${thing}`
      const ms = (next() * 0.9).toFixed(2)
      if (count === failAt) {
        lines.push(`\u2715 durations > parses "-90s" [0.31ms]`)
        lines.push(`    got 90, want -90 (src/duration.ts:41)`)
        lines.push(`    at parseDuration (src/duration.ts:41:12)`)
        continue
      }
      lines.push(`\u2713 ${AREAS[file % AREAS.length]} > ${name} [${ms}ms]`)
    }
    lines.push("")
  }
  lines.push(` ${count - 1} pass`, " 1 fail", ` ${count * 3} expect() calls`, `Ran ${count} tests across 38 files. [1.84s]`)
  return lines.join("\n") + "\n"
}

const here = path.dirname(new URL(import.meta.url).pathname)
writeFileSync(path.join(here, "noisy-service.out.gz"), gzipSync(noisyService(), { level: 9 }))
writeFileSync(path.join(here, "long-run.out.gz"), gzipSync(longRun(), { level: 9 }))
