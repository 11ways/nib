import { randomInt, randomUUID } from "node:crypto"
import { constants, type BigIntStats } from "node:fs"
import { lstat, link, mkdir, open, readdir, readFile, readlink, realpath, rename, stat, unlink } from "node:fs/promises"
import type { FileHandle } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import { anchorLines, fileTag, lineChanges, remapEdits, rescueShifted, resolveEdges, SnapshotStore, unverified, type LineEdit, type Version, type View } from "./snapshots"
import { parsePatch, type EditSection } from "./patch"

export const TEXT_LIMIT = 2 * 1024 * 1024
export const MEDIA_LIMIT = 5 * 1024 * 1024
/** Shared byte budget of one read or grep result; Claude Code warns above 10k tokens and truncates at 25k. */
export const OUTPUT_LIMIT = 40 * 1024
export const LINE_LIMIT = 2000
export const READ_LINES = 2000
const PREVIEW_CONTEXT = 2
// An edit response shows one kept row on each side of a change, where a leftover line or a misplaced range shows.
const EDIT_CONTEXT = 1
// Runs of new rows up to this long show every row whole, so a row that landed other than the agent meant shows; longer runs show their first and
// last row whole and a sparse map between them.
const WRITTEN_ROWS = 5
const REVEAL_LINES = 40
const REVEAL_CONTEXT = 2
const TRUSTED_CREATE_LINES = 10
const REVEAL_COLUMNS = 512
const GREP_COLUMNS = 512
const GREP_FILES = 20
const GREP_LIST_FILES = 200
// A count over more files than this sums them by folder instead.
const COUNT_ROWS = 40
const GREP_PER_FILE = 20
// One file gets more matches than a page of several, but no more than a harness's own grep shows (OpenCode stops at 100).
const GREP_SINGLE_FILE = 100
const GREP_TIMEOUT = 30_000
// Folders a search outside git walks looking for repositories, since one may ignore the repositories nested in it.
const FOLDERS_OUTSIDE_GIT = 2000
// An ignored folder holding more entries than this is a package or build cache, not a folder of repositories, and is not looked inside.
const CONTAINER_ENTRIES = 1000
const GREP_CONTEXT = 20
/** Context lines one grep page shows in all; past it definitions get theirs first, since around every match they multiply the output. One match always gets all it may ask for. */
export const GREP_CONTEXT_LINES = 2 * GREP_CONTEXT
const FIND_FILES = 200
const FIND_BATCH = 256
const FIND_SNIFF = 8000
const MAP_COLUMNS = 30
const MAP_GAP = 4
const MAP_STRIDE = 10
const MAP_ROWS = 300
const EDIT_MAP_ROWS = 40
const TOKENS = 64
const CADENCE_LINES = 8
const ECHO_MIN_LINES = 3
const ECHO_MAX_LINES = 200
const ECHO_CANDIDATES = 16
const WINDOWS = 3
const SINCE_NOTES = 10
const TOKEN_ALPHABET = "abcdefghijklmnopqrstuvwxyz0123456789"

/** Per-call state; `authorize` decides writes outside the workspace roots and throws to refuse; with `sandbox`, no tool touches anything outside them. */
export type Context = {
  directory: string
  session: string
  signal: AbortSignal
  roots?: () => Promise<string[]>
  authorize?: (canonical: string, action: string) => Promise<void>
  sandbox?: boolean
}

export type Image = { data: string; mimeType: string }
export type Result = { text: string; images?: Image[] }

const locks = new Map<string, Promise<void>>()

async function serialized<T>(key: string, ctx: Context, run: () => Promise<T>): Promise<T> {
  const previous = locks.get(key) ?? Promise.resolve()
  let release!: () => void
  const next = new Promise<void>((resolve) => { release = resolve })
  locks.set(key, next)
  try {
    await previous
    ctx.signal.throwIfAborted()
    return await run()
  } finally {
    release()
    if (locks.get(key) === next) locks.delete(key)
  }
}

/** Runs `run` holding the locks of all `keys`, taken in sorted order like `locked`'s so it cannot deadlock against an edit. */
function serializedAll<T>(keys: string[], ctx: Context, run: () => Promise<T>): Promise<T> {
  const sorted = [...new Set(keys)].sort()
  const step = (index: number): Promise<T> => index === sorted.length ? run() : serialized(sorted[index]!, ctx, () => step(index + 1))
  return step(0)
}

function missing(error: unknown) {
  return error instanceof Error && "code" in error && error.code === "ENOENT"
}

/** A path that names nothing; read, grep and find note it and still serve their other paths. */
class NotFound extends Error {}

/** A path the sandbox refuses; read, grep and find note it like a missing path, and an edit or write fails. */
class Outside extends Error {}

/** The client's roots, or the server's directory, canonicalized. */
async function workspaceRoots(ctx: Context) {
  return Promise.all((await ctx.roots?.() ?? [ctx.directory]).map((root) => realpath(root).catch(() => root)))
}

/**
 * Why the sandbox refuses `input`, if it is on and the path resolves outside every workspace root. Symlinks are followed, and a path that does not
 * exist yet is judged by its nearest existing parent, so a link inside cannot lead a read or a new directory out.
 */
async function outsideSandbox(ctx: Context, input: string) {
  if (!ctx.sandbox) return undefined
  let existing = path.resolve(ctx.directory, expandHome(input))
  let rest: string[] = []
  let real = await realpath(existing).catch(() => undefined)
  while (real === undefined && path.dirname(existing) !== existing) {
    rest = [path.basename(existing), ...rest]
    existing = path.dirname(existing)
    real = await realpath(existing).catch(() => undefined)
  }
  const canonical = path.join(real ?? existing, ...rest)
  const roots = await workspaceRoots(ctx)
  return roots.some((root) => within(root, canonical)) ? undefined : `${input} is outside the sandbox, which allows only ${roots.join(", ")}`
}

/** Why `entry` names nothing; one written path:A:B gets the range form, since a selector writes A to B as A-B. */
async function missingPath(entry: string, ctx: Context) {
  const pair = /^(.*):([1-9]\d*):([1-9]\d*)$/.exec(entry)
  if (!pair || !await stat(path.resolve(ctx.directory, expandHome(pair[1]!))).then(() => true, () => false)) return `${entry} does not exist`
  return `${entry} does not exist; for lines ${pair[2]} to ${pair[3]}, write ${pair[1]}:${pair[2]}-${pair[3]}`
}

function expandHome(input: string) {
  return input === "~" || input.startsWith("~/") ? path.join(os.homedir(), input.slice(1)) : input
}

type Target = { canonical: string; display: string; check(): Promise<void> }

/** True when the sandbox is off, or `canonical` lies inside a workspace root. */
async function allowed(ctx: Context, canonical: string) {
  return !ctx.sandbox || (await workspaceRoots(ctx)).some((root) => within(root, canonical))
}

/**
 * Fails unless the file `handle` holds lies inside the sandbox; the path it was opened by may have changed since it was judged. Linux names an open
 * file's real path; elsewhere only the path the descriptor was opened by can be judged.
 */
async function confined(ctx: Context, handle: FileHandle, opened: string) {
  if (!ctx.sandbox) return
  const real = process.platform === "linux" ? await readlink(`/proc/self/fd/${handle.fd}`) : await realpath(opened)
  if (!await allowed(ctx, real)) throw new Outside(`${opened} resolves outside the sandbox`)
}

/**
 * Canonicalizes a path; `create` resolves the parent only, so a final symlink is the target itself rather than followed. The sandbox judges the
 * canonical path this returns, so a link changed between the two resolutions cannot lead outside.
 */
async function resolveTarget(input: string, ctx: Context, create = false): Promise<Target> {
  ctx.signal.throwIfAborted()
  const refused = await outsideSandbox(ctx, input)
  if (refused) throw new Outside(refused)
  const requested = path.resolve(ctx.directory, expandHome(input))
  const locate = async () => create
    ? path.join(await realpath(path.dirname(requested)), path.basename(requested))
    : await realpath(requested)
  const canonical = await locate().catch((error: unknown) => {
    if (missing(error)) throw new NotFound(`${input} does not exist${create ? " (parent directory missing)" : ""}`)
    throw error
  })
  if (!await allowed(ctx, canonical)) throw new Outside(`${input} resolves outside the sandbox, which allows only ${(await workspaceRoots(ctx)).join(", ")}`)
  const base = await realpath(ctx.directory).catch(() => ctx.directory)
  const relative = path.relative(base, canonical)
  const inside = relative !== "" && !relative.startsWith("..") && !path.isAbsolute(relative)
  return {
    canonical,
    display: inside ? relative : canonical,
    async check() {
      ctx.signal.throwIfAborted()
      if (await locate() !== canonical || await realpath(path.dirname(canonical)) !== path.dirname(canonical)) {
        throw new Error("Path changed during operation; read again")
      }
    },
  }
}

function within(root: string, canonical: string) {
  const relative = path.relative(root, canonical)
  return relative === "" || (!relative.startsWith("..") && !path.isAbsolute(relative))
}

/** Writes inside a workspace root proceed; anything else needs `ctx.authorize`, and is refused without it. */
async function authorizeWrite(ctx: Context, canonical: string, action: string) {
  const roots = await workspaceRoots(ctx)
  if (roots.some((root) => within(root, canonical))) return
  if (!ctx.authorize) throw new Error(`${canonical} is outside the workspace roots (${roots.join(", ")}); refusing to ${action} it`)
  await ctx.authorize(canonical, action)
}

type Parent = { directory: string; location: string; check(): Promise<void>; sync(): Promise<void> }

async function inParent<T>(target: Target, run: (parent: Parent) => Promise<T>) {
  const directory = path.dirname(target.canonical)
  const handle = process.platform === "linux"
    ? await open(directory, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW)
    : undefined
  try {
    const info = handle ? await handle.stat({ bigint: true }) : await stat(directory, { bigint: true })
    const pinned = handle ? `/proc/self/fd/${handle.fd}` : directory
    const parent = {
      directory: pinned,
      location: path.basename(target.canonical) ? path.join(pinned, path.basename(target.canonical)) : `${pinned}/.`,
      async check() {
        await target.check()
        const current = await lstat(directory, { bigint: true })
        if (current.dev !== info.dev || current.ino !== info.ino || (handle && await readlink(pinned) !== directory)) {
          throw new Error("Parent directory changed; read again")
        }
      },
      async sync() { if (handle) await handle.sync() },
    }
    await parent.check()
    return await run(parent)
  } finally { await handle?.close() }
}

async function checkDescriptor(file: FileHandle, location: string, canonical: string) {
  const info = await file.stat({ bigint: true })
  const current = await lstat(location, { bigint: true })
  if (current.dev !== info.dev || current.ino !== info.ino ||
      (process.platform === "linux" && await readlink(`/proc/self/fd/${file.fd}`) !== canonical)) {
    throw new Error("File descriptor path changed; read again")
  }
}

type PublishOptions = { replace?: BigIntStats; mode?: number; validate?: () => Promise<void> }

// Linux pins the parent; other platforms only recheck paths/inodes. Neither is a sandbox or external CAS.
async function publish(parent: Parent, canonical: string, bytes: Buffer, ctx: Context, options: PublishOptions = {}) {
  const { replace, validate } = options
  await parent.check()
  ctx.signal.throwIfAborted()
  const name = `.nib-${randomUUID()}.tmp`
  const location = path.join(parent.directory, name)
  const file = await open(location, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600)
  let published = false
  try {
    await parent.check()
    await checkDescriptor(file, location, path.join(path.dirname(canonical), name))
    let written = 0
    while (written < bytes.length) {
      ctx.signal.throwIfAborted()
      const result = await file.write(bytes, written, Math.min(64 * 1024, bytes.length - written), written)
      if (!result.bytesWritten) throw new Error("Staged write made no progress")
      written += result.bytesWritten
    }
    const info = await file.stat({ bigint: true })
    if (replace && (replace.uid !== info.uid || replace.gid !== info.gid)) {
      await file.chown(Number(replace.uid), Number(replace.gid))
    }
    // Replacement preserves POSIX ownership/mode, not ACLs or extended attributes.
    await file.chmod(replace ? Number(replace.mode & 0o7777n) : options.mode ?? 0o666 & ~process.umask())
    await file.sync()
    await validate?.()
    await parent.check()
    await checkDescriptor(file, location, path.join(path.dirname(canonical), name))
    ctx.signal.throwIfAborted()
    if (replace) await rename(location, parent.location)
    else await link(location, parent.location)
    published = true
  } finally {
    try {
      // Never sweep temp-name patterns or remove a replacement someone else put at our unique name.
      const current = await lstat(location, { bigint: true }).catch((error: unknown) => {
        if (missing(error)) return undefined
        throw error
      })
      const owned = await file.stat({ bigint: true })
      if (current?.dev === owned.dev && current.ino === owned.ino) await unlink(location)
    } catch (error) {
      if (published) throw new Error("File was published, but stage cleanup failed", { cause: error })
      throw error
    } finally { await file.close() }
  }
  if (published) {
    await parent.sync().catch((error: unknown) => {
      throw new Error("File was published, but parent directory fsync failed", { cause: error })
    })
  }
}

/** A file that changed while it was read, such as a log still being written; grep falls back to ripgrep's own rows for it. */
class ChangedWhileReading extends Error {}

async function snapshot(file: FileHandle, limit: number) {
  const before = await file.stat({ bigint: true })
  if (!before.isFile()) throw new Error("Only regular files are supported")
  if (before.size > BigInt(limit)) throw new Error(`File exceeds ${limit} byte size limit`)
  const buffer = Buffer.alloc(Number(before.size) + 1)
  let length = 0
  while (length < buffer.length) {
    const result = await file.read(buffer, length, buffer.length - length, length)
    if (!result.bytesRead) break
    length += result.bytesRead
  }
  const after = await file.stat({ bigint: true })
  if (length !== Number(before.size) || before.mtimeNs !== after.mtimeNs || before.ctimeNs !== after.ctimeNs) {
    throw new ChangedWhileReading("File changed while reading; read again")
  }
  return { bytes: buffer.subarray(0, length), info: after }
}

const BOM = String.fromCharCode(0xfeff)

type Text = { decoded: string; bom: string; body: string; lines: string[]; ending: string }

function text(bytes: Buffer): Text {
  if (bytes.length > TEXT_LIMIT) throw new Error(`Text exceeds ${TEXT_LIMIT} byte size limit`)
  if (mime(bytes)) throw new Error("Binary attachments cannot be edited as text")
  // ESC stays allowed: terminal colors are common in logs, and rows show such lines with the codes stripped.
  if (bytes.some((byte) => byte < 9 || (byte > 13 && byte < 32 && byte !== 27) || byte === 127)) throw new Error("Binary files are not supported")
  const decoded = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes)
  const bom = decoded.startsWith(BOM) ? BOM : ""
  const body = decoded.slice(bom.length)
  const lines = body.match(/[^\n]*\n|[^\n]+$/g) ?? [""]
  return { decoded, bom, body, lines, ending: body.includes("\r\n") ? "\r\n" : "\n" }
}

function mime(bytes: Buffer) {
  if (bytes.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))) return "image/png"
  if (bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) return "image/jpeg"
  if (/^GIF8[79]a/.test(bytes.subarray(0, 6).toString("ascii"))) return "image/gif"
  if (bytes.subarray(0, 4).toString() === "RIFF" && bytes.subarray(8, 12).toString() === "WEBP") return "image/webp"
  if (bytes.subarray(0, 5).toString() === "%PDF-") return "application/pdf"
}

function lineContent(raw: string) { return raw.replace(/\r?\n$/, "") }

function view(source: Text): View {
  return { key: source.body.replaceAll("\r\n", "\n"), lines: source.lines.map(lineContent), tag: fileTag(source.body) }
}

function ranges(lines: number[]) {
  const parts: string[] = []
  for (let index = 0; index < lines.length;) {
    let last = index
    while (last + 1 < lines.length && lines[last + 1] === lines[last]! + 1) last++
    parts.push(last === index ? `${lines[index]}` : `${lines[index]}-${lines[last]}`)
    index = last + 1
  }
  return parts.join(", ")
}

// Terminal escape sequences: CSI (colors), OSC (ended by BEL or ST), charset selection, other two-byte escapes, and a lone ESC.
const ESCAPES = /\x1b(?:\[[0-?]*[ -\/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()][0-9A-Za-z]|[@-_])?/g

/** The text a row shows for a line, with terminal escape codes removed. */
function visible(content: string) {
  return content.includes("\x1b") ? content.replace(ESCAPES, "") : content
}

type Marks = { cut?: boolean; colored?: boolean }
type Span = { start: number; end: number }

/** Character offsets of the first WINDOWS submatches of a match on one line; ripgrep reports UTF-8 byte offsets, and multi-line matches get none. */
function matchSpans(text: string, submatches: Span[]): Span[] {
  const body = text.replace(/\r?\n$/, "")
  if (body.includes("\n")) return []
  const bytes = Buffer.from(body)
  return submatches.slice(0, WINDOWS).map(({ start, end }) => ({ start: bytes.subarray(0, start).toString().length, end: bytes.subarray(0, end).toString().length }))
}

/** Where `after` differs from `before`: everything between their common start and common end, at least one character wide. */
function changedSpan(before: string, after: string): Span {
  let start = 0
  while (start < before.length && start < after.length && before[start] === after[start]) start++
  let end = 0
  while (end < before.length - start && end < after.length - start && before[before.length - 1 - end] === after[after.length - 1 - end]) end++
  return { start, end: Math.max(start + 1, after.length - end) }
}

/**
 * The stretches around the first matches of a long line, for when cutting it at `columns` would hide the first match; they merge when they touch.
 * @return the row label and text, or undefined when a cut at the start already shows the first match
 */
function matchWindows(content: string, spans: Span[] | undefined, columns: number) {
  if (!spans?.length || spans[0]!.end <= columns) return
  const reach = Math.max(20, Math.floor(columns / (2 * WINDOWS)))
  const windows: Span[] = []
  for (const span of spans) {
    const start = Math.max(0, span.start - reach)
    const end = Math.min(content.length, span.end + reach, start + columns)
    const last = windows.at(-1)
    if (last && start <= last.end) last.end = Math.min(Math.max(last.end, end), last.start + columns)
    else windows.push({ start, end })
  }
  return {
    label: `chars ${windows.map((window) => `${window.start + 1}-${window.end}`).join(", ")} of ${content.length}`,
    text: windows.map((window) => visible(content.slice(window.start, window.end))).join(" ... "),
  }
}

/** An `N:TEXT` row, or a bracketed row that is deliberately never an anchor: for a line longer than `limit`, or one whose escape codes were stripped. */
function lineRow(line: number, content: string, limit: number): { text: string; anchor: boolean } & Marks {
  const shown = visible(content)
  const colored = shown !== content
  const cut = shown.length > limit
  if (!colored && !cut) return { text: `${line}:${content}`, anchor: true }
  const label = [colored ? "ansi" : "", cut ? `cut at ${limit} of ${shown.length} chars` : ""].filter(Boolean).join(", ")
  return { text: `[${line} ${label}]|${shown.slice(0, limit)}`, anchor: false, cut, colored }
}

/** Footer notes explaining the bracketed rows among `rows`, once per block. */
function rowNotes(display: string, rows: Marks[]) {
  return [
    ...rows.some((row) => row.cut) ? [`[cut rows: change them in place with SUB N: OLD, or read ${display}:N with maxLineLength to see one whole]`] : [],
    ...rows.some((row) => row.colored) ? ["[ansi rows: their color codes are stripped, so they are not anchors; change them in place with SUB N: OLD]"] : [],
  ]
}

/** Rows around `marked` lines (prefixed `*`, others ` `), bracketed past `columns`; `shown` lists the rows displayed in full. */
function contextRows(lines: string[], marked: number[], radius: number, columns: number) {
  const flagged = new Set(marked)
  const display = new Set<number>()
  for (const anchor of flagged) {
    for (let line = Math.max(1, anchor - radius); line <= Math.min(lines.length, anchor + radius); line++) display.add(line)
  }
  const rows: string[] = []
  const shown: number[] = []
  const marks: Marks[] = []
  let previous = 0
  for (const line of [...display].sort((a, b) => a - b)) {
    if (previous && line > previous + 1) rows.push("...")
    const row = lineRow(line, lines[line - 1]!, columns)
    rows.push((flagged.has(line) ? "*" : " ") + row.text)
    if (row.anchor) shown.push(line)
    else marks.push(row)
    previous = line
  }
  return { rows, shown, marks }
}

/** The first MAP_COLUMNS characters after the indentation, with an ellipsis when the line goes on. */
function clip(content: string) {
  const text = visible(content)
  const indent = text.length - text.trimStart().length
  const rest = [...text.slice(indent)]
  return rest.length > MAP_COLUMNS ? text.slice(0, indent) + rest.slice(0, MAP_COLUMNS).join("") + "..." : text
}

/**
 * Lines a sparse row map shows in `from..to`: a non-blank line after a blank one once MAP_GAP lines passed since the last shown row,
 * and every MAP_STRIDE-th non-blank line without one; `first` and `last` add those non-blank lines, and `after` is the last row already shown.
 */
function mapLines(lines: string[], from: number, to: number, options: { first?: boolean; last?: boolean; after?: number } = {}) {
  const blank = (line: number) => !lines[line - 1]!.trim()
  let first = from
  let last = to
  while (first <= last && blank(first)) first++
  while (last >= first && blank(last)) last--
  const picked: number[] = []
  let previous = options.after ?? Number.NEGATIVE_INFINITY
  let count = 0
  for (let line = first; line <= last; line++) {
    if (blank(line)) continue
    count++
    if ((options.first && line === first) || (options.last && line === last) || (line > 1 && blank(line - 1) && line - previous >= MAP_GAP) || count === MAP_STRIDE) {
      picked.push(line)
      previous = line
      count = 0
    }
  }
  return picked
}

/** The response rows for a file the agent just wrote: a row map, or past MAP_ROWS rows the outline a read would give. */
function writtenMap(display: string, created: View) {
  const lines = created.lines
  const counts = `${lines.length} lines, ${lines.filter((content) => content.trim()).length} non-blank`
  const picked = mapLines(lines, 1, lines.length, { first: true, last: true })
  if (picked.length <= MAP_ROWS) return { summary: `${counts}, ${picked.length} shown`, rows: picked.map((line) => `${line}~${clip(lines[line - 1]!)}`), seen: picked, mapped: true }
  const sketch = outline(display, lines)
  if (sketch.rows.filter((row) => "line" in row).length < OUTLINE_MIN_ROWS || sketch.candidates * 2 > lines.filter((content) => content.trim()).length) {
    return { summary: `${counts}; too long to map`, rows: [], seen: [] }
  }
  const rows = sketch.rows.map((row) => "line" in row ? { ...lineRow(row.line, lines[row.line - 1]!, LINE_LIMIT), line: row.line } : { text: row.text, anchor: false })
  return { summary: `${counts}; outline only`, rows: rows.map((row) => row.text), seen: rows.flatMap((row) => row.anchor && "line" in row ? [row.line] : []) }
}

/** A refused edit; `retry` means it failed only on lines it has now displayed, so the same patch succeeds when sent again. */
class Refusal extends Error {
  constructor(message: string, readonly retry = false) { super(message) }
}

/** What a check of a section's hunks leaves: the edits that passed it, and a refusal naming those that did not. */
type Checked = { edits: LineEdit[]; refusal?: Refusal }

/** One refusal for all of a section's problems; the patch may be sent again unchanged only if each of them allows it. */
function together(problems: Refusal[]) {
  return problems.length === 1 ? problems[0]! : new Refusal(problems.map((problem) => problem.message).join("\n\n"), problems.every((problem) => problem.retry))
}

// Mirrors oh-my-pi's unseen-line guard: reveal what the model would have touched and let a straight retry pass.
function unseenError(store: SnapshotStore, session: string, target: Target, current: View, unseen: number[], known?: Set<number>) {
  const header = `This edit anchors to lines ${ranges(unseen)} of ${target.display} that [${target.display}#${current.tag}] never displayed in full (partial read, truncated line, or rows elided from an edit response).`
  if (unseen.length > REVEAL_LINES || unseen.some((line) => { const content = current.lines[line - 1] ?? ""; return content.length > REVEAL_COLUMNS || visible(content) !== content })) {
    const { rows } = contextRows(current.lines, unseen.slice(0, REVEAL_LINES), 0, REVEAL_COLUMNS)
    return new Refusal(`${header} Preview of the first ${Math.min(unseen.length, REVEAL_LINES)} unseen line(s):\n${rows.join("\n")}\nRead the remaining lines in full before re-issuing the edit; change a cut or ansi line in place with SUB N: OLD.`)
  }
  const { rows, shown } = contextRows(current.lines, unseen, REVEAL_CONTEXT, REVEAL_COLUMNS)
  store.record(session, target.canonical, current, shown)
  const written = unseen.some((line) => known?.has(line))
    ? " Lines you wrote, or saw before an edit moved them, can skip this step: a ~TEXT row with the start of the line, right under the PUT or CUT line, confirms it."
    : ""
  return new Refusal(`${header} Actual file content (* marks those lines):\n${rows.join("\n")}\nIf these are the lines you meant, the same patch now succeeds; otherwise fix your line numbers.${written}`, true)
}

function mismatchError(store: SnapshotStore, session: string, target: Target, current: View, tag: string, recognized: boolean, anchors: number[], appendOnly = false) {
  const { rows, shown } = contextRows(current.lines, anchors, 2, LINE_LIMIT)
  store.record(session, target.canonical, current, shown)
  const lines = recognized
    ? [`Edit rejected for ${target.display}: file changed between read and edit and the edit could not be rebased safely.`,
      `Tag #${tag} is stale; the current file is [${target.display}#${current.tag}]. If a prior edit in this session modified this file, use the tag and numbering from that edit's response; otherwise read the file again.`]
    : [`Edit rejected for ${target.display}: tag #${tag} was not issued for this file in this session.`,
      ...store.origins(session, tag, target.canonical).map((origin) => `Tag #${tag} was issued for ${origin}.`),
      `The current file is [${target.display}#${current.tag}]. Read the file to get a current tag; never invent tags.`,
      ...appendOnly ? [`To append without a tag, use a [${target.display}] header with only PUT >$: hunks.`] : []]
  return new Error([...lines, ...rows.length ? ["", "Current content around the requested lines (* marks them):", ...rows] : []].join("\n"))
}

/**
 * Marks the lines whose ~ check rows match: a check is the start of the line after its indentation, and an empty one matches a blank line.
 * @return the edits whose checks all match, and a refusal naming the others
 */
function confirmChecks(named: string, display: string, lines: string[], edits: LineEdit[], record?: (shown: number[]) => void): Checked {
  const failures: { line: number; check: string }[] = []
  const confirmed = edits.flatMap((edit) => {
    if (!edit.checks) return [edit]
    const targets = edit.checks.length === 2 ? [edit.start, edit.end] : [edit.start]
    const matched: number[] = []
    const before = failures.length
    edit.checks.forEach((check, index) => {
      const line = targets[index]!
      if (line > lines.length) throw new Error(`Line ${line} does not exist in ${display} (file has ${lines.length} lines)`)
      const wanted = check.trim()
      const content = lines[line - 1]!
      if (wanted ? content.trimStart().startsWith(wanted) : !content.trim()) matched.push(line)
      else failures.push({ line, check: wanted })
    })
    return failures.length > before ? [] : [{ ...edit, confirmed: matched }]
  })
  if (!failures.length) return { edits: confirmed }
  const { rows, shown, marks } = contextRows(lines, failures.map((failure) => failure.line), REVEAL_CONTEXT, REVEAL_COLUMNS)
  record?.(shown)
  return { edits: confirmed, refusal: new Refusal([
    `${named}: ~ check rows do not match, so these line numbers are off:`,
    ...failures.map(({ line, check }) => check ? `line ${line} does not start with ${JSON.stringify(check)}` : `line ${line} is not blank`),
    "",
    `Lines of ${named} around them (* marks them):`,
    ...rows,
    ...rowNotes(display, marks),
  ].join("\n")) }
}

/**
 * Turns SUB edits into one replace per line; each old text must occur exactly once in that line, and two on one line must not overlap.
 * @return the other edits and the SUBs that resolved, and a refusal naming the rest
 */
function resolveSubs(named: string, display: string, lines: string[], edits: LineEdit[], record?: (shown: number[]) => void): Checked {
  if (!edits.some((edit) => edit.sub !== undefined)) return { edits }
  const failures: { line: number; message: string }[] = []
  const done = new Set<number>()
  const resolved: LineEdit[] = []
  for (const edit of edits) {
    if (edit.sub === undefined) {
      resolved.push(edit)
      continue
    }
    const line = edit.start
    if (done.has(line)) continue
    done.add(line)
    if (line > lines.length) throw new Error(`Line ${line} does not exist in ${display} (file has ${lines.length} lines)`)
    const content = lines[line - 1]!
    const spans: { at: number; old: string; content: string[] }[] = []
    for (const sub of edits.filter((other) => other.sub !== undefined && other.start === line)) {
      const old = sub.sub!
      const at = content.indexOf(old)
      if (at < 0) failures.push({ line, message: `SUB ${line}: ${JSON.stringify(old)} does not occur on line ${line}` })
      else if (content.indexOf(old, at + 1) >= 0) failures.push({ line, message: `SUB ${line}: ${JSON.stringify(old)} occurs more than once on line ${line}; quote more of the line` })
      else spans.push({ at, old, content: sub.content })
    }
    if (failures.some((failure) => failure.line === line)) continue
    spans.sort((a, b) => a.at - b.at)
    if (spans.some((span, index) => index > 0 && span.at < spans[index - 1]!.at + spans[index - 1]!.old.length)) {
      failures.push({ line, message: `SUB ${line}: two old texts overlap on line ${line}; merge them into one SUB` })
      continue
    }
    let composed = ""
    let cursor = 0
    for (const span of spans) {
      composed += content.slice(cursor, span.at) + span.content.join("\n")
      cursor = span.at + span.old.length
    }
    resolved.push({ start: line, end: line, operation: "replace", content: (composed + content.slice(cursor)).split("\n"), quoted: true })
  }
  if (!failures.length) return { edits: resolved }
  const { rows, shown, marks } = contextRows(lines, [...done].filter((line) => failures.some((failure) => failure.line === line)), REVEAL_CONTEXT, REVEAL_COLUMNS)
  record?.(shown)
  return { edits: resolved, refusal: new Refusal([
    `${named}: SUB refused:`,
    ...failures.map((failure) => failure.message),
    "",
    `Lines of ${named} around them (* marks them):`,
    ...rows,
    ...rowNotes(display, marks),
  ].join("\n")) }
}

const trailing = (line: string) => line.replace(/[ \t\r]+$/, "")

/**
 * Checks diff-style - rows against the lines their hunk replaces; matching rows quote those lines, so like SUB they need no earlier read.
 * Trailing whitespace is ignored, and a line with color codes may be quoted as rows show it, without them.
 * @return the edits whose - rows match, and a refusal naming the others
 */
function confirmOld(named: string, display: string, lines: string[], edits: LineEdit[], record?: (shown: number[]) => void): Checked {
  if (!edits.some((edit) => edit.old)) return { edits }
  const failures: { line: number; message: string }[] = []
  const checked = edits.flatMap((edit) => {
    if (!edit.old) return [edit]
    const before = failures.length
    const replaced = lines.slice(edit.start - 1, edit.end)
    const span = edit.start === edit.end ? `line ${edit.start}` : `lines ${edit.start}-${edit.end}`
    if (edit.end > lines.length) failures.push({ line: Math.min(edit.start, lines.length), message: `${span} do not all exist (the file has ${lines.length} lines)` })
    else if (edit.old.length !== replaced.length) failures.push({ line: edit.start, message: `the - rows under ${span} quote ${edit.old.length} line${edit.old.length === 1 ? "" : "s"}, but the hunk replaces ${replaced.length}` })
    else {
      edit.old.forEach((quote, offset) => {
        const actual = replaced[offset]!
        if (trailing(quote) !== trailing(actual) && trailing(quote) !== trailing(visible(actual))) failures.push({ line: edit.start + offset, message: `line ${edit.start + offset} is not ${JSON.stringify(quote)}` })
      })
    }
    return failures.length > before ? [] : [{ ...edit, quoted: true as const }]
  })
  if (!failures.length) return { edits: checked }
  const { rows, shown, marks } = contextRows(lines, failures.map((failure) => failure.line), REVEAL_CONTEXT, REVEAL_COLUMNS)
  record?.(shown)
  return { edits: checked, refusal: new Refusal([
    `${named}: the - rows do not match the file:`,
    ...failures.map((failure) => failure.message),
    "",
    `Lines of ${named} around them (* marks them):`,
    ...rows,
    ...rowNotes(display, marks),
    "",
    "nib needs no - rows: PUT N.=M: with only + rows replaces those lines.",
  ].join("\n")) }
}

/** A note for each hunk header the parser read as nib's own form, so the next patch writes that form. */
function writtenLessons(edits: LineEdit[]) {
  return edits.flatMap((edit) => edit.written ? [`${edit.written} was read as PUT ${edit.start}.=${edit.end}:; a range is written N.=M, with a dot.`] : [])
}

/** The note for a patch whose - rows matched: the edit went through, but those rows cost tokens nib never needs, so it shows the hunk without them. */
function oldRowsLesson(edits: LineEdit[]) {
  const edit = edits.find((item) => item.old)
  if (!edit) return []
  const header = edit.content.length ? edit.start === edit.end ? `PUT ${edit.start}:` : `PUT ${edit.start}.=${edit.end}:` : edit.start === edit.end ? `CUT ${edit.start}` : `CUT ${edit.start}.=${edit.end}`
  return [[
    "The - rows matched the file, so the edit was applied; but nib never needs the old text, since the line numbers already say what a hunk replaces.",
    "Leave - rows out next time. This hunk is just:",
    header,
    ...edit.content.slice(0, 3).map((line) => `+${line.length > 120 ? line.slice(0, 120) + "..." : line}`),
    ...edit.content.length > 3 ? [`(and ${edit.content.length - 3} more + rows)`] : [],
  ].join("\n")]
}

/** `was` is the line a one-for-one replacement overwrote, so a long changed line can show where it changed. */
type Row = { raw: string; origin?: number; opaque?: true; was?: string }

// Rows keep their original index so the response can carry seen lines and number the changes.
function applyChanges(source: Text, changes: (LineEdit & { raw: string[] })[]) {
  const rows: Row[] = []
  const runs: { at: number; length: number }[] = []
  const keep = (from: number, to: number) => {
    for (let index = from; index < to; index++) if (source.lines[index] !== "") rows.push({ raw: source.lines[index]!, origin: index })
  }
  let cursor = 0
  for (const change of changes) {
    const at = change.operation === "after" ? change.end : change.start - 1
    keep(cursor, at)
    runs.push({ at: rows.length, length: change.raw.length })
    // A replacement with as many rows as lines it replaces pairs them up, so each new row knows the line it overwrote.
    const paired = change.operation === "replace" && change.raw.length === change.end - change.start + 1
    change.raw.forEach((raw, offset) => {
      rows.push({ raw, ...change.opaque && { opaque: true as const }, ...paired && { was: lineContent(source.lines[change.start - 1 + offset]!) } })
    })
    cursor = at + (change.operation === "replace" ? change.end - change.start + 1 : 0)
  }
  keep(cursor, source.lines.length)
  // A file without a final newline that gets an empty last row has gained its final newline, not a line: the row ends the line before it.
  const terminated = source.body !== "" && !source.body.endsWith("\n") && rows.length > 1 && rows.at(-1)!.origin === undefined && lineContent(rows.at(-1)!.raw) === ""
  if (terminated) {
    const last = rows.length - 1
    rows.pop()
    // The row belongs to whichever run wrote it, and a deletion after it now sits at the new end.
    runs.findLast((run) => run.at <= last && last < run.at + run.length)!.length--
    for (const run of runs) run.at = Math.min(run.at, rows.length)
  }
  // An unterminated original last line needs a separator when inserting after it.
  const body = rows.map((row, index) => index < rows.length - 1 && !row.raw.endsWith("\n") ? row.raw + source.ending : row.raw).join("")
  const final = body.replace(/\r?\n$/, "") + (terminated ? source.ending : body !== "" ? source.body.match(/\r?\n$/)?.[0] ?? "" : "")
  return { rows, runs, bytes: Buffer.from(source.bom + final), terminated }
}

// How a file without a final newline gets one; edits keep a file's final-newline state otherwise.
const FINAL_NEWLINE = "PUT >$: with a lone + row adds one"
// The model wrote every line of a new file, so a ~ check of its start anchors a line the response does not show.
const WRITTEN_CHECK = "[to anchor a line not shown, put a ~TEXT row with its start under the PUT or CUT; no read needed]"
// Past the row map's size the response shows no rows or only an outline, but the model still knows every line it wrote.
const WRITTEN_UNMAPPED = "[every line is known: grep finds a line's number, and a ~TEXT row with its start under the PUT or CUT anchors it; no read needed]"

/**
 * Content for a whole-file overwrite, in the file's own style: plain line breaks become CRLF in a file that uses only CRLF, and its BOM stays,
 * unless the content brings its own; models rarely type a CR, so plain breaks mean line breaks, not LF.
 * @return the content and what of the old style it kept
 */
function inStyleOf(current: Text, content: string) {
  const kept: string[] = []
  let styled = content
  if (current.body.includes("\r\n") && !/(^|[^\r])\n/.test(current.body) && styled.includes("\n") && !styled.includes("\r\n")) {
    styled = styled.replaceAll("\n", "\r\n")
    kept.push("CRLF line endings")
  }
  if (current.bom && !styled.startsWith(BOM)) {
    styled = BOM + styled
    kept.push("BOM")
  }
  return { content: styled, kept }
}

// Two lines this alike, by their common start and end, read as one line edited; shorter lines are too plain to tell.
const ALIKE = 0.75
const ALIKE_MIN_CHARS = 12

/** How alike two lines are, from 0 to 1: their common start and end, indentation aside, against the longer one. */
function alike(a: string, b: string) {
  const x = a.trim()
  const y = b.trim()
  if (Math.min(x.length, y.length) < ALIKE_MIN_CHARS) return 0
  let start = 0
  while (start < x.length && start < y.length && x[start] === y[start]) start++
  let end = 0
  while (end < x.length - start && end < y.length - start && x[x.length - 1 - end] === y[y.length - 1 - end]) end++
  return (start + end) / Math.max(x.length, y.length)
}

/**
 * Warnings for replaced ranges that look one line off: the kept line next to the new rows nearly matches the new edge row, more than the line
 * that row replaced did, as when PUT 66.=70: rewrites a header that sits on line 65. The numbers are those of the edited file.
 */
function offByOne(source: Text, edits: LineEdit[], applied: ReturnType<typeof applyChanges>) {
  const kept = (index: number) => {
    const row = applied.rows[index]
    return row?.origin === undefined ? undefined : lineContent(row.raw)
  }
  return edits.flatMap((edit, index) => {
    if (edit.operation !== "replace" || !edit.content.length || edit.quoted) return []
    const { at, length } = applied.runs[index]!
    const above = kept(at - 1)
    const below = kept(at + length)
    const first = edit.content[0]!
    const last = edit.content.at(-1)!
    const warnings: string[] = []
    if (above !== undefined && alike(above, first) >= ALIKE && alike(above, first) > alike(lineContent(source.lines[edit.start - 1]!), first)) {
      warnings.push(`Lines ${at} and ${at + 1} now nearly match: line ${at} was kept just above an edit and line ${at + 1} is the edit's first new line. If the edit was meant to replace line ${at}, its range started one line late; fix that now.`)
    }
    if (below !== undefined && alike(below, last) >= ALIKE && alike(below, last) > alike(lineContent(source.lines[edit.end - 1]!), last)) {
      warnings.push(`Lines ${at + length} and ${at + length + 1} now nearly match: line ${at + length} is an edit's last new line and line ${at + length + 1} was kept just below it. If the edit was meant to replace line ${at + length + 1}, its range ended one line early; fix that now.`)
    }
    return warnings
  })
}

/**
 * The rows of an edit response: the kept row on each side of every change, where a leftover line or a misplaced range shows; the new rows whole,
 * except a sparse `N~` map inside a long run; and one note per stretch of moved lines the agent knew, with their old numbers. Full rows past
 * `limit` are cut; a long replaced line shows the stretch where it changed instead.
 */
function preview(rows: Row[], runs: { at: number; length: number }[], prior: Version | undefined, limit: number) {
  const full = new Set<number>()
  const mapped = new Set<number>()
  let contents: string[] | undefined
  const lines = () => contents ??= rows.map((row) => lineContent(row.raw))
  const kept = (index: number) => index >= 0 && index < rows.length && rows[index]!.origin !== undefined
  const strided: number[] = []
  for (const { at, length } of runs) {
    for (let index = at - EDIT_CONTEXT; index < at; index++) if (kept(index)) full.add(index)
    for (let index = at + length; index < at + length + EDIT_CONTEXT; index++) if (kept(index)) full.add(index)
    if (length <= WRITTEN_ROWS) {
      for (let index = at; index < at + length; index++) full.add(index)
      continue
    }
    full.add(at)
    full.add(at + length - 1)
    strided.push(...mapLines(lines(), at + 2, at + length - 1, { after: at + 1 }).map((line) => line - 1).filter((index) => !rows[index]!.opaque))
  }
  for (const index of strided.slice(0, EDIT_MAP_ROWS)) mapped.add(index)
  const knew = (index: number) => {
    const origin = rows[index]!.origin
    return origin !== undefined && !!prior?.seen && knownLine(prior, origin + 1)
  }
  // Kept rows whose number changed, in stretches that moved by one offset, get one note each when they hold a line the agent knew.
  const notes = new Map<number, string>()
  let stretch: { from: number; to: number; offset: number; known: boolean } | undefined
  const close = () => {
    if (!stretch) return
    let { from, to } = stretch
    while (from <= to && (full.has(from) || mapped.has(from))) from++
    while (to >= from && (full.has(to) || mapped.has(to))) to--
    const { offset, known } = stretch
    stretch = undefined
    if (!known || from > to) return
    const span = (low: number, high: number) => low === high ? `${low}` : `${low}-${high}`
    const many = from < to
    notes.set(from, `[line${many ? "s" : ""} ${span(from + 1, to + 1)} ${many ? "are" : "is"} old line${many ? "s" : ""} ${span(from + 1 - offset, to + 1 - offset)}, moved ${offset > 0 ? "down" : "up"} by ${Math.abs(offset)}]`)
  }
  rows.forEach((row, index) => {
    const offset = row.origin === undefined ? 0 : index - row.origin
    if (!offset) return close()
    if (stretch && stretch.to === index - 1 && stretch.offset === offset) {
      stretch.to = index
      stretch.known ||= knew(index)
      return
    }
    close()
    stretch = { from: index, to: index, offset, known: knew(index) }
  })
  close()
  const output: string[] = []
  const shown: number[] = []
  const marks: Marks[] = []
  let previous = -1
  let previousFull = false
  for (const index of [...new Set([...full, ...mapped, ...notes.keys()])].sort((a, b) => a - b)) {
    const whole = full.has(index)
    const note = notes.get(index)
    // Map rows and notes carry their own numbers, so only a gap between two full rows gets an ellipsis.
    if (note) output.push(note)
    else if (whole && previousFull && index > previous + 1) output.push("...")
    const row = rows[index]!
    if (whole) {
      const content = lineContent(row.raw)
      const windows = row.was !== undefined && visible(content).length > limit ? matchWindows(content, [changedSpan(row.was, content)], limit) : undefined
      if (windows) {
        output.push(`[${index + 1} ${windows.label}]|${windows.text}`)
        marks.push({ cut: true })
      } else {
        const line = lineRow(index + 1, content, limit)
        output.push(line.text)
        if (line.anchor) shown.push(index + 1)
        else marks.push(line)
      }
    } else if (mapped.has(index)) {
      output.push(`${index + 1}~${clip(lines()[index]!)}`)
      shown.push(index + 1)
    }
    previous = index
    previousFull = whole
  }
  return { output, shown, marks }
}

/**
 * Sorts edits into file order and rejects reversed or overlapping ones: a line can be replaced by one hunk only, and an insert may touch a
 * replaced range only at its edges. In doubled coordinates line L is 2L and the gap after it 2L + 1, so an insert sits between the lines it separates.
 */
function orderEdits(edits: LineEdit[]) {
  for (const edit of edits) if (edit.end < edit.start) throw new Error(`Range ${edit.start}.=${edit.end} is reversed`)
  const from = (edit: LineEdit) => edit.edge ? Number.POSITIVE_INFINITY : edit.operation === "before" ? 2 * edit.start - 1 : edit.operation === "after" ? 2 * edit.end + 1 : 2 * edit.start
  const to = (edit: LineEdit) => edit.operation === "replace" ? 2 * edit.end : from(edit)
  // At a shared gap, PUT >N: content lands before PUT <N+1: content.
  const rank = (edit: LineEdit) => edit.operation === "after" ? 0 : 1
  const sorted = [...edits].sort((a, b) => from(a) < from(b) ? -1 : from(a) > from(b) ? 1 : rank(a) - rank(b))
  let reach = 0
  let last: LineEdit | undefined
  for (const edit of sorted) {
    const start = from(edit)
    if (start < reach || (start === reach && !(last?.operation === "after" && edit.operation === "before"))) {
      const where = edit.edge ? "the end of the file" : `line ${edit.start}`
      throw new Error(start === reach && last?.operation !== "replace"
        ? `Edits overlap at ${where}: two hunks insert at the same place; put their + rows into one hunk`
        : `Edits overlap at ${where}: a line can be replaced by one hunk only, and an insert may only touch a replaced range at its edges`)
    }
    reach = to(edit)
    last = edit
  }
  if (edits.reduce((size, edit) => size + edit.content.reduce((sum, line) => sum + Buffer.byteLength(line) + 1, 0), 0) > TEXT_LIMIT) {
    throw new Error(`Combined edit content exceeds ${TEXT_LIMIT} byte size limit`)
  }
  return sorted
}

/** `move` and `copy` write the result to another path; a move then deletes the source, a copy leaves it as it was. */
type EditRequest = { kind: "edit"; filePath: string; tag?: string; edits: LineEdit[]; remove?: boolean; move?: string; copy?: string }
type CreateRequest = { kind: "create"; filePath: string; content: string; overwrite?: string; destination?: boolean }
type Request = EditRequest | CreateRequest
type Handle<R extends Request = Request> = { request: R; target: Target; parent: Parent; file?: FileHandle; destination?: Handle<CreateRequest> }

type Staged =
  | { kind: "edit"; handle: Handle<EditRequest>; current: Awaited<ReturnType<typeof snapshot>>; source: Text; live: View; prior?: Version; applied: ReturnType<typeof applyChanges>; result: Text; warnings: string[] }
  | { kind: "noop"; handle: Handle<EditRequest>; source: Text; live: View; prior?: Version; applied: ReturnType<typeof applyChanges> }
  | { kind: "remove"; handle: Handle<EditRequest>; current: Awaited<ReturnType<typeof snapshot>> }
  | { kind: "create"; handle: Handle<CreateRequest>; bytes: Buffer; created: View; replace?: Awaited<ReturnType<typeof snapshot>>; kept?: string[] }

/** Authorization refusals end the call at once instead of being collected with a patch's other problems. */
class Denied extends Error {}

/**
 * Locks are taken in canonical order so concurrent multi-file patches cannot deadlock; handles stay open until `run` returns.
 * With `failed`, a request whose path cannot be resolved is reported there and left out instead of ending the call.
 */
async function locked<T>(ctx: Context, requests: Request[], run: (handles: Handle[]) => Promise<T>, failed?: (request: Request, error: unknown) => void): Promise<T> {
  const resolved: Handle[] = []
  // A moved file and its destination are claimed together, so a refused pair leaves neither behind.
  const claim = (...handles: Handle[]) => {
    handles.forEach((handle, index) => {
      const duplicate = [...resolved, ...handles.slice(0, index)].find((item) => item.target.canonical === handle.target.canonical)
      if (duplicate) throw new Error(`${duplicate.request.filePath} and ${handle.request.filePath} resolve to the same file ${handle.target.canonical}; merge them into one section`)
    })
    resolved.push(...handles)
  }
  const authorize = (canonical: string, action: string) => authorizeWrite(ctx, canonical, action).catch((error: unknown) => {
    throw new Denied(error instanceof Error ? error.message : String(error), { cause: error })
  })
  // Creates the parents of a path the checks above passed, then authorizes the canonical path that symlinks may redirect to.
  const prepare = async (filePath: string, action: string, create: boolean) => {
    const requested = path.resolve(ctx.directory, expandHome(filePath))
    if (create) await mkdir(path.dirname(requested), { recursive: true })
    const target = await resolveTarget(filePath, ctx, create)
    if (target.canonical !== requested) await authorize(target.canonical, action)
    return target
  }
  const actionOf = (request: Request) => request.kind === "create" ? (request.overwrite ? "overwrite" : "create") : request.remove ? "delete" : request.move ? "move" : request.copy ? "copy" : "edit"
  const pathsOf = (request: Request): [string, string][] => {
    const to = request.kind === "edit" ? request.move ?? request.copy : undefined
    return [[request.filePath, actionOf(request)], ...to ? [[to, "create"] as [string, string]] : []]
  }
  // Every path of the call is judged before a parent directory is created for any of them: by the sandbox first, then by the roots check.
  for (const request of requests) {
    for (const [filePath] of pathsOf(request)) {
      const refused = await outsideSandbox(ctx, filePath)
      if (refused) throw new Outside(refused)
    }
  }
  for (const request of requests) {
    for (const [filePath, action] of pathsOf(request)) {
      await authorize(path.resolve(ctx.directory, expandHome(filePath)), action).catch((error: unknown) => { throw error instanceof Denied ? error.cause : error })
    }
  }
  for (const request of requests) {
    try {
      const create = request.kind === "create" && !request.overwrite
      const handle = { request, target: await prepare(request.filePath, actionOf(request), create) } as Handle
      const to = request.kind === "edit" ? request.move ?? request.copy : undefined
      if (to) {
        const destination: CreateRequest = { kind: "create", filePath: to, content: "", destination: true }
        handle.destination = { request: destination, target: await prepare(to, "create", true) } as Handle<CreateRequest>
      }
      claim(handle, ...handle.destination ? [handle.destination] : [])
    } catch (error) {
      if (error instanceof Denied) throw error.cause
      // A link that leads outside the sandbox ends the call like a refused path.
      if (error instanceof Outside) throw error
      if (!failed || ctx.signal.aborted) throw error
      failed(request, error)
    }
  }
  const ordered = [...resolved].sort((a, b) => a.target.canonical < b.target.canonical ? -1 : a.target.canonical > b.target.canonical ? 1 : 0)
  const step = async (index: number): Promise<T> => {
    if (index === ordered.length) return run(resolved.filter((handle) => !(handle.request.kind === "create" && handle.request.destination)))
    const handle = ordered[index]!
    return serialized(handle.target.canonical, ctx, () => inParent(handle.target, async (parent) => {
      handle.parent = parent
      if (handle.request.kind === "create" && !handle.request.overwrite) return step(index + 1)
      const file = await open(parent.location, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
      try {
        await checkDescriptor(file, parent.location, handle.target.canonical)
        handle.file = file
        return await step(index + 1)
      } finally { await file.close() }
    }))
  }
  return step(0)
}

const RECOVERY_REMAPPED = "Recovered by remapping stale line numbers to unchanged current lines (file changed since the tagged read). Verify the diff matches your intent."
const RECOVERY_EXTERNAL = "Recovered from a stale tag using the tagged read (file changed externally between read and edit)."
const RECOVERY_CHAIN = "Recovered from a stale tag using an earlier read (a prior edit in this session advanced the tag)."
const TAIL_DRIFT = "Applied the end-of-file insert despite a stale tag (file changed since the tagged read). Read again if that change was unexpected."
const unknownTag = (tag: string) => `Tag #${tag} was never issued for this file; never invent tags. The edit was applied only because every hunk is safe without one: SUB and matching - rows quote what they replace, and PUT >$: only appends. Any other hunk would have been refused; use the tag in the header above.`

/** True when `tag` names the live content; a retained version with the same 4-hex tag but other text means a collision. */
function tagMatches(versions: Version[], live: View, tag: string) {
  const tagged = versions.filter((version) => version.tag === tag)
  return live.tag === tag && (!tagged.length || tagged.some((version) => version.key === live.key))
}

/** True when the agent has the whole content of `line` in `version`: it saw the line, or wrote it. */
function knownLine(version: Version, line: number) {
  return !!version.seen?.has(line) || !!version.known?.has(line)
}

async function stageEdit(ctx: Context, store: SnapshotStore, handle: Handle<EditRequest>): Promise<Staged> {
  const { request, target } = handle
  if (handle.destination) await destinationFree(handle.destination)
  const current = await snapshot(handle.file!, TEXT_LIMIT)
  if (current.info.nlink > 1n) throw new Error("Hardlinked targets are not supported; refusing to break links")
  const source = text(current.bytes)
  const live = view(source)
  const versions = store.versions(ctx.session, target.canonical)
  let prior = versions.find((version) => version.key === live.key)
  const finish = (edits: LineEdit[], warnings: string[]): Staged => {
    const applied = applyChanges(source, edits.map((edit) => ({ ...edit, raw: edit.content.map((line) => line + source.ending) })))
    if (applied.bytes.equals(current.bytes) && !request.move && !request.copy) return { kind: "noop", handle, source, live, prior, applied }
    return { kind: "edit", handle, current, source, live, prior, applied, result: text(applied.bytes), warnings: [...warnings, ...offByOne(source, edits, applied)] }
  }
  // A tagless section only appends, which depends on nothing but where the file ends.
  if (request.tag === undefined) return finish(resolveEdges(request.edits, live.lines.length), [])
  const tagged = versions.filter((version) => version.tag === request.tag)
  const matches = tagMatches(versions, live, request.tag)
  // A tag never issued for the file tells nothing about what the agent saw, but hunks that check themselves against the live file are safe without one.
  const unknown = !matches && !tagged.length
  if (unknown) {
    // An append checks nothing, so under a tag issued for another file, a likely mix-up of files, it is refused.
    const origins = store.origins(ctx.session, request.tag, target.canonical)
    const safe = (edit: LineEdit) => edit.sub !== undefined || !!edit.old?.length || (!!edit.edge && !origins.length)
    if (request.remove || request.move || request.copy || !request.edits.every(safe)) {
      throw mismatchError(store, ctx.session, target, live, request.tag, false, request.edits.flatMap(anchorLines), request.edits.every((edit) => edit.edge))
    }
    // Without a version on record the response would count every unchanged line as seen; an empty record keeps the unread ones unseen.
    prior ??= store.record(ctx.session, target.canonical, live, [])
  }
  if (request.remove) {
    if (!matches) throw mismatchError(store, ctx.session, target, live, request.tag, true, [])
    return { kind: "remove", handle, current }
  }
  // Line numbers, checks and SUB texts all refer to the tagged version; only the live one may record what a refusal displays.
  const basis: Version = matches || unknown ? prior ?? live : tagged[0]!
  const named = `[${request.filePath}#${request.tag}]`
  const record = matches || unknown ? (shown: number[]) => { store.record(ctx.session, target.canonical, live, shown) } : undefined
  // A hunk past the end is a problem of its own, and the checks run on the other hunks, so one refusal names all of a section's problems.
  const length = basis.lines.length
  const past = (edit: LineEdit) => Math.max(0, ...anchorLines(edit), ...edit.checks ? [edit.start, edit.end] : []) > length
  const beyond = request.edits.filter(past)
  const ending = beyond.length ? new Refusal(`${named}: Line${beyond.length === 1 ? "" : "s"} ${beyond.map((edit) => Math.max(...anchorLines(edit), edit.start)).join(", ")} ${beyond.length === 1 ? "does" : "do"} not exist in ${target.display} (file has ${length} lines)`) : undefined
  const checks = confirmChecks(named, target.display, basis.lines, request.edits.filter((edit) => !past(edit)), record)
  const subs = resolveSubs(named, target.display, basis.lines, checks.edits, record)
  const quotes = confirmOld(named, target.display, basis.lines, subs.edits.map((edit) => edit.quoted && !knownLine(basis, edit.start) ? { ...edit, opaque: true as const } : edit), record)
  const problems = [ending, checks.refusal, subs.refusal, quotes.refusal].filter((refusal) => refusal !== undefined)
  const lessons = [...writtenLessons(request.edits), ...oldRowsLesson(request.edits)]
  let requested = resolveBlocks(quotes.edits, basis.lines)
  try { requested = orderEdits(requested) } catch (error) {
    throw together([...problems, new Refusal(`${named}: ${error instanceof Error ? error.message : String(error)}`)])
  }
  const anchors = requested.flatMap(anchorLines)
  if (matches || unknown) {
    const edits = resolveEdges(requested, live.lines.length)
    const end = Math.max(0, ...anchors)
    if (end > live.lines.length) throw new Error(`Line ${end} does not exist in ${target.display} (file has ${live.lines.length} lines)`)
    const unseen = prior ? unverified(prior, requested) : []
    if (unseen.length) {
      const rescued = rescueShifted(versions, live, edits, new Set(unseen))
      const remaining = unseen.filter((line) => !rescued.has(line))
      if (remaining.length) problems.push(unseenError(store, ctx.session, target, live, remaining, prior?.known))
    }
    if (problems.length) throw together(problems)
    return finish(edits, unknown ? [unknownTag(request.tag), ...lessons] : lessons)
  }
  if (problems.length) throw together(problems)
  // Like oh-my-pi, edits without line anchors only depend on where the file ends, so any tag drift is tolerable.
  const remapped = !anchors.length && !request.move && !request.copy
    ? { offset: 0, edits: resolveEdges(requested, live.lines.length) }
    : anchors.length && !unverified(basis, requested).length ? remapEdits(basis, live.lines, requested) : undefined
  if (!remapped) throw mismatchError(store, ctx.session, target, live, request.tag, true, anchors)
  return finish(remapped.edits, [!anchors.length ? TAIL_DRIFT : remapped.offset !== 0 ? RECOVERY_REMAPPED : basis === versions[0] ? RECOVERY_EXTERNAL : RECOVERY_CHAIN, ...lessons])
}

// Publication still links exclusively; this early check only keeps a multi-file patch from failing halfway.
async function stageCreate(ctx: Context, store: SnapshotStore, handle: Handle<CreateRequest>): Promise<Staged> {
  let bytes = Buffer.from(handle.request.content)
  if (handle.request.overwrite) {
    const current = await snapshot(handle.file!, TEXT_LIMIT)
    if (current.info.nlink > 1n) throw new Error("Hardlinked targets are not supported; refusing to break links")
    const old = text(current.bytes)
    const live = view(old)
    const versions = store.versions(ctx.session, handle.target.canonical)
    if (!tagMatches(versions, live, handle.request.overwrite)) {
      throw new Error(`${handle.target.display} changed since tag #${handle.request.overwrite}; the current file is [${handle.target.display}#${live.tag}]. Read it again before overwriting.`)
    }
    const styled = inStyleOf(old, handle.request.content)
    bytes = Buffer.from(styled.content)
    return { kind: "create", handle, bytes, created: view(text(bytes)), replace: current, ...styled.kept.length && { kept: styled.kept } }
  }
  const created = view(text(bytes))
  const existing = await lstat(handle.parent.location).catch((error: unknown) => {
    if (missing(error)) return undefined
    throw error
  })
  if (existing) throw new Error(`${handle.target.display} already exists; edit it with a [PATH#TAG] section, or write it with its tag to overwrite`)
  return { kind: "create", handle, bytes, created }
}

async function destinationFree(handle: Handle<CreateRequest>) {
  const existing = await lstat(handle.parent.location).catch((error: unknown) => {
    if (missing(error)) return undefined
    throw error
  })
  if (existing) throw new Error(`Destination ${handle.target.display} already exists; REM it in its own section first`)
}

/** Fails unless the open source descriptor still holds the bytes that were staged. */
async function unchanged(handle: Handle, current: Awaited<ReturnType<typeof snapshot>>) {
  await checkDescriptor(handle.file!, handle.parent.location, handle.target.canonical)
  const latest = await snapshot(handle.file!, TEXT_LIMIT)
  if (latest.info.nlink > 1n) throw new Error("Hardlinked targets are not supported; refusing to break links")
  if (latest.info.ctimeNs !== current.info.ctimeNs || !latest.bytes.equals(current.bytes)) {
    throw new Error("File changed before publication; read again")
  }
}

/** What a grep replace token's preview showed: by canonical path, the changed lines it showed whole, and how many changing lines it left out. */
type Preview = { shown: Map<string, Set<number>>; left: number }

/** Publishes every staged change in order; a failure after the first write names what already landed. `previewed` names the lines a grep replace showed whole. */
async function commit(ctx: Context, store: SnapshotStore, staged: Staged[], previewed?: Map<string, Set<number>>) {
  const changes = staged.filter((item) => item.kind !== "noop")
  for (const item of changes) {
    await item.handle.parent.check()
    if (item.handle.file) await checkDescriptor(item.handle.file, item.handle.parent.location, item.handle.target.canonical)
    if (item.kind === "edit" && item.handle.destination) await destinationFree(item.handle.destination)
  }
  // Every response is rendered before anything is written, so one that cannot be rendered fails the call while nothing has changed yet.
  const responses = staged.map((item) => respond(ctx, store, item, previewed))
  const written: string[] = []
  for (const item of changes) {
    try {
      if (item.kind === "create") {
        await publish(item.handle.parent, item.handle.target.canonical, item.bytes, ctx, {
          replace: item.replace?.info,
          validate: item.replace ? () => unchanged(item.handle, item.replace!) : undefined,
        })
      } else if (item.kind === "remove") {
        await unchanged(item.handle, item.current)
        await item.handle.parent.check()
        await unlink(item.handle.parent.location)
      } else {
        const { handle, current, applied } = item
        const destination = handle.destination
        if (destination) {
          await unchanged(handle, current)
          await publish(destination.parent, destination.target.canonical, applied.bytes, ctx, { mode: Number(current.info.mode & 0o7777n) })
          if (handle.request.move) {
            await unchanged(handle, current)
            await handle.parent.check()
            await unlink(handle.parent.location)
          }
        } else {
          await publish(handle.parent, handle.target.canonical, applied.bytes, ctx, { replace: current.info, validate: () => unchanged(handle, current) })
        }
      }
    } catch (error) {
      if (!written.length) throw error
      throw new Error(`Patch partially applied: ${written.join(", ")} already written; ${item.handle.target.display} failed: ${error instanceof Error ? error.message : String(error)}`, { cause: error })
    }
    written.push(item.handle.target.display)
  }
  return responses.map((response) => {
    response.record()
    return response.text
  })
}

/** The response for one staged change, and the seen and known lines it records once the change is published. */
function respond(ctx: Context, store: SnapshotStore, item: Staged, previewed?: Map<string, Set<number>>): { text: string; record: () => void } {
  const { canonical, display } = item.handle.target
  const nothing = () => {}
  if (item.kind === "noop") {
    if (!item.applied.runs.length) return { text: `[${display}#${item.live.tag}]\nNo changes: the edit reproduces the current content; this tag is still current.`, record: nothing }
    // The lines come back whole, so an edit that keeps landing as it already is shows what the file holds.
    const rows = preview(item.applied.rows, item.applied.runs, item.prior, minifiedReason(canonical, Buffer.byteLength(item.live.key), item.live.lines) ? MINIFIED_COLUMNS : LINE_LIMIT)
    // Rewriting the last line of a file without a final newline is the usual way to try adding one, which an edit never does on its own.
    const unterminated = item.source.body !== "" && !item.source.body.endsWith("\n") && item.applied.runs.some((run) => run.at + run.length === item.applied.rows.length)
    return {
      text: [`[${display}#${item.live.tag}]`, "No changes: these lines already read as the edit writes them; this tag is still current.", ...rows.output, ...rowNotes(display, rows.marks),
        ...unterminated ? [`[${display} has no final newline, which an edit keeps; ${FINAL_NEWLINE}]`] : []].join("\n"),
      // A version without a seen set is unrestricted, and recording rows would restrict it.
      record: () => { if (item.prior?.seen) store.record(ctx.session, canonical, item.live, rows.shown) },
    }
  }
  if (item.kind === "remove") return { text: `Deleted ${display}`, record: nothing }
  if (item.kind === "create") {
    // The model wrote every line but saw no numbers: the row map shows some, a small file is trusted whole, and the rest is known content that a ~ check can anchor.
    const lines = item.created.lines
    const map = writtenMap(display, item.created)
    const trusted = lines.length < TRUSTED_CREATE_LINES
    // Otherwise the first edit of a line the response leaves out would learn of the check from its refusal.
    const hint = trusted || map.seen.length === lines.length ? [] : [map.mapped ? WRITTEN_CHECK : WRITTEN_UNMAPPED]
    return {
      text: [`${item.replace ? "Overwrote" : "Created"} [${display}#${item.created.tag}] (${map.summary}${item.kept ? `; kept its ${item.kept.join(" and ")}` : ""})`, ...map.rows, ...hint].join("\n"),
      record: () => { store.record(ctx.session, canonical, item.created, trusted ? lines.map((_, index) => index + 1) : map.seen, lines.map((_, index) => index + 1)) },
    }
  }
  // Like oh-my-pi, only lines that kept both number and content stay anchorable, plus the rows shown below.
  // Lines the agent wrote, or saw before they moved, stay known content that a ~ check can anchor.
  const destination = item.handle.destination?.target
  const next = view(item.result)
  const prior = item.prior
  const seen = item.applied.rows.flatMap((row, index) => row.origin === index && (!prior?.seen || prior.seen.has(index + 1)) ? [index + 1] : [])
  const known = item.applied.rows.flatMap((row, index) => (row.origin === undefined ? !row.opaque : !!prior?.seen && knownLine(prior, row.origin + 1)) ? [index + 1] : [])
  // A grep replace showed each new line as a >N| row; applied as previewed, one line for one, the rows need not come again.
  const shown = destination ? undefined : previewed?.get(canonical)
  const { runs, rows: written } = item.applied
  if (shown && !item.warnings.length && runs.every((run) => run.length === 1 && written[run.at]!.was !== undefined)) {
    return {
      text: `[${display}#${next.tag}] ${runs.length} line${runs.length === 1 ? "" : "s"} replaced as previewed`,
      record: () => { store.record(ctx.session, canonical, next, [...seen, ...runs.map((run) => run.at + 1).filter((line) => shown.has(line))], known) },
    }
  }
  // Edits to a minified file cut long rows like a read of it does.
  const limit = minifiedReason(destination?.canonical ?? canonical, Buffer.byteLength(next.key), next.lines) ? MINIFIED_COLUMNS : LINE_LIMIT
  const rows = preview(item.applied.rows, item.applied.runs, item.prior, limit)
  const output = [`${destination ? `${item.handle.request.copy ? "Copied" : "Moved"} ${display} to ` : ""}[${destination?.display ?? display}#${next.tag}]`]
  if (item.applied.runs.length) output.push(...item.applied.rows.length ? rows.output : ["(file is now empty)"])
  if (item.applied.terminated) output.push(`[${destination?.display ?? display} now ends with a newline]`)
  output.push(...rowNotes(destination?.display ?? display, rows.marks))
  if (item.warnings.length) output.push("", "Warnings:", ...item.warnings)
  return { text: output.join("\n"), record: () => { store.record(ctx.session, destination?.canonical ?? canonical, next, [...seen, ...rows.shown], known) } }
}

type Range = { start: number; end?: number }
type Selection = { ranges?: Range[]; tail?: number }

const SELECTOR = /^(.*?):((?:[1-9]\d*(?:-(?:[1-9]\d*)?|\+[1-9]\d*)?|-[1-9]\d*)(?:,[1-9]\d*(?:-(?:[1-9]\d*)?|\+[1-9]\d*)?)*)$/

/** Splits `path:selector`; an existing literal path wins over selector parsing. */
async function splitSelector(entry: string, ctx: Context): Promise<{ file: string; selection: Selection }> {
  const match = SELECTOR.exec(entry)
  if (!match) return { file: entry, selection: {} }
  const literal = await lstat(path.resolve(ctx.directory, expandHome(entry))).then(() => true, () => false)
  if (literal) return { file: entry, selection: {} }
  const spec = match[2]!
  if (spec.startsWith("-")) return { file: match[1]!, selection: { tail: Number(spec.slice(1)) } }
  const parsed = spec.split(",").map((part): Range => {
    const [, start, operator, value] = /^(\d+)(?:([-+])(\d*))?$/.exec(part)!
    if (operator === "+") return { start: Number(start), end: Number(start) + Number(value) - 1 }
    if (operator === "-") return { start: Number(start), end: value ? Number(value) : undefined }
    // A bare number is that one line: models write path:N the way grep prints a hit, so reading onward needs N-.
    return { start: Number(start), end: Number(start) }
  })
  for (const range of parsed) if (range.end !== undefined && range.end < range.start) throw new Error(`Range ${range.start}-${range.end} in ${entry} is reversed`)
  return { file: match[1]!, selection: { ranges: parsed } }
}

const SINCE = /^(.+)#([0-9A-Fa-f]{4})$/

/** Splits `path#TAG`, a read of what changed since that tag; an existing literal path wins. */
async function splitSince(entry: string, ctx: Context): Promise<{ file: string; since?: string }> {
  const match = SINCE.exec(entry)
  if (!match) return { file: entry }
  const literal = await lstat(path.resolve(ctx.directory, expandHome(entry))).then(() => true, () => false)
  return literal ? { file: entry } : { file: match[1]!, since: match[2]!.toUpperCase() }
}

/**
 * Streams the first `size` bytes of a file line by line instead of holding it whole, so a huge log, or one still being written, reads fine.
 * Keeps up to READ_LINES of the lines `wanted` accepts, or with `ring` only the last `ring` lines, each up to `cap` bytes; `total` counts every line.
 */
async function scanLines(file: FileHandle, size: number, signal: AbortSignal, wanted: (line: number) => boolean, cap: number, ring?: number) {
  const kept = new Map<number, { bytes: Buffer; cut: boolean }>()
  const chunk = Buffer.alloc(Math.max(1, Math.min(size, 1 << 20)))
  let line = 1
  let parts: Buffer[] = []
  let held = 0
  let length = 0
  let keep = wanted(line)
  const take = (piece: Buffer) => {
    length += piece.length
    if (!keep || held >= cap) return
    const part = Buffer.from(piece.subarray(0, cap - held))
    parts.push(part)
    held += part.length
  }
  const close = () => {
    if (keep) {
      kept.set(line, { bytes: Buffer.concat(parts), cut: length > held })
      if (ring) kept.delete(line - ring)
    }
    line++
    parts = []
    held = 0
    length = 0
    keep = ring ? true : kept.size < READ_LINES && wanted(line)
  }
  for (let position = 0; position < size;) {
    signal.throwIfAborted()
    const { bytesRead } = await file.read(chunk, 0, Math.min(chunk.length, size - position), position)
    if (!bytesRead) break
    position += bytesRead
    const data = chunk.subarray(0, bytesRead)
    let start = 0
    for (let at = data.indexOf(10); at >= 0; at = data.indexOf(10, start)) {
      take(data.subarray(start, at))
      close()
      start = at + 1
    }
    take(data.subarray(start))
  }
  if (length) close()
  return { total: line - 1, kept }
}

type LineFacts = { lines: number; longest: number; longestAt: number; crlf: number; lf: number; final: boolean; bom: boolean; escapes: boolean; binary: boolean }

/** Counts line facts over bytes fed in order; widths are UTF-16 units like a line's `length`, without its CR or a leading BOM. */
function lineFacts() {
  const facts: LineFacts = { lines: 0, longest: 0, longestAt: 0, crlf: 0, lf: 0, final: false, bom: false, escapes: false, binary: false }
  let width = 0
  let previous = -1
  let fed = 0
  const end = () => {
    facts.lines++
    if (width > facts.longest) {
      facts.longest = width
      facts.longestAt = facts.lines
    }
    width = 0
  }
  return {
    feed(data: Buffer) {
      if (!fed && data[0] === 0xef && data[1] === 0xbb && data[2] === 0xbf) {
        facts.bom = true
        width = -1
      }
      fed += data.length
      for (let index = 0; index < data.length; index++) {
        const byte = data[index]!
        if (byte === 10) {
          if (previous === 13) {
            facts.crlf++
            width--
          } else facts.lf++
          end()
        } else {
          // Lead bytes count once, and four-byte sequences twice, as surrogate pairs.
          if ((byte & 0xc0) !== 0x80) width += byte >= 0xf0 ? 2 : 1
          if (byte === 27) facts.escapes = true
          else if (byte < 9 || (byte > 13 && byte < 32) || byte === 127) facts.binary = true
        }
        previous = byte
      }
    },
    done() {
      if (fed && previous !== 10) end()
      facts.final = previous === 10
      return facts
    },
  }
}

/** Line facts of the first `size` bytes, streamed so a file of any size works. */
async function measure(file: FileHandle, size: number, signal: AbortSignal) {
  const counter = lineFacts()
  const chunk = Buffer.alloc(Math.max(1, Math.min(size, 1 << 20)))
  for (let position = 0; position < size;) {
    signal.throwIfAborted()
    const { bytesRead } = await file.read(chunk, 0, Math.min(chunk.length, size - position), position)
    if (!bytesRead) break
    counter.feed(chunk.subarray(0, bytesRead))
    position += bytesRead
  }
  return counter.done()
}

/**
 * Collapses repetition among plain rows, comparing lines with their digits masked so timestamps, counters and durations do not count as changes.
 * A line or a block of up to CADENCE_LINES repeating back to back prints once, then a count and, unless every copy is identical, the last copy.
 * A block of at least ECHO_MIN_LINES that recurs later, such as a stack trace, prints once under an id, and each later copy as one row.
 * `similar` reports whether anything collapsed had other numbers than what is shown.
 */
function compress(lines: { at: number; text: string }[]) {
  const keys = lines.map((line) => line.text.replace(/\d+/g, "0"))
  const same = (a: number, b: number, length: number) => {
    for (let offset = 0; offset < length; offset++) if (keys[a + offset] !== keys[b + offset]) return false
    return true
  }
  const identical = (a: number, b: number, length: number) => {
    for (let offset = 0; offset < length; offset++) if (lines[a + offset]!.text !== lines[b + offset]!.text) return false
    return true
  }
  type Item = { kind: "line"; index: number } | { kind: "note"; text: string; at: number } | { kind: "echo"; of: number; index: number; length: number }
  const items: Item[] = []
  // Lines printed as their own rows, by masked key; only such runs can be the original a later echo points at.
  const printed = lines.map(() => false)
  const starts = new Map<string, number[]>()
  const print = (index: number) => {
    items.push({ kind: "line", index })
    printed[index] = true
    const list = starts.get(keys[index]!)
    if (list) list.push(index)
    else starts.set(keys[index]!, [index])
  }
  let similar = false
  // The back-to-back repeat starting at a line, when collapsing it prints fewer rows; memoized because block matching asks about many lines.
  const cadences = new Map<number, { period: number; copies: number; exact: boolean } | undefined>()
  const cadence = (index: number) => {
    if (cadences.has(index)) return cadences.get(index)
    let best: { period: number; copies: number; exact: boolean } | undefined
    for (let period = 1; period <= CADENCE_LINES && index + period * 3 <= lines.length; period++) {
      if (keys.slice(index, index + period).every((key) => !key.trim())) continue
      let copies = 1
      while (index + (copies + 1) * period <= lines.length && same(index, index + copies * period, period)) copies++
      let exact = true
      for (let copy = 1; copy < copies && exact; copy++) exact = identical(index, index + copy * period, period)
      const rows = exact ? period + 1 : period * 2 + 1
      if (copies >= 3 && rows < copies * period && (!best || copies * period > best.copies * best.period)) best = { period, copies, exact }
    }
    cadences.set(index, best)
    return best
  }
  for (let index = 0; index < lines.length;) {
    const best = cadence(index)
    if (best) {
      const { period, copies, exact } = best
      for (let offset = 0; offset < period; offset++) print(index + offset)
      const what = period === 1 ? "the line above" : `the ${period} lines above`
      items.push({ kind: "note", at: lines[index + period]!.at, text: exact
        ? `[${what} repeat${period === 1 ? "s" : ""} ${copies - 1} more time${copies > 2 ? "s" : ""}]`
        : `[${copies - 1} more like ${what}; the last ${period === 1 ? "one" : "ones"}:]` })
      if (!exact) {
        similar = true
        for (let offset = 0; offset < period; offset++) print(index + (copies - 1) * period + offset)
      }
      index += copies * period
      continue
    }
    // A recurring block ends where a back-to-back repeat begins on either side, so each repeat still collapses on its own.
    let echo: { of: number; length: number } | undefined
    for (const earlier of (starts.get(keys[index]!) ?? []).slice(-ECHO_CANDIDATES)) {
      let length = 0
      while (length < ECHO_MAX_LINES && earlier + length < index && index + length < lines.length && printed[earlier + length] &&
        keys[earlier + length] === keys[index + length] && (length === 0 || (!cadence(index + length) && !cadence(earlier + length)))) length++
      if (length >= ECHO_MIN_LINES && (!echo || length > echo.length)) echo = { of: earlier, length }
    }
    if (echo) {
      items.push({ kind: "echo", of: echo.of, index, length: echo.length })
      if (!identical(echo.of, index, echo.length)) similar = true
      index += echo.length
      continue
    }
    print(index++)
  }
  // Ids follow the order the originals appear in; an original shows the most lines any of its echoes covered.
  const originals = new Map<number, { id: number; length: number }>()
  for (const of of [...new Set(items.flatMap((item) => item.kind === "echo" ? [item.of] : []))].sort((a, b) => a - b)) originals.set(of, { id: originals.size + 1, length: 0 })
  for (const item of items) if (item.kind === "echo") originals.get(item.of)!.length = Math.max(originals.get(item.of)!.length, item.length)
  const rows = items.map((item) => {
    if (item.kind === "note") return { text: item.text, at: item.at }
    const line = lines[item.index]!
    if (item.kind === "line") {
      const original = originals.get(item.index)
      return { text: original ? `[#${original.id}, ${original.length} lines] ${line.text}` : line.text, at: line.at }
    }
    const original = originals.get(item.of)!
    return { text: `[#${original.id} again, ${item.length < original.length ? `its first ${item.length}` : item.length} lines] ${line.text}`, at: line.at }
  })
  return { rows, similar }
}

/** Whether a line falls inside any of a file's line selections; unlike a read, an open range runs to the end of the file. */
async function selectedLines(ctx: Context, file: string, selections: Selection[]) {
  let length = 0
  if (selections.some((selection) => selection.tail)) {
    const handle = await open(file, constants.O_RDONLY | constants.O_NONBLOCK)
    try {
      await confined(ctx, handle, file)
      length = (await scanLines(handle, Number((await handle.stat()).size), ctx.signal, () => false, 0)).total
    } finally { await handle.close() }
  }
  return (line: number) => selections.some((selection) => selection.tail
    ? line > length - selection.tail
    : selection.ranges!.some((range) => line >= range.start && line <= (range.end ?? Number.POSITIVE_INFINITY)))
}

/** Resolves a selection against a file length into sorted, merged, inclusive ranges. */
function selectLines(selection: Selection, length: number): { start: number; end: number }[] {
  if (selection.tail) return [{ start: Math.max(1, length - selection.tail + 1), end: length }]
  const requested = selection.ranges ?? [{ start: 1 }]
  const sorted = requested.map((range) => ({ start: range.start, end: Math.min(length, range.end ?? range.start + READ_LINES - 1) }))
    .sort((a, b) => a.start - b.start)
  if (sorted[0]!.start > length) throw new Error(`Line ${sorted[0]!.start} is beyond the end of the file (${length} lines)`)
  const merged: { start: number; end: number }[] = []
  for (const range of sorted) {
    if (range.start > length) continue
    const previous = merged.at(-1)
    if (previous && range.start <= previous.end + 1) previous.end = Math.max(previous.end, range.end)
    else merged.push({ ...range })
  }
  return merged
}

/** One displayable read entry; `rows` compete for the shared budget, while `head` and the footer always print. */
type Block = {
  head: string
  rows: ({ text: string; line?: number } & Marks)[]
  /** Footer lines for a block cut after `shown` rows; also records what was displayed. */
  finish(shown: number): string[]
}

const OUTLINE_LINES = 500
const OUTLINE_BYTES = 20 * 1024
const OUTLINE_ROWS = 150
const OUTLINE_MIN_ROWS = 5
const HEAD_LINES = 100
const SKIP_BYTES = 4 * 1024
const MINIFIED_LINE_CHARS = 300
const MINIFIED_COLUMNS = 300
// Bundler output: .bundle/.chunk names, or a content hash with at least one hex letter, so dates and version numbers do not count.
const BUNDLE_NAME = /\.(bundle|chunk)\.(js|mjs|cjs|css)$|[.-](?=[0-9a-f]*[a-f])[0-9a-f]{8,}\.(js|mjs|cjs|css)$/i
const LOCKFILES = new Set(["package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "Cargo.lock", "poetry.lock", "uv.lock", "Pipfile.lock", "composer.lock", "Gemfile.lock", "go.sum", "flake.lock", "gradle.lockfile", "packages.lock.json"])
const GENERATED_DIRECTORIES = new Set(["node_modules", "dist"])

function approximateTokens(bytes: number) {
  const tokens = Math.round(bytes / 4)
  return tokens >= 1000 ? `about ${Math.round(tokens / 1000)}k tokens` : `about ${tokens} token${tokens === 1 ? "" : "s"}`
}

/** An agent's number held to `limit` instead of refused, with a note so the agent learns the limit; undefined stays undefined. */
function capped(name: string, value: number | undefined, limit: number, notes: string[]) {
  if (value === undefined || value <= limit) return value
  notes.push(`[${name} ${value} is over its limit of ${limit}, so ${limit} was used]`)
  return limit
}

function formatSize(bytes: number) {
  if (bytes < 1024) return `${bytes} byte${bytes === 1 ? "" : "s"}`
  return bytes < 1048576 ? `${(bytes / 1024).toFixed(1)} KiB` : `${(bytes / 1048576).toFixed(1)} MiB`
}

/** How long before now `time` (epoch milliseconds) was, in its largest whole unit up to days. */
function formatAge(time: number) {
  const seconds = Math.max(0, Math.floor((Date.now() - time) / 1000))
  if (seconds < 60) return `${seconds}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  return seconds < 172800 ? `${Math.floor(seconds / 3600)}h ago` : `${Math.floor(seconds / 86400)}d ago`
}

const UNITS: Record<string, number> = { "": 1, k: 1024, m: 1024 ** 2, g: 1024 ** 3 }
const PERIODS: Record<string, number> = { s: 1000, m: 60_000, h: 3_600_000, d: 86_400_000, w: 604_800_000 }

/** Bytes in a size like `512`, `20k`, `1.5M` or `2GiB`; units count in 1024s. */
function parseSize(input: string) {
  const match = /^(\d+(?:\.\d+)?)\s*([kmg]?)(?:i?b)?$/i.exec(input.trim())
  if (!match) throw new Error(`Size ${JSON.stringify(input)} is not like 512, 20k, 1.5M or 2G`)
  return Math.round(Number(match[1]) * UNITS[match[2]!.toLowerCase()]!)
}

/** Milliseconds in an age like `30s`, `10m`, `2h`, `3d` or `1w`. */
function parseAge(input: string) {
  const match = /^(\d+(?:\.\d+)?)\s*([smhdw])$/i.exec(input.trim())
  if (!match) throw new Error(`Age ${JSON.stringify(input)} is not like 30s, 10m, 2h, 3d or 1w`)
  return Number(match[1]) * PERIODS[match[2]!.toLowerCase()]!
}

const CODE_PUNCTUATION = new Set([..."{}()[];,=<>+*&|!?"].map((char) => char.charCodeAt(0)))

/**
 * Why a file looks minified or bundled, if it does: by its name, or by most of its text sitting in dense lines over MINIFIED_LINE_CHARS.
 * A dense line has more code punctuation than whitespace, or almost no whitespace at all; prose written one paragraph per line has neither.
 */
function minifiedReason(canonical: string, bytes: number, lines: string[]) {
  const name = path.basename(canonical)
  if (/\.min\.(js|mjs|css)$|\.map$/.test(name)) return "minified or source map"
  if (BUNDLE_NAME.test(name)) return "bundled or minified"
  let dense = 0
  for (const line of lines) {
    if (line.length <= MINIFIED_LINE_CHARS) continue
    let spaces = 0
    let punctuation = 0
    for (let index = 0; index < line.length; index++) {
      const code = line.charCodeAt(index)
      if (code === 32 || code === 9) spaces++
      else if (CODE_PUNCTUATION.has(code)) punctuation++
    }
    if (spaces * 20 < line.length || punctuation > spaces) dense += line.length
  }
  if (dense * 2 > bytes) return "minified (most of it in long, dense lines)"
}

/** What kind of generated file this is, if it is one: a lockfile, minified or bundled code, or something under node_modules/ or dist/. */
function generatedKind(canonical: string, bytes: number, lines: string[]) {
  if (LOCKFILES.has(path.basename(canonical))) return "lockfile"
  const minified = minifiedReason(canonical, bytes, lines)
  if (minified) return minified
  const segment = canonical.split(path.sep).find((part) => GENERATED_DIRECTORIES.has(part))
  if (segment) return `inside ${segment}/`
}

/** Why an unselected read of this file is not worth its tokens, if it is not. */
function junk(canonical: string, bytes: number, lines: string[]) {
  return bytes <= SKIP_BYTES ? undefined : generatedKind(canonical, bytes, lines)
}

const CLOSER = /^[\s)\]}>;,]*$/
const COMMENT = /^\s*(\/\/|\/\*|\*|<!--|--|#(?![[!]|include\b))/
const CONTAINER = /\b(class|interface|impl|trait|struct|enum|object|namespace|module|record|protocol|extension)\b/
const IMPORT = /^(import|from|use|require|#include|using|package|extern crate)\b/

type OutlineRow = { line: number } | { text: string }

/**
 * Marks lines that continue a multi-line string (triple quotes, template literals, Go raw strings) so their text is not taken for code.
 * Quote marks inside one-line strings, comments and regex literals open nothing, or a file quoting them would lose the rest of its outline.
 */
function insideStrings(lines: string[]) {
  let open: string | undefined
  return lines.map((content) => {
    const inside = open !== undefined
    if (!inside && COMMENT.test(content)) return false
    // Escaped characters never open or close anything; blanks keep the offsets.
    const text = content.replace(/\\./g, "  ")
    let at = 0
    while (at < text.length) {
      if (open !== undefined) {
        const end = text.indexOf(open, at)
        if (end < 0) break
        at = end + open.length
        open = undefined
        continue
      }
      const char = text[at]!
      const triple = text.slice(at, at + 3)
      if (triple === '"""' || triple === "'''") {
        open = triple
        at += 3
      } else if (char === "`") {
        open = char
        at++
      } else if (char === '"' || char === "'") {
        const end = text.indexOf(char, at + 1)
        at = end < 0 ? text.length : end + 1
      } else if (text.startsWith("//", at)) break
      else if (text.startsWith("/*", at)) {
        const end = text.indexOf("*/", at + 2)
        at = end < 0 ? text.length : end + 2
      } else if (char === "/" && /(^|[(,=:[!&|?{};]|\breturn)\s*$/.test(text.slice(0, at))) at = regexEnd(text, at + 1)
      else at++
    }
    return inside
  })
}

/** The offset just past a regex literal whose body starts at `at`; a / inside a [class] does not end it. */
function regexEnd(text: string, at: number) {
  let inClass = false
  for (; at < text.length; at++) {
    if (text[at] === "[") inClass = true
    else if (text[at] === "]") inClass = false
    else if (text[at] === "/" && !inClass) return at + 1
  }
  return text.length
}

const DECORATOR = /^\s*(@|#\[)/
const BLOCK_CLOSER = /^([)\]}]|end\b)/
// A line at the opener's depth that still belongs to it: a lone or trailing opener, or the next branch of the statement.
const BLOCK_CONTINUATION = /^([)\]}]|[{([]\s*$|(else|elif|except|finally|catch)\b)/

/**
 * The lines of the block that starts at `line`, by indentation like `outline`: leading comments and decorators, the opener,
 * its deeper body, and a closing line at the opener's depth.
 * @throws Error when `line` does not start a multi-line block
 */
function blockRange(lines: string[], line: number) {
  if (line > lines.length) throw new Error(`Line ${line} does not exist (file has ${lines.length} lines)`)
  const indent = (content: string) => content.length - content.trimStart().length
  const quoted = insideStrings(lines)
  const code = (index: number) => lines[index]!.trim() !== "" && !quoted[index] && !COMMENT.test(lines[index]!)
  const refuse = () => new Error(`Line ${line} does not start a multi-line block; use PUT N.=M: or CUT N.=M instead`)
  let opener = line - 1
  while (opener < lines.length && lines[opener]!.trim() !== "" && !code(opener)) opener++
  while (opener < lines.length && DECORATOR.test(lines[opener]!)) opener++
  if (opener >= lines.length || !code(opener) || indent(lines[opener]!) !== indent(lines[line - 1]!)) throw refuse()
  const depth = indent(lines[opener]!)
  const nextCode = (from: number) => { for (let index = from + 1; index < lines.length; index++) if (code(index)) return index }
  let last = opener
  let body = false
  for (let index = opener + 1; index < lines.length; index++) {
    const content = lines[index]!
    if (!content.trim()) continue
    // A deeper comment belongs to the body only after deeper code, so a following doc comment never counts as one.
    if (quoted[index] || indent(content) > depth && (body || code(index))) {
      body = true
      last = index
      continue
    }
    if (!code(index)) continue
    const trimmed = content.trimStart()
    const next = nextCode(index)
    if (indent(content) === depth && BLOCK_CONTINUATION.test(trimmed) && next !== undefined && indent(lines[next]!) > depth) {
      last = index
      continue
    }
    if (indent(content) === depth && BLOCK_CLOSER.test(trimmed)) last = index
    break
  }
  if (!body) throw refuse()
  return last + 1
}

/**
 * Gives block edits their resolved end: a block replace or cut spans the whole block, a block insert goes after it.
 * nib finds that end itself, so it counts as confirmed; an insert after the block is confirmed only through a check on the block's first line.
 */
function resolveBlocks(edits: LineEdit[], lines: string[]): LineEdit[] {
  return edits.map((edit) => {
    if (!edit.block) return edit
    const end = blockRange(lines, edit.start)
    const { block, ...rest } = edit
    return edit.operation === "after"
      ? { ...rest, start: end, end, confirmed: edit.confirmed?.includes(edit.start) ? [end] : [] }
      : { ...rest, end, confirmed: [...edit.confirmed ?? [], end] }
  })
}

const MARKDOWN = /\.(md|mdx|markdown)$/i

/** Markdown heading lines outside fenced code. */
function headings(lines: string[]) {
  const found: number[] = []
  let fenced = false
  lines.forEach((content, index) => {
    if (/^\s*(```|~~~)/.test(content)) fenced = !fenced
    else if (!fenced && /^#{1,6}\s/.test(content)) found.push(index + 1)
  })
  return found
}

/** The code lines an outline can list, by indentation: top-level lines and the members of top-level container types. */
function layout(lines: string[]) {
  const indent = (content: string) => content.length - content.trimStart().length
  const quoted = insideStrings(lines)
  const code = (index: number) => lines[index]!.trim() !== "" && !quoted[index] && !COMMENT.test(lines[index]!)
  const unit = Math.min(...lines.filter((content, index) => code(index) && indent(content) > 0).map(indent), Number.POSITIVE_INFINITY)
  const depth = (content: string) => Number.isFinite(unit) ? Math.floor(indent(content) / unit) : 0
  const candidates: { line: number; depth: number }[] = []
  let container = false
  lines.forEach((content, index) => {
    if (!code(index) || CLOSER.test(content) || /^[)\]}]/.test(content.trimStart())) return
    const level = depth(content)
    if (level === 0) {
      container = CONTAINER.test(content)
      candidates.push({ line: index + 1, depth: 0 })
    } else if (level === 1 && container) candidates.push({ line: index + 1, depth: 1 })
  })
  return { candidates, code, depth }
}

// A declared name comes before the line's parameters, type, value or body; a Go receiver or a Rust visibility group comes before the name.
const DECLARATION_END = /[(=:{<[,;"'`]/
const DECLARATION_GROUP = /^\s*(func|pub)\s*\([^)]*\)/
// Words that make a top-level line a declaration; without one, a plain text or log line would pass for one.
const DECLARATION_WORD = /\b(function|def|fn|func|fun|class|interface|type|struct|enum|trait|impl|union|const|let|var|val|export|pub|module|namespace|object|record|protocol|extension|macro|sub|proc|local|static|public|private|protected|internal|abstract|final|async|override|readonly|declare|default|extern|inline|virtual|sealed|typealias|defmodule|defp|defn|defmacro)\b/

/**
 * The lines among `matched` whose first match sits in their declaring part, before the parameters, type, value or body, on a line an outline would list;
 * a top-level line also needs a declaring word in that part, while container members and Markdown headings need none.
 */
function definitionLines(display: string, lines: string[], matched: number[], spans: Map<number, Span[]> | undefined) {
  // Each listed line, and whether it needs a declaring word.
  const listed = new Map<number, boolean>(MARKDOWN.test(display)
    ? headings(lines).map((line) => [line, false])
    : layout(lines).candidates.flatMap(({ line, depth }) => depth === 0 && IMPORT.test(lines[line - 1]!) ? [] : [[line, depth === 0] as const]))
  return matched.filter((line) => {
    const start = spans?.get(line)?.[0]?.start
    const needsWord = listed.get(line)
    if (start === undefined || needsWord === undefined) return false
    const content = lines[line - 1]!
    const group = DECLARATION_GROUP.exec(content)
    const from = group?.[0].length ?? 0
    const head = content.slice(from)
    const end = head.search(DECLARATION_END)
    const at = start - from
    // The word may be part of the match itself, as when the pattern is ^function.
    return at >= 0 && (end < 0 || at < end) && (!needsWord || DECLARATION_WORD.test(`${group?.[1] ?? ""} ${end < 0 ? head : head.slice(0, end)}`))
  })
}

/** Top-level code lines plus the members of top-level container types, by indentation; runs of imports or one-line declarations show their first line and a count. Markdown uses headings. */
function outline(display: string, lines: string[]): { rows: OutlineRow[]; candidates: number } {
  if (MARKDOWN.test(display)) {
    const rows = headings(lines).map((line) => ({ line }))
    return { rows, candidates: rows.length }
  }
  const { candidates, code, depth } = layout(lines)
  // A declaration opens a block when the next code line is deeper; the rest (constants, aliases, fields) say little one by one.
  const opens = (line: number) => {
    for (let index = line; index < lines.length; index++) if (code(index)) return depth(lines[index]!) > depth(lines[line - 1]!)
    return false
  }
  const kind = (row: { line: number; depth: number }) => row.depth === 0 && IMPORT.test(lines[row.line - 1]!) ? "import" : opens(row.line) ? "block" : "line"
  // Runs of imports, or of three or more one-line declarations, become their first row plus a count.
  const groups = (list: { line: number; depth: number }[]) => {
    const result: { rows: OutlineRow[]; covers: number }[] = []
    for (let index = 0; index < list.length;) {
      const first = list[index]!
      const run = kind(first)
      let last = index
      if (run !== "block") while (last + 1 < list.length && list[last + 1]!.depth === first.depth && kind(list[last + 1]!) === run) last++
      if (run === "line" && last - index < 2) last = index
      const more = last > index ? [{ text: `[lines ${first.line + 1}-${list[last]!.line}: ${last - index} more ${run === "import" ? "import lines" : "one-line declarations"}]` }] : []
      result.push({ rows: [{ line: first.line }, ...more], covers: last - index + 1 })
      index = last + 1
    }
    return result
  }
  const size = (list: { rows: OutlineRow[] }[]) => list.reduce((sum, group) => sum + group.rows.length, 0)
  let chosen = groups(candidates)
  if (size(chosen) > OUTLINE_ROWS) {
    // Over the cap, top-level rows stay and members fill the remaining room in file order.
    let room = OUTLINE_ROWS - size(groups(candidates.filter((row) => row.depth === 0)))
    chosen = groups(candidates.filter((row) => row.depth === 0 || room-- > 0))
  }
  const rows: OutlineRow[] = []
  let covered = 0
  for (const group of chosen) {
    if (rows.length + group.rows.length > OUTLINE_ROWS) break
    rows.push(...group.rows)
    covered += group.covers
  }
  const omitted = candidates.length - covered
  if (omitted > 0) rows.push({ text: `[outline capped at ${OUTLINE_ROWS} rows; ${omitted} more members or declarations not shown]` })
  return { rows, candidates: candidates.length }
}

/** The folder of the repository holding `location`, or undefined outside git. */
async function repositoryOf(location: string) {
  for (let directory = location; ; directory = path.dirname(directory)) {
    if (await lstat(path.join(directory, ".git")).then(() => true, () => false)) return directory
    if (path.dirname(directory) === directory) return undefined
  }
}

/** The git directory a repository shares with all its worktrees. */
async function commonDirectory(repository: string) {
  const marker = path.join(repository, ".git")
  const info = await lstat(marker).catch(() => undefined)
  if (!info?.isFile()) return info ? realpath(marker).catch(() => marker) : undefined
  const own = /^gitdir:\s*(.+)$/m.exec(await readFile(marker, "utf8").catch(() => ""))?.[1]?.trim()
  if (!own) return undefined
  const gitdir = path.resolve(repository, own)
  const shared = await readFile(path.join(gitdir, "commondir"), "utf8").then((text) => path.resolve(gitdir, text.trim()), () => gitdir)
  return realpath(shared).catch(() => shared)
}

/**
 * The repositories nested in `repository` (under `within`, when given) that its ignore rules cover, from git's own list of ignored folders: git never
 * looks inside a nested repository, so it lists an ignored one as a folder, and an ignored folder holding repositories, like a `modules/` one, as itself;
 * the folders right inside such a folder are looked at too, unless it holds more than CONTAINER_ENTRIES entries (a package or build cache).
 * A worktree of `repository` is left out, since it repeats the same files.
 */
async function nestedRepositories(repository: string, within: string | undefined, signal: AbortSignal) {
  // Git stops at `repository` instead of finding an outer one, and reads `within` as a path, never as a pattern.
  const env: Record<string, string> = { GIT_CEILING_DIRECTORIES: path.dirname(repository), GIT_LITERAL_PATHSPECS: "1" }
  for (const [key, value] of Object.entries(process.env)) if (value !== undefined && !key.startsWith("GIT_")) env[key] = value
  let listed: string
  try {
    // A repository's config can name a command for core.fsmonitor, which git would run here; a search must never run code it finds.
    const child = Bun.spawn(["git", "-c", "core.fsmonitor=false", "-C", repository, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory", "--", within ?? "."], { stdout: "pipe", stderr: "ignore", env, signal })
    const [stdout, exit] = await Promise.all([new Response(child.stdout).text(), child.exited])
    if (exit !== 0) return []
    listed = stdout
  } catch (error) {
    // Without git, ripgrep's own walk is all there is.
    if (signal.aborted) throw error
    return []
  }
  const own = await commonDirectory(repository)
  const isRepository = async (folder: string) => await lstat(path.join(folder, ".git")).then(() => true, () => false) && (!own || await commonDirectory(folder) !== own)
  const folders = listed.split("\0").filter((entry) => entry.endsWith("/")).map((entry) => path.resolve(repository, entry))
  const nested = await Promise.all(folders.map(async (folder) => {
    if (await lstat(path.join(folder, ".git")).then(() => true, () => false)) return await isRepository(folder) ? [folder] : []
    const entries = await readdir(folder, { withFileTypes: true }).catch(() => [])
    if (entries.length > CONTAINER_ENTRIES) return []
    const inside = entries.filter((entry) => entry.isDirectory()).map((entry) => path.join(folder, entry.name))
    return (await Promise.all(inside.map(async (child) => await isRepository(child) ? [child] : []))).flat()
  }))
  return nested.flat()
}

/** The folders among `folders` that ripgrep run with `flags` from `root` lists a file right inside of. */
async function listedIn(root: string, folders: string[], flags: string[], signal: AbortSignal) {
  const depth = Math.max(...folders.map((folder) => path.relative(root, folder).split(path.sep).length)) + 1
  const child = Bun.spawn(["rg", "--files", "--null", "--no-config", "--hidden", "--glob", "!.git", `--max-depth=${depth}`, ...flags, "--", root], { stdout: "pipe", stderr: "ignore", signal })
  const [stdout] = await Promise.all([new Response(child.stdout).text(), child.exited])
  const parents = new Set(stdout.split("\0").filter(Boolean).map((file) => path.dirname(file)))
  return new Set(folders.filter((folder) => parents.has(folder)))
}

/** The repositories under `root`, a folder outside git, found by walking at most FOLDERS_OUTSIDE_GIT folders; like ripgrep, the walk follows no links. */
async function repositoriesBelow(root: string, signal: AbortSignal) {
  const repositories: string[] = []
  const queue = [root]
  for (let walked = 0; queue.length; walked++) {
    if (walked === FOLDERS_OUTSIDE_GIT) return { repositories, capped: true }
    signal.throwIfAborted()
    const folder = queue.shift()!
    const entries = await readdir(folder, { withFileTypes: true }).catch(() => [])
    if (folder !== root && entries.some((entry) => entry.name === ".git")) repositories.push(folder)
    else for (const entry of entries) if (entry.isDirectory()) queue.push(path.join(folder, entry.name))
  }
  return { repositories, capped: false }
}

/**
 * The nested repositories under `roots` that ripgrep would leave out. Inside a repository ripgrep follows git, but it judges a nested repository's own
 * folder by the outer repository's ignore rules, which git never applies to it; a workspace that ignores the repositories it holds would hide them all.
 * Searched as roots of their own, they follow their own rules. Only git's rules are overruled: a repository that .ignore, .rgignore or one of the
 * search's excluding `globs` keeps out stays out, and so does everything inside it.
 */
async function hiddenRepositories(roots: string[], globs: string[], signal: AbortSignal) {
  const hidden = new Set<string>()
  const notes: string[] = []
  const visited = new Set<string>()
  const excluding = globs.filter((glob) => glob.startsWith("!")).flatMap((glob) => ["--glob", glob])
  /** Finds what `repository` hides under `root`, and goes on into every nested repository the search will cover. */
  const visit = async (root: string, repository: string): Promise<void> => {
    if (visited.has(root)) return
    visited.add(root)
    const nested = await nestedRepositories(repository, root === repository ? undefined : path.relative(repository, root), signal)
    if (!nested.length) return
    const entered = await listedIn(root, nested, [], signal)
    const left = nested.filter((folder) => !entered.has(folder))
    const allowed = left.length ? await listedIn(root, left, ["--no-ignore-vcs", ...excluding], signal) : new Set<string>()
    for (const folder of allowed) hidden.add(folder)
    await Promise.all(nested.filter((folder) => entered.has(folder) || allowed.has(folder)).map((folder) => visit(folder, folder)))
  }
  await Promise.all(roots.map(async (root) => {
    if (!(await stat(root).catch(() => undefined))?.isDirectory()) return
    const repository = await repositoryOf(root)
    if (repository) return visit(root, repository)
    // Outside git ripgrep enters a repository by itself unless an .ignore or a glob keeps it out, and each one's own rules decide what it holds.
    const below = await repositoriesBelow(root, signal)
    if (below.capped) notes.push(`[looked for git repositories in the first ${FOLDERS_OUTSIDE_GIT} folders of ${root} only; one further down may hide its nested repositories]`)
    const searched = below.repositories.length ? await listedIn(root, below.repositories, [...excluding], signal) : new Set<string>()
    await Promise.all([...searched].map((folder) => visit(folder, folder)))
  }))
  return { roots: [...hidden].sort(), notes }
}

/**
 * Runs ripgrep with `args` over `roots` and the nested repositories they hold. ripgrep's default, that .gitignore only counts inside a repository, is
 * git's own; with --no-require-git a .gitignore would also reach into every repository nested below it.
 */
async function ripgrep(ctx: Context, roots: string[], args: string[], signal: AbortSignal) {
  const globs = args.flatMap((arg, index) => args[index - 1] === "--glob" && arg !== "!.git" ? [arg] : [])
  const nested = args.includes("--no-ignore") ? { roots: [], notes: [] } : await hiddenRepositories(roots, globs, signal)
  const child = Bun.spawn(["rg", ...args, "--", ...roots, ...nested.roots], { cwd: ctx.directory, stdout: "pipe", stderr: "pipe", signal })
  const [stdout, stderr, exit] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
  if (exit === 2 && !stdout) throw new Error(`rg failed: ${stderr.trim()}`)
  return { stdout, stderr, notes: nested.notes }
}

/**
 * Match counts by folder, most first, at the deepest folder level below the folder all matches share that fits in COUNT_ROWS rows; a file above
 * that level gets a row of its own.
 * @return the rows, or undefined when summing by folder saves no rows, as when every match sits in one folder or the top level alone is too wide
 */
function countsByFolder(names: string[], count: (name: string) => number, display: (name: string) => string) {
  const split = (shown: string) => {
    const segments = shown.split("/")
    // An absolute path keeps its leading slash on its first segment.
    return segments[0] === "" && segments.length > 1 ? [`/${segments[1]}`, ...segments.slice(2)] : segments
  }
  const paths = names.map((name) => ({ name, segments: split(display(name)) }))
  // The folder every match is in says nothing, so the levels start below it, and grepping a row's folder splits it further.
  let base = 0
  while (paths.every((item) => item.segments.length - 1 > base && item.segments[base] === paths[0]!.segments[base])) base++
  const group = (segments: string[], level: number) => segments.length > base + level ? `${segments.slice(0, base + level).join("/")}/` : segments.join("/")
  const width = (level: number) => new Set(paths.map((item) => group(item.segments, level))).size
  const deepest = Math.max(...paths.map((item) => item.segments.length)) - 1 - base
  if (deepest < 1 || width(1) > COUNT_ROWS) return undefined
  let level = 1
  while (level < deepest && width(level + 1) <= COUNT_ROWS) level++
  if (width(level) === names.length) return undefined
  const groups = new Map<string, { matches: number; files: number }>()
  for (const item of paths) {
    const key = group(item.segments, level)
    const sum = groups.get(key) ?? { matches: 0, files: 0 }
    sum.matches += count(item.name)
    sum.files++
    groups.set(key, sum)
  }
  return [...groups].sort((a, b) => b[1].matches - a[1].matches || a[0].localeCompare(b[0]))
    .map(([key, sum]) => key.endsWith("/") ? `${sum.matches} ${key} (${sum.files} file${sum.files === 1 ? "" : "s"})` : `${sum.matches} ${key}`)
}

type Submatch = Span & { replacement?: { text?: string; bytes?: string } }
/** A matched line as a grep replace would leave it, where its first replacements sit in it, and whether it differs from the line. */
type Replaced = { text: string; spans: Span[]; changed: boolean }
type GrepShape = {
  limit: number
  before: number
  after: number
  columns: number
  inside?: (line: number) => boolean
  spans?: Map<number, Span[]>
  replaced?: Map<number, Replaced>
  ripgrep: (file: string) => Promise<Map<number, string>>
}

/** One file of a grep page, read under its lock; `listed` holds the lines its block shows: the shown matches and the context lines they were given. */
type GrepFile = {
  name: string
  shape: GrepShape
  matches: number[]
  target?: Target
  /** The whole block, for a file that could not be read. */
  failed?: string
  anchored?: View
  /** Why the rows carry no tag, when nib could not hold the whole file. */
  untagged?: string
  /** The whole file, when nib holds it. */
  lines?: string[]
  lineAt: (line: number) => string | undefined
  shown: number[]
  listed: Set<number>
}

/**
 * Lists context lines around the page's shown matches while they come to at most GREP_CONTEXT_LINES. Past that, context that was asked for goes to
 * every definition, up to the output budget, or with no definition among the matches to the first matches, in page order up to the first that no
 * longer fits; the default context goes only to definitions, the same way, within the budget, so a grep listing many definitions stays a list.
 * @return the note saying which matches got context, when not all did
 */
function frameMatches(files: GrepFile[], before: number, after: number, asked: boolean) {
  const around = (file: GrepFile, line: number) => {
    const found: number[] = []
    for (let index = Math.max(1, line - before); index <= line + after; index++) {
      if (!file.listed.has(index) && file.lineAt(index) !== undefined && (!file.shape.inside || file.shape.inside(index))) found.push(index)
    }
    return found
  }
  const all = files.flatMap((file) => file.shown.map((line) => ({ file, line })))
  const wanted = files.reduce((sum, file) => sum + new Set(file.shown.flatMap((line) => around(file, line))).size, 0)
  if (wanted <= GREP_CONTEXT_LINES) {
    for (const { file, line } of all) for (const index of around(file, line)) file.listed.add(index)
    return
  }
  const defined = new Map(files.map((file) => [file, new Set(file.lines ? definitionLines(file.target!.display, file.lines, file.shown, file.shape.spans) : [])]))
  const definitions = all.filter(({ file, line }) => defined.get(file)!.has(line))
  const given = new Set<(typeof all)[number]>()
  let left = GREP_CONTEXT_LINES
  const give = (match: (typeof all)[number]) => {
    const fresh = around(match.file, match.line)
    if (fresh.length > left) return false
    left -= fresh.length
    for (const index of fresh) match.file.listed.add(index)
    given.add(match)
    return true
  }
  if (definitions.length && asked) {
    for (const match of definitions) {
      for (const index of around(match.file, match.line)) match.file.listed.add(index)
      given.add(match)
    }
  } else for (const match of definitions.length ? definitions : asked ? all : []) if (!give(match)) break
  const bare = all.find((match) => !given.has(match))
  if (!bare) return
  const over = `around all ${all.length} matches they would be ${wanted} lines, over grep's budget of ${GREP_CONTEXT_LINES}`
  const range = `${bare.file.target!.display}:${Math.max(1, bare.line - before)}-${bare.line + after}`
  if (!given.size) return `[no context lines: ${over}; ask with before or after for some around the first matches, or read a range like ${range}]`
  const first = [...given][0]!
  const which = !definitions.length ? `the first ${given.size} of ${all.length} matches`
    : given.size === 1 ? `the definition at ${first.file.target!.display}:${first.line}` : given.size < definitions.length ? `the first ${given.size} of ${definitions.length} definitions` : `the ${given.size} definitions`
  return `[context lines only around ${which}: ${over}; for another match, read a range like ${range}]`
}

/** Swaps a line's submatches for ripgrep's replacements; ripgrep reports UTF-8 byte offsets. */
function replaceLine(text: string, submatches: Submatch[]): Replaced {
  const body = text.replace(/\r?\n$/, "")
  const bytes = Buffer.from(body)
  const spans: Span[] = []
  let composed = ""
  let cursor = 0
  for (const submatch of submatches) {
    const start = Math.min(submatch.start, bytes.length)
    const replacement = submatch.replacement?.text ?? Buffer.from(submatch.replacement?.bytes ?? "", "base64").toString()
    composed += bytes.subarray(cursor, start).toString()
    spans.push({ start: composed.length, end: composed.length + replacement.length })
    composed += replacement
    cursor = Math.max(start, Math.min(submatch.end, bytes.length))
  }
  const result = composed + bytes.subarray(cursor).toString()
  return { text: result, spans: spans.slice(0, WINDOWS), changed: result !== body }
}

/** A `>N|` row: a line as a replace would leave it, cut like a match row, so a long one shows its first replacements. */
function replacementRow(line: number, content: string, spans: Span[] | undefined, columns: number) {
  const shown = visible(content)
  if (shown.length <= columns) return `>${line}|${shown}`
  const windows = matchWindows(content, spans, columns)
  return windows ? `>[${line} ${windows.label}]|${windows.text}` : `>[${line} cut at ${columns} of ${shown.length} chars]|${shown.slice(0, columns)}`
}

/** Splits `budget` across demands so small entries get everything and large ones share the rest equally. */
function allocate(demands: number[], budget: number) {
  const shares = demands.map(() => 0)
  let left = budget
  const order = demands.map((demand, index) => ({ demand, index })).sort((a, b) => a.demand - b.demand)
  order.forEach(({ demand, index }, position) => {
    shares[index] = Math.min(demand, Math.floor(left / (order.length - position)))
    left -= shares[index]!
  })
  return shares
}

async function listDirectory(file: FileHandle, location: string, display: string): Promise<Block> {
  const entries = (await readdir(process.platform === "linux" ? `/proc/self/fd/${file.fd}` : location, { withFileTypes: true }))
    .map((entry) => entry.name + (entry.isDirectory() ? "/" : entry.isSymbolicLink() ? "@" : "")).sort()
  return {
    head: `${display}/`,
    rows: entries.map((text) => ({ text })),
    finish: (shown) => shown < entries.length ? [`[${entries.length - shown} more entries not shown]`] : [],
  }
}

async function directoryFacts(file: FileHandle, location: string, display: string): Promise<Block> {
  const entries = await readdir(process.platform === "linux" ? `/proc/self/fd/${file.fd}` : location, { withFileTypes: true })
  const directories = entries.filter((entry) => entry.isDirectory()).length
  const modified = formatAge((await file.stat()).mtimeMs)
  return {
    head: `${display}/ directory, ${entries.length} entr${entries.length === 1 ? "y" : "ies"} (${directories} director${directories === 1 ? "y" : "ies"}); modified ${modified}`,
    rows: [],
    finish: () => [],
  }
}

/** Directory trees are joined into one result; one line asked for alone may use the whole text limit, anything else shares the output budget. */
export class Engine {
  readonly store = new SnapshotStore()
  /** Patches stored for `REAPPLY` by session and token, oldest first: refused patches a straight retry would apply, and grep replace previews. */
  readonly #tokens = new Map<string, { input: string; preview?: Preview }>()

  async read(ctx: Context, args: { paths: string[]; maxLineLength?: number; plain?: boolean; collapse?: boolean; info?: boolean }): Promise<Result> {
    if (args.info && (args.plain || args.collapse !== undefined || args.maxLineLength !== undefined)) {
      throw new Error("info: true gives facts instead of content, so it takes no plain, collapse or maxLineLength")
    }
    const caps: string[] = []
    const maxLineLength = capped("maxLineLength", args.maxLineLength, TEXT_LIMIT, caps) ?? LINE_LIMIT
    const blocks: Block[] = []
    const absent: string[] = []
    const images: Image[] = []
    const note = (head: string): Block => ({ head, rows: [], finish: () => [] })
    let single = false
    for (const entry of args.paths) {
      const { file: selected, selection } = await splitSelector(entry, ctx)
      const { file: input, since } = await splitSince(selected, ctx)
      const narrowed = selection.ranges !== undefined || selection.tail !== undefined
      if (since && (narrowed || args.info || args.plain)) throw new Error(`${entry}: path#TAG shows what changed since that tag, so it takes no line selector, info or plain`)
      if (args.info && narrowed) throw new Error(`${entry}: info describes whole files, so it takes no line selector`)
      const only = selection.ranges?.length === 1 ? selection.ranges[0]! : undefined
      single = args.paths.length === 1 && only !== undefined && only.end === only.start
      const target = await resolveTarget(input, ctx).catch((error: unknown) => {
        if (error instanceof NotFound || error instanceof Outside) return error
        throw error
      })
      // A path that names nothing, or that the sandbox refuses, is noted in its place, so the other files of a read still come back.
      if (target instanceof Error) {
        absent.push(target instanceof Outside ? target.message : await missingPath(entry, ctx))
        blocks.push(note(`[${absent.at(-1)}]`))
        continue
      }
      blocks.push(await serialized(target.canonical, ctx, () => inParent(target, async (parent) => {
        const file = await open(parent.location, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
        try {
          await checkDescriptor(file, parent.location, target.canonical)
          if ((await file.stat()).isDirectory()) {
            return args.info ? await directoryFacts(file, parent.location, target.display) : await listDirectory(file, parent.location, target.display)
          }
          if (args.info) return await this.#facts(ctx, target, file)
          // Images and PDFs fall through to the usual handling, since plain only changes how text lines print.
          const plain = args.plain ? await this.#plainBlock(ctx, target, file, selection, maxLineLength, args.collapse !== false) : undefined
          if (plain) return plain
          // A file too big to hold as text is noted rather than failing the call, so the other files of a read still come back.
          const tooBig = (bytes: number) => note(`[${target.display}] skipped: ${(bytes / 1048576).toFixed(1)} MiB, over the 2 MiB text limit. Read it with plain: true (a range like ${target.display}:-200 works too), or grep it.`)
          const size = Number((await file.stat()).size)
          if (size > MEDIA_LIMIT) return tooBig(size)
          const current = await snapshot(file, MEDIA_LIMIT)
          await parent.check()
          await checkDescriptor(file, parent.location, target.canonical)
          ctx.signal.throwIfAborted()
          const attachment = mime(current.bytes)
          if (attachment === "application/pdf") return note(`${target.display}: PDF is not supported here; use the built-in Read tool`)
          if (attachment) {
            images.push({ data: current.bytes.toString("base64"), mimeType: attachment })
            return note(`${target.display}: ${attachment} image attached`)
          }
          if (current.bytes.length > TEXT_LIMIT) return tooBig(current.bytes.length)
          if (since) return this.#sinceBlock(ctx, target, text(current.bytes), since, maxLineLength)
          return this.#textBlock(ctx, target, text(current.bytes), selection, maxLineLength, args.maxLineLength !== undefined)
        } finally { await file.close() }
      })))
    }
    const size = (row: string) => Buffer.byteLength(row) + 1
    const shares = allocate(blocks.map((block) => block.rows.reduce((sum, row) => sum + size(row.text), 0)), single && maxLineLength > LINE_LIMIT ? TEXT_LIMIT + 1024 : OUTPUT_LIMIT)
    const parts = blocks.map((block, index) => {
      let left = shares[index]!
      let shown = 0
      while (shown < block.rows.length && size(block.rows[shown]!.text) <= left) left -= size(block.rows[shown++]!.text)
      return [block.head, ...block.rows.slice(0, shown).map((row) => row.text), ...block.finish(shown)].join("\n")
    })
    if (absent.length === args.paths.length) throw new Error(absent.join("; "))
    return { text: [...caps, ...parts].join("\n\n"), images }
  }

  /** One line of facts about a file instead of its content; a file nib can hold as text also gets its tag, recorded with no line seen. */
  async #facts(ctx: Context, target: Target, file: FileHandle): Promise<Block> {
    const info = await file.stat()
    const size = Number(info.size)
    const line = (tag: string | undefined, facts: string[]): Block => ({
      head: `[${target.display}${tag ? `#${tag}` : ""}] ${[...facts, `modified ${formatAge(info.mtimeMs)}`].join("; ")}`,
      rows: [],
      finish: () => [],
    })
    const start = Buffer.alloc(Math.min(size, 16))
    await file.read(start, 0, start.length, 0)
    const attachment = mime(start)
    if (attachment) return line(undefined, [`${attachment === "application/pdf" ? "PDF" : attachment}, ${formatSize(size)}`])
    // A log still being written cannot be held as one version, so like a file over the text limit it is only streamed.
    const held = size <= TEXT_LIMIT ? await snapshot(file, TEXT_LIMIT).catch((error: unknown) => {
      if (error instanceof ChangedWhileReading) return undefined
      throw error
    }) : undefined
    let facts: LineFacts
    let tag: string | undefined
    let kind: string | undefined
    if (held) {
      const counter = lineFacts()
      counter.feed(held.bytes)
      facts = counter.done()
      if (!facts.binary) {
        let source: Text
        try { source = text(held.bytes) } catch { return line(undefined, [`not UTF-8 text, ${formatSize(size)}`]) }
        const anchored = view(source)
        tag = anchored.tag
        kind = generatedKind(target.canonical, Buffer.byteLength(source.body), anchored.lines)
        this.store.record(ctx.session, target.canonical, anchored, [])
      }
    } else {
      facts = await measure(file, size, ctx.signal)
      kind = generatedKind(target.canonical, size, [])
    }
    if (facts.binary) return line(undefined, [`binary, ${formatSize(size)}`])
    if (!facts.lines) return line(tag, ["empty"])
    const parts = [
      `${facts.lines} line${facts.lines === 1 ? "" : "s"}, ${formatSize(size)}, ${approximateTokens(size)}`,
      `longest line ${facts.longest} char${facts.longest === 1 ? "" : "s"}${facts.lines > 1 ? ` (line ${facts.longestAt})` : ""}`,
    ]
    if (facts.crlf && facts.lf) parts.push(`mixed line endings (${facts.crlf} CRLF, ${facts.lf} LF)`)
    else if (facts.crlf || facts.lf) parts.push(facts.crlf ? "CRLF" : "LF")
    if (!facts.final) parts.push("no final newline")
    if (facts.bom) parts.push("BOM")
    if (facts.escapes) parts.push("color codes")
    if (kind) parts.push(kind)
    if (!held) parts.push(`${size > TEXT_LIMIT ? "over 2 MiB" : "changing while read"}, so no tag: read it with plain: true, or grep it`)
    return line(tag, parts)
  }

  /**
   * What changed since the version tagged `since`: changed lines with context, numbered as the file is now, and removed lines as -| rows.
   * Like an edit response, unchanged lines keep being seen only where their number held, and stay known content where it moved.
   */
  #sinceBlock(ctx: Context, target: Target, source: Text, since: string, maxLineLength: number): Block {
    const live = view(source)
    const note = (head: string): Block => ({ head, rows: [], finish: () => [] })
    const old = this.store.versions(ctx.session, target.canonical).find((version) => version.tag === since)
    if (!old) {
      const elsewhere = this.store.origins(ctx.session, since, target.canonical)
      return note(`[${target.display}] #${since} is not a version of this file that nib kept in this session${elsewhere.length ? ` (it was issued for ${elsewhere.join(", ")})` : ""}; read the file instead.`)
    }
    if (old.key === live.key) return note(`[${target.display}#${live.tag}] unchanged since #${since}`)
    const changes = lineChanges(old.lines, live.lines)
    if (!changes) {
      this.store.record(ctx.session, target.canonical, live, [])
      return note(`[${target.display}#${live.tag}] changed too much since #${since} to compare in time; read it again.`)
    }
    // A current line with `old` kept its content from that old line; one without was added.
    const entries: ({ line: number; old?: number } | { removed: string })[] = []
    let before = 1
    let after = 1
    for (const change of changes) {
      for (const removed of change.removed ? change.value : []) {
        entries.push({ removed })
        before++
      }
      if (!change.removed) for (let offset = 0; offset < change.value.length; offset++) entries.push(change.added ? { line: after++ } : { line: after++, old: before++ })
    }
    const listed = new Set<number>()
    entries.forEach((entry, index) => {
      if (!("removed" in entry) && entry.old !== undefined) return
      for (let near = Math.max(0, index - PREVIEW_CONTEXT); near <= Math.min(entries.length - 1, index + PREVIEW_CONTEXT); near++) listed.add(near)
    })
    const rows: Block["rows"] = []
    let previous = -1
    for (const index of [...listed].sort((a, b) => a - b)) {
      if (previous >= 0 && index > previous + 1) rows.push({ text: "..." })
      const entry = entries[index]!
      if ("removed" in entry) {
        const shown = visible(entry.removed)
        rows.push({ text: `-|${shown.length > maxLineLength ? shown.slice(0, maxLineLength) + "..." : shown}` })
      } else {
        const row = lineRow(entry.line, live.lines[entry.line - 1]!, maxLineLength)
        const marker = entry.old === undefined ? "+" : " "
        rows.push(row.anchor ? { text: marker + row.text, line: entry.line } : { text: marker + row.text, cut: row.cut, colored: row.colored })
      }
      previous = index
    }
    const knew = (line: number) => !!old.seen && knownLine(old, line)
    // Unchanged lines that moved by one offset, as stretches; only those holding a line the agent knew get a note.
    const moved: { from: number; to: number; offset: number; known: boolean }[] = []
    for (const entry of entries) {
      if ("removed" in entry || entry.old === undefined || entry.old === entry.line) continue
      const last = moved.at(-1)
      if (last && last.to === entry.line - 1 && last.offset === entry.line - entry.old) last.to = entry.line
      else moved.push({ from: entry.line, to: entry.line, offset: entry.line - entry.old, known: false })
      moved.at(-1)!.known ||= knew(entry.old)
    }
    const span = (low: number, high: number) => low === high ? `${low}` : `${low}-${high}`
    const stretches = moved.filter((stretch) => stretch.known)
    const notes = stretches.slice(0, SINCE_NOTES).map(({ from, to, offset }) => `[line${from === to ? "" : "s"} ${span(from, to)} ${from === to ? "is" : "are"} old line${from === to ? "" : "s"} ${span(from - offset, to - offset)}, moved ${offset > 0 ? "down" : "up"} by ${Math.abs(offset)}]`)
    if (stretches.length > SINCE_NOTES) notes.push(`[${stretches.length - SINCE_NOTES} more stretches of lines you knew moved]`)
    const carried = entries.flatMap((entry) => "line" in entry && entry.old === entry.line && (!old.seen || old.seen.has(entry.old)) ? [entry.line] : [])
    const known = entries.flatMap((entry) => "line" in entry && entry.old !== undefined && knew(entry.old) ? [entry.line] : [])
    const added = entries.filter((entry) => "line" in entry && entry.old === undefined).length
    return {
      head: `[${target.display}#${live.tag}] changed since #${since}: ${added} line${added === 1 ? "" : "s"} added, ${entries.length - live.lines.length} removed`,
      rows,
      finish: (shown) => {
        this.store.record(ctx.session, target.canonical, live, [...carried, ...rows.slice(0, shown).flatMap((row) => row.line === undefined ? [] : [row.line])], known)
        return [...shown < rows.length ? ["[changes cut by the output limit; read the ranges you need]"] : [], ...notes, ...rowNotes(target.display, rows.slice(0, shown))]
      },
    }
  }

  /**
   * Lines for looking only: no numbers, tag or snapshot, escape codes stripped, long lines shortened and, unless `collapse` is false, repetition collapsed.
   * It streams the file up to its size when opened, so huge logs and logs still being written read fine; undefined for images and PDFs.
   */
  async #plainBlock(ctx: Context, target: Target, file: FileHandle, selection: Selection, maxLineLength: number, collapse: boolean): Promise<Block | undefined> {
    const size = Number((await file.stat()).size)
    const start = Buffer.alloc(Math.min(size, 8192))
    await file.read(start, 0, start.length, 0)
    if (mime(start)) return
    if (start.includes(0)) throw new Error("Binary files are not supported")
    // A line can carry many escape bytes, so more is kept than the visible width.
    const cap = maxLineLength * 4 + 4096
    const scan = (wanted: (line: number) => boolean, ring?: number) => scanLines(file, size, ctx.signal, wanted, cap, ring)
    const ranges = selection.ranges ?? [{ start: 1 }]
    const inRange = (line: number) => ranges.some((range) => line >= range.start && line <= (range.end ?? range.start + READ_LINES - 1))
    // A tail that fits the line cap is kept in one pass as a ring of the last lines; a longer one needs the line count first.
    const tailed = async (count: number) => {
      if (count <= READ_LINES) return scan(() => true, count)
      const before = (await scan(() => false)).total - count
      return scan((line) => line > before)
    }
    const { total, kept } = selection.tail ? await tailed(selection.tail) : await scan(inRange)
    if (!total) return { head: `[${target.display}] empty, plain`, rows: [], finish: () => [] }
    const selected = selectLines(selection, total)
    const decoder = new TextDecoder()
    const rows: { text: string; at: number }[] = []
    let similar = false
    // Repetition is collapsed per selected range, never across a gap between ranges.
    let segment: { text: string; at: number }[] = []
    const flush = () => {
      const compressed = collapse ? compress(segment) : { rows: segment, similar: false }
      rows.push(...compressed.rows)
      similar ||= compressed.similar
      segment = []
    }
    let missing: number | undefined
    let last = 0
    for (const range of selected) {
      if (last && range.start > last + 1) {
        flush()
        rows.push({ text: `[lines ${last + 1}-${range.start - 1} skipped]`, at: range.start })
      }
      for (let line = range.start; line <= range.end && missing === undefined; line++) {
        const entry = kept.get(line)
        if (!entry) {
          missing = line
          break
        }
        let content = visible(decoder.decode(entry.bytes)).replace(/[\x00-\x08\x0b-\x1f\x7f]/g, "")
        if (line === 1 && content.startsWith(BOM)) content = content.slice(BOM.length)
        if (entry.cut || content.length > maxLineLength) content = content.slice(0, maxLineLength) + "..."
        segment.push({ text: content, at: line })
        last = line
      }
      if (missing !== undefined) break
    }
    flush()
    const whole = selected.length === 1 && selected[0]!.start === 1 && selected[0]!.end === total
    return {
      head: whole ? `[${target.display}] ${total} lines, plain` : `[${target.display}] lines ${selected.map((range) => range.start === range.end ? `${range.start}` : `${range.start}-${range.end}`).join(", ")} of ${total}, plain`,
      rows,
      finish: (shown) => {
        const notes = similar ? ["[repeats were matched with their numbers ignored; read with collapse: false to see every line]"] : []
        if (shown < rows.length) {
          const next = rows[shown]!
          return [Buffer.byteLength(next.text) + 1 > OUTPUT_LIMIT
            ? `[line ${next.at} is longer than one call's output; read ${target.display}:${next.at} alone]`
            : `[output limit reached; continue with ${target.display}:${next.at}-]`, ...notes]
        }
        if (missing !== undefined) return [`[continue with ${target.display}:${missing}-]`, ...notes]
        const open = !selection.tail && (selection.ranges ?? [{ start: 1 }]).every((range) => range.end === undefined)
        return [...open && last < total ? [`[continue with ${target.display}:${last + 1}-]`] : [], ...notes]
      },
    }
  }

  /** `explicit` is whether the agent chose `maxLineLength`; when it did not, lines of a minified file are cut at MINIFIED_COLUMNS instead. */
  #textBlock(ctx: Context, target: Target, source: Text, selection: Selection, maxLineLength: number, explicit: boolean): Block {
    const anchored = view(source)
    const total = anchored.lines.length
    const bytes = Buffer.byteLength(source.body)
    const squeezed = !explicit && maxLineLength > MINIFIED_COLUMNS && anchored.lines.some((content) => content.length > MINIFIED_COLUMNS) &&
      minifiedReason(target.canonical, bytes, anchored.lines) !== undefined
    const limit = squeezed ? MINIFIED_COLUMNS : maxLineLength
    const head = `[${target.display}#${anchored.tag}]${squeezed ? ` minified, so lines are cut at ${MINIFIED_COLUMNS} chars; pass maxLineLength for more` : ""}`
    const rowFor = (line: number): { text: string; line?: number } & Marks => {
      const row = lineRow(line, anchored.lines[line - 1]!, limit)
      return row.anchor ? { text: row.text, line } : { text: row.text, cut: row.cut, colored: row.colored }
    }
    const record = (rows: { line?: number }[]) => this.store.record(ctx.session, target.canonical, anchored, rows.flatMap((row) => row.line === undefined ? [] : [row.line]))
    const cuts = (rows: Marks[]) => rowNotes(target.display, rows)
    // A row bigger than a whole call's budget never fits, so continuing from it would repeat the same cut.
    const stop = (line: number, text: string) => Buffer.byteLength(text) + 1 > OUTPUT_LIMIT
      ? `[line ${line} has ${anchored.lines[line - 1]!.length} chars, more than one call's output; read ${target.display}:${line} alone]`
      : `[output limit reached; continue with ${target.display}:${line}-]`
    const size = `${total} lines, ${approximateTokens(bytes)}`
    if (!selection.ranges && !selection.tail) {
      const reason = junk(target.canonical, bytes, anchored.lines)
      if (reason) {
        return {
          head: `[${target.display}] skipped: ${reason}, ${size}. Read ${target.display}:1- to read it anyway, a range like ${target.display}:1-100, or grep it.`,
          rows: [],
          finish: () => [],
        }
      }
      if (total > OUTLINE_LINES || bytes > OUTLINE_BYTES) {
        const sketch = outline(target.display, anchored.lines)
        const anchors = sketch.rows.filter((row) => "line" in row).length
        // Flat data (logs, CSV, lists) puts nearly every line at the top level; its head says more than such an "outline".
        if (anchors >= OUTLINE_MIN_ROWS && sketch.candidates * 2 <= anchored.lines.filter((content) => content.trim()).length) {
          const rows = sketch.rows.map((row) => "line" in row ? rowFor(row.line) : { text: row.text })
          return {
            head: `${head} ${size}; outline only. Read the ranges you need, like ${target.display}:120-180, or ${target.display}:1- for everything.`,
            rows,
            finish: (shown) => {
              record(rows.slice(0, shown))
              return [...shown < rows.length ? ["[outline cut by the output limit]"] : [], ...cuts(rows.slice(0, shown))]
            },
          }
        }
        const rows = Array.from({ length: Math.min(HEAD_LINES, total) }, (_, index) => ({ ...rowFor(index + 1), at: index + 1 }))
        return {
          head: `${head} ${size}; first ${rows.length} lines only. Continue with ${target.display}:${rows.length + 1}-, or grep it.`,
          rows,
          finish: (shown) => {
            record(rows.slice(0, shown))
            return [...shown < rows.length ? [stop(rows[shown]!.at, rows[shown]!.text)] : [], ...cuts(rows.slice(0, shown))]
          },
        }
      }
    }
    const selected = selectLines(selection, total)
    const rows: ({ text: string; line?: number; at: number } & Marks)[] = []
    let last = 0
    for (const range of selected) {
      if (last && range.start > last + 1) rows.push({ text: "...", at: range.start })
      for (let line = range.start; line <= range.end; line++) rows.push({ ...rowFor(line), at: line })
      last = range.end
    }
    return {
      head,
      rows,
      finish: (shown) => {
        record(rows.slice(0, shown))
        const notes = cuts(rows.slice(0, shown))
        if (shown < rows.length) return [stop(rows[shown]!.at, rows[shown]!.text), ...notes]
        const whole = selected.length === 1 && selected[0]!.start === 1 && selected[0]!.end === total
        // The missing final newline shows only with the last line, where a task to add one would look for it.
        const ending = last === total && source.body !== "" && !source.body.endsWith("\n") ? [`[no final newline; ${FINAL_NEWLINE}]`] : []
        if (whole) return [...ending, ...notes]
        const open = !selection.tail && (selection.ranges ?? [{ start: 1 }]).every((range) => range.end === undefined)
        return [...ending, `[${total} lines total${open && last < total ? `; continue with ${target.display}:${last + 1}-` : ""}]`, ...notes]
      },
    }
  }

  async grep(ctx: Context, args: { pattern: string; paths?: string[]; glob?: string | string[]; ignoreCase?: boolean; literal?: boolean; before?: number; after?: number; skip?: number; mode?: "matches" | "files" | "count"; noIgnore?: boolean; maxLineLength?: number; replace?: string }): Promise<Result> {
    const caps: string[] = []
    const multiline = args.pattern.includes("\n") || args.pattern.includes("\\n")
    if (args.replace !== undefined && (args.mode ?? "matches") !== "matches") throw new Error("replace previews its changes as rows, so it needs mode matches")
    if (args.replace !== undefined && multiline) throw new Error("replace works on matches within one line; change multi-line text with edit")
    // A line selector narrows its file's matches; the same file named again without one searches all of it.
    const selections = new Map<string, Selection[] | undefined>()
    const skipped: string[] = []
    const kept: string[] = []
    const entries = args.paths?.length ? args.paths : ["."]
    for (const entry of entries) {
      const { file, selection } = await splitSelector(entry, ctx)
      const refused = await outsideSandbox(ctx, file)
      if (refused) {
        skipped.push(refused)
        continue
      }
      const root = path.resolve(ctx.directory, expandHome(file))
      const chosen = selection.ranges || selection.tail ? selection : undefined
      // ripgrep only warns about a missing path, which would read as a search that found nothing.
      const found = await stat(root).catch(() => undefined)
      if (!found) {
        skipped.push(await missingPath(entry, ctx))
        continue
      }
      if (chosen && found.isDirectory()) throw new Error(`${entry}: line selectors need a file, not a directory`)
      kept.push(entry)
      const earlier = selections.get(root)
      selections.set(root, !chosen || (selections.has(root) && !earlier) ? undefined : [...earlier ?? [], chosen])
    }
    // A path that names nothing, or that the sandbox refuses, is noted, and the other paths are searched without it.
    if (skipped.length) {
      if (!kept.length) throw new Error(skipped.join("; "))
      const result = await this.grep(ctx, { ...args, paths: kept })
      return { ...result, text: `[${skipped.join("; ")}; searched the other paths]\n\n${result.text}` }
    }
    const roots = [...selections.keys()]
    const command = ["--json", "--no-config", "--hidden", "--glob", "!.git", "--max-columns", "0"]
    if (args.ignoreCase) command.push("--ignore-case")
    if (args.literal) command.push("--fixed-strings")
    if (args.noIgnore) command.push("--no-ignore")
    if (multiline) command.push("--multiline")
    if (args.replace !== undefined) command.push("--replace", args.replace)
    for (const glob of [args.glob ?? []].flat()) command.push("--glob", glob)
    command.push("--regexp", args.pattern)
    const signal = AbortSignal.any([ctx.signal, AbortSignal.timeout(GREP_TIMEOUT)])
    const { stdout, stderr, notes: searched } = await ripgrep(ctx, roots, command, signal)
    const files = new Map<string, Set<number>>()
    const spans = new Map<string, Map<number, Span[]>>()
    const replaced = new Map<string, Map<number, Replaced>>()
    for (const line of stdout.split("\n")) {
      if (!line.startsWith('{"type":"match"')) continue
      const event = JSON.parse(line) as { data: { path: { text?: string }; line_number: number; lines: { text?: string }; submatches?: Submatch[] } }
      const file = event.data.path.text
      if (!file) continue
      const matched = files.get(file) ?? new Set<number>()
      const span = Math.max(1, (event.data.lines.text ?? "").replace(/\n$/, "").split("\n").length)
      for (let offset = 0; offset < span; offset++) matched.add(event.data.line_number + offset)
      files.set(file, matched)
      const found = matchSpans(event.data.lines.text ?? "", event.data.submatches ?? [])
      if (found.length) spans.set(file, (spans.get(file) ?? new Map()).set(event.data.line_number, found))
      if (args.replace !== undefined && event.data.lines.text !== undefined) {
        replaced.set(file, (replaced.get(file) ?? new Map()).set(event.data.line_number, replaceLine(event.data.lines.text, event.data.submatches ?? [])))
      }
    }
    // ripgrep is handed paths, not descriptors, so in the sandbox a file it found by a link changed since is dropped unless it resolves inside.
    if (ctx.sandbox) {
      const roots = await workspaceRoots(ctx)
      await Promise.all([...files.keys()].map(async (file) => {
        const real = await realpath(file).catch(() => file)
        if (!roots.some((root) => within(root, real))) files.delete(file)
      }))
    }
    const inside = new Map<string, (line: number) => boolean>()
    for (const [file, lines] of files) {
      const chosen = selections.get(file)
      if (!chosen) continue
      const inRange = await selectedLines(ctx, file, chosen)
      inside.set(file, inRange)
      for (const line of lines) if (!inRange(line)) lines.delete(line)
      if (!lines.size) files.delete(file)
    }
    if (!files.size) return { text: [stderr.trim() ? `No matches found (rg: ${stderr.trim().split("\n")[0]})` : "No matches found", ...searched].join("\n\n") }
    const names = [...files.keys()].sort()
    const skip = args.skip ?? 0
    const mode = args.mode ?? "matches"
    if (mode !== "matches") {
      const total = names.reduce((sum, name) => sum + files.get(name)!.size, 0)
      const display = (name: string) => within(ctx.directory, name) ? path.relative(ctx.directory, name) : name
      const grouped = mode === "count" && !skip && names.length > COUNT_ROWS ? countsByFolder(names, (name) => files.get(name)!.size, display) : undefined
      if (grouped) return { text: [`${total} matches in ${names.length} files, by folder`, grouped.join("\n"), "[grep one folder for its files' counts]", ...searched].join("\n\n") }
      const page = names.slice(skip, skip + GREP_LIST_FILES)
      if (!page.length) return { text: `No more results (${names.length} matching files; skip ${skip} is past the end)` }
      const rows = page.map((name) => mode === "files" ? display(name) : `${files.get(name)!.size} ${display(name)}`)
      const notes = skip + page.length < names.length ? [`[${names.length} matching files; continue with skip ${skip + page.length}]`] : []
      return { text: [`${total} matches in ${names.length} files`, rows.join("\n"), ...notes, ...searched].join("\n\n") }
    }
    const page = names.slice(skip, skip + GREP_FILES)
    if (!page.length) return { text: `No more results (${names.length} matching files; skip ${skip} is past the end)` }
    // Lines from this page on that a replace would change; the token reports those it leaves out.
    const changing = names.slice(skip).reduce((sum, name) => sum + [...files.get(name)!].filter((line) => replaced.get(name)?.get(line)?.changed).length, 0)
    const replacing = args.replace !== undefined
    let total = 0
    for (const name of page) total += files.get(name)!.size
    const shape = {
      limit: names.length === 1 ? GREP_SINGLE_FILE : GREP_PER_FILE,
      before: capped("before", args.before, GREP_CONTEXT, caps) ?? (replacing ? 0 : 1),
      after: capped("after", args.after, GREP_CONTEXT, caps) ?? (replacing ? 0 : 3),
      columns: capped("maxLineLength", args.maxLineLength, LINE_LIMIT, caps) ?? GREP_COLUMNS,
    }
    // Rows for a file too big to hold, or one still growing, come from ripgrep itself, which reports the context lines too.
    const fallback = async (file: string) => {
      const flags = [`--before-context=${shape.before}`, `--after-context=${shape.after}`, ...inside.has(file) ? [] : [`--max-count=${shape.limit}`]]
      const texts = new Map<number, string>()
      for (const line of (await ripgrep(ctx, [file], [...command, ...flags], signal)).stdout.split("\n")) {
        if (!line.startsWith('{"type":"match"') && !line.startsWith('{"type":"context"')) continue
        const event = JSON.parse(line) as { data: { line_number: number; lines: { text?: string; bytes?: string } } }
        const content = event.data.lines.text ?? Buffer.from(event.data.lines.bytes ?? "", "base64").toString("utf8")
        content.replace(/\n$/, "").split("\n").forEach((text, offset) => texts.set(event.data.line_number + offset, text.replace(/\r$/, "")))
      }
      return texts
    }
    const targets = await Promise.all(page.map((name) => resolveTarget(name, ctx).catch(() => undefined)))
    // The page's files are locked together, as an edit locks its files, so context lines are shared out across all of them and each block still comes from one snapshot.
    return serializedAll(targets.flatMap((target) => target ? [target.canonical] : []), ctx, async () => {
      const loaded: GrepFile[] = []
      for (const [index, name] of page.entries()) {
        const matches = [...files.get(name)!].sort((a, b) => a - b)
        // A replace shows every match of a file nib holds, so its token covers them all unless the output budget cuts it.
        loaded.push(await this.#grepLoad(ctx, name, targets[index], matches, { ...shape, limit: replacing ? Number.POSITIVE_INFINITY : shape.limit, inside: inside.get(name), spans: spans.get(name), replaced: replaced.get(name), ripgrep: fallback }))
      }
      const framing = frameMatches(loaded, shape.before, shape.after, args.before !== undefined || args.after !== undefined)
      const budget = { left: OUTPUT_LIMIT }
      const parts: string[] = []
      const patches: string[] = []
      const previewed = new Map<string, Set<number>>()
      let changes = 0
      for (const file of loaded) {
        const result = this.#grepBlock(ctx, file, budget, !parts.length)
        if (!result) break
        parts.push(result.block)
        if (result.patch) patches.push(result.patch)
        if (result.previewed) previewed.set(result.previewed.canonical, new Set(result.previewed.lines))
        changes += result.changes
      }
      const notes: string[] = []
      if (parts.length < page.length) notes.push(`[output limit reached after ${parts.length} files; narrow the search or continue with skip ${skip + parts.length}]`)
      else if (skip + page.length < names.length) notes.push(`[${names.length} matching files; continue with skip ${skip + page.length}]`)
      if (patches.length) notes.push(`To apply the ${changes} replaced line${changes === 1 ? "" : "s"} shown (>N| rows), call edit with input: REAPPLY ${this.#remember(ctx.session, patches.join("\n"), { shown: previewed, left: Math.max(0, changing - changes) })}`)
      else if (replacing) notes.push("[no replacement to apply: the shown matches stay the same or are in files nib cannot edit]")
      if (framing) notes.push(framing)
      return { text: [`${total} matches in ${page.length} files${names.length > page.length ? ` (of ${names.length})` : ""}`, ...parts, ...notes, ...caps, ...searched].join("\n\n") }
    })
  }

  /** Lists the files under `paths` that grep would search, with line counts for text nib can hold, sizes and ages; filters and sorts by size or age. */
  async find(ctx: Context, args: { paths?: string[]; glob?: string | string[]; noIgnore?: boolean; minSize?: string; newer?: string; sort?: "path" | "size" | "modified"; skip?: number }): Promise<Result> {
    const minSize = args.minSize === undefined ? undefined : parseSize(args.minSize)
    const newer = args.newer === undefined ? undefined : parseAge(args.newer)
    const sort = args.sort ?? "path"
    const roots: string[] = []
    const entries = args.paths?.length ? args.paths : ["."]
    const skipped: string[] = []
    const kept: string[] = []
    for (const entry of entries) {
      const refused = await outsideSandbox(ctx, entry)
      const root = path.resolve(ctx.directory, expandHome(entry))
      if (refused) skipped.push(refused)
      else if (!await stat(root).then(() => true, () => false)) skipped.push(await missingPath(entry, ctx))
      else {
        roots.push(root)
        kept.push(entry)
      }
    }
    // A path that names nothing, or that the sandbox refuses, is noted, and the other paths are listed without it.
    if (skipped.length) {
      if (!kept.length) throw new Error(skipped.join("; "))
      const result = await this.find(ctx, { ...args, paths: kept })
      return { ...result, text: `[${skipped.join("; ")}; listed the other paths]\n\n${result.text}` }
    }
    const command = ["--files", "--null", "--no-config", "--hidden", "--glob", "!.git"]
    if (args.noIgnore) command.push("--no-ignore")
    for (const glob of [args.glob ?? []].flat()) command.push("--glob", glob)
    const signal = AbortSignal.any([ctx.signal, AbortSignal.timeout(GREP_TIMEOUT)])
    const { stdout, stderr, notes: searched } = await ripgrep(ctx, roots, command, signal)
    const listed = [...new Set(stdout.split("\0").filter(Boolean))].sort()
    // As in grep, a file ripgrep reached by a link changed since is dropped unless it resolves inside the sandbox.
    const allowedRoots = ctx.sandbox ? await workspaceRoots(ctx) : []
    const reals = ctx.sandbox ? await Promise.all(listed.map((name) => realpath(name).catch(() => name))) : []
    const names = ctx.sandbox ? listed.filter((_, index) => allowedRoots.some((root) => within(root, reals[index]!))) : listed
    if (!names.length) return { text: [stderr.trim() ? `No files found (rg: ${stderr.trim().split("\n")[0]})` : "No files found", ...searched].join("\n\n") }
    const describe = async (list: string[]) => {
      const found: { name: string; size: number; modified: number }[] = []
      for (let index = 0; index < list.length; index += FIND_BATCH) {
        signal.throwIfAborted()
        const infos = await Promise.all(list.slice(index, index + FIND_BATCH).map((name) => stat(name).catch(() => undefined)))
        infos.forEach((info, offset) => { if (info?.isFile()) found.push({ name: list[index + offset]!, size: info.size, modified: info.mtimeMs }) })
      }
      return found
    }
    const skip = args.skip ?? 0
    // Without a filter or a sort by size or age, only the page needs its files looked at.
    const whole = minSize !== undefined || newer !== undefined || sort !== "path"
    const now = Date.now()
    const matching = whole
      ? (await describe(names)).filter((item) => (minSize === undefined || item.size >= minSize) && (newer === undefined || now - item.modified <= newer))
      : undefined
    if (sort === "size") matching!.sort((a, b) => b.size - a.size)
    if (sort === "modified") matching!.sort((a, b) => b.modified - a.modified)
    const total = matching?.length ?? names.length
    const page = matching ? matching.slice(skip, skip + FIND_FILES) : await describe(names.slice(skip, skip + FIND_FILES))
    if (!page.length) return { text: total ? `No more results (${total} files; skip ${skip} is past the end)` : `No files found (none of the ${names.length} listed pass the filters)` }
    const rows = await Promise.all(page.map(async (item) => {
      const display = within(ctx.directory, item.name) ? path.relative(ctx.directory, item.name) : item.name
      const facts = [formatSize(item.size), formatAge(item.modified)]
      if (!item.size) return `${display} (empty, ${facts[1]})`
      const bytes = item.size <= TEXT_LIMIT ? await readFile(item.name).catch(() => undefined) : undefined
      if (!bytes) return `${display} (${facts.join(", ")})`
      const attachment = mime(bytes)
      // Like git, a file counts as binary by its start; reading every byte of every listed file would cost more than the listing is worth.
      const start = bytes.subarray(0, FIND_SNIFF)
      if (attachment || start.some((byte) => byte < 9 || (byte > 13 && byte < 32 && byte !== 27) || byte === 127)) return `${display} (${attachment ?? "binary"}, ${facts.join(", ")})`
      let lines = bytes[bytes.length - 1] === 10 ? 0 : 1
      for (let at = bytes.indexOf(10); at >= 0; at = bytes.indexOf(10, at + 1)) lines++
      return `${display} (${lines} line${lines === 1 ? "" : "s"}, ${facts.join(", ")})`
    }))
    const conditions = [minSize !== undefined ? `at least ${formatSize(minSize)}` : "", newer !== undefined ? `modified in the last ${args.newer}` : ""].filter(Boolean)
    const head = `${total} file${total === 1 ? "" : "s"}${conditions.length ? ` ${conditions.join(", ")} (of ${names.length} listed)` : ""}${sort === "size" ? ", largest first" : sort === "modified" ? ", newest first" : ""}`
    const more = (shown: number) => `[${total} files; continue with skip ${skip + shown}]`
    let left = OUTPUT_LIMIT - Buffer.byteLength(head) - Buffer.byteLength(more(rows.length)) - searched.reduce((sum, note) => sum + Buffer.byteLength(note) + 2, 4)
    let shown = 0
    while (shown < rows.length && Buffer.byteLength(rows[shown]!) + 1 <= left) left -= Buffer.byteLength(rows[shown++]!) + 1
    const notes = [...skip + shown < total ? [more(shown)] : [], ...searched]
    return { text: [head, rows.slice(0, shown).join("\n"), ...notes].join("\n\n") }
  }

  /** Reads one file of a grep page and picks the matches its block shows; the caller holds the file's lock. */
  async #grepLoad(ctx: Context, file: string, target: Target | undefined, matches: number[], shape: GrepShape): Promise<GrepFile> {
    const loaded: GrepFile = { name: file, shape, matches, target, lineAt: () => undefined, shown: [], listed: new Set() }
    if (!target) return { ...loaded, failed: `[${file}] vanished before it could be read` }
    const handle = await open(target.canonical, constants.O_RDONLY | constants.O_NONBLOCK)
    try {
      await confined(ctx, handle, target.canonical)
      if ((await handle.stat()).size > TEXT_LIMIT) loaded.untagged = "over 2 MiB"
      else {
        const current = await snapshot(handle, TEXT_LIMIT)
        try {
          loaded.anchored = view(text(current.bytes))
          loaded.lines = loaded.anchored.lines
        } catch {
          loaded.lines = current.bytes.toString("utf8").split("\n").map((line) => line.replace(/\r$/, ""))
        }
      }
    } catch (error) {
      if (!(error instanceof ChangedWhileReading)) return { ...loaded, failed: `[${target.display}] skipped: ${error instanceof Error ? error.message : String(error)}` }
      loaded.untagged = "changing while read"
    } finally { await handle.close() }
    const lines = loaded.lines
    if (lines) loaded.lineAt = (line) => lines[line - 1]
    if (loaded.untagged) {
      const texts = await shape.ripgrep(file)
      loaded.lineAt = (line) => texts.get(line)
    }
    loaded.shown = matches.filter((line) => loaded.lineAt(line) !== undefined).slice(0, shape.limit)
    loaded.listed = new Set(loaded.shown)
    return loaded
  }

  /** One loaded file's block, recording its fully shown rows as seen; past the budget the file is left out, unless `cut` lets it stop at the budget instead, as the first file of a page must. */
  #grepBlock(ctx: Context, file: GrepFile, budget: { left: number }, cut: boolean): { block: string; patch?: string; changes: number; previewed?: { canonical: string; lines: number[] } } | undefined {
    if (file.failed) return { block: file.failed, changes: 0 }
    const { anchored, untagged, lineAt, shown, matches, shape } = file
    const target = file.target!
    const hits = new Set(shown)
    const head = anchored ? `[${target.display}#${anchored.tag}]` : untagged ? `[${target.display}] (not anchorable: ${untagged}; rows from ripgrep)` : `[${target.display}] (not anchorable)`
    // Each listed line with the rows it brings: a gap marker, its own row and, for a replace, the rows it would become.
    const groups: { line: number; rows: string[]; seen?: true; mark?: Marks; change?: { content: string; next: string } }[] = []
    let previous = 0
    for (const line of [...file.listed].sort((a, b) => a - b)) {
      const group: (typeof groups)[number] = { line, rows: previous && line > previous + 1 ? ["..."] : [] }
      const marker = hits.has(line) ? "*" : " "
      const content = lineAt(line)!
      const next = hits.has(line) ? shape.replaced?.get(line) : undefined
      // A line a replace changes shows only as it will be: the >N| rows carry the match too, so the current text would only repeat it.
      if (next && next.text !== content) {
        group.change = { content, next: next.text }
        // An empty line is replaced with a PUT, which needs the line seen, and its row costs a few characters.
        if (!content && !untagged) {
          group.rows.push(`${marker}${line}:`)
          group.seen = true
        }
        const parts = next.text.split("\n")
        for (const part of parts) group.rows.push(replacementRow(line, part, parts.length === 1 ? next.spans : undefined, shape.columns))
        groups.push(group)
        previous = line
        continue
      }
      // A long matched line shows the stretches around its matches when cutting it at the start would hide them.
      const windows = hits.has(line) && visible(content).length > shape.columns ? matchWindows(content, shape.spans?.get(line), shape.columns) : undefined
      // Untagged rows use `N|` so they can never pass for anchors; they are shortened like plain rows.
      if (untagged) {
        const shownText = visible(content)
        group.rows.push(`${marker}${line}|${windows ? `[${windows.label}] ${windows.text}` : shownText.length > shape.columns ? shownText.slice(0, shape.columns) + "..." : shownText}`)
      } else if (windows) {
        group.rows.push(`${marker}[${line} ${windows.label}]|${windows.text}`)
        group.mark = { cut: true }
      } else {
        const row = lineRow(line, content, shape.columns)
        group.rows.push(marker + row.text)
        if (row.anchor) group.seen = true
        else group.mark = row
      }
      groups.push(group)
      previous = line
    }
    const notes = (kept: typeof groups, stop?: number) => [
      ...stop !== undefined ? [`[output limit reached; grep ${target.display}:${stop}- to go on from line ${stop}]`] : [],
      ...matches.length > shown.length ? [`[${matches.length - shown.length} more matches in this file${shape.replaced ? "; they are not in the token, so grep again after applying it" : ""}]`] : [],
      ...untagged ? [`[to read around a match, use read with plain: true and a range, like ${target.display}:${Math.max(1, (shown[0] ?? 1) - 10)}-${(shown[0] ?? 1) + 10}]`] : [],
      ...untagged && kept.some((group) => group.change) ? ["[these replacements are not in the token, since nib cannot hold this file whole]"] : [],
      ...rowNotes(target.display, kept.flatMap((group) => group.mark ? [group.mark] : [])),
    ]
    const bytes = (rows: string[]) => rows.reduce((sum, row) => sum + Buffer.byteLength(row) + 1, 1)
    let kept = groups
    let stop: number | undefined
    if (bytes([head, ...groups.flatMap((group) => group.rows), ...notes(groups)]) > budget.left) {
      if (!cut) return undefined
      // Room for the notes the whole file would need, so the ones the kept rows need fit too.
      let left = budget.left - bytes([head, ...notes(groups, Number.MAX_SAFE_INTEGER)])
      let count = 0
      while (count < groups.length && bytes(groups[count]!.rows) - 1 <= left) left -= bytes(groups[count++]!.rows) - 1
      kept = groups.slice(0, Math.max(1, count))
      stop = groups[kept.length]?.line
    }
    const block = [head, ...kept.flatMap((group) => group.rows), ...notes(kept, stop)].join("\n")
    budget.left -= Buffer.byteLength(block) + 2
    if (anchored) this.store.record(ctx.session, target.canonical, anchored, kept.flatMap((group) => group.seen ? [group.line] : []))
    const changes = anchored ? kept.flatMap((group) => group.change ? [{ line: group.line, ...group.change }] : []) : []
    // Whole-line SUBs replace exactly what the preview showed, even where a row was cut, and need no ~ checks; only an empty line needs a PUT.
    const patch = changes.length ? [`[${target.display}#${anchored!.tag}]`, ...changes.flatMap(({ line, content, next }) => [
      content ? `SUB ${line}: ${content}` : `PUT ${line}.=${line}:`,
      ...next.split("\n").map((part) => `+${part}`),
    ])].join("\n") : undefined
    // A changed line the preview showed whole, as one uncut and uncolored >N| row, is one the agent has seen as it will be.
    const whole = changes.filter(({ next }) => !next.includes("\n") && visible(next) === next && next.length <= shape.columns).map(({ line }) => line)
    return { block, patch, changes: changes.length, ...patch && { previewed: { canonical: target.canonical, lines: whole } } }
  }

  /** Applies a patch; `REAPPLY TOKEN` as the whole input applies the patch stored under that token, once. */
  async patch(ctx: Context, input: string): Promise<Result> {
    const reapply = /^REAPPLY\s+(\S+)$/.exec(input.trim())
    if (!reapply) return this.#apply(ctx, input)
    const key = `${ctx.session}\0${reapply[1]}`
    const stored = this.#tokens.get(key)
    if (stored === undefined) throw new Error(`Reapply token ${reapply[1]} is unknown, already used or expired; send the full patch again`)
    this.#tokens.delete(key)
    const result = await this.#apply(ctx, stored.input, stored.preview)
    // Said here too, since after applying a token a model takes the job as done.
    const left = stored.preview?.left ?? 0
    if (!left) return result
    const one = left === 1
    return { ...result, text: `${result.text}\n\n[${left} more line${one ? "" : "s"} that the search would change ${one ? "was" : "were"} not in this token; grep again with the same pattern and replace for ${one ? "it" : "them"}]` }
  }

  /** Validates every section before writing any, and refuses with all of their problems at once. */
  async #apply(ctx: Context, input: string, preview?: Preview): Promise<Result> {
    const sections = parsePatch(input)
    const failures: { index: number; error: unknown }[] = []
    const order = new Map<Request, number>()
    sections.forEach((section, index) => {
      try {
        if (section.kind === "create") text(Buffer.from(section.content))
        order.set(section.kind === "create"
          ? { kind: "create", filePath: section.path, content: section.content }
          : { kind: "edit", filePath: section.path, tag: section.tag, edits: section.edits, remove: section.remove, move: section.move, copy: section.copy } satisfies EditRequest & Pick<EditSection, "remove" | "move" | "copy">, index)
      } catch (error) { failures.push({ index, error }) }
    })
    const failed = (request: Request, error: unknown) => { failures.push({ index: order.get(request)!, error }) }
    return locked(ctx, [...order.keys()], async (handles) => {
      const staged: Staged[] = []
      for (const handle of handles) {
        try {
          staged.push(handle.request.kind === "create"
            ? await stageCreate(ctx, this.store, handle as Handle<CreateRequest>)
            : await stageEdit(ctx, this.store, handle as Handle<EditRequest>))
        } catch (error) {
          if (ctx.signal.aborted) throw error
          failed(handle.request, error)
        }
      }
      if (failures.length) throw this.#refusal(ctx.session, input, failures, preview)
      return { text: (await commit(ctx, this.store, staged, preview?.shown)).join("\n\n") }
    }, failed)
  }

  /** One error for every refused section, in patch order; when each refusal only displayed unseen lines, a token lets the agent apply the patch unchanged. */
  #refusal(session: string, input: string, failures: { index: number; error: unknown }[], preview?: Preview) {
    const errors = failures.sort((a, b) => a.index - b.index).map((failure) => failure.error)
    const messages = errors.map((error) => error instanceof Error ? error.message : String(error))
    const parts = messages.length > 1 ? [`Nothing was written; ${messages.length} sections have problems.`, ...messages] : messages
    if (errors.every((error) => error instanceof Refusal && error.retry)) {
      parts.push(`To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY ${this.#remember(session, input, preview)}`)
    }
    return new Error(parts.join("\n\n"))
  }

  /** Stores a patch under a new single-use token for `REAPPLY`, dropping the oldest past TOKENS. */
  #remember(session: string, input: string, preview?: Preview) {
    const token = Array.from({ length: 6 }, () => TOKEN_ALPHABET[randomInt(TOKEN_ALPHABET.length)]).join("")
    this.#tokens.set(`${session}\0${token}`, { input, preview })
    for (const key of this.#tokens.keys()) {
      if (this.#tokens.size <= TOKENS) break
      this.#tokens.delete(key)
    }
    return token
  }

  async write(ctx: Context, args: { path: string; content: string; tag?: string }): Promise<Result> {
    text(Buffer.from(args.content))
    const requested = path.resolve(ctx.directory, expandHome(args.path))
    const exists = await lstat(requested).then(() => true, (error: unknown) => {
      if (missing(error)) return false
      throw error
    })
    if (exists && !args.tag) throw new Error(`${args.path} already exists; pass the tag from its latest read to overwrite it, or use edit`)
    if (!exists && args.tag) throw new Error(`${args.path} does not exist, so a tag cannot apply; omit tag to create it`)
    const request: CreateRequest = { kind: "create", filePath: args.path, content: args.content, overwrite: args.tag?.toUpperCase() }
    return locked(ctx, [request], async ([handle]) => {
      const staged = await stageCreate(ctx, this.store, handle as Handle<CreateRequest>)
      return { text: (await commit(ctx, this.store, [staged]))[0]! }
    })
  }
}
