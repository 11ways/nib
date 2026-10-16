package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/11ways/nib/internal/syntax"
)

// symbolPart is one name of a symbol selector; a name never starts with a digit, so it never reads as a line range.
const symbolPart = `[A-Za-z_$][\w$]*`

// templateTag matches a Hawkeye tag that opens or closes a named block or snippet: {% block "name" %}, {% snippet name(...) %}, {% /block %}.
var templateTag = regexp.MustCompile(`\{%-?\s*(/?)(block|snippet)\b\s*(?:"([^"]*)"|(` + symbolPart + `))?`)

// symbolRanges are the lines of each declaration of symbol in lines, as the file's syntax tree has them (Outer.inner names inner declared
// in Outer); a Hawkeye template's are its blocks and snippets. A file nib has no grammar for, or a declaration that did not parse cleanly,
// is refused rather than guessed at.
func symbolRanges(display string, lines []string, symbol string) ([]lineRange, error) {
	names := strings.Split(symbol, ".")
	if strings.EqualFold(filepath.Ext(display), ".hwk") {
		if len(names) > 1 {
			return nil, fmt.Errorf("%s: a template's blocks and snippets are named alone, like %s:%s", display, display, names[len(names)-1])
		}
		return templateRanges(display, lines, symbol)
	}
	found, err := syntax.Find(display, []byte(strings.Join(lines, "\n")), symbol)
	if errors.Is(err, syntax.ErrNoGrammar) {
		return nil, fmt.Errorf("%s: nib reads declarations by parsing and has no grammar for %s files; grep %s instead", display, filepath.Ext(display), names[len(names)-1])
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %v; grep %s instead", display, err, names[len(names)-1])
	}
	ranges := make([]lineRange, len(found))
	for i, r := range found {
		ranges[i] = lineRange{r.Start, r.End}
	}
	return ranges, nil
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
