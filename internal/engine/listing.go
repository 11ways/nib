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
// path is under, its folder row and the row joined. A folder row takes every row of its folder, subfolders included, so no folder is named
// twice and its count is whole. Of all such layouts it takes the shortest, keeping a row whole where grouping saves nothing.
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
			if i > 0 && strings.HasPrefix(paths[i-1], candidate) {
				continue
			}
			rest := 0
			j := i
			for ; j < n && strings.HasPrefix(paths[j], candidate); j++ {
				rest += 2 + len(paths[j]) - len(candidate) + 1
			}
			if j-i < 2 {
				continue
			}
			if cost := len(groupHead(candidate, j-i)) + 1 + rest + best[j]; cost < best[i] {
				best[i], folder[i], end[i] = cost, candidate, j
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

// GroupCount is the smallest folder run whose folder row also says how many rows it holds.
const GroupCount = groupCount

// TreeLine is one line of a Tree layout: its text, and the row it prints, -1 for a folder row.
type TreeLine struct {
	Text string
	Row  int
}

// treeRows is the most rows Tree lays out as a tree; past it, its search would cost more than the output saves.
const treeRows = 4000

// Tree lays out a listing nib does not make itself, its rows given by the text before each path, the path and the text after it, in their
// order: a run of rows in one folder becomes head's row for it and the rest of each row's path under it, indented two spaces, and the same
// within it, folder by folder, so a nested folder is named once under the one above it. A folder row takes every row of its folder in the run,
// subfolders included, so no folder is named twice in it. Of all such layouts it takes the shortest, keeping a row whole where grouping saves
// nothing; past treeRows rows, every row stays whole.
func Tree(before, paths, after []string, head func(folder string, n int) string) []TreeLine {
	n := len(paths)
	whole := make([]TreeLine, n)
	for i := range paths {
		whole[i] = TreeLine{before[i] + paths[i] + after[i], i}
	}
	if n < 2 || n > treeRows {
		return whole
	}
	type key struct{ lo, hi, strip int }
	type layout struct {
		cost  int
		lines []TreeLine
	}
	memo := map[key]layout{}
	var lay func(lo, hi, strip int) layout
	// lay is the shortest layout of rows lo to hi, whose paths all start with the same strip bytes, as lines one level deeper than its caller's.
	lay = func(lo, hi, strip int) layout {
		k := key{lo, hi, strip}
		if l, ok := memo[k]; ok {
			return l
		}
		rest := func(i int) string { return paths[i][strip:] }
		best := make([]int, hi-lo+1)
		pick := make([]string, hi-lo)
		end := make([]int, hi-lo)
		inner := make([]layout, hi-lo)
		for i := hi - 1; i >= lo; i-- {
			at := i - lo
			best[at] = len(before[i]+rest(i)+after[i]) + 1 + best[at+1]
			end[at] = i + 1
			for cut := strings.LastIndex(rest(i), "/"); cut > 0; cut = strings.LastIndex(rest(i)[:cut], "/") {
				candidate := rest(i)[:cut+1]
				if i > lo && strings.HasPrefix(rest(i-1), candidate) {
					continue
				}
				j := i
				for j < hi && strings.HasPrefix(rest(j), candidate) {
					j++
				}
				if j-i < 2 {
					continue
				}
				under := lay(i, j, strip+len(candidate))
				// Every line under the folder row is indented two spaces more.
				cost := len(head(candidate, j-i)) + 1 + under.cost + 2*len(under.lines) + best[j-lo]
				if cost < best[at] {
					best[at], pick[at], end[at], inner[at] = cost, candidate, j, under
				}
			}
		}
		var lines []TreeLine
		for i := lo; i < hi; i = end[i-lo] {
			at := i - lo
			if pick[at] == "" {
				lines = append(lines, TreeLine{before[i] + rest(i) + after[i], i})
				continue
			}
			lines = append(lines, TreeLine{head(pick[at], end[at]-i), -1})
			for _, line := range inner[at].lines {
				lines = append(lines, TreeLine{"  " + line.Text, line.Row})
			}
		}
		l := layout{best[0], lines}
		memo[k] = l
		return l
	}
	return lay(0, n, 0).lines
}
