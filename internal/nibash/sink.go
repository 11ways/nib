package nibash

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/js"
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
	rowLimit    = engine.LineLimit
	// collapseAfter is how long plain output gets before repeats in it collapse.
	collapseAfter = 20
)

// logLimit caps a log, so a runaway program cannot fill the disk; a variable so a test can reach it.
var logLimit = 64 * 1024 * 1024

// sink is the terminal: what reaches the model. It takes records from nib commands and bytes from everything else, in the order they come;
// once detached, what comes goes on to a job's log instead.
type sink struct {
	mu       sync.Mutex
	entries  []entry
	facts    []func() string
	detached *rawLog
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

// detach writes what the sink holds to w as its log text and sends all that comes later there too; what it holds stays for render.
func (s *sink) detach(w io.Writer) {
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

// item is one rendered unit before budgeting: an anchored row of a file, or a plain line. first and last are the log lines it stands for.
type item struct {
	source      *engine.Source
	line        int
	text        string
	anchor      bool
	marker      string
	gap         bool
	plain       bool
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

// decision is what a decider needs to know about a call besides its output.
type decision struct {
	decider  decider.Decider
	command  string
	purpose  string
	exit     int
	timedOut bool
}

// render turns what the sink holds into the tool result, records the anchored rows it shows as seen, and writes the whole output to a log when
// the budget cuts it or a row is too long to show whole.
func (s *sink) render(e *engine.Engine, ctx *engine.Context, logs *logDirectory, d decision) string {
	s.mu.Lock()
	facts := s.facts
	s.mu.Unlock()
	rows, raw, echoed := s.collect(e, ctx)
	rows, footer := fit(ctx.Ctx, rows, raw, logs, d)
	// Only rows that are shown count as seen.
	seen := map[*engine.Source][]int{}
	var texts []string
	for _, r := range rows {
		texts = append(texts, r.text)
		if r.anchor {
			seen[r.source] = append(seen[r.source], r.line)
		}
	}
	for source, lines := range seen {
		e.Seen(ctx, source.Canonical, *source.View, lines, nil)
	}
	texts = append(texts, footer...)
	if echoed {
		texts = append(texts, e.EchoNote(ctx)...)
	}
	out := strings.Join(texts, "\n")
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
func fit(ctx context.Context, rows []row, raw string, logs *logDirectory, d decision) ([]row, []string) {
	over := len(rows) > budgetLines || size(rows) > budgetBytes
	if !over && !slices.ContainsFunc(rows, func(r row) bool { return r.capped }) {
		return rows, nil
	}
	log := logs.save(raw)
	if over {
		rows = shape(ctx, rows, log, d)
	}
	if !slices.ContainsFunc(rows, func(r row) bool { return r.capped }) {
		return rows, nil
	}
	if log.path == "" {
		return rows, []string{"[cut rows: N is the row's line in the whole output, which could not be saved]"}
	}
	return rows, []string{fmt.Sprintf("[cut rows: N is the row's line in the whole output; read %s:N with maxLineLength to see one whole]", log.path)}
}

// collect lays out what the sink holds as rows, each with the log lines it stands for, and the log's text; echoed is whether a recurring block
// of program output got [#N] markers.
func (s *sink) collect(e *engine.Engine, ctx *engine.Context) ([]row, string, bool) {
	s.mu.Lock()
	entries := s.entries
	s.mu.Unlock()
	var items []item
	var raw strings.Builder
	log := &rawLog{w: &raw}
	var plain []engine.PlainLine
	echoed := false
	var plainAt []int
	flushPlain := func() {
		if len(plain) == 0 {
			return
		}
		// Program output collapses only exact repeats, since lines that differ in a number may differ in what matters, and only when there is
		// enough of it that collapsing pays.
		rows := plain
		if len(plain) > collapseAfter {
			var echo bool
			rows, _, echo = engine.Compress(plain, false)
			echoed = echoed || echo
		}
		// A row stands for the log lines from its own to the next row's, which covers what a collapsed run left out.
		end := plainAt[len(plainAt)-1]
		for k, r := range rows {
			first, last := plainAt[r.At-1], end
			if k+1 < len(rows) {
				last = plainAt[rows[k+1].At-1] - 1
			}
			items = append(items, item{text: r.Text, plain: true, first: first, last: max(first, last)})
		}
		plain, plainAt = nil, nil
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
			}
			log.bytes(en.bytes)
			continue
		}
		r := en.record
		at := log.record(r.text)
		if r.separator {
			continue
		}
		if r.origin == nil {
			plain = append(plain, engine.PlainLine{At: len(plain) + 1, Text: engine.PlainText(r.text)})
			plainAt = append(plainAt, at)
			continue
		}
		flushPlain()
		marker := ""
		if r.match {
			marker = "*"
		} else if r.context {
			marker = " "
		}
		items = append(items, item{source: r.origin.source, line: r.origin.line, marker: marker, first: at, last: at})
	}
	flushPlain()
	return s.layout(e, ctx, items), raw.String(), echoed
}

func size(rows []row) int {
	n := 0
	for _, r := range rows {
		n += len(r.text) + 1
	}
	return n
}

// row is one rendered row; header is the index of the file header an anchored row sits under, -1 for a plain row or a header itself. first and
// last are the log lines it stands for, 0 for a row of nib's own such as a file header; capped marks a plain row cut at rowLimit.
type row struct {
	text        string
	header      int
	source      *engine.Source
	line        int
	anchor      bool
	plain       bool
	capped      bool
	first, last int
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
	previous := 0
	for _, it := range items {
		if it.plain {
			if current != nil && engine.RowShaped.MatchString(it.text) {
				rows = append(rows, row{text: plainFollows, header: -1})
			}
			current = nil
			text, capped := it.text, false
			if js.Len(text) > rowLimit {
				text, _ = engine.Row(it.first, text, rowLimit)
				capped = true
			}
			rows = append(rows, row{text: text, header: -1, plain: true, capped: capped, first: it.first, last: it.last})
			continue
		}
		source := it.source
		if current != source {
			current = source
			previous = 0
			rows = append(rows, row{text: header(e, ctx, source), header: -1})
			headerAt = len(rows) - 1
		}
		if previous != 0 && it.line != previous+1 && it.line != previous {
			rows = append(rows, row{text: "...", header: headerAt})
		}
		previous = it.line
		content, latest := lineOf(e, ctx, source, it.line)
		if latest {
			text, anchor := engine.Row(it.line, content, rowLimit)
			rows = append(rows, row{text: it.marker + text, header: headerAt, source: source, line: it.line, anchor: anchor, first: it.first, last: it.first})
			continue
		}
		shown := engine.Visible(content)
		if len([]rune(shown)) > rowLimit {
			shown = string([]rune(shown)[:rowLimit]) + "..."
		}
		rows = append(rows, row{text: fmt.Sprintf("%s%d|%s", it.marker, it.line, shown), header: headerAt, first: it.first, last: it.first})
	}
	return rows
}

// header is a file's [DISPLAY#TAG] line, or [DISPLAY] when nib could not hold it as one version. A version the session no longer holds,
// dropped as the command showed more files than the store keeps, is still the one the tag names, so only a newer one makes it stale.
func header(e *engine.Engine, ctx *engine.Context, source *engine.Source) string {
	if source.View == nil {
		return fmt.Sprintf("[%s] (not anchorable: over 2 MiB, binary or changing)", source.Display)
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
	clauses := []string{leftOut(rows[headRows:start])}
	if because != "" {
		clauses = append(clauses, because)
	}
	kept = append(kept, row{text: closing(shown, log, clauses), header: -1})
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
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("nib-%d", os.Getuid()))
	} else {
		base = filepath.Join(base, "nib")
	}
	return &logDirectory{path: base}
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

// saved is a log as written: its path ("" when it could not be written), the lines of the whole output, and how many of them it holds.
type saved struct {
	path         string
	lines, holds int
}

// save writes text to a new log, capped at logLimit.
func (l *logDirectory) save(text string) saved {
	s := saved{lines: lineCount(text)}
	path, err := l.file("output")
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
