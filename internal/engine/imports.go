package engine

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/11ways/nib/internal/snapshots"
	"github.com/11ways/nib/internal/syntax"
)

const (
	// importFold is the fewest imports in a row that a read shows as their packages instead of their lines.
	importFold = 10
	// checkBytes caps the files an edit's syntax checks parse: a parse costs about 70 ms per 1,000 lines.
	checkBytes = 512 * 1024
)

// foldImports shows each run of at least importFold imports the rows hold, blank lines between them included, as one row per package with
// the names imported from it, when the rows go on past the run: an agent reading code needs to know what the file has, not each line. A
// run whose every line the agent holds is left to the held marker, which is shorter still. The folded lines are not seen, so an edit needs
// a read of them first; IMPORT adds an import without one. It runs before held lines turn into markers, so rows stand for their own lines.
func foldImports(canonical string, lines []string, rows []blockRow, holds map[int]int) []blockRow {
	imports, ok := syntax.Imports(canonical, lines)
	if !ok || len(imports) < importFold {
		return rows
	}
	// runs are the imports with only blank lines between them.
	var runs [][]syntax.Import
	for index, i := range imports {
		joined := index > 0
		if joined {
			for line := imports[index-1].Line + 1; line < i.Line; line++ {
				if strings.TrimSpace(lines[line-1]) != "" {
					joined = false
				}
			}
		}
		if joined {
			runs[len(runs)-1] = append(runs[len(runs)-1], i)
		} else {
			runs = append(runs, []syntax.Import{i})
		}
	}
	for _, run := range runs {
		if len(run) < importFold {
			continue
		}
		first, last := run[0].Line, run[len(run)-1].Line
		at := slices.IndexFunc(rows, func(r blockRow) bool { return r.at == first })
		end := at + last - first + 1
		if at < 0 || end >= len(rows) {
			continue
		}
		whole, held := true, true
		for k := at; k < end; k++ {
			whole = whole && rows[k].at == first+k-at
			_, ok := holds[first+k-at]
			held = held && ok
		}
		if !whole || held {
			continue
		}
		folded := []blockRow{{text: fmt.Sprintf("[lines %d-%d: %d imports as a package tree; read that range for their lines]", first, last, len(run)), at: first}}
		for _, row := range syntax.ImportTree(run) {
			folded = append(folded, blockRow{text: row, at: first})
		}
		rows = slices.Concat(rows[:at], folded, rows[end:])
	}
	return rows
}

// splitImports parts a section's IMPORT edits from the rest.
func splitImports(edits []snapshots.LineEdit) (rest, imports []snapshots.LineEdit) {
	for _, e := range edits {
		if e.Import != "" {
			imports = append(imports, e)
		} else {
			rest = append(rest, e)
		}
	}
	return rest, imports
}

// placeImports adds each IMPORT to edits, sorted, as the lines that put it among lines, the file as it is: one the file has already, or that
// a hunk of the patch writes itself, adds nothing and gets a note.
func placeImports(canonical string, lines []string, edits, imports []snapshots.LineEdit) ([]snapshots.LineEdit, []string, error) {
	if len(imports) == 0 {
		return edits, nil, nil
	}
	written := map[string]bool{}
	for _, e := range edits {
		for _, line := range e.Content {
			written[strings.TrimSpace(line)] = true
		}
	}
	type spot struct {
		before, after bool
		texts         []string
	}
	spots := map[int]*spot{}
	names := map[string]string{}
	var notes []string
	for _, e := range imports {
		place, err := syntax.PlaceImport(canonical, lines, e.Import)
		if err != nil {
			return nil, nil, fmt.Errorf("IMPORT %s: %v", e.Import, err)
		}
		switch {
		case place.Existing > 0:
			notes = append(notes, fmt.Sprintf("[IMPORT %s: line %d imports it already]", place.Import.Path, place.Existing))
			continue
		case written[place.Text]:
			notes = append(notes, fmt.Sprintf("[IMPORT %s: a hunk of this patch writes that line already]", place.Import.Path))
			continue
		}
		if name := place.Import.Name(); name != "" && !place.Import.Static {
			if other, ok := names[name]; ok && other != place.Import.Path {
				return nil, nil, fmt.Errorf("IMPORT %s and IMPORT %s both import a %s; write one of them in full where you use it", other, place.Import.Path, name)
			}
			names[name] = place.Import.Path
		}
		s := spots[place.After]
		if s == nil {
			s = &spot{before: place.Lines[0] == "", after: place.Lines[len(place.Lines)-1] == ""}
			spots[place.After] = s
		}
		if !slices.Contains(s.texts, place.Text) {
			s.texts = append(s.texts, place.Text)
		}
	}
	all := slices.Clone(edits)
	for after, s := range spots {
		slices.Sort(s.texts)
		content := slices.Clone(s.texts)
		if s.before {
			content = append([]string{""}, content...)
		}
		if s.after {
			content = append(content, "")
		}
		edit := snapshots.LineEdit{Start: after, End: after, Operation: snapshots.After, Content: content}
		if after == 0 {
			edit.Start, edit.End, edit.Operation = 1, 1, snapshots.Before
		}
		all = append(all, edit)
	}
	sorted, err := orderEdits(all)
	if err != nil {
		return nil, nil, fmt.Errorf("an IMPORT goes where another hunk of this patch replaces or inserts lines (%v); write the import in that hunk's rows instead", err)
	}
	return sorted, notes, nil
}

// codeWarnings parses a file an edit changed, in a language nib has a grammar for: an edit that leaves it no longer parsing, when it parsed
// before and unbalanced brackets do not already say so (bracketed), says where the first error is, and an edit to a Java file that uses a
// class nothing in the file imports or declares says which class nib finds by that name, with the IMPORT that adds it.
func codeWarnings(ctx *Context, t *target, before *text, result applied, bracketed bool) []string {
	body := strings.TrimPrefix(string(result.bytes), bom)
	if !syntax.Supported(t.canonical) || len(body) > checkBytes {
		return nil
	}
	after, err := syntax.Inspect(t.canonical, []byte(body))
	if err != nil {
		return nil
	}
	if after.ErrorLine > 0 {
		if bracketed {
			return nil
		}
		earlier, err := syntax.Inspect(t.canonical, []byte(before.body))
		if err != nil || earlier.ErrorLine > 0 {
			return nil
		}
		return []string{fmt.Sprintf("The file no longer parses as %s: before this edit it did, and now its first error is at line %d; check the new rows above it too.", syntax.Language(t.canonical), after.ErrorLine)}
	}
	if !after.Types || !after.TopLevel {
		return nil
	}
	lines := appliedLines(result)
	imports, _ := syntax.Imports(t.canonical, lines)
	imported := map[string]bool{}
	var wildcards []string
	for _, i := range imports {
		switch {
		// A module import brings in whole packages, and a static wildcard every nested type of a class: nib cannot tell what they hold.
		case i.Module, i.Static && i.Wildcard:
			return nil
		case i.Wildcard:
			wildcards = append(wildcards, i.Path)
		case i.Static:
			imported[i.Path[strings.LastIndex(i.Path, ".")+1:]] = true
		default:
			imported[i.Name()] = true
		}
	}
	// A name the file used before the edit resolved then, through an import, its package, a parent class or a mention nib cannot tell.
	earlier := strings.Join(sourceLines(before), "\n")
	var missing []string
	for _, r := range result.runs {
		for line := r.at + 1; line <= r.at+r.length; line++ {
			for _, name := range after.Uses[line] {
				if !after.Declared[name] && !after.Implicit[name] && !imported[name] && !slices.Contains(missing, name) &&
					!regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).MatchString(earlier) {
					missing = append(missing, name)
				}
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	own := packageOf(lines)
	var searched []string
	for _, w := range wildcards {
		if !syntax.JDKPackage(w) {
			searched = append(searched, w)
		}
	}
	found := searchClasses(ctx, t.canonical, missing, searched)
	// A wildcard of a package neither the JDK nor the workspace holds, such as a library's, may bring in a class of any name.
	for _, w := range searched {
		if !found.packages[w] {
			return nil
		}
	}
	var sure, unsure []string
	var named []string
	for _, name := range missing {
		var choices []string
		for _, c := range found.classes[name] {
			if !slices.Contains(choices, c.path) {
				choices = append(choices, c.path)
			}
		}
		choices = append(choices, syntax.JDKClasses(name)...)
		resolved := slices.ContainsFunc(choices, func(path string) bool {
			pkg := path[:max(0, strings.LastIndex(path, "."))]
			return pkg == own || slices.Contains(wildcards, pkg)
		})
		slices.Sort(choices)
		switch {
		case resolved || len(choices) == 0:
		case len(choices) == 1:
			sure = append(sure, choices[0])
			named = append(named, name)
		default:
			unsure = append(unsure, fmt.Sprintf("%s is not imported, and nib finds %s; IMPORT the one meant.", name, strings.Join(choices, " and ")))
		}
	}
	var warnings []string
	if len(sure) > 0 {
		form := "%s, used in the new rows, is not imported, and nib knows one class of that name; if it is the one meant, this adds it:\n[%s]\nIMPORT %s"
		if len(named) > 1 {
			form = "%s, used in the new rows, are not imported, and nib knows one class of each name; if they are the ones meant, this adds them:\n[%s]\nIMPORT %s"
		}
		warnings = append(warnings, fmt.Sprintf(form, strings.Join(named, ", "), t.display, strings.Join(sure, ", ")))
	}
	return append(warnings, unsure...)
}

// class is a class found by its file's name: its full name and the file.
type class struct{ path, file string }

// found is what a search of the workspace found: the classes named after each name, and the packages it holds a source file of.
type found struct {
	classes  map[string][]class
	packages map[string]bool
}

// packageLine reads a JVM file's package declaration.
var packageLine = regexp.MustCompile(`^package\s+([\p{L}_$][\p{L}\p{N}_$]*(?:\.[\p{L}_$][\p{L}\p{N}_$]*)*)\s*;?\s*$`)

// packageOf is the package lines declare, "" for none.
func packageOf(lines []string) string {
	for _, line := range lines {
		if m := packageLine.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// searchClasses finds, in the workspace roots that hold canonical, the Java and Kotlin files named after each name, with the class each
// declares by its package, and whether each of packages has a source file there, in one search; nothing when no root holds canonical or
// the search fails.
func searchClasses(ctx *Context, canonical string, names, packages []string) found {
	result := found{classes: map[string][]class{}, packages: map[string]bool{}}
	roots, err := workspaceRoots(ctx)
	if err != nil {
		return result
	}
	var holding []string
	for _, root := range roots {
		if within(root, canonical) {
			holding = append(holding, root)
		}
	}
	if len(holding) == 0 {
		return result
	}
	command := []string{"--files", "--null", "--no-config", "--hidden", "--glob", "!.git", "--glob", "**/{" + strings.Join(names, ",") + "}.{java,kt}"}
	for _, p := range packages {
		command = append(command, "--glob", "**/"+strings.ReplaceAll(p, ".", "/")+"/*.{java,kt}")
	}
	signal, cancel := context.WithTimeout(ctx.Ctx, grepTimeout)
	defer cancel()
	out, err := ripgrep(ctx, signal, holding, command)
	if err != nil {
		return result
	}
	for _, file := range strings.Split(out.stdout, "\x00") {
		if file == "" {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(file), ".java"), ".kt")
		named := slices.Contains(names, name)
		// One file is enough to tell a package is the workspace's.
		wanted := slices.ContainsFunc(packages, func(p string) bool {
			return !result.packages[p] && strings.HasSuffix(filepath.ToSlash(filepath.Dir(file)), "/"+strings.ReplaceAll(p, ".", "/"))
		})
		if !named && !wanted {
			continue
		}
		pkg := filePackage(file)
		if pkg == "" {
			continue
		}
		result.packages[pkg] = true
		if named {
			result.classes[name] = append(result.classes[name], class{path: pkg + "." + name, file: file})
		}
	}
	return result
}

// filePackage is the package a source file declares in its first lines, "" when it declares none there.
func filePackage(file string) string {
	handle, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer handle.Close()
	scanner := bufio.NewScanner(handle)
	for count := 0; count < 50 && scanner.Scan(); count++ {
		if m := packageLine.FindStringSubmatch(scanner.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}
