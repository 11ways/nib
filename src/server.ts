import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { RootsListChangedNotificationSchema } from "@modelcontextprotocol/sdk/types.js"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { z } from "zod"
import { Engine, LINE_LIMIT, READ_LINES, TEXT_LIMIT, type Context, type Result } from "./engine"

export const INSTRUCTIONS = `Hashline file tools; use them to read, search, edit and write text files, instead of the built-in Read, Grep, Edit and Write and instead of cat, head, sed, grep, find or heredocs in the shell (built-in Read stays for PDFs and notebooks).
read and grep print a [PATH#TAG] header (TAG: 4-hex snapshot of the whole file) and LINE:TEXT rows. edit addresses those line numbers, so never repeat old text.
Save calls: read several files and ranges in one read; edit straight from grep rows; put every change, across files, into one edit call; after an edit, use the new tag and rows from its response instead of re-reading.`

export const EDIT_DESCRIPTION = `Apply a hashline patch to one or more files. Numbers refer to the [PATH#TAG] output of your latest read, grep or edit of that file. Write the sections directly, with no envelope lines.

[PATH#TAG]
PUT N.=M:   replace lines N..M (N.=N for one) with the + rows below
PUT N*:     replace the whole block starting at N (doc comment, decorators, body, closing line)
PUT <N:     insert + rows before line N
PUT >N:     insert + rows after line N
PUT >N*:    insert + rows after the block starting at N
PUT >$:     append + rows at end of file
CUT N.=M    delete lines N..M
CUT N*      delete the block starting at N
REM         delete the file (only op in its section)
MV DEST     move the file, after the section's edits
*** Add File: PATH
+content    new file; missing parents are created

Rules:
- Copy [PATH#TAG] exactly; one section per file. Body rows are + then the exact line, indent included; a lone + is a blank line. Never write - rows, context rows or LINE: prefixes.
- Every number in a section refers to that same output, never shifted by earlier hunks. Hunks must not overlap. Keep ranges tight; split distant changes into separate hunks.
- For a whole function, class or statement block use N* at its first line instead of counting to its closing line.
- Only anchor lines that output showed in full (grep context rows count). Otherwise the edit is rejected with their content and an unchanged retry succeeds.
- The response has a new [PATH#TAG] and numbered rows around each change: use them for follow-ups. A stale tag is rebased when provably safe (with a warning), otherwise rejected with current content.
- All files are validated before any is written; each is written atomically.

Example, for [src/a.ts#3C4D] showing 1:const a = 1 2:const b = 2 3:export { a }:
[src/a.ts#3C4D]
PUT 2.=2:
+const b = 3
PUT >3:
+export { b }`

const READ_DESCRIPTION = `Read text files as a [PATH#TAG] header plus LINE:TEXT rows, list directories, or view PNG/JPEG/GIF/WebP images. Pass every file you need in one call. Each entry may end in a line selector: path:A (that one line), path:A-B, path:A+COUNT, path:A- (from A on), path:-N (last N), or path:A-B,C-D. Without one: whole files up to 500 lines and 20 KiB; larger files return an outline (top-level declarations and class members with their line numbers, markdown headings) or, for flat data, the first 100 lines; lockfiles, minified files and files under node_modules/ or dist/ are skipped. Outline rows are editable; read the ranges you need, or path:1- for up to ${READ_LINES} lines. One call splits a 40 KiB output budget fairly across its files and ends with a continuation hint when cut. Lines over ${LINE_LIMIT} characters are truncated and cannot be edited until read alone as path:N with a larger maxLineLength.`

const GREP_DESCRIPTION = `Search file contents with ripgrep (Rust regex, .gitignore respected, hidden files included). Results are grouped per file under [PATH#TAG] headers with *LINE:TEXT match rows and  LINE:TEXT context rows; they are valid edit anchors, so edit straight from them. 20 files per page (continue with skip), 20 matches per file (200 when one file matches).`

const WRITE_DESCRIPTION = `Write a whole file. Creates it (and missing parent directories) when absent; overwriting an existing file requires the tag of its latest read or edit. Content is literal text. Prefer edit for partial changes.`

const ALWAYS_LOAD = { "anthropic/alwaysLoad": true }

function reply(result: Result) {
  return {
    content: [
      { type: "text" as const, text: result.text },
      ...(result.images ?? []).map((image) => ({ type: "image" as const, data: image.data, mimeType: image.mimeType })),
    ],
  }
}

function failure(error: unknown) {
  return { isError: true, content: [{ type: "text" as const, text: error instanceof Error ? error.message : String(error) }] }
}

export type ServerOptions = { directory: string; session?: string; confirmOutsideRoots?: boolean }

/** Builds the MCP server; writes outside the client's roots proceed unless `confirmOutsideRoots` asks the user through elicitation. */
export function createServer(options: ServerOptions) {
  const engine = new Engine()
  const server = new McpServer({ name: "nib", version: "0.1.0" }, { instructions: INSTRUCTIONS })
  const approved = new Set<string>()
  let roots: Promise<string[]> | undefined

  const listRoots = () => roots ??= (async () => {
    if (!server.server.getClientCapabilities()?.roots) return [options.directory]
    try {
      const listed = await server.server.listRoots()
      const paths = listed.roots.flatMap((root) => root.uri.startsWith("file://") ? [fileURLToPath(root.uri)] : [])
      return paths.length ? paths : [options.directory]
    } catch {
      roots = undefined
      return [options.directory]
    }
  })()

  server.server.setNotificationHandler(RootsListChangedNotificationSchema, async () => { roots = undefined })

  const authorize = async (canonical: string, action: string) => {
    let directory = path.dirname(canonical)
    for (;;) {
      if (approved.has(directory)) return
      const parent = path.dirname(directory)
      if (parent === directory) break
      directory = parent
    }
    if (!server.server.getClientCapabilities()?.elicitation) {
      throw new Error(`${canonical} is outside the workspace roots and this client cannot confirm it; refusing to ${action} it`)
    }
    const answer = await server.server.elicitInput({
      mode: "form",
      message: `nib wants to ${action} ${canonical}, which is outside the workspace. Allow it?`,
      requestedSchema: {
        type: "object",
        properties: { scope: { type: "string", title: "Allow", enum: ["this file once", `everything under ${path.dirname(canonical)} this session`] } },
        required: ["scope"],
      },
    })
    if (answer.action !== "accept") throw new Error(`The user declined to let nib ${action} ${canonical}`)
    if (String(answer.content?.scope ?? "").startsWith("everything")) approved.add(path.dirname(canonical))
  }

  const allow = async () => {}
  const context = (signal: AbortSignal): Context => ({ directory: options.directory, session: options.session ?? "main", signal, roots: listRoots, authorize: options.confirmOutsideRoots ? authorize : allow })
  const run = async (signal: AbortSignal, action: (ctx: Context) => Promise<Result>) => {
    try { return reply(await action(context(signal))) } catch (error) { return failure(error) }
  }

  server.registerTool("read", {
    description: READ_DESCRIPTION,
    inputSchema: {
      paths: z.array(z.string().min(1)).min(1).describe("Files or directories, absolute or relative to the project, each optionally with a :selector"),
      maxLineLength: z.number().int().min(1).max(TEXT_LIMIT).optional().describe(`Default ${LINE_LIMIT}; larger values need a single path:N entry`),
    },
    annotations: { readOnlyHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx) => engine.read(ctx, args)))

  server.registerTool("grep", {
    description: GREP_DESCRIPTION,
    inputSchema: {
      pattern: z.string().min(1).describe("Regex; with literal:true a plain string"),
      paths: z.array(z.string().min(1)).optional().describe("Files or directories to search; default the project"),
      glob: z.string().optional().describe("Include glob like *.ts or !*.test.ts"),
      ignoreCase: z.boolean().optional(),
      literal: z.boolean().optional(),
      before: z.number().int().min(0).max(20).optional().describe("Context lines before each match; default 1"),
      after: z.number().int().min(0).max(20).optional().describe("Context lines after each match; default 3"),
      skip: z.number().int().min(0).optional().describe("Matching files to skip, for the next page"),
    },
    annotations: { readOnlyHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx) => engine.grep(ctx, args)))

  server.registerTool("edit", {
    description: EDIT_DESCRIPTION,
    inputSchema: { input: z.string().min(1).describe("The complete hashline patch") },
    annotations: { destructiveHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx) => engine.patch(ctx, args.input)))

  server.registerTool("write", {
    description: WRITE_DESCRIPTION,
    inputSchema: {
      path: z.string().min(1),
      content: z.string().max(TEXT_LIMIT),
      tag: z.string().regex(/^[0-9A-Fa-f]{4}$/).optional().describe("Required to overwrite an existing file"),
    },
    annotations: { destructiveHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx) => engine.write(ctx, args)))

  return server
}
