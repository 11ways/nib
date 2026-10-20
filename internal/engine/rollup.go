package engine

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// rollupRows is the most rows a rolled-up listing shows: folders are opened, the biggest first, while their files and subfolders fit.
const rollupRows = 30

// folderNode is a folder of a rolled-up listing, path ending in a slash, with what lies at and below it.
type folderNode struct {
	path      string
	folders   map[string]*folderNode
	files     []listedFile
	count     int
	size      int64
	newest    time.Time
	largest   int64
	described bool
}

// rollupEntry is one row of a rolled-up listing: a folder shown as one row, or a file.
type rollupEntry struct {
	folder *folderNode
	file   listedFile
}

// rollup lays out more files than a page holds as the shape of their folders, so a listing too long to read says where its files are: rows
// for the files of opened folders and one row for each folder left closed, with its count, ordered as the listing sorts. described says the
// files carry their size and age, which a closed folder then sums up. rows are nil when the folder every file lies in holds more files and
// folders than rollupRows, which only paging shows.
func rollup(ctx *Context, files []listedFile, sortBy string, described bool) (rows []listedRow, shown []string) {
	root := &folderNode{folders: map[string]*folderNode{}, described: described}
	for _, file := range files {
		parts := strings.Split(ctx.relativeDisplay(file.name), "/")
		node := root
		node.add(file)
		for _, part := range parts[:len(parts)-1] {
			child := node.folders[part]
			if child == nil {
				child = &folderNode{path: node.path + part + "/", folders: map[string]*folderNode{}, described: described}
				node.folders[part] = child
			}
			child.add(file)
			node = child
		}
		node.files = append(node.files, file)
	}
	// A folder holding nothing but one folder opens by itself, so a chain of them is one row.
	compress := func(node *folderNode) *folderNode {
		for len(node.files) == 0 && len(node.folders) == 1 {
			for _, only := range node.folders {
				node = only
			}
		}
		return node
	}
	open := func(node *folderNode) []rollupEntry {
		var entries []rollupEntry
		for _, file := range node.files {
			entries = append(entries, rollupEntry{file: file})
		}
		for _, name := range slices.Sorted(maps.Keys(node.folders)) {
			entries = append(entries, rollupEntry{folder: compress(node.folders[name])})
		}
		return entries
	}
	entries := open(compress(root))
	if len(entries) > rollupRows {
		return nil, nil
	}
	for {
		best := -1
		for i, entry := range entries {
			if entry.folder != nil && len(entries)-1+len(open(entry.folder)) <= rollupRows && (best < 0 || entry.folder.count > entries[best].folder.count) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		entries = slices.Concat(entries[:best], open(entries[best].folder), entries[best+1:])
	}
	key := func(entry rollupEntry) (string, time.Time, int64) {
		if entry.folder != nil {
			return entry.folder.path, entry.folder.newest, entry.folder.largest
		}
		return ctx.relativeDisplay(entry.file.name), entry.file.modified, entry.file.size
	}
	slices.SortStableFunc(entries, func(a, b rollupEntry) int {
		pa, ma, sa := key(a)
		pb, mb, sb := key(b)
		switch sortBy {
		case "modified":
			return mb.Compare(ma)
		case "size":
			return cmp.Compare(sb, sa)
		}
		return strings.Compare(pa, pb)
	})
	rows = make([]listedRow, len(entries))
	for i, entry := range entries {
		if entry.folder != nil {
			rows[i] = listedRow{path: entry.folder.path, after: entry.folder.facts(sortBy)}
			continue
		}
		file := entry.file
		if !described {
			if info, err := os.Stat(file.name); err == nil {
				file.size, file.modified = info.Size(), info.ModTime()
			}
		}
		rows[i] = listedRow{path: ctx.relativeDisplay(file.name), after: fileFacts(file)}
		shown = append(shown, file.name)
	}
	return rows, shown
}

func (n *folderNode) add(file listedFile) {
	n.count++
	n.size += file.size
	if file.modified.After(n.newest) {
		n.newest = file.modified
	}
	n.largest = max(n.largest, file.size)
}

// facts is what a closed folder's row says after its path: how many files, and what the listing sorts by of them, or their size.
func (n *folderNode) facts(sortBy string) string {
	files := fmt.Sprintf("%d files", n.count)
	if !n.described {
		return " (" + files + ")"
	}
	switch sortBy {
	case "modified":
		return fmt.Sprintf(" (%s, newest %s)", files, formatAge(n.newest))
	case "size":
		return fmt.Sprintf(" (%s, largest %s)", files, formatSize(n.largest))
	}
	return fmt.Sprintf(" (%s, %s)", files, formatSize(n.size))
}
