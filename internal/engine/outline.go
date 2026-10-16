package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/snapshots"
	"github.com/11ways/nib/internal/syntax"
)

// jsSpace is JavaScript's \s as an RE2 class.
const jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

const (
	outlineLines      = 500
	outlineBytes      = 20 * 1024
	outlineRows       = 150
	outlineMinRows    = 5
	headLines         = 100
	skipBytes         = 4 * 1024
	minifiedLineChars = 300
	minifiedColumns   = 300
)

var (
	bundleName     = regexp.MustCompile(`(?i)\.(bundle|chunk)\.(js|mjs|cjs|css)$`)
	hashedName     = regexp.MustCompile(`(?i)[.-]([0-9a-f]{8,})\.(js|mjs|cjs|css)$`)
	minifiedName   = regexp.MustCompile(`\.min\.(js|mjs|css)$|\.map$`)
	container      = regexp.MustCompile(`\b(class|interface|impl|trait|struct|enum|object|namespace|module|record|protocol|extension)\b`)
	importLine     = regexp.MustCompile(`^(import|from|use|require|#include|using|package|extern crate)\b`)
	decorator      = regexp.MustCompile(`^` + jsSpace + `*(@|#\[)`)
	blockCloser    = regexp.MustCompile(`^([)\]}]|end\b)`)
	continuation   = regexp.MustCompile(`^([)\]}]|[{(\[]` + jsSpace + `*$|(else|elif|except|finally|catch)\b)`)
	markdown       = regexp.MustCompile(`(?i)\.(md|mdx|markdown)$`)
	fence          = regexp.MustCompile(`^` + jsSpace + `*(` + "```" + `|~~~)`)
	heading        = regexp.MustCompile(`^#{1,6}` + jsSpace)
	declarationEnd = regexp.MustCompile("[(=:{<\\[,;\"'`]")
	declarationGrp = regexp.MustCompile(`^` + jsSpace + `*(func|pub)` + jsSpace + `*\([^)]*\)`)
	declarationWrd = regexp.MustCompile(`\b(function|def|fn|func|fun|class|interface|type|struct|enum|trait|impl|union|const|let|var|val|export|pub|module|namespace|object|record|protocol|extension|macro|sub|proc|local|static|public|private|protected|internal|abstract|final|async|override|readonly|declare|default|extern|inline|virtual|sealed|typealias|defmodule|defp|defn|defmacro)\b`)
	regexPrefix    = regexp.MustCompile(`(^|[(,=:\[!&|?{};]|\breturn)` + jsSpace + `*$`)
	escaped        = regexp.MustCompile(`\\[^\n\r\x{2028}\x{2029}]`)
)

var lockfiles = map[string]bool{"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lock": true, "Cargo.lock": true, "poetry.lock": true, "uv.lock": true, "Pipfile.lock": true, "composer.lock": true, "Gemfile.lock": true, "go.sum": true, "flake.lock": true, "gradle.lockfile": true, "packages.lock.json": true}

var generatedDirectories = map[string]bool{"node_modules": true, "dist": true}

// privateMember is a JavaScript private field or method, #name followed by what declares or calls it, which a # comment rarely is.
var privateMember = regexp.MustCompile(`^#[A-Za-z_$][\w$]*` + jsSpace + `*[(=:;<!?]`)

// isComment is /^\s*(\/\/|\/\*|\*|<!--|--|#(?![[!]|include\b))/ without the lookahead RE2 lacks, and with private members left as code.
func isComment(line string) bool {
	t := js.TrimStart(line)
	for _, prefix := range []string{"//", "/*", "*", "<!--", "--"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	if !strings.HasPrefix(t, "#") || privateMember.MatchString(t) {
		return false
	}
	rest := t[1:]
	if strings.HasPrefix(rest, "[") || strings.HasPrefix(rest, "!") {
		return false
	}
	if strings.HasPrefix(rest, "include") {
		after := rest[len("include"):]
		return after != "" && isWordByte(after[0])
	}
	return true
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isCloser(line string) bool {
	for _, r := range line {
		if !js.IsSpace(r) && !strings.ContainsRune(")]}>;,", r) {
			return false
		}
	}
	return true
}

func isBundleName(name string) bool {
	if bundleName.MatchString(name) {
		return true
	}
	match := hashedName.FindStringSubmatch(name)
	return match != nil && strings.ContainsAny(strings.ToLower(match[1]), "abcdef")
}

var codePunctuation = func() map[rune]bool {
	set := map[rune]bool{}
	for _, r := range "{}()[];,=<>+*&|!?" {
		set[r] = true
	}
	return set
}()

// minifiedReason says why a file looks minified or bundled, if it does: by its name, or by most of its text sitting in dense lines.
func minifiedReason(canonical string, size int, lines []string) string {
	name := filepath.Base(canonical)
	if minifiedName.MatchString(name) {
		return "minified or source map"
	}
	if isBundleName(name) {
		return "bundled or minified"
	}
	dense := 0
	for _, line := range lines {
		length := js.Len(line)
		if length <= minifiedLineChars {
			continue
		}
		spaces, punctuation := 0, 0
		for _, r := range line {
			if r == ' ' || r == '\t' {
				spaces++
			} else if codePunctuation[r] {
				punctuation++
			}
		}
		if spaces*20 < length || punctuation > spaces {
			dense += length
		}
	}
	if dense*2 > size {
		return "minified (most of it in long, dense lines)"
	}
	return ""
}

// generatedKind says what kind of generated file this is, if it is one.
func generatedKind(canonical string, size int, lines []string) string {
	if lockfiles[filepath.Base(canonical)] {
		return "lockfile"
	}
	if minified := minifiedReason(canonical, size, lines); minified != "" {
		return minified
	}
	for _, part := range strings.Split(canonical, string(filepath.Separator)) {
		if generatedDirectories[part] {
			return "inside " + part + "/"
		}
	}
	return ""
}

// junk says why an unselected read of this file is not worth its tokens, if it is not.
func junk(canonical string, size int, lines []string) string {
	if size <= skipBytes {
		return ""
	}
	return generatedKind(canonical, size, lines)
}

// insideStrings marks lines that continue a multi-line string (triple quotes, template literals, Go raw strings) so their text is not taken for code.
// Quote marks inside one-line strings, comments and regex literals open nothing.
func insideStrings(lines []string) []bool { return scanCode(lines, nil) }

// scanCode walks lines as code, handing code every byte outside strings, comments and regex literals with its line's index, and says which
// lines start inside a multi-line string. A template literal's ${...} is code, so a template nested in one does not end the outer.
func scanCode(lines []string, code func(index int, char byte)) []bool {
	open := ""
	// Each ${ entered from a template holds its own brace depth; the } that ends it goes back into the template.
	var templates []int
	result := make([]bool, len(lines))
	for i, content := range lines {
		wasInside := open != ""
		result[i] = wasInside
		if !wasInside && len(templates) == 0 && isComment(content) {
			continue
		}
		// Escaped characters never open or close anything.
		t := escaped.ReplaceAllString(content, "  ")
		at := 0
		for at < len(t) {
			if open == "`" {
				end := strings.IndexByte(t[at:], '`')
				interpolation := strings.Index(t[at:], "${")
				if interpolation >= 0 && (end < 0 || interpolation < end) {
					at += interpolation + 2
					open = ""
					templates = append(templates, 0)
					continue
				}
				if end < 0 {
					break
				}
				at += end + 1
				open = ""
				continue
			}
			if open != "" {
				end := strings.Index(t[at:], open)
				if end < 0 {
					break
				}
				at += end + len(open)
				open = ""
				continue
			}
			char := t[at]
			if top := len(templates) - 1; top >= 0 && (char == '{' || char == '}') {
				if char == '}' && templates[top] == 0 {
					templates = templates[:top]
					open = "`"
					at++
					continue
				}
				if char == '{' {
					templates[top]++
				} else {
					templates[top]--
				}
			}
			if strings.HasPrefix(t[at:], `"""`) || strings.HasPrefix(t[at:], "'''") {
				open = t[at : at+3]
				at += 3
			} else if char == '`' {
				open = "`"
				at++
			} else if char == '"' || char == '\'' {
				end := strings.IndexByte(t[at+1:], char)
				if end < 0 {
					at = len(t)
				} else {
					at += 1 + end + 1
				}
			} else if strings.HasPrefix(t[at:], "//") {
				break
			} else if strings.HasPrefix(t[at:], "/*") {
				end := strings.Index(t[at+2:], "*/")
				if end < 0 {
					at = len(t)
				} else {
					at += 2 + end + 2
				}
			} else if char == '/' && regexPrefix.MatchString(t[:at]) {
				at = regexEnd(t, at+1)
			} else {
				if code != nil {
					code(i, char)
				}
				at++
			}
		}
	}
	return result
}

// regexEnd is the offset just past a regex literal whose body starts at at; a / inside a [class] does not end it.
func regexEnd(t string, at int) int {
	inClass := false
	for ; at < len(t); at++ {
		switch t[at] {
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				return at + 1
			}
		}
	}
	return len(t)
}

func indentOf(content string) int { return js.Len(content) - js.Len(js.TrimStart(content)) }

// blockRange is the last line of the block that starts at line, by indentation like outline: leading comments and decorators, the opener,
// its deeper body, and a closing line at the opener's depth.
func blockRange(lines []string, line int) (int, error) {
	if line > len(lines) {
		return 0, fmt.Errorf("Line %d does not exist (file has %d lines)", line, len(lines))
	}
	quoted := insideStrings(lines)
	code := func(index int) bool {
		return js.Trim(lines[index]) != "" && !quoted[index] && !isComment(lines[index])
	}
	refuse := fmt.Errorf("Line %d does not start a multi-line block; use PUT N.=M: or CUT N.=M instead", line)
	opener := line - 1
	for opener < len(lines) && js.Trim(lines[opener]) != "" && !code(opener) {
		opener++
	}
	for opener < len(lines) && decorator.MatchString(lines[opener]) {
		opener++
	}
	if opener >= len(lines) || !code(opener) || indentOf(lines[opener]) != indentOf(lines[line-1]) {
		return 0, refuse
	}
	depth := indentOf(lines[opener])
	nextCode := func(from int) int {
		for index := from + 1; index < len(lines); index++ {
			if code(index) {
				return index
			}
		}
		return -1
	}
	last := opener
	body := false
	for index := opener + 1; index < len(lines); index++ {
		content := lines[index]
		if js.Trim(content) == "" {
			continue
		}
		// A deeper comment belongs to the body only after deeper code, so a following doc comment never counts as one.
		if quoted[index] || (indentOf(content) > depth && (body || code(index))) {
			body = true
			last = index
			continue
		}
		if !code(index) {
			continue
		}
		trimmed := js.TrimStart(content)
		next := nextCode(index)
		if indentOf(content) == depth && continuation.MatchString(trimmed) && next >= 0 && indentOf(lines[next]) > depth {
			last = index
			continue
		}
		if indentOf(content) == depth && blockCloser.MatchString(trimmed) {
			last = index
		}
		break
	}
	if !body {
		return 0, refuse
	}
	return last + 1, nil
}

// resolveBlocks gives block edits their resolved end: a block replace or cut spans the whole block, a block insert goes after it. In a
// language nib has a grammar for, the syntax tree says where the block ends; in any other file, indentation does.
func resolveBlocks(path string, edits []snapshots.LineEdit, lines []string) ([]snapshots.LineEdit, error) {
	resolved := make([]snapshots.LineEdit, len(edits))
	for i, edit := range edits {
		if !edit.Block {
			resolved[i] = edit
			continue
		}
		end, err := syntax.Block(path, []byte(strings.Join(lines, "\n")), edit.Start)
		switch {
		case errors.Is(err, syntax.ErrNoGrammar):
			end, err = blockRange(lines, edit.Start)
		case errors.Is(err, syntax.ErrNotABlock):
			err = fmt.Errorf("Line %d does not start a multi-line block; use PUT N.=M: or CUT N.=M instead", edit.Start)
		case err != nil:
			err = fmt.Errorf("%v; use PUT N.=M: or CUT N.=M instead", err)
		}
		if err != nil {
			return nil, err
		}
		edit.Block = false
		if edit.Operation == snapshots.After {
			confirmed := []int{}
			for _, c := range edit.Confirmed {
				if c == edit.Start {
					confirmed = []int{end}
					break
				}
			}
			edit.Start, edit.End, edit.Confirmed = end, end, confirmed
		} else {
			edit.End = end
			edit.Confirmed = append(append([]int{}, edit.Confirmed...), end)
		}
		resolved[i] = edit
	}
	return resolved, nil
}

// headings are Markdown heading lines outside fenced code.
func headings(lines []string) []int {
	var found []int
	fenced := false
	for index, content := range lines {
		if fence.MatchString(content) {
			fenced = !fenced
		} else if !fenced && heading.MatchString(content) {
			found = append(found, index+1)
		}
	}
	return found
}

type candidate struct{ line, depth int }

type laidOut struct {
	candidates []candidate
	code       func(int) bool
	depth      func(string) int
}

// layout finds the code lines an outline can list, by indentation: top-level lines and the members of top-level container types.
func layout(lines []string) laidOut {
	quoted := insideStrings(lines)
	code := func(index int) bool {
		return js.Trim(lines[index]) != "" && !quoted[index] && !isComment(lines[index])
	}
	unit := -1
	for index, content := range lines {
		if indent := indentOf(content); code(index) && indent > 0 && (unit < 0 || indent < unit) {
			unit = indent
		}
	}
	depth := func(content string) int {
		if unit < 0 {
			return 0
		}
		return indentOf(content) / unit
	}
	var candidates []candidate
	inContainer := false
	for index, content := range lines {
		trimmed := js.TrimStart(content)
		if !code(index) || isCloser(content) || (trimmed != "" && strings.ContainsRune(")]}", rune(trimmed[0]))) {
			continue
		}
		level := depth(content)
		if level == 0 {
			inContainer = container.MatchString(content)
			candidates = append(candidates, candidate{index + 1, 0})
		} else if level == 1 && inContainer {
			candidates = append(candidates, candidate{index + 1, 1})
		}
	}
	return laidOut{candidates, code, depth}
}

// declaringParts split a line as grep tells definitions from uses: a Go receiver, if any, and after it the part that names what the line
// declares, up to the first ( = : { < [ , ; or quote.
func declaringParts(content string) (receiver, declaring string) {
	receiver = declarationGrp.FindString(content)
	declaring = content[len(receiver):]
	if at := declarationEnd.FindStringIndex(declaring); at != nil {
		declaring = declaring[:at[0]]
	}
	return receiver, declaring
}

// definitionLines are the lines among matched whose first match sits in their declaring part on a line an outline would list.
func definitionLines(display string, lines []string, matched []int, spans map[int][]span) map[int]bool {
	listed := map[int]bool{}
	if markdown.MatchString(display) {
		for _, line := range headings(lines) {
			listed[line] = false
		}
	} else {
		for _, c := range layout(lines).candidates {
			if c.depth == 0 && importLine.MatchString(lines[c.line-1]) {
				continue
			}
			listed[c.line] = c.depth == 0
		}
	}
	found := map[int]bool{}
	for _, line := range matched {
		s := spans[line]
		needsWord, ok := listed[line]
		if len(s) == 0 || !ok {
			continue
		}
		start := s[0].start
		content := lines[line-1]
		receiver, declaring := declaringParts(content)
		from := js.Len(receiver)
		if start >= from && start < from+js.Len(declaring) && (!needsWord || declarationWrd.MatchString(receiver+" "+declaring)) {
			found[line] = true
		}
	}
	return found
}

type outlineRow struct {
	line int
	text string
}

type sketch struct {
	rows       []outlineRow
	candidates int
}

// outline lists top-level code lines plus the members of top-level container types; runs of imports or one-line declarations show their first
// line and a count. Markdown uses headings.
func outline(display string, lines []string) sketch {
	if markdown.MatchString(display) {
		found := headings(lines)
		rows := make([]outlineRow, len(found))
		for i, line := range found {
			rows[i] = outlineRow{line: line}
		}
		return sketch{rows, len(rows)}
	}
	l := layout(lines)
	opens := func(line int) bool {
		for index := line; index < len(lines); index++ {
			if l.code(index) {
				return l.depth(lines[index]) > l.depth(lines[line-1])
			}
		}
		return false
	}
	kind := func(c candidate) string {
		if c.depth == 0 && importLine.MatchString(lines[c.line-1]) {
			return "import"
		}
		if opens(c.line) {
			return "block"
		}
		return "line"
	}
	type group struct {
		rows   []outlineRow
		covers int
	}
	groups := func(list []candidate) []group {
		var result []group
		for index := 0; index < len(list); {
			first := list[index]
			run := kind(first)
			last := index
			if run != "block" {
				for last+1 < len(list) && list[last+1].depth == first.depth && kind(list[last+1]) == run {
					last++
				}
			}
			if run == "line" && last-index < 2 {
				last = index
			}
			rows := []outlineRow{{line: first.line}}
			if last > index {
				what := "one-line declarations"
				if run == "import" {
					what = "import lines"
				}
				rows = append(rows, outlineRow{text: fmt.Sprintf("[lines %d-%d: %d more %s]", first.line+1, list[last].line, last-index, what)})
			}
			result = append(result, group{rows, last - index + 1})
			index = last + 1
		}
		return result
	}
	size := func(list []group) int {
		n := 0
		for _, g := range list {
			n += len(g.rows)
		}
		return n
	}
	chosen := groups(l.candidates)
	if size(chosen) > outlineRows {
		// Over the cap, top-level rows stay and members fill the remaining room in file order.
		var top []candidate
		for _, c := range l.candidates {
			if c.depth == 0 {
				top = append(top, c)
			}
		}
		room := outlineRows - size(groups(top))
		var kept []candidate
		for _, c := range l.candidates {
			if c.depth == 0 {
				kept = append(kept, c)
				continue
			}
			if room > 0 {
				kept = append(kept, c)
			}
			room--
		}
		chosen = groups(kept)
	}
	var rows []outlineRow
	covered := 0
	for _, g := range chosen {
		if len(rows)+len(g.rows) > outlineRows {
			break
		}
		rows = append(rows, g.rows...)
		covered += g.covers
	}
	if omitted := len(l.candidates) - covered; omitted > 0 {
		rows = append(rows, outlineRow{text: fmt.Sprintf("[outline capped at %d rows; %d more members or declarations not shown]", outlineRows, omitted)})
	}
	return sketch{rows, len(l.candidates)}
}
