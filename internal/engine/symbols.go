package engine

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/11ways/nib/internal/js"
)

// symbolPart is one name of a symbol selector; a name never starts with a digit, so it never reads as a line range.
const symbolPart = `[A-Za-z_$][\w$]*`

var symbolSelector = regexp.MustCompile(`^` + symbolPart + `(?:\.` + symbolPart + `)*$`)

// templateTag matches a Hawkeye tag that opens or closes a named block or snippet: {% block "name" %}, {% snippet name(...) %}, {% /block %}.
var templateTag = regexp.MustCompile(`\{%-?\s*(/?)(block|snippet)\b\s*(?:"([^"]*)"|(` + symbolPart + `))?`)

// symbolRanges are the lines of each declaration of symbol in lines, as Outer.inner names an inner declared inside Outer's block. Each range
// takes the doc comment and annotations above its declaration; overloads, declarations at the same least depth, all count.
func symbolRanges(display string, lines []string, symbol string) ([]lineRange, error) {
	names := strings.Split(symbol, ".")
	if strings.EqualFold(filepath.Ext(display), ".hwk") {
		if len(names) > 1 {
			return nil, fmt.Errorf("%s: a template's blocks and snippets are named alone, like %s:%s", display, display, names[len(names)-1])
		}
		return templateRanges(display, lines, symbol)
	}
	scopes := []lineRange{{1, len(lines)}}
	for depth, name := range names {
		var found []lineRange
		for _, scope := range scopes {
			from := scope.start
			// Inside an outer declaration, its own line is not a member of it.
			if depth > 0 {
				from++
			}
			found = append(found, declarationsIn(lines, name, from, scope.end)...)
		}
		if len(found) == 0 {
			within := ""
			if depth > 0 {
				within = " inside " + strings.Join(names[:depth], ".")
			}
			return nil, fmt.Errorf("%s declares no %s%s; read %s for its outline, or grep it", display, name, within, display)
		}
		scopes = found
	}
	return scopes, nil
}

// declarationsIn are the declarations of name between lines from and to, those at the least indentation, each with its block.
func declarationsIn(lines []string, name string, from, to int) []lineRange {
	quoted := insideStrings(lines)
	word := regexp.MustCompile(`(^|[^\w$])` + regexp.QuoteMeta(name) + `($|[^\w$])`)
	least := -1
	var openers []int
	for line := from; line <= to; line++ {
		content := lines[line-1]
		if quoted[line-1] || js.Trim(content) == "" || isComment(content) {
			continue
		}
		receiver, declaring := declaringParts(content)
		if !word.MatchString(declaring) {
			continue
		}
		// Without a declaring word, only a line that opens a block declares: a call ends where it starts.
		if _, err := blockRange(lines, line); err != nil && !declarationWrd.MatchString(receiver+" "+declaring) {
			continue
		}
		indent := indentOf(content)
		switch {
		case least < 0 || indent < least:
			least, openers = indent, []int{line}
		case indent == least:
			openers = append(openers, line)
		}
	}
	var ranges []lineRange
	for _, opener := range openers {
		end := opener
		if block, err := blockRange(lines, opener); err == nil {
			end = block
		}
		start := opener
		for start > from && js.Trim(lines[start-2]) != "" && (isComment(lines[start-2]) || decorator.MatchString(lines[start-2])) {
			start--
		}
		ranges = append(ranges, lineRange{start, end})
	}
	return ranges
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

// templateRanges are the lines of each Hawkeye block or snippet named name, from its opening tag to the tag that closes it.
func templateRanges(display string, lines []string, name string) ([]lineRange, error) {
	var ranges []lineRange
	for index, content := range lines {
		for _, open := range templateTag.FindAllStringSubmatch(content, -1) {
			if open[1] != "" || (open[3] != name && open[4] != name) || (open[3] == "" && open[4] == "") {
				continue
			}
			end := templateEnd(lines, index, open[2])
			if end < 0 {
				return nil, fmt.Errorf("%s: the %s %s on line %d is never closed with {%% /%s %%}", display, open[2], name, index+1, open[2])
			}
			ranges = append(ranges, lineRange{index + 1, end + 1})
		}
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("%s has no block or snippet named %s; read %s, or grep it", display, name, display)
	}
	return ranges, nil
}

// templateEnd is the index of the line holding the tag that closes the kind of tag opened on line index, counting nested ones; -1 for none.
func templateEnd(lines []string, index int, kind string) int {
	depth := 0
	for at := index; at < len(lines); at++ {
		for _, tag := range templateTag.FindAllStringSubmatch(lines[at], -1) {
			if tag[2] != kind {
				continue
			}
			if tag[1] == "" {
				depth++
			} else {
				depth--
			}
			if depth == 0 {
				return at
			}
		}
	}
	return -1
}
