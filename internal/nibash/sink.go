package nibash

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/server"
)

const (
	// Output past either limit keeps its first headRows and last tailRows rows; the whole goes to a log.
	budgetLines = 400
	budgetBytes = 30 * 1024
	headRows    = 100
	tailRows    = 200
	// A decider keeps decidedHead and decidedTail rows and chooses the middle, up to headRows+tailRows rows and budgetBytes in all; each
	// passage it picks is widened by pickContext rows on both sides.
	decidedHead = 30
	decidedTail = 100
	pickContext = 3
	// A pick goes in best first unless fewer than pickNovelty of the kinds of row it holds are new (novelFirst); then it waits for the rest.
	pickNovelty = 0.25
	// A summarizer reads the first excerptHead and last excerptTail deduplicated entries and the decider's picks scored excerptScore or
	// more, each widened by excerptContext lines, up to excerptLines entries in all.
	excerptHead    = 30
	excerptTail    = 100
	excerptScore   = 0.3
	excerptContext = 10
	excerptLines   = 600
	// A summary shows the lines it points at, each widened by summaryContext lines, sharing summaryRows rows, at least summaryShare each; it
	// points only at lines the agent must read as printed, so these stay few.
	summaryContext = 1
	summaryRows    = 30
	summaryShare   = 3
	// A failed call's summary is followed by its output's last failedTail rows as printed.
	failedTail = 20
	// passageBytes bounds the rows a summary's passages show in all, each row at most passageColumns and at least passageLeast characters
	// wide; a pointed-at line can be thousands of characters of JSON or encoded data the summary already told.
	passageBytes   = 4 * 1024
	passageColumns = 240
	passageLeast   = 80
	// Output within the budget that reports listProblems or more problem lines, listPerKind or more of each kind, is shown whole rather
	// than summarized (problemList).
	listProblems = 10
	listPerKind  = 3
	rowLimit     = engine.LineLimit
	// With a summarizer, plain output past summaryLines lines or summaryBytes gets a summary even within the budget: at 8 KB the captures
	// lost an end-to-end run's results to it, at 12 KB none but a list whose every line counted.
	summaryLines = 150
	summaryBytes = 12 * 1024
	// Output that shrinks under dedupe more than summaryShrink times gets a summary sooner, the limits divided by how much more it shrinks,
	// though never under summaryFloorLines lines and summaryFloorBytes; every recorded output under the limits shrinks 1.06 times or less.
	summaryShrink     = 1.5
	summaryFloorLines = 50
	summaryFloorBytes = 3 * 1024
	// collapseAfter is how long plain output gets before repeats in it collapse.
	collapseAfter = 20
)

// logLimit caps a log, so a runaway program cannot fill the disk; a variable so a test can reach it.
var logLimit = 64 * 1024 * 1024

// A summarizer reads a deduplicated output whole when it holds no more than excerptLines entries, or takes no more than wholeBytes as it
// reads them: some 24k tokens, which a skim of a few dozen short files fits; a variable so a test can reach the excerpt with short lines.
var wholeBytes = 96 * 1024

// sink is the terminal: what reaches the model. It takes records from nib commands and bytes from everything else, in the order they come;
// once detached, what comes goes on to a job's log instead.
type sink struct {
	mu       sync.Mutex
	entries  []entry
	facts    []func() string
	detached *rawLog
	// tails are the tails of program output at the model's end that left lines out, which render may check.
	tails []*tailed
}

type entry struct {
	record *record
	bytes  []byte
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached != nil {
		s.detached.bytes(p)
		return len(p), nil
	}
	if n := len(s.entries); n > 0 && s.entries[n-1].record == nil {
		s.entries[n-1].bytes = append(s.entries[n-1].bytes, p...)
	} else {
		s.entries = append(s.entries, entry{bytes: bytes.Clone(p)})
	}
	return len(p), nil
}

func (s *sink) add(r record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached != nil {
		s.detached.record(r.text)
		return
	}
	copied := r
	s.entries = append(s.entries, entry{record: &copied})
}

// detach writes what the sink holds to w as its log text and sends all that comes later there too; what it holds stays for render. It returns
// how many whole lines it wrote.
func (s *sink) detach(w io.Writer) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := &rawLog{w: w}
	for _, en := range s.entries {
		if en.record == nil {
			log.bytes(en.bytes)
		} else {
			log.record(en.record.text)
		}
	}
	s.detached = log
	return log.newlines
}

// rawLog writes the log text of what a sink takes: bytes as they come, and each record on a line of its own.
type rawLog struct {
	w io.Writer
	// newlines counts the line breaks written; open is true while the text written does not end in one.
	newlines int
	open     bool
}

func (l *rawLog) bytes(p []byte) {
	if len(p) == 0 {
		return
	}
	_, _ = l.w.Write(p)
	l.newlines += bytes.Count(p, []byte("\n"))
	l.open = p[len(p)-1] != '\n'
}

// record writes a record's line and returns its line number in the log.
func (l *rawLog) record(text string) int {
	if l.open {
		l.bytes([]byte("\n"))
	}
	at := l.newlines + 1
	l.bytes([]byte(text + "\n"))
	return at
}

// fact adds a line computed when the call ends, after every stage has finished.
func (s *sink) fact(f func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = append(s.facts, f)
}

// addTail adds a tail that left lines of program output out: its note is a fact, which render checks first.
func (s *sink) addTail(t *tailed) {
	s.mu.Lock()
	s.tails = append(s.tails, t)
	s.mu.Unlock()
	s.fact(t.note)
}

// item is one rendered unit before budgeting: an anchored row of a file, or a plain line. first and last are the log lines it stands for.
type item struct {
	source  *engine.Source
	line    int
	text    string
	anchor  bool
	marker  string
	gap     bool
	plain   bool
	display bool
	listed  *listedLine
	// kept are the stretches of the line a command like cut kept, nil for the whole line.
	kept        []engine.Stretch
	first, last int
}

// plainLines splits program bytes into display lines: a carriage-return overwrite keeps the line's final text, colors go, invalid UTF-8 shows
// as U+FFFD.
func plainLines(data []byte) []string {
	text := strings.ToValidUTF8(string(data), "\ufffd")
	text = strings.TrimSuffix(text, "\n")
	if text == "" && len(data) == 0 {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if at := strings.LastIndex(line, "\r"); at >= 0 {
			line = line[at+1:]
		}
		lines[i] = engine.PlainText(line)
	}
	return lines
}

// decision is what a decider and a summarizer need to know about a call besides its output.
type decision struct {
	decider    decider.Decider
	summarizer decider.Summarizer
	command    string
	purpose    string
	expect     string
	exit       int
	timedOut   bool
	// failed says a program of the call failed where its exit status does not say so, as a failureNote names it.
	failed bool
	// read says the purpose is skim's expect, not a call's description.
	read bool
	// recall folds blocks the agent saw in an earlier call; nil folds none.
	recall *recall
	// teach says whether the agent is to learn a lesson now, marking it learned; render sets it for its agent, and nil teaches every time.
	teach func(lesson string) bool
}

// asked names the purpose in notes: the call's description, or what the agent expects when it gave only that or read with skim.
func (d decision) asked() string {
	if d.read || d.purpose == "" && d.expect != "" {
		return "what you expect"
	}
	return "your description"
}

// render turns what the sink holds into the tool result, records the anchored rows it shows as seen, and writes the whole output to a log when
// the budget cuts it or a row is too long to show whole.
func (s *sink) render(e *engine.Engine, ctx *engine.Context, logs *logDirectory, d decision) string {
	if d.teach == nil {
		d.teach = func(lesson string) bool { return e.Store.Teach(ctx.Session, lesson) }
	}
	s.mu.Lock()
	facts, tails := s.facts, s.tails
	s.mu.Unlock()
	rows, raw, echoed, own := s.collect(e, ctx)
	// One log serves the cut and the folds.
	var log *saved
	save := func() saved {
		if log == nil {
			written := logs.save(raw)
			log = &written
		}
		return *log
	}
	rows, footer := fit(ctx.Ctx, rows, raw, d, save)
	least := foldMin
	if d.exit != 0 || d.failed {
		least = foldFailedMin
	}
	rows = d.recall.fold(ctx.Session, e.Store.Cleared(ctx.Session), rows, least, save)
	// Only what is shown is laid out by folder, so a cut never leaves a path without its folder row.
	rows = groupListings(rows)
	// Only rows that are shown count as seen.
	seen := map[*engine.Source][]int{}
	var texts []string
	for _, r := range rows {
		texts = append(texts, r.printed())
		if r.anchor {
			seen[r.source] = append(seen[r.source], r.line)
		}
		seen[r.source] = append(seen[r.source], r.covers...)
	}
	for source, lines := range seen {
		if source != nil {
			e.Seen(ctx, source.Canonical, *source.View, lines, nil)
		}
	}
	texts = append(texts, footer...)
	if slices.ContainsFunc(rows, func(r row) bool { return r.covers != nil && !r.mapped }) {
		texts = append(texts, e.HeldNote(ctx)...)
	}
	if echoed {
		texts = append(texts, e.EchoNote(ctx)...)
	}
	if slices.ContainsFunc(rows, func(r row) bool { return r.plain && slices.Contains(own, r.first) }) {
		texts = append(texts, e.OwnAuthorNote(ctx)...)
	}
	out := strings.Join(texts, "\n")
	for _, t := range tails {
		t.check(ctx.Ctx, d)
	}
	var extra []string
	for _, f := range facts {
		if text := f(); text != "" {
			extra = append(extra, text)
		}
	}
	if len(extra) > 0 {
		if out != "" {
			out += "\n"
		}
		out += strings.Join(extra, "\n")
	}
	return out
}

// fit brings rows within the budget and saves the log when it cuts them or a row was cut at rowLimit; footer explains cut rows.
func fit(ctx context.Context, rows []row, raw string, d decision, save func() saved) ([]row, []string) {
	due := shapeDue(rows, raw, d)
	if !due && !slices.ContainsFunc(rows, func(r row) bool { return r.capped }) {
		return rows, nil
	}
	log := save()
	if due {
		rows = shape(ctx, rows, raw, log, d)
	}
	if !slices.ContainsFunc(rows, func(r row) bool { return r.capped }) {
		return rows, nil
	}
	return rows, []string{cutFooter(log, d.teach)}
}

// cutFooter explains plain rows cut at rowLimit, numbered with their line in the log: in full the first time teach says so, then briefly.
func cutFooter(log saved, teach func(lesson string) bool) string {
	if log.path == "" {
		return "[cut rows: N is the row's line in the whole output, which could not be saved]"
	}
	if teach != nil && !teach("log-cut-rows") {
		return fmt.Sprintf("[cut rows: read %s:N whole]", log.path)
	}
	return fmt.Sprintf("[cut rows: N is the row's line in the %s; read %s:N with maxLineLength to see one whole]", log.whole(), log.path)
}

// overBudget says whether rows are past budgetLines or budgetBytes.
func overBudget(rows []row) bool { return len(rows) > budgetLines || size(rows) > budgetBytes }

// shapeDue says whether rows go to shape: past the budget, or due a summary.
func shapeDue(rows []row, raw string, d decision) bool {
	return overBudget(rows) || d.summarizer != nil && summaryDue(rows, raw)
}

// summaryDue says whether output gets a summary: past summaryLines or summaryBytes, or past lower limits the more it shrinks under dedupe.
func summaryDue(rows []row, raw string) bool {
	lines, bytes := len(rows), size(rows)
	if lines > summaryLines || bytes > summaryBytes {
		return true
	}
	if lines <= summaryFloorLines && bytes <= summaryFloorBytes || slices.ContainsFunc(rows, func(r row) bool { return !r.plain }) {
		return false
	}
	log := plainLines([]byte(raw))
	entries, _ := dedupe(log)
	ratio := float64(len(log)) / float64(max(1, len(entries)))
	if ratio <= summaryShrink {
		return false
	}
	scale := summaryShrink / ratio
	return float64(lines) > float64(summaryLines)*scale || float64(bytes) > float64(summaryBytes)*scale
}

// collect lays out what the sink holds as rows, each with the log lines it stands for, and the log's text; echoed is whether a recurring block
// of program output got [#N] markers, and own are the log lines of commit headers printed without their author's email.
func (s *sink) collect(e *engine.Engine, ctx *engine.Context) ([]row, string, bool, []int) {
	s.mu.Lock()
	entries := s.entries
	s.mu.Unlock()
	// How far each part of git's reshaped output reached the terminal decides how its lines print.
	reached := map[*gitPart]int{}
	for _, en := range entries {
		if r := en.record; r != nil && r.shape != nil && r.shape.counts() {
			reached[r.shape.part]++
		}
	}
	var items []item
	var raw strings.Builder
	log := &rawLog{w: &raw}
	var plain []engine.PlainLine
	echoed := false
	var plainAt, own []int
	var plainListed []*listedLine
	// plainEnd is the last log line the plain lines stand for, past the last of them when lines that print as part of it follow.
	plainEnd := 0
	flushPlain := func() {
		if len(plain) == 0 {
			return
		}
		more, echo := plainItems(plain, plainAt, plainListed, plainEnd)
		items = append(items, more...)
		echoed = echoed || echo
		plain, plainAt, plainListed, plainEnd = nil, nil, nil, 0
	}
	for _, en := range entries {
		if en.record == nil {
			first := log.newlines + 1
			if bytes.IndexByte(en.bytes, 0) >= 0 {
				flushPlain()
				log.bytes(en.bytes)
				last := log.newlines
				if log.open {
					last++
				}
				items = append(items, item{text: fmt.Sprintf("[binary output, %d bytes; not shown]", len(en.bytes)), plain: true, first: first, last: max(first, last)})
				continue
			}
			for i, line := range plainLines(en.bytes) {
				plain = append(plain, engine.PlainLine{At: len(plain) + 1, Text: line})
				plainAt = append(plainAt, first+i)
				plainListed = append(plainListed, nil)
			}
			log.bytes(en.bytes)
			continue
		}
		r := en.record
		at := log.record(r.text)
		if r.separator {
			continue
		}
		text := r.text
		if r.shape != nil {
			shown, ok := r.shape.view(text, reached)
			if !ok {
				// A line the row before it stands for: a commit header's one line, or a file's header.
				if len(plain) > 0 {
					plainEnd = at
				}
				continue
			}
			if r.shape.role == commitLine && r.shape.part.own && shown == r.shape.part.header {
				own = append(own, at)
			}
			text = shown
		}
		if r.origin == nil {
			plain = append(plain, engine.PlainLine{At: len(plain) + 1, Text: engine.PlainText(text)})
			plainAt = append(plainAt, at)
			plainListed = append(plainListed, r.listed)
			continue
		}
		flushPlain()
		marker := ""
		switch {
		case r.match:
			marker = "*"
		case r.added:
			marker = "+"
		case r.context:
			marker = " "
		}
		if r.display {
			items = append(items, item{source: r.origin.source, text: engine.PlainText(text), display: true, first: at, last: at})
			continue
		}
		items = append(items, item{source: r.origin.source, line: r.origin.line, marker: marker, kept: r.kept, first: at, last: at})
	}
	flushPlain()
	return s.layout(e, ctx, items), raw.String(), echoed, own
}

// plainItems are lines of program output as items, each standing for the log lines at gives it, the last up to end when that is further;
// echoed says a recurring block got [#N] markers.
func plainItems(plain []engine.PlainLine, at []int, listed []*listedLine, end int) (items []item, echoed bool) {
	// Program output collapses only exact repeats, since lines that differ in a number may differ in what matters, and only when there is
	// enough of it that collapsing pays.
	rows := plain
	if len(plain) > collapseAfter {
		rows, _, echoed = engine.Compress(plain, false)
	}
	// A row stands for the log lines from its own to the next row's, which covers what a collapsed run left out.
	end = max(end, at[len(at)-1])
	for k, r := range rows {
		first, last := at[r.At-1], end
		if k+1 < len(rows) {
			last = at[rows[k+1].At-1] - 1
		}
		// A collapsed run's row is no line the program printed.
		var line *listedLine
		if r.Text == plain[r.At-1].Text {
			line = listed[r.At-1]
		}
		items = append(items, item{text: r.Text, plain: true, listed: line, first: first, last: max(first, last)})
	}
	return items, echoed
}

// size is the bytes rows take as the agent reads them, each with its line break.
func size(rows []row) int {
	n := 0
	for _, r := range rows {
		n += len(r.printed()) + 1
	}
	return n
}

// row is one rendered row; header is the index of the file header an anchored row, or a plain row of joined skim output, sits under, -1 for
// other plain rows and a header itself. first and last are the log lines it stands for, 0 for a row of nib's own such as a file header;
// capped marks a plain row cut at rowLimit.
type row struct {
	text        string
	header      int
	source      *engine.Source
	line        int
	anchor      bool
	match       bool
	plain       bool
	capped      bool
	first, last int
	// label is the log lines a row of output with gaps in it prints before its text, as N or N-M, so its numbers show every gap.
	label string
	// covers are the lines a held marker stands for; a match row (grep's match, a line a diff adds) is what the command is for, so it always
	// prints.
	covers []int
	// mapped is a held line printed as its N~ row rather than inside a marker, which needs no lesson.
	mapped bool
	// listed is the path a plain row's line names, which the terminal may print under its folder.
	listed *listedLine
}

// printed is the row as the agent reads it: its text, after its label when it has one.
func (r row) printed() string {
	if r.label == "" {
		return r.text
	}
	return r.label + "|" + r.text
}

// plainFollows separates a file's rows from plain output shaped like them, which is not an anchor.
const plainFollows = "[plain program output follows, not anchors]"

// layout groups anchored rows under their file's header and checks each against the latest version, so an N: row is always a real line.
// A plain row too long to show whole is cut in read's bracketed form, numbered with its log line, so it never looks like an anchor; plain
// output shaped like rows (grep -n through a real program) that follows a file's rows gets a line of nib's own between them.
func (s *sink) layout(e *engine.Engine, ctx *engine.Context, items []item) []row {
	var rows []row
	var current *engine.Source
	headerAt := -1
	for _, it := range items {
		if it.plain {
			if current != nil && engine.RowShaped.MatchString(it.text) {
				rows = append(rows, row{text: plainFollows, header: -1})
			}
			current = nil
			rows = append(rows, plainRow(it.text, it.first, it.last, it.listed))
			continue
		}
		source := it.source
		if current != source {
			current = source
			rows = append(rows, row{text: header(e, ctx, source), header: -1, source: source})
			headerAt = len(rows) - 1
		}
		if it.display {
			text := it.text
			if js.Len(text) > rowLimit {
				text = string([]rune(text)[:rowLimit]) + "..."
			}
			rows = append(rows, row{text: text, header: headerAt, first: it.first, last: it.first})
			continue
		}
		content, latest := lineOf(e, ctx, source, it.line)
		if it.kept != nil {
			// Part of the line is no anchor, whatever version it shows.
			text, shown := engine.KeptRow(it.line, content, it.kept, rowLimit)
			if !latest {
				text = fmt.Sprintf("%d|%s", it.line, shown)
			}
			rows = append(rows, row{text: it.marker + text, header: headerAt, first: it.first, last: it.first})
			continue
		}
		if latest {
			text, anchor := engine.Row(it.line, content, rowLimit)
			rows = append(rows, row{text: it.marker + text, header: headerAt, source: source, line: it.line, anchor: anchor, match: it.marker == "*" || it.marker == "+", first: it.first, last: it.first})
			continue
		}
		shown := engine.Visible(content)
		if len([]rune(shown)) > rowLimit {
			shown = string([]rune(shown)[:rowLimit]) + "..."
		}
		rows = append(rows, row{text: fmt.Sprintf("%s%d|%s", it.marker, it.line, shown), header: headerAt, first: it.first, last: it.first})
	}
	return heldRows(e, ctx, rows)
}

// plainRow is a row of program output standing for log lines first to last; one too long to show whole is cut in read's bracketed form,
// numbered with its log line, so it never looks like an anchor.
func plainRow(text string, first, last int, listed *listedLine) row {
	return plainRowAt(text, first, first, last, listed)
}

// plainRowAt is plainRow with a cut row numbered label, its line in a file whose lines the log starts after. A line holding encoded data shows
// without it, as briefLine cuts it, and the log keeps it whole.
func plainRowAt(text string, label, first, last int, listed *listedLine) row {
	capped := false
	switch {
	case len(encodedRuns(text)) > 0:
		text, capped = briefLine(label, text, rowLimit)
	case js.Len(text) > rowLimit:
		text, _ = engine.Row(label, text, rowLimit)
		capped = true
	}
	return row{text: text, header: -1, plain: true, capped: capped, listed: listed, first: first, last: last}
}

// heldRows puts each run of anchored rows the agent already holds as N~ rows under their current numbers, or, past engine's limit for
// those, as one held marker that stands for their log lines.
func heldRows(e *engine.Engine, ctx *engine.Context, rows []row) []row {
	done := map[*engine.Source]bool{}
	for _, r := range rows {
		source := r.source
		if source == nil || source.View == nil || done[source] {
			continue
		}
		done[source] = true
		rows = engine.HeldGroups(rows, e.HeldLines(ctx, source.Canonical, *source.View), func(r row) int {
			if r.source != source || !r.anchor || r.match {
				return 0
			}
			return r.line
		}, func(run []row, h engine.HeldRun) []row {
			if !h.Mapped() {
				return []row{{text: e.MarkerFor(ctx, source.Canonical, *source.View, h), header: run[0].header, source: source, covers: h.Lines(), first: run[0].first, last: run[len(run)-1].last}}
			}
			mapped := make([]row, len(run))
			for i, r := range run {
				line := h.First + i
				text, _ := lineOf(e, ctx, source, line)
				mapped[i] = row{text: engine.MapRow(line, text), header: r.header, source: source, covers: []int{line}, mapped: true, first: r.first, last: r.last}
			}
			return mapped
		})
	}
	return rows
}

// header is a file's [DISPLAY#TAG] line, or [DISPLAY] when nib could not hold it as one version. A version the session no longer holds,
// dropped as the command showed more files than the store keeps, is still the one the tag names, so only a newer one makes it stale.
func header(e *engine.Engine, ctx *engine.Context, source *engine.Source) string {
	if source.View == nil {
		return fmt.Sprintf("[%s] (not anchorable: over %s, binary or changing)", source.Display, engine.TextLimitName)
	}
	if latest := e.Latest(ctx, source.Canonical); latest != nil && latest.Key != source.View.Key {
		return fmt.Sprintf("[%s] (changed since this command read it; read it again to edit)", source.Display)
	}
	return fmt.Sprintf("[%s#%s]", source.Display, source.View.Tag)
}

// lineOf is a source line's text and whether that version is still the latest the session holds, or the session holds none, which makes
// the row an anchor.
func lineOf(e *engine.Engine, ctx *engine.Context, source *engine.Source, line int) (string, bool) {
	if source.View == nil {
		text := ""
		_ = source.Lines(func(n int, t string) bool {
			if n == line {
				text = t
				return false
			}
			return true
		})
		return text, false
	}
	if line < 1 || line > len(source.View.Lines) {
		return "", false
	}
	content := source.View.Lines[line-1]
	latest := e.Latest(ctx, source.Canonical)
	return content, latest == nil || latest.Key == source.View.Key
}

// fixedCut keeps the first headRows and last tailRows rows, with a note between them that may carry why no decider chose (because), and
// repeats the file header the tail starts under.
func fixedCut(rows []row, log saved, because string) []row {
	total := len(rows)
	if total <= headRows+tailRows {
		return rows
	}
	start := total - tailRows
	kept := append([]row{}, rows[:headRows]...)
	var header []row
	if h := rows[start].header; h >= headRows {
		header = append(header, rows[h])
	}
	shown := headRows + len(header) + tailRows
	clauses := []string{log.leftOut(rows[headRows:start])}
	if because != "" {
		clauses = append(clauses, because)
	}
	kept = append(kept, row{text: closing(shown, log, logEnding, clauses), header: -1})
	kept = append(kept, header...)
	return append(kept, rows[start:]...)
}

// logDirectory holds the shell's logs in a private directory; one per shell, so its numbering keeps every agent's logs apart.
type logDirectory struct {
	mu   sync.Mutex
	path string
	next int
}

func newLogDirectory() *logDirectory {
	return &logDirectory{path: server.RuntimeDirectory()}
}

// file is a new log file's path, creating the private directory first.
func (l *logDirectory) file(kind string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.path, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(l.path, 0o700); err != nil {
		return "", err
	}
	l.next++
	return filepath.Join(l.path, fmt.Sprintf("%d-%s-%d.log", os.Getpid(), kind, l.next)), nil
}

// The kinds of log a shell writes: a long output's and a job's.
const (
	outputKind = "output"
	jobKind    = "job"
)

var nibLog = regexp.MustCompile(`^[0-9]+-(?:` + outputKind + `|` + jobKind + `)-[0-9]+\.log$`)

// holds reports whether path, an absolute path, names a log a nib shell wrote, this server's or another's.
func (l *logDirectory) holds(path string) bool {
	return filepath.Dir(filepath.Clean(path)) == l.path && nibLog.MatchString(filepath.Base(path))
}

// saved is a log as written: its path ("" when it could not be written), the lines of the whole output, and how many of them it holds. A
// file read with skim is its own log, whose notes name skim and number its lines from base on: the log's line N is the file's base+N. The
// files of a skim shaped together are joined in a log of their own, whose notes name skim too; files counts them, 0 for any other log.
type saved struct {
	path         string
	lines, holds int
	file         bool
	files        int
	base         int
}

// note is a row of nib's own about the log.
func (l saved) note(text string) string {
	if l.file || l.files > 0 {
		return "[" + engine.Skim + ": " + text + "]"
	}
	return "[nibash: " + text + "]"
}

// whole names what the log holds.
func (l saved) whole() string {
	switch {
	case l.file:
		return "whole file"
	case l.files > 0:
		return "joined files"
	}
	return "whole output"
}

// passages is the rows and bytes a summary's passages share: what one output's get for each file a joined skim's log holds, within two
// thirds of budgetBytes, the rest left to the summary of each file, and within as many rows as leave each half of passageColumns.
func (l saved) passages() (rows, bytes int) {
	files := max(1, l.files)
	bytes = min(budgetBytes*2/3, passageBytes*files)
	return min(summaryRows*files, bytes*2/passageColumns), bytes
}

// span names log lines first to last as notes number them.
func (l saved) span(first, last int) string { return lineSpan(l.base+first, l.base+last) }

// numbered is r with its log lines as its label when it is a plain row shown whole that stands for some; a cut row names its line already.
func (l saved) numbered(r row) row {
	if !r.plain || r.capped || r.first == 0 {
		return r
	}
	r.label = fmt.Sprint(l.base + r.first)
	if r.last > r.first {
		r.label += fmt.Sprintf("-%d", l.base+r.last)
	}
	return r
}

// save writes text to a new log, capped at logLimit.
func (l *logDirectory) save(text string) saved {
	s := saved{lines: lineCount(text)}
	path, err := l.file(outputKind)
	if err != nil {
		return s
	}
	if len(text) > logLimit {
		text = text[:logLimit]
	}
	if os.WriteFile(path, []byte(text), 0o600) != nil {
		return s
	}
	s.path, s.holds = path, lineCount(text)
	return s
}

// lineCount counts lines as sed does: a last line without its line break counts.
func lineCount(text string) int {
	n := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}
