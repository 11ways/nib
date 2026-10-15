#!/usr/bin/env bun
/**
 * Checks one finished tree in a fresh process, so the tree's own modules load cleanly: bun bench/check.ts TASK WORK PRISTINE < NONCE.
 * The nonce is read before any of the tree's code loads and printed only once the check returned, so an early exit with status 0 never passes.
 */
import { TASKS } from "./tasks"

const nonce = (await Bun.stdin.text()).trim()
const [name, work, pristine] = process.argv.slice(2)
const task = TASKS.find((item) => item.name === name)
if (!task || !work || !pristine || !nonce) {
  console.error("usage: bun bench/check.ts TASK WORK PRISTINE < NONCE")
  process.exit(2)
}
try {
  await task.check(work, pristine)
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error))
  process.exit(1)
}
console.log(nonce)
