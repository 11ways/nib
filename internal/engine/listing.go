package engine

import (
	"fmt"
	"strings"
)

// groupCount is the smallest folder run whose folder row also says how many files it holds.
const groupCount = 10

// listedRow is one row of a path listing: its displayed path and the text before and after it.
type listedRow struct{ before, path, after string }

// grouped lays out a listing's rows without repeating folders: the folder every row lies in is returned as under, for the head to name once,
// and runs of rows in one folder become a "folder/" row followed by the rest of each row's path, indented two spaces, so an indented row's
// path is under, its folder row and the row joined. Of all such layouts it takes the shortest, keeping a row whole where grouping saves
// nothing.
func grouped(rows []listedRow) (under string, lines []string) {
	paths := make([]string, len(rows))
	for i, row := range rows {
		paths[i] = row.path
	}
	if len(rows) > 1 {
		under = paths[0][:strings.LastIndex(paths[0], "/")+1]
		for _, path := range paths[1:] {
			for !strings.HasPrefix(path, under) {
				under = under[:strings.LastIndex(strings.TrimSuffix(under, "/"), "/")+1]
			}
		}
		// The root alone would only make every path look relative.
		if under == "/" {
			under = ""
		}
		for i := range paths {
			paths[i] = paths[i][len(under):]
		}
	}
	// best[i] is the length of the shortest layout of rows i on; folder[i] and end[i] are its first group, "" for row i kept whole.
	n := len(rows)
	best := make([]int, n+1)
	folder := make([]string, n)
	end := make([]int, n)
	for i := n - 1; i >= 0; i-- {
		best[i] = len(paths[i]) + 1 + best[i+1]
		end[i] = i + 1
		for cut := strings.LastIndex(paths[i], "/"); cut > 0; cut = strings.LastIndex(paths[i][:cut], "/") {
			candidate := paths[i][:cut+1]
			rest := 0
			for j := i; j < n && strings.HasPrefix(paths[j], candidate); j++ {
				rest += 2 + len(paths[j]) - len(candidate) + 1
				if j == i {
					continue
				}
				if cost := len(groupHead(candidate, j+1-i)) + 1 + rest + best[j+1]; cost < best[i] {
					best[i], folder[i], end[i] = cost, candidate, j+1
				}
			}
		}
	}
	for i := 0; i < n; i = end[i] {
		if folder[i] == "" {
			lines = append(lines, rows[i].before+paths[i]+rows[i].after)
			continue
		}
		lines = append(lines, groupHead(folder[i], end[i]-i))
		for j := i; j < end[i]; j++ {
			lines = append(lines, "  "+rows[j].before+paths[j][len(folder[i]):]+rows[j].after)
		}
	}
	return under, lines
}

// groupHead is the folder row of a group of n rows.
func groupHead(folder string, n int) string {
	if n >= groupCount {
		return fmt.Sprintf("%s (%d)", folder, n)
	}
	return folder
}
