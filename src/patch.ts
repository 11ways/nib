import type { LineEdit } from "./snapshots"

/** `tag` is undefined for a tagless [PATH] section, which may only append; `move` and `copy` write the section's result to another path. */
export type EditSection = { kind: "edit"; path: string; tag?: string; edits: LineEdit[]; remove?: boolean; move?: string; copy?: string; line: number }
export type CreateSection = { kind: "create"; path: string; content: string; line: number }
export type PatchSection = EditSection | CreateSection

const HEADER = /^\[(.+)#([0-9A-Fa-f]{4})\]$/
const TAGLESS = /^\[(.+)\]$/
const ADD = /^\*\*\* Add File: (.+)$/
const RANGE_PUT = /^PUT ([1-9][0-9]*)\.=([1-9][0-9]*):$/
const GAP_PUT = /^PUT ([<>])([1-9][0-9]*):$/
const TAIL_PUT = /^PUT >\$:$/
const CUT = /^CUT ([1-9][0-9]*)\.=([1-9][0-9]*)$/
const BLOCK_PUT = /^PUT (>?)([1-9][0-9]*)\*:$/
const BLOCK_CUT = /^CUT ([1-9][0-9]*)\*$/
// `[^]` rather than `.`, so old text holding a lone CR or a Unicode line separator still parses.
const SUB = /^SUB ([1-9][0-9]*): ([^]+)$/
const MV = /^MV (.+)$/
const CP = /^CP (.+)$/
// Forms models borrow from other edit languages or from nib's own output; named so the error can say what to write instead.
const CONTAMINATION: [RegExp, string][] = [
  [/^[-+ ]?@@/, "unified-diff hunk headers are not hashline; use PUT/CUT with line numbers"],
  [/^\*\*\* (Update|Delete) File:/, "apply_patch file headers are not hashline; use a [PATH#TAG] section (REM deletes)"],
  [/^-/, "- rows are not accepted; PUT replaces the range with + rows, CUT deletes"],
  [/^\d+:/, "LINE:TEXT rows are read output; body rows are + followed by the new text"],
  [/^\d+~/, "N~TEXT rows are row-map output; anchor with PUT or CUT, and check a line with a ~TEXT row under that line"],
  [/^~/, "~ check rows go right after a PUT or CUT line, before its + rows"],
  [/^SUB [1-9][0-9]*:\s*$/, "SUB N: is followed on the same line by the exact old text"],
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
 * @throws Error naming every offending patch line, not just the first
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
  const errors: string[] = []
  const failed = new Set<PatchSection>()
  const sections: PatchSection[] = []
  let current: PatchSection | undefined
  let index = first
  const rows = (prefix: string) => {
    const found: string[] = []
    while (index < last && lines[index]!.startsWith(prefix)) found.push(lines[index++]!.slice(1))
    return found
  }
  while (index < last) {
    const line = lines[index]!
    if (!line.trim()) {
      index++
      continue
    }
    const at = index++
    try {
      const header = HEADER.exec(line)
      const add = ADD.exec(line)
      const tagless = !header && !add ? TAGLESS.exec(line) : null
      if (header || tagless) {
        const raw = (header ?? tagless)![1]!
        const path = unquote(raw)
        const tag = header?.[2]!.toUpperCase()
        const earlier = sections.find((section): section is EditSection => section.kind === "edit" && section.path === path)
        // A refused header still opens a section that is never returned, so its hunks are checked without being blamed on another file.
        current = earlier && earlier.tag === tag && !earlier.remove && !earlier.move && !earlier.copy ? earlier : { kind: "edit", path, tag: tag ?? "", edits: [], line: at + 1 }
        if (tagless && !/^["']/.test(raw) && /#[0-9A-Za-z]*$/.test(raw)) fail(at, "a tag is 4 hex digits, like [PATH#3C4D]")
        if (earlier && earlier.tag !== tag) fail(at, `${path} already has a section ${earlier.tag ? `with tag ${earlier.tag}` : "without a tag"}`)
        if (earlier?.remove || earlier?.move || earlier?.copy) fail(at, `${path} already ends with REM, MV or CP`)
        if (!earlier) {
          current.tag = tag
          sections.push(current)
        }
        continue
      }
      if (add) {
        current = { kind: "create", path: unquote(add[1]!), content: "", line: at + 1 }
        const body = rows("+")
        current.content = body.length ? body.join("\n") + "\n" : ""
        sections.push(current)
        continue
      }
      if (!current) {
        current = { kind: "edit", path: "", tag: "", edits: [], line: at + 1 }
        fail(at, CONTAMINATION.find(([pattern]) => pattern.test(line))?.[1] ?? "expected a [PATH#TAG] header or *** Add File: PATH")
      }
      if (current!.kind === "create") fail(at, "an Add File section takes only + rows")
      const section = current as EditSection
      if (section.remove || section.move || section.copy) fail(at, "nothing may follow REM, MV or CP in a section")
      const range = RANGE_PUT.exec(line)
      const gap = GAP_PUT.exec(line)
      const cut = CUT.exec(line)
      const block = BLOCK_PUT.exec(line)
      const blockCut = BLOCK_CUT.exec(line)
      const sub = SUB.exec(line)
      const move = MV.exec(line)
      const copy = CP.exec(line)
      const untagged = (form: string) => fail(at, `a [PATH] section without a tag may only append with PUT >$:, not ${form}; copy [PATH#TAG] from your latest read to edit elsewhere`)
      if (line.trim() === "REM") {
        if (section.tag === undefined) untagged("REM")
        if (section.edits.length) fail(at, "REM deletes the file and cannot be combined with PUT or CUT")
        section.remove = true
        continue
      }
      if (move || copy) {
        if (section.tag === undefined) untagged(move ? "MV" : "CP")
        if (move) section.move = unquote(move[1]!)
        else section.copy = unquote(copy![1]!)
        continue
      }
      // `checks` is how many ~ rows the hunk accepts: its named first line, plus the last line of a multi-line range.
      let edit: Omit<LineEdit, "content">
      let checks = 1
      let body = true
      if (range) {
        edit = { start: Number(range[1]), end: Number(range[2]), operation: "replace" }
        checks = edit.start < edit.end ? 2 : 1
      } else if (gap) edit = { start: Number(gap[2]), end: Number(gap[2]), operation: gap[1] === "<" ? "before" : "after" }
      else if (TAIL_PUT.test(line)) {
        edit = { start: Number.MAX_SAFE_INTEGER, end: Number.MAX_SAFE_INTEGER, operation: "after", edge: "tail" }
        checks = 0
      } else if (block) edit = { start: Number(block[2]), end: Number(block[2]), operation: block[1] ? "after" : "replace", block: true }
      else if (cut) {
        edit = { start: Number(cut[1]), end: Number(cut[2]), operation: "replace" }
        checks = edit.start < edit.end ? 2 : 1
        body = false
      } else if (blockCut) {
        edit = { start: Number(blockCut[1]), end: Number(blockCut[1]), operation: "replace", block: true }
        body = false
      } else if (sub) {
        edit = { start: Number(sub[1]), end: Number(sub[1]), operation: "replace", sub: sub[2]! }
        checks = 0
      } else {
        const hint = CONTAMINATION.find(([pattern]) => pattern.test(line))?.[1]
        return fail(at, hint ?? "expected PUT N.=M:, PUT N*:, PUT <N:, PUT >N:, PUT >N*:, PUT >$:, CUT N.=M, CUT N*, SUB N: OLD, REM, MV DEST, CP DEST, or a new section header")
      }
      if (section.tag === undefined && !edit.edge) untagged(line.split(" ")[0]!)
      const found = rows("~")
      if (found.length > checks) fail(at, checks ? `this hunk takes at most ${checks} ~ check row${checks > 1 ? "s" : ""}` : "this hunk takes no ~ check rows")
      const content = body ? rows("+") : []
      if (body && !content.length) fail(at, sub ? "SUB needs + rows with the new text (a lone + deletes the old text)" : "PUT needs at least one + row (use CUT to delete lines)")
      section.edits.push({ ...edit, content, ...found.length && { checks: found } })
    } catch (error) {
      errors.push(error instanceof Error ? error.message : String(error))
      if (current) failed.add(current)
      while (index < last && /^[+~]/.test(lines[index]!)) index++
    }
  }
  for (const section of sections) {
    if (section.kind !== "edit" || failed.has(section)) continue
    if (!section.edits.length && !section.remove && !section.move && !section.copy) {
      errors.push(`Section [${section.path}${section.tag ? `#${section.tag}` : ""}] has no PUT, CUT, SUB, REM, MV or CP`)
    }
    section.edits.sort((a, b) => position(a) - position(b))
  }
  if (!sections.length && !errors.length) errors.push("Patch contains no sections")
  if (errors.length) throw new Error(errors.join("\n"))
  return sections
}
