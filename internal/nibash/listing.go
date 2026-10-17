package nibash

import (
	"fmt"

	"github.com/11ways/nib/internal/engine"
)

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
