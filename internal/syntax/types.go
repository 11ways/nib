package syntax

import (
	"strings"
	"unicode"

	ts "github.com/odvcencio/gotreesitter"
)

// typeNodes are the node types a grammar names types with: name is a type's name, scoped a qualified one (`Map.Entry`), whose first
// segment alone resolves through an import, parameter a type parameter, which declares its name; annotations name their type in a name
// field, receivers are nodes whose field may hold a class name (`Duration.ofSeconds`), and references take one as their first child
// (`Ref::go`).
type typeNodes struct {
	name, scoped, parameter string
	annotations             map[string]bool
	receivers               map[string]string
	references              string
}

// Inspection is what one parse of a file tells: the first line that did not parse, 0 when every line did, and, in a language whose type
// names nib tells apart (Types), the class names each line uses by their first segment, the type names the file declares, the names
// every file sees without an import (Implicit), and whether it declares a type at its top level, as a compact source file does not.
type Inspection struct {
	ErrorLine int
	Types     bool
	Uses      map[int][]string
	Declared  map[string]bool
	Implicit  map[string]bool
	TopLevel  bool
}

// Inspect parses src, the file at path, once for what an edit's checks need; ErrNoGrammar for a language without a grammar.
func Inspect(path string, src []byte) (Inspection, error) {
	t, err := parse(path, src)
	if err != nil {
		return Inspection{}, err
	}
	defer t.tree.Release()
	var found Inspection
	if t.root.HasError() {
		found.ErrorLine = firstError(t.root) + 1
	}
	nodes := t.language.types
	if nodes == nil {
		return found, nil
	}
	found.Types, found.Uses, found.Declared = true, map[int][]string{}, map[string]bool{}
	if t.language.imports != nil {
		found.Implicit = jdkPackageClasses(t.language.imports.implicit)
	}
	for _, d := range t.declarations() {
		if d.kind == container {
			found.Declared[d.name] = true
			found.TopLevel = found.TopLevel || d.parent < 0
		}
	}
	use := func(n *ts.Node) {
		if n == nil || n.Type(t.grammar) != "identifier" && n.Type(t.grammar) != nodes.name {
			return
		}
		if name := n.Text(t.src); className(name) {
			row := int(n.StartPoint().Row) + 1
			found.Uses[row] = append(found.Uses[row], name)
		}
	}
	var walk func(n *ts.Node)
	walk = func(n *ts.Node) {
		kind := n.Type(t.grammar)
		switch {
		case kind == nodes.name:
			parent := n.Parent()
			switch {
			case parent != nil && parent.Type(t.grammar) == nodes.parameter:
				found.Declared[n.Text(t.src)] = true
			case parent != nil && parent.Type(t.grammar) == nodes.scoped && parent.NamedChild(0) != n:
			default:
				use(n)
			}
		case nodes.annotations[kind]:
			use(n.ChildByFieldName("name", t.grammar))
		case nodes.receivers[kind] != "":
			use(n.ChildByFieldName(nodes.receivers[kind], t.grammar))
		case kind == nodes.references:
			use(n.NamedChild(0))
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(t.root)
	return found, nil
}

// className says whether a name reads as a class's by Java's naming: a capital first, with a small letter in it, unlike a variable or a
// CONSTANT.
func className(name string) bool {
	first := []rune(name)
	return len(first) > 0 && unicode.IsUpper(first[0]) && strings.ContainsFunc(name, unicode.IsLower)
}
