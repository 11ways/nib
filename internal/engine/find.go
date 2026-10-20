package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/11ways/nib/internal/js"
)

// FindArgs are find's arguments.
type FindArgs struct {
	Paths    []string
	Glob     []string
	NoIgnore bool
	MinSize  *string
	Newer    *string
	Sort     string
	Skip     *int
}

type listedFile struct {
	name     string
	size     int64
	modified time.Time
}

// Find lists the files under paths that grep would search, with line counts for text nib can hold, sizes and ages; filters and sorts by size or age.
func (e *Engine) Find(ctx *Context, args FindArgs) (Result, error) {
	var minSize *int64
	if args.MinSize != nil {
		size, err := parseSize(*args.MinSize)
		if err != nil {
			return Result{}, err
		}
		minSize = &size
	}
	var newer *time.Duration
	if args.Newer != nil {
		age, err := parseAge(*args.Newer)
		if err != nil {
			return Result{}, err
		}
		newer = &age
	}
	sortBy := args.Sort
	if sortBy == "" {
		sortBy = "path"
	}
	var roots, skipped, kept []string
	entries := args.Paths
	if len(entries) == 0 {
		entries = []string{"."}
	}
	for _, entry := range entries {
		refused, err := outsideSandbox(ctx, entry)
		if err != nil {
			return Result{}, err
		}
		root := ctx.resolve(entry)
		switch {
		case refused != "":
			skipped = append(skipped, refused)
		case !statExists(root):
			skipped = append(skipped, missingPath(entry, ctx))
		default:
			roots = append(roots, root)
			kept = append(kept, entry)
		}
	}
	// A path that names nothing, or that the sandbox refuses, is noted, and the other paths are listed without it.
	if len(skipped) > 0 {
		if len(kept) == 0 {
			return Result{}, errors.New(strings.Join(skipped, "; "))
		}
		again := args
		again.Paths = kept
		result, err := e.Find(ctx, again)
		if err != nil {
			return result, err
		}
		result.Text = fmt.Sprintf("[%s; listed the other paths]\n\n%s", strings.Join(skipped, "; "), result.Text)
		return result, nil
	}
	command := []string{"--files", "--null", "--no-config", "--hidden", "--glob", "!.git"}
	if args.NoIgnore {
		command = append(command, "--no-ignore")
	}
	for _, glob := range args.Glob {
		command = append(command, "--glob", glob)
	}
	signal, cancel := context.WithTimeout(ctx.Ctx, grepTimeout)
	defer cancel()
	out, err := ripgrep(ctx, signal, roots, command)
	if err != nil {
		return Result{}, err
	}
	set := map[string]bool{}
	for _, name := range strings.Split(out.stdout, "\x00") {
		if name != "" {
			set[name] = true
		}
	}
	listed := sortedKeysString(set)
	// As in grep, a file ripgrep reached by a link changed since is dropped unless it resolves inside the sandbox.
	names := listed
	if ctx.Sandbox {
		permitted, err := workspaceRoots(ctx)
		if err != nil {
			return Result{}, err
		}
		names = nil
		for _, name := range listed {
			real, err := realpath(name)
			if err != nil {
				real = name
			}
			if slices.ContainsFunc(permitted, func(root string) bool { return within(root, real) }) {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		head := "No files found"
		if stderr := js.Trim(out.stderr); stderr != "" {
			head = fmt.Sprintf("No files found (rg: %s)", strings.Split(stderr, "\n")[0])
		}
		return Result{Text: strings.Join(append([]string{head}, out.notes...), "\n\n")}, nil
	}
	describe := func(list []string) ([]listedFile, error) {
		var found []listedFile
		for _, name := range list {
			if err := signal.Err(); err != nil {
				return nil, err
			}
			info, err := os.Stat(name)
			if err == nil && info.Mode().IsRegular() {
				found = append(found, listedFile{name, info.Size(), info.ModTime()})
			}
		}
		return found, nil
	}
	skip := valueOr(args.Skip, 0)
	// Without a filter or a sort by size or age, only the page needs its files looked at.
	whole := minSize != nil || newer != nil || sortBy != "path"
	now := time.Now()
	var matching []listedFile
	if whole {
		described, err := describe(names)
		if err != nil {
			return Result{}, err
		}
		for _, item := range described {
			if (minSize == nil || item.size >= *minSize) && (newer == nil || now.Sub(item.modified) <= *newer) {
				matching = append(matching, item)
			}
		}
		switch sortBy {
		case "size":
			slices.SortStableFunc(matching, func(a, b listedFile) int { return int(b.size - a.size) })
		case "modified":
			slices.SortStableFunc(matching, func(a, b listedFile) int { return b.modified.Compare(a.modified) })
		}
	}
	total := len(names)
	if whole {
		total = len(matching)
	}
	// More files than a page holds, with no page asked for, show as the shape of their folders, which an explicit skip pages through; files
	// too many in one folder for that are paged.
	var rows []listedRow
	rolled := false
	if args.Skip == nil && total > findFiles {
		files := matching
		if !whole {
			files = make([]listedFile, len(names))
			for i, name := range names {
				files[i] = listedFile{name: name}
			}
		}
		var shownFiles []string
		rows, shownFiles = rollup(ctx, files, sortBy, whole)
		if rolled = rows != nil; rolled {
			ctx.preview(shownFiles)
		}
	}
	if !rolled {
		var page []listedFile
		if whole {
			page = matching[min(skip, len(matching)):min(skip+findFiles, len(matching))]
		} else {
			page, err = describe(names[min(skip, len(names)):min(skip+findFiles, len(names))])
			if err != nil {
				return Result{}, err
			}
		}
		if len(page) == 0 {
			if total > 0 {
				return Result{Text: fmt.Sprintf("No more results (%d files; skip %d is past the end)", total, skip)}, nil
			}
			return Result{Text: fmt.Sprintf("No files found (none of the %d listed pass the filters)", len(names))}, nil
		}
		previewed := make([]string, len(page))
		for i, item := range page {
			previewed[i] = item.name
		}
		ctx.preview(previewed)
		rows = make([]listedRow, len(page))
		for i, item := range page {
			rows[i] = listedRow{path: ctx.relativeDisplay(item.name), after: fileFacts(item)}
		}
	}
	var conditions []string
	if minSize != nil {
		conditions = append(conditions, "at least "+formatSize(*minSize))
	}
	if newer != nil {
		conditions = append(conditions, "modified in the last "+*args.Newer)
	}
	plural := "s"
	if total == 1 {
		plural = ""
	}
	head := fmt.Sprintf("%d file%s", total, plural)
	if len(conditions) > 0 {
		head += fmt.Sprintf(" %s (of %d listed)", strings.Join(conditions, ", "), len(names))
	}
	switch sortBy {
	case "size":
		head += ", largest first"
	case "modified":
		head += ", newest first"
	}
	more := func(shown int) string { return fmt.Sprintf("[%d files; continue with skip %d]", total, skip+shown) }
	left := OutputLimit - len(head) - len(more(len(rows)))
	reserved := 4
	for _, note := range out.notes {
		reserved += len(note) + 2
	}
	left -= reserved
	// Grouping only shortens rows, so the page is measured as they are whole.
	shown := 0
	for shown < len(rows) && len(rows[shown].path+rows[shown].after)+1 <= left {
		left -= len(rows[shown].path+rows[shown].after) + 1
		shown++
	}
	under, lines := grouped(rows[:shown])
	if under != "" {
		head += " under " + under
	}
	parts := []string{head, strings.Join(lines, "\n")}
	switch {
	case rolled:
		parts = append(parts, "[a folder of many files shows as one row with their count; find it for its files, or skip 0 lists every file]")
	case skip+shown < total:
		parts = append(parts, more(shown))
	}
	return Result{Text: strings.Join(append(parts, out.notes...), "\n\n")}, nil
}

// fileFacts is what a listing says of a file after its path: its line count for text, its type for an image or binary file, size and age.
func fileFacts(item listedFile) string {
	age := formatAge(item.modified)
	if item.size == 0 {
		return fmt.Sprintf(" (empty, %s)", age)
	}
	facts := formatSize(item.size) + ", " + age
	var data []byte
	if item.size <= TextLimit {
		data, _ = os.ReadFile(item.name)
	}
	if data == nil {
		return fmt.Sprintf(" (%s)", facts)
	}
	// Like git, a file counts as binary by its start; reading every byte of every listed file would cost more than the listing is worth.
	attachment := mime(data)
	start := data[:min(len(data), findSniff)]
	binary := slices.ContainsFunc(start, func(b byte) bool { return b < 9 || (b > 13 && b < 32 && b != 27) || b == 127 })
	if attachment != "" || binary {
		if attachment == "" {
			attachment = "binary"
		}
		return fmt.Sprintf(" (%s, %s)", attachment, facts)
	}
	lines := 1
	if data[len(data)-1] == '\n' {
		lines = 0
	}
	lines += bytes.Count(data, []byte{'\n'})
	word := "lines"
	if lines == 1 {
		word = "line"
	}
	return fmt.Sprintf(" (%d %s, %s)", lines, word, facts)
}

func statExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
