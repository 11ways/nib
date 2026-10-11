import { afterEach, describe, expect, test } from "bun:test"
import { chmod, link, lstat, mkdir, mkdtemp, open, readdir, readFile, readlink, rm, stat, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { Engine, type Context } from "../src/engine"
import { fileTag, SnapshotStore } from "../src/snapshots"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })

type Edit = { start: number; end?: number; content: string; operation: "replace" | "before" | "after" }

/** Renders structured edits as patch hunks so the structured-edit cases keep exercising the same engine paths. */
function hunks(edits: Edit[]) {
  return edits.map((edit) => {
    const rows = edit.content.replace(/\n$/, "").split("\n").map((row) => `+${row}`)
    if (edit.operation === "replace" && edit.content === "") return `CUT ${edit.start}.=${edit.end ?? edit.start}`
    if (edit.operation === "replace") return [`PUT ${edit.start}.=${edit.end ?? edit.start}:`, ...rows].join("\n")
    return [`PUT ${edit.operation === "before" ? "<" : ">"}${edit.start}:`, ...rows].join("\n")
  }).join("\n")
}

async function workspace() {
  const directory = await mkdtemp(path.join(tmpdir(), "nib-"))
  directories.push(directory)
  return directory
}

async function fixture(content = "one\ntwo\nthree\n") {
  const directory = await workspace()
  const filePath = path.join(directory, "file.txt")
  await writeFile(filePath, content)
  const controller = new AbortController()
  const ctx: Context = { directory, session: "session", signal: controller.signal }
  const engine = new Engine()
  const tagOf = (output: string) => /^\[[^\n]*#([0-9A-F]{4})\]/m.exec(output)?.[1] ?? ""
  const read = async (selector = "", options: { maxLineLength?: number; file?: string } = {}) => {
    const result = await engine.read(ctx, { paths: [(options.file ?? filePath) + selector], maxLineLength: options.maxLineLength })
    return { ...result, tag: tagOf(result.text), anchors: [...result.text.matchAll(/^(\d+):/gm)].map((match) => Number(match[1])) }
  }
  const patch = async (input: string) => {
    const result = await engine.patch(ctx, input)
    return { ...result, tag: tagOf(result.text) }
  }
  const edit = (tag: string, edits: Edit[], file = "file.txt") => patch(`[${file}#${tag}]\n${hunks(edits)}`)
  return { directory, filePath, controller, ctx, engine, read, edit, patch }
}

describe("read", () => {
  test("tagged header, relative display path, selectors and continuation hints", async () => {
    const f = await fixture("one\r\ntwo\r\nthree")
    const tag = fileTag("one\r\ntwo\r\nthree")
    expect((await f.read()).text).toBe(`[file.txt#${tag}]\n1:one\n2:two\n3:three`)
    expect((await f.read(":2-2")).text).toBe(`[file.txt#${tag}]\n2:two\n[3 lines total]`)
    expect((await f.read(":2")).text).toBe(`[file.txt#${tag}]\n2:two\n3:three\n[3 lines total]`)
    expect((await f.read(":1+2")).text).toBe(`[file.txt#${tag}]\n1:one\n2:two\n[3 lines total]`)
    expect((await f.read(":-1")).text).toBe(`[file.txt#${tag}]\n3:three\n[3 lines total]`)
    expect((await f.read(":1,3")).text).toBe(`[file.txt#${tag}]\n1:one\n...\n3:three\n[3 lines total]`)
    await expect(f.read(":4")).rejects.toThrow("beyond the end")
    await expect(f.read(":3-1")).rejects.toThrow("reversed")
  })

  test("several files and a directory in one call; literal names win over selectors", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "b.txt"), "bee\n")
    await writeFile(path.join(f.directory, "odd:1"), "literal\n")
    await mkdir(path.join(f.directory, "sub"))
    const result = await f.engine.read(f.ctx, { paths: ["file.txt:2-2", "b.txt", "odd:1", "."] })
    expect(result.text).toBe([
      `[file.txt#${fileTag("one\ntwo\nthree\n")}]\n2:two\n[3 lines total]`,
      `[b.txt#${fileTag("bee\n")}]\n1:bee`,
      `[odd:1#${fileTag("literal\n")}]\n1:literal`,
      `${f.directory}/\nb.txt\nfile.txt\nodd:1\nsub/`,
    ].join("\n\n"))
  })

  test("an open range returns up to 2000 lines, with a hint to continue", async () => {
    const f = await fixture(Array.from({ length: 2500 }, (_, index) => `${index}`).join("\n"))
    const first = await f.read(":1-")
    expect(first.anchors).toHaveLength(2000)
    expect(first.text).toEndWith("[2500 lines total; continue with file.txt:2001-]")
    expect((await f.read(":2001")).anchors).toHaveLength(500)
  })

  test("binary rejection, images attached, PDFs deferred to the built-in Read, size limit", async () => {
    const f = await fixture()
    await writeFile(f.filePath, Buffer.from([0, 1, 2, 3]))
    await expect(f.read()).rejects.toThrow("Binary")
    await writeFile(f.filePath, Buffer.from([0xff, 0xfe, 0x61]))
    await expect(f.read()).rejects.toThrow()
    const png = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])
    await writeFile(f.filePath, png)
    const image = await f.read()
    expect(image.images).toEqual([{ data: png.toString("base64"), mimeType: "image/png" }])
    await writeFile(f.filePath, "%PDF-1.7\n")
    expect((await f.read()).text).toContain("use the built-in Read tool")
    await writeFile(f.filePath, Buffer.alloc(5 * 1024 * 1024 + 1))
    await expect(f.read()).rejects.toThrow("size limit")
  })

  test("truncated long lines are not editable until read alone; output budget and text size limits", async () => {
    const f = await fixture("x".repeat(2001) + "\nshort\n")
    const r = await f.read()
    expect(r.text).toContain("[1 truncated at 2000 of 2001 chars; read file.txt:1 with maxLineLength before editing]|")
    expect(r.anchors).toEqual([2])
    await expect(f.edit(r.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow("Preview of the first 1 unseen line(s)")
    await expect(f.read("", { maxLineLength: 40_000 })).rejects.toThrow("single-line selector")
    await expect(f.read(":1-2", { maxLineLength: 40_000 })).rejects.toThrow("single-line selector")
    const full = await f.read(":1", { maxLineLength: 40_000 })
    expect(full.anchors).toEqual([1])
    await f.edit(full.tag, [{ start: 1, content: "x", operation: "replace" }])
    expect(await readFile(f.filePath, "utf8")).toBe("x\nshort\n")
    await writeFile(f.filePath, ("x".repeat(1900) + "\n").repeat(100))
    const page = await f.read(":1-")
    expect(page.text).toMatch(/continue with file\.txt:\d+-\]$/)
    expect(Buffer.byteLength(page.text)).toBeLessThan(41 * 1024)
    await writeFile(f.filePath, "x".repeat(2 * 1024 * 1024 + 1))
    await expect(f.read()).rejects.toThrow("Text exceeds")
  })

  test("a single 2 MiB line can be read and edited, keeping stale-tag protection", async () => {
    const f = await fixture("x".repeat(2 * 1024 * 1024))
    const full = await f.read(":1", { maxLineLength: 2 * 1024 * 1024 })
    expect(full.anchors).toEqual([1])
    await f.edit(full.tag, [{ start: 1, content: "short", operation: "replace" }])
    expect(await readFile(f.filePath, "utf8")).toBe("short")
    await expect(f.edit(full.tag, [{ start: 1, content: "stale", operation: "replace" }])).rejects.toThrow("is stale")
  })
})

describe("large reads", () => {
  const source = (members: number) => [
    "import { a } from \"a\"",
    "import { b } from \"b\"",
    "import { c } from \"c\"",
    "",
    "// Explains the class.",
    "export class Big {",
    ...Array.from({ length: members }, (_, index) => [`  method${index}(value: number) {`, `    const doubled = value * ${index}`, "    return doubled", "  }", ""]).flat(),
    "}",
    "",
    "export function helper() {",
    "  const inner = 1",
    "  return inner",
    "}",
  ].join("\n") + "\n"

  test("an unselected read of a large file returns an outline whose rows are editable", async () => {
    const f = await fixture(source(120))
    const r = await f.read()
    const lines = source(120).split("\n")
    expect(r.text.split("\n")[0]).toBe(`[file.txt#${r.tag}] ${lines.length - 1} lines, about 3k tokens; outline only. Read the ranges you need, like file.txt:120-180, or file.txt:1- for everything.`)
    expect(r.text).toContain("\n1:import { a } from \"a\"\n[lines 2-3: 2 more import lines]\n6:export class Big {\n7:  method0(value: number) {\n12:  method1(value: number) {")
    expect(r.text).toContain(`\n${lines.indexOf("export function helper() {") + 1}:export function helper() {`)
    expect(r.text).not.toContain("const doubled")
    expect(r.text).not.toContain("Explains")
    const edited = await f.edit(r.tag, [{ start: 7, content: "  method0(value: number, scale = 1) {", operation: "replace" }])
    expect(edited.text).toContain("7:  method0(value: number, scale = 1) {")
    await expect(f.edit(edited.tag, [{ start: 13, content: "x", operation: "replace" }])).rejects.toThrow("never displayed in full")
    expect((await f.read(":1-")).anchors).toHaveLength(lines.length - 1)
  })

  test("outlines skip comments, multi-line strings and signature tails, and list class members of any language", async () => {
    const f = await fixture()
    const java = [
      "package demo;",
      "",
      "/**",
      " * Javadoc lines are indented by one space.",
      " */",
      "public class Parser extends Base {",
      "    private static final String NAME = \"x\";",
      ...Array.from({ length: 300 }, (_, index) => [`    public Result parse${index}(String input) {`, "        return null;", "    }"]).flat(),
      "}",
    ].join("\n")
    await writeFile(path.join(f.directory, "Parser.java"), java)
    const javaRows = (await f.read("", { file: path.join(f.directory, "Parser.java") })).text.split("\n").slice(1, 5)
    expect(javaRows).toEqual(["1:package demo;", "6:public class Parser extends Base {", "7:    private static final String NAME = \"x\";", "8:    public Result parse0(String input) {"])
    expect((await f.read("", { file: path.join(f.directory, "Parser.java") })).text).toEndWith("\n[outline capped at 150 rows; 153 more members or declarations not shown]")
    const python = [
      "#: A documented constant.",
      "LIMIT = 3",
      "TEMPLATE = \"\"\"",
      "not = code",
      "\"\"\"",
      "def run(",
      "    value: int,",
      ") -> None:",
      ...Array.from({ length: 600 }, () => "    pass"),
      "class Thing:",
      "    def method(self):",
      "        return 1",
    ].join("\n")
    await writeFile(path.join(f.directory, "mod.py"), python + "\n" + Array.from({ length: 4 }, (_, index) => `def extra${index}():\n    pass`).join("\n"))
    const pythonRows = (await f.read("", { file: path.join(f.directory, "mod.py") })).text.split("\n").slice(1, 7)
    expect(pythonRows).toEqual(["2:LIMIT = 3", "3:TEMPLATE = \"\"\"", "6:def run(", "609:class Thing:", "610:    def method(self):", "612:def extra0():"])
  })

  test("small files and explicit ranges are never outlined", async () => {
    const f = await fixture(source(20))
    expect((await f.read()).anchors).toHaveLength(source(20).split("\n").length - 1)
  })

  test("markdown outlines headings outside code fences", async () => {
    const f = await fixture()
    const body = ["# Title", "", "```", "# not a heading", "```", "## Section", ...Array.from({ length: 600 }, (_, index) => `text ${index}`), "### Deep", "#### Deeper", "##### Deepest"].join("\n")
    await writeFile(path.join(f.directory, "doc.md"), body)
    const r = await f.read("", { file: path.join(f.directory, "doc.md") })
    expect(r.text.split("\n").slice(1)).toEqual(["1:# Title", "6:## Section", "607:### Deep", "608:#### Deeper", "609:##### Deepest"])
  })

  test("flat data and outline-less files show their first 100 lines", async () => {
    const f = await fixture(Array.from({ length: 900 }, (_, index) => `2026-10-11 log entry ${index}`).join("\n"))
    const r = await f.read()
    expect(r.text.split("\n")[0]).toEndWith("first 100 lines only. Continue with file.txt:101-, or grep it.")
    expect(r.anchors).toHaveLength(100)
    await writeFile(f.filePath, JSON.stringify({ items: Array.from({ length: 700 }, (_, index) => ({ index })) }, null, 2))
    expect((await f.read()).anchors).toHaveLength(100)
  })

  test("lockfiles, minified files and generated directories are skipped unless a range is given", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "package-lock.json"), "{\n" + "  \"x\": 1,\n".repeat(1000) + "}\n")
    await writeFile(path.join(f.directory, "app.js"), "var a=1;".repeat(2000) + "\n")
    await mkdir(path.join(f.directory, "node_modules/lib"), { recursive: true })
    await writeFile(path.join(f.directory, "node_modules/lib/index.js"), "export const x = 1\n".repeat(400))
    await writeFile(path.join(f.directory, "node_modules/lib/small.d.ts"), "export declare const x: number\n")
    const r = await f.engine.read(f.ctx, { paths: ["package-lock.json", "app.js", "node_modules/lib/index.js", "node_modules/lib/small.d.ts"] })
    const parts = r.text.split("\n\n")
    expect(parts[0]).toBe("[package-lock.json] skipped: lockfile, 1002 lines, about 3k tokens. Read package-lock.json:1- to read it anyway, a range like package-lock.json:1-100, or grep it.")
    expect(parts[1]).toStartWith("[app.js] skipped: minified (very long lines)")
    expect(parts[2]).toStartWith("[node_modules/lib/index.js] skipped: inside node_modules/")
    expect(parts[3]).toStartWith("[node_modules/lib/small.d.ts#")
    expect((await f.engine.read(f.ctx, { paths: ["package-lock.json:1-"] })).text).toContain("\n2:  \"x\": 1,")
  })

  test("one call shares its budget so small files are not starved by a large one", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "big.txt"), ("y".repeat(1000) + "\n").repeat(100))
    await writeFile(path.join(f.directory, "small.txt"), "tiny\n")
    const r = await f.engine.read(f.ctx, { paths: ["big.txt:1-", "small.txt", "big.txt:50-"] })
    const parts = r.text.split("\n\n")
    expect(parts[1]).toBe(`[small.txt#${fileTag("tiny\n")}]\n1:tiny`)
    expect(parts[0]).toEndWith("-]")
    expect(parts[2]).toMatch(/^\[big\.txt#[0-9A-F]{4}\]\n50:y+\n/)
    expect(Buffer.byteLength(r.text)).toBeLessThan(41 * 1024)
  })
})

describe("edit", () => {
  test.each(["﻿one\r\ntwo\r\n", "﻿one\r\ntwo", "one\ntwo\n", "one\ntwo"])("preserves encoding, mode and final newline: %j", async (source) => {
    const f = await fixture(source)
    await chmod(f.filePath, 0o640)
    const original = await f.read()
    await f.edit(original.tag, [{ start: 1, content: "ONE", operation: "replace" }])
    expect(await readFile(f.filePath, "utf8")).toBe(source.replace("one", "ONE"))
    expect((await stat(f.filePath)).mode & 0o777).toBe(0o640)
  })

  test("empty existing file uses a virtual line 1; BOM-only file remains BOM-prefixed", async () => {
    for (const [content, operation] of [["", "replace"], ["", "before"], ["", "after"], ["﻿", "replace"]] as const) {
      const f = await fixture(content)
      const original = await f.read()
      expect(original.anchors).toEqual([1])
      await f.edit(original.tag, [{ start: 1, content: "hello\n", operation }])
      expect(await readFile(f.filePath, "utf8")).toBe(content + "hello")
    }
  })

  test("original coordinates across hunks, ranges, before/after, deletion and tail append", async () => {
    const f = await fixture("a\nb\nc\nd\ne")
    const original = await f.read()
    await f.patch(`*** Begin Patch\n[file.txt#${original.tag}]\nPUT <1:\n+first\nCUT 2.=3\nPUT >5:\n+last\nPUT >$:\n+tail\n*** End Patch\n`)
    expect(await readFile(f.filePath, "utf8")).toBe("first\na\nd\ne\nlast\ntail")
  })

  test("mixed line endings retain untouched bytes and the original final terminator", async () => {
    const f = await fixture("a\r\nb\r\nc\n")
    const r = await f.read()
    await f.edit(r.tag, [{ start: 1, content: "A\nB\n", operation: "replace" }])
    expect(await readFile(f.filePath, "utf8")).toBe("A\r\nB\r\nb\r\nc\n")
  })

  test("deleting every line preserves only the BOM", async () => {
    const f = await fixture("﻿one\r\ntwo\r\n")
    const r = await f.read()
    const result = await f.edit(r.tag, [{ start: 1, end: 2, content: "", operation: "replace" }])
    expect(result.text).toContain("(file is now empty)")
    expect(await readFile(f.filePath, "utf8")).toBe("﻿")
  })

  test("large line-count insertions avoid argument-count limits", async () => {
    const f = await fixture("end")
    const r = await f.read()
    const content = "a\n".repeat(150_000)
    await f.edit(r.tag, [{ start: 1, content, operation: "before" }])
    expect(await readFile(f.filePath, "utf8")).toBe(content + "end")
  })

  test("rejects overlap, reversed ranges and malformed hunks without writing", async () => {
    const f = await fixture()
    const r = await f.read()
    for (const body of ["PUT 1.=2:\n+x\nPUT 2.=2:\n+y", "PUT 3.=1:\n+x", "PUT 1.=1:", "-one\n+ONE", "@@ -1 +1 @@", "PUT 1-2:\n+x", "PUT 1:\n+x", "1:one"]) {
      await expect(f.patch(`[file.txt#${r.tag}]\n${body}`)).rejects.toThrow()
    }
    await expect(f.patch(`[file.txt#${r.tag}]\n-one`)).rejects.toThrow("- rows are not accepted")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT 1-2:\n+x`)).rejects.toThrow("ranges are written N.=M")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("stale tag: changed anchored lines are rejected; unrelated drift is rebased with a warning", async () => {
    const f = await fixture()
    const r = await f.read()
    await writeFile(f.filePath, "one\nCHANGED\nthree\n")
    await expect(f.edit(r.tag, [{ start: 1, end: 3, content: "x", operation: "replace" }])).rejects.toThrow(`Tag #${r.tag} is stale; the current file is [file.txt#${fileTag("one\nCHANGED\nthree\n")}]`)
    await expect(f.edit(r.tag, [{ start: 1, end: 3, content: "x", operation: "replace" }])).rejects.toThrow("*2:CHANGED")
    expect(await readFile(f.filePath, "utf8")).toBe("one\nCHANGED\nthree\n")
    const g = await fixture()
    const s = await g.read()
    await writeFile(g.filePath, "one\ntwo\nthree\nfour\n")
    const appended = await g.edit(s.tag, [{ start: 1, content: "ONE", operation: "replace" }])
    expect(await readFile(g.filePath, "utf8")).toBe("ONE\ntwo\nthree\nfour\n")
    expect(appended.text).toContain("Recovered from a stale tag using the tagged read")
    const h = await fixture()
    const t = await h.read()
    await writeFile(h.filePath, "zero\none\ntwo\nthree\n")
    const shifted = await h.edit(t.tag, [{ start: 2, content: "TWO", operation: "replace" }])
    expect(await readFile(h.filePath, "utf8")).toBe("zero\none\nTWO\nthree\n")
    expect(shifted.text).toContain("Recovered by remapping stale line numbers")
    expect(shifted.text).toStartWith(`[file.txt#${fileTag("zero\none\nTWO\nthree\n")}]\n1:zero\n2:one\n3:TWO\n4:three`)
    const d = await fixture()
    const u = await d.read()
    await writeFile(d.filePath, "one\ntwo\nthree\none\n")
    await expect(d.edit(u.tag, [{ start: 1, end: 3, content: "x", operation: "replace" }])).rejects.toThrow("could not be rebased safely")
  })

  test("unknown tags, tags of other files and missing lines are rejected", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "other.txt"), "other\n")
    const r = await f.read()
    const other = await f.read("", { file: path.join(f.directory, "other.txt") })
    const wrong = r.tag === "0000" ? "0001" : "0000"
    await expect(f.edit(wrong, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow(`tag #${wrong} was not issued for this file`)
    await expect(f.edit(other.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow(`Tag #${other.tag} was issued for ${await realpathOf(f.directory, "other.txt")}`)
    await expect(f.edit(r.tag, [{ start: 4, content: "x", operation: "replace" }])).rejects.toThrow("Line 4 does not exist in file.txt (file has 3 lines)")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("aborted calls stop before writing", async () => {
    const f = await fixture()
    const r = await f.read()
    f.controller.abort(new Error("cancelled"))
    await expect(f.edit(r.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow("cancelled")
    await expect(f.read()).rejects.toThrow("cancelled")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("symlinked files are edited through the link, which is retained", async () => {
    const f = await fixture()
    const alias = path.join(f.directory, "alias")
    await symlink(f.filePath, alias)
    const r = await f.read("", { file: alias })
    expect(r.text).toStartWith("[file.txt#")
    await f.edit(r.tag, [{ start: 1, content: "edited", operation: "replace" }], "alias")
    expect(await readFile(f.filePath, "utf8")).toBe("edited\ntwo\nthree\n")
    expect(await readlink(alias)).toBe(f.filePath)
  })

  test("concurrent edits serialize through canonical symlink aliases", async () => {
    const f = await fixture()
    const alias = f.filePath + ".link"
    await symlink(f.filePath, alias)
    const r = await f.read()
    const results = await Promise.allSettled([
      f.edit(r.tag, [{ start: 1, content: "winner", operation: "replace" }]),
      f.edit(r.tag, [{ start: 1, end: 3, content: "loser", operation: "replace" }], "file.txt.link"),
    ])
    expect(results.filter((result) => result.status === "fulfilled")).toHaveLength(1)
    expect(await readFile(alias, "utf8")).toBe("winner\ntwo\nthree\n")
  })

  test("hardlinked targets are rejected", async () => {
    const f = await fixture()
    const r = await f.read()
    await link(f.filePath, f.filePath + ".link")
    await expect(f.edit(r.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow("Hardlinked")
    expect(await readFile(f.filePath + ".link", "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("edits publish a new complete inode without changing an already-open original", async () => {
    const f = await fixture("﻿one\r\ntwo")
    const r = await f.read()
    await chmod(f.filePath, 0o640)
    const original = await open(f.filePath, "r")
    try {
      await f.edit(r.tag, [{ start: 1, content: "new".repeat(100_000), operation: "replace" }])
      expect(await original.readFile("utf8")).toBe("﻿one\r\ntwo")
      expect((await original.stat()).ino).not.toBe((await stat(f.filePath)).ino)
      expect(await readFile(f.filePath, "utf8")).toBe("﻿" + "new".repeat(100_000) + "\r\ntwo")
      expect((await stat(f.filePath)).mode & 0o777).toBe(0o640)
      expect(await readdir(f.directory)).toEqual(["file.txt"])
    } finally { await original.close() }
  })

  test("edit responses return a new tag and numbered rows that follow-up edits can use without a re-read", async () => {
    const f = await fixture(Array.from({ length: 20 }, (_, index) => `line ${index + 1}`).join("\n") + "\n")
    const r = await f.read()
    const first = await f.edit(r.tag, [{ start: 10, content: "inserted a\ninserted b", operation: "after" }])
    expect(first.text).toBe(`[file.txt#${first.tag}]\n9:line 9\n10:line 10\n11:inserted a\n12:inserted b\n13:line 11\n14:line 12`)
    expect(first.tag).toBe(fileTag(await readFile(f.filePath, "utf8")))
    const second = await f.edit(first.tag, [{ start: 2, content: "LINE 2", operation: "replace" }, { start: 12, content: "INSERTED B", operation: "replace" }])
    expect(second.text).toContain("2:LINE 2\n3:line 3\n4:line 4\n...\n10:line 10\n11:inserted a\n12:INSERTED B\n13:line 11")
    const long = await f.edit(second.tag, [{ start: 1, content: Array.from({ length: 10 }, (_, index) => `new ${index}`).join("\n"), operation: "before" }])
    expect(long.text).toBe(`[file.txt#${long.tag}]\n1:new 0\n2:new 1\n...\n9:new 8\n10:new 9\n11:line 1\n12:LINE 2`)
  })

  test("lines outside the tagged output are rejected with their content, then a straight retry succeeds", async () => {
    const f = await fixture()
    const r = await f.read(":1-1")
    const attempt = () => f.edit(r.tag, [{ start: 2, end: 3, content: "x", operation: "replace" }])
    await expect(attempt()).rejects.toThrow("Actual file content at those lines:\n  2:two\n  3:three")
    await attempt()
    expect(await readFile(f.filePath, "utf8")).toBe("one\nx\n")
    const g = await fixture(Array.from({ length: 50 }, (_, index) => `row ${index}`).join("\n"))
    const s = await g.read(":1-1")
    const wide = () => g.edit(s.tag, [{ start: 2, end: 50, content: "x", operation: "replace" }])
    await expect(wide()).rejects.toThrow("Preview of the first 40 unseen line(s)")
    await expect(wide()).rejects.toThrow("Read the remaining lines in full")
  })

  test("shifted lines below an edit are accepted only when the payload carries their current content", async () => {
    const source = Array.from({ length: 30 }, (_, index) => `const value${index} = compute(${index})`).join("\n") + "\n"
    const f = await fixture(source)
    const first = await f.edit((await f.read()).tag, [{ start: 1, content: "// header", operation: "before" }])
    await expect(f.edit(first.tag, [{ start: 21, content: "unrelated", operation: "replace" }])).rejects.toThrow("never displayed in full")
    const g = await fixture(source)
    const shifted = await g.edit((await g.read()).tag, [{ start: 1, content: "// header", operation: "before" }])
    await g.edit(shifted.tag, [{ start: 21, content: "const value19 = compute(19) // checked", operation: "replace" }])
    expect((await readFile(g.filePath, "utf8")).split("\n")[20]).toBe("const value19 = compute(19) // checked")
    const h = await fixture(source)
    const moved = await h.edit((await h.read()).tag, [{ start: 1, content: "// header", operation: "before" }])
    await expect(h.edit(moved.tag, [{ start: 21, content: "const value1 = compute(1)", operation: "replace" }])).rejects.toThrow("21:const value19 = compute(19)")
  })

  test("a colliding tag for different content is treated as stale, not applied blindly", async () => {
    const f = await fixture("alpha\nbeta\ngamma\n")
    const r = await f.read()
    let collision = ""
    for (let index = 0; !collision; index++) {
      const candidate = `alpha\nchanged ${index}\ngamma\n`
      if (fileTag(candidate) === r.tag) collision = candidate
    }
    await writeFile(f.filePath, collision)
    await expect(f.edit(r.tag, [{ start: 2, content: "BETA", operation: "replace" }])).rejects.toThrow("is stale")
    expect(await readFile(f.filePath, "utf8")).toBe(collision)
  })

  test("no-op edits do not rewrite the file", async () => {
    const f = await fixture()
    const r = await f.read()
    const inode = (await stat(f.filePath)).ino
    const result = await f.edit(r.tag, [{ start: 2, content: "two", operation: "replace" }])
    expect(result.text).toBe(`[file.txt#${r.tag}]\nNo changes: the edit reproduces the current content; this tag is still current.`)
    expect(result.tag).toBe(r.tag)
    expect((await stat(f.filePath)).ino).toBe(inode)
  })

  test("seen-line restrictions are per session; a session without snapshots is unrestricted", async () => {
    const f = await fixture()
    const r = await f.read(":1-1")
    await f.engine.patch({ ...f.ctx, session: "other" }, `[file.txt#${r.tag}]\nPUT 3.=3:\n+THREE`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nTHREE\n")
  })
})

async function realpathOf(directory: string, name: string) {
  const { realpath } = await import("node:fs/promises")
  return realpath(path.join(directory, name))
}

describe("multi-file patches", () => {
  test("edits several files, creates, moves and deletes in one call", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "b.txt"), "bee\n")
    await writeFile(path.join(f.directory, "gone.txt"), "bye\n")
    const read = await f.engine.read(f.ctx, { paths: ["file.txt", "b.txt", "gone.txt"] })
    const tags = [...read.text.matchAll(/#([0-9A-F]{4})\]/g)].map((match) => match[1])
    const result = await f.patch([
      "*** Begin Patch",
      `[file.txt#${tags[0]}]`, "PUT 2.=2:", "+TWO",
      `[b.txt#${tags[1]}]`, "PUT >1:", "+cee", "MV nested/dir/c.txt",
      `[gone.txt#${tags[2]}]`, "REM",
      "*** Add File: new/created.txt", "+hello", "+",
      "*** End Patch",
    ].join("\n"))
    expect(await readFile(f.filePath, "utf8")).toBe("one\nTWO\nthree\n")
    expect(await readFile(path.join(f.directory, "nested/dir/c.txt"), "utf8")).toBe("bee\ncee\n")
    expect(await readFile(path.join(f.directory, "new/created.txt"), "utf8")).toBe("hello\n\n")
    await expect(stat(path.join(f.directory, "b.txt"))).rejects.toThrow()
    await expect(stat(path.join(f.directory, "gone.txt"))).rejects.toThrow()
    expect(result.text).toContain(`Moved b.txt to [nested/dir/c.txt#${fileTag("bee\ncee\n")}]\n1:bee\n2:cee`)
    expect(result.text).toContain("Deleted gone.txt")
    expect(result.text).toContain(`Created [new/created.txt#${fileTag("hello\n\n")}] (2 lines)`)
    const followUp = await f.patch(`[nested/dir/c.txt#${fileTag("bee\ncee\n")}]\nPUT 2.=2:\n+CEE`)
    expect(followUp.text).toContain("2:CEE")
  })

  test("one invalid section stops every write", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "b.txt"), "bee\n")
    const read = await f.engine.read(f.ctx, { paths: ["file.txt", "b.txt"] })
    const [first] = [...read.text.matchAll(/#([0-9A-F]{4})\]/g)].map((match) => match[1])
    await expect(f.patch(`[file.txt#${first}]\nPUT 1.=1:\n+ONE\n[b.txt#0000]\nPUT 1.=1:\n+BEE`)).rejects.toThrow("tag #0000 was not issued")
    await expect(f.patch(`[file.txt#${first}]\nPUT 1.=1:\n+ONE\n*** Add File: b.txt\n+clobber`)).rejects.toThrow("already exists")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
    expect(await readFile(path.join(f.directory, "b.txt"), "utf8")).toBe("bee\n")
  })

  test("repeated sections for one file merge; conflicting tags and duplicate aliases are rejected", async () => {
    const f = await fixture()
    const r = await f.read()
    await f.patch(`[file.txt#${r.tag}]\nPUT 3.=3:\n+THREE\n[file.txt#${r.tag}]\nPUT 1.=1:\n+ONE`)
    expect(await readFile(f.filePath, "utf8")).toBe("ONE\ntwo\nTHREE\n")
    await expect(f.patch("[file.txt#AAAA]\nCUT 1.=1\n[file.txt#BBBB]\nCUT 2.=2")).rejects.toThrow("already has a section with tag AAAA")
    await expect(f.patch(`[file.txt#AAAA]\nCUT 1.=1\n[./file.txt#AAAA]\nCUT 2.=2`)).rejects.toThrow("merge them into one section")
  })

  test("REM needs a current tag; MV refuses an existing destination", async () => {
    const f = await fixture()
    const r = await f.read()
    await writeFile(path.join(f.directory, "taken.txt"), "taken\n")
    await expect(f.patch(`[file.txt#${r.tag}]\nMV taken.txt`)).rejects.toThrow("already exists")
    await writeFile(f.filePath, "changed\n")
    await expect(f.patch(`[file.txt#${r.tag}]\nREM`)).rejects.toThrow("is stale")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT 1.=1:\n+x\nREM`)).rejects.toThrow("REM")
    expect(await readFile(f.filePath, "utf8")).toBe("changed\n")
    expect(await readFile(path.join(f.directory, "taken.txt"), "utf8")).toBe("taken\n")
  })

  test("a plain MV keeps content and mode", async () => {
    const f = await fixture()
    await chmod(f.filePath, 0o750)
    const r = await f.read()
    expect((await f.patch(`[file.txt#${r.tag}]\nMV renamed.txt`)).text).toBe(`Moved file.txt to [renamed.txt#${r.tag}]`)
    expect(await readFile(path.join(f.directory, "renamed.txt"), "utf8")).toBe("one\ntwo\nthree\n")
    expect((await stat(path.join(f.directory, "renamed.txt"))).mode & 0o777).toBe(0o750)
    await expect(lstat(f.filePath)).rejects.toThrow()
  })
})

describe("write", () => {
  test("creates with parents, overwrites only with a current tag, and returns an editable tag", async () => {
    const f = await fixture()
    const created = await f.engine.write(f.ctx, { path: "deep/new.txt", content: "a\nb\n" })
    expect(created.text).toBe(`Created [deep/new.txt#${fileTag("a\nb\n")}] (2 lines)`)
    await f.patch(`[deep/new.txt#${fileTag("a\nb\n")}]\nPUT 2.=2:\n+B`)
    expect(await readFile(path.join(f.directory, "deep/new.txt"), "utf8")).toBe("a\nB\n")
    await expect(f.engine.write(f.ctx, { path: "file.txt", content: "x" })).rejects.toThrow("pass the tag")
    await expect(f.engine.write(f.ctx, { path: "absent.txt", content: "x", tag: "ABCD" })).rejects.toThrow("omit tag")
    const r = await f.read()
    await writeFile(f.filePath, "external\n")
    await expect(f.engine.write(f.ctx, { path: "file.txt", content: "x", tag: r.tag })).rejects.toThrow("changed since tag")
    const current = await f.read()
    expect((await f.engine.write(f.ctx, { path: "file.txt", content: "replaced\n", tag: current.tag })).text).toStartWith("Overwrote [file.txt#")
    expect(await readFile(f.filePath, "utf8")).toBe("replaced\n")
  })

  test("never follows or clobbers symlinks when creating; cleanup leaves unrelated temp files", async () => {
    const f = await fixture()
    const dangling = path.join(f.directory, "dangling")
    await symlink(path.join(f.directory, "missing"), dangling)
    await expect(f.engine.write(f.ctx, { path: "dangling", content: "x" })).rejects.toThrow()
    await expect(stat(path.join(f.directory, "missing"))).rejects.toThrow()
    const unrelated = path.join(f.directory, ".nib-someone-else.tmp")
    await writeFile(unrelated, "not ours")
    const content = "complete\r\n".repeat(100_000)
    await f.engine.write(f.ctx, { path: "big.txt", content })
    expect(await readFile(path.join(f.directory, "big.txt"), "utf8")).toBe(content)
    expect((await stat(path.join(f.directory, "big.txt"))).mode & 0o777).toBe(0o666 & ~process.umask())
    expect((await readdir(f.directory)).sort()).toEqual([".nib-someone-else.tmp", "big.txt", "dangling", "file.txt"])
  })

  test.skipIf(process.platform !== "linux")("partial staged writes under RLIMIT_FSIZE fail without touching the target", async () => {
    for (const operation of ["edit", "write"]) {
      const f = await fixture()
      const script = `
        import { Engine } from ${JSON.stringify(new URL("../src/engine.ts", import.meta.url).href)};
        process.on("SIGXFSZ", () => {});
        const engine = new Engine();
        const ctx = { directory: ${JSON.stringify(f.directory)}, session: "test", signal: new AbortController().signal };
        const read = await engine.read(ctx, { paths: ["file.txt"] });
        const tag = /#([0-9A-F]{4})\\]/.exec(read.text)[1];
        const content = "x".repeat(200_000);
        try {
          ${operation === "edit"
            ? "await engine.patch(ctx, `[file.txt#${tag}]\\nPUT 1.=1:\\n+${content}`);"
            : 'await engine.write(ctx, { path: "file.txt.new", content });'}
          throw new Error("Expected EFBIG from the staged write");
        } catch (error) {
          if (error.code !== "EFBIG") throw error;
          console.log("staged write rejected with EFBIG");
        }
      `
      const child = Bun.spawn(["bash", "-c", 'ulimit -f 1; exec "$@"', "nib-test", process.execPath, "--eval", script], { stdout: "pipe", stderr: "pipe" })
      const [exit, stdout, stderr] = await Promise.all([child.exited, new Response(child.stdout).text(), new Response(child.stderr).text()])
      expect({ exit, stderr }).toEqual({ exit: 0, stderr: "" })
      expect(stdout).toContain("EFBIG")
      expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
      expect(await readdir(f.directory)).toEqual(["file.txt"])
    }
  })
})

describe("workspace roots", () => {
  test("writes outside the roots are refused without authorization and allowed with it", async () => {
    const f = await fixture()
    const outside = await workspace()
    await writeFile(path.join(outside, "x.txt"), "x\n")
    const target = path.join(outside, "x.txt")
    const r = await f.read("", { file: target })
    expect(r.text).toStartWith(`[${await realpathOf(outside, "x.txt")}#`)
    await expect(f.patch(`[${target}#${r.tag}]\nPUT 1.=1:\n+y`)).rejects.toThrow("outside the workspace roots")
    await expect(f.engine.write(f.ctx, { path: path.join(outside, "new/deep.txt"), content: "z" })).rejects.toThrow("outside the workspace roots")
    await expect(stat(path.join(outside, "new"))).rejects.toThrow()
    const asked: string[] = []
    const allowed: Context = { ...f.ctx, authorize: async (canonical, action) => { asked.push(`${action} ${canonical}`) } }
    await f.engine.patch(allowed, `[${target}#${r.tag}]\nPUT 1.=1:\n+y`)
    expect(await readFile(target, "utf8")).toBe("y\n")
    expect(asked).toEqual([`edit ${target}`, ...target === await realpathOf(outside, "x.txt") ? [] : [`edit ${await realpathOf(outside, "x.txt")}`]])
    const rooted: Context = { ...f.ctx, roots: async () => [f.directory, outside] }
    await f.engine.write(rooted, { path: path.join(outside, "free.txt"), content: "ok" })
    expect(await readFile(path.join(outside, "free.txt"), "utf8")).toBe("ok")
  })

  test("a symlink inside the workspace cannot redirect writes outside it", async () => {
    const f = await fixture()
    const outside = await workspace()
    await symlink(outside, path.join(f.directory, "escape"))
    await expect(f.engine.write(f.ctx, { path: "escape/new.txt", content: "x" })).rejects.toThrow("outside the workspace roots")
    expect(await readdir(outside)).toEqual([])
  })
})

describe("grep", () => {
  test("anchored match and context rows, editable without a read", async () => {
    const f = await fixture(Array.from({ length: 12 }, (_, index) => `line ${index + 1}`).join("\n") + "\n")
    await writeFile(path.join(f.directory, "b.txt"), "nothing\nline needle\n")
    const result = await f.engine.grep(f.ctx, { pattern: "line (6|needle)" })
    const tag = fileTag(await readFile(f.filePath, "utf8"))
    expect(result.text).toBe([
      "2 matches in 2 files",
      `[b.txt#${fileTag("nothing\nline needle\n")}]\n 1:nothing\n*2:line needle`,
      `[file.txt#${tag}]\n 5:line 5\n*6:line 6\n 7:line 7\n 8:line 8\n 9:line 9`,
    ].join("\n\n"))
    await f.patch(`[file.txt#${tag}]\nPUT 5.=6:\n+five\n+six\nPUT >9:\n+after nine`)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(4, 10)).toEqual(["five", "six", "line 7", "line 8", "line 9", "after nine"])
    await expect(f.patch(`[file.txt#${fileTag(await readFile(f.filePath, "utf8"))}]\nPUT 1.=1:\n+x`)).rejects.toThrow("never displayed")
  })

  test("paths, globs, literal and case options; .gitignore is respected", async () => {
    const f = await fixture("Alpha\n")
    await mkdir(path.join(f.directory, "src"))
    await writeFile(path.join(f.directory, "src/a.ts"), "alpha(\n")
    await writeFile(path.join(f.directory, "ignored.txt"), "alpha\n")
    await writeFile(path.join(f.directory, ".gitignore"), "ignored.txt\n")
    await mkdir(path.join(f.directory, ".git"))
    expect((await f.engine.grep(f.ctx, { pattern: "alpha" })).text).toStartWith("1 matches in 1 files\n\n[src/a.ts#")
    expect((await f.engine.grep(f.ctx, { pattern: "alpha", ignoreCase: true, glob: "file.*" })).text).toStartWith("1 matches in 1 files\n\n[file.txt#")
    expect((await f.engine.grep(f.ctx, { pattern: "alpha(", literal: true, paths: ["src"] })).text).toContain("*1:alpha(")
    expect((await f.engine.grep(f.ctx, { pattern: "zzz" })).text).toBe("No matches found")
    await expect(f.engine.grep(f.ctx, { pattern: "(" })).rejects.toThrow("rg failed")
  })

  test("pages files and caps matches per file", async () => {
    const f = await fixture(Array.from({ length: 30 }, () => "hit").join("\n"))
    for (let index = 0; index < 25; index++) await writeFile(path.join(f.directory, `f${String(index).padStart(2, "0")}.txt`), "hit\n")
    const first = await f.engine.grep(f.ctx, { pattern: "hit", before: 0, after: 0 })
    expect(first.text).toStartWith("20 matches in 20 files (of 26)")
    expect(first.text).toEndWith("[26 matching files; continue with skip 20]")
    const second = await f.engine.grep(f.ctx, { pattern: "hit", before: 0, after: 0, skip: 20 })
    expect(second.text).toStartWith("35 matches in 6 files (of 26)")
    expect(second.text).toContain("[file.txt#")
    expect(second.text).toContain("[10 more matches in this file]")
    expect((await f.engine.grep(f.ctx, { pattern: "hit", skip: 99 })).text).toStartWith("No more results")
  })

  test("long lines are truncated and not anchorable", async () => {
    const f = await fixture("x".repeat(600) + "needle\nneedle\n")
    const result = await f.engine.grep(f.ctx, { pattern: "needle" })
    expect(result.text).toContain("*[1 truncated at 512 of 606 chars")
    expect(result.text).toContain("*2:needle")
  })
})

describe("snapshots", () => {
  test("tags match oh-my-pi file_hash vectors and ignore trailing whitespace and CRs", () => {
    expect([fileTag("a \n b\t\r\nc"), fileTag("hello\n"), fileTag("")]).toEqual(["80BA", "5BF9", "5D05"])
    expect(fileTag("x  \r\ny\t")).toBe(fileTag("x\ny"))
    expect(fileTag("x\ny")).not.toBe(fileTag("x\ny\n"))
  })

  test("store deduplicates versions, unions seen lines and evicts least recently used paths", () => {
    const store = new SnapshotStore({ paths: 2, versions: 2, units: Number.MAX_SAFE_INTEGER })
    const version = (key: string) => ({ key, lines: key.split("\n"), tag: fileTag(key) })
    store.record("s", "a", version("one"), [1])
    store.record("s", "a", version("two"), [2])
    store.record("s", "a", version("one"), [3])
    expect(store.versions("s", "a").map((item) => [item.key, [...item.seen!]])).toEqual([["one", [1, 3]], ["two", [2]]])
    store.record("s", "a", version("three"))
    expect(store.versions("s", "a").map((item) => item.key)).toEqual(["three", "one"])
    store.record("s", "b", version("b"))
    store.versions("s", "a")
    store.record("other", "a", version("c"))
    expect(store.versions("s", "b")).toEqual([])
    expect(store.versions("s", "a")).toHaveLength(2)
    expect(store.origins("s", fileTag("three"), "b")).toEqual(["a"])
  })
})

describe("block edits", () => {
  const java = [
    "class A {",
    "    int a() {",
    "        return 1;",
    "    }",
    "",
    "    /**",
    "     * Doc.",
    "     */",
    "    @Override",
    "    public int b(int x,",
    "                 int y) {",
    "        if (x > y) {",
    "            return x;",
    "        } else {",
    "            return y;",
    "        }",
    "    }",
    "",
    "    int c() {",
    "        return 3;",
    "    }",
    "}",
    "",
  ].join("\n")

  test("a block replace, cut or insert resolves the block's end, closing line included", async () => {
    const f = await fixture(java)
    const { tag } = await f.read()
    const result = await f.patch(`[file.txt#${tag}]\nPUT >2*:\n+\n+    int a2() { return 2; }\nPUT 6*:\n+    int b() { return 0; }\nCUT 19*`)
    expect(result.text).not.toContain("rejected")
    expect(await readFile(f.filePath, "utf8")).toBe([
      "class A {",
      "    int a() {",
      "        return 1;",
      "    }",
      "",
      "    int a2() { return 2; }",
      "",
      "    int b() { return 0; }",
      "",
      "}",
      "",
    ].join("\n"))
  })

  test("a statement block keeps its else branch; a python block ends before the next sibling", async () => {
    const f = await fixture(java)
    const { tag } = await f.read()
    await f.patch(`[file.txt#${tag}]\nPUT 12*:\n+        return Math.max(x, y);`)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(9, 14)).toEqual([
      "    public int b(int x,",
      "                 int y) {",
      "        return Math.max(x, y);",
      "    }",
      "",
    ])
    const py = await fixture("@cache\ndef f(a):\n    if a:\n        return 1\n    return 2\n\n# helpers\ndef g():\n    pass\n")
    const read = await py.read()
    await py.patch(`[file.txt#${read.tag}]\nPUT 1*:\n+def f(a):\n+    return 3`)
    expect(await readFile(py.filePath, "utf8")).toBe("def f(a):\n    return 3\n\n# helpers\ndef g():\n    pass\n")
  })

  test("lines that start no multi-line block are refused, and a block overlapping another hunk is rejected", async () => {
    const f = await fixture(java)
    const { tag } = await f.read()
    await expect(f.patch(`[file.txt#${tag}]\nPUT 3*:\n+x`)).rejects.toThrow("Line 3 does not start a multi-line block")
    await expect(f.patch(`[file.txt#${tag}]\nCUT 4*`)).rejects.toThrow("Line 4 does not start a multi-line block")
    await expect(f.patch(`[file.txt#${tag}]\nPUT 5*:\n+x`)).rejects.toThrow("Line 5 does not start a multi-line block")
    await expect(f.patch(`[file.txt#${tag}]\nPUT 2*:\n+x\nPUT 3.=3:\n+y`)).rejects.toThrow("overlap")
    expect(await readFile(f.filePath, "utf8")).toBe(java)
  })

  test("a block must have been seen in full, and a stale tag rebases the resolved block", async () => {
    const f = await fixture(java)
    const { tag } = await f.read(":19-19")
    const unseen = await f.patch(`[file.txt#${tag}]\nCUT 19*`).catch((error: Error) => error.message)
    expect(unseen).toContain("20:        return 3;")
    await f.patch(`[file.txt#${tag}]\nCUT 19*`)
    expect(await readFile(f.filePath, "utf8")).not.toContain("int c()")

    const g = await fixture(java)
    const stale = await g.read()
    await writeFile(g.filePath, "// header\n" + java)
    const result = await g.patch(`[file.txt#${stale.tag}]\nCUT 19*`)
    expect(result.text).toContain("Recovered by remapping stale line numbers")
    expect(await readFile(g.filePath, "utf8")).toBe("// header\n" + java.split("\n").filter((_, index) => index < 18 || index > 20).join("\n"))
  })
})
