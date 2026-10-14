import { afterEach, describe, expect, test } from "bun:test"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js"
import { ElicitRequestSchema, ListRootsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { pathToFileURL } from "node:url"
import { createServer, EDIT_DESCRIPTION, INSTRUCTIONS } from "../src/server"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })

async function workspace() {
  const directory = await realpath(await mkdtemp(path.join(tmpdir(), "nib-server-")))
  directories.push(directory)
  return directory
}

type Options = { confirm?: boolean; roots?: string[]; elicit?: (message: string) => { action: "accept" | "decline"; content?: Record<string, string> } }

async function connect(directory: string, options: Options = {}) {
  const server = createServer({ directory, confirmOutsideRoots: options.confirm })
  const capabilities = { ...options.roots && { roots: { listChanged: true } }, ...options.elicit && { elicitation: { form: {} } } }
  const client = new Client({ name: "test", version: "1" }, { capabilities })
  if (options.roots) client.setRequestHandler(ListRootsRequestSchema, async () => ({ roots: options.roots!.map((root) => ({ uri: pathToFileURL(root).href })) }))
  const prompts: string[] = []
  if (options.elicit) client.setRequestHandler(ElicitRequestSchema, async (request) => {
    prompts.push(request.params.message)
    return options.elicit!(request.params.message)
  })
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair()
  await Promise.all([server.connect(serverTransport), client.connect(clientTransport)])
  const call = async (name: string, args: Record<string, unknown>) => {
    const result = await client.callTool({ name, arguments: args }) as { isError?: boolean; content: { type: string; text?: string; data?: string; mimeType?: string }[] }
    return { ...result, text: result.content[0]?.text ?? "" }
  }
  return { client, call, prompts }
}

const tagOf = (text: string) => /#([0-9A-F]{4})\]/.exec(text)![1]!

describe("MCP server", () => {
  test("lists five always-loaded tools within Claude Code's description limit", async () => {
    const { client } = await connect(await workspace())
    const { tools } = await client.listTools()
    expect(tools.map((tool) => tool.name).sort()).toEqual(["edit", "find", "grep", "read", "write"])
    for (const tool of tools) {
      expect(tool._meta).toEqual({ "anthropic/alwaysLoad": true })
      expect(tool.description!.length).toBeLessThanOrEqual(2048)
    }
    for (const name of ["read", "grep", "find"]) expect(tools.find((tool) => tool.name === name)!.annotations?.readOnlyHint).toBe(true)
    expect(EDIT_DESCRIPTION.length).toBeLessThanOrEqual(2048)
    expect(EDIT_DESCRIPTION).not.toContain("*** Begin Patch")
    expect(EDIT_DESCRIPTION).not.toContain("*** End Patch")
    expect(INSTRUCTIONS.length).toBeLessThanOrEqual(2048)
    expect(client.getInstructions()).toBe(INSTRUCTIONS)
  })

  test("read, edit and write round-trip; failures are tool errors, not protocol errors", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "one\ntwo\n")
    await writeFile(path.join(directory, "pixel.png"), Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))
    const { call } = await connect(directory)
    const read = await call("read", { paths: ["a.txt", "pixel.png"] })
    expect(read.content[1]).toEqual({ type: "image", data: Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]).toString("base64"), mimeType: "image/png" })
    const edited = await call("edit", { input: `[a.txt#${tagOf(read.text)}]\nPUT 2.=2:\n+TWO` })
    expect(edited.isError).toBeFalsy()
    expect(await readFile(path.join(directory, "a.txt"), "utf8")).toBe("one\nTWO\n")
    const stale = await call("edit", { input: `[a.txt#${tagOf(read.text)}]\nPUT 1.=1:\n+x\nPUT 2.=2:\n+y` })
    expect(stale.isError).toBe(true)
    expect(stale.text).toContain("is stale")
    expect((await call("write", { path: "b.txt", content: "bee\n" })).text).toStartWith("Created [b.txt#")
    expect((await call("grep", { pattern: "bee" })).text).toContain("*1:bee")
  })

  test("new options travel through MCP: grep modes and reapply tokens", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), Array.from({ length: 30 }, (_, index) => `row ${index + 1}`).join("\n") + "\n")
    const { call } = await connect(directory)
    expect((await call("grep", { pattern: "row 1", mode: "count", glob: ["*.txt"] })).text).toBe("11 matches in 1 files\n\n11 a.txt")
    expect((await call("grep", { pattern: "row", paths: ["a.txt:5-6"], before: 0, after: 0 })).text).toContain("*5:row 5\n*6:row 6")
    expect((await call("read", { paths: ["a.txt:1-2"], plain: true })).text).toBe("[a.txt] lines 1-2 of 30, plain\nrow 1\nrow 2")
    const read = await call("read", { paths: ["a.txt:1-2"] })
    const refused = await call("edit", { input: `[a.txt#${tagOf(read.text)}]\nPUT 20.=20:\n+TWENTY` })
    expect(refused.isError).toBe(true)
    const applied = await call("edit", { input: `REAPPLY ${/REAPPLY ([a-z0-9]{6})$/.exec(refused.text)![1]}` })
    expect(applied.isError).toBeFalsy()
    expect(applied.text).toContain("20:TWENTY")
  })

  test("find, read info, read since, grep replace and CP travel through MCP", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "one\ntwo\nthree\n")
    const { call } = await connect(directory)
    expect((await call("find", { glob: "*.txt" })).text).toMatch(/^1 file\n\na\.txt \(3 lines, 14 bytes, \d+s ago\)$/)
    const info = await call("read", { paths: ["a.txt"], info: true })
    expect(info.text).toMatch(/^\[a\.txt#[0-9A-F]{4}\] 3 lines, 14 bytes, about 4 tokens; longest line 5 chars \(line 3\); LF; modified \d+s ago$/)
    const preview = await call("grep", { pattern: "t(\\w+)", replace: "T$1" })
    expect(preview.text).toContain("*2:two\n>2|Two\n*3:three\n>3|Three")
    const applied = await call("edit", { input: `REAPPLY ${/REAPPLY ([a-z0-9]{6})$/.exec(preview.text)![1]}` })
    expect(applied.isError).toBeFalsy()
    expect(await readFile(path.join(directory, "a.txt"), "utf8")).toBe("one\nTwo\nThree\n")
    const since = await call("read", { paths: [`a.txt#${tagOf(info.text)}`] })
    expect(since.text).toBe(`[a.txt#${tagOf(applied.text)}] changed since #${tagOf(info.text)}: 2 lines added, 2 removed\n 1:one\n-|two\n-|three\n+2:Two\n+3:Three`)
    const copied = await call("edit", { input: `[a.txt#${tagOf(applied.text)}]\nCP b.txt` })
    expect(copied.text).toStartWith(`Copied a.txt to [b.txt#${tagOf(applied.text)}]`)
    expect(await readFile(path.join(directory, "b.txt"), "utf8")).toBe("one\nTwo\nThree\n")
  })

  test("by default, writes outside the roots proceed without asking", async () => {
    const directory = await workspace()
    const outside = await workspace()
    const { call, prompts } = await connect(directory, { roots: [directory], elicit: () => ({ action: "decline" }) })
    expect((await call("write", { path: path.join(outside, "a.txt"), content: "a" })).isError).toBeFalsy()
    expect(await readFile(path.join(outside, "a.txt"), "utf8")).toBe("a")
    expect(prompts).toEqual([])
  })

  test("client roots widen where writes may go without asking", async () => {
    const directory = await workspace()
    const extra = await workspace()
    const { call, prompts } = await connect(directory, { confirm: true, roots: [directory, extra], elicit: () => ({ action: "decline" }) })
    expect((await call("write", { path: path.join(extra, "x.txt"), content: "x" })).isError).toBeFalsy()
    expect(prompts).toEqual([])
  })

  test("writes outside the roots ask the user; directory approval lasts for the session", async () => {
    const directory = await workspace()
    const outside = await workspace()
    let answer: "decline" | "once" | "directory" = "decline"
    const { call, prompts } = await connect(directory, {
      confirm: true,
      roots: [directory],
      elicit: (message) => answer === "decline" ? { action: "decline" } : { action: "accept", content: { scope: answer === "once" ? "this file once" : `everything under ${outside} this session` } },
    })
    const denied = await call("write", { path: path.join(outside, "a.txt"), content: "a" })
    expect(denied.isError).toBe(true)
    expect(denied.text).toContain("declined")
    expect(prompts[0]).toBe(`nib wants to create ${path.join(outside, "a.txt")}, which is outside the workspace. Allow it?`)
    answer = "once"
    expect((await call("write", { path: path.join(outside, "a.txt"), content: "a" })).isError).toBeFalsy()
    expect((await call("write", { path: path.join(outside, "b.txt"), content: "b" })).isError).toBeFalsy()
    expect(prompts).toHaveLength(3)
    answer = "directory"
    await call("write", { path: path.join(outside, "c.txt"), content: "c" })
    answer = "decline"
    expect((await call("write", { path: path.join(outside, "d.txt"), content: "d" })).isError).toBeFalsy()
    expect(prompts).toHaveLength(4)
  })

  test("without elicitation support, writes outside the workspace are refused", async () => {
    const directory = await workspace()
    const outside = await workspace()
    const { call } = await connect(directory, { confirm: true })
    const result = await call("write", { path: path.join(outside, "a.txt"), content: "a" })
    expect(result.isError).toBe(true)
    expect(result.text).toContain("outside the workspace roots")
  })

  test("the stdio entrypoint serves the tools", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "hello\n")
    const client = new Client({ name: "test", version: "1" })
    await client.connect(new StdioClientTransport({ command: process.execPath, args: [path.join(import.meta.dir, "../src/main.ts")], cwd: directory, stderr: "pipe" }))
    try {
      const result = await client.callTool({ name: "read", arguments: { paths: ["a.txt"] } }) as { content: { text: string }[] }
      expect(result.content[0]!.text).toMatch(/^\[a\.txt#[0-9A-F]{4}\]\n1:hello$/)
    } finally { await client.close() }
  })

  test("NIB_CONFIRM_OUTSIDE_ROOTS=1 turns on confirmation for writes outside the roots", async () => {
    const directory = await workspace()
    const outside = await workspace()
    const writeOutside = async (env: Record<string, string>) => {
      const client = new Client({ name: "test", version: "1" })
      await client.connect(new StdioClientTransport({ command: process.execPath, args: [path.join(import.meta.dir, "../src/main.ts")], cwd: directory, env, stderr: "pipe" }))
      try {
        return await client.callTool({ name: "write", arguments: { path: path.join(outside, "a.txt"), content: "a" } }) as { isError?: boolean; content: { text: string }[] }
      } finally { await client.close() }
    }
    const env = { PATH: process.env.PATH ?? "" }
    const confirmed = await writeOutside({ ...env, NIB_CONFIRM_OUTSIDE_ROOTS: "1" })
    expect(confirmed.isError).toBe(true)
    expect(confirmed.content[0]!.text).toContain("outside the workspace roots")
    expect((await writeOutside(env)).isError).toBeFalsy()
  })
})
