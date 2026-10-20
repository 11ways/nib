package nibash

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/engine"
)

// captured runs a real program as the shell runs any, and returns the lines it printed and its exit status. Its standard error goes where the
// shell sends it, or into the lines when the shell sends both to one place, as 2>&1 does, so they keep their order; quiet drops it, for a
// second run whose standard error the first already showed.
func captured(inv *invocation, args []string, quiet bool) ([]string, int) {
	var out bytes.Buffer
	status := runInto(inv, args, &out, quiet)
	return splitLines(out.String()), status
}

// streamed runs a real program whose lines go on to another nib command as they come, each made a record by recordOf; once that command
// reads no more, the program's next write fails, as it would in a pipe.
func streamed(inv *invocation, args []string, recordOf func(string) record) int {
	w := &lineWriter{each: func(line string) bool { return inv.out.emit(recordOf(line)) }}
	status := runInto(inv, args, w, false)
	w.flush()
	return status
}

func runInto(inv *invocation, args []string, out io.Writer, quiet bool) int {
	hc := interp.HandlerCtx(inv.ctx)
	stderr := inv.stderr
	switch {
	case hc.Stderr == hc.Stdout:
		stderr = out
	case quiet:
		stderr = io.Discard
	}
	err := inv.call.runProgramTo(inv.ctx, args, out, stderr, nil)
	var status interp.ExitStatus
	switch {
	case err == nil:
		return 0
	case errors.As(err, &status):
		return int(status)
	}
	return 1
}

// lineWriter hands each whole line written to it to each, and fails every write once each returns false.
type lineWriter struct {
	each    func(string) bool
	partial []byte
	stopped bool
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.stopped {
		return 0, io.ErrClosedPipe
	}
	w.partial = append(w.partial, p...)
	for {
		at := bytes.IndexByte(w.partial, '\n')
		if at < 0 {
			return len(p), nil
		}
		line := string(w.partial[:at])
		w.partial = w.partial[at+1:]
		if !w.each(line) {
			w.stopped = true
			return 0, io.ErrClosedPipe
		}
	}
}

// flush hands on a last line that had no newline.
func (w *lineWriter) flush() {
	if len(w.partial) > 0 && !w.stopped {
		w.each(string(w.partial))
	}
	w.partial = nil
}

// emitListing prints a program's lines, each one that names a path marked with its parts, so that where the lines reach the model the
// terminal prints its runs with each folder once; a nib command that reads them gets the lines as the program printed them.
func emitListing(inv *invocation, lines []string, parse func(string) (listed, bool)) {
	for _, text := range lines {
		if !inv.out.emit(listingRecord(text, parse)) {
			return
		}
	}
}

// listingRecord is a program's line as a record, marked with its path's parts when it names one.
func listingRecord(text string, parse func(string) (listed, bool)) record {
	r := record{text: text}
	if parse != nil && engine.PlainText(text) == text {
		if l, ok := parse(text); ok {
			r.listed = &listedLine{text: text, listed: l}
		}
	}
	return r
}

// listed is a program's line that names one path: the text before the path, the path, and the text after it.
type listed struct{ before, path, after string }

// listedLine is the line a listed was read from; a command that changes the line, as grep -n does, leaves it no longer the line's.
type listedLine struct {
	text string
	listed
}

// listingRun is the fewest path lines in a row worth printing with each folder once, and listingSaves the fewest characters doing so must
// save.
const (
	listingRun   = 3
	listingSaves = 16
)

// layoutListing is the shortest layout of a run of path lines with each folder once, as nib's find lists files: a folder row in brackets, so
// it is never taken for a line the program printed, and under it the rest of each line's path, indented two spaces, the text around the
// path kept, nested folder by folder; nil when it would not save listingSaves characters on the lines as printed.
func layoutListing(rows []listed) []engine.TreeLine {
	if len(rows) < listingRun {
		return nil
	}
	before, paths, after := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	raw := 0
	for i, r := range rows {
		before[i], paths[i], after[i] = r.before, r.path, r.after
		raw += len(r.before+r.path+r.after) + 1
	}
	lines := engine.Tree(before, paths, after, folderRow)
	size := 0
	for _, line := range lines {
		size += len(line.Text) + 1
	}
	if size > raw-listingSaves {
		return nil
	}
	return lines
}

// compactListing is lines as the terminal prints them: every run of lines parse reads as paths laid out by layoutListing.
func compactListing(lines []string, parse func(string) (listed, bool)) []string {
	var out, run []string
	var rows []listed
	flush := func() {
		if layout := layoutListing(rows); layout != nil {
			for _, line := range layout {
				out = append(out, line.Text)
			}
		} else {
			out = append(out, run...)
		}
		run, rows = nil, nil
	}
	for _, line := range lines {
		r, ok := parse(line)
		if !ok {
			flush()
			out = append(out, line)
			continue
		}
		run, rows = append(run, line), append(rows, r)
	}
	flush()
	return out
}

// groupListings lays out each run of shown plain rows that are still the path lines a program printed with each folder once; a folder row
// stands for no log line, and every other row keeps the log lines it stood for. A row numbered with its log line keeps its whole path.
func groupListings(rows []row) []row {
	var out []row
	var run []row
	flush := func() {
		listedRows := make([]listed, len(run))
		for i, r := range run {
			listedRows[i] = r.listed.listed
		}
		layout := layoutListing(listedRows)
		if layout == nil {
			out = append(out, run...)
		}
		for _, line := range layout {
			if line.Row < 0 {
				out = append(out, row{text: line.Text, header: -1, plain: true})
				continue
			}
			r := run[line.Row]
			r.text = line.Text
			out = append(out, r)
		}
		run = nil
	}
	for _, r := range rows {
		if r.plain && !r.capped && r.label == "" && r.listed != nil && r.text == r.listed.text {
			run = append(run, r)
			continue
		}
		flush()
		out = append(out, r)
	}
	flush()
	return out
}

// folderRow is the row that names the folder of the n path lines under it.
func folderRow(folder string, n int) string {
	if n >= engine.GroupCount {
		return fmt.Sprintf("[%s (%d)]", folder, n)
	}
	return "[" + folder + "]"
}
