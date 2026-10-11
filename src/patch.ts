import type { LineEdit } from "./snapshots"

export type EditSection = { kind: "edit"; path: string; tag: string; edits: LineEdit[]; remove?: boolean; move?: string; line: number }
export type CreateSection = { kind: "create"; path: string; content: string; line: number }
export type PatchSection = EditSection | CreateSection

const HEADER = /^\[(.+)#([0-9A-Fa-f]{4})\]$/
const ADD = /^\*\*\* Add File: (.+)$/
const RANGE_PUT = /^PUT ([1-9][0-9]*)\.=([1-9][0-9]*):$/
const GAP_PUT = /^PUT ([<>])([1-9][0-9]*):$/
const TAIL_PUT = /^PUT >\$:$/
const CUT = /^CUT ([1-9][0-9]*)\.=([1-9][0-9]*)$/
const BLOCK_PUT = /^PUT (>?)([1-9][0-9]*)\*:$/
const BLOCK_CUT = /^CUT ([1-9][0-9]*)\*$/
const MV = /^MV (.+)$/
// Forms models borrow from other edit languages; named so the error can say what to write instead.
const CONTAMINATION: [RegExp, string][] = [
  [/^[-+ ]?@@/, "unified-diff hunk headers are not hashline; use PUT/CUT with line numbers"],
  [/^\*\*\* (Update|Delete) File:/, "apply_patch file headers are not hashline; use a [PATH#TAG] section (REM deletes)"],
  [/^-/, "- rows are not accepted; PUT replaces the range with + rows, CUT deletes"],
  [/^\d+:/, "LINE:TEXT rows are read output; body rows are + followed by the new text"],
  [/^PUT ([1-9][0-9]*)(-|\.\.)([1-9][0-9]*):$/, "ranges are written N.=M"],
  [/^PUT ([1-9][0-9]*):$/, "a single-line replace is written PUT N.=N:"],
]

// At a shared gap, "PUT >N:" content lands before "PUT <N+1:" content; a tail append always sorts last.
function position(edit: LineEdit) {
  if (edit.edge) return Number.POSITIVE_INFINITY
  return edit.operation === "before" ? edit.start - 0.4 : edit.operation === "after" ? edit.start + 0.4 : edit.start
}

function unquote(value: string) {
  const trimmed = value.trim()
  return trimmed.length > 1 && /^(["']).*\1$/.test(trimmed) ? trimmed.slice(1, -1) : trimmed
}

/**
 * Parses a patch into per-file sections with edits sorted by original position.
 * Sections repeating a path and tag are merged; a missing Begin/End envelope and blank lines between hunks are tolerated.
 * @throws Error naming the offending patch line
 */
export function parsePatch(input: string): PatchSection[] {
  const lines = input.replaceAll("\r\n", "\n").split("\n")
  if (lines.at(-1) === "") lines.pop()
  let first = 0
  let last = lines.length
  while (first < last && !lines[first]!.trim()) first++
  while (last > first && !lines[last - 1]!.trim()) last--
  if (lines[first]?.trim() === "*** Begin Patch") first++
  if (last > first && lines[last - 1]!.trim() === "*** End Patch") last--
  const fail = (index: number, message: string): never => {
    throw new Error(`Patch line ${index + 1}: ${message}; got ${JSON.stringify(lines[index] ?? "")}`)
  }
  const sections: PatchSection[] = []
  let current: PatchSection | undefined
  let index = first
  const body = () => {
    const rows: string[] = []
    while (index < last && lines[index]!.startsWith("+")) rows.push(lines[index++]!.slice(1))
    return rows
  }
  while (index < last) {
    const line = lines[index]!
    if (!line.trim()) {
      index++
      continue
    }
    const header = HEADER.exec(line)
    const add = ADD.exec(line)
    if (header || add) {
      const at = index++
      if (header) {
        const path = unquote(header[1]!)
        const tag = header[2]!.toUpperCase()
        const earlier = sections.find((section): section is EditSection => section.kind === "edit" && section.path === path)
        if (earlier && earlier.tag !== tag) fail(at, `${path} already has a section with tag ${earlier.tag}`)
        if (earlier?.remove || earlier?.move) fail(at, `${path} already ends with REM or MV`)
        current = earlier ?? { kind: "edit", path, tag, edits: [], line: at + 1 }
        if (!earlier) sections.push(current)
      } else {
        const rows = body()
        current = { kind: "create", path: unquote(add![1]!), content: rows.length ? rows.join("\n") + "\n" : "", line: at + 1 }
        sections.push(current)
      }
      continue
    }
    if (!current) fail(index, CONTAMINATION.find(([pattern]) => pattern.test(line))?.[1] ?? "expected a [PATH#TAG] header or *** Add File: PATH")
    if (current!.kind === "create") fail(index, "an Add File section takes only + rows")
    const section = current as EditSection
    const at = index++
    if (section.remove || section.move) fail(at, "nothing may follow REM or MV in a section")
    const range = RANGE_PUT.exec(line)
    const gap = GAP_PUT.exec(line)
    const cut = CUT.exec(line)
    const block = BLOCK_PUT.exec(line)
    const blockCut = BLOCK_CUT.exec(line)
    const move = MV.exec(line)
    let edit: Omit<LineEdit, "content">
    if (range) edit = { start: Number(range[1]), end: Number(range[2]), operation: "replace" }
    else if (gap) edit = { start: Number(gap[2]), end: Number(gap[2]), operation: gap[1] === "<" ? "before" : "after" }
    else if (TAIL_PUT.test(line)) edit = { start: Number.MAX_SAFE_INTEGER, end: Number.MAX_SAFE_INTEGER, operation: "after", edge: "tail" }
    else if (block) edit = { start: Number(block[2]), end: Number(block[2]), operation: block[1] ? "after" : "replace", block: true }
    else if (cut || blockCut) {
      section.edits.push(cut
        ? { start: Number(cut[1]), end: Number(cut[2]), operation: "replace", content: [] }
        : { start: Number(blockCut![1]), end: Number(blockCut![1]), operation: "replace", content: [], block: true })
      continue
    } else if (line.trim() === "REM") {
      if (section.edits.length) fail(at, "REM deletes the file and cannot be combined with PUT or CUT")
      section.remove = true
      continue
    } else if (move) {
      section.move = unquote(move[1]!)
      continue
    } else {
      const hint = CONTAMINATION.find(([pattern]) => pattern.test(line))?.[1]
      return fail(at, hint ?? "expected PUT N.=M:, PUT N*:, PUT <N:, PUT >N:, PUT >N*:, PUT >$:, CUT N.=M, CUT N*, REM, MV DEST, or a new section header")
    }
    const content = body()
    if (!content.length) fail(at, "PUT needs at least one + row (use CUT to delete lines)")
    section.edits.push({ ...edit, content })
  }
  if (!sections.length) throw new Error("Patch contains no sections")
  for (const section of sections) {
    if (section.kind !== "edit") continue
    if (!section.edits.length && !section.remove && !section.move) {
      throw new Error(`Section [${section.path}#${section.tag}] has no PUT, CUT, REM or MV`)
    }
    section.edits.sort((a, b) => position(a) - position(b))
  }
  return sections
}
