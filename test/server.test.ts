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

  test("tool definitions and instructions stay within a size budget", async () => {
    const { client } = await connect(await workspace())
    const { tools } = await client.listTools()
    const size = tools.reduce((sum, tool) => sum + tool.description!.length + JSON.stringify(tool.inputSchema).length, 0) + INSTRUCTIONS.length
    // Every session carries these in each model call's context; 8.2K characters is about 2K tokens.
    expect(size).toBeLessThanOrEqual(8200)
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
    expect(applied.text).toContain("19:row 19\n20:TWENTY\n21:row 21")
  })

  test("arguments in other tools' shapes work and teach nib's own: filePath with offset and limit, one path string, numbers as strings", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "one\ntwo\nthree\nfour\n")
    const { call } = await connect(directory)
    const borrowed = await call("read", { filePath: "a.txt", offset: "2", limit: "2" })
    expect(borrowed.text).toMatch(/^\[read takes paths, like \["a\.txt:2\+2"\]; filePath, offset, limit were read that way\]\n\n\[a\.txt#[0-9A-F]{4}\]\n2:two\n3:three\n/)
    expect((await call("read", { paths: "a.txt:4" })).text).toMatch(/^\[a\.txt#[0-9A-F]{4}\]\n4:four\n/)
    const missing = await call("read", { maxLineLength: 10 })
    expect([missing.isError, missing.text]).toEqual([true, 'read needs paths, a list like ["src/a.ts", "src/b.ts:120-180"]'])
    const grep = await call("grep", { pattern: "two", path: "a.txt", before: "0", after: "30" })
    expect(grep.text).toStartWith('[grep takes paths, a list like ["a.txt"]; path was read that way]\n\n1 matches in 1 files')
    expect(grep.text).toContain("*2:two\n 3:three\n 4:four")
    expect(grep.text).toEndWith("[after 30 is over its limit of 20, so 20 was used]")
    expect((await call("find", { paths: "a.txt" })).text).toStartWith("1 file\n\na.txt (4 lines")
  })

  test("JSON mistakes are read as meant, with a note: lists sent as JSON text, and a patch sent as one line of escapes", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "one\ntwo\n")
    const { call } = await connect(directory)
    const listed = (value: string) => `[paths was a string holding a JSON list; it was read as the list ${value}, so send the list itself]`
    const read = await call("read", { paths: '["a.txt"]' })
    expect(read.text).toStartWith(`${listed('["a.txt"]')}\n\n[a.txt#`)
    const grep = await call("grep", { pattern: "two", paths: '["a.txt"]', glob: '["*.txt"]' })
    expect(grep.text).toStartWith(`${listed('["a.txt"]')}\n[glob was a string holding a JSON list; it was read as the list ["*.txt"], so send the list itself]\n\n1 matches in 1 files`)
    expect((await call("find", { paths: '["a.txt"]' })).text).toStartWith(`${listed('["a.txt"]')}\n\n1 file`)
    const edited = await call("edit", { input: `[a.txt#${tagOf(read.text)}]\\nPUT 2:\\n+TWO` })
    expect(edited.isError).toBeFalsy()
    expect(edited.text).toStartWith("[the patch came as one line with escaped line breaks (\\n); it was read with real line breaks, so send those]\n\n[a.txt#")
    expect(await readFile(path.join(directory, "a.txt"), "utf8")).toBe("one\nTWO\n")
    // A failed call keeps the note, so the model learns both things at once.
    const missing = await call("grep", { pattern: "x", paths: '["nope.txt"]' })
    expect([missing.isError, missing.text]).toEqual([true, `${listed('["nope.txt"]')}\n\nnope.txt does not exist`])
    // The end of a tool call's markup after the list, as small models leave it, is dropped; other text after it leaves one path.
    const marked = await call("grep", { pattern: "one", paths: '["a.txt"]><parameter name="mode' })
    expect(marked.text).toStartWith(`[paths was a string holding a JSON list; it was read as the list ["a.txt"], and the tool-call markup after it ("><parameter name=\\"mode") was dropped, so send the list itself]\n\n1 matches in 1 files`)
    expect((await call("read", { paths: '["a.txt"]>' })).text).toStartWith('[paths was a string holding a JSON list; it was read as the list ["a.txt"], and the tool-call markup after it (">") was dropped, so send the list itself]\n\n[a.txt#')
    expect((await call("read", { paths: '["a.txt"]x' })).text).toBe('["a.txt"]x does not exist')
    // Text that only looks like a list stays one path, and a one-line patch without escapes is left to the parser.
    expect((await call("read", { paths: "[draft].txt" })).text).toBe("[draft].txt does not exist")
    expect((await call("edit", { input: "PUT 1:" })).text).not.toContain("escaped line breaks")
  })

  test("grep takes another grep tool's names for its modes, with a note", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "one\ntwo\n")
    const { call } = await connect(directory)
    expect((await call("grep", { pattern: "two", mode: "content" })).text).toStartWith("[mode content was read as matches, nib's name for it]\n\n1 matches in 1 files\n\n[a.txt#")
    expect((await call("grep", { pattern: "two", mode: "files_with_matches" })).text).toBe("[mode files_with_matches was read as files, nib's name for it]\n\n1 matches in 1 files\n\na.txt")
    const wrong = await call("grep", { pattern: "two", mode: "lines" })
    expect([wrong.isError, wrong.text]).toEqual([true, 'mode "lines" is not one of matches, files or count'])
  })

  test("find, read info, read since, grep replace and CP travel through MCP", async () => {
    const directory = await workspace()
    await writeFile(path.join(directory, "a.txt"), "one\ntwo\nthree\n")
    const { call } = await connect(directory)
    expect((await call("find", { glob: "*.txt" })).text).toMatch(/^1 file\n\na\.txt \(3 lines, 14 bytes, \d+s ago\)$/)
    const info = await call("read", { paths: ["a.txt"], info: true })
    expect(info.text).toMatch(/^\[a\.txt#[0-9A-F]{4}\] 3 lines, 14 bytes, about 4 tokens; longest line 5 chars \(line 3\); LF; modified \d+s ago$/)
    const preview = await call("grep", { pattern: "t(\\w+)", replace: "T$1" })
    expect(preview.text).toContain("]\n>2|Two\n>3|Three\n")
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

  test("NIB_SANDBOX=1 keeps every tool inside the roots", async () => {
    const directory = await workspace()
    const outside = await workspace()
    await writeFile(path.join(outside, "secret.txt"), "secret answer\n")
    const readOutside = async (env: Record<string, string>) => {
      const client = new Client({ name: "test", version: "1" })
      await client.connect(new StdioClientTransport({ command: process.execPath, args: [path.join(import.meta.dir, "../src/main.ts")], cwd: directory, env, stderr: "pipe" }))
      try {
        return await client.callTool({ name: "read", arguments: { paths: [path.join(outside, "secret.txt")] } }) as { isError?: boolean; content: { text: string }[] }
      } finally { await client.close() }
    }
    const env = { PATH: process.env.PATH ?? "" }
    const boxed = await readOutside({ ...env, NIB_SANDBOX: "1" })
    expect(boxed.isError).toBe(true)
    expect(boxed.content[0]!.text).toBe(`${path.join(outside, "secret.txt")} is outside the sandbox, which allows only ${directory}`)
    expect((await readOutside(env)).content[0]!.text).toContain("secret answer")
  })
})
