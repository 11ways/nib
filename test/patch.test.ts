import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { Engine, type Context } from "../src/engine"
import { parsePatch } from "../src/patch"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })

async function fixture(files: Record<string, string>) {
  const directory = await mkdtemp(path.join(tmpdir(), "nib-patch-"))
  directories.push(directory)
  for (const [name, content] of Object.entries(files)) await writeFile(path.join(directory, name), content)
  const ctx: Context = { directory, session: "session", signal: new AbortController().signal }
  const engine = new Engine()
  return {
    directory,
    read: async (name: string) => /#([0-9A-F]{4})\]/.exec((await engine.read(ctx, { paths: [name] })).text)![1]!,
    patch: (input: string) => engine.patch(ctx, input),
    file: (name: string) => readFile(path.join(directory, name), "utf8"),
  }
}

describe("parsePatch", () => {
  test("parses sections, sorts hunks by original position and builds created file content", () => {
    const sections = parsePatch([
      "*** Begin Patch",
      "[src/a.ts#3c4d]",
      "PUT >$:",
      "+tail",
      "CUT 7.=8",
      "PUT 2.=3:",
      "+two",
      "+",
      "PUT <2:",
      "+before two",
      "PUT >1:",
      "+after one",
      "*** Add File: notes/new.md",
      "+# Title",
      "+",
      "*** Add File: empty.txt",
      "[\"with space.ts\"#ABCD]",
      "MV 'moved here.ts'",
      "*** End Patch",
    ].join("\n"))
    expect(sections).toEqual([
      {
        kind: "edit", path: "src/a.ts", tag: "3C4D", line: 2, edits: [
          { start: 1, end: 1, operation: "after", content: ["after one"] },
          { start: 2, end: 2, operation: "before", content: ["before two"] },
          { start: 2, end: 3, operation: "replace", content: ["two", ""] },
          { start: 7, end: 8, operation: "replace", content: [] },
          { start: Number.MAX_SAFE_INTEGER, end: Number.MAX_SAFE_INTEGER, operation: "after", content: ["tail"], edge: "tail" },
        ],
      },
      { kind: "create", path: "notes/new.md", content: "# Title\n\n", line: 13 },
      { kind: "create", path: "empty.txt", content: "", line: 16 },
      { kind: "edit", path: "with space.ts", tag: "ABCD", line: 17, edits: [], move: "moved here.ts" },
    ])
  })

  test("tolerates a missing envelope, CRLF and blank lines between hunks", () => {
    expect(parsePatch("\r\n[a.ts#ABCD]\r\n\r\nPUT 1.=1:\r\n+x\r\n\r\n")).toEqual([
      { kind: "edit", path: "a.ts", tag: "ABCD", line: 2, edits: [{ start: 1, end: 1, operation: "replace", content: ["x"] }] },
    ])
  })

  test.each([
    ["*** Begin Patch\nPUT 1.=1:\n+x\n*** End Patch", "Patch line 2: expected a [PATH#TAG] header"],
    ["*** Begin Patch\n[a.ts#ABCD]\nPUT 1.=1:\n*** End Patch", "Patch line 3: PUT needs at least one + row"],
    ["*** Begin Patch\n[a.ts#ABCD]\n-old\n+new\n*** End Patch", "Patch line 3: - rows are not accepted"],
    ["[a.ts#ABCD]\n@@ -1,2 +1,2 @@", "unified-diff hunk headers are not hashline"],
    ["*** Update File: a.ts\n+x", "apply_patch file headers are not hashline"],
    ["[a.ts#ABCD]\n12:old", "LINE:TEXT rows are read output"],
    ["[a.ts#ABCD]\nPUT 3:\n+x", "a single-line replace is written PUT N.=N:"],
    ["[a.ts#ABCD]\nPUT 1..3:\n+x", "ranges are written N.=M"],
    ["[a.ts#ABCD]\nREM\nCUT 1.=1", "nothing may follow REM or MV"],
    ["[a.ts#ABCD]\nMV b.ts\n[a.ts#ABCD]\nCUT 1.=1", "a.ts already ends with REM or MV"],
    ["*** Begin Patch\n*** Add File: a.ts\n+x\nPUT 1.=1:\n+y\n*** End Patch", "Patch line 4: an Add File section takes only + rows"],
    ["*** Begin Patch\n[a.ts#ABCD]\n*** End Patch", "Section [a.ts#ABCD] has no PUT, CUT, REM or MV"],
    ["*** Begin Patch\n*** End Patch", "Patch contains no sections"],
  ])("rejects malformed patches: %j", (input, message) => {
    expect(() => parsePatch(input)).toThrow(message)
  })
})

describe("edit tool", () => {
  test("end-of-file inserts apply despite a stale tag; anchored hunks still need a safe rebase", async () => {
    const f = await fixture({ "a.ts": "a\nb\n" })
    const tag = await f.read("a.ts")
    await writeFile(path.join(f.directory, "a.ts"), "changed\nb\n")
    const tail = await f.patch(`*** Begin Patch\n[a.ts#${tag}]\nPUT >$:\n+tail\n*** End Patch`)
    expect(await f.file("a.ts")).toBe("changed\nb\ntail\n")
    expect(tail.text).toContain("Applied the end-of-file insert despite a stale tag")
    await expect(f.patch(`*** Begin Patch\n[a.ts#${tag}]\nPUT 1.=1:\n+A\n*** End Patch`)).rejects.toThrow("is stale")
    expect(await f.file("a.ts")).toBe("changed\nb\ntail\n")
  })

  test("inserts at the same gap both apply, after-inserts first", async () => {
    const f = await fixture({ "a.ts": "a\nb\n" })
    const tag = await f.read("a.ts")
    await f.patch(`*** Begin Patch\n[a.ts#${tag}]\nPUT <2:\n+before b\nPUT >1:\n+after a\n*** End Patch`)
    expect(await f.file("a.ts")).toBe("a\nafter a\nbefore b\nb\n")
  })

  test("overlapping hunks are rejected with the section named", async () => {
    const f = await fixture({ "a.ts": "a\nb\nc\n" })
    const tag = await f.read("a.ts")
    await expect(f.patch(`*** Begin Patch\n[a.ts#${tag}]\nPUT 1.=2:\n+x\nCUT 2.=3\n*** End Patch`)).rejects.toThrow(`[a.ts#${tag}]: Edits overlap`)
    expect(await f.file("a.ts")).toBe("a\nb\nc\n")
  })

  test("body rows keep backticks, quotes, dollars and leading + or - literally", async () => {
    const f = await fixture({ "a.ts": "x\n" })
    const tag = await f.read("a.ts")
    await f.patch(`[a.ts#${tag}]\nPUT 1.=1:\n+const s = \`\${a}\` + "b" // 'c'\n+- item\n++ plus`)
    expect(await f.file("a.ts")).toBe("const s = `${a}` + \"b\" // 'c'\n- item\n+ plus\n")
  })
})
