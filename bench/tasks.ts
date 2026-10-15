/** Benchmark tasks: prompts a model works through, a check of the finished tree, and a reference solution that proves the check passes. */
import { diffArrays } from "diff"
import { readFileSync, rmSync } from "node:fs"
import { mkdtemp, readdir, readFile, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { API_CONFIG, DATA_FILES, NEW_HOST, OLD_HOST } from "./fixture"

/** One prompt of a task; `before` changes the tree first, as a teammate would, and `solve` is the reference for this step. */
export type Step = { prompt: string; before?: (work: string) => Promise<void>; solve: (work: string) => Promise<void> }
/**
 * `check` throws with the reason when the finished tree in `work` is wrong; `pristine` is the untouched fixture. `tests` names the test file the
 * prompts ask for: verification runs it, and it must pass with more tests and assertions than the untouched fixture's copy.
 */
export type Task = { name: string; about: string; steps: Step[]; check: (work: string, pristine: string) => Promise<void>; tests?: string }
/** One-shot tasks take one prompt; multi-step ones take several on the same files, the repeated editing nib is built for. */
export type TaskKind = "one-shot" | "multi-step"
export const taskKind = (task: Task): TaskKind => task.steps.length > 1 ? "multi-step" : "one-shot"

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
