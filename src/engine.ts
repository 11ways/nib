import { randomUUID } from "node:crypto"
import { constants, type BigIntStats } from "node:fs"
import { lstat, link, mkdir, open, readdir, readlink, realpath, rename, stat, unlink } from "node:fs/promises"
import type { FileHandle } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import { anchorLines, fileTag, remapEdits, rescueShifted, resolveEdges, SnapshotStore, type LineEdit, type Version, type View } from "./snapshots"
import { parsePatch, type EditSection } from "./patch"

export const TEXT_LIMIT = 2 * 1024 * 1024
export const MEDIA_LIMIT = 5 * 1024 * 1024
/** Shared byte budget of one read or grep result; Claude Code warns above 10k tokens and truncates at 25k. */
export const OUTPUT_LIMIT = 40 * 1024
export const LINE_LIMIT = 2000
export const READ_LINES = 2000
const PREVIEW_CONTEXT = 2
const REVEAL_LINES = 40
const REVEAL_COLUMNS = 512
const GREP_COLUMNS = 512
const GREP_FILES = 20
const GREP_PER_FILE = 20
const GREP_SINGLE_FILE = 200
const GREP_TIMEOUT = 30_000

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
    throw new Error("File changed while reading; read again")
  }
  return { bytes: buffer.subarray(0, length), info: after }
}

type Text = { decoded: string; bom: string; body: string; lines: string[]; ending: string }

function text(bytes: Buffer): Text {
  if (bytes.length > TEXT_LIMIT) throw new Error(`Text exceeds ${TEXT_LIMIT} byte size limit`)
  if (mime(bytes)) throw new Error("Binary attachments cannot be edited as text")
  if (bytes.some((byte) => byte < 9 || (byte > 13 && byte < 32) || byte === 127)) throw new Error("Binary files are not supported")
  const decoded = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes)
  const bom = decoded.startsWith("﻿") ? "﻿" : ""
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

/** A row that cannot be anchored; the prefix is deliberately not `N:` so it is never mistaken for an editable row. */
function truncatedRow(display: string, line: number, content: string, limit: number) {
  return `[${line} truncated at ${limit} of ${content.length} chars; read ${display}:${line} with maxLineLength before editing]|${content.slice(0, limit)}`
}

// Mirrors oh-my-pi's unseen-line guard: reveal what the model would have touched and let a straight retry pass.
function unseenError(store: SnapshotStore, session: string, target: Target, current: View, unseen: number[]) {
  const shown = unseen.slice(0, REVEAL_LINES).map((line) => ({ line, text: current.lines[line - 1] ?? "" }))
  const truncated = unseen.length > shown.length || shown.some((item) => item.text.length > REVEAL_COLUMNS)
  if (!truncated) store.record(session, target.canonical, current, unseen)
  const header = `This edit anchors to lines ${ranges(unseen)} of ${target.display} that [${target.display}#${current.tag}] never displayed in full (partial read, truncated line, or rows elided from an edit response).`
  const preview = shown.map((item) => `  ${item.line}:${item.text.length > REVEAL_COLUMNS ? `${item.text.slice(0, REVEAL_COLUMNS)}...` : item.text}`).join("\n")
  return new Error(truncated
    ? `${header} Preview of the first ${shown.length} unseen line(s):\n${preview}\nRead the remaining lines in full before re-issuing the edit.`
    : `${header} Actual file content at those lines:\n${preview}\nIf this is what you intend to touch, re-issue the same edit with tag ${current.tag}; a straight retry now succeeds. Otherwise fix your line numbers.`)
}

function mismatchError(store: SnapshotStore, session: string, target: Target, current: View, tag: string, recognized: boolean, anchors: number[]) {
  const display = new Set<number>()
  for (const anchor of anchors) {
    for (let line = Math.max(1, anchor - 2); line <= Math.min(current.lines.length, anchor + 2); line++) display.add(line)
  }
  const rows: string[] = []
  let previous = 0
  for (const line of [...display].sort((a, b) => a - b)) {
    if (previous && line > previous + 1) rows.push("...")
    const content = current.lines[line - 1]!
    rows.push(`${anchors.includes(line) ? "*" : " "}${line}:${content.length > LINE_LIMIT ? `${content.slice(0, LINE_LIMIT)}...` : content}`)
    previous = line
  }
  store.record(session, target.canonical, current, [...display].filter((line) => current.lines[line - 1]!.length <= LINE_LIMIT))
  const lines = recognized
    ? [`Edit rejected for ${target.display}: file changed between read and edit and the edit could not be rebased safely.`,
      `Tag #${tag} is stale; the current file is [${target.display}#${current.tag}]. If a prior edit in this session modified this file, use the tag and numbering from that edit's response; otherwise read the file again.`]
    : [`Edit rejected for ${target.display}: tag #${tag} was not issued for this file in this session.`,
      ...store.origins(session, tag, target.canonical).map((origin) => `Tag #${tag} was issued for ${origin}.`),
      `The current file is [${target.display}#${current.tag}]. Read the file to get a current tag; never invent tags.`]
  return new Error([...lines, ...rows.length ? ["", "Current content around the requested lines (* marks them):", ...rows] : []].join("\n"))
}

type Row = { raw: string; origin?: number }

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
    for (const raw of change.raw) rows.push({ raw })
    cursor = at + (change.operation === "replace" ? change.end - change.start + 1 : 0)
  }
  keep(cursor, source.lines.length)
  // An unterminated original last line needs a separator when inserting after it.
  const body = rows.map((row, index) => index < rows.length - 1 && !row.raw.endsWith("\n") ? row.raw + source.ending : row.raw).join("")
  const final = body.replace(/\r?\n$/, "") + (body !== "" ? source.body.match(/\r?\n$/)?.[0] ?? "" : "")
  return { rows, runs, bytes: Buffer.from(source.bom + final) }
}

// Numbered post-edit rows around each change, eliding the middle of long inserted runs like oh-my-pi's compact preview.
function preview(display: string, rows: Row[], runs: { at: number; length: number }[]) {
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
  const output: string[] = []
  const shown: number[] = []
  let previous = -1
  for (const index of [...indexes].sort((a, b) => a - b)) {
    if (previous >= 0 && index > previous + 1) output.push("...")
    const content = lineContent(rows[index]!.raw)
    if (content.length > LINE_LIMIT) output.push(truncatedRow(display, index + 1, content, LINE_LIMIT))
    else {
      output.push(`${index + 1}:${content}`)
      shown.push(index + 1)
    }
    previous = index
  }
  return { output, shown }
}

/** Rejects reversed, overlapping or out-of-order edits; tail edges sort last by construction. */
function validateEdits(edits: LineEdit[]) {
  let previous = 0
  for (const edit of edits) {
    if (edit.end < edit.start || edit.start <= previous) throw new Error("Edits overlap or are not in ascending original line order")
    previous = edit.end
  }
  if (edits.reduce((size, edit) => size + edit.content.reduce((sum, line) => sum + Buffer.byteLength(line) + 1, 0), 0) > TEXT_LIMIT) {
    throw new Error(`Combined edit content exceeds ${TEXT_LIMIT} byte size limit`)
  }
}

type EditRequest = { kind: "edit"; filePath: string; tag: string; edits: LineEdit[]; remove?: boolean; move?: string }
type CreateRequest = { kind: "create"; filePath: string; content: string; overwrite?: string; destination?: boolean }
type Request = EditRequest | CreateRequest
type Handle<R extends Request = Request> = { request: R; target: Target; parent: Parent; file?: FileHandle; destination?: Handle<CreateRequest> }

type Staged =
  | { kind: "edit"; handle: Handle<EditRequest>; current: Awaited<ReturnType<typeof snapshot>>; source: Text; live: View; prior?: Version; applied: ReturnType<typeof applyChanges>; result: Text; warnings: string[] }
  | { kind: "noop"; handle: Handle<EditRequest>; live: View }
  | { kind: "remove"; handle: Handle<EditRequest>; current: Awaited<ReturnType<typeof snapshot>> }
  | { kind: "create"; handle: Handle<CreateRequest>; bytes: Buffer; created: View; replace?: Awaited<ReturnType<typeof snapshot>> }

// Locks are taken in canonical order so concurrent multi-file patches cannot deadlock; handles stay open until `run` returns.
async function locked<T>(ctx: Context, requests: Request[], run: (handles: Handle[]) => Promise<T>): Promise<T> {
  const resolved: Handle[] = []
  const claim = (handle: Handle) => {
    const duplicate = resolved.find((item) => item.target.canonical === handle.target.canonical)
    if (duplicate) throw new Error(`${duplicate.request.filePath} and ${handle.request.filePath} resolve to the same file ${handle.target.canonical}; merge them into one section`)
    resolved.push(handle)
  }
  // Authorizes the requested path before creating parents for it, then the canonical path that symlinks may redirect to.
  const prepare = async (filePath: string, action: string, create: boolean) => {
    const requested = path.resolve(ctx.directory, expandHome(filePath))
    await authorizeWrite(ctx, requested, action)
    if (create) await mkdir(path.dirname(requested), { recursive: true })
    const target = await resolveTarget(filePath, ctx, create)
    if (target.canonical !== requested) await authorizeWrite(ctx, target.canonical, action)
    return target
  }
  for (const request of requests) {
    const create = request.kind === "create" && !request.overwrite
    const action = request.kind === "create" ? (request.overwrite ? "overwrite" : "create") : request.remove ? "delete" : request.move ? "move" : "edit"
    const handle = { request, target: await prepare(request.filePath, action, create) } as Handle
    claim(handle)
    if (request.kind === "edit" && request.move) {
      const destination: CreateRequest = { kind: "create", filePath: request.move, content: "", destination: true }
      handle.destination = { request: destination, target: await prepare(request.move, "create", true) } as Handle<CreateRequest>
      claim(handle.destination)
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

async function stageEdit(ctx: Context, store: SnapshotStore, handle: Handle<EditRequest>): Promise<Staged> {
  const { request, target } = handle
  const current = await snapshot(handle.file!, TEXT_LIMIT)
  if (current.info.nlink > 1n) throw new Error("Hardlinked targets are not supported; refusing to break links")
  const source = text(current.bytes)
  const live = view(source)
  const versions = store.versions(ctx.session, target.canonical)
  const tagged = versions.filter((version) => version.tag === request.tag)
  const matches = tagMatches(versions, live, request.tag)
  if (request.remove) {
    if (!matches) throw mismatchError(store, ctx.session, target, live, request.tag, tagged.length > 0, [])
    return { kind: "remove", handle, current }
  }
  const basis = matches ? live.lines : tagged[0]?.lines
  const requested = basis ? resolveBlocks(request.edits, basis) : request.edits
  try { validateEdits(requested) } catch (error) {
    throw new Error(`[${request.filePath}#${request.tag}]: ${error instanceof Error ? error.message : String(error)}`)
  }
  const anchors = requested.flatMap(anchorLines)
  const warnings: string[] = []
  let edits: LineEdit[]
  if (matches) {
    edits = resolveEdges(requested, live.lines.length)
    const end = Math.max(0, ...anchors)
    if (end > live.lines.length) throw new Error(`Line ${end} does not exist in ${target.display} (file has ${live.lines.length} lines)`)
    const prior = versions.find((version) => version.key === live.key)
    const unseen = prior?.seen ? anchors.filter((line) => !prior.seen!.has(line)) : []
    if (unseen.length) {
      const rescued = rescueShifted(versions, live, edits, new Set(unseen))
      const remaining = unseen.filter((line) => !rescued.has(line))
      if (remaining.length) throw unseenError(store, ctx.session, target, live, remaining)
    }
  } else {
    // Like oh-my-pi, edits without line anchors only depend on where the file ends, so any tag drift is tolerable.
    const remapped = !anchors.length && !request.move
      ? { offset: 0, edits: resolveEdges(requested, live.lines.length) }
      : tagged.length && anchors.length ? remapEdits(tagged[0]!, live.lines, requested) : undefined
    if (!remapped) throw mismatchError(store, ctx.session, target, live, request.tag, tagged.length > 0, anchors)
    edits = remapped.edits
    warnings.push(!anchors.length ? TAIL_DRIFT : remapped.offset !== 0 ? RECOVERY_REMAPPED : tagged[0] === versions[0] ? RECOVERY_EXTERNAL : RECOVERY_CHAIN)
  }
  const applied = applyChanges(source, edits.map((edit) => ({ ...edit, raw: edit.content.map((line) => line + source.ending) })))
  const prior = versions.find((version) => version.key === live.key)
  if (applied.bytes.equals(current.bytes) && !request.move) return { kind: "noop", handle, live }
  return { kind: "edit", handle, current, source, live, prior, applied, result: text(applied.bytes), warnings }
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
  if (existing) throw new Error(`MV destination ${handle.target.display} already exists; REM it in its own section first`)
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
          await unchanged(handle, current)
          await handle.parent.check()
          await unlink(handle.parent.location)
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
      // The model authored every line, so like oh-my-pi the version is recorded without a seen-line restriction.
      store.record(ctx.session, canonical, item.created)
      return `${item.replace ? "Overwrote" : "Created"} [${display}#${item.created.tag}] (${item.created.lines.length} lines)`
    }
    // Like oh-my-pi, only lines that kept both number and content stay anchorable, plus the rows shown below.
    const destination = item.handle.destination?.target
    const next = view(item.result)
    const rows = preview(destination?.display ?? display, item.applied.rows, item.applied.runs)
    const seen = item.applied.rows.flatMap((row, index) => row.origin === index && (!item.prior?.seen || item.prior.seen.has(index + 1)) ? [index + 1] : [])
    store.record(ctx.session, destination?.canonical ?? canonical, next, [...seen, ...rows.shown])
    const output = [`${destination ? `Moved ${display} to ` : ""}[${destination?.display ?? display}#${next.tag}]`]
    if (item.applied.runs.length) output.push(...item.applied.rows.length ? rows.output : ["(file is now empty)"])
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
    return { start: Number(start), end: spec.includes(",") ? Number(start) : undefined }
  })
  for (const range of parsed) if (range.end !== undefined && range.end < range.start) throw new Error(`Range ${range.start}-${range.end} in ${entry} is reversed`)
  return { file: match[1]!, selection: { ranges: parsed } }
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
  rows: { text: string; line?: number }[]
  /** Footer lines for a block cut after `shown` rows; also records what was displayed. */
  finish(shown: number): string[]
}

const OUTLINE_LINES = 500
const OUTLINE_BYTES = 20 * 1024
const OUTLINE_ROWS = 150
const OUTLINE_MIN_ROWS = 5
const HEAD_LINES = 100
const SKIP_BYTES = 4 * 1024
const MINIFIED_LINE_BYTES = 400
const LOCKFILES = new Set(["package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "Cargo.lock", "poetry.lock", "uv.lock", "Pipfile.lock", "composer.lock", "Gemfile.lock", "go.sum", "flake.lock", "gradle.lockfile", "packages.lock.json"])
const GENERATED_DIRECTORIES = new Set(["node_modules", "dist"])

function approximateTokens(bytes: number) {
  const tokens = Math.round(bytes / 4)
  return tokens >= 1000 ? `about ${Math.round(tokens / 1000)}k tokens` : `about ${tokens} tokens`
}

/** Why an unselected read of this file is not worth its tokens, if it is not. */
function junk(canonical: string, bytes: number, lines: number) {
  if (bytes <= SKIP_BYTES) return
  const name = path.basename(canonical)
  if (LOCKFILES.has(name)) return "lockfile"
  if (/\.min\.(js|mjs|css)$|\.map$/.test(name)) return "minified or source map"
  if (bytes / lines > MINIFIED_LINE_BYTES) return "minified (very long lines)"
  const segment = canonical.split(path.sep).find((part) => GENERATED_DIRECTORIES.has(part))
  if (segment) return `inside ${segment}/`
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

/** Gives block edits their resolved end: a block replace or cut spans the whole block, a block insert goes after it. */
function resolveBlocks(edits: LineEdit[], lines: string[]) {
  return edits.map((edit) => {
    if (!edit.block) return edit
    const end = blockRange(lines, edit.start)
    const { block, ...rest } = edit
    return edit.operation === "after" ? { ...rest, start: end, end } : { ...rest, end }
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

/** Directory trees are joined into one result; `maxLineLength` beyond the default needs a single-line selection. */
export class Engine {
  readonly store = new SnapshotStore()

  async read(ctx: Context, args: { paths: string[]; maxLineLength?: number }): Promise<Result> {
    const maxLineLength = args.maxLineLength ?? LINE_LIMIT
    const expanded = maxLineLength > LINE_LIMIT
    const blocks: Block[] = []
    const images: Image[] = []
    const note = (head: string): Block => ({ head, rows: [], finish: () => [] })
    for (const entry of args.paths) {
      const { file: input, selection } = await splitSelector(entry, ctx)
      if (expanded) {
        const only = selection.ranges?.length === 1 ? selection.ranges[0]! : undefined
        if (args.paths.length !== 1 || !only || (only.end !== undefined && only.end !== only.start)) {
          throw new Error("maxLineLength above the default needs exactly one path with a single-line selector, like path:12")
        }
        only.end = only.start
      }
      const target = await resolveTarget(input, ctx)
      blocks.push(await serialized(target.canonical, ctx, () => inParent(target, async (parent) => {
        const file = await open(parent.location, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
        try {
          await checkDescriptor(file, parent.location, target.canonical)
          if ((await file.stat()).isDirectory()) return await listDirectory(file, parent.location, target.display)
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
          return this.#textBlock(ctx, target, text(current.bytes), selection, maxLineLength)
        } finally { await file.close() }
      })))
    }
    const size = (row: string) => Buffer.byteLength(row) + 1
    const shares = allocate(blocks.map((block) => block.rows.reduce((sum, row) => sum + size(row.text), 0)), expanded ? TEXT_LIMIT + 1024 : OUTPUT_LIMIT)
    const parts = blocks.map((block, index) => {
      let left = shares[index]!
      let shown = 0
      while (shown < block.rows.length && size(block.rows[shown]!.text) <= left) left -= size(block.rows[shown++]!.text)
      return [block.head, ...block.rows.slice(0, shown).map((row) => row.text), ...block.finish(shown)].join("\n")
    })
    return { text: parts.join("\n\n"), images }
  }

  #textBlock(ctx: Context, target: Target, source: Text, selection: Selection, maxLineLength: number): Block {
    const anchored = view(source)
    const total = anchored.lines.length
    const bytes = Buffer.byteLength(source.body)
    const head = `[${target.display}#${anchored.tag}]`
    const rowFor = (line: number) => {
      const content = anchored.lines[line - 1]!
      return { text: content.length > maxLineLength ? truncatedRow(target.display, line, content, maxLineLength) : `${line}:${content}`, line: content.length > maxLineLength ? undefined : line }
    }
    const record = (rows: { line?: number }[]) => this.store.record(ctx.session, target.canonical, anchored, rows.flatMap((row) => row.line === undefined ? [] : [row.line]))
    const size = `${total} lines, ${approximateTokens(bytes)}`
    if (!selection.ranges && !selection.tail) {
      const reason = junk(target.canonical, bytes, total)
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
          const rows = sketch.rows.map((row) => "line" in row ? rowFor(row.line) : { text: row.text, line: undefined })
          return {
            head: `${head} ${size}; outline only. Read the ranges you need, like ${target.display}:120-180, or ${target.display}:1- for everything.`,
            rows,
            finish: (shown) => {
              record(rows.slice(0, shown))
              return shown < rows.length ? ["[outline cut by the output limit]"] : []
            },
          }
        }
        const rows = Array.from({ length: Math.min(HEAD_LINES, total) }, (_, index) => rowFor(index + 1))
        return {
          head: `${head} ${size}; first ${rows.length} lines only. Continue with ${target.display}:${rows.length + 1}-, or grep it.`,
          rows,
          finish: (shown) => {
            record(rows.slice(0, shown))
            return shown < rows.length ? [`[output limit reached; continue with ${target.display}:${rows[shown]!.line ?? shown + 1}-]`] : []
          },
        }
      }
    }
    const selected = selectLines(selection, total)
    const rows: { text: string; line?: number; at: number }[] = []
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
        if (shown < rows.length) return [`[output limit reached; continue with ${target.display}:${rows[shown]!.at}-]`]
        const whole = selected.length === 1 && selected[0]!.start === 1 && selected[0]!.end === total
        if (whole) return []
        const open = !selection.tail && (selection.ranges ?? [{ start: 1 }]).every((range) => range.end === undefined)
        return [`[${total} lines total${open && last < total ? `; continue with ${target.display}:${last + 1}-` : ""}]`]
      },
    }
  }

  async grep(ctx: Context, args: { pattern: string; paths?: string[]; glob?: string; ignoreCase?: boolean; literal?: boolean; before?: number; after?: number; skip?: number }): Promise<Result> {
    const roots = (args.paths?.length ? args.paths : ["."]).map((entry) => path.resolve(ctx.directory, expandHome(entry)))
    const command = ["rg", "--json", "--no-config", "--no-require-git", "--hidden", "--glob", "!.git", "--max-columns", "0"]
    if (args.ignoreCase) command.push("--ignore-case")
    if (args.literal) command.push("--fixed-strings")
    if (args.pattern.includes("\n") || args.pattern.includes("\\n")) command.push("--multiline")
    if (args.glob) command.push("--glob", args.glob)
    command.push("--regexp", args.pattern, "--", ...roots)
    const child = Bun.spawn(command, { cwd: ctx.directory, stdout: "pipe", stderr: "pipe", signal: AbortSignal.any([ctx.signal, AbortSignal.timeout(GREP_TIMEOUT)]) })
    const [stdout, stderr, exit] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
    if (exit === 2 && !stdout) throw new Error(`rg failed: ${stderr.trim()}`)
    const files = new Map<string, Set<number>>()
    for (const line of stdout.split("\n")) {
      if (!line.startsWith('{"type":"match"')) continue
      const event = JSON.parse(line) as { data: { path: { text?: string }; line_number: number; lines: { text?: string } } }
      const file = event.data.path.text
      if (!file) continue
      const matched = files.get(file) ?? new Set<number>()
      const span = Math.max(1, (event.data.lines.text ?? "").replace(/\n$/, "").split("\n").length)
      for (let offset = 0; offset < span; offset++) matched.add(event.data.line_number + offset)
      files.set(file, matched)
    }
    if (!files.size) return { text: stderr.trim() ? `No matches found (rg: ${stderr.trim().split("\n")[0]})` : "No matches found" }
    const names = [...files.keys()].sort()
    const skip = args.skip ?? 0
    const page = names.slice(skip, skip + GREP_FILES)
    if (!page.length) return { text: `No more results (${names.length} matching files; skip ${skip} is past the end)` }
    const perFile = names.length === 1 ? GREP_SINGLE_FILE : GREP_PER_FILE
    const before = args.before ?? 1
    const after = args.after ?? 3
    const budget = { left: OUTPUT_LIMIT }
    const parts: string[] = []
    let total = 0
    for (const name of page) total += files.get(name)!.size
    for (const name of page) {
      const block = await this.#grepFile(ctx, path.resolve(ctx.directory, name), [...files.get(name)!].sort((a, b) => a - b), perFile, before, after, budget)
      if (!block) break
      parts.push(block)
    }
    const notes: string[] = []
    if (parts.length < page.length) notes.push(`[output limit reached after ${parts.length} files; narrow the search or continue with skip ${skip + parts.length}]`)
    else if (skip + page.length < names.length) notes.push(`[${names.length} matching files; continue with skip ${skip + page.length}]`)
    return { text: [`${total} matches in ${page.length} files${names.length > page.length ? ` (of ${names.length})` : ""}`, ...parts, ...notes].join("\n\n") }
  }

  async #grepFile(ctx: Context, file: string, matches: number[], limit: number, before: number, after: number, budget: { left: number }) {
    const target = await resolveTarget(file, ctx).catch(() => undefined)
    if (!target) return `[${file}] vanished before it could be read`
    return serialized(target.canonical, ctx, async () => {
      const handle = await open(target.canonical, constants.O_RDONLY | constants.O_NONBLOCK)
      let anchored: View | undefined
      let raw: string[]
      try {
        const current = await snapshot(handle, TEXT_LIMIT)
        try {
          anchored = view(text(current.bytes))
          raw = anchored.lines
        } catch {
          raw = current.bytes.toString("utf8").split("\n").map((line) => line.replace(/\r$/, ""))
        }
      } catch (error) {
        return `[${target.display}] skipped: ${error instanceof Error ? error.message : String(error)}`
      } finally { await handle.close() }
      const shown = matches.filter((line) => line <= raw.length).slice(0, limit)
      const visible = new Set<number>()
      for (const line of shown) {
        for (let index = Math.max(1, line - before); index <= Math.min(raw.length, line + after); index++) visible.add(index)
      }
      const hits = new Set(shown)
      const output = [anchored ? `[${target.display}#${anchored.tag}]` : `[${target.display}] (not anchorable)`]
      const seen: number[] = []
      let previous = 0
      for (const line of [...visible].sort((a, b) => a - b)) {
        if (previous && line > previous + 1) output.push("...")
        const content = raw[line - 1]!
        const marker = hits.has(line) ? "*" : " "
        if (content.length > GREP_COLUMNS) output.push(`${marker}${truncatedRow(target.display, line, content, GREP_COLUMNS)}`)
        else {
          output.push(`${marker}${line}:${content}`)
          seen.push(line)
        }
        previous = line
      }
      if (matches.length > shown.length) output.push(`[${matches.length - shown.length} more matches in this file]`)
      const block = output.join("\n")
      const size = Buffer.byteLength(block) + 2
      if (size > budget.left) return undefined
      budget.left -= size
      if (anchored) this.store.record(ctx.session, target.canonical, anchored, seen)
      return block
    })
  }

  async patch(ctx: Context, input: string): Promise<Result> {
    const sections = parsePatch(input)
    const requests = sections.map((section): Request => {
      if (section.kind === "create") {
        text(Buffer.from(section.content))
        return { kind: "create", filePath: section.path, content: section.content }
      }
      return { kind: "edit", filePath: section.path, tag: section.tag, edits: section.edits, remove: section.remove, move: section.move } satisfies EditRequest & Pick<EditSection, "remove" | "move">
    })
    return locked(ctx, requests, async (handles) => {
      const staged: Staged[] = []
      for (const handle of handles) {
        staged.push(handle.request.kind === "create"
          ? await stageCreate(ctx, this.store, handle as Handle<CreateRequest>)
          : await stageEdit(ctx, this.store, handle as Handle<EditRequest>))
      }
      return { text: (await commit(ctx, this.store, staged)).join("\n\n") }
    })
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
