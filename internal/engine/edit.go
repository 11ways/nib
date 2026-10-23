package engine

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/patch"
	"github.com/11ways/nib/internal/platform"
	"github.com/11ways/nib/internal/snapshots"
	"github.com/11ways/nib/internal/trash"
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

// unseenError mirrors oh-my-pi's unseen-line guard: reveal what the model would have touched and let a straight retry pass. Lines that moved
// since the agent saw or wrote them say so; the others get the general explanations once, and later refusals keep only what differs.
func unseenError(store *snapshots.Store, session string, t *target, current snapshots.View, unseen []int, known map[int]bool) *Refusal {
	first := store.Teach(session, "unseen")
	var moved, never []int
	for _, line := range unseen {
		if known[line] {
			moved = append(moved, line)
		} else {
			never = append(never, line)
		}
	}
	var header string
	if len(never) > 0 {
		why := ""
		if store.Teach(session, "unseen-why") {
			why = " (partial read, truncated line, or rows elided from an edit response)"
		}
		header = fmt.Sprintf("This edit anchors to lines %s of %s that [%s#%s] never displayed in full%s", ranges(never), t.display, t.display, current.Tag, why)
	}
	switch {
	case len(moved) > 0 && header == "":
		header = fmt.Sprintf("This edit anchors to lines %s of %s, which moved since you saw or wrote them.", ranges(moved), t.display)
	case len(moved) > 0:
		header += fmt.Sprintf(", and to lines %s, which moved since you saw or wrote them.", ranges(moved))
	default:
		header += "."
	}
	if store.Forgotten(session, t.canonical) {
		header = fmt.Sprintf("This edit anchors to lines %s of %s that this agent was not shown since nib dropped its earlier views of the file: nib keeps the views of a limited number of files across all agents, and dropped this one's to make room, so lines read before then no longer count.", ranges(unseen), t.display)
	}
	ansi := slices.ContainsFunc(unseen, func(line int) bool {
		return line-1 < len(current.Lines) && visible(current.Lines[line-1]) != current.Lines[line-1]
	})
	columns, total := revealColumns, 0
	cut := false
	for _, line := range unseen {
		if line-1 < len(current.Lines) {
			size := js.Len(current.Lines[line-1])
			total += size
			cut = cut || size > revealColumns
		}
	}
	if cut && !ansi && len(unseen) <= revealLines && total <= wholeBudget {
		columns = wholeColumns
	}
	long := ansi || len(unseen) > revealLines
	for _, line := range unseen {
		if line-1 < len(current.Lines) && js.Len(current.Lines[line-1]) > columns {
			long = true
		}
	}
	if long {
		shown := unseen[:min(len(unseen), revealLines)]
		f := contextRows(current.Lines, shown, 0, revealColumns)
		// Color codes keep a line from ever showing whole, so only a SUB, never a read, can make such a patch apply.
		next := "Read those lines in full (with maxLineLength for cut ones), or change a cut line in place with SUB N: OLD."
		if ansi {
			next = "Change a cut or ansi line in place with SUB N: OLD; color codes keep an ansi line from ever showing in full."
		}
		return &Refusal{message: fmt.Sprintf("%s Preview of the first %d unseen line(s):\n%s\n%s", header, len(shown), strings.Join(f.rows, "\n"), next), Retry: !ansi, Unread: true}
	}
	f := contextRows(current.Lines, unseen, revealContext, columns)
	store.Record(session, t.canonical, current, nonNil(f.shown), nil)
	written := ""
	if slices.ContainsFunc(unseen, func(line int) bool { return known[line] }) && store.Teach(session, "unseen-known") {
		written = " Lines you wrote, or saw before an edit moved them, can skip this step: a ~TEXT row with the start of the line, right under the PUT or CUT line, confirms it."
	}
	meant := "Meant these? Resend unchanged; otherwise fix the numbers."
	if first {
		meant = "If these are the lines you meant, the same patch now succeeds; otherwise fix your line numbers."
	}
	return &Refusal{message: fmt.Sprintf("%s Actual file content (* marks those lines):\n%s\n%s%s", header, strings.Join(f.rows, "\n"), meant, written), Retry: true}
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
// finds that line certain, with a lesson naming both; on the one line holding it further away, it is refused with that line shown, and
// offered says whether that was offered already, as a resend after the refusal applies it there.
func resolveSubs(named, display string, lines []string, edits []snapshots.LineEdit, known func(line int) bool, offered func(start, found int, old string) bool, record recorder) (checked, error) {
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
	// elsewhere is the one line, far from its own, a SUB's old text is on, by the SUB's line.
	elsewhere := map[int]int{}
	edits = slices.Clone(edits)
	for i, edit := range edits {
		if edit.Sub == nil || edit.Start > len(lines) || strings.Contains(lines[edit.Start-1], *edit.Sub) {
			continue
		}
		to, ok := subTarget(lines, edits, i)
		switch {
		case !ok:
			continue
		case to < edit.Start-subReach || to > edit.Start+subReach || !known(to):
			if !offered(edit.Start, to, *edit.Sub) || !known(to) {
				elsewhere[edit.Start] = to
				continue
			}
		}
		lessons = append(lessons, fmt.Sprintf("SUB %d: its old text is not on line %d but on line %d, the one line of the file that holds it, so it applied there; take line numbers from the rows you were shown.", edit.Start, edit.Start, to))
		edits[i].Start, edits[i].End = to, to
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
			case at < 0 && elsewhere[line] > 0:
				failures = append(failures, failure{line, fmt.Sprintf("SUB %d: %s does not occur on line %d, but once in the file, on line %d", line, js.Quote(old), line, elsewhere[line])})
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
		resolved = append(resolved, snapshots.LineEdit{Start: line, End: line, Operation: snapshots.Replace, Content: strings.Split(composed.String(), "\n"), Quoted: true, Composed: true})
	}
	if len(failures) == 0 {
		return checked{edits: resolved, lessons: lessons}, nil
	}
	var marked []int
	for _, line := range doneOrder {
		if failedLine(line) {
			marked = append(marked, line)
			if to := elsewhere[line]; to > 0 && !slices.Contains(marked, to) {
				marked = append(marked, to)
			}
		}
	}
	slices.Sort(marked)
	f := contextRows(lines, marked, revealContext, revealColumns)
	if record != nil {
		record(nonNil(f.shown))
	}
	message := []string{named + ": SUB refused:"}
	// A refusal that only offers the line its old text is on applies there when the patch comes again.
	retry := true
	for _, f := range failures {
		message = append(message, f.message)
		retry = retry && elsewhere[f.line] > 0
	}
	if retry {
		message = append(message, "Sent again unchanged, the patch applies each SUB on the line named.")
	}
	message = append(message, "", fmt.Sprintf("Lines of %s around them (* marks them):", named))
	message = append(message, f.rows...)
	message = append(message, rowNotes(display, f.marks)...)
	return checked{edits: resolved, refusal: &Refusal{message: strings.Join(message, "\n"), Retry: retry}, lessons: lessons}, nil
}

// subReach is how far from its line a SUB's old text may be and still be taken as meant there.
const subReach = 2

// subTarget is the line a SUB whose old text is not on its own line may have meant: the one line of the file holding that text, long enough
// to tell (snapshots.EvidenceMinRunes), and anchored by no other hunk; ok is false when any of that is in doubt, so the SUB is refused as
// before. resolveSubs applies it there when it is at most subReach lines away and known to the agent, or once it offered that line.
func subTarget(lines []string, edits []snapshots.LineEdit, index int) (int, bool) {
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
	if found == 0 {
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
// replacement overwrote, so a long changed line can show where it changed; written marks a new row the agent wrote whole, as it is.
type changeRow struct {
	raw     string
	origin  int
	opaque  bool
	written bool
	was     *string
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
			r := changeRow{raw: raw, origin: -1, opaque: change.Opaque, written: !change.Composed}
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

// likeness is alike, except that two lines too short for alike to judge count as the same (1) when they match apart from indentation and
// hold a letter or digit, as a repeated short statement would.
func likeness(a, b string) float64 {
	if score := alike(a, b); score > 0 {
		return score
	}
	if js.Trim(a) == js.Trim(b) && meaningful(a) {
		return 1
	}
	return 0
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
		if above, ok := kept(at - 1); ok && likeness(above, first) >= alikeThreshold && likeness(above, first) > likeness(lineContent(source.lines[edit.Start-1]), first) {
			// Shifted down by one, the range kept the line it meant to replace and overwrote the line after its end.
			overwritten := lineContent(source.lines[edit.End-1])
			warnings = append(warnings, fmt.Sprintf("Lines %d and %d now nearly match: line %d was kept just above an edit and line %d is the edit's first new line. If the edit was meant to replace line %d, its range started one line late and overwrote the line below it instead; these hunks, with the new tag, delete line %d and put that line back:\nCUT %d\nPUT >%d:\n+%s", at, at+1, at, at+1, at, at, at, at+length, overwritten))
		}
		if below, ok := kept(at + length); ok && likeness(below, last) >= alikeThreshold && likeness(below, last) > likeness(lineContent(source.lines[edit.End-1]), last) {
			warnings = append(warnings, fmt.Sprintf("Lines %d and %d now nearly match: line %d is an edit's last new line and line %d was kept just below it. If the edit was meant to replace line %d, its range ended one line early; this hunk, with the new tag, deletes it:\nCUT %d", at+length, at+length+1, at+length, at+length+1, at+length+1, at+length+1))
		}
	}
	return warnings
}

// swallowed warns about a replaced range one line too long: its first or last old line, long enough to judge, lives on in no new row of the
// patch while the range's other lines do, so the range likely took in a line it never meant to change. A range that gains rows as well, which
// an insert next to the meant line does, warns only when the kept lines moved to the edge the lost line left: an edge line rewritten in place
// keeps them where they were. Each warning carries the hunk that puts the line back.
func swallowed(source *text, edits []snapshots.LineEdit, result applied) []string {
	var rows []string
	for _, edit := range edits {
		rows = append(rows, edit.Content...)
	}
	resembles := func(row, line string) bool {
		return js.Trim(row) == js.Trim(line) || alike(row, line) >= alikeThreshold
	}
	resembled := func(line string) bool {
		return slices.ContainsFunc(rows, func(row string) bool { return resembles(row, line) })
	}
	var warnings []string
	for index, edit := range edits {
		if edit.Operation != snapshots.Replace || len(edit.Content) == 0 || edit.Quoted || edit.Opaque {
			continue
		}
		grows := len(edit.Content) >= edit.End-edit.Start+1
		var old []string
		for line := edit.Start; line <= edit.End; line++ {
			old = append(old, lineContent(source.lines[line-1]))
		}
		lost := func(i int) bool { return js.Len(js.Trim(old[i])) >= alikeMinChars && !resembled(old[i]) }
		// The other lines live on when every one that is not blank resembles a new row, and there is at least one.
		live := func(others []string) bool {
			count := 0
			for _, line := range others {
				if js.Trim(line) == "" {
					continue
				}
				if !resembled(line) {
					return false
				}
				count++
			}
			return count > 0
		}
		at, length := result.runs[index].at, result.runs[index].length
		short := func(line string) string {
			line = js.Trim(line)
			if js.Len(line) > 60 {
				line = js.Slice(line, 0, 60) + "..."
			}
			return js.Quote(line)
		}
		last := len(old) - 1
		switch {
		case lost(last) && live(old[:last]) && (!grows || resembles(edit.Content[len(edit.Content)-1], old[last-1])):
			warnings = append(warnings, fmt.Sprintf("The edit of lines %d-%d dropped line %d, %s: no new row resembles it, while they keep the range's other lines. If the range was meant to end at line %d, this hunk, with the new tag, puts it back:\nPUT >%d:\n+%s", edit.Start, edit.End, edit.End, short(old[last]), edit.End-1, at+length, old[last]))
		case lost(0) && live(old[1:]) && (!grows || resembles(edit.Content[0], old[1])):
			warnings = append(warnings, fmt.Sprintf("The edit of lines %d-%d dropped line %d, %s: no new row resembles it, while they keep the range's other lines. If the range was meant to start at line %d, this hunk, with the new tag, puts it back:\nPUT <%d:\n+%s", edit.Start, edit.End, edit.Start, short(old[0]), edit.Start+1, at+1, old[0]))
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
	count := bracketCounter(file)
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

// bracketCounter is how a file's lines count brackets: templates their own way, everything else as code.
func bracketCounter(file string) func([]string) []brackets {
	if strings.EqualFold(filepath.Ext(file), ".hwk") {
		return templateBrackets
	}
	return bracketsOf
}

// rawChanges are edits as the raw lines applyChanges writes, in the file's line ending.
func rawChanges(source *text, edits []snapshots.LineEdit) []rawEdit {
	changes := make([]rawEdit, len(edits))
	for i, e := range edits {
		raw := make([]string, len(e.Content))
		for j, line := range e.Content {
			raw[j] = line + source.ending
		}
		changes[i] = rawEdit{e, raw}
	}
	return changes
}

// extendSlips widens a replaced range by the line just past it when the model's rows end (or start) with that very line, the range having ended
// one line early (or started one late), and the widening is proven: only the wider range keeps a file whose brackets balanced balanced, which
// catches the slip on a line as short as a closing brace; or the rows go on past that line as the range's own edge line, as a copy of the
// lines from one line too far would, which catches it on a statement. It corrects only when exactly one widening is proven, and says what it did.
func extendSlips(file string, source *text, edits []snapshots.LineEdit) ([]snapshots.LineEdit, []string) {
	count := bracketCounter(file)
	total := func(lines []string) brackets {
		var sum brackets
		for _, b := range count(lines) {
			sum = sum.add(b)
		}
		return sum
	}
	lines := sourceLines(source)
	balanced := func(candidate []snapshots.LineEdit) bool {
		return total(appliedLines(applyChanges(source, rawChanges(source, candidate)))) == brackets{}
	}
	byBrackets := total(lines) == (brackets{}) && !balanced(edits)
	touched := func(skip, line int) bool {
		for j, other := range edits {
			if j != skip && (slices.Contains(snapshots.AnchorLines(other), line) || other.Operation == snapshots.Replace && other.Start <= line && line <= other.End) {
				return true
			}
		}
		return false
	}
	type slip struct {
		edits []snapshots.LineEdit
		note  string
	}
	var found []slip
	for i, e := range edits {
		if e.Operation != snapshots.Replace || e.Quoted || len(e.Content) == 0 {
			continue
		}
		span := fmt.Sprintf("lines %d-%d", e.Start, e.End)
		if e.Start == e.End {
			span = "line " + itoa(e.Start)
		}
		for _, below := range []bool{true, false} {
			// line is the neighbour the edge row repeats; inner is the row next to it and edge the range's own line it would go on as.
			line, row, inner, edge := e.End+1, e.Content[len(e.Content)-1], len(e.Content)-2, e.End
			if !below {
				line, row, inner, edge = e.Start-1, e.Content[0], 1, e.Start
			}
			if line < 1 || line > len(lines) || js.Trim(row) == "" || js.Trim(row) != js.Trim(lines[line-1]) || touched(i, line) {
				continue
			}
			copied := meaningful(row) && inner >= 0 && inner < len(e.Content) && js.Trim(e.Content[inner]) == js.Trim(lines[edge-1])
			wider := slices.Clone(edits)
			if below {
				wider[i].End = line
			} else {
				wider[i].Start = line
			}
			var why string
			switch {
			case byBrackets && balanced(wider):
				why = "with the brackets unbalanced"
			case copied:
				why = fmt.Sprintf("and its rows go on from there as line %d", edge)
			default:
				continue
			}
			note := fmt.Sprintf("The edit of %s replaced lines %d-%d: its first new row repeats line %d, which would have stayed above it %s, so the range started one line late.", span, line, e.End, line, why)
			if below {
				note = fmt.Sprintf("The edit of %s replaced lines %d-%d: its last new row repeats line %d, which would have stayed below it %s, so the range ended one line early.", span, e.Start, line, line, why)
			}
			found = append(found, slip{wider, note})
		}
	}
	if len(found) != 1 {
		return edits, nil
	}
	return found[0].edits, []string{found[0].note}
}

// meaningful says whether a line holds a letter or digit, unlike a blank line or a lone closing bracket, which a file often repeats.
func meaningful(line string) bool {
	return strings.ContainsFunc(line, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
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

const (
	// editColumns is where an edit response cuts a new row, showing where a replaced line changed; keptColumns where it cuts a kept row.
	editColumns = 300
	keptColumns = 120
)

// preview is the rows of an edit response: the kept row on each side of every change, the new rows whole except a sparse N~ map inside a long
// run, and one note per stretch of moved lines the agent knew, with their old numbers. New rows are cut past limit and kept rows past keptLimit.
// With sent, a run of new rows the agent wrote whole prints as one marker naming their lines instead, a long run's map rows under it.
func preview(rows []changeRow, runs []run, prior *snapshots.Version, limit, keptLimit int, sent bool) previewed {
	full := map[int]bool{}
	mapped := map[int]bool{}
	markers := map[int]string{}
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
		switch {
		case r.length == 0:
			continue
		case sent && rows[r.at].written:
			markers[r.at] = sentMarker(r.at+1, r.at+r.length)
			if r.length <= writtenRows {
				continue
			}
		case r.length <= writtenRows:
			for index := r.at; index < r.at+r.length; index++ {
				full[index] = true
			}
			continue
		default:
			full[r.at] = true
			full[r.at+r.length-1] = true
		}
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
	for k := range markers {
		indexes[k] = true
	}
	var out previewed
	for _, index := range sortedSet(indexes) {
		whole := full[index]
		if note, ok := notes[index]; ok {
			out.output = append(out.output, note)
		}
		if marker, ok := markers[index]; ok {
			out.output = append(out.output, marker)
		}
		r := rows[index]
		if whole {
			content := lineContent(r.raw)
			var w *windowed
			rowLimit := limit
			if r.origin >= 0 && r.was == nil {
				rowLimit = keptLimit
			}
			if r.was != nil && js.Len(visible(content)) > rowLimit {
				w = matchWindows(content, []span{changedSpan(*r.was, content)}, rowLimit)
			}
			// A cut row still shows the line's new number, so a line the agent knew whole before stays one it may anchor.
			anchor := knew(index)
			if w != nil {
				out.output = append(out.output, cutRow(index+1, w.label, w.text))
				out.marks = append(out.marks, marks{cut: true, short: rowLimit < LineLimit})
			} else {
				line := lineRow(index+1, content, rowLimit)
				out.output = append(out.output, line.text)
				anchor = anchor || line.anchor
				if !line.anchor {
					m := line.marks
					m.short = m.cut && rowLimit < LineLimit
					out.marks = append(out.marks, m)
				}
			}
			if anchor {
				out.shown = append(out.shown, index+1)
			}
		} else if mapped[index] {
			out.output = append(out.output, fmt.Sprintf("%d~%s", index+1, clip(lines()[index])))
			out.shown = append(out.shown, index+1)
		}
	}
	out.moved = len(notes) > 0
	return out
}

// sentMarker is the row that stands for lines first to last, which the agent sent whole in its edit and knows as they are.
func sentMarker(first, last int) string {
	if first == last {
		return fmt.Sprintf("[line %d as you sent it]", first)
	}
	return fmt.Sprintf("[lines %d-%d as you sent them]", first, last)
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
	const infinity = math.MaxInt / 2
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

// release closes the handle's open file, as a replacement on Windows needs.
func (h *handle) release() {
	if h.file != nil {
		h.file.Close()
	}
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
				file, err := p.open()
				if err != nil {
					return zero, err
				}
				defer file.Close()
				if err := checkDescriptor(file, p, p.name, h.target.canonical); err != nil {
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
	// notes say what an IMPORT did instead of adding a line.
	notes   []string
	bytes   []byte
	created snapshots.View
	replace *held
	kept    []string
}

func hardlinked(h *held) bool { return h.links > 1 }

var errHardlinked = errors.New("Hardlinked targets are not supported; refusing to break links")

func stageEdit(ctx *Context, store *snapshots.Store, h *handle) (*staged, error) {
	r, t := h.request, h.target
	// IMPORT edits are placed among the file's imports as it is, so they depend on no line the agent saw, and on no tag.
	requestedEdits, imports := splitImports(r.edits)
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
		edits, fixes := extendSlips(h.target.canonical, source, edits)
		warnings = append(warnings, fixes...)
		edits, notes, err := placeImports(t.canonical, live.Lines, edits, imports)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", t.display, err)
		}
		result := applyChanges(source, rawChanges(source, edits))
		if bytes.Equal(result.bytes, current.bytes) && r.move == "" && r.copy == "" {
			return &staged{kind: stagedNoop, handle: h, source: source, live: live, prior: prior, applied: result, notes: notes}, nil
		}
		decoded, err := decodeText(result.bytes)
		if err != nil {
			return nil, err
		}
		brackets := unbalanced(h.target.canonical, source, edits, result)
		return &staged{kind: stagedEdit, handle: h, current: current, source: source, live: live, prior: prior, applied: result, result: decoded, notes: notes,
			warnings: slices.Concat(warnings, offByOne(source, edits, result), swallowed(source, edits, result), brackets,
				dataWarnings(ctx, store, h.target.canonical, sourceLines(source), appliedLines(result)), codeWarnings(ctx, store, t, source, result, len(brackets) > 0))}, nil
	}
	// A tagless section only appends, which depends on nothing but where the file ends, and IMPORTs alone on nothing at all.
	if r.tag == nil || len(requestedEdits) == 0 && len(imports) > 0 && !r.remove && r.move == "" && r.copy == "" {
		return finish(snapshots.ResolveEdges(requestedEdits, len(live.Lines)), nil)
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
		for _, e := range requestedEdits {
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
	for _, e := range requestedEdits {
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
	// A SUB offered the one line its old text is on, of this version, applies there when sent again.
	offered := func(start, found int, old string) bool {
		return !store.Teach(ctx.Session, fmt.Sprintf("sub-line:%s#%s:%d:%d:%s", t.canonical, basis.Tag, start, found, old))
	}
	subs, err := resolveSubs(named, t.display, basis.Lines, checks.edits, func(line int) bool { return knownLine(basis, line) }, offered, record)
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
			// Lines seen under another path holding the same text, as a worktree's copy of the file, are the same lines.
			if len(remaining) > 0 {
				elsewhere := store.SeenAs(ctx.Session, live.Key, t.canonical)
				remaining = slices.DeleteFunc(remaining, func(line int) bool { return elsewhere[line] })
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
	var moved bool
	var remapped []snapshots.LineEdit
	ok := false
	switch {
	case len(anchors) == 0 && r.move == "" && r.copy == "":
		remapped, ok = snapshots.ResolveEdges(requested, len(live.Lines)), true
	case len(anchors) > 0 && len(snapshots.Unverified(basis, requested)) == 0:
		moved, remapped, ok = snapshots.RemapEdits(basis, live.Lines, requested)
	}
	if !ok {
		return nil, mismatchError(store, ctx.Session, t, live, tagValue, true, anchors, false)
	}
	note := recoveryChain
	switch {
	case len(anchors) == 0:
		note = tailDrift
	case moved:
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
		return &staged{kind: stagedCreate, handle: h, bytes: data, created: viewOf(decoded), replace: current, kept: kept, warnings: dataWarnings(ctx, store, h.target.canonical, viewOf(old).Lines, viewOf(decoded).Lines)}, nil
	}
	decoded, err := decodeText(data)
	if err != nil {
		return nil, err
	}
	if _, err := h.parent.root.Lstat(h.parent.name); err == nil {
		return nil, fmt.Errorf("%s already exists; edit it with a [PATH#TAG] section, or write it with its tag to overwrite", h.target.display)
	} else if !missing(err) {
		return nil, err
	}
	return &staged{kind: stagedCreate, handle: h, bytes: data, created: viewOf(decoded), warnings: dataWarnings(ctx, store, h.target.canonical, nil, viewOf(decoded).Lines)}, nil
}

func destinationFree(h *handle) error {
	if _, err := h.parent.root.Lstat(h.parent.name); err == nil {
		return fmt.Errorf("Destination %s already exists; REM it in its own section first", h.target.display)
	} else if !missing(err) {
		return err
	}
	return nil
}

// unchanged fails unless the open source descriptor still holds the bytes that were staged.
func unchanged(h *handle, current *held) error {
	if err := checkDescriptor(h.file, h.parent, h.parent.name, h.target.canonical); err != nil {
		return err
	}
	latest, err := snapshot(h.file, TextLimit)
	if err != nil {
		return err
	}
	if hardlinked(latest) {
		return errHardlinked
	}
	if platform.ChangeTime(latest.info) != platform.ChangeTime(current.info) || !bytes.Equal(latest.bytes, current.bytes) {
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
func commit(ctx *Context, store *snapshots.Store, can *trash.Can, wrote func(string), items []*staged, shownByPreview map[string]map[int]bool) ([]string, error) {
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
			if err := checkDescriptor(item.handle.file, item.handle.parent, item.handle.parent.name, item.handle.target.canonical); err != nil {
				return nil, err
			}
		}
		if item.kind == stagedEdit && item.handle.destination != nil {
			if err := destinationFree(item.handle.destination); err != nil {
				return nil, err
			}
		}
	}
	// Every response is rendered before anything is written, so one that cannot be rendered fails the call while nothing has changed yet;
	// the folders written into are known first, so their paths print as an agent working there has them.
	for _, item := range changes {
		ctx.worked(item.handle)
	}
	responses := make([]response, len(items))
	for i, item := range items {
		responses[i] = respond(ctx, store, item, shownByPreview)
	}
	var written []string
	for _, item := range changes {
		// Marked before and after, so a command's end that checks between them still finds the write.
		marked := []string{item.handle.target.canonical}
		if d := item.handle.destination; d != nil {
			marked = append(marked, d.target.canonical)
		}
		for _, path := range marked {
			wrote(path)
		}
		err := func() error {
			h := item.handle
			switch item.kind {
			case stagedCreate:
				options := publishOptions{}
				if item.replace != nil {
					options.replace = item.replace.info
					options.validate = func() error { return unchanged(h, item.replace) }
					options.release = h.release
				}
				return publish(h.parent, h.target.canonical, item.bytes, ctx, options)
			case stagedRemove:
				if err := unchanged(h, item.current); err != nil {
					return err
				}
				if err := h.parent.check(); err != nil {
					return err
				}
				_, err := can.PutBy(h.target.canonical, h.parent.moveOut, h.target.canonical, ctx.Sandbox)
				return err
			}
			if d := h.destination; d != nil {
				if err := unchanged(h, item.current); err != nil {
					return err
				}
				if err := publish(d.parent, d.target.canonical, item.applied.bytes, ctx, publishOptions{mode: rawMode(item.current.info), hasMode: true}); err != nil {
					return err
				}
				if h.request.move != "" {
					if err := unchanged(h, item.current); err != nil {
						return err
					}
					if err := h.parent.check(); err != nil {
						return err
					}
					return h.parent.root.Remove(h.parent.name)
				}
				return nil
			}
			return publish(h.parent, h.target.canonical, item.applied.bytes, ctx, publishOptions{replace: item.current.info, validate: func() error { return unchanged(h, item.current) }, release: h.release})
		}()
		for _, path := range marked {
			wrote(path)
		}
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
			if len(item.notes) > 0 {
				return response{strings.Join(append([]string{fmt.Sprintf("[%s#%s]", display, item.live.Tag)}, item.notes...), "\n"), nothing}
			}
			return response{fmt.Sprintf("[%s#%s]\nNo changes: the edit reproduces the current content; this tag is still current.", display, item.live.Tag), nothing}
		}
		// The lines come back whole, so an edit that keeps landing as it already is shows what the file holds.
		limit := LineLimit
		if minifiedReason(canonical, len(item.live.Key), item.live.Lines) != "" {
			limit = minifiedColumns
		}
		rows := preview(item.applied.rows, item.applied.runs, item.prior, limit, limit, false)
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
		lines = append(lines, item.notes...)
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
		return response{"Moved " + display + " to the trash; trash restore " + display + " in nibash puts it back", nothing}
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
			// The agent wrote the whole row, so it counts as seen however the response shows it; a SUB on a line it never knew whole does not.
			if !r.opaque {
				seen = append(seen, index+1)
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
	// The agent wrote the new rows, so a long one shows where it changed; the kept rows around a change are cut shorter still.
	limit := min(LineLimit, editColumns)
	if minifiedReason(limitPath, len(next.Key), next.Lines) != "" {
		limit = minifiedColumns
	}
	rows := preview(item.applied.rows, item.applied.runs, item.prior, limit, min(limit, keptColumns), true)
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
	output = append(output, item.notes...)
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
		// An edit that drew warnings may not have done what the agent pictures, so its next view of the file shows every line.
		if len(item.warnings) > 0 {
			store.Doubt(ctx.Session, recordPath)
		}
	}}
}

// reapplyPattern is the first line of a REAPPLY input; corrected sections may follow it.
var reapplyPattern = regexp.MustCompile(`^REAPPLY` + jsSpace + `+(\S+)$`)

// Engine holds the snapshot store, the stored patches of REAPPLY tokens, the trash what nib deletes goes to, and when its edits and writes
// last wrote each file.
type Engine struct {
	Store  *snapshots.Store
	Trash  *trash.Can
	mu     sync.Mutex
	tokens map[string]storedPatch
	order  []string
	writes map[string]time.Time
}

type storedPatch struct {
	sections []*patch.Section
	preview  *replacePreview
}

// New makes an engine with an empty store.
func New() *Engine {
	return &Engine{Store: snapshots.NewStore(), Trash: trash.New(), tokens: map[string]storedPatch{}}
}

// Patch applies a patch. REAPPLY TOKEN as its first line applies the patch stored under that token, once; sections below it replace the
// stored sections for their paths, and those for other paths join it.
func (e *Engine) Patch(ctx *Context, input string) (result Result, err error) {
	defer func() { result.Text = e.teachRowNotes(ctx, result.Text) }()
	lines := strings.Split(input, "\n")
	first := slices.IndexFunc(lines, func(line string) bool { return js.Trim(line) != "" })
	var reapply []string
	if first >= 0 {
		reapply = reapplyPattern.FindStringSubmatch(js.Trim(lines[first]))
	}
	if reapply == nil {
		sections, err := patch.Parse(input)
		if err != nil {
			return Result{}, err
		}
		return e.apply(ctx, sections, nil)
	}
	// The REAPPLY line stays as a blank one, so parse errors name the lines of the input as sent.
	var fixes []*patch.Section
	if rest := strings.Join(lines[first+1:], "\n"); js.Trim(rest) != "" {
		parsed, err := patch.Parse(strings.Repeat("\n", first+1) + rest)
		if err != nil {
			return Result{}, err
		}
		fixes = parsed
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
	sections, preview := stored.sections, stored.preview
	var replaced []replacement
	if len(fixes) > 0 {
		sections, preview, replaced = corrected(ctx, stored, fixes)
	}
	result, err = e.apply(ctx, sections, preview)
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
	// A resent section stands in for the stored one of its path whole, so one sent with fewer hunks dropped some: said, since a model
	// resending one corrected hunk takes the others as applied.
	for _, r := range replaced {
		if r.sent < r.stored {
			result.Text += fmt.Sprintf("\n\n[REAPPLY: the section you resent for %s stood in for the stored one whole: its %s applied, not the stored %d]", r.path, hunks(r.sent), r.stored)
		}
	}
	// Said here too, since after applying a token a model takes the job as done.
	left := 0
	if preview != nil {
		left = preview.left
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

// replacement is a stored section a resent one stood in for: its path and the hunks each had.
type replacement struct {
	path         string
	stored, sent int
}

// hunks counts n as "1 hunk" or "N hunks".
func hunks(n int) string {
	if n == 1 {
		return "1 hunk"
	}
	return fmt.Sprintf("%d hunks", n)
}

// corrected is a stored patch with fixes in place of its sections for the same paths, in their place, and the other fixes after them; a
// grep replace's preview no longer vouches for a path whose sections were replaced. replaced lists each path's stored and resent hunks.
func corrected(ctx *Context, stored storedPatch, fixes []*patch.Section) ([]*patch.Section, *replacePreview, []replacement) {
	paths := func(section *patch.Section) []string {
		resolved := ctx.resolve(section.Path)
		if real, err := filepath.EvalSymlinks(resolved); err == nil && real != resolved {
			return []string{resolved, real}
		}
		return []string{resolved}
	}
	byPath := map[string][]*patch.Section{}
	var order []string
	for _, fix := range fixes {
		key := paths(fix)[0]
		if _, ok := byPath[key]; !ok {
			order = append(order, key)
		}
		byPath[key] = append(byPath[key], fix)
	}
	used := map[string]bool{}
	var sections []*patch.Section
	replaced := map[string]bool{}
	var replacements []replacement
	at := map[string]int{}
	for _, section := range stored.sections {
		keys := paths(section)
		key := keys[0]
		if _, ok := byPath[key]; !ok {
			sections = append(sections, section)
			continue
		}
		for _, k := range keys {
			replaced[k] = true
		}
		if !used[key] {
			sections = append(sections, byPath[key]...)
			used[key] = true
			sent := 0
			for _, fix := range byPath[key] {
				sent += len(fix.Edits)
			}
			at[key] = len(replacements)
			replacements = append(replacements, replacement{path: section.Path, sent: sent})
		}
		replacements[at[key]].stored += len(section.Edits)
	}
	for _, key := range order {
		if !used[key] {
			sections = append(sections, byPath[key]...)
		}
	}
	preview := stored.preview
	if preview != nil && len(replaced) > 0 {
		shown := map[string]map[int]bool{}
		for path, lines := range preview.shown {
			if !replaced[path] {
				shown[path] = lines
			}
		}
		preview = &replacePreview{shown: shown, left: preview.left}
	}
	return sections, preview, replacements
}

type indexedFailure struct {
	index int
	err   error
}

// apply validates every section before writing any, and refuses with all of their problems at once.
func (e *Engine) apply(ctx *Context, sections []*patch.Section, p *replacePreview) (Result, error) {
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
			return Result{}, e.refusal(ctx.Session, sections, failures, p)
		}
		var shown map[string]map[int]bool
		if p != nil {
			shown = p.shown
		}
		texts, err := commit(ctx, e.Store, e.Trash, e.wrote, items, shown)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: strings.Join(texts, "\n\n")}, nil
	}, failed)
}

// refusal is one error for every refused section, in patch order, ending in a token for the patch: when each refusal only needs lines read,
// the token applies it unchanged, and otherwise with corrected sections in place of the refused ones.
func (e *Engine) refusal(session string, sections []*patch.Section, failures []indexedFailure, p *replacePreview) error {
	slices.SortStableFunc(failures, func(a, b indexedFailure) int { return a.index - b.index })
	var parts []string
	if len(failures) > 1 {
		parts = append(parts, fmt.Sprintf("Nothing was written; %d sections have problems.", len(failures)))
	}
	retry, unread := true, false
	for _, f := range failures {
		parts = append(parts, f.err.Error())
		var r *Refusal
		if !errors.As(f.err, &r) || !r.Retry {
			retry = false
		} else if r.Unread {
			unread = true
		}
	}
	token := e.remember(session, sections, p)
	switch {
	case retry && unread && e.Store.Teach(session, "reapply-read"):
		parts = append(parts, "To apply this patch unchanged once you have read those lines, call edit with input: REAPPLY "+token)
	case retry && !unread && e.Store.Teach(session, "reapply"):
		parts = append(parts, "To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY "+token)
	case retry:
		parts = append(parts, "REAPPLY "+token)
	case e.Store.Teach(session, "reapply-fix"):
		parts = append(parts, "To resend a file's section corrected, call edit with input: REAPPLY "+token+" on the first line, then that [PATH#TAG] section whole, since it stands in for the stored one; the other files' sections apply as sent.")
	default:
		parts = append(parts, "REAPPLY "+token+", then corrected sections, each whole")
	}
	return errors.New(strings.Join(parts, "\n\n"))
}

// remember stores a patch under a new single-use token for REAPPLY, dropping the oldest past the token limit.
func (e *Engine) remember(session string, sections []*patch.Section, p *replacePreview) string {
	token := make([]byte, 6)
	for i := range token {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(tokenAlphabet))))
		token[i] = tokenAlphabet[n.Int64()]
	}
	key := session + "\x00" + string(token)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tokens[key] = storedPatch{sections, p}
	e.order = append(e.order, key)
	for len(e.tokens) > tokens && len(e.order) > 0 {
		delete(e.tokens, e.order[0])
		e.order = e.order[1:]
	}
	return string(token)
}

// Write writes a whole file; overwriting an existing one needs the tag of its latest read or edit.
func (e *Engine) Write(ctx *Context, path, content string, tagValue string) (result Result, err error) {
	defer func() { result.Text = e.teachRowNotes(ctx, result.Text) }()
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
		texts, err := commit(ctx, e.Store, e.Trash, e.wrote, []*staged{item}, nil)
		if err != nil {
			return Result{}, err
		}
		return Result{Text: texts[0]}, nil
	}, nil)
}
