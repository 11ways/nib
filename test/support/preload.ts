import { afterAll } from "bun:test"
import { mkdtempSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import path from "node:path"

// Every nib the suites start keeps the names it learns in a state home of the run's own, never the user's, and moves what it deletes to a
// trash of the run's own: a spawn that passes its own environment names these too.
const state = mkdtempSync(path.join(tmpdir(), "nib-test-state-"))
const data = mkdtempSync(path.join(tmpdir(), "nib-test-data-"))
process.env.XDG_STATE_HOME = state
process.env.XDG_DATA_HOME = data
afterAll(() => {
	rmSync(state, { recursive: true, force: true })
	rmSync(data, { recursive: true, force: true })
})
