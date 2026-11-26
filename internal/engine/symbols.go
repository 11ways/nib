package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/syntax"
)

// symbolPart is one name of a symbol selector; a name never starts with a digit, so it never reads as a line range.
const symbolPart = `[A-Za-z_$][\w$]*`

// templateTag matches a Hawkeye tag that opens or closes a named block or snippet: {% block "name" %}, {% snippet name(...) %}, {% /block %}.
var templateTag = regexp.MustCompile(`\{%-?\s*(/?)(block|snippet)\b\s*(?:"([^"]*)"|(` + symbolPart + `))?`)

// symbolRanges are the lines of each declaration of symbol in lines, as the file's syntax tree has them (Outer.inner names inner declared
// in Outer); a Hawkeye template's are its blocks and snippets, and a Markdown heading's its section. A file nib has no grammar for, or a
// declaration that did not parse cleanly, is refused rather than guessed at.
func symbolRanges(display string, lines []string, symbol string) ([]lineRange, error) {
	if strings.HasPrefix(symbol, "#") {
		return headingRanges(display, lines, symbol)
	}
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

// headingsNamed caps the headings a miss lists.
const headingsNamed = 12

// headingRanges are the sections of the Markdown headings wanted names, each from its heading to the line before the next heading of its
// level or a higher one: the headings that read exactly wanted, whatever the case, else those that begin with it.
func headingRanges(display string, lines []string, wanted string) ([]lineRange, error) {
	level := func(line int) int { return len(lines[line-1]) - len(strings.TrimLeft(lines[line-1], "#")) }
	all := headings(lines)
	var exact, begins []int
	for at, line := range all {
		text := strings.ToLower(js.Trim(lines[line-1]))
		if text == strings.ToLower(wanted) {
			exact = append(exact, at)
		} else if strings.HasPrefix(text, strings.ToLower(wanted)) {
			begins = append(begins, at)
		}
	}
	if len(exact) == 0 {
		exact = begins
	}
	if len(exact) == 0 {
		var same []string
		for _, line := range all {
			if level(line) == len(wanted)-len(strings.TrimLeft(wanted, "#")) && len(same) < headingsNamed {
				same = append(same, js.Trim(lines[line-1]))
			}
		}
		if len(same) == 0 {
			return nil, fmt.Errorf("%s has no heading that begins with %q, and none of that level; read %s for its outline", display, wanted, display)
		}
		return nil, fmt.Errorf("%s has no heading that begins with %q; of that level it has: %s", display, wanted, strings.Join(same, " | "))
	}
	var ranges []lineRange
	for _, at := range exact {
		end := len(lines)
		for _, next := range all[at+1:] {
			if level(next) <= level(all[at]) {
				end = next - 1
				break
			}
		}
		ranges = append(ranges, lineRange{all[at], end})
	}
	return ranges, nil
}
