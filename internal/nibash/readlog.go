package nibash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/11ways/nib/internal/engine"
)

// logMark is how far a file was read with skim: its size and start then, and the whole lines read, which end at offset; partial is how many
// bytes of a last line without its line break were shown after them. shaped marks what nib shape showed, which counts for every agent.
type logMark struct {
	size    int64
	head    []byte
	lines   int
	offset  int64
	partial int
	shaped  bool
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

// since says what a mark of an earlier read is, for the head line of a read going on from it.
func (m logMark) since() string {
	if m.shaped {
		return "since nib shape showed it"
	}
	return "since your last " + engine.Skim
}

// ReadLog reads files for what the agent expects of them, as plain lines shaped the way long program output is: past the limits a decider
// chooses the lines that bear on expect, or a summary leads the lines it points at. A file read whole again shows only what was added
// since, unless it shrank or its start changed.
func (s *Shell) ReadLog(ctx context.Context, key any, paths []string, expect string, tools *engine.Context) (engine.Result, error) {
	sess := s.session(key)
	d := decision{decider: s.decider, summarizer: s.activeSummarizer(), purpose: purpose(expect), read: true}
	var blocks, failures []string
	for _, entry := range paths {
		text, err := s.readLog(ctx, sess, entry, d, tools)
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

// readLog is one file's block: a head line of facts, then each window of its lines as shape leaves it.
func (s *Shell) readLog(ctx context.Context, sess *session, entry string, d decision, tools *engine.Context) (string, error) {
	whole, resumed, rewritten := false, false, false
	// mark is where this read goes on from; previous is the earlier read's mark, rewritten or not, which the head line names.
	var mark, previous logMark
	file, err := s.engine.ReadLog(tools, entry, logLimit, func(f *engine.LogFile) int64 {
		whole = true
		sess.mu.Lock()
		m, ok := sess.marks[f.Canonical]
		sess.mu.Unlock()
		// What nib shape showed counts for every agent, as nothing tells which one asked for it.
		if shaped, found := s.shapedMark(f.Canonical); found && (!ok || shaped.offset > m.offset) {
			m, ok = shaped, true
		}
		if !ok {
			return 0
		}
		if f.Size < m.size || !bytes.HasPrefix(f.Head, m.head) {
			rewritten, previous = true, m
			return 0
		}
		mark, previous, resumed = m, m, true
		return m.offset
	})
	if err != nil {
		return "", err
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
	if whole {
		next := advanced(file, raw, skipped, base)
		sess.mu.Lock()
		if sess.marks == nil {
			sess.marks = map[string]logMark{}
		}
		sess.marks[file.Canonical] = next
		sess.mu.Unlock()
		// A last line still being written was shown whole as it stood; shown again unchanged, it is nothing new.
		if resumed && (len(raw) == 0 || len(raw) == mark.partial && next.lines == mark.lines) {
			return fmt.Sprintf("[%s] %d lines, plain: nothing new %s", file.Display, total, previous.since()), nil
		}
	}
	var out []string
	switch {
	case len(lines) == 0 && !resumed:
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
		out = append(out, fmt.Sprintf("[%s] lines %d-%d of %d, plain: new %s", file.Display, base+1, total, total, previous.since()))
	case rewritten:
		out = append(out, fmt.Sprintf("[%s] %d lines, plain: rewritten %s, so all of it", file.Display, total, previous.since()))
	default:
		out = append(out, fmt.Sprintf("[%s] %d lines, plain", file.Display, total))
	}
	windows := file.Ranges
	if windows == nil {
		windows = [][2]int{{base + 1, total}}
	}
	echoed := false
	for _, w := range windows {
		texts, echo := shapeLines(ctx, lines[w[0]-1-base:w[1]-base], d, saved{path: file.Display, lines: w[1] - w[0] + 1, holds: w[1] - w[0] + 1, file: true, base: w[0] - 1})
		out = append(out, texts...)
		echoed = echoed || echo
	}
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

// shapeLines is lines as rows numbered from 1 that shape reads as the log of their own, while every note and cut row numbers them from
// log.base on, as the file does.
func shapeLines(ctx context.Context, lines []string, d decision, log saved) ([]string, bool) {
	if len(lines) == 0 {
		return nil, false
	}
	plain := make([]engine.PlainLine, len(lines))
	at := make([]int, len(lines))
	for i, line := range lines {
		plain[i] = engine.PlainLine{At: i + 1, Text: line}
		at[i] = i + 1
	}
	items, echoed := plainItems(plain, at, make([]*listedLine, len(lines)))
	rows := make([]row, len(items))
	for i, it := range items {
		rows[i] = plainRowAt(it.text, log.base+it.first, it.first, it.last, nil)
	}
	raw := strings.Join(lines, "\n")
	rows, footer := fit(ctx, rows, raw, d, func() saved { return log })
	texts := make([]string, 0, len(rows)+len(footer))
	for _, r := range rows {
		texts = append(texts, r.text)
	}
	return append(texts, footer...), echoed
}
