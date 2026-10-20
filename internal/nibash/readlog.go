package nibash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/11ways/nib/internal/engine"
)

// logMark is what an agent got of a file with skim: its size and start then, the whole lines read, which end at offset, and partial, how many
// bytes of a last line without its line break were shown after them; asked is the expect that read served, shown the file's lines the agent
// was shown, and at when, so a later compaction or clear of its context drops the mark. nib shape's mark holds only size, head, shown and at.
type logMark struct {
	size    int64
	head    []byte
	lines   int
	offset  int64
	partial int
	asked   string
	shown   lineSet
	at      time.Time
}

// rewrittenIn says whether file no longer starts as the file did when the mark was made, or shrank, so none of the mark holds.
func (m logMark) rewrittenIn(file *engine.LogFile) bool {
	return file.Size < m.size || !bytes.HasPrefix(file.Head, m.head)
}

// advanced is the mark a whole read of file leaves, going on from base lines: raw is what it held from file.From on, after skipped bytes of a
// byte order mark.
func advanced(file *engine.LogFile, raw []byte, skipped, base int) logMark {
	next := logMark{size: file.Size, head: file.Head, lines: base, offset: file.From + int64(skipped)}
	if end := bytes.LastIndexByte(raw, '\n'); end >= 0 {
		next.lines += bytes.Count(raw[:end+1], []byte("\n"))
		next.offset += int64(end + 1)
	}
	next.partial = len(raw) - int(next.offset-file.From-int64(skipped))
	return next
}

// question is expect as two reads compare it: the same words in any case and spacing ask the same.
func question(expect string) string {
	return strings.ToLower(strings.Join(strings.Fields(expect), " "))
}

// lineSet is a set of line numbers as sorted ranges that neither overlap nor touch.
type lineSet [][2]int

// with is the set with lines first to last added.
func (s lineSet) with(first, last int) lineSet {
	i := sort.Search(len(s), func(i int) bool { return s[i][1] >= first-1 })
	j := i
	for j < len(s) && s[j][0] <= last+1 {
		first, last = min(first, s[j][0]), max(last, s[j][1])
		j++
	}
	return slices.Insert(slices.Delete(slices.Clone(s), i, j), i, [2]int{first, last})
}

// holds says whether every line from first to last is in the set.
func (s lineSet) holds(first, last int) bool {
	i := sort.Search(len(s), func(i int) bool { return s[i][1] >= first })
	return i < len(s) && s[i][0] <= first && s[i][1] >= last
}

// mark is the agent's mark of the file at canonical, unless its context was dropped after the mark was made.
func (sess *session) mark(canonical string, cleared time.Time) (logMark, bool) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	m, ok := sess.marks[canonical]
	if !ok || m.at.Before(cleared) {
		return logMark{}, false
	}
	return m, true
}

func (sess *session) setMark(canonical string, m logMark) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.marks == nil {
		sess.marks = map[string]logMark{}
	}
	sess.marks[canonical] = m
}

// ReadLog reads files for what the agent expects of them, as plain lines shaped the way long program output is: past the limits a decider
// chooses the lines that bear on expect, or a summary leads the lines it points at. A file read whole again for the same expect shows only
// what was added since; for another, all of it again but the lines the agent was shown, unless it shrank or its start changed.
func (s *Shell) ReadLog(ctx context.Context, key any, paths []string, expect string, tools *engine.Context) (engine.Result, error) {
	sess := s.session(key)
	d := decision{decider: s.decider, summarizer: s.activeSummarizer(), purpose: purpose(expect), read: true,
		teach: func(lesson string) bool { return s.engine.Store.Teach(tools.Session, lesson) }}
	var blocks, failures []string
	for _, entry := range paths {
		text, err := s.readLog(ctx, sess, entry, question(expect), d, tools)
		if err != nil {
			if ctx.Err() != nil {
				return engine.Result{}, err
			}
			failures = append(failures, err.Error())
			text = "[" + err.Error() + "]"
		}
		blocks = append(blocks, text)
	}
	if len(failures) == len(paths) {
		return engine.Result{}, errors.New(strings.Join(failures, "; "))
	}
	return engine.Result{Text: strings.Join(blocks, "\n\n")}, nil
}

// readLog is one file's block: a head line of facts, then each window of its lines as shape leaves it, every run of lines the agent was
// shown before folded into a row naming them; asked is the read's expect as question gives it.
func (s *Shell) readLog(ctx context.Context, sess *session, entry, asked string, d decision, tools *engine.Context) (string, error) {
	whole, resumed, rewritten := false, false, false
	// mark is where this read goes on from, earlier the agent's mark when one holds for the file as it is now.
	var mark, earlier logMark
	known := false
	cleared := s.engine.Store.Cleared(tools.Session)
	file, err := s.engine.ReadLog(tools, entry, logLimit, func(f *engine.LogFile) int64 {
		whole = true
		m, ok := sess.mark(f.Canonical, cleared)
		if !ok {
			return 0
		}
		if m.rewrittenIn(f) {
			rewritten = true
			return 0
		}
		earlier, known = m, true
		// Another question may need lines that read passed over, so the file is read whole again.
		if m.asked != asked {
			return 0
		}
		mark, resumed = m, true
		return m.offset
	})
	if err != nil {
		return "", err
	}
	if !whole {
		if m, ok := sess.mark(file.Canonical, cleared); ok && !m.rewrittenIn(file) {
			earlier, known = m, true
		}
	}
	raw := file.Raw
	skipped := 0
	if file.From == 0 && bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		raw, skipped = raw[3:], 3
	}
	lines := plainLines(raw)
	base := mark.lines
	total := base + len(lines)
	d.command = "cat " + file.Display
	// next is the mark this read leaves: a selector's leaves where the next read goes on alone.
	next := earlier
	if !known {
		next = logMark{size: file.Size, head: file.Head}
	}
	if whole {
		next = advanced(file, raw, skipped, base)
		next.asked, next.shown = asked, earlier.shown
	}
	next.at = time.Now()
	// A last line still being written was shown whole as it stood; shown again unchanged, it is nothing new.
	if resumed && (len(raw) == 0 || len(raw) == mark.partial && next.lines == mark.lines) {
		sess.setMark(file.Canonical, next)
		return fmt.Sprintf("[%s] %d lines, plain: nothing new since your last %s with this expect", file.Display, total, engine.Skim), nil
	}
	var out []string
	switch {
	case len(lines) == 0 && !resumed:
		sess.setMark(file.Canonical, next)
		return fmt.Sprintf("[%s] empty, plain", file.Display), nil
	case file.Ranges != nil:
		var parts []string
		for _, r := range file.Ranges {
			if r[0] == r[1] {
				parts = append(parts, fmt.Sprint(r[0]))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
			}
		}
		out = append(out, fmt.Sprintf("[%s] lines %s of %d, plain", file.Display, strings.Join(parts, ", "), total))
	case resumed:
		out = append(out, fmt.Sprintf("[%s] lines %d-%d of %d, plain: new since your last %s", file.Display, base+1, total, total, engine.Skim))
	case rewritten:
		out = append(out, fmt.Sprintf("[%s] %d lines, plain: rewritten since your last %s, so all of it", file.Display, total, engine.Skim))
	default:
		out = append(out, fmt.Sprintf("[%s] %d lines, plain", file.Display, total))
	}
	// What nib shape showed went to a tool's result, which no agent can be told apart for; a selector asks for its lines again.
	var shaped logMark
	if m, ok := s.shapedMark(file.Canonical); ok && whole && !m.rewrittenIn(file) {
		shaped = m
	}
	if !whole {
		earlier.shown = nil
	}
	// A last line without its line break may still grow, so it does not count as shown.
	complete := base + bytes.Count(raw, []byte("\n"))
	windows := file.Ranges
	if windows == nil {
		windows = [][2]int{{base + 1, total}}
	}
	echoed := false
	for _, w := range windows {
		before := w[0] - 1
		var mine, theirs lineSet
		held := func(r row) bool {
			first, last := before+r.first, before+r.last
			switch {
			case earlier.shown.holds(first, last):
				mine = mine.with(first, last)
			case shaped.shown.holds(first, last):
				theirs = theirs.with(first, last)
			default:
				return false
			}
			return true
		}
		rows, footer, echo := shapeLines(ctx, lines[w[0]-1-base:w[1]-base], d, saved{path: file.Display, lines: w[1] - w[0] + 1, holds: w[1] - w[0] + 1, file: true, base: before}, held)
		if len(mine) > 0 {
			out = append(out, fmt.Sprintf("[%s: %s left out, which your earlier %[1]s showed]", engine.Skim, saved{}.spans(mine)))
		}
		if len(theirs) > 0 {
			out = append(out, fmt.Sprintf("[%s: %s left out, which a tool's result showed through nib shape at %s; read %s:%s if you lack them]", engine.Skim,
				saved{}.spans(theirs), shaped.at.Format("15:04"), file.Display, theirs.selector()))
		}
		next.shown = shownIn(rows, before, complete, next.shown)
		out = append(append(out, rowTexts(rows)...), footer...)
		echoed = echoed || echo
	}
	sess.setMark(file.Canonical, next)
	if file.Short {
		if whole {
			out = append(out, fmt.Sprintf("[%s: stopped after %d MiB, at line %d; the next %[1]s goes on from there]", engine.Skim, logLimit>>20, total))
		} else {
			out = append(out, fmt.Sprintf("[%s: only the first %d MiB of the file were read]", engine.Skim, logLimit>>20))
		}
	}
	if echoed {
		out = append(out, s.engine.EchoNote(tools)...)
	}
	return strings.Join(out, "\n"), nil
}

// selector is the set as a read's line selector, like 1-100,207-403.
func (s lineSet) selector() string {
	parts := make([]string, len(s))
	for i, r := range s {
		parts[i] = fmt.Sprintf("%d-%d", r[0], r[1])
		if r[0] == r[1] {
			parts[i] = fmt.Sprint(r[0])
		}
	}
	return strings.Join(parts, ",")
}

// shownIn is set with the file lines of rows' plain rows up to line limit added, before the file line before their first log line.
func shownIn(rows []row, before, limit int, set lineSet) lineSet {
	for _, r := range rows {
		if r.plain && r.first > 0 && before+r.first <= limit {
			set = set.with(before+r.first, min(before+r.last, limit))
		}
	}
	return set
}

// rowTexts are the texts of rows.
func rowTexts(rows []row) []string {
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.text
	}
	return texts
}

// shapeLines is lines as rows numbered from 1 that shape reads as the log of their own, while every note and cut row numbers them from
// log.base on, as the file does; footer explains cut rows, and echoed says a recurring block got markers. Rows known says the agent has
// are left out before the cut, so its budget goes to the rest; the deciders still read every line.
func shapeLines(ctx context.Context, lines []string, d decision, log saved, known func(row) bool) (rows []row, footer []string, echoed bool) {
	if len(lines) == 0 {
		return nil, nil, false
	}
	plain := make([]engine.PlainLine, len(lines))
	at := make([]int, len(lines))
	for i, line := range lines {
		plain[i] = engine.PlainLine{At: i + 1, Text: line}
		at[i] = i + 1
	}
	items, echoed := plainItems(plain, at, make([]*listedLine, len(lines)), 0)
	for _, it := range items {
		r := plainRowAt(it.text, log.base+it.first, it.first, it.last, nil)
		if known == nil || !known(r) {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return nil, nil, echoed
	}
	rows, footer = fit(ctx, rows, strings.Join(lines, "\n"), d, func() saved { return log })
	return rows, footer, echoed
}
