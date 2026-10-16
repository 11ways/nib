// Package syntax parses source files with tree-sitter grammars (gotreesitter, pure Go) to tell what a file declares and where a block
// ends. It never guesses: a file in a language without a grammar, or a declaration whose lines did not parse cleanly, is refused.
package syntax

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	ts "github.com/odvcencio/gotreesitter"
	grammarruntime "github.com/odvcencio/gotreesitter/grammars/runtime"
)

// A decoded grammar holds 12 to 33 MB, so nib keeps the two used last and lets one go after ten idle minutes; gotreesitter keeps every one
// for good by default. Loading one again takes a single decode.
func init() {
	grammarruntime.SetEmbeddedLanguageCacheLimit(2)
	grammarruntime.SetEmbeddedLanguageIdleTTL(10 * time.Minute)
}

// ErrNoGrammar is returned for a file in a language nib has no grammar for.
var ErrNoGrammar = errors.New("no grammar")

// ErrNotABlock is returned when a line starts nothing that spans more than that line.
var ErrNotABlock = errors.New("not a multi-line block")

// Range is lines start through end, counted from 1.
type Range struct{ Start, End int }

// declaration is one name a file declares: its lines, from the comments and attributes above it through its end; the enclosing declaration
// (-1 for none); the type it belongs to though declared outside it (a Go method's receiver); and whether its lines parsed cleanly.
type declaration struct {
	name   string
	lines  Range
	parent int
	owner  string
	broken bool
}

// tree is a parsed file.
type tree struct {
	language *language
	grammar  *ts.Language
	root     *ts.Node
	tree     *ts.Tree
	src      []byte
}

func parse(path string, src []byte) (*tree, error) {
	l := languageOf(path)
	if l == nil {
		return nil, ErrNoGrammar
	}
	grammar := l.grammar()
	parsed, err := ts.NewParser(grammar).Parse(src)
	if err != nil {
		return nil, err
	}
	return &tree{language: l, grammar: grammar, root: parsed.RootNode(), tree: parsed, src: src}, nil
}

// Supported reports whether nib has a grammar for the file at path.
func Supported(path string) bool { return languageOf(path) != nil }

// Find is the lines of each declaration of symbol in src, the file at path, where Outer.inner names inner declared directly inside Outer (or
// belonging to it, as a Go method to its receiver's type); a bare name takes the declarations at the least depth, so overloads come together.
func Find(path string, src []byte, symbol string) ([]Range, error) {
	t, err := parse(path, src)
	if err != nil {
		return nil, err
	}
	defer t.tree.Release()
	declarations := t.declarations()
	names := strings.Split(symbol, ".")
	last := names[len(names)-1]
	chain := func(d declaration) []string {
		var owners []string
		if d.owner != "" {
			owners = append(owners, d.owner)
		}
		for p := d.parent; p >= 0; p = declarations[p].parent {
			owners = append(owners, declarations[p].name)
			if declarations[p].owner != "" {
				owners = append(owners, declarations[p].owner)
			}
		}
		return owners
	}
	// A bare name takes the matches with the fewest owners; a dotted one those whose nearest owners are the names before it.
	type match struct {
		declaration
		depth int
	}
	var matches []match
	least := -1
	for _, d := range declarations {
		if d.name != last {
			continue
		}
		owners := chain(d)
		if len(names) > 1 && (len(owners) < len(names)-1 || !slices.Equal(reversed(owners[:len(names)-1]), names[:len(names)-1])) {
			continue
		}
		matches = append(matches, match{d, len(owners)})
		if least < 0 || len(owners) < least {
			least = len(owners)
		}
	}
	var found, broken []declaration
	for _, m := range matches {
		switch {
		case len(names) == 1 && m.depth > least:
		case m.broken:
			broken = append(broken, m.declaration)
		default:
			found = append(found, m.declaration)
		}
	}
	if len(found) == 0 && len(broken) > 0 {
		return nil, fmt.Errorf("%s at line %d did not parse cleanly, so nib cannot tell its lines", symbol, broken[0].lines.Start)
	}
	if len(found) == 0 {
		if t.root.HasError() {
			return nil, fmt.Errorf("nib found no declaration of %s, but the file has lines that did not parse (from line %d), so it may be there", symbol, firstError(t.root)+1)
		}
		return nil, fmt.Errorf("nib found no declaration of %s", symbol)
	}
	var ranges []Range
	for _, d := range found {
		if !slices.Contains(ranges, d.lines) {
			ranges = append(ranges, d.lines)
		}
	}
	return ranges, nil
}

func reversed(s []string) []string {
	r := slices.Clone(s)
	slices.Reverse(r)
	return r
}

// declarations walks the tree for what it declares; a variable inside a function, or inside a function expression, is a local and left out.
func (t *tree) declarations() []declaration {
	var found []declaration
	var walk func(n *ts.Node, parent int, inFunction bool)
	walk = func(n *ts.Node, parent int, inFunction bool) {
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if !c.IsNamed() {
				continue
			}
			kind := c.Type(t.grammar)
			if r, ok := t.language.rules[kind]; ok && !(r.kind == variable && inFunction) {
				if names, owner := r.names(c, t.grammar, t.src); len(names) > 0 {
					at := len(found)
					lines := t.span(c)
					for _, name := range names {
						found = append(found, declaration{name: name, lines: lines, parent: parent, owner: owner, broken: c.HasError()})
					}
					walk(c, at, r.kind == function)
					continue
				}
			}
			walk(c, parent, inFunction || functionLike(kind))
		}
	}
	walk(t.root, -1, false)
	return found
}

// functionLike tells a function expression (a lambda, an arrow function, a closure) by its node type, whose variables are locals.
func functionLike(kind string) bool {
	for _, part := range []string{"function", "func_literal", "lambda", "closure", "arrow"} {
		if strings.Contains(kind, part) {
			return true
		}
	}
	return false
}

// span is a declaration's lines: the wrapper that holds it alone (export, decorators, a one-spec Go type block), and the comments and
// attributes directly above it, up to a blank line or a comment that ends a line of code.
func (t *tree) span(n *ts.Node) Range {
	outer := n
	for p := outer.Parent(); p != nil && t.language.wrappers[p.Type(t.grammar)] && t.declares(p) == 1; p = outer.Parent() {
		outer = p
	}
	start := int(outer.StartPoint().Row)
	for p := outer.PrevSibling(); p != nil && p.IsNamed() && t.language.above[p.Type(t.grammar)] && endRow(p) >= start-1; p = p.PrevSibling() {
		if q := p.PrevSibling(); q != nil && !t.language.above[q.Type(t.grammar)] && endRow(q) == int(p.StartPoint().Row) {
			break
		}
		start = int(p.StartPoint().Row)
	}
	return Range{start + 1, endRow(outer) + 1}
}

// declares counts a wrapper's children that are declarations.
func (t *tree) declares(n *ts.Node) int {
	count := 0
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c.IsNamed() {
			if _, ok := t.language.rules[c.Type(t.grammar)]; ok {
				count++
			}
		}
	}
	return count
}

// endRow is the row of a node's last character; a node that ends at the start of a line ends on the line before.
func endRow(n *ts.Node) int {
	end := n.EndPoint()
	if end.Column == 0 && end.Row > n.StartPoint().Row {
		return int(end.Row) - 1
	}
	return int(end.Row)
}

func firstError(n *ts.Node) int {
	if n.IsError() || n.IsMissing() {
		return int(n.StartPoint().Row)
	}
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c.HasError() || c.IsError() || c.IsMissing() {
			return firstError(c)
		}
	}
	return int(n.StartPoint().Row)
}

// leading are node types that open a declaration ahead of what it is: modifiers, annotations, decorators and attributes.
var leading = map[string]bool{"modifiers": true, "annotation": true, "marker_annotation": true, "decorator": true, "attribute_item": true}

// Block is the last line of the block that starts at line of src, the file at path: the outermost node that starts on that line, or whose
// first part after its comments, annotations and modifiers does, so a signature line under its annotations ends with its method; from a
// comment or attribute line, through the declaration they stand above. ErrNoGrammar leaves the caller to its own rule; ErrNotABlock when
// it spans only that line.
func Block(path string, src []byte, line int) (int, error) {
	t, err := parse(path, src)
	if err != nil {
		return 0, err
	}
	defer t.tree.Release()
	row := uint32(line - 1)
	starts := func(n *ts.Node) bool {
		if n.StartPoint().Row == row {
			return true
		}
		for i := 0; i < n.ChildCount(); i++ {
			c := n.Child(i)
			if kind := c.Type(t.grammar); !(c.IsNamed() && (leading[kind] || t.language.above[kind])) {
				return c.StartPoint().Row == row
			}
		}
		return false
	}
	var best *ts.Node
	var visit func(n *ts.Node)
	visit = func(n *ts.Node) {
		for i := 0; i < n.ChildCount() && best == nil; i++ {
			c := n.Child(i)
			if c.StartPoint().Row > row || uint32(endRow(c)) < row {
				continue
			}
			if c.IsNamed() && starts(c) {
				best = c
				return
			}
			visit(c)
		}
	}
	visit(t.root)
	if best == nil {
		return 0, ErrNotABlock
	}
	for t.language.above[best.Type(t.grammar)] {
		next := best.NextSibling()
		for next != nil && !next.IsNamed() {
			next = next.NextSibling()
		}
		if next == nil || int(next.StartPoint().Row) > endRow(best)+1 {
			break
		}
		best = next
	}
	if best.HasError() {
		return 0, fmt.Errorf("the block at line %d did not parse cleanly, so nib cannot tell where it ends", line)
	}
	end := endRow(best) + 1
	if end <= line {
		return 0, ErrNotABlock
	}
	return end, nil
}
