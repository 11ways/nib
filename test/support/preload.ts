import { afterAll } from "bun:test"
import { mkdtempSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import path from "node:path"

// Every nib the suites start keeps the names it learns in a state home of the run's own, never the user's: a spawn that passes its own
// environment names this one too.
const state = mkdtempSync(path.join(tmpdir(), "nib-test-state-"))
process.env.XDG_STATE_HOME = state
afterAll(() => rmSync(state, { recursive: true, force: true }))
