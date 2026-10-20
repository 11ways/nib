package syntax

import (
	"path/filepath"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars/c"
	"github.com/odvcencio/gotreesitter/grammars/cpp"
	golang "github.com/odvcencio/gotreesitter/grammars/go"
	"github.com/odvcencio/gotreesitter/grammars/java"
	"github.com/odvcencio/gotreesitter/grammars/javascript"
	"github.com/odvcencio/gotreesitter/grammars/kotlin"
	"github.com/odvcencio/gotreesitter/grammars/python"
	"github.com/odvcencio/gotreesitter/grammars/rust"
	"github.com/odvcencio/gotreesitter/grammars/tsx"
	"github.com/odvcencio/gotreesitter/grammars/typescript"
	"github.com/odvcencio/gotreesitter/grammars/zig"
)

// kind is what a declaration declares; a variable whose nearest enclosing declaration is a function is a local, which declares nothing.
type kind int

const (
	container kind = iota
	function
	variable
)

// rule says a node type declares, what, and its names; owner, when set, is the type a declaration outside it belongs to (a Go method's
// receiver, a C++ definition's Class:: qualifier).
type rule struct {
	kind  kind
	names func(n *ts.Node, l *ts.Language, src []byte) (names []string, owner string)
}

// language is one grammar and what its trees declare. above are node types that belong to the declaration below them (comments,
// attributes, decorators); wrappers are node types that wrap one declaration with lines of their own (export, decorators, template<>).
// imports is how its import declarations read, nil where nib reads none, and types the node types that name a type, nil where nib does
// not tell type names apart.
type language struct {
	name     string
	grammar  func() *ts.Language
	rules    map[string]rule
	above    map[string]bool
	wrappers map[string]bool
	imports  *imports
	types    *typeNodes
}

// languages are the grammars nib parses, by file extension: the one home of that mapping.
var languages = func() map[string]*language {
	javaLike := &language{name: "Java", grammar: java.Language, above: set("line_comment", "block_comment"), wrappers: set(), rules: map[string]rule{
		"class_declaration":                   {container, field("name")},
		"interface_declaration":               {container, field("name")},
		"enum_declaration":                    {container, field("name")},
		"record_declaration":                  {container, field("name")},
		"annotation_type_declaration":         {container, field("name")},
		"method_declaration":                  {function, field("name")},
		"constructor_declaration":             {function, field("name")},
		"compact_constructor_declaration":     {function, field("name")},
		"annotation_type_element_declaration": {function, field("name")},
		"field_declaration":                   {variable, declarators("declarator")},
		"constant_declaration":                {variable, declarators("declarator")},
		"enum_constant":                       {variable, field("name")},
	}, imports: jvmImports(true, true, false, true, "java.lang"), types: &typeNodes{
		name: "type_identifier", scoped: "scoped_type_identifier", parameter: "type_parameter",
		annotations: set("marker_annotation", "annotation"),
		receivers:   map[string]string{"method_invocation": "object", "field_access": "object"},
		references:  "method_reference",
	}}
	kotlinLike := &language{name: "Kotlin", grammar: kotlin.Language, above: set("line_comment", "multiline_comment"), wrappers: set(), rules: map[string]rule{
		"class_declaration":    {container, child("type_identifier")},
		"object_declaration":   {container, child("type_identifier")},
		"companion_object":     {container, companion},
		"function_declaration": {function, child("simple_identifier")},
		"property_declaration": {variable, path("variable_declaration", "simple_identifier")},
		"type_alias":           {variable, child("type_identifier")},
		"enum_entry":           {variable, child("simple_identifier")},
	}, imports: jvmImports(false, false, true, false, "")}
	script := func(name string, grammar func() *ts.Language) *language {
		return &language{name: name, grammar: grammar, above: set("comment", "decorator"), wrappers: set("export_statement"), rules: map[string]rule{
			"function_declaration":           {function, field("name")},
			"generator_function_declaration": {function, field("name")},
			"function_signature":             {function, field("name")},
			"class_declaration":              {container, field("name")},
			"abstract_class_declaration":     {container, field("name")},
			"interface_declaration":          {container, field("name")},
			"enum_declaration":               {container, field("name")},
			"internal_module":                {container, field("name")},
			"module":                         {container, field("name")},
			"type_alias_declaration":         {variable, field("name")},
			"method_definition":              {function, methodName},
			"method_signature":               {function, methodName},
			"pair":                           {function, functionValue("key", "value")},
			"assignment_expression":          {function, functionValue("left", "right")},
			"abstract_method_signature":      {function, field("name")},
			"public_field_definition":        {variable, field("name")},
			"field_definition":               {variable, field("property")},
			"variable_declarator":            {variable, identifier("name")},
		}}
	}
	goLike := &language{name: "Go", grammar: golang.Language, above: set("comment"), wrappers: set("type_declaration", "var_declaration", "const_declaration"), rules: map[string]rule{
		"function_declaration": {function, field("name")},
		"method_declaration":   {function, receiver},
		"type_spec":            {container, field("name")},
		"type_alias":           {variable, field("name")},
		"method_elem":          {function, field("name")},
		"field_declaration":    {variable, declarators("name")},
		"var_spec":             {variable, declarators("name")},
		"const_spec":           {variable, declarators("name")},
	}}
	pythonLike := &language{name: "Python", grammar: python.Language, above: set("comment"), wrappers: set("decorated_definition"), rules: map[string]rule{
		"class_definition":    {container, field("name")},
		"function_definition": {function, field("name")},
		"assignment":          {variable, identifier("left")},
	}}
	rustLike := &language{name: "Rust", grammar: rust.Language, above: set("line_comment", "block_comment", "attribute_item"), wrappers: set(), rules: map[string]rule{
		"function_item":           {function, field("name")},
		"function_signature_item": {function, field("name")},
		"macro_definition":        {function, field("name")},
		"struct_item":             {container, field("name")},
		"enum_item":               {container, field("name")},
		"union_item":              {container, field("name")},
		"trait_item":              {container, field("name")},
		"mod_item":                {container, field("name")},
		"impl_item":               {container, implType},
		"const_item":              {variable, field("name")},
		"static_item":             {variable, field("name")},
		"type_item":               {variable, field("name")},
		"enum_variant":            {variable, field("name")},
		"field_declaration":       {variable, field("name")},
	}}
	cLike := func(name string, grammar func() *ts.Language) *language {
		return &language{name: name, grammar: grammar, above: set("comment"), wrappers: set("template_declaration"), rules: map[string]rule{
			"function_definition":  {function, declarator},
			"struct_specifier":     {container, withBody(field("name"))},
			"union_specifier":      {container, withBody(field("name"))},
			"enum_specifier":       {container, withBody(field("name"))},
			"class_specifier":      {container, withBody(field("name"))},
			"namespace_definition": {container, field("name")},
			"preproc_def":          {variable, field("name")},
			"preproc_function_def": {function, field("name")},
			"type_definition":      {variable, declarator},
			"enumerator":           {variable, field("name")},
			"declaration":          {variable, declarator},
			"field_declaration":    {variable, declarator},
		}}
	}
	zigLike := &language{name: "Zig", grammar: zig.Language, above: set("comment"), wrappers: set(), rules: map[string]rule{
		"function_declaration": {function, field("name")},
		"variable_declaration": {variable, child("identifier")},
		"container_field":      {variable, field("name")},
	}}
	typeScript, tsxLike, js := script("TypeScript", typescript.Language), script("TSX", tsx.Language), script("JavaScript", javascript.Language)
	cOnly, cPlus := cLike("C", c.Language), cLike("C++", cpp.Language)
	return map[string]*language{
		".java": javaLike, ".kt": kotlinLike, ".kts": kotlinLike,
		".ts": typeScript, ".mts": typeScript, ".cts": typeScript, ".tsx": tsxLike, ".js": js, ".jsx": js, ".mjs": js, ".cjs": js,
		".go": goLike, ".py": pythonLike, ".pyi": pythonLike, ".rs": rustLike, ".zig": zigLike,
		".c": cOnly, ".h": cOnly, ".cpp": cPlus, ".cc": cPlus, ".cxx": cPlus, ".hpp": cPlus, ".hh": cPlus, ".hxx": cPlus,
	}
}()

// languageOf is the language of a file by its extension; nil when nib has no grammar for it.
func languageOf(path string) *language { return languages[strings.ToLower(filepath.Ext(path))] }

func set(types ...string) map[string]bool {
	m := map[string]bool{}
	for _, t := range types {
		m[t] = true
	}
	return m
}

// field names a declaration by one field's text.
func field(name string) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return func(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
		if c := n.ChildByFieldName(name, l); c != nil {
			return []string{c.Text(src)}, ""
		}
		return nil, ""
	}
}

// identifier is field, for a field that names something only when it is a plain identifier, not a destructuring pattern or a member.
func identifier(name string) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return func(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
		if c := n.ChildByFieldName(name, l); c != nil && c.Type(l) == "identifier" {
			return []string{c.Text(src)}, ""
		}
		return nil, ""
	}
}

// declarators names a declaration by every child in one field, as `int a, b;` declares two; each may itself be a declarator with a name.
func declarators(name string) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return func(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
		var names []string
		for i := 0; i < n.ChildCount(); i++ {
			if n.FieldNameForChild(i, l) != name {
				continue
			}
			c := n.Child(i)
			if inner := c.ChildByFieldName("name", l); inner != nil {
				c = inner
			}
			names = append(names, c.Text(src))
		}
		return names, ""
	}
}

// child names a declaration by its first named child of one type, for grammars without a name field.
func child(kind string) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return path(kind)
}

// path names a declaration by following first named children of the given types.
func path(kinds ...string) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return func(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
		for _, kind := range kinds {
			n = firstOf(n, l, kind)
			if n == nil {
				return nil, ""
			}
		}
		return []string{n.Text(src)}, ""
	}
}

func firstOf(n *ts.Node, l *ts.Language, kind string) *ts.Node {
	for i := 0; i < n.ChildCount(); i++ {
		if c := n.Child(i); c.IsNamed() && c.Type(l) == kind {
			return c
		}
	}
	return nil
}

// withBody names a C struct, union, enum or class only where it is defined, not where `struct S` merely names its type.
func withBody(names func(*ts.Node, *ts.Language, []byte) ([]string, string)) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return func(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
		if n.ChildByFieldName("body", l) == nil {
			return nil, ""
		}
		return names(n, l, src)
	}
}

// companion is a Kotlin companion object's name, Companion unless it has its own.
func companion(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
	if c := firstOf(n, l, "type_identifier"); c != nil {
		return []string{c.Text(src)}, ""
	}
	return []string{"Companion"}, ""
}

// methodName names a method by the last name before its parameters. gotreesitter 0.56 parses `static async *items()` with `async` in the
// name field and `items` after it; on a sound parse the name field is that last name anyway.
func methodName(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
	name := ""
	for i := 0; i < n.ChildCount(); i++ {
		if n.FieldNameForChild(i, l) == "parameters" {
			break
		}
		switch c := n.Child(i); c.Type(l) {
		case "property_identifier", "private_property_identifier":
			name = c.Text(src)
		}
	}
	if name == "" {
		return field("name")(n, l, src)
	}
	return []string{name}, ""
}

// functionValue names a function assigned to a key or a member (`remove: function () {}`, `module.exports.legacy = () => {}`) by that
// key or the member's last property; any other value declares nothing.
func functionValue(target, value string) func(*ts.Node, *ts.Language, []byte) ([]string, string) {
	return func(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
		v, t := n.ChildByFieldName(value, l), n.ChildByFieldName(target, l)
		if v == nil || t == nil || !functionLike(v.Type(l)) {
			return nil, ""
		}
		if p := t.ChildByFieldName("property", l); p != nil {
			t = p
		}
		switch t.Type(l) {
		case "property_identifier", "identifier", "private_property_identifier":
			return []string{t.Text(src)}, ""
		}
		return nil, ""
	}
}

// receiver names a Go method and the type it belongs to, from its receiver's type without pointer or type arguments.
func receiver(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
	names, _ := field("name")(n, l, src)
	owner := ""
	if r := n.ChildByFieldName("receiver", l); r != nil {
		if p := firstOf(r, l, "parameter_declaration"); p != nil {
			if t := p.ChildByFieldName("type", l); t != nil {
				owner = baseType(t.Text(src))
			}
		}
	}
	return names, owner
}

// implType names a Rust impl block by the type it implements for, so its functions are that type's members.
func implType(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
	if t := n.ChildByFieldName("type", l); t != nil {
		return []string{baseType(t.Text(src))}, ""
	}
	return nil, ""
}

// baseType is a type's own name: no pointer, reference or type arguments, and the last segment of a path.
func baseType(text string) string {
	text = strings.TrimLeft(text, "*&")
	if at := strings.IndexAny(text, "[<"); at >= 0 {
		text = text[:at]
	}
	if at := strings.LastIndex(text, "::"); at >= 0 {
		text = text[at+2:]
	}
	if at := strings.LastIndex(text, "."); at >= 0 {
		text = text[at+1:]
	}
	return strings.TrimSpace(text)
}

// declarator names a C or C++ declaration through its declarator chain (pointers, references, arrays, function declarators), with a
// Class:: qualifier as owner. A declaration whose innermost declarator is a function declarator declares a function prototype.
func declarator(n *ts.Node, l *ts.Language, src []byte) ([]string, string) {
	var names []string
	owner := ""
	for i := 0; i < n.ChildCount(); i++ {
		if n.FieldNameForChild(i, l) != "declarator" {
			continue
		}
		d := n.Child(i)
		for {
			next := d.ChildByFieldName("declarator", l)
			if next == nil {
				break
			}
			d = next
		}
		switch d.Type(l) {
		case "identifier", "field_identifier", "type_identifier", "destructor_name", "operator_name":
			names = append(names, d.Text(src))
		case "qualified_identifier":
			if name := d.ChildByFieldName("name", l); name != nil {
				names = append(names, name.Text(src))
			}
			if scope := d.ChildByFieldName("scope", l); scope != nil {
				owner = baseType(scope.Text(src))
			}
		}
	}
	return names, owner
}
