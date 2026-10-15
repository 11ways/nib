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
})
