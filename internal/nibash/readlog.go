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
// chooses the lines that bear on expect, or a summary leads the lines it points at. The limits hold for the whole call, so files past them
// together are shaped as one output. A file read whole again for the same expect shows only what was added since; for another, all of it
// again but the lines the agent was shown, unless it shrank or its start changed.
func (s *Shell) ReadLog(ctx context.Context, key any, paths []string, expect string, tools *engine.Context) (engine.Result, error) {
	sess := s.session(key)
	d := decision{decider: s.decider, summarizer: s.activeSummarizer(), purpose: purpose(expect), read: true,
		teach: func(lesson string) bool { return s.engine.Store.Teach(tools.Session, lesson) }}
	var blocks []*logBlock
	var failures []string
	for _, entry := range paths {
		b, err := s.readLog(sess, entry, question(expect), tools)
		if err != nil {
			if ctx.Err() != nil {
				return engine.Result{}, err
			}
			failures = append(failures, err.Error())
			b = &logBlock{head: []string{"[" + err.Error() + "]"}}
		}
		blocks = append(blocks, b)
	}
	if len(failures) == len(paths) {
		return engine.Result{}, errors.New(strings.Join(failures, "; "))
	}
	return engine.Result{Text: s.shapeBlocks(ctx, blocks, d, tools)}, nil
}

// logBlock is one file of a skim before the call's budget shapes it: rows of nib's own before and after its windows, and the mark the read
// leaves once what it showed is known (nil when the read left it already).
type logBlock struct {
	display    string
	head, tail []string
	windows    []*logWindow
	echoed     bool
	sess       *session
	canonical  string
	next       *logMark
	// complete is the file's last line with its line break; a last line that may still grow does not count as shown.
	complete int
}

// logWindow is a run of a file's lines, the first after file line before; notes name what was left out of them before the cut, and items
// are the rows still to show, numbered from 1.
type logWindow struct {
	lines  []string
	before int
	notes  []string
	items  []item
}

// rows are the window's items as rows standing for log lines from shift on, one cut at rowLimit labeled with its line from label on.
func (w *logWindow) rows(label, shift int) []row {
	rows := make([]row, len(w.items))
	for i, it := range w.items {
		rows[i] = plainRowAt(it.text, label+it.first, shift+it.first, shift+it.last, nil)
	}
	return rows
}

// fit is the window's rows shaped as the log of their own, which numbers them from log.base on, as the file does; footer explains cut rows.
func (w *logWindow) fit(ctx context.Context, d decision, log saved) (rows []row, footer []string) {
	if rows = w.rows(log.base, 0); len(rows) == 0 {
		return nil, nil
	}
	return fit(ctx, rows, strings.Join(w.lines, "\n"), d, func() saved { return log })
}

// shapeLines is lines as a window after log.base, shaped as its fit does; echoed says a recurring block got markers.
func shapeLines(ctx context.Context, lines []string, d decision, log saved) (rows []row, footer []string, echoed bool) {
	w := &logWindow{lines: lines, before: log.base}
	w.items, echoed = windowItems(lines, func(int, int) bool { return false })
	rows, footer = w.fit(ctx, d, log)
	return rows, footer, echoed
}

// shown adds to the block's mark the file lines that rows show of w, whose lines are the log's from offset on.
func (b *logBlock) shown(w *logWindow, rows []row, offset int) {
	b.next.shown = shownIn(rows, offset, len(w.lines), w.before, b.complete, b.next.shown)
}

// shownIn is set with the file lines up to limit that rows show of n lines after file line before, which are the log's from offset on.
func shownIn(rows []row, offset, n, before, limit int, set lineSet) lineSet {
	for _, r := range rows {
		first, last := r.first-offset, r.last-offset
		if r.plain && first > 0 && last <= n && before+first <= limit {
			set = set.with(before+first, min(before+last, limit))
		}
	}
	return set
}

// finish leaves the block's mark.
func (b *logBlock) finish() {
	if b.next != nil {
		b.sess.setMark(b.canonical, *b.next)
	}
}

// readLog is one file's block: a head line of facts, then each window of its lines, every run of lines the agent was shown before left out
// and named in a note; asked is the read's expect as question gives it.
func (s *Shell) readLog(sess *session, entry, asked string, tools *engine.Context) (*logBlock, error) {
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
		return nil, err
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
		return &logBlock{head: []string{fmt.Sprintf("[%s] %d lines, plain: nothing new since your last %s with this expect", file.Display, total, engine.Skim)}}, nil
	}
	b := &logBlock{display: file.Display, sess: sess, canonical: file.Canonical}
	switch {
	case len(lines) == 0 && !resumed:
		sess.setMark(file.Canonical, next)
		b.head = []string{fmt.Sprintf("[%s] empty, plain", file.Display)}
		return b, nil
	case file.Ranges != nil:
		var parts []string
		for _, r := range file.Ranges {
			if r[0] == r[1] {
				parts = append(parts, fmt.Sprint(r[0]))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
			}
		}
		b.head = []string{fmt.Sprintf("[%s] lines %s of %d, plain", file.Display, strings.Join(parts, ", "), total)}
	case resumed:
		b.head = []string{fmt.Sprintf("[%s] lines %d-%d of %d, plain: new since your last %s", file.Display, base+1, total, total, engine.Skim)}
	case rewritten:
		b.head = []string{fmt.Sprintf("[%s] %d lines, plain: rewritten since your last %s, so all of it", file.Display, total, engine.Skim)}
	default:
		b.head = []string{fmt.Sprintf("[%s] %d lines, plain", file.Display, total)}
	}
	// What nib shape showed went to a tool's result, which no agent can be told apart for; a selector asks for its lines again.
	var shaped logMark
	if m, ok := s.shapedMark(file.Canonical); ok && whole && !m.rewrittenIn(file) {
		shaped = m
	}
	if !whole {
		earlier.shown = nil
	}
	b.complete = base + bytes.Count(raw, []byte("\n"))
	windows := file.Ranges
	if windows == nil {
		windows = [][2]int{{base + 1, total}}
	}
	for _, w := range windows {
		win := &logWindow{lines: lines[w[0]-1-base : w[1]-base], before: w[0] - 1}
		var mine, theirs lineSet
		var echoed bool
		win.items, echoed = windowItems(win.lines, func(first, last int) bool {
			first, last = win.before+first, win.before+last
			switch {
			case earlier.shown.holds(first, last):
				mine = mine.with(first, last)
			case shaped.shown.holds(first, last):
				theirs = theirs.with(first, last)
			default:
				return false
			}
			return true
		})
		if len(mine) > 0 {
			win.notes = append(win.notes, fmt.Sprintf("[%s: %s left out, which your earlier %[1]s showed]", engine.Skim, saved{}.spans(mine)))
		}
		if len(theirs) > 0 {
			win.notes = append(win.notes, fmt.Sprintf("[%s: %s left out, which a tool's result showed through nib shape at %s; read %s:%s if you lack them]",
				engine.Skim, saved{}.spans(theirs), shaped.at.Format("15:04"), file.Display, theirs.selector()))
		}
		b.windows = append(b.windows, win)
		b.echoed = b.echoed || echoed
	}
	b.next = &next
	if file.Short {
		if whole {
			b.tail = []string{fmt.Sprintf("[%s: stopped after %d MiB, at line %d; the next %[1]s goes on from there]", engine.Skim, logLimit>>20, total)}
		} else {
			b.tail = []string{fmt.Sprintf("[%s: only the first %d MiB of the file were read]", engine.Skim, logLimit>>20)}
		}
	}
	return b, nil
}

// shapeBlocks is a skim's text. Within the limits each window is shaped on its own, numbered as its file numbers its lines; past them, the
// windows of several are one output, so many short files cannot add up to more than one long one.
func (s *Shell) shapeBlocks(ctx context.Context, blocks []*logBlock, d decision, tools *engine.Context) string {
	windows := 0
	for _, b := range blocks {
		windows += len(b.windows)
	}
	if windows > 1 {
		if text, ok := s.joined(ctx, blocks, d, tools); ok {
			return text
		}
	}
	texts := make([]string, len(blocks))
	for i, b := range blocks {
		out := slices.Clone(b.head)
		d.command = "cat " + b.display
		for _, w := range b.windows {
			out = append(out, w.notes...)
			rows, footer := w.fit(ctx, d, saved{path: b.display, lines: len(w.lines), holds: len(w.lines), file: true, base: w.before})
			b.shown(w, rows, 0)
			out = append(append(out, rowTexts(rows)...), footer...)
		}
		b.finish()
		out = append(out, b.tail...)
		if b.echoed {
			out = append(out, s.engine.EchoNote(tools)...)
		}
		texts[i] = strings.Join(out, "\n")
	}
	return strings.Join(texts, "\n\n")
}

// joined shapes the windows of blocks as one output when together they are due for it, and says whether they were. The rows of nib's own
// about each file are lines of that output too, which a nib log holds, and every row stands under its file's head row, which a cut repeats.
func (s *Shell) joined(ctx context.Context, blocks []*logBlock, d decision, tools *engine.Context) (string, bool) {
	var rows []row
	var lines, displays []string
	head := -1
	own := func(text string) {
		lines = append(lines, text)
		r := plainRow(text, len(lines), len(lines), nil)
		r.header = head
		rows = append(rows, r)
	}
	type placed struct {
		block  *logBlock
		window *logWindow
		offset int
	}
	var at []placed
	echoed := false
	for _, b := range blocks {
		head = -1
		for _, text := range b.head {
			own(text)
			if head < 0 {
				head = len(rows) - 1
			}
		}
		for _, w := range b.windows {
			for _, note := range w.notes {
				own(note)
			}
			offset := len(lines)
			lines = append(lines, w.lines...)
			for _, r := range w.rows(offset, offset) {
				r.header = head
				rows = append(rows, r)
			}
			at = append(at, placed{b, w, offset})
		}
		for _, text := range b.tail {
			own(text)
		}
		if len(b.windows) > 0 {
			displays = append(displays, b.display)
		}
		echoed = echoed || b.echoed
	}
	raw := strings.Join(lines, "\n")
	if !shapeDue(rows, raw, d) {
		return "", false
	}
	d.command = "cat " + strings.Join(displays, " ")
	var log *saved
	rows, footer := fit(ctx, rows, raw, d, func() saved {
		if log == nil {
			written := s.logs.save(raw + "\n")
			written.joined = true
			log = &written
		}
		return *log
	})
	for _, p := range at {
		p.block.shown(p.window, rows, p.offset)
	}
	for _, b := range blocks {
		b.finish()
	}
	out := append(rowTexts(rows), footer...)
	if echoed {
		out = append(out, s.engine.EchoNote(tools)...)
	}
	return strings.Join(out, "\n"), true
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

// rowTexts are the texts of rows as the agent reads them.
func rowTexts(rows []row) []string {
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.printed()
	}
	return texts
}

// windowItems are lines as items numbered from 1, without those known says the agent has, which shape never sees, so its budget goes to the
// rest while the deciders still read every line; echoed says a recurring block got markers.
func windowItems(lines []string, known func(first, last int) bool) (items []item, echoed bool) {
	if len(lines) == 0 {
		return nil, false
	}
	plain := make([]engine.PlainLine, len(lines))
	at := make([]int, len(lines))
	for i, line := range lines {
		plain[i] = engine.PlainLine{At: i + 1, Text: line}
		at[i] = i + 1
	}
	all, echoed := plainItems(plain, at, make([]*listedLine, len(lines)), 0)
	for _, it := range all {
		if !known(it.first, it.last) {
			items = append(items, it)
		}
	}
	return items, echoed
}
