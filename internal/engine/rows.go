package engine

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/11ways/nib/internal/js"
)

func itoa(n int) string { return strconv.Itoa(n) }

// ranges writes sorted lines as "1-3, 7".
func ranges(lines []int) string {
	var parts []string
	for index := 0; index < len(lines); {
		last := index
		for last+1 < len(lines) && lines[last+1] == lines[last]+1 {
			last++
		}
		if last == index {
			parts = append(parts, itoa(lines[index]))
		} else {
			parts = append(parts, itoa(lines[index])+"-"+itoa(lines[last]))
		}
		index = last + 1
	}
	return strings.Join(parts, ", ")
}

// escapes are terminal escape sequences: CSI (colors), OSC (ended by BEL or ST), charset selection, other two-byte escapes, and a lone ESC.
var escapes = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()][0-9A-Za-z]|[@-_])?`)

// visible is the text a row shows for a line, with terminal escape codes removed.
func visible(content string) string {
	if !strings.Contains(content, "\x1b") {
		return content
	}
	return escapes.ReplaceAllString(content, "")
}

// Visible is visible for other packages.
func Visible(content string) string { return visible(content) }

type marks struct{ cut, colored bool }

type span struct{ start, end int }

// matchSpans are the UTF-16 offsets of the first submatches of a match on one line; ripgrep reports UTF-8 byte offsets.
func matchSpans(lineText string, submatches []submatch) []span {
	body := lineContent(lineText)
	if strings.Contains(body, "\n") {
		return nil
	}
	var spans []span
	for i, s := range submatches {
		if i == windows {
			break
		}
		spans = append(spans, span{js.UnitsOf(body, s.Start), js.UnitsOf(body, s.End)})
	}
	return spans
}

// changedSpan is where after differs from before: everything between their common start and common end, at least one character wide.
func changedSpan(before, after string) span {
	b, a := js.Units(before), js.Units(after)
	start := 0
	for start < len(b) && start < len(a) && b[start] == a[start] {
		start++
	}
	end := 0
	for end < len(b)-start && end < len(a)-start && b[len(b)-1-end] == a[len(a)-1-end] {
		end++
	}
	return span{start, max(start+1, len(a)-end)}
}

type windowed struct{ label, text string }

// matchWindows are the stretches around the first matches of a long line, for when cutting it at columns would hide the first match.
func matchWindows(content string, spans []span, columns int) *windowed {
	if len(spans) == 0 || spans[0].end <= columns {
		return nil
	}
	reach := max(20, columns/(2*windows))
	length := js.Len(content)
	var found []span
	for _, s := range spans {
		start := max(0, s.start-reach)
		end := min(length, s.end+reach, start+columns)
		if n := len(found); n > 0 && start <= found[n-1].end {
			found[n-1].end = min(max(found[n-1].end, end), found[n-1].start+columns)
		} else {
			found = append(found, span{start, end})
		}
	}
	labels := make([]string, len(found))
	texts := make([]string, len(found))
	for i, w := range found {
		labels[i] = fmt.Sprintf("%d-%d", w.start+1, w.end)
		texts[i] = visible(js.Slice(content, w.start, w.end))
	}
	return &windowed{label: fmt.Sprintf("chars %s of %d", strings.Join(labels, ", "), length), text: strings.Join(texts, " ... ")}
}

type row struct {
	text   string
	anchor bool
	marks
}

// lineRow is an N:TEXT row, or a bracketed row that is deliberately never an anchor: a line longer than limit, or one whose escape codes were stripped.
func lineRow(line int, content string, limit int) row {
	shown := visible(content)
	colored := shown != content
	length := js.Len(shown)
	cut := length > limit
	if !colored && !cut {
		return row{text: itoa(line) + ":" + content, anchor: true}
	}
	var labels []string
	if colored {
		labels = append(labels, "ansi")
	}
	if cut {
		labels = append(labels, fmt.Sprintf("cut at %d of %d chars", limit, length))
	}
	return row{text: fmt.Sprintf("[%d %s]|%s", line, strings.Join(labels, ", "), js.Slice(shown, 0, limit)), marks: marks{cut, colored}}
}

// rowNotes are footer notes explaining the bracketed rows among rows, once per block.
func rowNotes(display string, rows []marks) []string {
	var notes []string
	if slices.ContainsFunc(rows, func(m marks) bool { return m.cut }) {
		notes = append(notes, fmt.Sprintf("[cut rows: change them in place with SUB N: OLD, or read %s:N with maxLineLength to see one whole]", display))
	}
	if slices.ContainsFunc(rows, func(m marks) bool { return m.colored }) {
		notes = append(notes, "[ansi rows: their color codes are stripped, so they are not anchors; change them in place with SUB N: OLD]")
	}
	return notes
}

type framed struct {
	rows  []string
	shown []int
	marks []marks
}

// contextRows are rows around marked lines (prefixed *, others space), bracketed past columns; shown lists the rows displayed in full.
func contextRows(lines []string, marked []int, radius, columns int) framed {
	flagged := map[int]bool{}
	display := map[int]bool{}
	for _, anchor := range marked {
		flagged[anchor] = true
		for line := max(1, anchor-radius); line <= min(len(lines), anchor+radius); line++ {
			display[line] = true
		}
	}
	var out framed
	previous := 0
	for _, line := range sortedSet(display) {
		if previous != 0 && line > previous+1 {
			out.rows = append(out.rows, "...")
		}
		r := lineRow(line, lines[line-1], columns)
		prefix := " "
		if flagged[line] {
			prefix = "*"
		}
		out.rows = append(out.rows, prefix+r.text)
		if r.anchor {
			out.shown = append(out.shown, line)
		} else {
			out.marks = append(out.marks, r.marks)
		}
		previous = line
	}
	return out
}

func sortedSet(set map[int]bool) []int {
	keys := make([]int, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// clip is the first mapColumns characters after the indentation, with an ellipsis when the line goes on.
func clip(content string) string {
	t := visible(content)
	rest := js.TrimStart(t)
	indent := t[:len(t)-len(rest)]
	runes := []rune(rest)
	if len(runes) > mapColumns {
		return indent + string(runes[:mapColumns]) + "..."
	}
	return t
}

type mapOptions struct {
	first, last bool
	after       *int
}

// mapLines are the lines a sparse row map shows in from..to: a non-blank line after a blank one once mapGap lines passed since the last shown row,
// and every mapStride-th non-blank line without one; first and last add those non-blank lines, and after is the last row already shown.
func mapLines(lines []string, from, to int, options mapOptions) []int {
	blank := func(line int) bool { return js.Trim(lines[line-1]) == "" }
	first, last := from, to
	for first <= last && blank(first) {
		first++
	}
	for last >= first && blank(last) {
		last--
	}
	var picked []int
	previous := -1 << 60
	if options.after != nil {
		previous = *options.after
	}
	count := 0
	for line := first; line <= last; line++ {
		if blank(line) {
			continue
		}
		count++
		if (options.first && line == first) || (options.last && line == last) || (line > 1 && blank(line-1) && line-previous >= mapGap) || count == mapStride {
			picked = append(picked, line)
			previous = line
			count = 0
		}
	}
	return picked
}

type writtenRowsMap struct {
	summary string
	rows    []string
}

// writtenMap is the response rows for a file the agent just wrote: a row map, or past mapRows rows the outline a read would give.
func writtenMap(display string, created viewLines) writtenRowsMap {
	lines := created
	nonBlank := 0
	for _, content := range lines {
		if js.Trim(content) != "" {
			nonBlank++
		}
	}
	counts := fmt.Sprintf("%d lines, %d non-blank", len(lines), nonBlank)
	picked := mapLines(lines, 1, len(lines), mapOptions{first: true, last: true})
	if len(picked) <= mapRows {
		rows := make([]string, len(picked))
		for i, line := range picked {
			rows[i] = fmt.Sprintf("%d~%s", line, clip(lines[line-1]))
		}
		return writtenRowsMap{summary: fmt.Sprintf("%s, %d shown", counts, len(picked)), rows: rows}
	}
	sketch := outline(display, lines)
	anchors := 0
	for _, r := range sketch.rows {
		if r.line > 0 {
			anchors++
		}
	}
	if anchors < outlineMinRows || sketch.candidates*2 > nonBlank {
		return writtenRowsMap{summary: counts + "; too long to map", rows: []string{}}
	}
	result := writtenRowsMap{summary: counts + "; outline only"}
	for _, r := range sketch.rows {
		if r.line > 0 {
			result.rows = append(result.rows, lineRow(r.line, lines[r.line-1], LineLimit).text)
		} else {
			result.rows = append(result.rows, r.text)
		}
	}
	return result
}

type viewLines = []string
