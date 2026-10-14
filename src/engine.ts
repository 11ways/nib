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
const REVEAL_LINES = 40
const REVEAL_CONTEXT = 2
const TRUSTED_CREATE_LINES = 10
const REVEAL_COLUMNS = 512
const GREP_COLUMNS = 512
const GREP_FILES = 20
const GREP_LIST_FILES = 200
const GREP_PER_FILE = 20
const GREP_SINGLE_FILE = 200
const GREP_TIMEOUT = 30_000
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

/** Per-call state; `authorize` decides writes outside the workspace roots and throws to refuse. */
export type Context = {
  directory: string
  session: string
  signal: AbortSignal
  roots?: () => Promise<string[]>
  authorize?: (canonical: string, action: string) => Promise<void>
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

function missing(error: unknown) {
  return error instanceof Error && "code" in error && error.code === "ENOENT"
}

function expandHome(input: string) {
  return input === "~" || input.startsWith("~/") ? path.join(os.homedir(), input.slice(1)) : input
}

type Target = { canonical: string; display: string; check(): Promise<void> }

/** Canonicalizes a path; `create` resolves the parent only, so a final symlink is the target itself rather than followed. */
async function resolveTarget(input: string, ctx: Context, create = false): Promise<Target> {
  ctx.signal.throwIfAborted()
  const requested = path.resolve(ctx.directory, expandHome(input))
  const locate = async () => create
    ? path.join(await realpath(path.dirname(requested)), path.basename(requested))
    : await realpath(requested)
  const canonical = await locate().catch((error: unknown) => {
    if (missing(error)) throw new Error(`${input} does not exist${create ? " (parent directory missing)" : ""}`)
    throw error
  })
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
  const roots = await Promise.all((await ctx.roots?.() ?? [ctx.directory]).map((root) => realpath(root).catch(() => root)))
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
  if (picked.length <= MAP_ROWS) return { summary: `${counts}, ${picked.length} shown`, rows: picked.map((line) => `${line}~${clip(lines[line - 1]!)}`), seen: picked }
  const sketch = outline(display, lines)
  if (sketch.rows.filter((row) => "line" in row).length < OUTLINE_MIN_ROWS || sketch.candidates * 2 > lines.filter((content) => content.trim()).length) {
    return { summary: `${counts}; too long to map, read the ranges you need`, rows: [], seen: [] }
  }
  const rows = sketch.rows.map((row) => "line" in row ? { ...lineRow(row.line, lines[row.line - 1]!, LINE_LIMIT), line: row.line } : { text: row.text, anchor: false })
  return { summary: `${counts}; outline only, read the ranges you need`, rows: rows.map((row) => row.text), seen: rows.flatMap((row) => row.anchor && "line" in row ? [row.line] : []) }
}

/** A refused edit; `retry` means it failed only on lines it has now displayed, so the same patch succeeds when sent again. */
class Refusal extends Error {
  constructor(message: string, readonly retry = false) { super(message) }
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

/** Marks the lines whose ~ check rows match: a check is the start of the line after its indentation, and an empty one matches a blank line. */
function confirmChecks(named: string, display: string, lines: string[], edits: LineEdit[], record?: (shown: number[]) => void) {
  const failures: { line: number; check: string }[] = []
  const confirmed = edits.map((edit) => {
    if (!edit.checks) return edit
    const targets = edit.checks.length === 2 ? [edit.start, edit.end] : [edit.start]
    const matched: number[] = []
    edit.checks.forEach((check, index) => {
      const line = targets[index]!
      if (line > lines.length) throw new Error(`Line ${line} does not exist in ${display} (file has ${lines.length} lines)`)
      const wanted = check.trim()
      const content = lines[line - 1]!
      if (wanted ? content.trimStart().startsWith(wanted) : !content.trim()) matched.push(line)
      else failures.push({ line, check: wanted })
    })
    return { ...edit, confirmed: matched }
  })
  if (!failures.length) return confirmed
  const { rows, shown, marks } = contextRows(lines, failures.map((failure) => failure.line), REVEAL_CONTEXT, REVEAL_COLUMNS)
  record?.(shown)
  throw new Refusal([
    `${named}: ~ check rows do not match, so these line numbers are off:`,
    ...failures.map(({ line, check }) => check ? `line ${line} does not start with ${JSON.stringify(check)}` : `line ${line} is not blank`),
    "",
    `Lines of ${named} around them (* marks them):`,
    ...rows,
    ...rowNotes(display, marks),
  ].join("\n"))
}

/** Turns SUB edits into one replace per line; each old text must occur exactly once in that line, and two on one line must not overlap. */
function resolveSubs(named: string, display: string, lines: string[], edits: LineEdit[], record?: (shown: number[]) => void) {
  if (!edits.some((edit) => edit.sub !== undefined)) return edits
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
  if (!failures.length) return resolved
  const { rows, shown, marks } = contextRows(lines, [...done].filter((line) => failures.some((failure) => failure.line === line)), REVEAL_CONTEXT, REVEAL_COLUMNS)
  record?.(shown)
  throw new Refusal([
    `${named}: SUB refused:`,
    ...failures.map((failure) => failure.message),
    "",
    `Lines of ${named} around them (* marks them):`,
    ...rows,
    ...rowNotes(display, marks),
  ].join("\n"))
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
  // An unterminated original last line needs a separator when inserting after it.
  const body = rows.map((row, index) => index < rows.length - 1 && !row.raw.endsWith("\n") ? row.raw + source.ending : row.raw).join("")
  const final = body.replace(/\r?\n$/, "") + (body !== "" ? source.body.match(/\r?\n$/)?.[0] ?? "" : "")
  return { rows, runs, bytes: Buffer.from(source.bom + final) }
}

/**
 * Numbered rows around each change, then at most EDIT_MAP_ROWS row-map rows in file order: in the elided middle of long inserted runs, and over
 * moved lines the agent knew, since a row for a line it never saw shows where it is but is no anchor. Known moved lines past that window get
 * one note per stretch with their old numbers. Rows past `limit` are cut; a long replaced line shows the stretch where it changed instead.
 */
function preview(rows: Row[], runs: { at: number; length: number }[], prior: Version | undefined, limit: number) {
  const indexes = new Set<number>()
  const add = (from: number, to: number) => { for (let index = Math.max(0, from); index < Math.min(rows.length, to); index++) indexes.add(index) }
  for (const run of runs) {
    add(run.at - PREVIEW_CONTEXT, run.at)
    if (run.length <= PREVIEW_CONTEXT * 2 + 1) add(run.at, run.at + run.length)
    else {
      add(run.at, run.at + PREVIEW_CONTEXT)
      add(run.at + run.length - PREVIEW_CONTEXT, run.at + run.length)
    }
    add(run.at + run.length, run.at + run.length + PREVIEW_CONTEXT)
  }
  let contents: string[] | undefined
  const lines = () => contents ??= rows.map((row) => lineContent(row.raw))
  // Kept lines whose number changed, in stretches that moved by one offset; `start` skips the rows already shown around the change.
  const moved: { start: number; to: number; offset: number; picked: number[] }[] = []
  rows.forEach((row, index) => {
    if (row.origin === undefined || row.origin === index) return
    const last = moved.at(-1)
    if (last && last.to === index && last.offset === index - row.origin) last.to = index + 1
    else moved.push({ start: index + 1, to: index + 1, offset: index - row.origin, picked: [] })
  })
  const candidates: number[] = []
  for (const run of runs) {
    if (run.length > PREVIEW_CONTEXT * 2 + 1 && !rows[run.at]!.opaque) {
      candidates.push(...mapLines(lines(), run.at + PREVIEW_CONTEXT + 1, run.at + run.length - PREVIEW_CONTEXT, { after: run.at + PREVIEW_CONTEXT }))
    }
  }
  const knew = (line: number) => {
    const origin = rows[line - 1]!.origin
    return origin !== undefined && !!prior?.seen && knownLine(prior, origin + 1)
  }
  for (const stretch of moved) {
    while (stretch.start <= stretch.to && indexes.has(stretch.start - 1)) stretch.start++
    let end = stretch.to
    while (end >= stretch.start && indexes.has(end - 1)) end--
    // Each run of known lines is mapped on its own; one that follows unknown lines also shows its first line as a landmark.
    for (let from = stretch.start; from <= end;) {
      if (!knew(from)) {
        from++
        continue
      }
      let to = from
      while (to < end && knew(to + 1)) to++
      stretch.picked.push(...mapLines(lines(), from, to, { first: from !== stretch.start, last: true, after: from - 1 }))
      from = to + 1
    }
    candidates.push(...stretch.picked)
  }
  const mapped = new Set(candidates.sort((a, b) => a - b).slice(0, EDIT_MAP_ROWS).map((line) => line - 1))
  const notes = new Map<number, string>()
  for (const { start, to, offset, picked } of moved) {
    if (picked.every((line) => mapped.has(line - 1))) continue
    const from = Math.max(start, ...picked.filter((line) => mapped.has(line - 1)).map((line) => line + 1))
    const span = (low: number, high: number) => low === high ? `${low}` : `${low}-${high}`
    notes.set(from - 1, `[line${from === to ? "" : "s"} ${span(from, to)} ${from === to ? "is" : "are"} old line${from === to ? "" : "s"} ${span(from - offset, to - offset)}, moved ${offset > 0 ? "down" : "up"} by ${Math.abs(offset)}]`)
  }
  const output: string[] = []
  const shown: number[] = []
  const marks: Marks[] = []
  let previous = -1
  let previousFull = false
  for (const index of [...new Set([...indexes, ...mapped, ...notes.keys()])].sort((a, b) => a - b)) {
    const full = indexes.has(index)
    const note = notes.get(index)
    // Map rows and notes carry their own numbers, so only a gap between two full rows gets an ellipsis.
    if (note) output.push(note)
    else if (full && previousFull && index > previous + 1) output.push("...")
    const row = rows[index]!
    if (full) {
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
      if (row.origin === undefined ? !row.opaque : knew(index + 1)) shown.push(index + 1)
    }
    previous = index
    previousFull = full
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
  | { kind: "noop"; handle: Handle<EditRequest>; live: View }
  | { kind: "remove"; handle: Handle<EditRequest>; current: Awaited<ReturnType<typeof snapshot>> }
  | { kind: "create"; handle: Handle<CreateRequest>; bytes: Buffer; created: View; replace?: Awaited<ReturnType<typeof snapshot>> }

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
  // Authorizes the requested path before creating parents for it, then the canonical path that symlinks may redirect to.
  const prepare = async (filePath: string, action: string, create: boolean) => {
    const requested = path.resolve(ctx.directory, expandHome(filePath))
    await authorize(requested, action)
    if (create) await mkdir(path.dirname(requested), { recursive: true })
    const target = await resolveTarget(filePath, ctx, create)
    if (target.canonical !== requested) await authorize(target.canonical, action)
    return target
  }
  for (const request of requests) {
    try {
      const create = request.kind === "create" && !request.overwrite
      const action = request.kind === "create" ? (request.overwrite ? "overwrite" : "create") : request.remove ? "delete" : request.move ? "move" : request.copy ? "copy" : "edit"
      const handle = { request, target: await prepare(request.filePath, action, create) } as Handle
      const to = request.kind === "edit" ? request.move ?? request.copy : undefined
      if (to) {
        const destination: CreateRequest = { kind: "create", filePath: to, content: "", destination: true }
        handle.destination = { request: destination, target: await prepare(to, "create", true) } as Handle<CreateRequest>
      }
      claim(handle, ...handle.destination ? [handle.destination] : [])
    } catch (error) {
      if (error instanceof Denied) throw error.cause
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
  const prior = versions.find((version) => version.key === live.key)
  const finish = (edits: LineEdit[], warnings: string[]): Staged => {
    const applied = applyChanges(source, edits.map((edit) => ({ ...edit, raw: edit.content.map((line) => line + source.ending) })))
    if (applied.bytes.equals(current.bytes) && !request.move && !request.copy) return { kind: "noop", handle, live }
    return { kind: "edit", handle, current, source, live, prior, applied, result: text(applied.bytes), warnings }
  }
  // A tagless section only appends, which depends on nothing but where the file ends.
  if (request.tag === undefined) return finish(resolveEdges(request.edits, live.lines.length), [])
  const tagged = versions.filter((version) => version.tag === request.tag)
  const matches = tagMatches(versions, live, request.tag)
  if (!matches && !tagged.length) {
    throw mismatchError(store, ctx.session, target, live, request.tag, false, request.edits.flatMap(anchorLines), request.edits.every((edit) => edit.edge))
  }
  if (request.remove) {
    if (!matches) throw mismatchError(store, ctx.session, target, live, request.tag, true, [])
    return { kind: "remove", handle, current }
  }
  // Line numbers, checks and SUB texts all refer to the tagged version; only the live one may record what a refusal displays.
  const basis: Version = matches ? prior ?? live : tagged[0]!
  const named = `[${request.filePath}#${request.tag}]`
  const record = matches ? (shown: number[]) => { store.record(ctx.session, target.canonical, live, shown) } : undefined
  let requested = confirmChecks(named, target.display, basis.lines, request.edits, record)
  requested = resolveSubs(named, target.display, basis.lines, requested, record)
    .map((edit) => edit.quoted && !knownLine(basis, edit.start) ? { ...edit, opaque: true as const } : edit)
  requested = resolveBlocks(requested, basis.lines)
  try { requested = orderEdits(requested) } catch (error) {
    throw new Error(`${named}: ${error instanceof Error ? error.message : String(error)}`)
  }
  const anchors = requested.flatMap(anchorLines)
  if (matches) {
    const edits = resolveEdges(requested, live.lines.length)
    const end = Math.max(0, ...anchors)
    if (end > live.lines.length) throw new Error(`Line ${end} does not exist in ${target.display} (file has ${live.lines.length} lines)`)
    const unseen = prior ? unverified(prior, requested) : []
    if (unseen.length) {
      const rescued = rescueShifted(versions, live, edits, new Set(unseen))
      const remaining = unseen.filter((line) => !rescued.has(line))
      if (remaining.length) throw unseenError(store, ctx.session, target, live, remaining, prior?.known)
    }
    return finish(edits, [])
  }
  // Like oh-my-pi, edits without line anchors only depend on where the file ends, so any tag drift is tolerable.
  const remapped = !anchors.length && !request.move && !request.copy
    ? { offset: 0, edits: resolveEdges(requested, live.lines.length) }
    : anchors.length && !unverified(basis, requested).length ? remapEdits(basis, live.lines, requested) : undefined
  if (!remapped) throw mismatchError(store, ctx.session, target, live, request.tag, true, anchors)
  return finish(remapped.edits, [!anchors.length ? TAIL_DRIFT : remapped.offset !== 0 ? RECOVERY_REMAPPED : basis === versions[0] ? RECOVERY_EXTERNAL : RECOVERY_CHAIN])
}

// Publication still links exclusively; this early check only keeps a multi-file patch from failing halfway.
async function stageCreate(ctx: Context, store: SnapshotStore, handle: Handle<CreateRequest>): Promise<Staged> {
  const bytes = Buffer.from(handle.request.content)
  const created = view(text(bytes))
  if (handle.request.overwrite) {
    const current = await snapshot(handle.file!, TEXT_LIMIT)
    if (current.info.nlink > 1n) throw new Error("Hardlinked targets are not supported; refusing to break links")
    const live = view(text(current.bytes))
    const versions = store.versions(ctx.session, handle.target.canonical)
    if (!tagMatches(versions, live, handle.request.overwrite)) {
      throw new Error(`${handle.target.display} changed since tag #${handle.request.overwrite}; the current file is [${handle.target.display}#${live.tag}]. Read it again before overwriting.`)
    }
    return { kind: "create", handle, bytes, created, replace: current }
  }
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

/** Publishes every staged change in order; a failure after the first write names what already landed. */
async function commit(ctx: Context, store: SnapshotStore, staged: Staged[]) {
  const changes = staged.filter((item) => item.kind !== "noop")
  for (const item of changes) {
    await item.handle.parent.check()
    if (item.handle.file) await checkDescriptor(item.handle.file, item.handle.parent.location, item.handle.target.canonical)
    if (item.kind === "edit" && item.handle.destination) await destinationFree(item.handle.destination)
  }
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
  return staged.map((item) => {
    const { canonical, display } = item.handle.target
    if (item.kind === "noop") return `[${display}#${item.live.tag}]\nNo changes: the edit reproduces the current content; this tag is still current.`
    if (item.kind === "remove") return `Deleted ${display}`
    if (item.kind === "create") {
      // The model wrote every line but saw no numbers: the row map shows some, a small file is trusted whole, and the rest is known content that a ~ check can anchor.
      const lines = item.created.lines
      const map = writtenMap(display, item.created)
      store.record(ctx.session, canonical, item.created, lines.length < TRUSTED_CREATE_LINES ? lines.map((_, index) => index + 1) : map.seen, lines.map((_, index) => index + 1))
      return [`${item.replace ? "Overwrote" : "Created"} [${display}#${item.created.tag}] (${map.summary})`, ...map.rows].join("\n")
    }
    // Like oh-my-pi, only lines that kept both number and content stay anchorable, plus the rows shown below.
    // Lines the agent wrote, or saw before they moved, stay known content that a ~ check can anchor.
    const destination = item.handle.destination?.target
    const next = view(item.result)
    // Edits to a minified file cut long rows like a read of it does.
    const limit = minifiedReason(destination?.canonical ?? canonical, Buffer.byteLength(next.key), next.lines) ? MINIFIED_COLUMNS : LINE_LIMIT
    const rows = preview(item.applied.rows, item.applied.runs, item.prior, limit)
    const prior = item.prior
    const seen = item.applied.rows.flatMap((row, index) => row.origin === index && (!prior?.seen || prior.seen.has(index + 1)) ? [index + 1] : [])
    const known = item.applied.rows.flatMap((row, index) => (row.origin === undefined ? !row.opaque : !!prior?.seen && knownLine(prior, row.origin + 1)) ? [index + 1] : [])
    store.record(ctx.session, destination?.canonical ?? canonical, next, [...seen, ...rows.shown], known)
    const output = [`${destination ? `${item.handle.request.copy ? "Copied" : "Moved"} ${display} to ` : ""}[${destination?.display ?? display}#${next.tag}]`]
    if (item.applied.runs.length) output.push(...item.applied.rows.length ? rows.output : ["(file is now empty)"])
    output.push(...rowNotes(destination?.display ?? display, rows.marks))
    if (item.warnings.length) output.push("", "Warnings:", ...item.warnings)
    return output.join("\n")
  })
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
async function selectedLines(file: string, selections: Selection[], signal: AbortSignal) {
  let length = 0
  if (selections.some((selection) => selection.tail)) {
    const handle = await open(file, constants.O_RDONLY | constants.O_NONBLOCK)
    try { length = (await scanLines(handle, Number((await handle.stat()).size), signal, () => false, 0)).total } finally { await handle.close() }
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

/** Marks lines that continue a multi-line string (Python triple quotes, JS template literals) so their text is not taken for code. */
function insideStrings(lines: string[]) {
  let open: string | undefined
  return lines.map((content) => {
    const inside = open !== undefined
    for (const match of content.replace(/\\./g, "").matchAll(/"""|'''|`/g)) {
      if (open === undefined) open = match[0]
      else if (open === match[0]) open = undefined
    }
    return inside
  })
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

/** Top-level code lines plus the members of top-level container types, by indentation; markdown uses headings. */
function outline(display: string, lines: string[]): { rows: OutlineRow[]; candidates: number } {
  if (/\.(md|mdx|markdown)$/i.test(display)) {
    const rows: OutlineRow[] = []
    let fenced = false
    lines.forEach((content, index) => {
      if (/^\s*(```|~~~)/.test(content)) fenced = !fenced
      else if (!fenced && /^#{1,6}\s/.test(content)) rows.push({ line: index + 1 })
    })
    return { rows, candidates: rows.length }
  }
  const indent = (content: string) => content.length - content.trimStart().length
  const quoted = insideStrings(lines)
  const code = (content: string, index: number) => content.trim() !== "" && !quoted[index] && !COMMENT.test(content)
  const unit = Math.min(...lines.filter((content, index) => code(content, index) && indent(content) > 0).map(indent), Number.POSITIVE_INFINITY)
  const depth = (content: string) => Number.isFinite(unit) ? Math.floor(indent(content) / unit) : 0
  const candidates: { line: number; depth: number }[] = []
  let container = false
  lines.forEach((content, index) => {
    if (!code(content, index) || CLOSER.test(content) || /^[)\]}]/.test(content.trimStart())) return
    const level = depth(content)
    if (level === 0) {
      container = CONTAINER.test(content)
      candidates.push({ line: index + 1, depth: 0 })
    } else if (level === 1 && container) candidates.push({ line: index + 1, depth: 1 })
  })
  // Over the cap, top-level rows stay and members fill the remaining room in file order.
  let room = OUTLINE_ROWS - candidates.filter((row) => row.depth === 0).length
  const kept = candidates.length > OUTLINE_ROWS ? candidates.filter((row) => row.depth === 0 || room-- > 0) : candidates
  const rows: OutlineRow[] = []
  let index = 0
  while (index < kept.length && rows.length < OUTLINE_ROWS) {
    const first = kept[index]!
    let last = index
    if (first.depth === 0 && IMPORT.test(lines[first.line - 1]!)) {
      while (last + 1 < kept.length && kept[last + 1]!.depth === 0 && IMPORT.test(lines[kept[last + 1]!.line - 1]!)) last++
    }
    rows.push({ line: first.line })
    if (last > index) rows.push({ text: `[lines ${first.line + 1}-${kept[last]!.line}: ${last - index} more import lines]` })
    index = last + 1
  }
  const omitted = candidates.length - index
  if (omitted > 0) rows.push({ text: `[outline capped at ${OUTLINE_ROWS} rows; ${omitted} more members or declarations not shown]` })
  return { rows, candidates: candidates.length }
}

/** True when `location` lies in a git work tree (a `.git` directory or file in it or an ancestor). */
async function insideGit(location: string) {
  for (let directory = location; ; directory = path.dirname(directory)) {
    if (await lstat(path.join(directory, ".git")).then(() => true, () => false)) return true
    if (path.dirname(directory) === directory) return false
  }
}

/**
 * Runs ripgrep with `args` over `roots`, once for the roots inside git and once for the rest: inside a repository ripgrep follows git,
 * which stops a parent repository's .gitignore at a nested one, and --no-require-git would let it hide that nested repository.
 */
async function ripgrep(ctx: Context, roots: string[], args: string[], signal: AbortSignal) {
  const run = async (group: string[], flags: string[]) => {
    const child = Bun.spawn(["rg", ...args, ...flags, "--", ...group], { cwd: ctx.directory, stdout: "pipe", stderr: "pipe", signal })
    const [stdout, stderr, exit] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
    if (exit === 2 && !stdout) throw new Error(`rg failed: ${stderr.trim()}`)
    return { stdout, stderr }
  }
  const tracked = await Promise.all(roots.map(insideGit))
  const runs = await Promise.all([
    roots.filter((_, index) => tracked[index]),
    roots.filter((_, index) => !tracked[index]),
  ].map((group, index) => group.length ? run(group, index ? ["--no-require-git"] : []) : undefined))
  return { stdout: runs.map((item) => item?.stdout ?? "").join(""), stderr: runs.map((item) => item?.stderr ?? "").join("") }
}

type Submatch = Span & { replacement?: { text?: string; bytes?: string } }
/** A matched line as a grep replace would leave it, and where its first replacements sit in it. */
type Replaced = { text: string; spans: Span[] }
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

/** Swaps a line's submatches for ripgrep's replacements; ripgrep reports UTF-8 byte offsets. */
function replaceLine(text: string, submatches: Submatch[]): Replaced {
  const bytes = Buffer.from(text.replace(/\r?\n$/, ""))
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
  return { text: composed + bytes.subarray(cursor).toString(), spans: spans.slice(0, WINDOWS) }
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
  /** Refused patches that a straight retry would apply, by session and reapply token, oldest first. */
  readonly #tokens = new Map<string, string>()

  async read(ctx: Context, args: { paths: string[]; maxLineLength?: number; plain?: boolean; collapse?: boolean; info?: boolean }): Promise<Result> {
    if (args.info && (args.plain || args.collapse !== undefined || args.maxLineLength !== undefined)) {
      throw new Error("info: true gives facts instead of content, so it takes no plain, collapse or maxLineLength")
    }
    const maxLineLength = args.maxLineLength ?? LINE_LIMIT
    const blocks: Block[] = []
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
      const target = await resolveTarget(input, ctx)
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
    return { text: parts.join("\n\n"), images }
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
        if (whole) return notes
        const open = !selection.tail && (selection.ranges ?? [{ start: 1 }]).every((range) => range.end === undefined)
        return [`[${total} lines total${open && last < total ? `; continue with ${target.display}:${last + 1}-` : ""}]`, ...notes]
      },
    }
  }

  async grep(ctx: Context, args: { pattern: string; paths?: string[]; glob?: string | string[]; ignoreCase?: boolean; literal?: boolean; before?: number; after?: number; skip?: number; mode?: "matches" | "files" | "count"; noIgnore?: boolean; maxLineLength?: number; replace?: string }): Promise<Result> {
    const multiline = args.pattern.includes("\n") || args.pattern.includes("\\n")
    if (args.replace !== undefined && (args.mode ?? "matches") !== "matches") throw new Error("replace previews its changes as rows, so it needs mode matches")
    if (args.replace !== undefined && multiline) throw new Error("replace works on matches within one line; change multi-line text with edit")
    // A line selector narrows its file's matches; the same file named again without one searches all of it.
    const selections = new Map<string, Selection[] | undefined>()
    for (const entry of args.paths?.length ? args.paths : ["."]) {
      const { file, selection } = await splitSelector(entry, ctx)
      const root = path.resolve(ctx.directory, expandHome(file))
      const chosen = selection.ranges || selection.tail ? selection : undefined
      if (chosen && (await stat(root).catch(() => undefined))?.isDirectory()) throw new Error(`${entry}: line selectors need a file, not a directory`)
      const earlier = selections.get(root)
      selections.set(root, !chosen || (selections.has(root) && !earlier) ? undefined : [...earlier ?? [], chosen])
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
    const { stdout, stderr } = await ripgrep(ctx, roots, command, signal)
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
    const inside = new Map<string, (line: number) => boolean>()
    for (const [file, lines] of files) {
      const chosen = selections.get(file)
      if (!chosen) continue
      const inRange = await selectedLines(file, chosen, ctx.signal)
      inside.set(file, inRange)
      for (const line of lines) if (!inRange(line)) lines.delete(line)
      if (!lines.size) files.delete(file)
    }
    if (!files.size) return { text: stderr.trim() ? `No matches found (rg: ${stderr.trim().split("\n")[0]})` : "No matches found" }
    const names = [...files.keys()].sort()
    const skip = args.skip ?? 0
    const mode = args.mode ?? "matches"
    if (mode !== "matches") {
      const page = names.slice(skip, skip + GREP_LIST_FILES)
      if (!page.length) return { text: `No more results (${names.length} matching files; skip ${skip} is past the end)` }
      const total = names.reduce((sum, name) => sum + files.get(name)!.size, 0)
      const display = (name: string) => within(ctx.directory, name) ? path.relative(ctx.directory, name) : name
      const rows = page.map((name) => mode === "files" ? display(name) : `${files.get(name)!.size} ${display(name)}`)
      const notes = skip + page.length < names.length ? [`[${names.length} matching files; continue with skip ${skip + page.length}]`] : []
      return { text: [`${total} matches in ${names.length} files`, rows.join("\n"), ...notes].join("\n\n") }
    }
    const page = names.slice(skip, skip + GREP_FILES)
    if (!page.length) return { text: `No more results (${names.length} matching files; skip ${skip} is past the end)` }
    const replacing = args.replace !== undefined
    const shape = {
      limit: names.length === 1 ? GREP_SINGLE_FILE : GREP_PER_FILE,
      before: args.before ?? (replacing ? 0 : 1),
      after: args.after ?? (replacing ? 0 : 3),
      columns: args.maxLineLength ?? GREP_COLUMNS,
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
    const budget = { left: OUTPUT_LIMIT }
    const parts: string[] = []
    const patches: string[] = []
    let changes = 0
    let total = 0
    for (const name of page) total += files.get(name)!.size
    for (const name of page) {
      const matches = [...files.get(name)!].sort((a, b) => a - b)
      const result = await this.#grepFile(ctx, name, matches, { ...shape, inside: inside.get(name), spans: spans.get(name), replaced: replaced.get(name), ripgrep: fallback }, budget, !parts.length)
      if (!result) break
      parts.push(result.block)
      if (result.patch) patches.push(result.patch)
      changes += result.changes
    }
    const notes: string[] = []
    if (parts.length < page.length) notes.push(`[output limit reached after ${parts.length} files; narrow the search or continue with skip ${skip + parts.length}]`)
    else if (skip + page.length < names.length) notes.push(`[${names.length} matching files; continue with skip ${skip + page.length}]`)
    if (patches.length) notes.push(`To apply the ${changes} replaced line${changes === 1 ? "" : "s"} shown (>N| rows), call edit with input: REAPPLY ${this.#remember(ctx.session, patches.join("\n"))}`)
    else if (replacing) notes.push("[no replacement to apply: the shown matches stay the same or are in files nib cannot edit]")
    return { text: [`${total} matches in ${page.length} files${names.length > page.length ? ` (of ${names.length})` : ""}`, ...parts, ...notes].join("\n\n") }
  }

  /** Lists the files under `paths` that grep would search, with line counts for text nib can hold, sizes and ages; filters and sorts by size or age. */
  async find(ctx: Context, args: { paths?: string[]; glob?: string | string[]; noIgnore?: boolean; minSize?: string; newer?: string; sort?: "path" | "size" | "modified"; skip?: number }): Promise<Result> {
    const minSize = args.minSize === undefined ? undefined : parseSize(args.minSize)
    const newer = args.newer === undefined ? undefined : parseAge(args.newer)
    const sort = args.sort ?? "path"
    const roots: string[] = []
    for (const entry of args.paths?.length ? args.paths : ["."]) {
      const root = path.resolve(ctx.directory, expandHome(entry))
      if (!await stat(root).then(() => true, () => false)) throw new Error(`${entry} does not exist`)
      roots.push(root)
    }
    const command = ["--files", "--null", "--no-config", "--hidden", "--glob", "!.git"]
    if (args.noIgnore) command.push("--no-ignore")
    for (const glob of [args.glob ?? []].flat()) command.push("--glob", glob)
    const signal = AbortSignal.any([ctx.signal, AbortSignal.timeout(GREP_TIMEOUT)])
    const { stdout, stderr } = await ripgrep(ctx, roots, command, signal)
    const names = [...new Set(stdout.split("\0").filter(Boolean))].sort()
    if (!names.length) return { text: stderr.trim() ? `No files found (rg: ${stderr.trim().split("\n")[0]})` : "No files found" }
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
    let left = OUTPUT_LIMIT - Buffer.byteLength(head) - Buffer.byteLength(more(rows.length)) - 4
    let shown = 0
    while (shown < rows.length && Buffer.byteLength(rows[shown]!) + 1 <= left) left -= Buffer.byteLength(rows[shown++]!) + 1
    const notes = skip + shown < total ? [more(shown)] : []
    return { text: [head, rows.slice(0, shown).join("\n"), ...notes].join("\n\n") }
  }

  /** One file's rows; past the budget the file is left out, unless `cut` lets it stop at the budget instead, as the first file of a page must. */
  async #grepFile(ctx: Context, file: string, matches: number[], shape: GrepShape, budget: { left: number }, cut: boolean): Promise<{ block: string; patch?: string; changes: number } | undefined> {
    const target = await resolveTarget(file, ctx).catch(() => undefined)
    if (!target) return { block: `[${file}] vanished before it could be read`, changes: 0 }
    return serialized(target.canonical, ctx, async () => {
      let anchored: View | undefined
      let lineAt: (line: number) => string | undefined = () => undefined
      // Why the rows carry no tag, when nib could not hold the whole file.
      let untagged: string | undefined
      const handle = await open(target.canonical, constants.O_RDONLY | constants.O_NONBLOCK)
      try {
        if ((await handle.stat()).size > TEXT_LIMIT) untagged = "over 2 MiB"
        else {
          const current = await snapshot(handle, TEXT_LIMIT)
          let raw: string[]
          try {
            anchored = view(text(current.bytes))
            raw = anchored.lines
          } catch {
            raw = current.bytes.toString("utf8").split("\n").map((line) => line.replace(/\r$/, ""))
          }
          lineAt = (line) => raw[line - 1]
        }
      } catch (error) {
        if (!(error instanceof ChangedWhileReading)) return { block: `[${target.display}] skipped: ${error instanceof Error ? error.message : String(error)}`, changes: 0 }
        untagged = "changing while read"
      } finally { await handle.close() }
      if (untagged) {
        const texts = await shape.ripgrep(file)
        lineAt = (line) => texts.get(line)
      }
      const shown = matches.filter((line) => lineAt(line) !== undefined).slice(0, shape.limit)
      const listed = new Set<number>()
      for (const line of shown) {
        for (let index = Math.max(1, line - shape.before); index <= line + shape.after; index++) {
          if (lineAt(index) !== undefined && (!shape.inside || shape.inside(index))) listed.add(index)
        }
      }
      const hits = new Set(shown)
      const head = anchored ? `[${target.display}#${anchored.tag}]` : untagged ? `[${target.display}] (not anchorable: ${untagged}; rows from ripgrep)` : `[${target.display}] (not anchorable)`
      // Each listed line with the rows it brings: a gap marker, its own row and, for a replace, the rows it would become.
      const groups: { line: number; rows: string[]; seen?: true; mark?: Marks; change?: { content: string; next: string } }[] = []
      let previous = 0
      for (const line of [...listed].sort((a, b) => a - b)) {
        const group: (typeof groups)[number] = { line, rows: previous && line > previous + 1 ? ["..."] : [] }
        const marker = hits.has(line) ? "*" : " "
        const content = lineAt(line)!
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
        const next = hits.has(line) ? shape.replaced?.get(line) : undefined
        if (next && next.text !== content) {
          group.change = { content, next: next.text }
          const parts = next.text.split("\n")
          for (const part of parts) group.rows.push(replacementRow(line, part, parts.length === 1 ? next.spans : undefined, shape.columns))
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
      return { block, patch, changes: changes.length }
    })
  }

  /** Applies a patch; `REAPPLY TOKEN` as the whole input applies the patch a refusal stored under that token, once. */
  async patch(ctx: Context, input: string): Promise<Result> {
    const reapply = /^REAPPLY\s+(\S+)$/.exec(input.trim())
    if (!reapply) return this.#apply(ctx, input)
    const key = `${ctx.session}\0${reapply[1]}`
    const stored = this.#tokens.get(key)
    if (stored === undefined) throw new Error(`Reapply token ${reapply[1]} is unknown, already used or expired; send the full patch again`)
    this.#tokens.delete(key)
    return this.#apply(ctx, stored)
  }

  /** Validates every section before writing any, and refuses with all of their problems at once. */
  async #apply(ctx: Context, input: string): Promise<Result> {
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
      if (failures.length) throw this.#refusal(ctx.session, input, failures)
      return { text: (await commit(ctx, this.store, staged)).join("\n\n") }
    }, failed)
  }

  /** One error for every refused section, in patch order; when each refusal only displayed unseen lines, a token lets the agent apply the patch unchanged. */
  #refusal(session: string, input: string, failures: { index: number; error: unknown }[]) {
    const errors = failures.sort((a, b) => a.index - b.index).map((failure) => failure.error)
    const messages = errors.map((error) => error instanceof Error ? error.message : String(error))
    const parts = messages.length > 1 ? [`Nothing was written; ${messages.length} sections have problems.`, ...messages] : messages
    if (errors.every((error) => error instanceof Refusal && error.retry)) {
      parts.push(`To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY ${this.#remember(session, input)}`)
    }
    return new Error(parts.join("\n\n"))
  }

  /** Stores a patch under a new single-use token for `REAPPLY`, dropping the oldest past TOKENS. */
  #remember(session: string, input: string) {
    const token = Array.from({ length: 6 }, () => TOKEN_ALPHABET[randomInt(TOKEN_ALPHABET.length)]).join("")
    this.#tokens.set(`${session}\0${token}`, input)
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
