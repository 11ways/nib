import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js"
import { RootsListChangedNotificationSchema } from "@modelcontextprotocol/sdk/types.js"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { z } from "zod"
import { Engine, GREP_CONTEXT_LINES, LINE_LIMIT, TEXT_LIMIT, type Context, type Result } from "./engine"

export const INSTRUCTIONS = `Hashline file tools; use them to list, read, search, edit and write text files, instead of the built-in Read, Grep, Glob, Edit and Write and instead of cat, head, sed, grep, find, ls, wc, cp or heredocs in the shell (built-in Read stays for PDFs and notebooks).
read and grep print a [PATH#TAG] header (TAG: 4-hex snapshot of the whole file) and LINE:TEXT rows. edit addresses those line numbers, so never repeat old text.
Save calls and tokens: grep for a name instead of reading a whole file; read several files and ranges in one read; edit straight from grep rows; put every change, across files, into one edit call; after an edit or write, use the new tag and rows from its response instead of re-reading.`

export const EDIT_DESCRIPTION = `Apply a hashline patch to one or more files. Numbers refer to the [PATH#TAG] output of your latest read, grep, write or edit of that file.

[PATH#TAG]
PUT N.=M:   replace lines N..M (PUT N: for one) with the + rows below
PUT N*:     replace the block starting at N (doc comment, decorators, body, closing line)
PUT <N:     insert + rows before line N
PUT >N:     insert + rows after line N
PUT >N*:    insert + rows after the block starting at N
PUT >$:     append + rows at end of file ([PATH] with no #TAG allows only this)
CUT N.=M    delete lines N..M (CUT N for one)
CUT N*      delete the block starting at N
SUB N: OLD  replace text OLD (once on line N) with the + rows; a lone + deletes it
REM         delete the file (only op in its section)
MV DEST     move the file, after the section's edits
CP DEST     like MV, but the file itself stays as it was
*** Add File: PATH
+content    new file; missing parents are created

Rules:
- Copy [PATH#TAG] exactly; one section per file. Body rows are + then the exact line, indent included; a lone + is a blank line. Never write - rows, context rows or LINE: prefixes.
- Every number in a section refers to that same output, never shifted by earlier hunks. Hunks must not overlap. Keep ranges tight; split distant changes into separate hunks.
- For a whole function, class or statement block use N* at its first line instead of counting to its end.
- Anchor lines shown as N:TEXT (grep context too) or N~TEXT. A ~TEXT row under PUT or CUT checks that line N starts with TEXT (a second ~ checks line M); checked, inserts may use any line and replaces any line you wrote or saw.
- The response has a new [PATH#TAG], a kept row on each side of a change, your new rows and notes for moved lines: use them for follow-ups. A stale tag is rebased when provably safe (with a warning), otherwise rejected with current content.

Example, for [src/a.ts#3C4D] showing 1:const a = 1 2:const b = 2 3:export { a }:
[src/a.ts#3C4D]
PUT 2.=2:
+const b = 3
PUT >3:
+export { b }`

const READ_DESCRIPTION = `Read text files as a [PATH#TAG] header plus LINE:TEXT rows, list directories, or view images. Pass every file you need in one call. A path may end in a line selector: path:A, path:A-B, path:A+COUNT, path:A- (to the end), path:-N (last N) or path:A-B,C-D. path#TAG, with a tag from an earlier read or edit, shows only what changed since (+N: added rows, -| removed ones). Without a selector, a file over 500 lines or 20 KiB returns an outline of its declarations, or for flat data its first 100 lines, so to find a name, grep for it instead. Lockfiles, minified files and node_modules/ or dist/ are skipped unless a range is given. One call shares a 40 KiB output budget. Rows cut at maxLineLength or holding color codes are bracketed and are not anchors; edit them with SUB.`

const GREP_DESCRIPTION = `Search file contents with ripgrep (Rust regex; .gitignore respected, hidden files included). Matches are grouped per file under [PATH#TAG] headers as *LINE:TEXT rows with LINE:TEXT context rows, all edit anchors. 20 files per page, 20 matches per file (100 for a single file, all with replace). Files over 2 MiB or still growing give N|TEXT rows, which are not anchors. replace previews each changed line as a >N| row and returns a token; edit with REAPPLY TOKEN applies exactly those changes.`

const FIND_DESCRIPTION = `List files recursively, like find or fd (.gitignore respected, hidden files included). Each row gives the path with its line count, size and age, like src/app.ts (120 lines, 3.4 KiB, 2h ago). 200 rows per page.`

const WRITE_DESCRIPTION = `Write a whole file, creating missing parent directories; overwriting an existing file needs the tag of its latest read or edit. Prefer edit for partial changes. The response maps the file with N~TEXT rows that edit can anchor on.`

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

const READ_ALIASES = ["filePath", "file_path", "path", "file"]

/**
 * Read's paths, also from arguments in another read tool's shape: a path under another name, with offset (taken as the first line) and limit.
 * A note in `notes` then shows nib's own shape for the same read, so the next call can use it.
 * @throws Error when no path is given at all
 */
function readPaths(args: Record<string, unknown>, notes: string[]): string[] {
  if (args.paths !== undefined) return listOf("paths", args.paths as string | string[], notes)!
  const name = READ_ALIASES.find((key) => typeof args[key] === "string" && args[key])
  if (!name) throw new Error('read needs paths, a list like ["src/a.ts", "src/b.ts:120-180"]')
  const file = args[name] as string
  const offset = Number(args.offset)
  const limit = Number(args.limit)
  const start = Number.isInteger(offset) && offset > 0 ? offset : 1
  const entry = Number.isInteger(limit) && limit > 0 ? `${file}:${start}+${limit}` : args.offset !== undefined ? `${file}:${start}-` : file
  const used = [name, ...["offset", "limit"].filter((key) => args[key] !== undefined)]
  notes.push(`[read takes paths, like ${JSON.stringify([entry])}; ${used.join(", ")} ${used.length === 1 ? "was" : "were"} read that way]`)
  return [entry]
}

/**
 * A list, also when it arrives as the JSON text of one, which small models send, or with the end of their tool call's markup after it, as in
 * `["a.ts"]>`; that is read as the list it spells, with a note. Any other text after the list leaves it one path.
 */
function listOf(name: string, value: string | string[] | undefined, notes: string[]) {
  if (typeof value === "string" && value.trimStart().startsWith("[")) {
    const list = (text: string) => {
      const parsed = (() => { try { return JSON.parse(text) as unknown } catch { return undefined } })()
      return Array.isArray(parsed) && parsed.length && parsed.every((item) => typeof item === "string" && item) ? parsed as string[] : undefined
    }
    const whole = list(value)
    if (whole) {
      notes.push(`[${name} was a string holding a JSON list; it was read as the list ${JSON.stringify(whole)}, so send the list itself]`)
      return whole
    }
    for (let end = value.indexOf("]"); end >= 0; end = value.indexOf("]", end + 1)) {
      const rest = value.slice(end + 1)
      const parsed = rest.trimStart().startsWith(">") ? list(value.slice(0, end + 1)) : undefined
      if (!parsed) continue
      const shown = rest.length > 40 ? `${rest.slice(0, 40)}...` : rest
      notes.push(`[${name} was a string holding a JSON list; it was read as the list ${JSON.stringify(parsed)}, and the tool-call markup after it (${JSON.stringify(shown)}) was dropped, so send the list itself]`)
      return parsed
    }
  }
  return value === undefined ? undefined : [value].flat()
}

/** A patch, also when it arrives as one line of JSON escapes (\n for each line break); one line can never be a patch, so that is decoded, with a note. */
function patchText(input: string, notes: string[]) {
  if (input.includes("\n") || !input.includes("\\n") || /^\s*REAPPLY /.test(input)) return input
  const quoted = input.length > 1 && input.startsWith('"') && input.endsWith('"') ? input : `"${input}"`
  const decoded = (() => { try { return JSON.parse(quoted) as unknown } catch { return undefined } })()
  if (typeof decoded !== "string") return input
  notes.push("[the patch came as one line with escaped line breaks (\\n); it was read with real line breaks, so send those]")
  return decoded
}

// Claude Code's Grep calls these output modes content and files_with_matches.
const MODE_ALIASES: Record<string, "matches" | "files"> = { content: "matches", files_with_matches: "files" }

/** grep's mode, also under another grep tool's name for it, with a note. */
function grepMode(value: string | undefined, notes: string[]) {
  if (value === undefined || value === "matches" || value === "files" || value === "count") return value
  const mode = MODE_ALIASES[value]
  if (!mode) throw new Error(`mode ${JSON.stringify(value)} is not one of matches, files or count`)
  notes.push(`[mode ${value} was read as ${mode}, nib's name for it]`)
  return mode
}

export type ServerOptions = { directory: string; session?: string; confirmOutsideRoots?: boolean; sandbox?: boolean }

/** Builds the MCP server; writes outside the client's roots proceed unless `confirmOutsideRoots` asks the user through elicitation, and `sandbox` refuses every path outside them. */
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
  const context = (signal: AbortSignal): Context => ({ directory: options.directory, session: options.session ?? "main", signal, roots: listRoots, authorize: options.confirmOutsideRoots ? authorize : allow, sandbox: options.sandbox })
  // Notes say what nib read differently than sent; they lead the response, a failed one too, so the next call can be sent right.
  const run = async (signal: AbortSignal, action: (ctx: Context, notes: string[]) => Promise<Result>) => {
    const notes: string[] = []
    try {
      const result = await action(context(signal), notes)
      return reply(notes.length ? { ...result, text: `${notes.join("\n")}\n\n${result.text}` } : result)
    } catch (error) {
      const failed = failure(error)
      return notes.length ? { ...failed, content: [{ type: "text" as const, text: `${notes.join("\n")}\n\n${failed.content[0]!.text}` }] } : failed
    }
  }

  // Numbers may arrive as strings and paths as one string; numbers over a limit are capped by the engine, with a note, rather than refused.
  const number = (min: number) => z.coerce.number().int().min(min).optional()
  const pathList = z.union([z.array(z.string().min(1)).min(1), z.string().min(1)])

  server.registerTool("read", {
    description: READ_DESCRIPTION,
    // Loose, so arguments in another read tool's shape reach readPaths instead of failing validation.
    inputSchema: z.object({
      paths: pathList.optional().describe("Required: files or directories, each optionally with a :selector, or path#TAG"),
      maxLineLength: number(1).describe(`Default ${LINE_LIMIT}`),
      plain: z.boolean().optional().describe("For logs and files you will not edit: no numbers or tags, color codes stripped, long lines shortened, repeats collapsed; any size"),
      collapse: z.boolean().optional().describe("Plain only: false keeps repeated lines"),
      info: z.boolean().optional().describe("One line of facts per file (tag, lines, size, line endings, age) instead of its content"),
    }).loose(),
    annotations: { readOnlyHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx, notes) => {
    const paths = readPaths(args, notes)
    return engine.read(ctx, { paths, maxLineLength: args.maxLineLength, plain: args.plain, collapse: args.collapse, info: args.info })
  }))

  server.registerTool("grep", {
    description: GREP_DESCRIPTION,
    inputSchema: z.object({
      pattern: z.string().min(1).describe("Regex; with literal:true a plain string"),
      paths: pathList.optional().describe("Files or directories; a file may carry a read selector like log.txt:100-250. Default the project"),
      glob: z.union([z.string(), z.array(z.string())]).optional().describe("Like *.ts or !*.test.ts, or a list"),
      ignoreCase: z.boolean().optional(),
      literal: z.boolean().optional(),
      before: number(0).describe("Context lines before each match; default 1, at most 20"),
      after: number(0).describe(`Context lines after each match; default 3, at most 20. Past ${GREP_CONTEXT_LINES} context lines in all, definitions get them first`),
      mode: z.string().optional().describe("matches (default); files: matching paths only; count: matches per file"),
      noIgnore: z.boolean().optional().describe("Also search ignored files"),
      maxLineLength: number(1).describe("Default 512"),
      skip: number(0).describe("Files to skip, for the next page"),
      replace: z.string().optional().describe("Replacement with $1 or ${name}; previews the changes and returns a token for edit"),
    }).loose(),
    annotations: { readOnlyHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx, notes) => {
    // Other grep tools name a single search root path.
    const alias = args.paths === undefined && typeof args.path === "string" && args.path ? args.path : undefined
    if (alias) notes.push(`[grep takes paths, a list like ${JSON.stringify([alias])}; path was read that way]`)
    const { path: _, ...rest } = args
    return engine.grep(ctx, { ...rest, mode: grepMode(args.mode, notes), paths: alias ? [alias] : listOf("paths", args.paths, notes), glob: listOf("glob", args.glob, notes) })
  }))

  server.registerTool("find", {
    description: FIND_DESCRIPTION,
    inputSchema: {
      paths: pathList.optional().describe("Directories or files to list; default the project"),
      glob: z.union([z.string(), z.array(z.string())]).optional().describe("Like *.ts, src/**/test_*.py or !*.min.js, or a list"),
      minSize: z.string().optional().describe("Like 100k or 2M"),
      newer: z.string().optional().describe("Modified within, like 30m, 2h or 3d"),
      sort: z.enum(["path", "size", "modified"]).optional().describe("path (default), size (largest first) or modified (newest first)"),
      noIgnore: z.boolean().optional().describe("Also list ignored files"),
      skip: number(0).describe("Files to skip, for the next page"),
    },
    annotations: { readOnlyHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx, notes) => engine.find(ctx, { ...args, paths: listOf("paths", args.paths, notes), glob: listOf("glob", args.glob, notes) })))

  server.registerTool("edit", {
    description: EDIT_DESCRIPTION,
    inputSchema: { input: z.string().min(1).describe("The complete hashline patch, or REAPPLY TOKEN from a refusal or a grep replace") },
    annotations: { destructiveHint: true },
    _meta: ALWAYS_LOAD,
  }, (args, extra) => run(extra.signal, (ctx, notes) => engine.patch(ctx, patchText(args.input, notes))))

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
