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
// and each run of rows directly in one folder becomes a "folder/" row followed by the rows with only their file names, indented two spaces,
// where that is shorter. An indented row never holds a slash, so its path is under, its folder row and its name joined.
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
	for i := 0; i < len(rows); {
		folder := paths[i][:strings.LastIndex(paths[i], "/")+1]
		end := i + 1
		for end < len(rows) && strings.HasPrefix(paths[end], folder) && !strings.Contains(paths[end][len(folder):], "/") {
			end++
		}
		n := end - i
		head := folder
		if n >= groupCount {
			head += fmt.Sprintf(" (%d)", n)
		}
		if folder == "" || n < 2 || n*len(folder) <= len(head)+1+2*n {
			for ; i < end; i++ {
				lines = append(lines, rows[i].before+paths[i]+rows[i].after)
			}
			continue
		}
		lines = append(lines, head)
		for ; i < end; i++ {
			lines = append(lines, "  "+rows[i].before+paths[i][len(folder):]+rows[i].after)
		}
	}
	return under, lines
}
