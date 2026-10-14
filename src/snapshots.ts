import { diffArrays } from "diff"

/** Text identity of one file version: LF-normalized BOM-free body, its line contents, and its tag. */
export type View = { key: string; lines: string[]; tag: string }
/** A retained version; `seen` is undefined when edits may anchor anywhere, `known` holds lines whose content the agent wrote or saw before they moved. */
export type Version = View & { seen?: Set<number>; known?: Set<number> }
/**
 * One edit in 1-based line coordinates; a `tail` edge appends after whatever the last line is when applied. A `block` edit names only the block's first line until the engine resolves its end.
 * `checks` are the ~ rows for the named first and last line, `confirmed` the lines they matched; `sub` is SUB's old text until the engine resolves it into a `quoted` replace,
 * which is `opaque` when the agent never saw the whole line, so its result is not known content.
 */
export type LineEdit = {
  start: number
  end: number
  operation: "replace" | "before" | "after"
  content: string[]
  edge?: "tail"
  block?: true
  checks?: string[]
  confirmed?: number[]
  sub?: string
  quoted?: true
  opaque?: true
}

const MAX_PATHS = 256
const MAX_VERSIONS = 4
const MAX_UNITS = 64 * 1024 * 1024
const DIFF_TIMEOUT = 1000
const EVIDENCE_MIN_CHARS = 8

function trimEnd(line: string) {
  let end = line.length
  while (end > 0 && (line[end - 1] === " " || line[end - 1] === "\t" || line[end - 1] === "\r")) end--
  return line.slice(0, end)
}

/** Four uppercase hex digits of xxHash32 over the text with trailing spaces, tabs and CRs stripped per line; matches oh-my-pi `file_hash`. */
export function fileTag(text: string) {
  return (Bun.hash.xxHash32(text.split("\n").map(trimEnd).join("\n"), 0) & 0xffff).toString(16).toUpperCase().padStart(4, "0")
}

type History = { versions: Version[]; units: number }

/** Per-session file versions with LRU eviction across all sessions and paths. */
export class SnapshotStore {
  readonly #histories = new Map<string, History>()
  #units = 0

  constructor(private readonly limits = { paths: MAX_PATHS, versions: MAX_VERSIONS, units: MAX_UNITS }) {}

  /** Records a version as most recent and unions `seen` and `known` into its line sets. */
  record(session: string, path: string, view: View, seen?: Iterable<number>, known?: Iterable<number>): Version {
    const id = `${session}\0${path}`
    const history = this.#histories.get(id) ?? { versions: [], units: 0 }
    this.#histories.delete(id)
    this.#units -= history.units
    const index = history.versions.findIndex((version) => version.key === view.key)
    const version: Version = index >= 0 ? history.versions.splice(index, 1)[0]! : { ...view }
    if (seen) {
      version.seen ??= new Set()
      for (const line of seen) version.seen.add(line)
    }
    if (known) {
      version.known ??= new Set()
      for (const line of known) version.known.add(line)
    }
    history.versions.unshift(version)
    history.versions.length = Math.min(history.versions.length, this.limits.versions)
    history.units = 1 + history.versions.reduce((sum, item) => sum + item.key.length * 2, 0)
    this.#histories.set(id, history)
    this.#units += history.units
    while (this.#histories.size > this.limits.paths || this.#units > this.limits.units) {
      const oldest = this.#histories.keys().next().value
      if (oldest === undefined) break
      this.#units -= this.#histories.get(oldest)!.units
      this.#histories.delete(oldest)
    }
    return version
  }

  /** Retained versions, newest first; refreshes recency. */
  versions(session: string, path: string): Version[] {
    const id = `${session}\0${path}`
    const history = this.#histories.get(id)
    if (!history) return []
    this.#histories.delete(id)
    this.#histories.set(id, history)
    return [...history.versions]
  }

  /** Other paths this session issued `tag` for. */
  origins(session: string, tag: string, except: string) {
    const prefix = `${session}\0`
    return [...this.#histories].filter(([id, history]) => id.startsWith(prefix) && id !== prefix + except && history.versions.some((version) => version.tag === tag))
      .map(([id]) => id.slice(prefix.length))
  }
}

/** The line diff from `previous` to `current`; undefined when it times out. */
export function lineChanges(previous: string[], current: string[]) {
  return diffArrays(previous, current, { timeout: DIFF_TIMEOUT })
}

/** Maps 1-based previous line numbers to current ones across unchanged runs; undefined when the diff times out. */
export function lineMap(previous: string[], current: string[]) {
  const changes = lineChanges(previous, current)
  if (!changes) return
  const map = new Map<number, number>()
  let before = 1
  let after = 1
  for (const change of changes) {
    const count = change.count ?? change.value.length
    if (change.added) after += count
    else if (change.removed) before += count
    else {
      for (let offset = 0; offset < count; offset++) map.set(before + offset, after + offset)
      before += count
      after += count
    }
  }
  return map
}

/** Lines whose content an edit depends on: the whole replaced range, or the insertion neighbour. */
export function anchorLines(edit: LineEdit) {
  if (edit.edge) return []
  if (edit.operation !== "replace") return [edit.start]
  return Array.from({ length: edit.end - edit.start + 1 }, (_, index) => edit.start + index)
}

/**
 * Anchor lines `version` does not let these edits use. A displayed line always counts; an insert may also rest on a line a ~ check confirmed,
 * and a replace may span lines the agent knows when each end that was not displayed is confirmed. SUB edits quote what they replace.
 */
export function unverified(version: Version, edits: LineEdit[]) {
  const seen = version.seen
  if (!seen) return []
  const lines = new Set<number>()
  for (const edit of edits) {
    if (edit.edge || edit.quoted) continue
    const confirmed = new Set(edit.confirmed)
    if (edit.operation !== "replace") {
      if (!seen.has(edit.start) && !confirmed.has(edit.start)) lines.add(edit.start)
      continue
    }
    for (let line = edit.start; line <= edit.end; line++) {
      if (seen.has(line)) continue
      const end = line === edit.start || line === edit.end
      if (!version.known?.has(line) || (end && !confirmed.has(line))) lines.add(line)
    }
  }
  return [...lines].sort((a, b) => a - b)
}

function duplicates(lines: string[]) {
  const seen = new Set<string>()
  const repeated = new Set<string>()
  for (const line of lines) (seen.has(line) ? repeated : seen).add(line)
  return repeated
}

// Port of oh-my-pi `validate_context`: every anchor needs a neighbour that moved by the same offset.
function contextHolds(previous: string[], current: string[], map: Map<number, number>, anchors: number[]) {
  const previousRepeated = duplicates(previous)
  const currentRepeated = duplicates(current)
  const sorted = [...new Set(anchors)].sort((a, b) => a - b)
  for (let index = 0; index < sorted.length;) {
    let last = index
    while (last + 1 < sorted.length && sorted[last + 1] === sorted[last]! + 1) last++
    const before = sorted[index]! > 1 ? sorted[index]! - 1 : undefined
    const after = sorted[last]! < previous.length ? sorted[last]! + 1 : undefined
    for (const line of sorted.slice(index, last + 1)) {
      const mapped = map.get(line)
      if (mapped === undefined) return false
      const offset = mapped - line
      const moved = (neighbour?: number) => neighbour !== undefined && map.get(neighbour) === neighbour + offset
      if (!previousRepeated.has(previous[line - 1]!) && !currentRepeated.has(current[mapped - 1]!)) {
        if (!moved(before) && !moved(after)) return false
      } else {
        if (before === undefined && after === undefined) return false
        if (before !== undefined && !moved(before)) return false
        if (after !== undefined && !moved(after)) return false
      }
    }
    index = last + 1
  }
  return true
}

/** Pins tail edges to the last line of a file with `length` lines. */
export function resolveEdges(edits: LineEdit[], length: number) {
  return edits.map((edit) => edit.edge ? { ...edit, start: length, end: length } : edit)
}

/** Rebases edits authored against `previous` onto `current` when every anchor moved by one uniform offset; whether the anchors were displayed is checked by the caller. */
export function remapEdits(previous: Version, current: string[], edits: LineEdit[]) {
  const anchors = edits.flatMap(anchorLines)
  if (!anchors.length) return { offset: 0, edits: resolveEdges(edits, current.length) }
  const map = lineMap(previous.lines, current)
  if (!map || !contextHolds(previous.lines, current, map, anchors)) return
  const offsets = new Set(anchors.map((line) => (map.get(line) ?? Number.NaN) - line))
  if (offsets.size !== 1) return
  const offset = [...offsets][0]!
  if (Number.isNaN(offset)) return
  return { offset, edits: resolveEdges(edits.map((edit) => edit.edge ? edit : { ...edit, start: edit.start + offset, end: edit.end + offset }), current.length) }
}

function uniqueEvidence(lines: string[], line: number) {
  const content = lines[line - 1]?.trim()
  if (!content || [...content].length < EVIDENCE_MIN_CHARS) return
  return lines.filter((other) => other.trim() === content).length === 1 ? content : undefined
}

const identifier = (char?: string) => char !== undefined && /[\p{L}\p{N}_]/u.test(char)

// Token-bounded containment: "foo(bar)" is not carried by "foo(bar_baz)".
function carries(line: string, evidence: string) {
  const startsWord = identifier(evidence[0])
  const endsWord = identifier(evidence.at(-1))
  for (let at = line.indexOf(evidence); at >= 0; at = line.indexOf(evidence, at + 1)) {
    if (!(startsWord && identifier(line[at - 1])) && !(endsWord && identifier(line[at + evidence.length]))) return true
  }
  return false
}

/**
 * Unseen replace anchors that a shift-aware reading accepts (port of oh-my-pi `rescue_shifted_anchors`).
 * Each must show content some retained version displayed at another number, and the payload must carry the current content of both span ends.
 */
export function rescueShifted(versions: Version[], current: View, edits: LineEdit[], unseen: Set<number>) {
  const images = versions.filter((version) => version.seen?.size).map((version) => {
    if (version.key === current.key) return (line: number) => version.seen!.has(line)
    const forward = lineMap(version.lines, current.lines)
    const backward = new Map([...forward ?? []].map(([before, after]) => [after, before]))
    return (line: number) => version.seen!.has(backward.get(line) ?? 0)
  })
  const rescued = new Set<number>()
  for (const edit of edits) {
    if (edit.operation !== "replace" || !edit.content.length) continue
    const pending = anchorLines(edit).filter((line) => unseen.has(line))
    if (!pending.length) continue
    const head = uniqueEvidence(current.lines, pending[0]!)
    const headAt = head === undefined ? -1 : edit.content.findIndex((line) => carries(line, head))
    if (headAt < 0) continue
    if (pending.length > 1) {
      const tail = uniqueEvidence(current.lines, pending.at(-1)!)
      if (tail === undefined || edit.content.findLastIndex((line) => carries(line, tail)) < headAt) continue
    }
    if (pending.every((line) => images.some((seen) => seen(line)))) for (const line of pending) rescued.add(line)
  }
  return rescued
}
