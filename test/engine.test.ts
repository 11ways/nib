import { afterEach, describe, expect, test } from "bun:test"
import { chmod, link, lstat, mkdir, mkdtemp, open, readdir, readFile, readlink, rm, stat, symlink, utimes, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { binary, Engine, fileTag, type Context } from "./support/engine"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })
const BOM = String.fromCharCode(0xfeff)

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

// movedLesson ends an agent's first edit response whose rows say lines it knew moved.
const movedLesson = "[a line you saw or wrote that an edit moved is anchored by a ~TEXT row with its start under the PUT or CUT; no read needed]"

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
  test("a declared name reads its lines, doc comment and overloads, as anchors an edit can use", async () => {
    const f = await fixture()
    const file = path.join(path.dirname(f.filePath), "repo.ts")
    await writeFile(file, ["class Repo {", "  // Saves one.", "  save(r) {", "    this.store.save(r)", "  }", "", "  load() {", "    return 1", "  }", "}", ""].join("\n"))
    const r = await f.read(":Repo.save", { file })
    expect(r.text).toBe(`[repo.ts#${r.tag}]\n2:  // Saves one.\n3:  save(r) {\n4:    this.store.save(r)\n5:  }\n[10 lines total]`)
    await f.patch(`[repo.ts#${r.tag}]\nPUT 3*:\n+  save(r) {\n+    this.store.put(r)\n+  }`)
    expect(await readFile(file, "utf8")).toContain("  // Saves one.\n  save(r) {\n    this.store.put(r)\n  }\n")
    expect((await f.read(":save,load", { file })).text).toContain("\n7:  load() {\n8:    return 1\n9:  }")
    expect((await f.read(":fetch", { file })).text).toBe("[repo.ts: nib found no declaration of fetch; grep fetch instead]")
    // A file nib has no grammar for is refused rather than guessed at.
    expect((await f.read(":save")).text).toBe("[file.txt: nib reads declarations by parsing and has no grammar for .txt files; grep save instead]")
  })

  test("tagged header, relative display path, selectors and continuation hints", async () => {
    const f = await fixture("one\r\ntwo\r\nthree")
    const tag = fileTag("one\r\ntwo\r\nthree")
    // The file has no final newline, so every read that shows its last line says so.
    const ending = "[no final newline; PUT >$: with a lone + row adds one]"
    expect((await f.read()).text).toBe(`[file.txt#${tag}]\n1:one\n2:two\n3:three\n${ending}`)
    expect((await f.read(":2-2")).text).toBe(`[file.txt#${tag}]\n2:two\n[3 lines total]`)
    expect((await f.read(":2")).text).toBe(`[file.txt#${tag}]\n2:two\n[3 lines total]`)
    expect((await f.read(":2-")).text).toBe(`[file.txt#${tag}]\n2:two\n3:three\n${ending}\n[3 lines total]`)
    expect((await f.read(":1+2")).text).toBe(`[file.txt#${tag}]\n1:one\n2:two\n[3 lines total]`)
    expect((await f.read(":-1")).text).toBe(`[file.txt#${tag}]\n3:three\n${ending}\n[3 lines total]`)
    expect((await f.read(":1,3")).text).toBe(`[file.txt#${tag}]\n1:one\n...\n3:three\n${ending}\n[3 lines total]`)
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
    expect((await f.read(":2001-")).anchors).toHaveLength(500)
    // A bare line number reads that line only, however long the file: models write path:N the way grep prints a hit.
    const single = await f.read(":2001")
    expect(single.anchors).toEqual([2001])
    expect(single.text).toEndWith("[2500 lines total]")
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
    expect((await f.read()).text).toBe("[file.txt] skipped: 5.0 MiB, over the 2 MiB text limit. Read it with plain: true (a range like file.txt:-200 works too), or grep it.")
  })

  test("cut long lines are not editable until read whole; a larger maxLineLength shares the output budget", async () => {
    // Prose-like long lines: whitespace keeps them from counting as minified, so the default cut at 2000 applies.
    const long = "word ".repeat(400) + "w"
    const f = await fixture(long + "\nshort\n")
    const r = await f.read()
    expect(r.text).toContain("\n[1 cut at 2000 of 2001 chars]|word word")
    expect(r.text).toEndWith("\n[cut rows: change them in place with SUB N: OLD, or read file.txt:N with maxLineLength to see one whole]")
    expect(r.anchors).toEqual([2])
    await expect(f.edit(r.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow("Preview of the first 1 unseen line(s)")
    expect((await f.read("", { maxLineLength: 40_000 })).anchors).toEqual([1, 2])
    expect((await f.read(":1-2", { maxLineLength: 40_000 })).anchors).toEqual([1, 2])
    const full = await f.read(":1", { maxLineLength: 40_000 })
    expect(full.anchors).toEqual([1])
    await f.edit(full.tag, [{ start: 1, content: "x", operation: "replace" }])
    expect(await readFile(f.filePath, "utf8")).toBe("x\nshort\n")
    await writeFile(f.filePath, ("word ".repeat(380) + "\n").repeat(100))
    const page = await f.read(":1-")
    expect(page.text).toMatch(/continue with file\.txt:\d+-\]$/)
    expect(Buffer.byteLength(page.text)).toBeLessThan(41 * 1024)
    await writeFile(f.filePath, "x".repeat(2 * 1024 * 1024 + 1))
    expect((await f.read()).text).toContain("[file.txt] skipped: 2.0 MiB, over the 2 MiB text limit.")
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

  test("outlines fold runs of one-line declarations, and quote marks in one-line strings, regex literals and comments hide nothing", async () => {
    const f = await fixture()
    const ts = [
      'import { a } from "a"',
      ...Array.from({ length: 40 }, (_, index) => `const LIMIT_${index} = ${index}`),
      "const QUOTES = /\"\"\"|'''|`/g",
      'const TICK = "`"',
      "// a lone ` in a comment",
      "export function first() {",
      ...Array.from({ length: 600 }, (_, index) => `  const value${index} = ${index}`),
      "}",
      "const TEMPLATE = `",
      "export function hidden() {",
      "`",
      "export class Store {",
      "  size = 1",
      "  limit = 2",
      "  count = 3",
      "  add(item: string) {",
      "    return item",
      "  }",
      "}",
      "export function last() {",
      "  return TICK",
      "}",
    ]
    await writeFile(path.join(f.directory, "big.ts"), ts.join("\n") + "\n")
    const at = (text: string) => ts.indexOf(text) + 1
    expect((await f.read("", { file: path.join(f.directory, "big.ts") })).text.split("\n").slice(1)).toEqual([
      '1:import { a } from "a"',
      "2:const LIMIT_0 = 0",
      "[lines 3-43: 41 more one-line declarations]",
      `${at("export function first() {")}:export function first() {`,
      `${at("const TEMPLATE = `")}:const TEMPLATE = \``,
      `${at("export class Store {")}:export class Store {`,
      `${at("  size = 1")}:  size = 1`,
      `[lines ${at("  limit = 2")}-${at("  count = 3")}: 2 more one-line declarations]`,
      `${at("  add(item: string) {")}:  add(item: string) {`,
      `${at("export function last() {")}:export function last() {`,
    ])
    // The cap counts rows after folding, so a class's fields no longer crowd out its methods.
    const many = [
      "export class Big {",
      ...Array.from({ length: 200 }, (_, index) => `  field${index} = ${index}`),
      ...Array.from({ length: 120 }, (_, index) => `  method${index}() {\n    const a = 1\n    const b = 2\n    const c = 3\n    return ${index}\n  }`),
      "}",
    ]
    await writeFile(path.join(f.directory, "many.ts"), many.join("\n") + "\n")
    const rows = (await f.read("", { file: path.join(f.directory, "many.ts") })).text.split("\n").slice(1)
    expect(rows.slice(0, 4)).toEqual(["1:export class Big {", "2:  field0 = 0", "[lines 3-201: 199 more one-line declarations]", "202:  method0() {"])
    expect(rows).toHaveLength(123)
    expect(rows.at(-1)).toBe("916:  method119() {")
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
    expect(parts[1]).toStartWith("[app.js] skipped: minified (most of it in long, dense lines)")
    expect(parts[2]).toStartWith("[node_modules/lib/index.js] skipped: inside node_modules/")
    expect(parts[3]).toStartWith("[node_modules/lib/small.d.ts#")
    expect((await f.engine.read(f.ctx, { paths: ["package-lock.json:1-"] })).text).toContain("\n2:  \"x\": 1,")
  })

  test("one call shares its budget so small files are not starved by a large one", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "big.txt"), ("word ".repeat(200) + "\n").repeat(100))
    await writeFile(path.join(f.directory, "small.txt"), "tiny\n")
    const r = await f.engine.read(f.ctx, { paths: ["big.txt:1-", "small.txt", "big.txt:50-"] })
    const parts = r.text.split("\n\n")
    expect(parts[1]).toBe(`[small.txt#${fileTag("tiny\n")}]\n1:tiny`)
    expect(parts[0]).toEndWith("-]")
    expect(parts[2]).toMatch(/^\[big\.txt#[0-9A-F]{4}\]\n50:(word )+\n/)
    expect(Buffer.byteLength(r.text)).toBeLessThan(41 * 1024)
  })
})

describe("edit", () => {
  test.each([`${BOM}one\r\ntwo\r\n`, `${BOM}one\r\ntwo`, "one\ntwo\n", "one\ntwo"])("preserves encoding, mode and final newline: %j", async (source) => {
    const f = await fixture(source)
    await chmod(f.filePath, 0o640)
    const original = await f.read()
    await f.edit(original.tag, [{ start: 1, content: "ONE", operation: "replace" }])
    expect(await readFile(f.filePath, "utf8")).toBe(source.replace("one", "ONE"))
    expect((await stat(f.filePath)).mode & 0o777).toBe(0o640)
  })

  test("empty existing file uses a virtual line 1; BOM-only file remains BOM-prefixed", async () => {
    for (const [content, operation] of [["", "replace"], ["", "before"], ["", "after"], [BOM, "replace"]] as const) {
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
    const f = await fixture(`${BOM}one\r\ntwo\r\n`)
    const r = await f.read()
    const result = await f.edit(r.tag, [{ start: 1, end: 2, content: "", operation: "replace" }])
    expect(result.text).toContain("(file is now empty)")
    expect(await readFile(f.filePath, "utf8")).toBe(BOM)
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
    for (const body of ["PUT 1.=2:\n+x\nPUT 2.=2:\n+y", "PUT 3.=1:\n+x", "PUT 1.=1:", "-one\n+ONE", "@@ -1 +1 @@", "PUT 1-2:\n+x", "1:one"]) {
      await expect(f.patch(`[file.txt#${r.tag}]\n${body}`)).rejects.toThrow()
    }
    await expect(f.patch(`[file.txt#${r.tag}]\n-one`)).rejects.toThrow("- rows only go under a PUT N.=M: or CUT N.=M line")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT 1-2:\n+x`)).rejects.toThrow("ranges are written N.=M")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("an insert may sit right next to a replaced range; hunks may not share a line or an insert point", async () => {
    const applied: [string, string][] = [
      ["PUT 2.=2:\n+TWO\nPUT >2:\n+after", "one\nTWO\nafter\nthree\n"],
      ["PUT <2:\n+before\nPUT 2.=2:\n+TWO", "one\nbefore\nTWO\nthree\n"],
      ["CUT 2.=2\nPUT >2:\n+instead", "one\ninstead\nthree\n"],
      ["CUT 2\nPUT >2:\n+instead", "one\ninstead\nthree\n"],
      ["PUT <2:\n+b\nPUT >2:\n+a", "one\nb\ntwo\na\nthree\n"],
      ["SUB 2: tw\n+TW\nPUT >2:\n+x", "one\nTWo\nx\nthree\n"],
      ["PUT >1:\n+x\nPUT <2:\n+y", "one\nx\ny\ntwo\nthree\n"],
    ]
    for (const [body, expected] of applied) {
      const f = await fixture()
      await f.patch(`[file.txt#${(await f.read()).tag}]\n${body}`)
      expect(await readFile(f.filePath, "utf8")).toBe(expected)
    }
    const f = await fixture()
    const r = await f.read()
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT 1.=2:\n+x\nPUT >1:\n+y`)).rejects.toThrow("Edits overlap at line 1: a line can be replaced by one hunk only, and an insert may only touch a replaced range at its edges")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT 1.=2:\n+x\nPUT 2.=3:\n+y`)).rejects.toThrow("Edits overlap at line 2: a line can be replaced by one hunk only")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT >1:\n+x\nPUT >1:\n+y`)).rejects.toThrow("Edits overlap at line 1: two hunks insert at the same place; put their + rows into one hunk")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT >$:\n+x\nPUT >$:\n+y`)).rejects.toThrow("Edits overlap at the end of the file: two hunks insert at the same place")
    await expect(f.patch(`[file.txt#${r.tag}]\nPUT 3.=1:\n+x`)).rejects.toThrow("Range 3.=1 is reversed")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("an insert after a block may go with a change inside that block", async () => {
    const f = await fixture("function a() {\n  return 1\n}\nend\n")
    await f.patch(`[file.txt#${(await f.read()).tag}]\nPUT >1*:\n+// after\nPUT 2.=2:\n+  return 2`)
    expect(await readFile(f.filePath, "utf8")).toBe("function a() {\n  return 2\n}\n// after\nend\n")
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
    expect(shifted.text).toStartWith(`[file.txt#${fileTag("zero\none\nTWO\nthree\n")}]\n2:one\n3:TWO\n4:three`)
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
    // Resolved first: awaiting it inside the matcher's argument would leave the edit's rejection unhandled meanwhile.
    const otherPath = await realpathOf(f.directory, "other.txt")
    await expect(f.edit(wrong, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow(`tag #${wrong} was not issued for this file`)
    await expect(f.edit(other.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow(`Tag #${other.tag} was issued for ${otherPath}`)
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
    // Calls run concurrently, so either may come first; the other must be refused rather than applied over it.
    expect(results.map((result) => result.status).sort()).toEqual(["fulfilled", "rejected"])
    expect(await readFile(alias, "utf8")).toBe(results[0].status === "fulfilled" ? "winner\ntwo\nthree\n" : "loser\n")
  })

  test("hardlinked targets are rejected", async () => {
    const f = await fixture()
    const r = await f.read()
    await link(f.filePath, f.filePath + ".link")
    await expect(f.edit(r.tag, [{ start: 1, content: "x", operation: "replace" }])).rejects.toThrow("Hardlinked")
    expect(await readFile(f.filePath + ".link", "utf8")).toBe("one\ntwo\nthree\n")
  })

  test("edits publish a new complete inode without changing an already-open original", async () => {
    const f = await fixture(`${BOM}one\r\ntwo`)
    const r = await f.read()
    await chmod(f.filePath, 0o640)
    const original = await open(f.filePath, "r")
    try {
      await f.edit(r.tag, [{ start: 1, content: "new".repeat(100_000), operation: "replace" }])
      expect(await original.readFile("utf8")).toBe(`${BOM}one\r\ntwo`)
      expect((await original.stat()).ino).not.toBe((await stat(f.filePath)).ino)
      expect(await readFile(f.filePath, "utf8")).toBe(BOM + "new".repeat(100_000) + "\r\ntwo")
      expect((await stat(f.filePath)).mode & 0o777).toBe(0o640)
      expect(await readdir(f.directory)).toEqual(["file.txt"])
    } finally { await original.close() }
  })

  test("edit responses return a new tag and numbered rows that follow-up edits can use without a re-read", async () => {
    const f = await fixture(Array.from({ length: 20 }, (_, index) => `line ${index + 1}`).join("\n") + "\n")
    const r = await f.read()
    const first = await f.edit(r.tag, [{ start: 10, content: "inserted a\ninserted b", operation: "after" }])
    // The two inserted lines come back whole between the kept rows around them, and the lines they moved down as one note.
    expect(first.text).toBe(`[file.txt#${first.tag}]\n10:line 10\n11:inserted a\n12:inserted b\n13:line 11\n[lines 14-22 are old lines 12-20, moved down by 2]\n${movedLesson}`)
    expect(first.tag).toBe(fileTag(await readFile(f.filePath, "utf8")))
    const second = await f.edit(first.tag, [{ start: 2, content: "LINE 2", operation: "replace" }, { start: 12, content: "INSERTED B", operation: "replace" }])
    expect(second.text).toBe(`[file.txt#${second.tag}]\n1:line 1\n2:LINE 2\n3:line 3\n...\n11:inserted a\n12:INSERTED B\n13:line 11`)
    const long = await f.edit(second.tag, [{ start: 1, content: Array.from({ length: 10 }, (_, index) => `new ${index}`).join("\n"), operation: "before" }])
    expect(long.text).toBe(`[file.txt#${long.tag}]\n1:new 0\n...\n10:new 9\n11:line 1\n[lines 12-32 are old lines 2-22, moved down by 10]`)
  })

  test("lines outside the tagged output are rejected with their content, then a straight retry succeeds", async () => {
    const f = await fixture()
    const r = await f.read(":1-1")
    const attempt = () => f.edit(r.tag, [{ start: 2, end: 3, content: "x", operation: "replace" }])
    await expect(attempt()).rejects.toThrow("Actual file content (* marks those lines):\n 1:one\n*2:two\n*3:three")
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

  test("no-op edits do not rewrite the file, and show the lines whole", async () => {
    const f = await fixture()
    const r = await f.read()
    const inode = (await stat(f.filePath)).ino
    const result = await f.edit(r.tag, [{ start: 2, content: "two", operation: "replace" }])
    expect(result.text).toBe(`[file.txt#${r.tag}]\nNo changes: these lines already read as the edit writes them; this tag is still current.\n1:one\n2:two\n3:three`)
    expect(result.tag).toBe(r.tag)
    expect((await stat(f.filePath)).ino).toBe(inode)
    // The rows it showed are seen, so the line next to the resent one is editable without a read.
    const g = await fixture(numbered(30))
    const s = await g.read(":20-20")
    await g.edit(s.tag, [{ start: 20, content: "line 20", operation: "replace" }])
    await g.patch(`[file.txt#${s.tag}]\nPUT 21.=21:\n+LINE 21`)
    expect((await readFile(g.filePath, "utf8")).split("\n")[20]).toBe("LINE 21")
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
    expect(result.text).toContain(`Created [new/created.txt#${fileTag("hello\n\n")}] (2 lines, 1 non-blank, 1 shown)\n1~hello`)
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

  test("CP writes the section's result to a new file and leaves the source as it was", async () => {
    const f = await fixture()
    await chmod(f.filePath, 0o640)
    const r = await f.read()
    const copied = await f.patch(`[file.txt#${r.tag}]\nPUT 2.=2:\n+TWO\nCP variant/copy.txt`)
    expect(copied.text).toBe(`Copied file.txt to [variant/copy.txt#${fileTag("one\nTWO\nthree\n")}]\n1:one\n2:TWO\n3:three`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\n")
    expect(await readFile(path.join(f.directory, "variant/copy.txt"), "utf8")).toBe("one\nTWO\nthree\n")
    expect((await stat(path.join(f.directory, "variant/copy.txt"))).mode & 0o777).toBe(0o640)
    // The source keeps its tag, and the copy can be edited at once with the numbers the agent already knows.
    await f.patch(`[file.txt#${r.tag}]\nPUT 3.=3:\n+THREE`)
    await f.patch(`[variant/copy.txt#${fileTag("one\nTWO\nthree\n")}]\nPUT 1.=1:\n+ONE`)
    expect(await readFile(path.join(f.directory, "variant/copy.txt"), "utf8")).toBe("ONE\nTWO\nthree\n")
  })

  test("a plain CP copies unchanged; it refuses an existing destination and a stale tag", async () => {
    const f = await fixture(numbered(30))
    const r = await f.read(":1-5")
    const copied = await f.patch(`[file.txt#${r.tag}]\nCP backup.txt`)
    expect(copied.text).toBe(`Copied file.txt to [backup.txt#${r.tag}]`)
    // Only what the agent saw of the source counts as seen in the copy.
    expect(await refusal(f.patch(`[backup.txt#${r.tag}]\nPUT 20.=20:\n+x`))).toContain("never displayed in full")
    await f.patch(`[backup.txt#${r.tag}]\nPUT 2.=2:\n+line two`)
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nCP backup.txt`))).toContain("Destination backup.txt already exists")
    await writeFile(f.filePath, "changed\n")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nCP other.txt`))).toContain("is stale")
    await expect(stat(path.join(f.directory, "other.txt"))).rejects.toThrow()
  })
})

describe("write", () => {
  test("creates with parents, overwrites only with a current tag, and returns an editable tag", async () => {
    const f = await fixture()
    const created = await f.engine.write(f.ctx, { path: "deep/new.txt", content: "a\nb\n" })
    expect(created.text).toBe(`Created [deep/new.txt#${fileTag("a\nb\n")}] (2 lines, 2 non-blank, 2 shown)\n1~a\n2~b`)
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
      // The binary runs under a 1 KiB file size limit, so its staged write fails with EFBIG partway through.
      const client = new Client({ name: "engine-test", version: "1" })
      await client.connect(new StdioClientTransport({
        command: "bash",
        args: ["-c", 'ulimit -f 1; exec "$0"', await binary()],
        cwd: f.directory,
        env: { PATH: process.env.PATH ?? "", NIB_DIRECTORY: f.directory, NIB_SHELL: "0" },
        stderr: "pipe",
      }))
      try {
        const call = async (name: string, args: Record<string, unknown>) => {
          const result = await client.callTool({ name, arguments: args }) as { isError?: boolean; content: { text: string }[] }
          return { failed: !!result.isError, text: result.content[0]!.text }
        }
        const tag = /#([0-9A-F]{4})\]/.exec((await call("read", { paths: ["file.txt"] })).text)![1]
        const content = "x".repeat(200_000)
        const result = operation === "edit"
          ? await call("edit", { input: `[file.txt#${tag}]\nPUT 1.=1:\n+${content}` })
          : await call("write", { path: "file.txt.new", content })
        expect(result.failed).toBe(true)
        expect(result.text).toContain("file too large")
      } finally { await client.close() }
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

describe("sandbox", () => {
  /** A workspace and a directory outside it, reachable through a file link and a directory link inside the workspace. */
  async function sandboxed() {
    const f = await fixture()
    const outside = await workspace()
    await writeFile(path.join(outside, "secret.txt"), "secret answer\n")
    await mkdir(path.join(outside, "dir"))
    await writeFile(path.join(outside, "dir/inner.txt"), "secret inner\n")
    await symlink(path.join(outside, "secret.txt"), path.join(f.directory, "file-link.txt"))
    await symlink(path.join(outside, "dir"), path.join(f.directory, "dir-link"))
    const boxed: Context = { ...f.ctx, sandbox: true }
    return { ...f, outside, boxed }
  }

  test("reads outside the roots, directly, through a link or under ~, are noted in place while the other paths are read", async () => {
    const s = await sandboxed()
    const result = await s.engine.read(s.boxed, { paths: ["file.txt", path.join(s.outside, "secret.txt"), "file-link.txt", "dir-link/inner.txt", "~/nib-missing"] })
    expect(result.text).toStartWith(`[file.txt#${fileTag("one\ntwo\nthree\n")}]\n1:one`)
    for (const refused of [path.join(s.outside, "secret.txt"), "file-link.txt", "dir-link/inner.txt", "~/nib-missing"]) expect(result.text).toContain(`[${refused} is outside the sandbox, which allows only `)
    expect(result.text).not.toContain("secret answer")
    expect(result.text).not.toContain("secret inner")
    await expect(s.engine.read(s.boxed, { paths: ["file-link.txt"] })).rejects.toThrow("file-link.txt is outside the sandbox")
    expect((await s.engine.read(s.ctx, { paths: ["file-link.txt"] })).text).toContain("secret answer")
  })

  test("grep and find refuse paths outside the roots, search the rest, and do not follow links out of the tree", async () => {
    const s = await sandboxed()
    const grep = await s.engine.grep(s.boxed, { pattern: "secret|one", paths: ["/", ".", s.outside] })
    expect(grep.text).toStartWith("[/ is outside the sandbox, which allows only ")
    expect(grep.text).toContain(`; ${s.outside} is outside the sandbox`)
    expect(grep.text).toContain("; searched the other paths]")
    expect(grep.text).toContain("*1:one")
    expect(grep.text).not.toContain("secret answer")
    expect(grep.text).not.toContain("secret inner")
    await expect(s.engine.grep(s.boxed, { pattern: "secret", paths: ["/"] })).rejects.toThrow("/ is outside the sandbox")
    await expect(s.engine.grep(s.boxed, { pattern: "secret", paths: ["dir-link"] })).rejects.toThrow("dir-link is outside the sandbox")
    const find = await s.engine.find(s.boxed, { paths: [".", s.outside] })
    expect(find.text).toStartWith(`[${s.outside} is outside the sandbox`)
    expect(find.text).toContain("; listed the other paths]")
    expect(find.text).toContain("file.txt")
    expect(find.text).not.toContain("inner.txt")
    await expect(s.engine.find(s.boxed, { paths: ["dir-link"] })).rejects.toThrow("dir-link is outside the sandbox")
  })

  test("edits and writes outside the roots fail before anything is created, through links and move targets too", async () => {
    const s = await sandboxed()
    await expect(s.engine.write(s.boxed, { path: path.join(s.outside, "new/deep.txt"), content: "x" })).rejects.toThrow("is outside the sandbox")
    await expect(stat(path.join(s.outside, "new"))).rejects.toThrow()
    await expect(s.engine.write(s.boxed, { path: "dir-link/made/x.txt", content: "x" })).rejects.toThrow("dir-link/made/x.txt is outside the sandbox")
    await expect(stat(path.join(s.outside, "dir/made"))).rejects.toThrow()
    const tag = fileTag("one\ntwo\nthree\n")
    await s.engine.read(s.boxed, { paths: ["file.txt"] })
    await expect(s.engine.patch(s.boxed, `[file.txt#${tag}]\nPUT 1.=1:\n+ONE\nMV ${path.join(s.outside, "moved.txt")}`)).rejects.toThrow("moved.txt is outside the sandbox")
    await expect(s.engine.patch(s.boxed, `[file.txt#${tag}]\nPUT 1.=1:\n+ONE\n[file-link.txt#${fileTag("secret answer\n")}]\nPUT 1.=1:\n+leaked`)).rejects.toThrow("file-link.txt is outside the sandbox")
    expect(await readFile(s.filePath, "utf8")).toBe("one\ntwo\nthree\n")
    expect(await readFile(path.join(s.outside, "secret.txt"), "utf8")).toBe("secret answer\n")
    await expect(stat(path.join(s.outside, "moved.txt"))).rejects.toThrow()
    await s.engine.patch(s.boxed, `[file.txt#${tag}]\nPUT 1.=1:\n+ONE`)
    expect(await readFile(s.filePath, "utf8")).toBe("ONE\ntwo\nthree\n")
  })

  test("a patch is judged whole before any parent directory is created for it", async () => {
    const s = await sandboxed()
    await expect(s.engine.patch(s.boxed, `*** Add File: new/deep/a.txt\n+a\n*** Add File: ${path.join(s.outside, "b.txt")}\n+b`)).rejects.toThrow("b.txt is outside the sandbox")
    await expect(stat(path.join(s.directory, "new"))).rejects.toThrow()
  })

  test("client roots widen the sandbox", async () => {
    const s = await sandboxed()
    const rooted: Context = { ...s.boxed, roots: async () => [s.directory, s.outside] }
    expect((await s.engine.read(rooted, { paths: ["file-link.txt"] })).text).toContain("secret answer")
    await s.engine.write(rooted, { path: path.join(s.outside, "free.txt"), content: "ok" })
    expect(await readFile(path.join(s.outside, "free.txt"), "utf8")).toBe("ok")
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
      `[file.txt#${tag}]\n 5:line 5\n*6:line 6\n 7:line 7`,
    ].join("\n\n"))
    await f.patch(`[file.txt#${tag}]\nPUT 5.=6:\n+five\n+six\nPUT >7:\n+after seven`)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(4, 8)).toEqual(["five", "six", "line 7", "after seven"])
    await expect(f.patch(`[file.txt#${fileTag(await readFile(f.filePath, "utf8"))}]\nPUT 1.=1:\n+x`)).rejects.toThrow("never displayed")
  })

  test("a parent repository's whitelist .gitignore does not hide a nested repository", async () => {
    const f = await fixture()
    await mkdir(path.join(f.directory, ".git"))
    await writeFile(path.join(f.directory, ".gitignore"), "*\n!.gitignore\n")
    await mkdir(path.join(f.directory, "sub/.git"), { recursive: true })
    await mkdir(path.join(f.directory, "sub/src"))
    await writeFile(path.join(f.directory, "sub/.gitignore"), "build/\n")
    await writeFile(path.join(f.directory, "sub/src/A.java"), "needle\n")
    await mkdir(path.join(f.directory, "sub/build"))
    await writeFile(path.join(f.directory, "sub/build/B.java"), "needle\n")
    const nested = await f.engine.grep(f.ctx, { pattern: "needle", paths: ["sub/src"] })
    expect(nested.text).toStartWith("1 matches in 1 files")
    expect((await f.engine.grep(f.ctx, { pattern: "needle", paths: ["sub"] })).text).toContain("[sub/src/A.java#")
    expect((await f.engine.grep(f.ctx, { pattern: "needle", paths: ["sub"] })).text).not.toContain("build")
  })

  /** Runs git with an identity, failing the test when git does. */
  const git = (...args: string[]) => expect(Bun.spawnSync(["git", "-c", "user.name=nib", "-c", "user.email=nib@example.com", ...args]).exitCode).toBe(0)

  /** A module repository holding a match in src/ and one in its ignored build/. */
  async function module(directory: string) {
    git("init", "-q", directory)
    await mkdir(path.join(directory, "src"), { recursive: true })
    await mkdir(path.join(directory, "build"))
    await writeFile(path.join(directory, ".gitignore"), "build/\ncopy/\n")
    await writeFile(path.join(directory, "src/A.java"), "needle\n")
    await writeFile(path.join(directory, "build/B.java"), "needle\n")
  }

  test("a workspace that ignores the repositories it holds still has them searched, each by its own rules", async () => {
    const f = await fixture()
    git("init", "-q", f.directory)
    await writeFile(path.join(f.directory, ".gitignore"), "*\n!.gitignore\n!apps/\napps/*\n")
    await module(path.join(f.directory, "mod"))
    await module(path.join(f.directory, "apps/app"))
    // A worktree of a repository inside it repeats that repository's files, so it stays hidden.
    git("-C", path.join(f.directory, "mod"), "commit", "-q", "--allow-empty", "-m", "start")
    git("-C", path.join(f.directory, "mod"), "worktree", "add", "-q", path.join(f.directory, "mod/copy"))
    await writeFile(path.join(f.directory, "mod/copy/C.java"), "needle\n")
    const found = (await f.engine.grep(f.ctx, { pattern: "needle" })).text
    expect(found).toStartWith("2 matches in 2 files\n\n[apps/app/src/A.java#")
    expect(found).toContain("\n\n[mod/src/A.java#")
    expect(found).not.toContain("build")
    expect(found).not.toContain("copy")
    const listed = (await f.engine.find(f.ctx, {})).text
    expect(listed).toContain("\napps/app/\n  .gitignore (2 lines, 13 bytes, ")
    expect(listed).toContain("\n  src/A.java (1 line")
    expect(listed).toContain("\nmod/src/A.java (1 line")
    expect(listed).not.toContain("build/")
    // An .ignore that already lets ripgrep into a repository changes nothing.
    await writeFile(path.join(f.directory, ".ignore"), "!/mod/\n")
    expect((await f.engine.grep(f.ctx, { pattern: "needle" })).text).toBe(found)
    // A search inside the workspace still reaches the repositories under its path, and noIgnore searches everything.
    expect((await f.engine.grep(f.ctx, { pattern: "needle", paths: ["apps"], mode: "files" })).text).toBe("1 matches in 1 files\n\napps/app/src/A.java")
    expect((await f.engine.grep(f.ctx, { pattern: "needle", noIgnore: true, mode: "count" })).text).toStartWith("5 matches in 5 files")
  })

  test("outside git, .gitignore does not apply, and a workspace below has its repositories searched", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, ".gitignore"), "loose.txt\n")
    await writeFile(path.join(f.directory, "loose.txt"), "needle\n")
    const workspace = path.join(f.directory, "lane/ws")
    await mkdir(workspace, { recursive: true })
    git("init", "-q", workspace)
    await writeFile(path.join(workspace, ".gitignore"), "*\n!.gitignore\n")
    await module(path.join(workspace, "mod"))
    expect((await f.engine.grep(f.ctx, { pattern: "needle", mode: "files" })).text).toBe("2 matches in 2 files\n\nlane/ws/mod/src/A.java\nloose.txt")
  })

  test("a searched folder whose name holds glob characters is asked of git as a path, so a folder the pattern would match stays out", async () => {
    const f = await fixture()
    git("init", "-q", f.directory)
    await writeFile(path.join(f.directory, ".gitignore"), "*\n!.gitignore\n!/we\\[ird\\]/\n/we\\[ird\\]/*\n!/wei/\n/wei/*\n")
    await module(path.join(f.directory, "we[ird]/mod"))
    await module(path.join(f.directory, "wei/mod"))
    expect((await f.engine.grep(f.ctx, { pattern: "needle", paths: ["we[ird]"], mode: "files" })).text).toBe("1 matches in 1 files\n\nwe[ird]/mod/src/A.java")
  })

  test("a repository inside an ignored folder that is not one itself is searched too", async () => {
    const f = await fixture()
    git("init", "-q", f.directory)
    await writeFile(path.join(f.directory, ".gitignore"), "/modules/\n/cache/\n")
    await module(path.join(f.directory, "modules/app"))
    // A folder with more entries than a folder of repositories has is a cache, and is not looked inside.
    await mkdir(path.join(f.directory, "cache"))
    for (let index = 0; index < 1001; index++) await mkdir(path.join(f.directory, "cache", `p${index}`))
    await module(path.join(f.directory, "cache/p0"))
    expect((await f.engine.grep(f.ctx, { pattern: "needle", mode: "files" })).text).toBe("1 matches in 1 files\n\nmodules/app/src/A.java")
    expect((await f.engine.find(f.ctx, { glob: "A.java" })).text).toStartWith("1 file\n\nmodules/app/src/A.java")
  })

  test("only git's rules are overruled: .ignore, .rgignore and an excluding glob keep a nested repository out", async () => {
    const f = await fixture()
    git("init", "-q", f.directory)
    await writeFile(path.join(f.directory, ".gitignore"), "*\n!.gitignore\n!.ignore\n!.rgignore\n")
    for (const name of ["kept", "dotignored", "rgignored", "globbed"]) await module(path.join(f.directory, name))
    await writeFile(path.join(f.directory, ".ignore"), "/dotignored/\n")
    await writeFile(path.join(f.directory, ".rgignore"), "/rgignored/\n")
    expect((await f.engine.grep(f.ctx, { pattern: "needle", glob: "!globbed", mode: "files" })).text).toBe("1 matches in 1 files\n\nkept/src/A.java")
    expect((await f.engine.find(f.ctx, { glob: ["A.java", "!globbed"] })).text).toStartWith("1 file\n\nkept/src/A.java")
  })

  test("a search never runs a command a repository's config names, such as core.fsmonitor", async () => {
    const f = await fixture()
    git("init", "-q", f.directory)
    const marker = path.join(await workspace(), "ran")
    git("-C", f.directory, "config", "core.fsmonitor", `touch ${marker}; echo`)
    await writeFile(path.join(f.directory, ".gitignore"), "*\n")
    await module(path.join(f.directory, "mod"))
    expect((await f.engine.grep(f.ctx, { pattern: "needle", mode: "files" })).text).toBe("1 matches in 1 files\n\nmod/src/A.java")
    await f.engine.find(f.ctx, {})
    await expect(stat(marker)).rejects.toThrow()
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
    // The match sits past the first 512 characters, so the row shows the stretch around it instead of the start.
    expect(result.text).toContain(`*[1 chars 516-606 of 606]|${"x".repeat(85)}needle\n`)
    expect(result.text).toContain("*2:needle")
  })
})

describe("snapshots", () => {
  test("tags match oh-my-pi file_hash vectors and ignore trailing whitespace and CRs", () => {
    expect([fileTag("a \n b\t\r\nc"), fileTag("hello\n"), fileTag("")]).toEqual(["80BA", "5BF9", "5D05"])
    expect(fileTag("x  \r\ny\t")).toBe(fileTag("x\ny"))
    expect(fileTag("x\ny")).not.toBe(fileTag("x\ny\n"))
  })
})

describe("created files", () => {
  test("a created file is editable at once, whatever its size, since the agent wrote every line; a full overwrite needs no lines", async () => {
    const f = await fixture()
    const small = await f.engine.write(f.ctx, { path: "small.txt", content: "a\nb\nc\n" })
    const smallTag = /#([0-9A-F]{4})\]/.exec(small.text)![1]!
    expect((await f.patch(`[small.txt#${smallTag}]\nPUT 2.=2:\n+B`)).text).toContain("1:a\n2:B\n3:c")
    const content = Array.from({ length: 12 }, (_, index) => `line ${index + 1}`).join("\n") + "\n"
    const large = await f.engine.write(f.ctx, { path: "large.txt", content })
    const largeTag = /#([0-9A-F]{4})\]/.exec(large.text)![1]!
    const edited = await f.patch(`[large.txt#${largeTag}]\nPUT 5.=5:\n+five`)
    expect(edited.text).toContain("4:line 4\n5:five\n6:line 6")
    const overwritten = await f.engine.write(f.ctx, { path: "large.txt", content: "x\n", tag: edited.tag })
    expect(overwritten.text).toStartWith("Overwrote [large.txt#")
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

const numbered = (count: number, prefix = "line") => Array.from({ length: count }, (_, index) => `${prefix} ${index + 1}`).join("\n") + "\n"
const refusal = (attempt: Promise<unknown>) => attempt.then(() => { throw new Error("expected a refusal") }, (error: Error) => error.message)
const tokenOf = (message: string) => /REAPPLY ([a-z0-9]{6})$/.exec(message)?.[1]

describe("refusals and reapply tokens", () => {
  const setup = async () => {
    const f = await fixture(numbered(20))
    await writeFile(path.join(f.directory, "b.txt"), numbered(20, "bee"))
    await writeFile(path.join(f.directory, "c.txt"), "c\n")
    const read = await f.engine.read(f.ctx, { paths: ["file.txt:1-2", "b.txt:1-2", "c.txt"] })
    const [a, b, c] = [...read.text.matchAll(/#([0-9A-F]{4})\]/g)].map((match) => match[1])
    return { f, input: `[file.txt#${a}]\nPUT 10.=10:\n+TEN\n[b.txt#${b}]\nPUT 15.=15:\n+FIFTEEN\n[c.txt#${c}]\nPUT 1.=1:\n+C` }
  }

  test("one section's failed checks, failed SUBs and unseen lines come in one refusal, and what it displays counts as seen", async () => {
    const f = await fixture(numbered(20))
    const tag = /#([0-9A-F]{4})\]/.exec((await f.read(":1-5")).text)![1]
    const message = await refusal(f.patch(`[file.txt#${tag}]\nPUT 2:\n~wrong\n+x\nSUB 3: nothere\n+y\nPUT 15:\n+z\nPUT 99:\n+past the end`))
    // A hunk past the end is named first and takes nothing away from the others.
    expect(message).toStartWith(`[file.txt#${tag}]: Line 99 does not exist in file.txt (file has 20 lines)\n\n[file.txt#${tag}]: ~ check rows do not match, so these line numbers are off:\nline 2 does not start with "wrong"`)
    expect(message).toContain(`\n\n[file.txt#${tag}]: SUB refused:\nSUB 3: "nothere" does not occur on line 3`)
    expect(message).toContain("\n\nThis edit anchors to lines 15 of file.txt")
    expect(tokenOf(message)).toBeUndefined()
    expect(await readFile(f.filePath, "utf8")).toBe(numbered(20))
    await f.patch(`[file.txt#${tag}]\nPUT 2:\n~line 2\n+x\nSUB 3: line\n+LINE\nPUT 15:\n+z`)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(1, 3).concat((await readFile(f.filePath, "utf8")).split("\n")[14]!)).toEqual(["x", "LINE 3", "z"])
  })

  test("an agent is told what an unseen-line refusal means once; later ones keep only the rows and the token", async () => {
    const f = await fixture(numbered(20))
    const tag = /#([0-9A-F]{4})\]/.exec((await f.read(":1-2")).text)![1]
    const first = await refusal(f.patch(`[file.txt#${tag}]\nPUT 10:\n+x`))
    expect(first).toContain("never displayed in full (partial read, truncated line, or rows elided from an edit response).")
    expect(first).toContain("If these are the lines you meant, the same patch now succeeds; otherwise fix your line numbers.")
    expect(first).toContain("To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY ")
    const later = await refusal(f.patch(`[file.txt#${tag}]\nPUT 15:\n+y`))
    expect(later).toContain(`This edit anchors to lines 15 of file.txt that [file.txt#${tag}] never displayed in full. Actual file content (* marks those lines):`)
    expect(later).toContain("*15:line 15\n 16:line 16\n 17:line 17\nMeant these? Resend unchanged; otherwise fix the numbers.\n\nREAPPLY ")
    expect(later).not.toContain("To apply this patch")
    await f.patch(`REAPPLY ${tokenOf(later)}`)
    expect((await readFile(f.filePath, "utf8")).split("\n")[14]).toBe("y")
  })

  test("every section's unseen lines come in one refusal, and its token applies the whole patch like an unchanged resend", async () => {
    const { f, input } = await setup()
    const message = await refusal(f.patch(input))
    expect(message).toStartWith("Nothing was written; 2 sections have problems.")
    expect(message).toContain("This edit anchors to lines 10 of file.txt")
    expect(message).toContain(" 8:line 8\n 9:line 9\n*10:line 10\n 11:line 11\n 12:line 12")
    expect(message).toContain("This edit anchors to lines 15 of b.txt")
    expect(await readFile(f.filePath, "utf8")).toBe(numbered(20))
    expect(await readFile(path.join(f.directory, "c.txt"), "utf8")).toBe("c\n")
    const token = tokenOf(message)
    expect(token).toBeDefined()
    const applied = await f.patch(`REAPPLY ${token}`)
    const { f: g, input: same } = await setup()
    await refusal(g.patch(same))
    const resent = await g.patch(same)
    expect(applied.text).toBe(resent.text)
    for (const name of ["file.txt", "b.txt", "c.txt"]) {
      expect(await readFile(path.join(f.directory, name), "utf8")).toBe(await readFile(path.join(g.directory, name), "utf8"))
    }
    expect((await readFile(f.filePath, "utf8")).split("\n")[9]).toBe("TEN")
    expect((await readFile(path.join(f.directory, "b.txt"), "utf8")).split("\n")[14]).toBe("FIFTEEN")
    expect(await readFile(path.join(f.directory, "c.txt"), "utf8")).toBe("C\n")
  })

  test("rows around a refused line are displayed too, so a miscount can be fixed without a read", async () => {
    const f = await fixture(numbered(20))
    const r = await f.read(":1-1")
    await refusal(f.edit(r.tag, [{ start: 10, content: "x", operation: "replace" }]))
    await f.edit(r.tag, [{ start: 12, content: "TWELVE", operation: "replace" }])
    expect((await readFile(f.filePath, "utf8")).split("\n")[11]).toBe("TWELVE")
  })

  test("a token works once, and not after its file changed in a way that cannot be rebased", async () => {
    const f = await fixture()
    const r = await f.read(":1-1")
    const token = tokenOf(await refusal(f.edit(r.tag, [{ start: 2, content: "TWO", operation: "replace" }])))!
    await f.patch(`REAPPLY ${token}`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\nTWO\nthree\n")
    expect(await refusal(f.patch(`REAPPLY ${token}`))).toBe(`Reapply token ${token} is unknown, already used or expired; send the full patch again`)
    const g = await fixture()
    const s = await g.read(":1-1")
    const stale = tokenOf(await refusal(g.edit(s.tag, [{ start: 2, content: "TWO", operation: "replace" }])))!
    await writeFile(g.filePath, "one\nCHANGED\nthree\n")
    expect(await refusal(g.patch(`REAPPLY ${stale}`))).toContain("is stale")
    expect(await readFile(g.filePath, "utf8")).toBe("one\nCHANGED\nthree\n")
  })

  test("refusals for any other reason carry no token, even beside an unseen-line refusal", async () => {
    const f = await fixture(numbered(10))
    await writeFile(path.join(f.directory, "b.txt"), "b\n")
    const r = await f.read(":1-1")
    const other = await f.read("", { file: path.join(f.directory, "b.txt") })
    const wrong = other.tag === "0000" ? "0001" : "0000"
    const messages = [
      await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 1.=1:\n+x\nCUT 1.=1`)),
      await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 1-2:\n+x`)),
      await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 1.=1:\n~nope\n+x`)),
      await refusal(f.patch(`[file.txt#${r.tag}]\nSUB 1: nope\n+x`)),
      await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 5.=5:\n+x\n[b.txt#${wrong}]\nPUT 1.=1:\n+y`)),
    ]
    for (const message of messages) expect(message).not.toContain("REAPPLY")
    expect(messages[0]).toContain("Edits overlap")
    expect(messages[2]).toContain("line 1 does not start with \"nope\"")
    expect(messages[3]).toContain("SUB 1: \"nope\" does not occur on line 1")
    expect(messages[4]).toStartWith("Nothing was written; 2 sections have problems.")
    expect(messages[4]).toContain("This edit anchors to lines 5 of file.txt")
    expect(messages[4]).toContain(`tag #${wrong} was not issued for this file`)
    await writeFile(f.filePath, numbered(10).replace("line 1\n", "LINE ONE\n"))
    const stale = await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 1.=1:\n+x`))
    expect(stale).toContain("is stale")
    expect(stale).not.toContain("REAPPLY")
    expect(await readFile(path.join(f.directory, "b.txt"), "utf8")).toBe("b\n")
  })

  test("an evicted token is refused with a hint to send the patch again", async () => {
    const f = await fixture(numbered(400))
    const r = await f.read(":1-1")
    const first = tokenOf(await refusal(f.edit(r.tag, [{ start: 10, content: "x", operation: "replace" }])))!
    for (let line = 20; line < 20 + 64 * 5; line += 5) await refusal(f.edit(r.tag, [{ start: line, content: "x", operation: "replace" }]))
    expect(await refusal(f.patch(`REAPPLY ${first}`))).toContain("unknown, already used or expired; send the full patch again")
  })
})

describe("final newlines", () => {
  test("a lone + row appended to a file without a final newline adds one in the file's own line endings, and no row for it", async () => {
    const f = await fixture("one\r\ntwo\r\nthree")
    const r = await f.read()
    const added = await f.patch(`[file.txt#${r.tag}]\nPUT >$:\n+`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\r\ntwo\r\nthree\r\n")
    expect(added.text).toBe(`[file.txt#${added.tag}]\n3:three\n[file.txt now ends with a newline]`)
    expect((await f.read()).text).toBe(`[file.txt#${added.tag}]\n1:one\n2:two\n3:three`)
    // With the final newline in place, the same hunk adds a real blank line.
    const blank = await f.patch(`[file.txt#${added.tag}]\nPUT >$:\n+`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\r\ntwo\r\nthree\r\n\r\n")
    expect(blank.text).toBe(`[file.txt#${blank.tag}]\n3:three\n4:`)
  })

  test("an empty row that ends an unterminated file next to a deletion still gets its response, before anything is written", async () => {
    const f = await fixture("a\nb\nc")
    const r = await f.read()
    const edited = await f.patch(`[file.txt#${r.tag}]\nPUT 2:\n+\nCUT 3`)
    expect(await readFile(f.filePath, "utf8")).toBe("a\n")
    expect(edited.text).toStartWith(`[file.txt#${edited.tag}]\n1:a`)
    expect(edited.text).toContain("[file.txt now ends with a newline]")
  })

  test("new last rows ending in an empty one end the file with a newline after the last of them", async () => {
    const f = await fixture("a\nb")
    const r = await f.read()
    const added = await f.patch(`[file.txt#${r.tag}]\nPUT >$:\n+c\n+`)
    expect(await readFile(f.filePath, "utf8")).toBe("a\nb\nc\n")
    expect(added.text).toBe(`[file.txt#${added.tag}]\n2:b\n3:c\n[file.txt now ends with a newline]`)
  })

  test("rewriting the last line of a file without a final newline says how to add one", async () => {
    const f = await fixture("one\ntwo")
    const r = await f.read()
    expect(r.text).toEndWith("\n2:two\n[no final newline; PUT >$: with a lone + row adds one]")
    const same = await f.patch(`[file.txt#${r.tag}]\nPUT 2:\n+two`)
    expect(same.text).toBe(`[file.txt#${r.tag}]\nNo changes: these lines already read as the edit writes them; this tag is still current.\n1:one\n2:two\n[file.txt has no final newline, which an edit keeps; PUT >$: with a lone + row adds one]`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo")
  })

  test("overwriting keeps a CRLF file's line endings and a BOM unless the content brings its own; the final newline follows the content", async () => {
    const f = await fixture()
    const write = async (name: string, before: string, content: string) => {
      await writeFile(path.join(f.directory, name), before)
      const tag = /#([0-9A-F]{4})\]/.exec((await f.engine.read(f.ctx, { paths: [name] })).text)![1]
      const result = await f.engine.write(f.ctx, { path: name, content, tag })
      return { text: result.text.split("\n")[0]!, bytes: await readFile(path.join(f.directory, name), "utf8") }
    }
    expect(await write("crlf.csv", "id,qty\r\n1,4", "id,qty\n1,4\n2,9\n")).toEqual({ text: expect.stringMatching(/; kept its CRLF line endings\)$/), bytes: "id,qty\r\n1,4\r\n2,9\r\n" })
    expect(await write("bom.txt", `${BOM}a\r\nb\r\n`, "a\nB")).toEqual({ text: expect.stringMatching(/; kept its CRLF line endings and BOM\)$/), bytes: `${BOM}a\r\nB` })
    expect((await write("given.txt", "a\r\nb\r\n", "a\r\nb\r\nc\r\n")).bytes).toBe("a\r\nb\r\nc\r\n")
    expect(await write("mixed.txt", "a\r\nb\nc\r\n", "a\nb\n")).toEqual({ text: expect.not.stringContaining("kept its"), bytes: "a\nb\n" })
    expect((await write("lf.txt", "a\nb\n", "a\nb")).bytes).toBe("a\nb")
  })
})

describe("row maps and ~ checks", () => {
  const brief = [
    "# Handoff",
    "",
    "Lane one is a long line that keeps going past thirty characters",
    "",
    "## Settled",
    ...Array.from({ length: 12 }, (_, index) => `- item ${index + 6}`),
    "",
    "## Do the remaining work before the deadline",
    "",
    "1. step",
    "2. step",
    "Last line",
  ].join("\n") + "\n"
  const tag = fileTag(brief)
  const lineOf = async (f: Awaited<ReturnType<typeof fixture>>, name: string, line: number) => (await readFile(path.join(f.directory, name), "utf8")).split("\n")[line - 1]

  test("write answers with a row map, and every line it wrote is an anchor at once", async () => {
    const f = await fixture()
    const created = await f.engine.write(f.ctx, { path: "brief.md", content: brief })
    expect(created.text).toBe([
      `Created [brief.md#${tag}] (23 lines, 19 non-blank, 5 shown)`,
      "1~# Handoff",
      "5~## Settled",
      "15~- item 15",
      "19~## Do the remaining work befor...",
      "23~Last line",
    ].join("\n"))
    const edited = await f.patch(`[brief.md#${tag}]\nPUT >19:\n+\n+Context.\nPUT 15.=15:\n+- item fifteen\nPUT 6.=7:\n+- items 6 and 7`)
    expect(edited.text).toContain("6:- items 6 and 7\n7:- item 8\n[lines 8-12 are old lines 9-13, moved up by 1]\n13:- item 14\n14:- item fifteen\n15:- item 16\n[lines 16-17 are old lines 17-18, moved up by 1]\n18:## Do the remaining work before the deadline\n19:\n20:Context.\n21:\n")
    expect(await lineOf(f, "brief.md", 20)).toBe("Context.")
    expect(await lineOf(f, "brief.md", 6)).toBe("- items 6 and 7")
  })

  test("a ~ check on a written line still verifies its number; a wrong one shows the real rows", async () => {
    const f = await fixture()
    for (const name of ["b.md", "c.md"]) await f.engine.write(f.ctx, { path: name, content: brief })
    const wrong = await refusal(f.patch(`[b.md#${tag}]\nPUT 21.=21:\n~2. step\n+x`))
    expect(wrong).toContain(`[b.md#${tag}]: ~ check rows do not match, so these line numbers are off:\nline 21 does not start with "2. step"`)
    expect(wrong).toContain(" 20:\n*21:1. step\n 22:2. step")
    expect(wrong).not.toContain("REAPPLY")
    expect(await lineOf(f, "b.md", 21)).toBe("1. step")
    // An empty check means a blank line.
    await f.patch(`[c.md#${tag}]\nCUT 18.=18\n~`)
    expect(await lineOf(f, "c.md", 18)).toBe("## Do the remaining work before the deadline")
  })

  test("on lines the agent did not write, a check lets an insert through but never a replace", async () => {
    const f = await fixture(numbered(30))
    const r = await f.read(":1-1")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 20.=20:\n~line 20\n+x`))).toContain("never displayed in full")
    await f.patch(`[file.txt#${r.tag}]\nPUT >25:\n~line 25\n+inserted`)
    expect(await lineOf(f, "file.txt", 26)).toBe("inserted")
  })

  test("lines seen before an edit moved them stay known, so a check anchors them without a read", async () => {
    const f = await fixture(numbered(30))
    const first = await f.edit((await f.read()).tag, [{ start: 1, content: "header", operation: "before" }])
    await f.patch(`[file.txt#${first.tag}]\nPUT 21.=21:\n~line 20\n+LINE 20`)
    expect(await lineOf(f, "file.txt", 21)).toBe("LINE 20")
  })

  test("a long inserted run is mapped in its elided middle, and those rows are anchors", async () => {
    const f = await fixture("end\n")
    const r = await f.read()
    const inserted = await f.edit(r.tag, [{ start: 1, content: Array.from({ length: 30 }, (_, index) => `new ${index + 1}`).join("\n"), operation: "before" }])
    expect(inserted.text).toBe(`[file.txt#${inserted.tag}]\n1:new 1\n11~new 11\n21~new 21\n30:new 30\n31:end`)
    // A shown map row is an anchor; one between them needs a ~ check.
    const next = await f.patch(`[file.txt#${inserted.tag}]\nPUT 11.=11:\n+NEW 11\nPUT 15.=15:\n~new 15\n+NEW 15`)
    expect(next.text).toContain("11:NEW 11")
    expect(await lineOf(f, "file.txt", 15)).toBe("NEW 15")
  })

  test("past 300 map rows a written file gets the outline instead, whose rows are anchors", async () => {
    const f = await fixture()
    const content = Array.from({ length: 1000 }, (_, index) => `export function f${index}() {\n  return ${index}\n}`).join("\n") + "\n"
    const lines = (await f.engine.write(f.ctx, { path: "big.ts", content })).text.split("\n")
    expect(lines[0]).toBe(`Created [big.ts#${fileTag(content)}] (3000 lines, 3000 non-blank; outline only)`)
    expect(lines.slice(1, 3)).toEqual(["1:export function f0() {", "4:export function f1() {"])
    expect(lines.at(-1)).toBe("[outline capped at 150 rows; 850 more members or declarations not shown]")
    await f.patch(`[big.ts#${fileTag(content)}]\nPUT 4.=4:\n+export function g1() {`)
    expect(await lineOf(f, "big.ts", 4)).toBe("export function g1() {")
  })
})

describe("SUB", () => {
  test("replaces text inside a line, several per line, splitting or deleting", async () => {
    const f = await fixture("const a = compute(1) // old note\nkeep\n")
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nSUB 1: compute(1)\n+compute(2)\nSUB 1:  // old note\n+\nSUB 2: ee\n+e\n+E`)
    expect(await readFile(f.filePath, "utf8")).toBe("const a = compute(2)\nke\nEp\n")
    expect(result.text).toBe(`[file.txt#${result.tag}]\n1:const a = compute(2)\n2:ke\n3:Ep`)
  })

  test("an old text that is missing, repeated or overlapping is refused with the line", async () => {
    const f = await fixture("aaa b\nother\n")
    const r = await f.read()
    const missing = await refusal(f.patch(`[file.txt#${r.tag}]\nSUB 1: zzz\n+x`))
    expect(missing).toContain("SUB 1: \"zzz\" does not occur on line 1")
    expect(missing).toContain("*1:aaa b")
    expect(missing).not.toContain("REAPPLY")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nSUB 1: aa\n+x`))).toContain("occurs more than once on line 1")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nSUB 1: aaa\n+x\nSUB 1: a b\n+y`))).toContain("two old texts overlap")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nSUB 1: b\n+x\nPUT 1.=1:\n+y`))).toContain("Edits overlap")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nSUB 9: b\n+x`))).toContain("Line 9 does not exist in file.txt")
    expect(await readFile(f.filePath, "utf8")).toBe("aaa b\nother\n")
  })

  test("an old text missing from its line applies on the one nearby line of the file that holds it, saying so", async () => {
    const content = "func a() {\n\twhy := \"one of its options\"\n\treturn why\n}\n\nfunc b() {\n\tnote := \"short\"\n}\n"
    const f = await fixture(content)
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nSUB 1: "one of its options"\n+"this form of it"`)
    expect(await readFile(f.filePath, "utf8")).toBe(content.replace("one of its options", "this form of it"))
    expect(result.text).toContain("SUB 1: its old text is not on line 1 but on line 2, the one line of the file that holds it, so it applied there")
    expect(result.text).toContain('2:\twhy := "this form of it"')
  })

  test("an old text missing from its line is still refused when its line is in doubt", async () => {
    const content = "alpha\n\tvalue := compute(1)\n\tvalue := compute(1)\nbeta\ngamma\ndelta\nepsilon\n\tfar := somewhere(2)\n\tnote := x\n"
    const f = await fixture(content)
    const r = await f.read()
    const doubt = async (input: string, why: string) => {
      const message = await refusal(f.patch(`[file.txt#${r.tag}]\n${input}`))
      expect(message, why).toContain("does not occur on line")
    }
    await doubt("SUB 1: value := compute(1)\n+x", "on two lines of the file")
    await doubt("SUB 4: somewhere(2)\n+x", "more than 2 lines away")
    await doubt("SUB 8: x\n+y", "too short to tell")
    await doubt("SUB 7: somewhere(2)\n+x\nPUT 8:\n+y", "on a line another hunk anchors")
    expect(await readFile(f.filePath, "utf8")).toBe(content)
    // A line the agent was never shown is not taken either.
    const g = await fixture(content.replace("\tnote := x", "\tnote := unique(3)"))
    const head = await g.read(":1-7")
    expect(await refusal(g.patch(`[file.txt#${head.tag}]\nSUB 7: unique(3)\n+x`))).toContain("does not occur on line 7")
  })

  test("SUB edits a cut long line without reading it whole, and its result does not become known content", async () => {
    const long = "x".repeat(3000) + " marker " + "y".repeat(100)
    const f = await fixture(long + "\nshort\n")
    const r = await f.read()
    expect(r.anchors).toEqual([2])
    const result = await f.patch(`[file.txt#${r.tag}]\nSUB 1: marker\n+MARKER`)
    expect(await readFile(f.filePath, "utf8")).toBe(long.replace("marker", "MARKER") + "\nshort\n")
    // The line is dense, so the file counts as minified: the response shows 50 characters on each side of the change, not the start of the line.
    expect(result.text).toContain(`\n[1 chars 2952-3057 of 3108]|${"x".repeat(49)} MARKER ${"y".repeat(49)}\n`)
    expect(result.text).toContain("\n[cut rows: change them in place with SUB N: OLD")
    expect(await refusal(f.patch(`[file.txt#${result.tag}]\nPUT 1.=1:\n~xxx\n+short`))).toContain("never displayed in full")
  })

  test("a long prose line edited deep inside shows the stretch around the change in the response", async () => {
    const paragraph = "alpha " + "word ".repeat(600) + "target " + "word ".repeat(100)
    const f = await fixture(paragraph + "\nshort\n")
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nSUB 1: target\n+goal`)
    // Prose is not minified, so the stretch reaches a sixth of the usual 2000 characters on each side of the change.
    expect(result.text).toContain("\n[1 chars 2674-3343 of 3511]|")
    expect(result.text).toContain(" word goal word ")
    // A change near the start of a long line is visible in a plain cut, so that stays as it was.
    const start = await f.patch(`[file.txt#${result.tag}]\nSUB 1: alpha\n+first`)
    expect(start.text).toContain("\n[1 cut at 2000 of 3511 chars]|first word word")
  })

  test("SUB rows that repeat the line's indentation get it once, with a lesson", async () => {
    const f = await fixture("class A {\n    run(a, b);\n\tstop();\n}\n")
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nSUB 2: run(a, b);\n+    run(a);\n+    log(a);\nSUB 3: stop\n+\thalt`)
    expect(await readFile(f.filePath, "utf8")).toBe("class A {\n    run(a);\n    log(a);\n\thalt();\n}\n")
    expect(result.text).toContain("SUB 2: the + rows repeated the line's indentation, which SUB keeps around its old text, so it was written once")
    expect(result.text).toContain("SUB 3: the + rows repeated")
    // Rows that do not repeat it, and old text that takes the indentation along, are written as sent.
    const plain = await f.patch(`[file.txt#${result.tag}]\nSUB 2: run(a);\n+go();\nSUB 5: }\n+  }`)
    expect(await readFile(f.filePath, "utf8")).toBe("class A {\n    go();\n    log(a);\n\thalt();\n  }\n")
    expect(plain.text).not.toContain("indentation")
  })

  test("a stale tag rebases SUB like any other edit", async () => {
    const f = await fixture("alpha one\nbeta two\ngamma three\n")
    const r = await f.read()
    await writeFile(f.filePath, "zero\nalpha one\nbeta two\ngamma three\n")
    const result = await f.patch(`[file.txt#${r.tag}]\nSUB 2: two\n+2`)
    expect(await readFile(f.filePath, "utf8")).toBe("zero\nalpha one\nbeta 2\ngamma three\n")
    expect(result.text).toContain("Recovered by remapping stale line numbers")
  })
})

describe("tagless appends", () => {
  test("a [PATH] section appends without a tag, even in a session that never read the file", async () => {
    const f = await fixture()
    const result = await f.patch("[file.txt]\nPUT >$:\n+four")
    expect(result.text).toBe(`[file.txt#${fileTag("one\ntwo\nthree\nfour\n")}]\n3:three\n4:four`)
    await f.engine.patch({ ...f.ctx, session: "other" }, "[file.txt]\nPUT >$:\n+five")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\nfour\nfive\n")
  })

  test("a tag never issued for the file still appends, with a warning, unless it was issued for another file", async () => {
    const f = await fixture()
    const r = await f.read()
    const wrong = r.tag === "0000" ? "0001" : "0000"
    const appended = await f.patch(`[file.txt#${wrong}]\nPUT >$:\n+four`)
    expect(appended.text).toContain(`\n\nWarnings:\nTag #${wrong} was never issued for this file; never invent tags.`)
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\nfour\n")
    await writeFile(path.join(f.directory, "other.txt"), "other\n")
    const other = await f.read("", { file: path.join(f.directory, "other.txt") })
    const message = await refusal(f.patch(`[file.txt#${other.tag}]\nPUT >$:\n+five`))
    expect(message).toContain(`tag #${other.tag} was not issued for this file`)
    expect(message).toContain(`Tag #${other.tag} was issued for`)
    expect(message).toContain("To append without a tag, use a [file.txt] header with only PUT >$: hunks.")
    expect(await readFile(f.filePath, "utf8")).toBe("one\ntwo\nthree\nfour\n")
  })
})

describe("unknown tags", () => {
  const lines = numbered(10)
  const wrongFor = (content: string) => fileTag(content) === "0000" ? "0001" : "0000"

  test("SUB and matching - rows apply under a tag never issued, with a warning, and leave unread lines unseen", async () => {
    const f = await fixture(lines)
    const wrong = wrongFor(lines)
    const result = await f.patch(`[file.txt#${wrong}]\nSUB 2: line 2\n+second\nPUT 4.=4:\n-line 4\n+fourth`)
    expect(await readFile(f.filePath, "utf8")).toBe(lines.replace("line 2\n", "second\n").replace("line 4\n", "fourth\n"))
    expect(result.text).toContain(`Warnings:\nTag #${wrong} was never issued for this file; never invent tags. The edit was applied only because every hunk is safe without one`)
    expect(result.text).toContain("\nThe - rows matched the file, so the edit was applied;")
    // The response showed lines 1 to 5; the agent never read line 9, so the new tag does not anchor it.
    await expect(f.patch(`[file.txt#${result.tag}]\nPUT 9.=9:\n+ninth`)).rejects.toThrow("never displayed in full")
    await f.patch(`[file.txt#${result.tag}]\nPUT 5.=5:\n+fifth`)
    expect(await readFile(f.filePath, "utf8")).toContain("fifth\n")
  })

  test("a tag issued for another file is no mix-up for hunks that check themselves", async () => {
    const f = await fixture(lines)
    await writeFile(path.join(f.directory, "other.txt"), "other\n")
    const other = await f.read("", { file: path.join(f.directory, "other.txt") })
    const result = await f.patch(`[file.txt#${other.tag}]\nSUB 3: line 3\n+third`)
    expect(result.text).toContain(`Tag #${other.tag} was never issued for this file`)
    expect(await readFile(f.filePath, "utf8")).toContain("\nthird\n")
  })

  test("any other hunk, a SUB that does not match, REM, MV and CP are refused as before", async () => {
    const f = await fixture(lines)
    const wrong = wrongFor(lines)
    for (const body of ["PUT 2.=2:\n+x", "SUB 2: line 2\n+x\nPUT 5.=5:\n+y", "PUT 2.=2:\n~line\n+x", "CUT 2.=2", "REM", "MV moved.txt", "CP copy.txt"]) {
      expect(await refusal(f.patch(`[file.txt#${wrong}]\n${body}`))).toContain(`tag #${wrong} was not issued for this file`)
    }
    expect(await refusal(f.patch(`[file.txt#${wrong}]\nSUB 2: line 3\n+x`))).toContain('SUB 2: "line 3" does not occur on line 2')
    expect(await refusal(f.patch(`[file.txt#${wrong}]\nPUT 2.=2:\n-line 3\n+x`))).toContain("the - rows do not match the file")
    expect(await readFile(f.filePath, "utf8")).toBe(lines)
  })
})

describe("grep options", () => {
  test("a missing path is noted and read, grep and find serve the others; only with none left is it an error", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "b.txt"), "one more\n")
    await expect(f.engine.grep(f.ctx, { pattern: "one", paths: ["nope.txt"] })).rejects.toThrow("nope.txt does not exist")
    await expect(f.engine.grep(f.ctx, { pattern: "one", paths: ["a.txt", "c.txt:1-2"] })).rejects.toThrow("a.txt does not exist; c.txt:1-2 does not exist")
    const grepped = (await f.engine.grep(f.ctx, { pattern: "one", paths: ["file.txt", "a.txt", "c.txt:1-2", "b.txt"] })).text
    expect(grepped).toStartWith("[a.txt does not exist; c.txt:1-2 does not exist; searched the other paths]\n\n2 matches in 2 files\n\n[b.txt#")
    expect((await f.engine.grep(f.ctx, { pattern: "one", paths: ["file.txt:1-2"] })).text).toContain("*1:one")
    const read = (await f.engine.read(f.ctx, { paths: ["file.txt", "nope.txt", "b.txt"] })).text
    expect(read).toContain("\n3:three\n\n[nope.txt does not exist]\n\n[b.txt#")
    await expect(f.engine.read(f.ctx, { paths: ["nope.txt", "a.txt"] })).rejects.toThrow("nope.txt does not exist; a.txt does not exist")
    expect((await f.engine.find(f.ctx, { paths: ["file.txt", "nope"] })).text).toMatch(/^\[nope does not exist; listed the other paths\]\n\n1 file\n\nfile\.txt \(3 lines/)
    // path:A:B reads as a selector on a file named path:A, so the error gives the range form.
    await expect(f.engine.read(f.ctx, { paths: ["file.txt:1:2"] })).rejects.toThrow("file.txt:1:2 does not exist; for lines 1 to 2, write file.txt:1-2")
    await expect(f.engine.grep(f.ctx, { pattern: "one", paths: ["file.txt:1:2"] })).rejects.toThrow("file.txt:1:2 does not exist; for lines 1 to 2, write file.txt:1-2")
  })

  test("past 40 context lines on a page only definitions get theirs, and the other matches stay anchors", async () => {
    const f = await fixture()
    const pad = (from: number, count: number) => Array.from({ length: count }, (_, index) => `// pad ${from + index}`)
    const lines = [
      'import { formatSize } from "./size"',
      ...pad(2, 19),
      "export function formatSize(bytes: number) {",
      ...Array.from({ length: 14 }, (_, index) => `  const step${index} = bytes + ${index}`),
      "}",
      ...pad(37, 19),
      "const half = formatSize(512)",
      ...pad(57, 19),
      "function report() {",
      ...Array.from({ length: 18 }, (_, index) => index === 3 ? "  return formatSize(2048)" : `  const value${index} = ${index}`),
      "}",
    ]
    const source = lines.join("\n") + "\n"
    await writeFile(path.join(f.directory, "size.ts"), source)
    const row = (line: number) => `${[1, 21, 56, 80].includes(line) ? "*" : " "}${line}:${lines[line - 1]}`
    const rows = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, index) => row(from + index))
    const roomy = (await f.engine.grep(f.ctx, { pattern: "formatSize", paths: ["size.ts"], after: 1 })).text
    expect(roomy).toContain(`\n${rows(55, 57).join("\n")}\n`)
    expect(roomy).not.toContain("match rows shown")
    // An import, the definition, a use in a value and a use in a body; 3 lines before and 12 after each would be 57 context lines.
    expect((await f.engine.grep(f.ctx, { pattern: "formatSize", paths: ["size.ts"], before: 3, after: 12 })).text).toBe([
      "4 matches in 1 files",
      [`[size.ts#${fileTag(source)}]`, row(1), "...", ...rows(18, 33), "...", row(56), "...", row(80)].join("\n"),
      "[all 4 match rows shown, context lines only around the definition at size.ts:21: around all 4 they would be 57 lines, over grep's budget of 40; for another match's, read a range like size.ts:1-13]",
    ].join("\n\n"))
    await f.patch(`[size.ts#${fileTag(source)}]\nPUT 56:\n+const half = formatSize(1024)`)
    expect(await readFile(path.join(f.directory, "size.ts"), "utf8")).toBe(source.replace("formatSize(512)", "formatSize(1024)"))
  })

  test("with no definition among the matches the first ones get the context asked for, and the default gives none", async () => {
    // A hit every fifth line: 20 matches.
    const f = await fixture(Array.from({ length: 100 }, (_, index) => (index + 1) % 5 ? `miss ${index + 1}` : `hit ${index + 1}`).join("\n") + "\n")
    const asked = (await f.engine.grep(f.ctx, { pattern: "hit", before: 0, after: 5 })).text
    expect(asked).toContain("\n*50:hit 50\n 51:miss 51\n 52:miss 52\n 53:miss 53\n 54:miss 54\n*55:hit 55\n...\n*60:hit 60\n")
    expect(asked).toEndWith("\n\n[all 20 match rows shown, context lines only around the first 10: around all 20 they would be 76 lines, over grep's budget of 40; for another match's, read a range like file.txt:55-60]")
    // The default context goes to none of them once it would pass the budget: a hit every third line is 50 matches.
    const g = await fixture(Array.from({ length: 150 }, (_, index) => (index + 1) % 3 ? `miss ${index + 1}` : `hit ${index + 1}`).join("\n") + "\n")
    const plain = (await g.engine.grep(g.ctx, { pattern: "hit" })).text
    expect(plain).not.toContain("miss")
    expect(plain).toEndWith("\n\n[all 50 match rows shown, without context lines: around all 50 they would be 99 lines, over grep's budget of 40; ask with before or after for some around the first matches, or read a range like file.txt:2-4]")
    // hit 5 to hit 25 of the first file: few enough for the default context.
    expect((await f.engine.grep(f.ctx, { pattern: "hit ([5-9]|1[05]|2[05])$" })).text).toContain("\n 4:miss 4\n*5:hit 5\n 6:miss 6\n...\n 9:miss 9\n*10:hit 10\n")
  })

  test("a definition is a match naming what its line declares: after a declaring word, as a container member or as a Markdown heading", async () => {
    const f = await fixture()
    const pad = (count: number, text = "// pad") => Array.from({ length: count }, () => text)
    const files: Record<string, string[]> = {
      "sizes.ts": ["export class Sizes {", "  format(bytes: number) {", "    return bytes", "  }", "}", ...pad(25), "const a = format(1)", ...pad(25), "format(2)", ...pad(25), "function bytes(format: string) {", "  return format", "}", ...pad(25)],
      "server.go": ["package main", "", "func (s *Server) Handle(w Writer) {", "\ts.log(w)", "}", ...pad(25), "func run(s *Server) {", "\ts.Handle(nil)", "}", ...pad(25), "var handler = s.Handle", ...pad(25)],
      "notes.md": ["# Sizes", "", "## formatSize", "", ...pad(25, "text"), "Call formatSize for sizes.", ...pad(25, "text"), "See formatSize.", ...pad(25, "text")],
      "a.py": ["from b import format_size", "print(format_size(1))", ...pad(25, "# pad"), "value = format_size(2)", ...pad(25, "# pad")],
      "b.py": ["def format_size(bytes):", "    return bytes", ...pad(25, "# pad")],
    }
    for (const [name, lines] of Object.entries(files)) await writeFile(path.join(f.directory, name), lines.join("\n") + "\n")
    // 1 line before and 20 after: 21 context lines per match, so any two are over the budget.
    const grep = (pattern: string, paths: string[]) => f.engine.grep(f.ctx, { pattern, paths, before: 1, after: 20 })
    const given = async (pattern: string, paths: string[]) => /context lines only around (.+?): around all/.exec((await grep(pattern, paths)).text)?.[1]
    expect(await given("format", ["sizes.ts"])).toBe("the definition at sizes.ts:2")
    expect(await given("Handle", ["server.go"])).toBe("the definition at server.go:3")
    // A name in a Go receiver or in parameters is not what its line declares.
    expect(await given("Server", ["server.go"])).toBe("the first 1")
    expect(await given("formatSize", ["notes.md"])).toBe("the definition at notes.md:3")
    // The definition wins over matches in a file earlier on the page.
    const python = (await grep("format_size", ["a.py", "b.py"])).text
    expect(python).toContain("\n*1:from b import format_size\n*2:print(format_size(1))\n...\n*28:value = format_size(2)\n\n[b.py#")
    expect(python).toContain("\n*1:def format_size(bytes):\n 2:    return bytes\n 3:# pad\n")
    expect(python).toContain("context lines only around the definition at b.py:1:")
  })

  test("definitions keep all their context past the budget, also when the pattern matches their declaring word", async () => {
    const f = await fixture()
    // 30 functions, each under a doc comment or a blank line: 2 lines before each come to 59 context lines.
    const lines = Array.from({ length: 30 }, (_, index) => [index % 2 ? `/** Doc ${index}. */` : "", `function f${index}() {`, `  return ${index}`, "}"]).flat()
    await writeFile(path.join(f.directory, "fns.ts"), lines.join("\n") + "\n")
    const text = (await f.engine.grep(f.ctx, { pattern: "^function ", paths: ["fns.ts"], before: 2, after: 0 })).text
    expect(text).toStartWith(`30 matches in 1 files\n\n[fns.ts#${fileTag(lines.join("\n") + "\n")}]\n 1:\n*2:function f0() {\n...\n 4:}\n 5:/** Doc 1. */\n*6:function f1() {\n`)
    expect(text).toEndWith("\n...\n 116:}\n 117:/** Doc 29. */\n*118:function f29() {")
  })

  test("past the budget, definitions that did not ask for context share it and the rest stay a list", async () => {
    const f = await fixture()
    const lines = Array.from({ length: 30 }, (_, index) => [index % 2 ? `/** Doc ${index}. */` : "", `function f${index}() {`, `  return ${index}`, "}"]).flat()
    await writeFile(path.join(f.directory, "fns.ts"), lines.join("\n") + "\n")
    const text = (await f.engine.grep(f.ctx, { pattern: "^function ", paths: ["fns.ts"] })).text
    // One line before and one after: each definition comes to 2 lines, so 20 fit in 40.
    expect(text).toContain("\n*50:function f12() {\n 51:  return 12\n...\n 53:/** Doc 13. */\n*54:function f13() {\n")
    expect(text).toEndWith("\n\n[all 30 match rows shown, context lines only around the first 20 of 30 definitions: around all 30 they would be 60 lines, over grep's budget of 40; for another match's, read a range like fns.ts:81-83]")
  })

  test("one file shows its first 100 matches, like a harness's own grep", async () => {
    const f = await fixture(Array.from({ length: 150 }, (_, index) => `hit ${index + 1}`).join("\n") + "\n")
    const text = (await f.engine.grep(f.ctx, { pattern: "hit", paths: ["file.txt"], before: 0, after: 0 })).text
    expect(text).toStartWith("150 matches in 1 files")
    expect([...text.matchAll(/^\*\d+:/gm)]).toHaveLength(100)
    expect(text).toContain("*100:hit 100\n[50 more matches in this file]")
  })

  test("grep locks its page's files together without deadlocking against an edit or a file named twice", async () => {
    const f = await fixture("one\ntwo\n")
    await writeFile(path.join(f.directory, "b.txt"), "one\n")
    await symlink(f.filePath, path.join(f.directory, "link.txt"))
    const { tag } = await f.read()
    const [grepped] = await Promise.all([
      f.engine.grep(f.ctx, { pattern: "one", paths: ["file.txt", "link.txt", "b.txt"] }),
      f.patch(`[file.txt#${tag}]\nPUT 2:\n+TWO`),
    ])
    expect(grepped.text).toStartWith("3 matches in 3 files")
    expect(await readFile(f.filePath, "utf8")).toBe("one\nTWO\n")
  })

  test("files and count modes list matching files without rows, 200 per page", async () => {
    const f = await fixture("hit\nhit\nmiss\n")
    await mkdir(path.join(f.directory, "many"))
    for (let index = 0; index < 205; index++) await writeFile(path.join(f.directory, "many", `f${String(index).padStart(3, "0")}.txt`), "hit\n")
    const parts = (await f.engine.grep(f.ctx, { pattern: "hit", mode: "files" })).text.split("\n\n")
    expect(parts[0]).toBe("207 matches in 206 files")
    // The folder of 199 files on this page is named once, its files indented under it.
    expect(parts[1]!.split("\n")).toHaveLength(201)
    expect(parts[1]!.split("\n").slice(0, 4)).toEqual(["file.txt", "many/ (199)", "  f000.txt", "  f001.txt"])
    expect(parts[2]).toBe("[206 matching files; continue with skip 200]")
    expect((await f.engine.grep(f.ctx, { pattern: "hit", mode: "files", skip: 200 })).text.split("\n\n").slice(0, 2)).toEqual(["207 matches in 206 files under many/", "f199.txt\nf200.txt\nf201.txt\nf202.txt\nf203.txt\nf204.txt"])
    expect((await f.engine.grep(f.ctx, { pattern: "hit", mode: "count", paths: ["file.txt", "many/f000.txt"] })).text).toBe("3 matches in 2 files\n\n2 file.txt\n1 many/f000.txt")
  })

  test("a count over many files sums them by folder, at the deepest level that fits, most matches first", async () => {
    const f = await fixture("hit\nhit\nmiss\n")
    for (const [folder, count] of [["src/a", 30], ["src/b", 12]] as const) {
      for (let index = 0; index < count; index++) {
        await mkdir(path.join(f.directory, folder, `x${index}`), { recursive: true })
        await writeFile(path.join(f.directory, folder, `x${index}`, "f.txt"), "hit\n")
      }
    }
    await mkdir(path.join(f.directory, "docs"))
    for (let index = 0; index < 3; index++) await writeFile(path.join(f.directory, "docs", `f${index}.txt`), "hit\n")
    expect((await f.engine.grep(f.ctx, { pattern: "hit", mode: "count" })).text).toBe([
      "47 matches in 46 files, by folder",
      "30 src/a/ (30 files)\n12 src/b/ (12 files)\n2 file.txt\n1 docs/f0.txt\n1 docs/f1.txt\n1 docs/f2.txt",
      "[grep one folder for its files' counts]",
    ].join("\n\n"))
    // skip still pages the files one by one.
    expect((await f.engine.grep(f.ctx, { pattern: "hit", mode: "count", skip: 44 })).text).toBe("47 matches in 46 files under src/b/\n\n1 x8/f.txt\n1 x9/f.txt")
  })

  test("a count that cannot be split by folder pages the files; a folder's own count splits below it", async () => {
    const f = await fixture("miss\n")
    await mkdir(path.join(f.directory, "src/a"), { recursive: true })
    await mkdir(path.join(f.directory, "src/b"))
    for (let index = 0; index < 41; index++) await writeFile(path.join(f.directory, "src", `f${String(index).padStart(2, "0")}.txt`), "hit\n")
    // Every match sits in src/, so a sum by folder would be one row: the files are listed instead, from the root and from src alike.
    for (const paths of [undefined, ["src"]]) {
      const text = (await f.engine.grep(f.ctx, { pattern: "hit", mode: "count", paths })).text
      expect(text).toStartWith("41 matches in 41 files under src/\n\n1 f00.txt\n1 f01.txt")
      expect(text).not.toContain("by folder")
    }
    for (let index = 0; index < 20; index++) {
      await writeFile(path.join(f.directory, "src/a", `g${index}.txt`), "hit\n")
      await writeFile(path.join(f.directory, "src/b", `g${index}.txt`), "hit\n")
    }
    await rm(path.join(f.directory, "src"), { recursive: true, force: true }).then(() => mkdir(path.join(f.directory, "src/a"), { recursive: true }))
    await mkdir(path.join(f.directory, "src/b"))
    for (let index = 0; index < 30; index++) await writeFile(path.join(f.directory, "src/a", `g${index}.txt`), "hit\n")
    for (let index = 0; index < 12; index++) await writeFile(path.join(f.directory, "src/b", `g${index}.txt`), "hit\n")
    expect((await f.engine.grep(f.ctx, { pattern: "hit", mode: "count", paths: ["src"] })).text).toBe("42 matches in 42 files, by folder\n\n30 src/a/ (30 files)\n12 src/b/ (12 files)\n\n[grep one folder for its files' counts]")
  })

  test("noIgnore searches ignored files, and glob takes a list", async () => {
    const f = await fixture("alpha\n")
    await mkdir(path.join(f.directory, ".git"))
    await writeFile(path.join(f.directory, ".gitignore"), "build/\n")
    await mkdir(path.join(f.directory, "build"))
    await writeFile(path.join(f.directory, "build/out.txt"), "alpha\n")
    await writeFile(path.join(f.directory, "notes.md"), "alpha\n")
    expect((await f.engine.grep(f.ctx, { pattern: "alpha", mode: "files" })).text).toBe("2 matches in 2 files\n\nfile.txt\nnotes.md")
    expect((await f.engine.grep(f.ctx, { pattern: "alpha", mode: "files", noIgnore: true })).text).toBe("3 matches in 3 files\n\nbuild/out.txt\nfile.txt\nnotes.md")
    expect((await f.engine.grep(f.ctx, { pattern: "alpha", mode: "files", noIgnore: true, glob: ["!build/**", "!*.md"] })).text).toBe("1 matches in 1 files\n\nfile.txt")
  })

  test("a path's line selector limits matches and context to those lines", async () => {
    const content = Array.from({ length: 20 }, (_, index) => index % 2 ? `ERR ${index + 1}` : `ok ${index + 1}`).join("\n") + "\n"
    const f = await fixture(content)
    expect((await f.engine.grep(f.ctx, { pattern: "ERR", paths: ["file.txt:5-8"] })).text).toBe(`2 matches in 1 files\n\n[file.txt#${fileTag(content)}]\n 5:ok 5\n*6:ERR 6\n 7:ok 7\n*8:ERR 8`)
    expect((await f.engine.grep(f.ctx, { pattern: "ERR", paths: ["file.txt:-3"], mode: "count" })).text).toBe("2 matches in 1 files\n\n2 file.txt")
    expect((await f.engine.grep(f.ctx, { pattern: "ERR", paths: ["file.txt:15-"], mode: "count" })).text).toBe("3 matches in 1 files\n\n3 file.txt")
    expect((await f.engine.grep(f.ctx, { pattern: "ERR", paths: ["file.txt:5-8", "file.txt"], mode: "count" })).text).toBe("10 matches in 1 files\n\n10 file.txt")
    expect((await f.engine.grep(f.ctx, { pattern: "ERR", paths: ["file.txt:3"] })).text).toBe("No matches found")
    await expect(f.engine.grep(f.ctx, { pattern: "ERR", paths: [".:1-5"] })).rejects.toThrow("line selectors need a file")
  })

  test("maxLineLength cuts grep rows, which then are not anchors", async () => {
    const content = "needle " + "x".repeat(50) + "\nneedle\n"
    const f = await fixture(content)
    const result = await f.engine.grep(f.ctx, { pattern: "needle", maxLineLength: 20 })
    expect(result.text).toBe(`2 matches in 1 files\n\n[file.txt#${fileTag(content)}]\n*[1 cut at 20 of 57 chars]|needle ${"x".repeat(13)}\n*2:needle\n[cut rows: change them in place with SUB N: OLD, or read file.txt:N with maxLineLength to see one whole]`)
    await expect(f.patch(`[file.txt#${fileTag(content)}]\nPUT 1.=1:\n+x`)).rejects.toThrow("never displayed in full")
  })
})

describe("read options", () => {
  test("a row larger than a whole call's output points to reading that line alone", async () => {
    const f = await fixture("y".repeat(50_000) + "\nshort\n")
    const r = await f.read(":1-2", { maxLineLength: 100_000 })
    expect(r.text).toBe(`[file.txt#${r.tag}]\n[line 1 has 50000 chars, more than one call's output; read file.txt:1 alone]`)
    await writeFile(f.filePath, "word ".repeat(10_000) + "\n" + numbered(200))
    expect((await f.read("", { maxLineLength: 100_000 })).text).toEndWith("\n[line 1 has 50000 chars, more than one call's output; read file.txt:1 alone]")
    await writeFile(f.filePath, "y".repeat(50_000) + "\nshort\n")
    expect((await f.read(":1", { maxLineLength: 100_000 })).anchors).toEqual([1])
  })
})

describe("moved lines after an edit", () => {
  test("lines an edit moved get one note with their old numbers; the kept rows around the change stay anchors", async () => {
    const f = await fixture(numbered(1000))
    const r = await f.read(":1-")
    const edited = await f.edit(r.tag, [{ start: 20, content: "a\nb\nc\nd", operation: "after" }])
    expect(edited.text.split("\n")).toEqual([`[file.txt#${edited.tag}]`, "20:line 20", "21:a", "22:b", "23:c", "24:d", "25:line 21", "[lines 26-1004 are old lines 22-1000, moved down by 4]", movedLesson])
    const unchecked = await refusal(f.patch(`[file.txt#${edited.tag}]\nPUT 500.=500:\n+x`))
    expect(unchecked).toContain("This edit anchors to lines 500 of file.txt")
    expect(unchecked).toContain("Lines you wrote, or saw before an edit moved them, can skip this step")
    await f.patch(`[file.txt#${edited.tag}]\nPUT 25.=25:\n+LINE 21`)
    expect((await readFile(f.filePath, "utf8")).split("\n")[24]).toBe("LINE 21")
  })

  test("a moved line the agent saw is edited with a check from its old number plus the offset", async () => {
    const f = await fixture(numbered(1000))
    const edited = await f.edit((await f.read(":1-")).tag, [{ start: 20, content: "a\nb\nc\nd", operation: "after" }])
    await f.patch(`[file.txt#${edited.tag}]\nPUT 500.=500:\n~line 496\n+LINE 496`)
    expect((await readFile(f.filePath, "utf8")).split("\n")[499]).toBe("LINE 496")
  })

  test("the moved-line lesson comes once per agent", async () => {
    const f = await fixture(numbered(100))
    const first = await f.edit((await f.read(":1-")).tag, [{ start: 10, content: "a", operation: "after" }])
    expect(first.text).toEndWith(movedLesson)
    const second = await f.edit(first.tag, [{ start: 12, content: "b", operation: "after" }])
    expect(second.text).toContain("moved down by 1]")
    expect(second.text).not.toContain(movedLesson)
  })

  test("deleted lines move the rest up, and an edit that moves nothing gets no map", async () => {
    const f = await fixture(numbered(1000))
    const cut = await f.edit((await f.read(":1-")).tag, [{ start: 10, end: 13, content: "", operation: "replace" }])
    expect(cut.text).toBe(`[file.txt#${cut.tag}]\n9:line 9\n10:line 14\n[lines 11-996 are old lines 15-1000, moved up by 4]\n${movedLesson}`)
    const same = await f.edit(cut.tag, [{ start: 5, content: "replaced", operation: "replace" }])
    expect(same.text).toBe(`[file.txt#${same.tag}]\n4:line 4\n5:replaced\n6:line 6`)
  })

  test("a moved stretch gets its note when it holds a line the agent saw, and a check lets an insert there but not a replace", async () => {
    const f = await fixture(numbered(100))
    const edited = await f.edit((await f.read(":1-5")).tag, [{ start: 2, content: "x", operation: "after" }])
    // Old lines 4 and 5 were read, so the stretch they moved in gets a note; the lines past them were never seen.
    expect(edited.text).toBe(`[file.txt#${edited.tag}]\n2:line 2\n3:x\n4:line 3\n[lines 5-101 are old lines 4-100, moved down by 1]\n${movedLesson}`)
    expect(await refusal(f.patch(`[file.txt#${edited.tag}]\nPUT 15.=15:\n~line 14\n+y`))).toContain("never displayed in full")
    await f.patch(`[file.txt#${edited.tag}]\nPUT >15:\n~line 14\n+after fourteen`)
    expect((await readFile(f.filePath, "utf8")).split("\n")[15]).toBe("after fourteen")
  })
})

describe("color codes", () => {
  test("lines with color codes show stripped as rows that are not anchors, and SUB still edits them", async () => {
    const content = "plain line\n\x1b[31mERROR\x1b[0m disk full\n"
    const f = await fixture(content)
    const r = await f.read()
    expect(r.text).toBe(`[file.txt#${fileTag(content)}]\n1:plain line\n[2 ansi]|ERROR disk full\n[ansi rows: their color codes are stripped, so they are not anchors; change them in place with SUB N: OLD]`)
    expect(r.anchors).toEqual([1])
    const subbed = await f.patch(`[file.txt#${r.tag}]\nSUB 2: disk full\n+disk ok`)
    expect(await readFile(f.filePath, "utf8")).toBe("plain line\n\x1b[31mERROR\x1b[0m disk ok\n")
    expect(subbed.text).toContain("\n[2 ansi]|ERROR disk ok\n")
    const replaced = await refusal(f.patch(`[file.txt#${subbed.tag}]\nPUT 2.=2:\n+x`))
    expect(replaced).toContain("Preview of the first 1 unseen line(s)")
    expect(replaced).not.toContain("REAPPLY")
    expect((await f.engine.grep(f.ctx, { pattern: "ERROR" })).text).toContain("*[2 ansi]|ERROR disk ok")
  })
})

describe("plain reads", () => {
  const content = ["start", "\x1b[32mok\x1b[0m step", "same", "same", "same", "same", "pair", "pair", "x".repeat(50), "end"].join("\n") + "\n"

  test("plain rows carry no numbers or tag: colors stripped, long lines shortened, repeats collapsed, gaps named", async () => {
    const f = await fixture(content)
    const plain = (paths: string[], maxLineLength?: number) => f.engine.read(f.ctx, { paths, plain: true, maxLineLength }).then((result) => result.text)
    expect(await plain(["file.txt"], 20)).toBe(["[file.txt] 10 lines, plain", "start", "ok step", "same", "[the line above repeats 3 more times]", "pair", "pair", "x".repeat(20) + "...", "end"].join("\n"))
    expect(await plain(["file.txt:1-2,9-10"], 20)).toBe(["[file.txt] lines 1-2, 9-10 of 10, plain", "start", "ok step", "[lines 3-8 skipped]", "x".repeat(20) + "...", "end"].join("\n"))
    expect(await plain(["file.txt:-2"])).toBe(["[file.txt] lines 9-10 of 10, plain", "x".repeat(50), "end"].join("\n"))
    expect(await plain(["file.txt:3-"])).toBe(["[file.txt] lines 3-10 of 10, plain", "same", "[the line above repeats 3 more times]", "pair", "pair", "x".repeat(50), "end"].join("\n"))
    await writeFile(path.join(f.directory, "pixel.png"), Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))
    expect((await f.engine.read(f.ctx, { paths: ["pixel.png"], plain: true })).images).toHaveLength(1)
    await writeFile(path.join(f.directory, "blob.bin"), Buffer.from([1, 0, 2]))
    await expect(plain(["blob.bin"])).rejects.toThrow("Binary files are not supported")
  })

  test("repeats collapse with numbers ignored: a run, a cadence, and a block that comes back like a stack trace", async () => {
    const lines = [
      "boot",
      ...Array.from({ length: 5 }, (_, index) => `12:00:0${index} heartbeat ok`),
      ...Array.from({ length: 4 }, (_, index) => [`12:01:0${index} poll queue`, `12:01:0${index} poll done in ${index} ms`]).flat(),
      "ERROR queue closed", "\tat com.acme.Queue.take(Queue.java:88)", "\tat com.acme.Worker.run(Worker.java:12)", "\tat java.lang.Thread.run(Thread.java:840)",
      "INFO reconnecting",
      "ERROR queue closed", "\tat com.acme.Queue.take(Queue.java:91)", "\tat com.acme.Worker.run(Worker.java:12)", "\tat java.lang.Thread.run(Thread.java:840)",
      "same", "same", "same",
      "done",
    ]
    const f = await fixture(lines.join("\n") + "\n")
    expect((await f.engine.read(f.ctx, { paths: ["file.txt"], plain: true })).text).toBe([
      "[file.txt] 27 lines, plain",
      "boot",
      "12:00:00 heartbeat ok",
      "[4 more like the line above; the last one:]",
      "12:00:04 heartbeat ok",
      "12:01:00 poll queue",
      "12:01:00 poll done in 0 ms",
      "[3 more like the 2 lines above; the last ones:]",
      "12:01:03 poll queue",
      "12:01:03 poll done in 3 ms",
      "[#1, 4 lines] ERROR queue closed",
      "\tat com.acme.Queue.take(Queue.java:88)",
      "\tat com.acme.Worker.run(Worker.java:12)",
      "\tat java.lang.Thread.run(Thread.java:840)",
      "INFO reconnecting",
      "[#1 again, 4 lines] ERROR queue closed",
      "same",
      "[the line above repeats 2 more times]",
      "done",
      "[repeats were matched with their numbers ignored; read with collapse: false to see every line]",
    ].join("\n"))
    expect((await f.engine.read(f.ctx, { paths: ["file.txt"], plain: true, collapse: false })).text).toBe(["[file.txt] 27 lines, plain", ...lines].join("\n"))
    // Three similar lines print the same number of rows collapsed or not, so they stay as they are.
    await writeFile(f.filePath, "1 tick\n2 tick\n3 tick\n")
    expect((await f.engine.read(f.ctx, { paths: ["file.txt"], plain: true })).text).toBe("[file.txt] 3 lines, plain\n1 tick\n2 tick\n3 tick")
  })

  test("a recurring block ends where a back-to-back repeat begins, so a service log collapses structure by structure", async () => {
    const log: string[] = []
    const time = (second: number) => `2026-10-14 12:${String(Math.floor(second / 60)).padStart(2, "0")}:${String(second % 60).padStart(2, "0")}`
    let second = 0
    for (let round = 0; round < 2; round++) {
      for (let beat = 0; beat < 6; beat++, second += 5) log.push(`${time(second)} \x1b[32mINFO\x1b[0m heartbeat ok`, `${time(second)} INFO polled queue: 0 jobs`)
      log.push(`${time(second)} ERROR worker crashed`, "java.lang.IllegalStateException: queue closed", `\tat com.acme.Queue.take(Queue.java:${88 + round})`, "\tat java.lang.Thread.run(Thread.java:840)")
      for (let retry = 0; retry < 4; retry++) log.push(`${time(second + retry)} WARN retrying in ${retry + 1}s`)
      second += 10
    }
    const f = await fixture(log.join("\n") + "\n")
    expect((await f.engine.read(f.ctx, { paths: ["file.txt"], plain: true })).text.split("\n")).toEqual([
      "[file.txt] 40 lines, plain",
      "2026-10-14 12:00:00 INFO heartbeat ok",
      "2026-10-14 12:00:00 INFO polled queue: 0 jobs",
      "[5 more like the 2 lines above; the last ones:]",
      "2026-10-14 12:00:25 INFO heartbeat ok",
      "2026-10-14 12:00:25 INFO polled queue: 0 jobs",
      "[#1, 4 lines] 2026-10-14 12:00:30 ERROR worker crashed",
      "java.lang.IllegalStateException: queue closed",
      "\tat com.acme.Queue.take(Queue.java:88)",
      "\tat java.lang.Thread.run(Thread.java:840)",
      "2026-10-14 12:00:30 WARN retrying in 1s",
      "[3 more like the line above; the last one:]",
      "2026-10-14 12:00:33 WARN retrying in 4s",
      "2026-10-14 12:00:40 INFO heartbeat ok",
      "2026-10-14 12:00:40 INFO polled queue: 0 jobs",
      "[5 more like the 2 lines above; the last ones:]",
      "2026-10-14 12:01:05 INFO heartbeat ok",
      "2026-10-14 12:01:05 INFO polled queue: 0 jobs",
      "[#1 again, 4 lines] 2026-10-14 12:01:10 ERROR worker crashed",
      "2026-10-14 12:01:10 WARN retrying in 1s",
      "[3 more like the line above; the last one:]",
      "2026-10-14 12:01:13 WARN retrying in 4s",
      "[repeats were matched with their numbers ignored; read with collapse: false to see every line]",
    ])
  })

  test("plain reads files over 2 MiB by range and tail, and continues past the output budget", async () => {
    const f = await fixture()
    const line = (index: number) => `2026-10-14 entry ${index} ${"y".repeat(60)}`
    await writeFile(path.join(f.directory, "big.log"), Array.from({ length: 40_000 }, (_, index) => line(index + 1)).join("\n") + "\n")
    // A normal read notes the oversized file instead of failing, so other files in the same call still come back.
    const both = (await f.engine.read(f.ctx, { paths: ["big.log:39999-", "file.txt"] })).text.split("\n\n")
    expect(both[0]).toBe("[big.log] skipped: 3.2 MiB, over the 2 MiB text limit. Read it with plain: true (a range like big.log:-200 works too), or grep it.")
    expect(both[1]).toStartWith("[file.txt#")
    const plain = (entry: string, collapse?: boolean) => f.engine.read(f.ctx, { paths: [entry], plain: true, collapse }).then((result) => result.text)
    expect(await plain("big.log:-2")).toBe(`[big.log] lines 39999-40000 of 40000, plain\n${line(39999)}\n${line(40000)}`)
    expect(await plain("big.log:20000-20001")).toBe(`[big.log] lines 20000-20001 of 40000, plain\n${line(20000)}\n${line(20001)}`)
    // Every line has the same shape with other numbers, so by default 2000 of them collapse into three rows.
    expect(await plain("big.log:100-")).toBe([
      "[big.log] lines 100-2099 of 40000, plain",
      line(100),
      "[1999 more like the line above; the last one:]",
      line(2099),
      "[continue with big.log:2100-]",
      "[repeats were matched with their numbers ignored; read with collapse: false to see every line]",
    ].join("\n"))
    const open = await plain("big.log:100-", false)
    expect(open.split("\n").slice(1, 3)).toEqual([line(100), line(101)])
    expect(open).toMatch(/\n\[output limit reached; continue with big\.log:\d+-\]$/)
    expect(Buffer.byteLength(open)).toBeLessThan(41 * 1024)
    // A tail longer than the 2000-line cap counts lines first, then keeps the start of that window.
    const long = await plain("big.log:-2500", false)
    expect(long.split("\n").slice(0, 2)).toEqual(["[big.log] lines 37501-40000 of 40000, plain", line(37501)])
    expect(long).toMatch(/\n\[output limit reached; continue with big\.log:\d+-\]$/)
    expect((await plain("big.log:-2500")).split("\n").slice(-3)).toEqual([line(39500), "[continue with big.log:39501-]", "[repeats were matched with their numbers ignored; read with collapse: false to see every line]"])
  })

  test("plain reads never refuse a log that grows while they run", async () => {
    const f = await fixture()
    const log = path.join(f.directory, "live.log")
    await writeFile(log, numbered(50_000, "boot"))
    const appender = await open(log, "a")
    let running = true
    const writing = (async () => { while (running) await appender.write("more\n") })()
    try {
      for (let attempt = 0; attempt < 5; attempt++) {
        expect((await f.engine.read(f.ctx, { paths: ["live.log:1-3"], plain: true })).text).toMatch(/^\[live\.log\] lines 1-3 of \d+, plain\nboot 1\nboot 2\nboot 3$/)
      }
    } finally {
      running = false
      await writing
      await appender.close()
    }
  })
})

describe("grep on files nib cannot hold whole", () => {
  test("a file over 2 MiB gets numbered rows from ripgrep: untagged, colors stripped, shortened, inside the selected lines", async () => {
    const f = await fixture()
    const line = (index: number) => index === 30_000 ? `\x1b[31mERROR\x1b[0m entry ${index} ${"z".repeat(60)}` : `info entry ${index} ${"y".repeat(60)}`
    await writeFile(path.join(f.directory, "big.log"), Array.from({ length: 40_000 }, (_, index) => line(index + 1)).join("\n") + "\n")
    const result = await f.engine.grep(f.ctx, { pattern: "ERROR", paths: ["big.log"], before: 1, after: 1, maxLineLength: 30 })
    expect(result.text).toBe("1 matches in 1 files\n\n" + [
      "[big.log] (not anchorable: over 2 MiB; rows from ripgrep)",
      ` 29999|info entry 29999 ${"y".repeat(13)}...`,
      `*30000|ERROR entry 30000 ${"z".repeat(12)}...`,
      ` 30001|info entry 30001 ${"y".repeat(13)}...`,
      "[to read around a match, use read with plain: true and a range, like big.log:29990-30010]",
    ].join("\n"))
    expect((await f.engine.grep(f.ctx, { pattern: "ERROR", paths: ["big.log:1-100"] })).text).toBe("No matches found")
    expect((await f.engine.grep(f.ctx, { pattern: "ERROR", paths: ["big.log:29999-30000"], after: 5 })).text).toContain("\n 29999|info entry 29999")
    expect((await f.engine.grep(f.ctx, { pattern: "ERROR", paths: ["big.log:29999-30000"], after: 5 })).text).not.toContain("30001|")
  })

  test("a file that changes while grep reads it is never skipped", async () => {
    const f = await fixture()
    const log = path.join(f.directory, "live.log")
    await writeFile(log, numbered(50_000, "boot"))
    const appender = await open(log, "a")
    let running = true
    const writing = (async () => { while (running) await appender.write("more\n") })()
    try {
      for (let attempt = 0; attempt < 5; attempt++) {
        const result = await f.engine.grep(f.ctx, { pattern: "^boot 1$", paths: ["live.log"] })
        expect(result.text).not.toContain("skipped")
        expect(result.text).toMatch(/\n\*1[:|]boot 1\n/)
      }
    } finally {
      running = false
      await writing
      await appender.close()
    }
  })
})

describe("minified files", () => {
  const code = (count: number) => Array.from({ length: count }, (_, index) => `function a${index}(b,c){return b+c*${index}}var x${index}=a${index}(1,2);`).join("")

  test("whole reads skip minified files by name or by dense long lines, but not prose or date-named files", async () => {
    const f = await fixture()
    const header = Array.from({ length: 100 }, (_, index) => ` * License line ${index} of the very long license header text here`).join("\n")
    await writeFile(path.join(f.directory, "licensed.js"), header + "\n" + code(500) + "\n")
    await writeFile(path.join(f.directory, "app.bundle.js"), "export const a = 1\n".repeat(300))
    await writeFile(path.join(f.directory, "main.3f9a2c1b.js"), "export const a = 1\n".repeat(300))
    await writeFile(path.join(f.directory, "migration-20261014.js"), "export const a = 1\n".repeat(300))
    await writeFile(path.join(f.directory, "notes.md"), Array.from({ length: 6 }, () => "A paragraph written on one line, as Markdown often is. ".repeat(30)).join("\n\n") + "\n")
    const parts = (await f.engine.read(f.ctx, { paths: ["licensed.js", "app.bundle.js", "main.3f9a2c1b.js", "migration-20261014.js", "notes.md"] })).text.split("\n\n")
    expect(parts[0]).toStartWith("[licensed.js] skipped: minified (most of it in long, dense lines), 101 lines")
    expect(parts[1]).toStartWith("[app.bundle.js] skipped: bundled or minified")
    expect(parts[2]).toStartWith("[main.3f9a2c1b.js] skipped: bundled or minified")
    expect(parts[3]).toStartWith("[migration-20261014.js#")
    expect(parts[4]).toStartWith("[notes.md#")
    expect(parts[4]).toContain("\n1:A paragraph written on one line, as Markdown often is.")
  })

  test("a range read of a minified file cuts lines at 300 chars unless maxLineLength is given", async () => {
    const f = await fixture(code(2000) + "\n" + code(10) + "\n")
    const r = await f.read(":1-")
    expect(r.text.split("\n").slice(0, 2)).toEqual([
      `[file.txt#${r.tag}] minified, so lines are cut at 300 chars; pass maxLineLength for more`,
      `[1 cut at 300 of ${code(2000).length} chars]|${code(2000).slice(0, 300)}`,
    ])
    const wide = await f.read(":1", { maxLineLength: 1000 })
    expect(wide.text.split("\n").slice(0, 2)).toEqual([`[file.txt#${wide.tag}]`, `[1 cut at 1000 of ${code(2000).length} chars]|${code(2000).slice(0, 1000)}`])
  })

  test("grep shows the stretches around matches deep in a long line, also for files over 2 MiB", async () => {
    const line = "a".repeat(3000) + "NEEDLE" + "b".repeat(3000) + "NEEDLE" + "c".repeat(50)
    const f = await fixture(line + "\n")
    expect((await f.engine.grep(f.ctx, { pattern: "NEEDLE" })).text).toContain(`\n*[1 chars 2916-3091, 5922-6062 of 6062]|${"a".repeat(85)}NEEDLE${"b".repeat(85)} ... ${"b".repeat(85)}NEEDLE${"c".repeat(50)}\n`)
    await writeFile(path.join(f.directory, "huge.js"), "q".repeat(1_500_000) + "NEEDLE" + "r".repeat(800_000) + "\n")
    expect((await f.engine.grep(f.ctx, { pattern: "NEEDLE", paths: ["huge.js"] })).text).toContain(`\n*1|[chars 1499916-1500091 of 2300006] ${"q".repeat(85)}NEEDLE${"r".repeat(85)}\n`)
  })
})

const NOW = Date.now() / 1000

/** Sets a file's modification time to `seconds` before the tests started, so ages and orders in output are exact. */
async function age(file: string, seconds: number) {
  await utimes(file, NOW - seconds, NOW - seconds)
}

const DAYS = 3 * 86400 + 60

describe("file facts (read info)", () => {
  test("one line of facts per file; its tag guards edits like a read that showed no lines", async () => {
    const content = `${BOM}one\r\n\x1b[31mtwo\x1b[0m\r\nthree`
    const f = await fixture(content)
    await age(f.filePath, DAYS)
    const tag = fileTag(content.slice(1))
    const info = await f.engine.read(f.ctx, { paths: ["file.txt"], info: true })
    expect(info.text).toBe(`[file.txt#${tag}] 3 lines, 27 bytes, about 7 tokens; longest line 12 chars (line 2); CRLF; no final newline; BOM; color codes; modified 3d ago`)
    await expect(f.patch(`[file.txt#${tag}]\nPUT 3.=3:\n+THREE`)).rejects.toThrow("never displayed in full")
    await f.patch("[file.txt]\nPUT >$:\n+four")
    expect(await readFile(f.filePath, "utf8")).toBe(`${BOM}one\r\n\x1b[31mtwo\x1b[0m\r\nthree\r\nfour`)
  })

  test("images, binary, non-UTF-8, empty, generated and mixed-ending files, and directories", async () => {
    const f = await fixture()
    const files: [string, string | Buffer][] = [
      ["pixel.png", Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])],
      ["blob.bin", Buffer.from([0, 1, 2, 3])],
      ["latin1.txt", Buffer.from([0x63, 0x61, 0x66, 0xe9, 0x0a])],
      ["empty.txt", ""],
      ["app.min.js", "var a=1\n"],
      ["package-lock.json", "{}\n"],
      ["mixed.txt", "a\r\nb\nc\n"],
      ["dir/one.txt", "1\n"],
      ["dir/sub/two.txt", "2\n"],
    ]
    await mkdir(path.join(f.directory, "dir/sub"), { recursive: true })
    for (const [name, content] of files) await writeFile(path.join(f.directory, name), content)
    for (const [name] of [...files, ["dir"], ["dir/sub"]]) await age(path.join(f.directory, name!), DAYS)
    const text = (await f.engine.read(f.ctx, { paths: [...files.slice(0, 7).map(([name]) => name), "dir"], info: true })).text
    expect(text.split("\n\n")).toEqual([
      "[pixel.png] image/png, 8 bytes; modified 3d ago",
      "[blob.bin] binary, 4 bytes; modified 3d ago",
      "[latin1.txt] not UTF-8 text, 5 bytes; modified 3d ago",
      `[empty.txt#${fileTag("")}] empty; modified 3d ago`,
      `[app.min.js#${fileTag("var a=1\n")}] 1 line, 8 bytes, about 2 tokens; longest line 7 chars; LF; minified or source map; modified 3d ago`,
      `[package-lock.json#${fileTag("{}\n")}] 1 line, 3 bytes, about 1 token; longest line 2 chars; LF; lockfile; modified 3d ago`,
      `[mixed.txt#${fileTag("a\r\nb\nc\n")}] 3 lines, 7 bytes, about 2 tokens; longest line 1 char (line 1); mixed line endings (1 CRLF, 2 LF); modified 3d ago`,
      "dir/ directory, 2 entries (1 directory); modified 3d ago",
    ])
  })

  test("a file over 2 MiB is streamed and gets no tag; info takes no selector, plain or since", async () => {
    const f = await fixture()
    await writeFile(path.join(f.directory, "big.log"), "line\n".repeat(500_000))
    await age(path.join(f.directory, "big.log"), DAYS)
    expect((await f.engine.read(f.ctx, { paths: ["big.log"], info: true })).text)
      .toBe("[big.log] 500000 lines, 2.4 MiB, about 625k tokens; longest line 4 chars (line 1); LF; over 2 MiB, so no tag: read it with plain: true, or grep it; modified 3d ago")
    await expect(f.engine.read(f.ctx, { paths: ["file.txt:1-2"], info: true })).rejects.toThrow("info describes whole files")
    await expect(f.engine.read(f.ctx, { paths: ["file.txt"], info: true, plain: true })).rejects.toThrow("takes no plain, collapse or maxLineLength")
    await expect(f.engine.read(f.ctx, { paths: ["file.txt#ABCD"], info: true })).rejects.toThrow("path#TAG shows what changed since that tag")
  })
})

describe("changes since a tag (read path#TAG)", () => {
  const lines = (count: number) => Array.from({ length: count }, (_, index) => `l${index + 1}`)

  test("added, removed and context rows numbered as the file is now, with notes for moved lines the agent knew", async () => {
    const f = await fixture(lines(20).join("\n") + "\n")
    const first = await f.read()
    const changed = lines(20).map((line) => line === "l5" ? "FIVE" : line).filter((line) => line !== "l12").concat("l21")
    await writeFile(f.filePath, changed.join("\n") + "\n")
    const now = fileTag(changed.join("\n") + "\n")
    expect((await f.read(`#${first.tag}`)).text).toBe([
      `[file.txt#${now}] changed since #${first.tag}: 2 lines added, 2 removed`,
      " 3:l3", " 4:l4", "-|l5", "+5:FIVE", " 6:l6", " 7:l7", "...",
      " 10:l10", " 11:l11", "-|l12", " 12:l13", " 13:l14", "...",
      " 18:l19", " 19:l20", "+20:l21",
      "[lines 12-19 are old lines 13-20, moved up by 1]",
    ].join("\n"))
    expect((await f.read(`#${now}`)).text).toBe(`[file.txt#${now}] unchanged since #${now}`)
    // Line 14 moved without being shown, so it needs a ~ check; the refusal shows lines 12-16, which leaves line 17 unseen but known.
    await expect(f.patch(`[file.txt#${now}]\nPUT 14.=14:\n+x`)).rejects.toThrow("never displayed in full")
    await f.patch(`[file.txt#${now}]\nPUT 8.=8:\n+EIGHT\nPUT 17.=17:\n~l18\n+EIGHTEEN`)
    expect((await readFile(f.filePath, "utf8")).split("\n")[7]).toBe("EIGHT")
    expect((await readFile(f.filePath, "utf8")).split("\n")[16]).toBe("EIGHTEEN")
  })

  test("a tag nib did not keep, or one issued for another file, is noted instead of diffed", async () => {
    const f = await fixture()
    const mine = await f.read()
    await writeFile(path.join(f.directory, "other.txt"), "other\n")
    const other = await f.read("", { file: path.join(f.directory, "other.txt") })
    const unknown = ["0000", "FFFF", "1234"].find((tag) => tag !== mine.tag && tag !== other.tag)!
    expect((await f.read(`#${unknown}`)).text).toBe(`[file.txt] #${unknown} is not a version of this file that nib kept in this session; read the file instead.`)
    expect((await f.read(`#${other.tag}`)).text).toContain(`#${other.tag} is not a version of this file that nib kept in this session (it was issued for `)
    await expect(f.read(`#${mine.tag}:1-2`)).rejects.toThrow("takes no line selector")
  })

  test("a large change is cut by the output budget and says so", async () => {
    const f = await fixture(Array.from({ length: 500 }, (_, index) => `${index} ${"x".repeat(200)}`).join("\n") + "\n")
    const first = await f.read(":1-10")
    await writeFile(f.filePath, Array.from({ length: 500 }, (_, index) => `${index} ${"y".repeat(200)}`).join("\n") + "\n")
    const since = (await f.read(`#${first.tag}`)).text
    expect(since).toContain("changed since")
    expect(since).toEndWith("[changes cut by the output limit; read the ranges you need]")
    expect(Buffer.byteLength(since)).toBeLessThan(41 * 1024)
  })
})

describe("find", () => {
  async function tree() {
    const f = await fixture()
    await rm(f.filePath)
    const files: [string, string | Buffer, number][] = [
      [".gitignore", "ignored.log\n", DAYS],
      [".hidden", "h", DAYS],
      ["a.txt", "one\ntwo\n", 3600 + 60],
      ["big.txt", "z".repeat(3_000_000), DAYS],
      ["bin.dat", Buffer.from([0, 1, 2]), DAYS],
      ["empty.txt", "", DAYS],
      ["ignored.log", "secret\n", DAYS],
      ["pic.png", Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), DAYS],
      ["src/b.ts", "x\n".repeat(100), 2 * 86400 + 60],
    ]
    // .gitignore counts inside a repository only, as in git.
    await mkdir(path.join(f.directory, ".git"))
    await mkdir(path.join(f.directory, "src"))
    for (const [name, content, seconds] of files) {
      await writeFile(path.join(f.directory, name), content)
      await age(path.join(f.directory, name), seconds)
    }
    return f
  }
  const all = [
    ".gitignore (1 line, 12 bytes, 3d ago)",
    ".hidden (1 line, 1 byte, 3d ago)",
    "a.txt (2 lines, 8 bytes, 1h ago)",
    "big.txt (2.9 MiB, 3d ago)",
    "bin.dat (binary, 3 bytes, 3d ago)",
    "empty.txt (empty, 3d ago)",
    "pic.png (image/png, 8 bytes, 3d ago)",
    "src/b.ts (100 lines, 200 bytes, 2d ago)",
  ]

  test("lists files like grep searches them, with line counts, sizes and ages", async () => {
    const f = await tree()
    expect((await f.engine.find(f.ctx, {})).text).toBe(`8 files\n\n${all.join("\n")}`)
    expect((await f.engine.find(f.ctx, { noIgnore: true })).text).toContain("\nignored.log (1 line, 7 bytes, 3d ago)\n")
    expect((await f.engine.find(f.ctx, { glob: "*.ts" })).text).toBe("1 file\n\nsrc/b.ts (100 lines, 200 bytes, 2d ago)")
    expect((await f.engine.find(f.ctx, { paths: ["src"] })).text).toBe("1 file\n\nsrc/b.ts (100 lines, 200 bytes, 2d ago)")
    expect((await f.engine.find(f.ctx, { glob: "*.nothing" })).text).toBe("No files found")
  })

  test("filters by size and age, and sorts by either", async () => {
    const f = await tree()
    const order = (indexes: number[]) => indexes.map((index) => all[index]).join("\n")
    expect((await f.engine.find(f.ctx, { sort: "size" })).text).toBe(`8 files, largest first\n\n${order([3, 7, 0, 2, 6, 4, 1, 5])}`)
    expect((await f.engine.find(f.ctx, { sort: "modified" })).text).toBe(`8 files, newest first\n\n${order([2, 7, 0, 1, 3, 4, 5, 6])}`)
    expect((await f.engine.find(f.ctx, { minSize: "100" })).text).toBe(`2 files at least 100 bytes (of 8 listed)\n\n${order([3, 7])}`)
    expect((await f.engine.find(f.ctx, { newer: "1d", minSize: "0.005k" })).text).toBe(`1 file at least 5 bytes, modified in the last 1d (of 8 listed)\n\n${order([2])}`)
    expect((await f.engine.find(f.ctx, { newer: "1s" })).text).toBe("No files found (none of the 8 listed pass the filters)")
    await expect(f.engine.find(f.ctx, { minSize: "big" })).rejects.toThrow('Size "big" is not like')
    await expect(f.engine.find(f.ctx, { newer: "2 days" })).rejects.toThrow('Age "2 days" is not like')
    await expect(f.engine.find(f.ctx, { paths: ["nope"] })).rejects.toThrow("nope does not exist")
  })

  test("pages 200 files at a time", async () => {
    const f = await fixture()
    await rm(f.filePath)
    for (let index = 0; index < 205; index++) await writeFile(path.join(f.directory, `f${String(index).padStart(3, "0")}.txt`), "x\n")
    const first = (await f.engine.find(f.ctx, {})).text
    expect(first).toStartWith("205 files\n\nf000.txt (1 line, 2 bytes, ")
    expect(first).toEndWith("\n\n[205 files; continue with skip 200]")
    const rest = (await f.engine.find(f.ctx, { skip: 200 })).text
    expect(rest.split("\n\n")[1]!.split("\n").map((row) => row.split(" ")[0])).toEqual(["f200.txt", "f201.txt", "f202.txt", "f203.txt", "f204.txt"])
    expect((await f.engine.find(f.ctx, { skip: 205 })).text).toBe("No more results (205 files; skip 205 is past the end)")
  })
})

describe("grep replace", () => {
  const token = (text: string) => /REAPPLY ([a-z0-9]{6})$/.exec(text)![1]!

  test("previews each changed line once, as it will be, and applies through a single-use token", async () => {
    const content = "const foo = 1\nlet bar = foo + foo\nnothing\n"
    const f = await fixture(content)
    const preview = (await f.engine.grep(f.ctx, { pattern: "foo", replace: "baz" })).text
    expect(preview).toMatch(new RegExp(`^2 matches in 1 files\\n\\n\\[file\\.txt#${fileTag(content)}\\]\\n>1\\|const baz = 1\\n>2\\|let bar = baz \\+ baz\\n\\nTo apply the 2 replaced lines shown \\(>N\\| rows\\), call edit with input: REAPPLY [a-z0-9]{6}$`))
    expect(await readFile(f.filePath, "utf8")).toBe(content)
    const applied = await f.patch(`REAPPLY ${token(preview)}`)
    expect(applied.text).toStartWith(`[file.txt#${fileTag("const baz = 1\nlet bar = baz + baz\nnothing\n")}]`)
    expect(await readFile(f.filePath, "utf8")).toBe("const baz = 1\nlet bar = baz + baz\nnothing\n")
    await expect(f.patch(`REAPPLY ${token(preview)}`)).rejects.toThrow("unknown, already used or expired")
  })

  test("capture groups, an empty matched line, and a replacement that changes nothing", async () => {
    const f = await fixture("let alpha = 1\n\nlet beta = 2\n")
    const groups = (await f.engine.grep(f.ctx, { pattern: "let (?P<name>\\w+) = (\\d)", replace: "var ${name} = $2$2" })).text
    expect(groups).toContain(">1|var alpha = 11\n")
    await f.patch(`REAPPLY ${token(groups)}`)
    const empty = (await f.engine.grep(f.ctx, { pattern: "^$", replace: "// gap" })).text
    expect(empty).toContain("]\n*2:\n>2|// gap\n")
    await f.patch(`REAPPLY ${token(empty)}`)
    expect(await readFile(f.filePath, "utf8")).toBe("var alpha = 11\n// gap\nvar beta = 22\n")
    const same = (await f.engine.grep(f.ctx, { pattern: "gap", replace: "gap" })).text
    expect(same).not.toContain(">2|")
    expect(same).toEndWith("[no replacement to apply: the shown matches stay the same or are in files nib cannot edit]")
    // A match the replace leaves as it is stays a plain match row, next to the lines that change.
    await writeFile(path.join(f.directory, "mixed.txt"), "color\ncolour\n")
    expect((await f.engine.grep(f.ctx, { pattern: "colou?r", replace: "color", paths: ["mixed.txt"] })).text).toContain("]\n*1:color\n>2|color\n")
  })

  test("a long line shows the stretch around its replacement, and the token changes it without a full read", async () => {
    const line = "a".repeat(3000) + "NEEDLE" + "b".repeat(100)
    const f = await fixture(`${line}\nshort\n`)
    const preview = (await f.engine.grep(f.ctx, { pattern: "NEEDLE", replace: "PIN" })).text
    expect(preview).toContain(`]\n>[1 chars 2916-3088 of 3103]|${"a".repeat(85)}PIN${"b".repeat(85)}\n`)
    const applied = await f.patch(`REAPPLY ${token(preview)}`)
    expect(await readFile(f.filePath, "utf8")).toBe(`${"a".repeat(3000)}PIN${"b".repeat(100)}\nshort\n`)
    // The preview showed only a stretch of the new line, so it is not seen as a whole.
    expect(await refusal(f.patch(`[file.txt#${applied.tag}]\nPUT 1:\n+x`))).toContain("never displayed in full")
  })

  test("a file that changed after the preview still gets the change when it can be rebased", async () => {
    const f = await fixture("one\ntwo\nthree\n")
    const preview = (await f.engine.grep(f.ctx, { pattern: "two", replace: "TWO" })).text
    await writeFile(f.filePath, "zero\none\ntwo\nthree\n")
    const applied = await f.patch(`REAPPLY ${token(preview)}`)
    expect(applied.text).toContain("Recovered")
    expect(applied.text).not.toContain("as previewed")
    expect(await readFile(f.filePath, "utf8")).toBe("zero\none\nTWO\nthree\n")
  })

  test("a replace shows every match of its files, so one token covers them, and applied as previewed it answers with tags and counts", async () => {
    const content = Array.from({ length: 25 }, (_, index) => `hit ${index}`).join("\n") + "\n"
    const f = await fixture(content)
    await writeFile(path.join(f.directory, "other.txt"), "hit\n")
    const preview = (await f.engine.grep(f.ctx, { pattern: "hit", replace: "HIT" })).text
    expect(preview).not.toContain("more matches")
    expect(preview).toContain("\n>24|HIT 23\n>25|HIT 24\n\n[other.txt#")
    expect(preview).not.toMatch(/^\*/m)
    expect(preview).toContain("To apply the 26 replaced lines shown")
    const replaced = content.replaceAll("hit", "HIT")
    const applied = await f.patch(`REAPPLY ${token(preview)}`)
    expect(applied.text).toBe(`[file.txt#${fileTag(replaced)}] 25 lines replaced as previewed\n\n[other.txt#${fileTag("HIT\n")}] 1 line replaced as previewed`)
    // The >N| rows showed the new lines whole, so they are anchors without a read.
    await f.patch(`[file.txt#${fileTag(replaced)}]\nPUT 3:\n+third`)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(1, 4)).toEqual(["HIT 1", "third", "HIT 3"])
    await expect(f.engine.grep(f.ctx, { pattern: "hit", replace: "x", mode: "files" })).rejects.toThrow("needs mode matches")
    await expect(f.engine.grep(f.ctx, { pattern: "hit\\nhit", replace: "x" })).rejects.toThrow("within one line")
  })

  test("applying a token that left changing lines out says how many, so the model greps again", async () => {
    const f = await fixture("hit\n")
    // 21 matching files: a page holds 20, so the last file's 2 lines stay out.
    for (let index = 0; index < 20; index++) await writeFile(path.join(f.directory, `more${String(index).padStart(2, "0")}.txt`), "hit\nhit\n")
    const preview = (await f.engine.grep(f.ctx, { pattern: "hit", replace: "HIT" })).text
    expect(preview).toContain("[21 matching files; continue with skip 20]")
    const applied = await f.patch(`REAPPLY ${token(preview)}`)
    expect(applied.text).toEndWith("\n\n[2 more lines that the search would change were not in this token; grep again with the same pattern and replace for them]")
    const rest = (await f.engine.grep(f.ctx, { pattern: "hit", replace: "HIT" })).text
    expect(rest).toStartWith("2 matches in 1 files\n\n[more19.txt#")
    expect((await f.patch(`REAPPLY ${token(rest)}`)).text).toBe(`[more19.txt#${fileTag("HIT\nHIT\n")}] 2 lines replaced as previewed`)
  })

  test("a replacement that adds lines shifts the ones below, so applying it shows the rows", async () => {
    const f = await fixture("one\ntwo\nthree\n")
    const preview = (await f.engine.grep(f.ctx, { pattern: "two", replace: "2\nII" })).text
    expect(preview).toContain("]\n>2|2\n>2|II\n")
    const applied = await f.patch(`REAPPLY ${token(preview)}`)
    expect(applied.text).toBe(`[file.txt#${fileTag("one\n2\nII\nthree\n")}]\n1:one\n2:2\n3:II\n4:three`)
  })

  test("a single file whose rows pass the output budget is cut with a way on, instead of left out", async () => {
    const f = await fixture(Array.from({ length: 300 }, (_, index) => `match ${"x".repeat(400)} ${index + 1}`).join("\n") + "\n")
    const result = (await f.engine.grep(f.ctx, { pattern: "match" })).text
    expect(Buffer.byteLength(result)).toBeLessThanOrEqual(40 * 1024 + 200)
    const next = Number(/\[output limit reached; grep file\.txt:(\d+)- to go on from line \1\]/.exec(result)![1])
    expect(result).toContain(`\n*${next - 1}:match `)
    expect((await f.engine.grep(f.ctx, { pattern: "match", paths: [`file.txt:${next}-`] })).text).toContain(`\n*${next}:match `)
    const preview = (await f.engine.grep(f.ctx, { pattern: "match", replace: "MATCH" })).text
    const shown = [...preview.matchAll(/^>\d+\|/gm)].length
    expect(preview).toContain(`To apply the ${shown} replaced lines shown`)
    expect((await f.patch(`REAPPLY ${token(preview)}`)).text).toEndWith(`[${300 - shown} more lines that the search would change were not in this token; grep again with the same pattern and replace for them]`)
    expect((await readFile(f.filePath, "utf8")).split("\n").filter((row) => row.startsWith("MATCH"))).toHaveLength(shown)
  })
})

describe("ranges one line off", () => {
  const plan = [
    "# Plan",
    "- **Name** of a peer, in precedence order, decided ONCE at server start:",
    "  1. an explicit name from the server's --name flag",
    "  2. the pane session it was started in",
    "  3. else a minted anon id",
    "- **Address** is the name plus the host it runs on",
  ].join("\n") + "\n"
  const rows = [
    "- **Name** of a peer, in precedence order, decided ONCE at registration:",
    "  1. an explicit name: --name or SKETERM_PEER_NAME",
    "  2. the pane session it was started in",
    "  3. else a minted anon-xxxx id",
  ].map((line) => `+${line}`).join("\n")

  test("a range that starts one line late applies with a warning whose hunks undo it", async () => {
    const f = await fixture(plan)
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nPUT 3.=6:\n${rows}`)
    const fix = "CUT 2\nPUT >6:\n+- **Address** is the name plus the host it runs on"
    expect(result.text).toEndWith(`Warnings:\nLines 2 and 3 now nearly match: line 2 was kept just above an edit and line 3 is the edit's first new line. If the edit was meant to replace line 2, its range started one line late and overwrote the line below it instead; these hunks, with the new tag, delete line 2 and put that line back:\n${fix}`)
    // The hunks give exactly the file the right range would have.
    const tag = /#([0-9A-F]{4})\]/.exec(result.text)![1]
    await f.patch(`[file.txt#${tag}]\n${fix}`)
    const intended = await fixture(plan)
    const s = await intended.read()
    await intended.patch(`[file.txt#${s.tag}]\nPUT 2.=5:\n${rows}`)
    expect(await readFile(f.filePath, "utf8")).toBe(await readFile(intended.filePath, "utf8"))
  })

  test("a range that ends one line early applies with a warning whose hunk undoes it", async () => {
    const f = await fixture(plan)
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nPUT 1.=4:\n${rows}`)
    expect(result.text).toEndWith("Warnings:\nLines 4 and 5 now nearly match: line 4 is an edit's last new line and line 5 was kept just below it. If the edit was meant to replace line 5, its range ended one line early; this hunk, with the new tag, deletes it:\nCUT 5")
  })

  test("a right range, or a line rewritten from one just like it, gets no warning", async () => {
    const f = await fixture(plan)
    const r = await f.read()
    expect((await f.patch(`[file.txt#${r.tag}]\nPUT 2.=5:\n${rows}`)).text).not.toContain("Warnings:")
    const g = await fixture("repeated line of some length\nrepeated line of some length\ntail\n")
    const s = await g.read()
    expect((await g.patch(`[file.txt#${s.tag}]\nPUT 2.=2:\n+repeated line of some length\n+an added line below it`)).text).not.toContain("Warnings:")
  })
})

describe("bracket balance after an edit", () => {
  const code = [
    "function formatSize(bytes: number) {",
    "  return `${bytes} bytes`",
    "}",
    "",
    "function formatAge(time: number) {",
    "  const seconds = Math.floor((Date.now() - time) / 1000)",
    "  return seconds < 60 ? `${seconds}s ago` : `${Math.floor(seconds / 60)}m ago`",
    "}",
    "",
    "const UNITS = { k: 1024 }",
  ].join("\n") + "\n"
  const rewrite = [
    "function formatAge(time: number) {",
    "  const seconds = Math.floor((Date.now() - time) / 1000)",
    "  if (seconds < 60) return `${seconds}s ago`",
    "  return `${Math.floor(seconds / 60)}m ago`",
    "}",
  ].map((line) => `+${line}`).join("\n")

  test("a closing line left outside a replaced block is named", async () => {
    const f = await fixture(code)
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nPUT 5.=7:\n${rewrite}`)
    expect(result.text).toEndWith("Warnings:\nBrackets no longer balance: before this edit they did, and now the file has 1 more } than {. The new lines 5-9 have 1 more } than {, counted against the lines they replaced; check them and the lines just outside them.")
  })

  test("a stray closer typed into one line is named too", async () => {
    const f = await fixture(code)
    const r = await f.read()
    const result = await f.patch(`[file.txt#${r.tag}]\nPUT 6:\n+  } const seconds = Math.floor((Date.now() - time) / 1000)`)
    expect(result.text).toContain("Brackets no longer balance: before this edit they did, and now the file has 1 more } than {. The new line 6 has")
  })

  test("the right range, brackets in strings and comments, and files that never balanced get no warning", async () => {
    const f = await fixture(code)
    const r = await f.read()
    expect((await f.patch(`[file.txt#${r.tag}]\nPUT 5.=8:\n${rewrite}`)).text).not.toContain("Warnings:")
    const g = await fixture(code)
    const s = await g.read()
    expect((await g.patch(`[file.txt#${s.tag}]\nPUT >10:\n+const brace = "}" // a } in a comment\n+const pattern = /[)]/`)).text).not.toContain("Warnings:")
    const h = await fixture("case $1 in\n  start) run ;;\nesac\n")
    const t = await h.read()
    expect((await h.patch(`[file.txt#${t.tag}]\nPUT >2:\n+  stop) halt ;;`)).text).not.toContain("Warnings:")
  })

  test("in a Hawkeye template, comments and the apostrophes of its text hide no brackets", async () => {
    const directory = await workspace()
    const file = path.join(directory, "page.hwk")
    const template = "{% if (items.length) %}\n  <ul>{% each items as item %}<li>{{ item.name }}</li>{% /each %}</ul>\n{% /if %}\n"
    await writeFile(file, template)
    const engine = new Engine()
    const ctx: Context = { directory, session: "session", signal: new AbortController().signal }
    const tag = /#([0-9A-F]{4})\]/.exec((await engine.read(ctx, { paths: [file] })).text)![1]
    // A comment spanning lines, with an apostrophe and a lone bracket, and an HTML comment with one, change nothing.
    const added = await engine.patch(ctx, `[${file}#${tag}]\nPUT >1:\n+  {#- Today's list (the open ones only:\n+      the rest wait } -#}\n+  <!-- a ( in markup's comment -->`)
    expect(added.text).not.toContain("Brackets")
    const tag2 = /#([0-9A-F]{4})\]/.exec(added.text)![1]
    // A real stray closer in the template is still named.
    expect((await engine.patch(ctx, `[${file}#${tag2}]\nPUT >4:\n+  <p>don't stop)</p>`)).text).toContain("Brackets no longer balance: before this edit they did, and now the file has 1 more ) than (")
  })

  test("an edit or write that leaves a sound .json file unsound says where", async () => {
    const directory = await workspace()
    const file = path.join(directory, "en.json")
    await writeFile(file, '{\n  "save": "Save",\n  "open": "Open"\n}\n')
    const engine = new Engine()
    const ctx: Context = { directory, session: "session", signal: new AbortController().signal }
    const tag = /#([0-9A-F]{4})\]/.exec((await engine.read(ctx, { paths: [file] })).text)![1]
    const edited = await engine.patch(ctx, `[${file}#${tag}]\nPUT 3:\n+  "open": "Open",\n+  "save": "Store"`)
    expect(edited.text).toContain('Warnings:\nThe file is not valid JSON now: line 4: key "save" repeats in its object (first on line 2).')
    const written = await engine.write(ctx, { path: path.join(directory, "nl.json"), content: '{\n  "save": "Opslaan",\n}\n' })
    expect(written.text).toContain("Warnings:\nThe file is not valid JSON now: line 3: invalid character '}' looking for beginning of object key string.")
    expect((await engine.write(ctx, { path: path.join(directory, "fr.json"), content: '{"save": "Enregistrer"}\n' })).text).not.toContain("Warnings:")
  })
})

describe("forgiving the forms models borrow", () => {
  const thirty = Array.from({ length: 30 }, (_, index) => `l${index + 1}`).join("\n") + "\n"

  test("PUT N:=M: applies as the range N to M, with a note giving nib's form", async () => {
    const f = await fixture(thirty)
    const r = await f.read(":1-5")
    const result = await f.patch(`[file.txt#${r.tag}]\nPUT 2:=3:\n+two and three`)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(0, 3)).toEqual(["l1", "two and three", "l4"])
    expect(result.text).toEndWith("Warnings:\nPUT 2:=3: was read as PUT 2.=3:; a range is written N.=M, with a dot.")
  })

  test("- rows that match quote the old lines: the edit applies, even to unseen lines, and the response shows the hunk without them", async () => {
    const f = await fixture(thirty)
    const r = await f.read(":1-5")
    const result = await f.patch(`[file.txt#${r.tag}]\nPUT 20:\n-l20\n+TWENTY\nCUT 25.=26\n-l25\n-l26  `)
    expect((await readFile(f.filePath, "utf8")).split("\n").slice(18, 24)).toEqual(["l19", "TWENTY", "l21", "l22", "l23", "l24"])
    expect(result.text).toContain("Warnings:\nThe - rows matched the file, so the edit was applied;")
    expect(result.text).toContain("Leave - rows out next time. This hunk is just:\nPUT 20:\n+TWENTY")
    expect(await readFile(f.filePath, "utf8")).not.toContain("l25")
    // The earlier CUT moved l28 up to line 26.
    const cut = await f.patch(`[file.txt#${result.tag}]\nCUT 26\n-l28`)
    expect(cut.text).toContain("This hunk is just:\nCUT 26")
    expect(await readFile(f.filePath, "utf8")).not.toContain("l28")
  })

  test("- rows that do not match are refused with the real lines, and nothing is written", async () => {
    const f = await fixture(thirty)
    const r = await f.read(":1-5")
    const wrong = await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 20:\n-l2O\n+x`))
    expect(wrong).toContain('line 20 is not "l2O"')
    expect(wrong).toContain("*20:l20")
    expect(wrong).toContain("nib needs no - rows")
    expect(wrong).not.toContain("REAPPLY")
    expect(await refusal(f.patch(`[file.txt#${r.tag}]\nPUT 20.=21:\n-l20\n+x`))).toContain("the - rows under lines 20-21 quote 1 line, but the hunk replaces 2")
    expect(await readFile(f.filePath, "utf8")).toBe(thirty)
  })

  test("numbers over a limit are capped, with a note, instead of refused", async () => {
    const f = await fixture(thirty)
    const grep = (await f.engine.grep(f.ctx, { pattern: "^l5$", before: 2, after: 25, maxLineLength: 5000 })).text
    expect(grep).toContain("*5:l5\n 6:l6")
    expect(grep).toContain(" 25:l25\n")
    expect(grep).not.toContain("26:l26")
    expect(grep).toEndWith("[after 25 is over its limit of 20, so 20 was used]\n\n[maxLineLength 5000 is over its limit of 2000, so 2000 was used]")
    const read = (await f.engine.read(f.ctx, { paths: ["file.txt:1"], maxLineLength: 3_000_000 })).text
    expect(read).toStartWith("[maxLineLength 3000000 is over its limit of 2097152, so 2097152 was used]\n\n[file.txt#")
  })
})
