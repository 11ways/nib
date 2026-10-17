/**
 * The engine tests' Engine, driving the nib binary over stdio MCP, so the same assertions check the Go implementation through its real interface.
 * One binary runs per directory, session and context shape; a context's roots answer the server's roots requests, its authorize answers
 * elicitation, and an aborted signal cancels the call.
 */
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { ElicitRequestSchema, ListRootsRequestSchema } from "@modelcontextprotocol/sdk/types.js"
import { mkdir, rename, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { pathToFileURL } from "node:url"
import { afterAll } from "bun:test"

export type Context = {
  directory: string
  session: string
  signal: AbortSignal
  roots?: () => Promise<string[]>
  authorize?: (canonical: string, action: string) => Promise<void>
  sandbox?: boolean
}

export type Result = { text: string; images?: { data: string; mimeType: string }[] }

const REPO = path.join(import.meta.dir, "../..")
let built: Promise<string> | undefined

/**
 * Builds the nib binary once per test run, into one path per user that every run reuses, so runs leave nothing behind; the build goes to a
 * name of its own and is renamed into place, so a binary another run is executing is never overwritten.
 */
export function binary() {
  return built ??= (async () => {
    const directory = path.join(tmpdir(), `nib-test-${process.getuid?.() ?? "user"}`)
    await mkdir(directory, { recursive: true, mode: 0o700 })
    const output = path.join(directory, "nib")
    const staged = `${output}.${process.pid}`
    const child = Bun.spawn(["go", "build", "-o", staged, "./cmd/nib"], { cwd: REPO, stdout: "pipe", stderr: "pipe" })
    const [exit, stderr] = await Promise.all([child.exited, new Response(child.stderr).text()])
    if (exit !== 0) {
      await rm(staged, { force: true })
      throw new Error(`go build failed: ${stderr}`)
    }
    await rename(staged, output)
    return output
  })()
}

/** A connected client with the context its handlers answer for, set before each call. */
type Connection = { client: Client; current: Context }

const open: Client[] = []
afterAll(async () => { await Promise.all(open.splice(0).map((client) => client.close().catch(() => undefined))) })

/** Starts the binary for a context shape; extra environment, such as NIB_SHELL, joins the shape's own. */
export async function connect(ctx: Context, env: Record<string, string> = {}): Promise<Connection> {
  const capabilities = { ...ctx.roots && { roots: { listChanged: true } }, ...ctx.authorize && { elicitation: { form: {} } } }
  const client = new Client({ name: "engine-test", version: "1" }, { capabilities })
  const connection: Connection = { client, current: ctx }
  if (ctx.roots) {
    client.setRequestHandler(ListRootsRequestSchema, async () => ({ roots: (await connection.current.roots!()).map((root) => ({ uri: pathToFileURL(root).href })) }))
  }
  if (ctx.authorize) {
    client.setRequestHandler(ElicitRequestSchema, async (request) => {
      const match = /^nib wants to (\S+) (.+), which is outside the workspace\. Allow it\?$/.exec(request.params.message)
      if (!match) return { action: "decline" as const }
      try {
        await connection.current.authorize!(match[2]!, match[1]!)
        return { action: "accept" as const, content: { scope: "this file once" } }
      } catch {
        return { action: "decline" as const }
      }
    })
  }
  await client.connect(new StdioClientTransport({
    command: await binary(),
    cwd: ctx.directory,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "", XDG_STATE_HOME: process.env.XDG_STATE_HOME ?? "",
      NIB_DIRECTORY: ctx.directory,
      NIB_SHELL: "0",
      // Writes outside the roots go through authorize, which a context without one refuses, as the engine did.
      NIB_CONFIRM_OUTSIDE_ROOTS: "1",
      ...ctx.sandbox && { NIB_SANDBOX: "1" },
      ...env,
    },
    stderr: "pipe",
  }))
  open.push(client)
  return connection
}

/** The engine's methods, each one tool call; an error result becomes a thrown error, as the engine threw. */
export class Engine {
  readonly #connections = new Map<string, Promise<Connection>>()

  async #call(ctx: Context, name: string, args: Record<string, unknown>): Promise<Result> {
    ctx.signal.throwIfAborted()
    const key = [ctx.directory, ctx.session, !!ctx.sandbox, !!ctx.roots, !!ctx.authorize].join("\0")
    let pending = this.#connections.get(key)
    if (!pending) {
      pending = connect(ctx)
      this.#connections.set(key, pending)
    }
    const connection = await pending
    connection.current = ctx
    // The server keeps the roots it listed until told they changed; a context's roots may differ from call to call.
    if (ctx.roots) await connection.client.sendRootsListChanged()
    const result = await connection.client.callTool({ name, arguments: args }, undefined, { signal: ctx.signal, timeout: 120_000 }) as {
      isError?: boolean
      content: { type: string; text?: string; data?: string; mimeType?: string }[]
    }
    const text = result.content.find((item) => item.type === "text")?.text ?? ""
    if (result.isError) throw new Error(text)
    const images = result.content.filter((item) => item.type === "image").map((item) => ({ data: item.data!, mimeType: item.mimeType! }))
    return { text, images }
  }

  read(ctx: Context, args: { paths: string[]; maxLineLength?: number; plain?: boolean; collapse?: boolean; info?: boolean }) {
    return this.#call(ctx, "read", args)
  }

  grep(ctx: Context, args: Record<string, unknown> & { pattern: string }) {
    return this.#call(ctx, "grep", args)
  }

  find(ctx: Context, args: Record<string, unknown>) {
    return this.#call(ctx, "find", args)
  }

  patch(ctx: Context, input: string) {
    return this.#call(ctx, "edit", { input })
  }

  write(ctx: Context, args: { path: string; content: string; tag?: string }) {
    return this.#call(ctx, "write", args)
  }
}

/** Four uppercase hex digits of xxHash32 over the text with trailing spaces, tabs and CRs stripped per line; oh-my-pi's file_hash. */
export function fileTag(text: string) {
  return (Bun.hash.xxHash32(text.split("\n").map((line) => line.replace(/[ \t\r]+$/, "")).join("\n"), 0) & 0xffff).toString(16).toUpperCase().padStart(4, "0")
}
