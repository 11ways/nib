package engine

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/patch"
	"github.com/11ways/nib/internal/snapshots"
)

type checked struct {
	edits   []snapshots.LineEdit
	refusal *Refusal
	// lessons are what nib read differently than sent, for the response.
	lessons []string
}

// together is one refusal for all of a section's problems; the patch may be sent again unchanged only if each of them allows it.
func together(problems []*Refusal) *Refusal {
	if len(problems) == 1 {
		return problems[0]
	}
	messages := make([]string, len(problems))
	retry := true
	for i, p := range problems {
		messages[i] = p.message
		retry = retry && p.Retry
	}
	return &Refusal{message: strings.Join(messages, "\n\n"), Retry: retry}
}

// nonNil keeps an empty line list a list: recording one restricts a version to the lines it names, which none means none.
func nonNil(lines []int) []int {
	if lines == nil {
		return []int{}
	}
	return lines
}

// unseenError mirrors oh-my-pi's unseen-line guard: reveal what the model would have touched and let a straight retry pass.
func unseenError(store *snapshots.Store, session string, t *target, current snapshots.View, unseen []int, known map[int]bool) *Refusal {
	header := fmt.Sprintf("This edit anchors to lines %s of %s that [%s#%s] never displayed in full (partial read, truncated line, or rows elided from an edit response).", ranges(unseen), t.display, t.display, current.Tag)
	if store.Forgotten(session, t.canonical) {
		header = fmt.Sprintf("This edit anchors to lines %s of %s that this agent was not shown since nib dropped its earlier views of the file: nib keeps the views of a limited number of files across all agents, and dropped this one's to make room, so lines read before then no longer count.", ranges(unseen), t.display)
	}
	long := len(unseen) > revealLines
	for _, line := range unseen {
		content := ""
		if line-1 < len(current.Lines) {
			content = current.Lines[line-1]
		}
		if js.Len(content) > revealColumns || visible(content) != content {
			long = true
		}
	}
	if long {
		first := unseen[:min(len(unseen), revealLines)]
		f := contextRows(current.Lines, first, 0, revealColumns)
		return &Refusal{message: fmt.Sprintf("%s Preview of the first %d unseen line(s):\n%s\nRead the remaining lines in full before re-issuing the edit; change a cut or ansi line in place with SUB N: OLD.", header, len(first), strings.Join(f.rows, "\n"))}
	}
	f := contextRows(current.Lines, unseen, revealContext, revealColumns)
	store.Record(session, t.canonical, current, nonNil(f.shown), nil)
	written := ""
	for _, line := range unseen {
		if known[line] {
			written = " Lines you wrote, or saw before an edit moved them, can skip this step: a ~TEXT row with the start of the line, right under the PUT or CUT line, confirms it."
			break
		}
	}
	return &Refusal{message: fmt.Sprintf("%s Actual file content (* marks those lines):\n%s\nIf these are the lines you meant, the same patch now succeeds; otherwise fix your line numbers.%s", header, strings.Join(f.rows, "\n"), written), Retry: true}
}

func mismatchError(store *snapshots.Store, session string, t *target, current snapshots.View, tagValue string, recognized bool, anchors []int, appendOnly bool) error {
	f := contextRows(current.Lines, anchors, 2, LineLimit)
	store.Record(session, t.canonical, current, nonNil(f.shown), nil)
	var lines []string
	if recognized {
		lines = []string{
			fmt.Sprintf("Edit rejected for %s: file changed between read and edit and the edit could not be rebased safely.", t.display),
			fmt.Sprintf("Tag #%s is stale; the current file is [%s#%s]. If a prior edit in this session modified this file, use the tag and numbering from that edit's response; otherwise read the file again.", tagValue, t.display, current.Tag),
		}
	} else {
		lines = []string{fmt.Sprintf("Edit rejected for %s: tag #%s was not issued for this file in this session.", t.display, tagValue)}
		for _, origin := range store.Origins(session, tagValue, t.canonical) {
			lines = append(lines, fmt.Sprintf("Tag #%s was issued for %s.", tagValue, origin))
		}
		lines = append(lines, fmt.Sprintf("The current file is [%s#%s]. Read the file to get a current tag; never invent tags.", t.display, current.Tag))
		if appendOnly {
			lines = append(lines, fmt.Sprintf("To append without a tag, use a [%s] header with only PUT >$: hunks.", t.display))
		}
	}
	if len(f.rows) > 0 {
		lines = append(lines, "", "Current content around the requested lines (* marks them):")
		lines = append(lines, f.rows...)
	}
	return errors.New(strings.Join(lines, "\n"))
}

type recorder func(shown []int)

// confirmChecks marks the lines whose ~ check rows match: a check is the start of the line after its indentation, and an empty one matches a blank line.
func confirmChecks(named, display string, lines []string, edits []snapshots.LineEdit, record recorder) (checked, error) {
	type failure struct {
		line  int
		check string
	}
	var failures []failure
	var confirmed []snapshots.LineEdit
	for _, edit := range edits {
		if !edit.HasChecks {
			confirmed = append(confirmed, edit)
			continue
		}
		targets := []int{edit.Start}
		if len(edit.Checks) == 2 {
			targets = []int{edit.Start, edit.End}
		}
		matched := []int{}
		before := len(failures)
		for index, check := range edit.Checks {
			line := targets[index]
			if line > len(lines) {
				return checked{}, fmt.Errorf("Line %d does not exist in %s (file has %d lines)", line, display, len(lines))
			}
			wanted := js.Trim(check)
			content := lines[line-1]
			ok := js.Trim(content) == ""
			if wanted != "" {
				ok = strings.HasPrefix(js.TrimStart(content), wanted)
			}
			if ok {
				matched = append(matched, line)
			} else {
				failures = append(failures, failure{line, wanted})
			}
		}
		if len(failures) == before {
			edit.Confirmed = matched
			confirmed = append(confirmed, edit)
		}
	}
	if len(failures) == 0 {
		return checked{edits: confirmed}, nil
	}
	marked := make([]int, len(failures))
	for i, f := range failures {
		marked[i] = f.line
	}
	f := contextRows(lines, marked, revealContext, revealColumns)
	if record != nil {
		record(nonNil(f.shown))
	}
	message := []string{named + ": ~ check rows do not match, so these line numbers are off:"}
	for _, f := range failures {
		if f.check != "" {
			message = append(message, fmt.Sprintf("line %d does not start with %s", f.line, js.Quote(f.check)))
		} else {
			message = append(message, fmt.Sprintf("line %d is not blank", f.line))
		}
	}
	message = append(message, "", fmt.Sprintf("Lines of %s around them (* marks them):", named))
	message = append(message, f.rows...)
	message = append(message, rowNotes(display, f.marks)...)
	return checked{edits: confirmed, refusal: &Refusal{message: strings.Join(message, "\n")}}, nil
}

// resolveSubs turns SUB edits into one replace per line; each old text must occur exactly once in that line, and two on one line must not overlap.
// Old text that starts where a line's indentation ends, replaced by rows whose first one repeats that indentation, gets it once, with a lesson:
// SUB keeps the text around its old text, which models forget. A SUB whose old text is on a nearby line instead applies there when subTarget
// finds that line certain, with a lesson naming both.
func resolveSubs(named, display string, lines []string, edits []snapshots.LineEdit, known func(line int) bool, record recorder) (checked, error) {
	if !slices.ContainsFunc(edits, func(e snapshots.LineEdit) bool { return e.Sub != nil }) {
		return checked{edits: edits}, nil
	}
	type failure struct {
		line    int
		message string
	}
	var failures []failure
	done := map[int]bool{}
	var doneOrder []int
	var resolved []snapshots.LineEdit
	var lessons []string
	edits = slices.Clone(edits)
	for i, edit := range edits {
		if edit.Sub == nil || edit.Start > len(lines) || strings.Contains(lines[edit.Start-1], *edit.Sub) {
			continue
		}
		if to, ok := subTarget(lines, edits, i, known); ok {
			lessons = append(lessons, fmt.Sprintf("SUB %d: its old text is not on line %d but on line %d, the one line of the file that holds it, so it applied there; take line numbers from the rows you were shown.", edit.Start, edit.Start, to))
			edits[i].Start, edits[i].End = to, to
		}
	}
	failedLine := func(line int) bool {
		return slices.ContainsFunc(failures, func(f failure) bool { return f.line == line })
	}
	for _, edit := range edits {
		if edit.Sub == nil {
			resolved = append(resolved, edit)
			continue
		}
		line := edit.Start
		if done[line] {
			continue
		}
		done[line] = true
		doneOrder = append(doneOrder, line)
		if line > len(lines) {
			return checked{}, fmt.Errorf("Line %d does not exist in %s (file has %d lines)", line, display, len(lines))
		}
		content := lines[line-1]
		type spanned struct {
			at      int
			old     string
			content []string
		}
		var spans []spanned
		for _, other := range edits {
			if other.Sub == nil || other.Start != line {
				continue
			}
			old := *other.Sub
			at := strings.Index(content, old)
			switch {
			case at < 0:
				failures = append(failures, failure{line, fmt.Sprintf("SUB %d: %s does not occur on line %d", line, js.Quote(old), line)})
			case strings.Contains(content[at+1:], old):
				failures = append(failures, failure{line, fmt.Sprintf("SUB %d: %s occurs more than once on line %d; quote more of the line", line, js.Quote(old), line)})
			default:
				replacement := other.Content
				if indent := content[:at]; at > 0 && strings.Trim(indent, " \t") == "" && len(replacement) > 0 && strings.HasPrefix(replacement[0], indent) {
					replacement = append([]string{replacement[0][len(indent):]}, replacement[1:]...)
					lessons = append(lessons, fmt.Sprintf("SUB %d: the + rows repeated the line's indentation, which SUB keeps around its old text, so it was written once; quote only the old text and its replacement.", line))
				}
				spans = append(spans, spanned{at, old, replacement})
			}
		}
		if failedLine(line) {
			continue
		}
		slices.SortStableFunc(spans, func(a, b spanned) int { return a.at - b.at })
		overlap := false
		for i := 1; i < len(spans); i++ {
			if spans[i].at < spans[i-1].at+len(spans[i-1].old) {
				overlap = true
			}
		}
		if overlap {
			failures = append(failures, failure{line, fmt.Sprintf("SUB %d: two old texts overlap on line %d; merge them into one SUB", line, line)})
			continue
		}
		var composed strings.Builder
		cursor := 0
		for _, s := range spans {
			composed.WriteString(content[cursor:s.at])
			composed.WriteString(strings.Join(s.content, "\n"))
			cursor = s.at + len(s.old)
		}
		composed.WriteString(content[cursor:])
		resolved = append(resolved, snapshots.LineEdit{Start: line, End: line, Operation: snapshots.Replace, Content: strings.Split(composed.String(), "\n"), Quoted: true})
	}
	if len(failures) == 0 {
		return checked{edits: resolved, lessons: lessons}, nil
	}
	var marked []int
	for _, line := range doneOrder {
		if failedLine(line) {
			marked = append(marked, line)
		}
	}
	f := contextRows(lines, marked, revealContext, revealColumns)
	if record != nil {
		record(nonNil(f.shown))
	}
	message := []string{named + ": SUB refused:"}
	for _, f := range failures {
		message = append(message, f.message)
	}
	message = append(message, "", fmt.Sprintf("Lines of %s around them (* marks them):", named))
	message = append(message, f.rows...)
	message = append(message, rowNotes(display, f.marks)...)
	return checked{edits: resolved, refusal: &Refusal{message: strings.Join(message, "\n")}, lessons: lessons}, nil
}

// subReach is how far from its line a SUB's old text may be and still be taken as meant there.
const subReach = 2

// subTarget is the line a SUB whose old text is not on its own line meant: the one line of the file holding that text, long enough to tell
// (snapshots.EvidenceMinRunes), at most subReach lines away, known to the agent and anchored by no other hunk; ok is false when any of that
// is in doubt, so the SUB is refused as before.
func subTarget(lines []string, edits []snapshots.LineEdit, index int, known func(line int) bool) (int, bool) {
	edit := edits[index]
	old := *edit.Sub
	if utf8.RuneCountInString(js.Trim(old)) < snapshots.EvidenceMinRunes {
		return 0, false
	}
	found := 0
	for n, line := range lines {
		if strings.Contains(line, old) {
			if found != 0 {
				return 0, false
			}
			found = n + 1
		}
	}
	if found == 0 || found < edit.Start-subReach || found > edit.Start+subReach || !known(found) {
		return 0, false
	}
	for j, other := range edits {
		if j != index && other.Sub == nil && (slices.Contains(snapshots.AnchorLines(other), found) || other.Start <= found && found <= other.End) {
			return 0, false
		}
	}
	return found, true
}

func trailing(line string) string { return strings.TrimRight(line, " \t\r") }

// confirmOld checks diff-style - rows against the lines their hunk replaces; matching rows quote those lines, so like SUB they need no earlier read.
func confirmOld(named, display string, lines []string, edits []snapshots.LineEdit, record recorder) checked {
	if !slices.ContainsFunc(edits, func(e snapshots.LineEdit) bool { return len(e.Old) > 0 }) {
		return checked{edits: edits}
	}
	type failure struct {
		line    int
		message string
	}
	var failures []failure
	var result []snapshots.LineEdit
	for _, edit := range edits {
		if len(edit.Old) == 0 {
			result = append(result, edit)
			continue
		}
		before := len(failures)
		span := fmt.Sprintf("lines %d-%d", edit.Start, edit.End)
		if edit.Start == edit.End {
			span = fmt.Sprintf("line %d", edit.Start)
		}
		var replaced []string
		if edit.Start-1 < len(lines) {
			replaced = lines[edit.Start-1 : min(edit.End, len(lines))]
		}
		switch {
		case edit.End > len(lines):
			failures = append(failures, failure{min(edit.Start, len(lines)), fmt.Sprintf("%s do not all exist (the file has %d lines)", span, len(lines))})
		case len(edit.Old) != len(replaced):
			plural := "s"
			if len(edit.Old) == 1 {
				plural = ""
			}
			failures = append(failures, failure{edit.Start, fmt.Sprintf("the - rows under %s quote %d line%s, but the hunk replaces %d", span, len(edit.Old), plural, len(replaced))})
		default:
			for offset, quote := range edit.Old {
				actual := replaced[offset]
				if trailing(quote) != trailing(actual) && trailing(quote) != trailing(visible(actual)) {
					failures = append(failures, failure{edit.Start + offset, fmt.Sprintf("line %d is not %s", edit.Start+offset, js.Quote(quote))})
				}
			}
		}
		if len(failures) == before {
			edit.Quoted = true
			result = append(result, edit)
		}
	}
	if len(failures) == 0 {
		return checked{edits: result}
	}
	marked := make([]int, len(failures))
	for i, f := range failures {
		marked[i] = f.line
	}
	f := contextRows(lines, marked, revealContext, revealColumns)
	if record != nil {
		record(nonNil(f.shown))
	}
	message := []string{named + ": the - rows do not match the file:"}
	for _, f := range failures {
		message = append(message, f.message)
	}
	message = append(message, "", fmt.Sprintf("Lines of %s around them (* marks them):", named))
	message = append(message, f.rows...)
	message = append(message, rowNotes(display, f.marks)...)
	message = append(message, "", "nib needs no - rows: PUT N.=M: with only + rows replaces those lines.")
	return checked{edits: result, refusal: &Refusal{message: strings.Join(message, "\n")}}
}

// writtenLessons is a note for each hunk header the parser read as nib's own form, so the next patch writes that form.
func writtenLessons(edits []snapshots.LineEdit) []string {
	var notes []string
	for _, edit := range edits {
		if edit.Written != "" {
			notes = append(notes, fmt.Sprintf("%s was read as PUT %d.=%d:; a range is written N.=M, with a dot.", edit.Written, edit.Start, edit.End))
		}
	}
	return notes
}

// oldRowsLesson is the note for a patch whose - rows matched: the edit went through, but those rows cost tokens nib never needs.
func oldRowsLesson(edits []snapshots.LineEdit) []string {
	index := slices.IndexFunc(edits, func(e snapshots.LineEdit) bool { return len(e.Old) > 0 })
	if index < 0 {
		return nil
	}
	edit := edits[index]
	var header string
	switch {
	case len(edit.Content) > 0 && edit.Start == edit.End:
		header = fmt.Sprintf("PUT %d:", edit.Start)
	case len(edit.Content) > 0:
		header = fmt.Sprintf("PUT %d.=%d:", edit.Start, edit.End)
	case edit.Start == edit.End:
		header = fmt.Sprintf("CUT %d", edit.Start)
	default:
		header = fmt.Sprintf("CUT %d.=%d", edit.Start, edit.End)
	}
	lines := []string{
		"The - rows matched the file, so the edit was applied; but nib never needs the old text, since the line numbers already say what a hunk replaces.",
		"Leave - rows out next time. This hunk is just:",
		header,
	}
	for _, line := range edit.Content[:min(3, len(edit.Content))] {
		if js.Len(line) > 120 {
			line = js.Slice(line, 0, 120) + "..."
		}
		lines = append(lines, "+"+line)
	}
	if len(edit.Content) > 3 {
		lines = append(lines, fmt.Sprintf("(and %d more + rows)", len(edit.Content)-3))
	}
	return []string{strings.Join(lines, "\n")}
}

// changeRow is one row of an edited file; origin is the 0-based index of the line it kept, -1 for a new row; was is the line a one-for-one
// replacement overwrote, so a long changed line can show where it changed.
type changeRow struct {
	raw    string
	origin int
	opaque bool
	was    *string
}

type run struct{ at, length int }

type applied struct {
	rows       []changeRow
	runs       []run
	bytes      []byte
	terminated bool
}

type rawEdit struct {
	snapshots.LineEdit
	raw []string
}

// applyChanges applies sorted edits to a decoded file; rows keep their original index so the response can carry seen lines and number the changes.
func applyChanges(source *text, changes []rawEdit) applied {
	var rows []changeRow
	var runs []run
	keep := func(from, to int) {
		for index := from; index < to; index++ {
			if source.lines[index] != "" {
				rows = append(rows, changeRow{raw: source.lines[index], origin: index})
			}
		}
	}
	cursor := 0
	for _, change := range changes {
		at := change.Start - 1
		if change.Operation == snapshots.After {
			at = change.End
		}
		keep(cursor, at)
		runs = append(runs, run{len(rows), len(change.raw)})
		// A replacement with as many rows as lines it replaces pairs them up, so each new row knows the line it overwrote.
		paired := change.Operation == snapshots.Replace && len(change.raw) == change.End-change.Start+1
		for offset, raw := range change.raw {
			r := changeRow{raw: raw, origin: -1, opaque: change.Opaque}
			if paired {
				was := lineContent(source.lines[change.Start-1+offset])
				r.was = &was
			}
			rows = append(rows, r)
		}
		cursor = at
		if change.Operation == snapshots.Replace {
			cursor += change.End - change.Start + 1
		}
	}
	keep(cursor, len(source.lines))
	// A file without a final newline that gets an empty last row has gained its final newline, not a line: the row ends the line before it.
	terminated := source.body != "" && !strings.HasSuffix(source.body, "\n") && len(rows) > 1 && rows[len(rows)-1].origin < 0 && lineContent(rows[len(rows)-1].raw) == ""
	if terminated {
		last := len(rows) - 1
		rows = rows[:last]
		// The row belongs to whichever run wrote it, and a deletion after it now sits at the new end.
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].at <= last && last < runs[i].at+runs[i].length {
				runs[i].length--
				break
			}
		}
		for i := range runs {
			runs[i].at = min(runs[i].at, len(rows))
		}
	}
	// An unterminated original last line needs a separator when inserting after it.
	var body strings.Builder
	for index, r := range rows {
		body.WriteString(r.raw)
		if index < len(rows)-1 && !strings.HasSuffix(r.raw, "\n") {
			body.WriteString(source.ending)
		}
	}
	joined := body.String()
	final := stripFinalNewline(joined)
	switch {
	case terminated:
		final += source.ending
	case joined != "":
		final += finalNewline(source.body)
	}
	return applied{rows: rows, runs: runs, bytes: []byte(source.bom + final), terminated: terminated}
}

func stripFinalNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return s[:len(s)-2]
	}
	return strings.TrimSuffix(s, "\n")
}

func finalNewline(s string) string {
	if strings.HasSuffix(s, "\r\n") {
		return "\r\n"
	}
	if strings.HasSuffix(s, "\n") {
		return "\n"
	}
	return ""
}

const (
	// finalNewlineHint is how a file without a final newline gets one; edits keep a file's final-newline state otherwise.
	finalNewlineHint = "PUT >$: with a lone + row adds one"
	// movedLesson is told once per agent: a moved line is anchored without a read, by a check of its start at its new number.
	movedLesson = "[a line you saw or wrote that an edit moved is anchored by a ~TEXT row with its start under the PUT or CUT; no read needed]"
)

var lfOnly = regexp.MustCompile(`(^|[^\r])\n`)

// inStyleOf is content for a whole-file overwrite in the file's own style: plain line breaks become CRLF in a file that uses only CRLF, and its
// BOM stays, unless the content brings its own.
func inStyleOf(current *text, content string) (string, []string) {
	var kept []string
	styled := content
	if strings.Contains(current.body, "\r\n") && !lfOnly.MatchString(current.body) && strings.Contains(styled, "\n") && !strings.Contains(styled, "\r\n") {
		styled = strings.ReplaceAll(styled, "\n", "\r\n")
		kept = append(kept, "CRLF line endings")
	}
	if current.bom != "" && !strings.HasPrefix(styled, bom) {
		styled = bom + styled
		kept = append(kept, "BOM")
	}
	return styled, kept
}

const (
	alikeThreshold = 0.75
	alikeMinChars  = 12
)

// alike says how alike two lines are, from 0 to 1: their common start and end, indentation aside, against the longer one.
func alike(a, b string) float64 {
	x, y := js.Units(js.Trim(a)), js.Units(js.Trim(b))
	if min(len(x), len(y)) < alikeMinChars {
		return 0
	}
	start := 0
	for start < len(x) && start < len(y) && x[start] == y[start] {
		start++
	}
	end := 0
	for end < len(x)-start && end < len(y)-start && x[len(x)-1-end] == y[len(y)-1-end] {
		end++
	}
	return float64(start+end) / float64(max(len(x), len(y)))
}

// offByOne warns about replaced ranges that look one line off: the kept line next to the new rows nearly matches the new edge row. Each warning
// carries the hunks that undo the slip, since a model told only that a range was off tends to delete the wrong line.
func offByOne(source *text, edits []snapshots.LineEdit, result applied) []string {
	kept := func(index int) (string, bool) {
		if index < 0 || index >= len(result.rows) || result.rows[index].origin < 0 {
			return "", false
		}
		return lineContent(result.rows[index].raw), true
	}
	var warnings []string
	for index, edit := range edits {
		if edit.Operation != snapshots.Replace || len(edit.Content) == 0 || edit.Quoted {
			continue
		}
		at, length := result.runs[index].at, result.runs[index].length
		first, last := edit.Content[0], edit.Content[len(edit.Content)-1]
		if above, ok := kept(at - 1); ok && alike(above, first) >= alikeThreshold && alike(above, first) > alike(lineContent(source.lines[edit.Start-1]), first) {
			// Shifted down by one, the range kept the line it meant to replace and overwrote the line after its end.
			overwritten := lineContent(source.lines[edit.End-1])
			warnings = append(warnings, fmt.Sprintf("Lines %d and %d now nearly match: line %d was kept just above an edit and line %d is the edit's first new line. If the edit was meant to replace line %d, its range started one line late and overwrote the line below it instead; these hunks, with the new tag, delete line %d and put that line back:\nCUT %d\nPUT >%d:\n+%s", at, at+1, at, at+1, at, at, at, at+length, overwritten))
		}
		if below, ok := kept(at + length); ok && alike(below, last) >= alikeThreshold && alike(below, last) > alike(lineContent(source.lines[edit.End-1]), last) {
			warnings = append(warnings, fmt.Sprintf("Lines %d and %d now nearly match: line %d is an edit's last new line and line %d was kept just below it. If the edit was meant to replace line %d, its range ended one line early; this hunk, with the new tag, deletes it:\nCUT %d", at+length, at+length+1, at+length, at+length+1, at+length+1, at+length+1))
		}
	}
	return warnings
}

// brackets are the ( [ { a line's code opens minus the ) ] } it closes.
type brackets [3]int

func (b brackets) add(other brackets) brackets {
	return brackets{b[0] + other[0], b[1] + other[1], b[2] + other[2]}
}

// bracketsOf counts each line's brackets, with strings, comments and regex literals left out.
func bracketsOf(lines []string) []brackets {
	counts := make([]brackets, len(lines))
	scanCode(lines, func(index int, char byte) {
		if kind := strings.IndexByte("([{", char); kind >= 0 {
			counts[index][kind]++
		} else if kind := strings.IndexByte(")]}", char); kind >= 0 {
			counts[index][kind]--
		}
	})
	return counts
}

// templateBrackets counts each line's brackets in a Hawkeye template, leaving out its comments, {# #} and <!-- -->, which may span lines. A
// template's text is prose, so a quote opens no string there: an apostrophe in "today's" would hide every bracket after it from bracketsOf.
func templateBrackets(lines []string) []brackets {
	counts := make([]brackets, len(lines))
	closer := ""
	for index, line := range lines {
		for at := 0; at < len(line); at++ {
			switch {
			case closer != "":
				end := strings.Index(line[at:], closer)
				if end < 0 {
					at = len(line)
					continue
				}
				at += end + len(closer) - 1
				closer = ""
			case strings.HasPrefix(line[at:], "{#"):
				closer, at = "#}", at+1
			case strings.HasPrefix(line[at:], "<!--"):
				closer, at = "-->", at+3
			default:
				if kind := strings.IndexByte("([{", line[at]); kind >= 0 {
					counts[index][kind]++
				} else if kind := strings.IndexByte(")]}", line[at]); kind >= 0 {
					counts[index][kind]--
				}
			}
		}
	}
	return counts
}

// describe says how a bracket count is off from balance, like "1 more } than {".
func (b brackets) describe() string {
	var parts []string
	for kind, n := range b {
		opener, closer := "([{"[kind:kind+1], ")]}"[kind:kind+1]
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d more %s than %s", n, opener, closer))
		} else if n < 0 {
			parts = append(parts, fmt.Sprintf("%d more %s than %s", -n, closer, opener))
		}
	}
	return strings.Join(parts, " and ")
}

// sourceLines are a file's lines before an edit, without their line endings.
func sourceLines(source *text) []string {
	lines := make([]string, len(source.lines))
	for i, line := range source.lines {
		lines[i] = lineContent(line)
	}
	return lines
}

// appliedLines are a file's lines after an edit, without their line endings.
func appliedLines(result applied) []string {
	lines := make([]string, len(result.rows))
	for i, r := range result.rows {
		lines[i] = lineContent(r.raw)
	}
	return lines
}

// unbalanced warns when an edit leaves a file whose brackets balanced unbalanced, naming the hunks whose new rows changed the count; a closing
// line left just outside a replaced range is too short for offByOne to compare. Files that never balanced, like shell case labels, are left alone.
func unbalanced(file string, source *text, edits []snapshots.LineEdit, result applied) []string {
	before, after := sourceLines(source), appliedLines(result)
	count := bracketsOf
	if strings.EqualFold(filepath.Ext(file), ".hwk") {
		count = templateBrackets
	}
	old, now := count(before), count(after)
	var total, changed brackets
	for _, b := range old {
		total = total.add(b)
	}
	for _, b := range now {
		changed = changed.add(b)
	}
	if total != (brackets{}) || changed == (brackets{}) {
		return nil
	}
	warning := fmt.Sprintf("Brackets no longer balance: before this edit they did, and now the file has %s.", changed.describe())
	for index, edit := range edits {
		var removed, added brackets
		if edit.Operation == snapshots.Replace {
			for line := edit.Start; line <= edit.End; line++ {
				removed = removed.add(old[line-1])
			}
		}
		run := result.runs[index]
		for row := run.at; row < run.at+run.length; row++ {
			added = added.add(now[row])
		}
		delta := brackets{added[0] - removed[0], added[1] - removed[1], added[2] - removed[2]}
		if delta == (brackets{}) {
			continue
		}
		rows := fmt.Sprintf("line %d has", run.at+1)
		if run.length > 1 {
			rows = fmt.Sprintf("lines %d-%d have", run.at+1, run.at+run.length)
		}
		if run.length == 0 {
			warning += fmt.Sprintf(" The deletion at line %d removed %s.", run.at+1, brackets{-delta[0], -delta[1], -delta[2]}.describe())
			continue
		}
		warning += fmt.Sprintf(" The new %s %s, counted against the lines they replaced; check them and the lines just outside them.", rows, delta.describe())
	}
	return []string{warning}
}

type previewed struct {
	output []string
	shown  []int
	marks  []marks
	// moved is set when a note says lines the agent knew moved.
	moved bool
}

func knownLine(version *snapshots.Version, line int) bool {
	return version.Seen[line] || version.Known[line]
}

// preview is the rows of an edit response: the kept row on each side of every change, the new rows whole except a sparse N~ map inside a long
// run, and one note per stretch of moved lines the agent knew, with their old numbers.
func preview(rows []changeRow, runs []run, prior *snapshots.Version, limit int) previewed {
	full := map[int]bool{}
	mapped := map[int]bool{}
	var contents []string
	lines := func() []string {
		if contents == nil {
			contents = make([]string, len(rows))
			for i, r := range rows {
				contents[i] = lineContent(r.raw)
			}
		}
		return contents
	}
	kept := func(index int) bool { return index >= 0 && index < len(rows) && rows[index].origin >= 0 }
	var strided []int
	for _, r := range runs {
		for index := r.at - editContext; index < r.at; index++ {
			if kept(index) {
				full[index] = true
			}
		}
		for index := r.at + r.length; index < r.at+r.length+editContext; index++ {
			if kept(index) {
				full[index] = true
			}
		}
		if r.length <= writtenRows {
			for index := r.at; index < r.at+r.length; index++ {
				full[index] = true
			}
			continue
		}
		full[r.at] = true
		full[r.at+r.length-1] = true
		after := r.at + 1
		for _, line := range mapLines(lines(), r.at+2, r.at+r.length-1, mapOptions{after: &after}) {
			if !rows[line-1].opaque {
				strided = append(strided, line-1)
			}
		}
	}
	for _, index := range strided[:min(len(strided), editMapRows)] {
		mapped[index] = true
	}
	knew := func(index int) bool {
		origin := rows[index].origin
		return origin >= 0 && prior != nil && prior.Seen != nil && knownLine(prior, origin+1)
	}
	// Kept rows whose number changed, in stretches that moved by one offset, get one note each when they hold a line the agent knew.
	notes := map[int]string{}
	type stretch struct {
		from, to, offset int
		known            bool
	}
	var current *stretch
	closeStretch := func() {
		if current == nil {
			return
		}
		from, to := current.from, current.to
		for from <= to && (full[from] || mapped[from]) {
			from++
		}
		for to >= from && (full[to] || mapped[to]) {
			to--
		}
		offset, known := current.offset, current.known
		current = nil
		if !known || from > to {
			return
		}
		spanText := func(low, high int) string {
			if low == high {
				return itoa(low)
			}
			return fmt.Sprintf("%d-%d", low, high)
		}
		many := from < to
		s, verb := "", "is"
		if many {
			s, verb = "s", "are"
		}
		direction := "down"
		if offset < 0 {
			direction = "up"
		}
		notes[from] = fmt.Sprintf("[line%s %s %s old line%s %s, moved %s by %d]", s, spanText(from+1, to+1), verb, s, spanText(from+1-offset, to+1-offset), direction, abs(offset))
	}
	for index, r := range rows {
		offset := 0
		if r.origin >= 0 {
			offset = index - r.origin
		}
		if offset == 0 {
			closeStretch()
			continue
		}
		if current != nil && current.to == index-1 && current.offset == offset {
			current.to = index
			current.known = current.known || knew(index)
			continue
		}
		closeStretch()
		current = &stretch{index, index, offset, knew(index)}
	}
	closeStretch()
	indexes := map[int]bool{}
	for k := range full {
		indexes[k] = true
	}
	for k := range mapped {
		indexes[k] = true
	}
	for k := range notes {
		indexes[k] = true
	}
	var out previewed
	previous := -1
	previousFull := false
	for _, index := range sortedSet(indexes) {
		whole := full[index]
		// Map rows and notes carry their own numbers, so only a gap between two full rows gets an ellipsis.
		if note, ok := notes[index]; ok {
			out.output = append(out.output, note)
		} else if whole && previousFull && index > previous+1 {
			out.output = append(out.output, "...")
		}
		r := rows[index]
		if whole {
			content := lineContent(r.raw)
			var w *windowed
			if r.was != nil && js.Len(visible(content)) > limit {
				w = matchWindows(content, []span{changedSpan(*r.was, content)}, limit)
			}
			if w != nil {
				out.output = append(out.output, fmt.Sprintf("[%d %s]|%s", index+1, w.label, w.text))
				out.marks = append(out.marks, marks{cut: true})
			} else {
				line := lineRow(index+1, content, limit)
				out.output = append(out.output, line.text)
				if line.anchor {
					out.shown = append(out.shown, index+1)
				} else {
					out.marks = append(out.marks, line.marks)
				}
			}
		} else if mapped[index] {
			out.output = append(out.output, fmt.Sprintf("%d~%s", index+1, clip(lines()[index])))
			out.shown = append(out.shown, index+1)
		}
		previous = index
		previousFull = whole
	}
	out.moved = len(notes) > 0
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// orderEdits sorts edits into file order and rejects reversed or overlapping ones. In doubled coordinates line L is 2L and the gap after it
// 2L + 1, so an insert sits between the lines it separates.
func orderEdits(edits []snapshots.LineEdit) ([]snapshots.LineEdit, error) {
	for _, edit := range edits {
		if edit.End < edit.Start {
			return nil, fmt.Errorf("Range %d.=%d is reversed", edit.Start, edit.End)
		}
	}
	const infinity = 1 << 62
	from := func(e snapshots.LineEdit) int {
		switch {
		case e.Edge:
			return infinity
		case e.Operation == snapshots.Before:
			return 2*e.Start - 1
		case e.Operation == snapshots.After:
			return 2*e.End + 1
		}
		return 2 * e.Start
	}
	to := func(e snapshots.LineEdit) int {
		if e.Operation == snapshots.Replace {
			return 2 * e.End
		}
		return from(e)
	}
	rank := func(e snapshots.LineEdit) int {
		if e.Operation == snapshots.After {
			return 0
		}
		return 1
	}
	sorted := slices.Clone(edits)
	slices.SortStableFunc(sorted, func(a, b snapshots.LineEdit) int {
		if from(a) != from(b) {
			if from(a) < from(b) {
				return -1
			}
			return 1
		}
		return rank(a) - rank(b)
	})
	reach := 0
	var last *snapshots.LineEdit
	for i := range sorted {
		edit := sorted[i]
		start := from(edit)
		if start < reach || (start == reach && !(last != nil && last.Operation == snapshots.After && edit.Operation == snapshots.Before)) {
			where := fmt.Sprintf("line %d", edit.Start)
			if edit.Edge {
				where = "the end of the file"
			}
			if start == reach && (last == nil || last.Operation != snapshots.Replace) {
				return nil, fmt.Errorf("Edits overlap at %s: two hunks insert at the same place; put their + rows into one hunk", where)
			}
			return nil, fmt.Errorf("Edits overlap at %s: a line can be replaced by one hunk only, and an insert may only touch a replaced range at its edges", where)
		}
		reach = to(edit)
		last = &sorted[i]
	}
	size := 0
	for _, edit := range edits {
		for _, line := range edit.Content {
			size += len(line) + 1
		}
	}
	if size > TextLimit {
		return nil, fmt.Errorf("Combined edit content exceeds %d byte size limit", TextLimit)
	}
	return sorted, nil
}

// request is one file a call changes: an edit of an existing file, or a create (or overwrite, or the destination of a move or copy).
type request struct {
	create      bool
	filePath    string
	tag         *string
	edits       []snapshots.LineEdit
	remove      bool
	move, copy  string
	content     string
	overwrite   string
	destination bool
}

func (r *request) action() string {
	switch {
	case r.create && r.overwrite != "":
		return "overwrite"
	case r.create:
		return "create"
	case r.remove:
		return "delete"
	case r.move != "":
		return "move"
	case r.copy != "":
		return "copy"
	}
	return "edit"
}

func (r *request) to() string {
	if r.create {
		return ""
	}
	if r.move != "" {
		return r.move
	}
	return r.copy
}

type handle struct {
	request     *request
	target      *target
	parent      *parent
	file        *os.File
	destination *handle
}

// locked takes locks in canonical order so concurrent multi-file patches cannot deadlock; handles stay open until run returns.
// With failed, a request whose path cannot be resolved is reported there and left out instead of ending the call.
func locked[T any](ctx *Context, requests []*request, runWith func(handles []*handle) (T, error), failed func(r *request, err error)) (T, error) {
	var zero T
	var resolved []*handle
	// A moved file and its destination are claimed together, so a refused pair leaves neither behind.
	claim := func(handles ...*handle) error {
		for index, h := range handles {
			for _, other := range append(slices.Clone(resolved), handles[:index]...) {
				if other.target.canonical == h.target.canonical {
					return fmt.Errorf("%s and %s resolve to the same file %s; merge them into one section", other.request.filePath, h.request.filePath, h.target.canonical)
				}
			}
		}
		resolved = append(resolved, handles...)
		return nil
	}
	authorize := func(canonical, action string) error {
		if err := authorizeWrite(ctx, canonical, action); err != nil {
			return &denied{err}
		}
		return nil
	}
	// prepare creates the parents of a path the checks above passed, then authorizes the canonical path that symlinks may redirect to.
	prepare := func(filePath, action string, create bool) (*target, error) {
		requested := ctx.resolve(filePath)
		if create {
			if err := os.MkdirAll(filepath.Dir(requested), 0o777); err != nil {
				return nil, err
			}
		}
		t, err := resolveTarget(filePath, ctx, create)
		if err != nil {
			return nil, err
		}
		if t.canonical != requested {
			if err := authorize(t.canonical, action); err != nil {
				return nil, err
			}
		}
		return t, nil
	}
	pathsOf := func(r *request) [][2]string {
		paths := [][2]string{{r.filePath, r.action()}}
		if to := r.to(); to != "" {
			paths = append(paths, [2]string{to, "create"})
		}
		return paths
	}
	// Every path of the call is judged before a parent directory is created for any of them: by the sandbox first, then by the roots check.
	for _, r := range requests {
		for _, p := range pathsOf(r) {
			refused, err := outsideSandbox(ctx, p[0])
			if err != nil {
				return zero, err
			}
			if refused != "" {
				return zero, &Outside{refused}
			}
		}
	}
	for _, r := range requests {
		for _, p := range pathsOf(r) {
			if err := authorizeWrite(ctx, ctx.resolve(p[0]), p[1]); err != nil {
				return zero, err
			}
		}
	}
	for _, r := range requests {
		err := func() error {
			create := r.create && r.overwrite == ""
			t, err := prepare(r.filePath, r.action(), create)
			if err != nil {
				return err
			}
			h := &handle{request: r, target: t}
			if to := r.to(); to != "" {
				dt, err := prepare(to, "create", true)
				if err != nil {
					return err
				}
				h.destination = &handle{request: &request{create: true, filePath: to, destination: true}, target: dt}
			}
			if h.destination != nil {
				return claim(h, h.destination)
			}
			return claim(h)
		}()
		if err == nil {
			continue
		}
		var d *denied
		if errors.As(err, &d) {
			return zero, d.cause
		}
		// A link that leads outside the sandbox ends the call like a refused path.
		var outside *Outside
		if errors.As(err, &outside) || failed == nil || ctx.aborted() != nil {
			return zero, err
		}
		failed(r, err)
	}
	ordered := slices.Clone(resolved)
	slices.SortStableFunc(ordered, func(a, b *handle) int { return strings.Compare(a.target.canonical, b.target.canonical) })
	var step func(index int) (T, error)
	step = func(index int) (T, error) {
		if index == len(ordered) {
			var handles []*handle
			for _, h := range resolved {
				if !(h.request.create && h.request.destination) {
					handles = append(handles, h)
				}
			}
			return runWith(handles)
		}
		h := ordered[index]
		return serialized(h.target.canonical, ctx, func() (T, error) {
			return inParent(h.target, func(p *parent) (T, error) {
				h.parent = p
				if h.request.create && h.request.overwrite == "" {
					return step(index + 1)
				}
				file, err := os.OpenFile(p.location, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
				if err != nil {
					return zero, err
				}
				defer file.Close()
				if err := checkDescriptor(file, p.location, h.target.canonical); err != nil {
					return zero, err
				}
				h.file = file
				return step(index + 1)
			})
		})
	}
	return step(0)
}

const (
	recoveryRemapped = "Recovered by remapping stale line numbers to unchanged current lines (file changed since the tagged read). Verify the diff matches your intent."
	recoveryExternal = "Recovered from a stale tag using the tagged read (file changed externally between read and edit)."
	recoveryChain    = "Recovered from a stale tag using an earlier read (a prior edit in this session advanced the tag)."
	tailDrift        = "Applied the end-of-file insert despite a stale tag (file changed since the tagged read). Read again if that change was unexpected."
)

func unknownTag(tagValue string) string {
	return fmt.Sprintf("Tag #%s was never issued for this file; never invent tags. The edit was applied only because every hunk is safe without one: SUB and matching - rows quote what they replace, and PUT >$: only appends. Any other hunk would have been refused; use the tag in the header above.", tagValue)
}

// tagMatches is true when tagValue names the live content; a retained version with the same 4-hex tag but other text means a collision.
func tagMatches(versions []*snapshots.Version, live snapshots.View, tagValue string) bool {
	if live.Tag != tagValue {
		return false
	}
	any := false
	for _, v := range versions {
		if v.Tag == tagValue {
			any = true
			if v.Key == live.Key {
				return true
			}
		}
	}
	return !any
}

type stagedKind int

const (
	stagedEdit stagedKind = iota
	stagedNoop
	stagedRemove
	stagedCreate
)

type staged struct {
	kind     stagedKind
	handle   *handle
	current  *held
	source   *text
	live     snapshots.View
	prior    *snapshots.Version
	applied  applied
	result   *text
	warnings []string
	bytes    []byte
	created  snapshots.View
	replace  *held
	kept     []string
}

func hardlinked(h *held) bool { return statOf(h.info).Nlink > 1 }

var errHardlinked = errors.New("Hardlinked targets are not supported; refusing to break links")

func stageEdit(ctx *Context, store *snapshots.Store, h *handle) (*staged, error) {
	r, t := h.request, h.target
	if h.destination != nil {
		if err := destinationFree(h.destination); err != nil {
			return nil, err
		}
	}
	current, err := snapshot(h.file, TextLimit)
	if err != nil {
		return nil, err
	}
	if hardlinked(current) {
		return nil, errHardlinked
	}
	source, err := decodeText(current.bytes)
	if err != nil {
		return nil, err
	}
	live := viewOf(source)
	versions := store.Versions(ctx.Session, t.canonical)
	var prior *snapshots.Version
	for _, v := range versions {
		if v.Key == live.Key {
			prior = v
			break
		}
	}
	finish := func(edits []snapshots.LineEdit, warnings []string) (*staged, error) {
		changes := make([]rawEdit, len(edits))
		for i, e := range edits {
			raw := make([]string, len(e.Content))
			for j, line := range e.Content {
				raw[j] = line + source.ending
			}
			changes[i] = rawEdit{e, raw}
		}
		result := applyChanges(source, changes)
		if bytes.Equal(result.bytes, current.bytes) && r.move == "" && r.copy == "" {
			return &staged{kind: stagedNoop, handle: h, source: source, live: live, prior: prior, applied: result}, nil
		}
		decoded, err := decodeText(result.bytes)
		if err != nil {
			return nil, err
		}
		return &staged{kind: stagedEdit, handle: h, current: current, source: source, live: live, prior: prior, applied: result, result: decoded, warnings: slices.Concat(warnings, offByOne(source, edits, result), unbalanced(h.target.canonical, source, edits, result), jsonWarnings(h.target.canonical, sourceLines(source), appliedLines(result)))}, nil
	}
	// A tagless section only appends, which depends on nothing but where the file ends.
	if r.tag == nil {
		return finish(snapshots.ResolveEdges(r.edits, len(live.Lines)), nil)
	}
	tagValue := *r.tag
	var tagged []*snapshots.Version
	for _, v := range versions {
		if v.Tag == tagValue {
			tagged = append(tagged, v)
		}
	}
	matches := tagMatches(versions, live, tagValue)
	// A tag never issued for the file tells nothing about what the agent saw, but hunks that check themselves against the live file are safe without one.
	unknown := !matches && len(tagged) == 0
	if unknown {
		// An append checks nothing, so under a tag issued for another file, a likely mix-up of files, it is refused.
		origins := store.Origins(ctx.Session, tagValue, t.canonical)
		safe := func(e snapshots.LineEdit) bool {
			return e.Sub != nil || len(e.Old) > 0 || (e.Edge && len(origins) == 0)
		}
		allSafe := true
		allEdge := true
		var anchors []int
		for _, e := range r.edits {
			allSafe = allSafe && safe(e)
			allEdge = allEdge && e.Edge
			anchors = append(anchors, snapshots.AnchorLines(e)...)
		}
		if r.remove || r.move != "" || r.copy != "" || !allSafe {
			return nil, mismatchError(store, ctx.Session, t, live, tagValue, false, anchors, allEdge)
		}
		// Without a version on record the response would count every unchanged line as seen; an empty record keeps the unread ones unseen.
		if prior == nil {
			prior = store.Record(ctx.Session, t.canonical, live, []int{}, nil)
		}
	}
	if r.remove {
		if !matches {
			return nil, mismatchError(store, ctx.Session, t, live, tagValue, true, nil, false)
		}
		return &staged{kind: stagedRemove, handle: h, current: current}, nil
	}
	// Line numbers, checks and SUB texts all refer to the tagged version; only the live one may record what a refusal displays.
	var basis *snapshots.Version
	if matches || unknown {
		basis = prior
		if basis == nil {
			basis = &snapshots.Version{View: live}
		}
	} else {
		basis = tagged[0]
	}
	named := fmt.Sprintf("[%s#%s]", r.filePath, tagValue)
	var record recorder
	if matches || unknown {
		record = func(shown []int) { store.Record(ctx.Session, t.canonical, live, shown, nil) }
	}
	// A hunk past the end is a problem of its own, and the checks run on the other hunks, so one refusal names all of a section's problems.
	length := len(basis.Lines)
	past := func(e snapshots.LineEdit) bool {
		top := 0
		for _, line := range snapshots.AnchorLines(e) {
			top = max(top, line)
		}
		if e.HasChecks {
			top = max(top, e.Start, e.End)
		}
		return top > length
	}
	var beyond, within []snapshots.LineEdit
	for _, e := range r.edits {
		if past(e) {
			beyond = append(beyond, e)
		} else {
			within = append(within, e)
		}
	}
	var problems []*Refusal
	if len(beyond) > 0 {
		numbers := make([]string, len(beyond))
		for i, e := range beyond {
			top := e.Start
			for _, line := range snapshots.AnchorLines(e) {
				top = max(top, line)
			}
			numbers[i] = itoa(top)
		}
		s, verb := "", "does"
		if len(beyond) != 1 {
			s, verb = "s", "do"
		}
		problems = append(problems, &Refusal{message: fmt.Sprintf("%s: Line%s %s %s not exist in %s (file has %d lines)", named, s, strings.Join(numbers, ", "), verb, t.display, length)})
	}
	checks, err := confirmChecks(named, t.display, basis.Lines, within, record)
	if err != nil {
		return nil, err
	}
	subs, err := resolveSubs(named, t.display, basis.Lines, checks.edits, func(line int) bool { return knownLine(basis, line) }, record)
	if err != nil {
		return nil, err
	}
	marked := make([]snapshots.LineEdit, len(subs.edits))
	for i, e := range subs.edits {
		if e.Quoted && !knownLine(basis, e.Start) {
			e.Opaque = true
		}
		marked[i] = e
	}
	quotes := confirmOld(named, t.display, basis.Lines, marked, record)
	for _, refusal := range []*Refusal{checks.refusal, subs.refusal, quotes.refusal} {
		if refusal != nil {
			problems = append(problems, refusal)
		}
	}
	lessons := append(append(writtenLessons(r.edits), oldRowsLesson(r.edits)...), subs.lessons...)
	requested, err := resolveBlocks(t.canonical, quotes.edits, basis.Lines)
	if err == nil {
		requested, err = orderEdits(requested)
	}
	if err != nil {
		return nil, together(append(problems, &Refusal{message: named + ": " + err.Error()}))
	}
	var anchors []int
	for _, e := range requested {
		anchors = append(anchors, snapshots.AnchorLines(e)...)
	}
	if matches || unknown {
		edits := snapshots.ResolveEdges(requested, len(live.Lines))
		end := 0
		for _, line := range anchors {
			end = max(end, line)
		}
		if end > len(live.Lines) {
			return nil, fmt.Errorf("Line %d does not exist in %s (file has %d lines)", end, t.display, len(live.Lines))
		}
		var unseen []int
		if prior != nil {
			unseen = snapshots.Unverified(prior, requested)
		}
		if len(unseen) > 0 {
			set := map[int]bool{}
			for _, line := range unseen {
				set[line] = true
			}
			rescued := snapshots.RescueShifted(versions, live, edits, set)
			var remaining []int
			for _, line := range unseen {
				if !rescued[line] {
					remaining = append(remaining, line)
				}
			}
			if len(remaining) > 0 {
				var known map[int]bool
				if prior != nil {
					known = prior.Known
				}
				problems = append(problems, unseenError(store, ctx.Session, t, live, remaining, known))
			}
		}
		if len(problems) > 0 {
			return nil, together(problems)
		}
		if unknown {
			return finish(edits, append([]string{unknownTag(tagValue)}, lessons...))
		}
		return finish(edits, lessons)
	}
	if len(problems) > 0 {
		return nil, together(problems)
	}
	// Like oh-my-pi, edits without line anchors only depend on where the file ends, so any tag drift is tolerable.
	var offset int
	var remapped []snapshots.LineEdit
	ok := false
	switch {
	case len(anchors) == 0 && r.move == "" && r.copy == "":
		remapped, ok = snapshots.ResolveEdges(requested, len(live.Lines)), true
	case len(anchors) > 0 && len(snapshots.Unverified(basis, requested)) == 0:
		offset, remapped, ok = snapshots.RemapEdits(basis, live.Lines, requested)
	}
	if !ok {
		return nil, mismatchError(store, ctx.Session, t, live, tagValue, true, anchors, false)
	}
	note := recoveryChain
	switch {
	case len(anchors) == 0:
		note = tailDrift
	case offset != 0:
		note = recoveryRemapped
	case len(versions) > 0 && basis == versions[0]:
		note = recoveryExternal
	}
	return finish(remapped, append([]string{note}, lessons...))
}

// stageCreate checks a new file or an overwrite early; publication still links exclusively, so this only keeps a multi-file patch from failing halfway.
func stageCreate(ctx *Context, store *snapshots.Store, h *handle) (*staged, error) {
	data := []byte(h.request.content)
	if h.request.overwrite != "" {
		current, err := snapshot(h.file, TextLimit)
		if err != nil {
			return nil, err
		}
		if hardlinked(current) {
			return nil, errHardlinked
		}
		old, err := decodeText(current.bytes)
		if err != nil {
			return nil, err
		}
		live := viewOf(old)
		versions := store.Versions(ctx.Session, h.target.canonical)
		if !tagMatches(versions, live, h.request.overwrite) {
			return nil, fmt.Errorf("%s changed since tag #%s; the current file is [%s#%s]. Read it again before overwriting.", h.target.display, h.request.overwrite, h.target.display, live.Tag)
		}
		styled, kept := inStyleOf(old, h.request.content)
		data = []byte(styled)
		decoded, err := decodeText(data)
		if err != nil {
			return nil, err
		}
		return &staged{kind: stagedCreate, handle: h, bytes: data, created: viewOf(decoded), replace: current, kept: kept, warnings: jsonWarnings(h.target.canonical, viewOf(old).Lines, viewOf(decoded).Lines)}, nil
	}
	decoded, err := decodeText(data)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(h.parent.location); err == nil {
		return nil, fmt.Errorf("%s already exists; edit it with a [PATH#TAG] section, or write it with its tag to overwrite", h.target.display)
	} else if !missing(err) {
		return nil, err
	}
	return &staged{kind: stagedCreate, handle: h, bytes: data, created: viewOf(decoded), warnings: jsonWarnings(h.target.canonical, nil, viewOf(decoded).Lines)}, nil
}

func destinationFree(h *handle) error {
	if _, err := os.Lstat(h.parent.location); err == nil {
		return fmt.Errorf("Destination %s already exists; REM it in its own section first", h.target.display)
	} else if !missing(err) {
		return err
	}
	return nil
}

// unchanged fails unless the open source descriptor still holds the bytes that were staged.
func unchanged(h *handle, current *held) error {
	if err := checkDescriptor(h.file, h.parent.location, h.target.canonical); err != nil {
		return err
	}
	latest, err := snapshot(h.file, TextLimit)
	if err != nil {
		return err
	}
	if hardlinked(latest) {
		return errHardlinked
	}
	if statOf(latest.info).Ctim != statOf(current.info).Ctim || !bytes.Equal(latest.bytes, current.bytes) {
		return errors.New("File changed before publication; read again")
	}
	return nil
}

// replacePreview is what a grep replace token's preview showed: by canonical path, the changed lines it showed whole, and how many changing
// lines it left out.
type replacePreview struct {
	shown map[string]map[int]bool
	left  int
}

type response struct {
	text   string
	record func()
}

// commit publishes every staged change in order; a failure after the first write names what already landed.
func commit(ctx *Context, store *snapshots.Store, items []*staged, shownByPreview map[string]map[int]bool) ([]string, error) {
	var changes []*staged
	for _, item := range items {
		if item.kind != stagedNoop {
			changes = append(changes, item)
		}
	}
	for _, item := range changes {
		if err := item.handle.parent.check(); err != nil {
			return nil, err
		}
		if item.handle.file != nil {
			if err := checkDescriptor(item.handle.file, item.handle.parent.location, item.handle.target.canonical); err != nil {
				return nil, err
			}
		}
		if item.kind == stagedEdit && item.handle.destination != nil {
			if err := destinationFree(item.handle.destination); err != nil {
				return nil, err
			}
		}
	}
	// Every response is rendered before anything is written, so one that cannot be rendered fails the call while nothing has changed yet.
	responses := make([]response, len(items))
	for i, item := range items {
		responses[i] = respond(ctx, store, item, shownByPreview)
	}
	var written []string
	for _, item := range changes {
		err := func() error {
			h := item.handle
			switch item.kind {
			case stagedCreate:
				options := publishOptions{}
				if item.replace != nil {
					options.replace = item.replace.info
					options.validate = func() error { return unchanged(h, item.replace) }
				}
				return publish(h.parent, h.target.canonical, item.bytes, ctx, options)
			case stagedRemove:
				if err := unchanged(h, item.current); err != nil {
					return err
				}
				if err := h.parent.check(); err != nil {
					return err
				}
				return os.Remove(h.parent.location)
			}
			if d := h.destination; d != nil {
				if err := unchanged(h, item.current); err != nil {
					return err
				}
				if err := publish(d.parent, d.target.canonical, item.applied.bytes, ctx, publishOptions{mode: statOf(item.current.info).Mode & 0o7777, hasMode: true}); err != nil {
					return err
				}
				if h.request.move != "" {
					if err := unchanged(h, item.current); err != nil {
						return err
					}
					if err := h.parent.check(); err != nil {
						return err
					}
					return os.Remove(h.parent.location)
				}
				return nil
			}
			return publish(h.parent, h.target.canonical, item.applied.bytes, ctx, publishOptions{replace: item.current.info, validate: func() error { return unchanged(h, item.current) }})
		}()
		if err != nil {
			if len(written) == 0 {
				return nil, err
			}
			return nil, &wrapped{fmt.Sprintf("Patch partially applied: %s already written; %s failed: %s", strings.Join(written, ", "), item.handle.target.display, err.Error()), err}
		}
		written = append(written, item.handle.target.display)
	}
	texts := make([]string, len(responses))
	for i, r := range responses {
		r.record()
		texts[i] = r.text
	}
	return texts, nil
}

func allLines(n int) []int {
	lines := make([]int, n)
	for i := range lines {
		lines[i] = i + 1
	}
	return lines
}

// respond is the response for one staged change, and the seen and known lines it records once the change is published.
func respond(ctx *Context, store *snapshots.Store, item *staged, shownByPreview map[string]map[int]bool) response {
	canonical, display := item.handle.target.canonical, item.handle.target.display
	nothing := func() {}
	switch item.kind {
	case stagedNoop:
		if len(item.applied.runs) == 0 {
			return response{fmt.Sprintf("[%s#%s]\nNo changes: the edit reproduces the current content; this tag is still current.", display, item.live.Tag), nothing}
		}
		// The lines come back whole, so an edit that keeps landing as it already is shows what the file holds.
		limit := LineLimit
		if minifiedReason(canonical, len(item.live.Key), item.live.Lines) != "" {
			limit = minifiedColumns
		}
		rows := preview(item.applied.rows, item.applied.runs, item.prior, limit)
		// Rewriting the last line of a file without a final newline is the usual way to try adding one, which an edit never does on its own.
		unterminated := false
		if item.source.body != "" && !strings.HasSuffix(item.source.body, "\n") {
			for _, r := range item.applied.runs {
				if r.at+r.length == len(item.applied.rows) {
					unterminated = true
				}
			}
		}
		lines := []string{fmt.Sprintf("[%s#%s]", display, item.live.Tag), "No changes: these lines already read as the edit writes them; this tag is still current."}
		lines = append(lines, rows.output...)
		lines = append(lines, rowNotes(display, rows.marks)...)
		if unterminated {
			lines = append(lines, fmt.Sprintf("[%s has no final newline, which an edit keeps; %s]", display, finalNewlineHint))
		}
		return response{strings.Join(lines, "\n"), func() {
			// A version without a seen set is unrestricted, and recording rows would restrict it.
			if item.prior != nil && item.prior.Seen != nil {
				store.Record(ctx.Session, canonical, item.live, nonNil(rows.shown), nil)
			}
		}}
	case stagedRemove:
		return response{"Deleted " + display, nothing}
	case stagedCreate:
		// The model wrote every line, so all of them count as seen; the row map shows where some of them are.
		lines := item.created.Lines
		m := writtenMap(display, lines)
		verb := "Created"
		if item.replace != nil {
			verb = "Overwrote"
		}
		kept := ""
		if len(item.kept) > 0 {
			kept = "; kept its " + strings.Join(item.kept, " and ")
		}
		out := append([]string{fmt.Sprintf("%s [%s#%s] (%s%s)", verb, display, item.created.Tag, m.summary, kept)}, m.rows...)
		if len(item.warnings) > 0 {
			out = append(append(out, "", "Warnings:"), item.warnings...)
		}
		return response{strings.Join(out, "\n"), func() {
			store.Record(ctx.Session, canonical, item.created, allLines(len(lines)), allLines(len(lines)))
		}}
	}
	// Like oh-my-pi, only lines that kept both number and content stay anchorable, plus the rows shown below.
	// Lines the agent wrote, or saw before they moved, stay known content that a ~ check can anchor.
	var destination *target
	if item.handle.destination != nil {
		destination = item.handle.destination.target
	}
	next := viewOf(item.result)
	prior := item.prior
	seen := []int{}
	known := []int{}
	for index, r := range item.applied.rows {
		if r.origin == index && (prior == nil || prior.Seen == nil || prior.Seen[index+1]) {
			seen = append(seen, index+1)
		}
		if r.origin < 0 {
			if !r.opaque {
				known = append(known, index+1)
			}
		} else if prior != nil && prior.Seen != nil && knownLine(prior, r.origin+1) {
			known = append(known, index+1)
		}
	}
	// A grep replace showed each new line as a >N| row; applied as previewed, one line for one, the rows need not come again.
	var shown map[int]bool
	if destination == nil && shownByPreview != nil {
		shown = shownByPreview[canonical]
	}
	runs, written := item.applied.runs, item.applied.rows
	if shown != nil && len(item.warnings) == 0 && !slices.ContainsFunc(runs, func(r run) bool { return r.length != 1 || written[r.at].was == nil }) {
		s := "s"
		if len(runs) == 1 {
			s = ""
		}
		return response{fmt.Sprintf("[%s#%s] %d line%s replaced as previewed", display, next.Tag, len(runs), s), func() {
			lines := slices.Clone(seen)
			for _, r := range runs {
				if shown[r.at+1] {
					lines = append(lines, r.at+1)
				}
			}
			store.Record(ctx.Session, canonical, next, lines, known)
		}}
	}
	// Edits to a minified file cut long rows like a read of it does.
	limitPath := canonical
	if destination != nil {
		limitPath = destination.canonical
	}
	limit := LineLimit
	if minifiedReason(limitPath, len(next.Key), next.Lines) != "" {
		limit = minifiedColumns
	}
	rows := preview(item.applied.rows, item.applied.runs, item.prior, limit)
	shownDisplay := display
	if destination != nil {
		shownDisplay = destination.display
	}
	head := fmt.Sprintf("[%s#%s]", shownDisplay, next.Tag)
	if destination != nil {
		verb := "Moved"
		if item.handle.request.copy != "" {
			verb = "Copied"
		}
		head = fmt.Sprintf("%s %s to %s", verb, display, head)
	}
	output := []string{head}
	if len(item.applied.runs) > 0 {
		if len(item.applied.rows) > 0 {
			output = append(output, rows.output...)
		} else {
			output = append(output, "(file is now empty)")
		}
	}
	if item.applied.terminated {
		output = append(output, fmt.Sprintf("[%s now ends with a newline]", shownDisplay))
	}
	output = append(output, rowNotes(shownDisplay, rows.marks)...)
	if rows.moved && store.Teach(ctx.Session, "moved") {
		output = append(output, movedLesson)
	}
	if len(item.warnings) > 0 {
		output = append(output, "", "Warnings:")
		output = append(output, item.warnings...)
	}
	recordPath := canonical
	if destination != nil {
		recordPath = destination.canonical
	}
	return response{strings.Join(output, "\n"), func() {
		store.Record(ctx.Session, recordPath, next, append(slices.Clone(seen), rows.shown...), known)
	}}
}

var reapplyPattern = regexp.MustCompile(`^REAPPLY` + jsSpace + `+(\S+)$`)

// Engine holds the snapshot store and the stored patches of REAPPLY tokens.
type Engine struct {
	Store  *snapshots.Store
	mu     sync.Mutex
	tokens map[string]storedPatch
	order  []string
}

type storedPatch struct {
	input   string
	preview *replacePreview
}

// New makes an engine with an empty store.
func New() *Engine {
	return &Engine{Store: snapshots.NewStore(), tokens: map[string]storedPatch{}}
}

// Patch applies a patch; REAPPLY TOKEN as the whole input applies the patch stored under that token, once.
func (e *Engine) Patch(ctx *Context, input string) (Result, error) {
	reapply := reapplyPattern.FindStringSubmatch(js.Trim(input))
	if reapply == nil {
		return e.apply(ctx, input, nil)
	}
	key := ctx.Session + "\x00" + reapply[1]
	e.mu.Lock()
	stored, ok := e.tokens[key]
	if ok {
		delete(e.tokens, key)
		e.order = slices.DeleteFunc(e.order, func(k string) bool { return k == key })
	}
	e.mu.Unlock()
	if !ok {
		return Result{}, fmt.Errorf("Reapply token %s is unknown, already used or expired; send the full patch again", reapply[1])
	}
	result, err := e.apply(ctx, stored.input, stored.preview)
	var paused *Paused
	if errors.As(err, &paused) {
		// The call is sent again once the client answers, and must find its token still there.
		e.mu.Lock()
		e.tokens[key] = stored
		e.order = append(e.order, key)
		e.mu.Unlock()
		return result, err
	}
	if err != nil {
		return result, err
	}
	// Said here too, since after applying a token a model takes the job as done.
	left := 0
	if stored.preview != nil {
		left = stored.preview.left
	}
	if left == 0 {
		return result, nil
	}
	if left == 1 {
		result.Text += "\n\n[1 more line that the search would change was not in this token; grep again with the same pattern and replace for it]"
	} else {
		result.Text += fmt.Sprintf("\n\n[%d more lines that the search would change were not in this token; grep again with the same pattern and replace for them]", left)
	}
	return result, nil
}

type indexedFailure struct {
	index int
	err   error
}

// apply validates every section before writing any, and refuses with all of their problems at once.
func (e *Engine) apply(ctx *Context, input string, p *replacePreview) (Result, error) {
	sections, err := patch.Parse(input)
	if err != nil {
		return Result{}, err
	}
	var failures []indexedFailure
	order := map[*request]int{}
	var requests []*request
	for index, section := range sections {
		if section.Create {
			if _, err := decodeText([]byte(section.Content)); err != nil {
				failures = append(failures, indexedFailure{index, err})
				continue
			}
			r := &request{create: true, filePath: section.Path, content: section.Content}
			order[r] = index
			requests = append(requests, r)
			continue
		}
		r := &request{filePath: section.Path, tag: section.Tag, edits: section.Edits, remove: section.Remove, move: section.Move, copy: section.Copy}
		order[r] = index
		requests = append(requests, r)
	}
	failed := func(r *request, err error) { failures = append(failures, indexedFailure{order[r], err}) }
	return locked(ctx, requests, func(handles []*handle) (Result, error) {
		var items []*staged
		for _, h := range handles {
			var item *staged
			var err error
			if h.request.create {
				item, err = stageCreate(ctx, e.Store, h)
			} else {
				item, err = stageEdit(ctx, e.Store, h)
			}
			if err != nil {
				if ctx.aborted() != nil {
					return Result{}, err
				}
				failed(h.request, err)
				continue
			}
			items = append(items, item)
		}
		if len(failures) > 0 {
			return Result{}, e.refusal(ctx.Session, input, failures, p)
		}
		var shown map[string]map[int]bool
		if p != nil {
			shown = p.shown
		}
		texts, err := commit(ctx, e.Store, items, shown)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: strings.Join(texts, "\n\n")}, nil
	}, failed)
}

// refusal is one error for every refused section, in patch order; when each refusal only displayed unseen lines, a token lets the agent apply
// the patch unchanged.
func (e *Engine) refusal(session, input string, failures []indexedFailure, p *replacePreview) error {
	slices.SortStableFunc(failures, func(a, b indexedFailure) int { return a.index - b.index })
	var parts []string
	if len(failures) > 1 {
		parts = append(parts, fmt.Sprintf("Nothing was written; %d sections have problems.", len(failures)))
	}
	retry := true
	for _, f := range failures {
		parts = append(parts, f.err.Error())
		var r *Refusal
		if !errors.As(f.err, &r) || !r.Retry {
			retry = false
		}
	}
	if retry {
		parts = append(parts, "To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY "+e.remember(session, input, p))
	}
	return errors.New(strings.Join(parts, "\n\n"))
}

// remember stores a patch under a new single-use token for REAPPLY, dropping the oldest past the token limit.
func (e *Engine) remember(session, input string, p *replacePreview) string {
	token := make([]byte, 6)
	for i := range token {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(tokenAlphabet))))
		token[i] = tokenAlphabet[n.Int64()]
	}
	key := session + "\x00" + string(token)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tokens[key] = storedPatch{input, p}
	e.order = append(e.order, key)
	for len(e.tokens) > tokens && len(e.order) > 0 {
		delete(e.tokens, e.order[0])
		e.order = e.order[1:]
	}
	return string(token)
}

// Write writes a whole file; overwriting an existing one needs the tag of its latest read or edit.
func (e *Engine) Write(ctx *Context, path, content string, tagValue string) (Result, error) {
	if _, err := decodeText([]byte(content)); err != nil {
		return Result{}, err
	}
	requested := ctx.resolve(path)
	exists := true
	if _, err := os.Lstat(requested); err != nil {
		if !missing(err) {
			return Result{}, err
		}
		exists = false
	}
	if exists && tagValue == "" {
		return Result{}, fmt.Errorf("%s already exists; pass the tag from its latest read to overwrite it, or use edit", path)
	}
	if !exists && tagValue != "" {
		return Result{}, fmt.Errorf("%s does not exist, so a tag cannot apply; omit tag to create it", path)
	}
	r := &request{create: true, filePath: path, content: content, overwrite: strings.ToUpper(tagValue)}
	return locked(ctx, []*request{r}, func(handles []*handle) (Result, error) {
		item, err := stageCreate(ctx, e.Store, handles[0])
		if err != nil {
			return Result{}, err
		}
		texts, err := commit(ctx, e.Store, []*staged{item}, nil)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: texts[0]}, nil
	}, nil)
}
