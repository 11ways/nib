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
// shell sends it, or into the lines when the shell sends both to one place, as 2>&1 does, so they keep their order.
func captured(inv *invocation, args []string) ([]string, int) {
	hc := interp.HandlerCtx(inv.ctx)
	var out bytes.Buffer
	var stderr io.Writer = hc.Stderr
	if hc.Stderr == hc.Stdout {
		stderr = &out
	}
	err := inv.call.runProgramTo(inv.ctx, args, &out, stderr, nil)
	var status interp.ExitStatus
	switch {
	case err == nil:
		return splitLines(out.String()), 0
	case errors.As(err, &status):
		return splitLines(out.String()), int(status)
	}
	return splitLines(out.String()), 1
}

// emitLines prints lines as plain output.
func emitLines(inv *invocation, lines []string) {
	for _, text := range lines {
		inv.out.emit(record{text: text})
	}
}

// listed is a program's line that names one path: the text before the path, the path, and the text after it.
type listed struct{ before, path, after string }

// listingRun is the fewest path lines in a row worth printing with each folder once.
const listingRun = 3

// compactListing prints the runs of path lines in a program's output with each folder once, as nib's find lists files: a folder row in
// brackets, so it is never taken for a line the program printed, and under it the rest of each line's path, indented two spaces, the text
// around the path kept. Of the layout with the run's common folder named first and the one without, it takes the shorter, and only where that
// is at least an eighth shorter than the lines as printed; every other line prints as it is.
func compactListing(lines []string, parse func(string) (listed, bool)) []string {
	var out, run []string
	var rows []listed
	flush := func() {
		defer func() { run, rows = nil, nil }()
		if len(rows) < listingRun {
			out = append(out, run...)
			return
		}
		before, paths, after := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
		for i, r := range rows {
			before[i], paths[i], after[i] = r.before, r.path, r.after
		}
		under, nested := engine.Grouped(before, paths, after, folderRow, true)
		if under != "" {
			for i, line := range nested {
				nested[i] = "  " + line
			}
			nested = append([]string{folderRow(under, len(rows))}, nested...)
		}
		_, flat := engine.Grouped(before, paths, after, folderRow, false)
		best := run
		for _, layout := range [][]string{nested, flat} {
			if 8*textSize(layout) <= 7*textSize(run) && textSize(layout) < textSize(best) {
				best = layout
			}
		}
		out = append(out, best...)
	}
	for _, line := range lines {
		r, ok := parse(line)
		if !ok {
			flush()
			out = append(out, line)
			continue
		}
		run = append(run, line)
		rows = append(rows, r)
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

func textSize(lines []string) int {
	n := 0
	for _, line := range lines {
		n += len(line) + 1
	}
	return n
}
