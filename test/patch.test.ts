import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { Engine, type Context } from "./support/engine"

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
    view: async (name: string) => (await engine.read(ctx, { paths: [name] })).text,
    patch: (input: string) => engine.patch(ctx, input),
    file: (name: string) => readFile(path.join(directory, name), "utf8"),
  }
}

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

  test("IMPORT puts Java imports where the file keeps its own, with no tag, and a read past many shows them by package", async () => {
    const imports = Array.from({ length: 10 }, (_, index) => `import be.example.orm.Type${index};`).join("\n")
    const f = await fixture({ "A.java": `package be.example;\n\n${imports}\nimport java.util.List;\n\nclass A {\n    List<Type1> all;\n}\n` })
    expect((await f.view("A.java:1-16")).split("\n").slice(1, 6)).toEqual([
      "1:package be.example;",
      "2:",
      "[lines 3-13: 11 imports, by package; read A.java:3-13 for their lines]",
      "[be.example.orm: Type0, Type1, Type2, Type3, Type4, Type5, Type6, Type7, Type8, Type9]",
      "[java.util: List]",
    ])
    const out = (await f.patch("[A.java]\nIMPORT java.util.Map, be.example.orm.Type1")).text
    expect(out).toContain("14:import java.util.Map;")
    expect(out).toContain("[IMPORT be.example.orm.Type1: line 4 imports it already]")
    expect(await f.file("A.java")).toContain("import java.util.List;\nimport java.util.Map;\n\nclass A {")
  })
})
