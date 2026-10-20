package syntax

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Import is one import declaration of a file, on a line of its own: Path is the dotted name it imports (a wildcard's package, without .*),
// Alias the name Kotlin's `as` gives it, and Module whether it is Java's `import module`, which imports every package of a module.
type Import struct {
	Line     int
	Path     string
	Static   bool
	Wildcard bool
	Alias    string
	Module   bool
}

// Name is the simple name the import makes visible: its alias, else its last segment; "" for a wildcard or a module.
func (i Import) Name() string {
	switch {
	case i.Wildcard || i.Module:
		return ""
	case i.Alias != "":
		return i.Alias
	}
	return i.Path[strings.LastIndex(i.Path, ".")+1:]
}

// group is the package an import takes its names from, a static import's class, or a module's name.
func (i Import) group() string {
	if i.Wildcard || i.Module {
		return i.Path
	}
	return i.Path[:max(0, strings.LastIndex(i.Path, "."))]
}

// kind sets apart what a file keeps apart: plain imports, static ones and module imports.
func (i Import) kind() int {
	switch {
	case i.Module:
		return 2
	case i.Static:
		return 1
	}
	return 0
}

// imports is how a language writes its import declarations, one to a line; implicit is the JDK package whose classes every file of it sees
// without one, "" for none.
type imports struct {
	line      *regexp.Regexp
	module    *regexp.Regexp
	semicolon bool
	static    bool
	alias     bool
	implicit  string
}

const (
	identifierPattern = `[\p{L}_$][\p{L}\p{N}_$]*`
	dottedPattern     = identifierPattern + `(?:\.` + identifierPattern + `)*`
)

// jvmImports is the import style of a JVM language: `import a.b.C;` with its semicolon required, optional or absent, and the forms it adds.
func jvmImports(semicolon, static, alias, module bool, implicit string) *imports {
	end := `\s*;?\s*$`
	if semicolon {
		end = `\s*;\s*$`
	}
	pattern := `^import\s+`
	if static {
		pattern += `(?P<static>static\s+)?`
	}
	pattern += `(?P<path>` + dottedPattern + `)(?P<wildcard>\.\*)?`
	if alias {
		pattern += `(?:\s+as\s+(?P<alias>` + identifierPattern + `))?`
	}
	style := &imports{line: regexp.MustCompile(pattern + end), semicolon: semicolon, static: static, alias: alias, implicit: implicit}
	if module {
		style.module = regexp.MustCompile(`^import\s+module\s+(?P<path>` + dottedPattern + `)` + end)
	}
	return style
}

// read is line as an import declaration; ok is false for any other line.
func (s *imports) read(line string) (Import, bool) {
	if s.module != nil {
		if m := s.module.FindStringSubmatch(line); m != nil {
			return Import{Path: m[s.module.SubexpIndex("path")], Module: true}, true
		}
	}
	m := s.line.FindStringSubmatch(line)
	if m == nil {
		return Import{}, false
	}
	found := Import{Path: m[s.line.SubexpIndex("path")], Wildcard: m[s.line.SubexpIndex("wildcard")] != ""}
	if s.static {
		found.Static = m[s.line.SubexpIndex("static")] != ""
	}
	if s.alias {
		found.Alias = m[s.line.SubexpIndex("alias")]
	}
	// A single-type import names a class in a package; a bare name is no import Java or Kotlin can write.
	if !found.Wildcard && !found.Module && !strings.Contains(found.Path, ".") {
		return Import{}, false
	}
	return found, true
}

// text is the line that declares i.
func (s *imports) text(i Import) string {
	line := "import "
	switch {
	case i.Module:
		line += "module "
	case i.Static:
		line += "static "
	}
	line += i.Path
	if i.Wildcard {
		line += ".*"
	}
	if i.Alias != "" {
		line += " as " + i.Alias
	}
	if s.semicolon {
		line += ";"
	}
	return line
}

// importsOf is the import style of the file at path; nil when nib reads no imports in its language.
func importsOf(path string) *imports {
	if l := languageOf(path); l != nil {
		return l.imports
	}
	return nil
}

// ErrNoImports is returned for a file in a language whose imports nib does not read.
var ErrNoImports = errors.New("nib reads imports only in Java and Kotlin files")

// Imports are the import declarations among lines, the file at path, each on a line of its own and unindented; ok is false when nib reads no
// imports in that language. One split over lines is not read.
func Imports(path string, lines []string) (found []Import, ok bool) {
	style := importsOf(path)
	if style == nil {
		return nil, false
	}
	for index, line := range lines {
		if !strings.HasPrefix(line, "import") {
			continue
		}
		if i, ok := style.read(line); ok {
			i.Line = index + 1
			found = append(found, i)
		}
	}
	return found, true
}

// ImportTree is imports as a package tree, one row per package with the names imported from it, a space between them as no name holds one:
// `java.util: ArrayList List`, then its sub-packages two spaces deeper by their own segments (`  concurrent: ConcurrentHashMap`). A package
// that imports nothing itself and holds one sub-package shares its row (`com.couchbase.client:`). A wildcard shows as `*` and a Kotlin alias
// as `Alias=Name`; static imports follow as a tree of their own, its top rows after `static `, and module imports as `module NAME`.
func ImportTree(imports []Import) []string {
	type node struct {
		names    []string
		children map[string]*node
	}
	plain, static := &node{children: map[string]*node{}}, &node{children: map[string]*node{}}
	var modules []string
	for _, i := range imports {
		if i.Module {
			modules = append(modules, "module "+i.Path)
			continue
		}
		at := plain
		if i.Static {
			at = static
		}
		for _, segment := range strings.Split(i.group(), ".") {
			next := at.children[segment]
			if next == nil {
				next = &node{children: map[string]*node{}}
				at.children[segment] = next
			}
			at = next
		}
		switch {
		case i.Wildcard:
			at.names = append(at.names, "*")
		case i.Alias != "":
			at.names = append(at.names, i.Alias+"="+i.Path[strings.LastIndex(i.Path, ".")+1:])
		default:
			at.names = append(at.names, i.Name())
		}
	}
	var rows []string
	var render func(n *node, depth int, prefix string)
	render = func(n *node, depth int, prefix string) {
		for _, segment := range slices.Sorted(maps.Keys(n.children)) {
			label, child := segment, n.children[segment]
			for len(child.names) == 0 && len(child.children) == 1 {
				for next, only := range child.children {
					label, child = label+"."+next, only
				}
			}
			row := strings.Repeat("  ", depth) + prefix + label + ":"
			if len(child.names) > 0 {
				row += " " + strings.Join(slices.Compact(slices.Sorted(slices.Values(child.names))), " ")
			}
			rows = append(rows, row)
			render(child, depth+1, "")
		}
	}
	render(plain, 0, "")
	render(static, 0, "static ")
	slices.Sort(modules)
	return append(rows, slices.Compact(modules)...)
}

// Placement is where an import goes in a file: Lines after line After, 0 for the top of the file, which are Text, the import's own line,
// with a blank line between it and the package line or the code below when it is the file's first import; Existing is the line of the same
// import when the file holds it already, and then nothing goes in.
type Placement struct {
	Import   Import
	Text     string
	After    int
	Lines    []string
	Existing int
}

// PlaceImport is where the import spec (`a.b.C`, `static a.b.C.m`, `a.b.*`, Kotlin's `a.b.C as D`, with or without `import` and the `;`)
// goes in lines, the file at path: beside the imports that share most of its package, in their alphabetical order, else after the last
// import, else after the package line. A single-type import of a name another import already brings in is refused, since the two clash.
func PlaceImport(path string, lines []string, spec string) (Placement, error) {
	style := importsOf(path)
	if style == nil {
		return Placement{}, ErrNoImports
	}
	written := strings.TrimSpace(spec)
	written = strings.TrimSpace(strings.TrimSuffix(written, ";"))
	written = strings.TrimSpace(strings.TrimPrefix(written, "import "))
	line := "import " + written
	if style.semicolon {
		line += ";"
	}
	wanted, ok := style.read(line)
	if !ok {
		if strings.HasPrefix(written, "static ") && !style.static {
			return Placement{}, fmt.Errorf("%s has no static imports; import %s", languageOf(path).name, strings.TrimPrefix(written, "static "))
		}
		forms := "a.b.C or a.b.*"
		if style.static {
			forms = "a.b.C, a.b.* or static a.b.C.m"
		}
		return Placement{}, fmt.Errorf("%q is no import nib can write; give a dotted name, like %s", spec, forms)
	}
	existing, _ := Imports(path, lines)
	place := Placement{Import: wanted}
	for _, i := range existing {
		if i.Path == wanted.Path && i.Static == wanted.Static && i.Wildcard == wanted.Wildcard && i.Alias == wanted.Alias && i.Module == wanted.Module {
			place.Existing = i.Line
			return place, nil
		}
	}
	if name := wanted.Name(); name != "" && !wanted.Static {
		for _, i := range existing {
			if !i.Static && i.Name() == name {
				return Placement{}, fmt.Errorf("line %d imports %s as %s already, which clashes with %s; write %s in full where you use it", i.Line, i.Path, name, wanted.Path, wanted.Path)
			}
		}
	}
	place.Text = style.text(wanted)
	place.Lines = []string{place.Text}
	if len(existing) == 0 {
		for index, line := range lines {
			if strings.HasPrefix(line, "package ") {
				place.After, place.Lines = index+1, []string{"", place.Text}
				return place, nil
			}
		}
		place.Lines = append(place.Lines, "")
		return place, nil
	}
	var kin []Import
	for _, i := range existing {
		if i.kind() == wanted.kind() {
			kin = append(kin, i)
		}
	}
	if len(kin) == 0 {
		place.After = existing[len(existing)-1].Line
		return place, nil
	}
	order := func(i Import) string {
		if i.Wildcard {
			return i.Path + ".*"
		}
		return i.Path
	}
	best := -1
	var nearest []Import
	for _, i := range kin {
		switch shared := sharedSegments(i.Path, wanted.Path); {
		case shared > best:
			best, nearest = shared, []Import{i}
		case shared == best:
			nearest = append(nearest, i)
		}
	}
	place.After = nearest[0].Line - 1
	for _, i := range nearest {
		if order(i) < order(wanted) {
			place.After = i.Line
		}
	}
	return place, nil
}

// sharedSegments counts the leading dotted segments two names have in common.
func sharedSegments(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	n := 0
	for n < len(left) && n < len(right) && left[n] == right[n] {
		n++
	}
	return n
}
