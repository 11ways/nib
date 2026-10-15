package nibash

import (
	"errors"
	"regexp"
	"strings"
)

// dialect is how grep reads a pattern: basic (-G), extended (-E) or fixed strings (-F).
type dialect int

const (
	basic dialect = iota
	extended
	fixed
)

// errUntranslatable marks a pattern feature RE2 cannot run exactly, such as a backreference; the command then runs as the real program.
var errUntranslatable = errors.New("pattern needs the system grep")

// translate turns a GNU grep pattern into RE2 syntax. Features without an exact RE2 meaning (backreferences, \< and \>, \` and \') refuse.
func translate(pattern string, d dialect) (string, error) {
	if d == fixed {
		return regexp.QuoteMeta(pattern), nil
	}
	var out strings.Builder
	inClass := false
	classStart := 0
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inClass {
			// Inside a bracket expression backslash is literal in POSIX, and ] right after [ or [^ is a member.
			switch {
			case r == '\\':
				out.WriteString(`\\`)
			case r == '[' && i+1 < len(runes) && (runes[i+1] == ':' || runes[i+1] == '=' || runes[i+1] == '.'):
				end := strings.Index(string(runes[i+2:]), string(runes[i+1])+"]")
				if end < 0 {
					return "", errUntranslatable
				}
				if runes[i+1] != ':' {
					return "", errUntranslatable
				}
				name := string(runes[i : i+2+end+2])
				out.WriteString(name)
				i += 1 + end + 2
			case r == ']' && i > classStart:
				out.WriteRune(r)
				inClass = false
			default:
				out.WriteRune(r)
			}
			continue
		}
		switch r {
		case '[':
			inClass = true
			out.WriteRune(r)
			classStart = i + 1
			if i+1 < len(runes) && runes[i+1] == '^' {
				out.WriteRune('^')
				i++
				classStart = i + 1
			}
			// A ] right after the opening counts as a member.
			if i+1 < len(runes) && runes[i+1] == ']' {
				out.WriteString(`\]`)
				i++
			}
		case '\\':
			if i+1 >= len(runes) {
				return "", errUntranslatable
			}
			i++
			next := runes[i]
			switch {
			case next >= '1' && next <= '9':
				return "", errUntranslatable
			case strings.ContainsRune("<>`'", next):
				return "", errUntranslatable
			case d == basic && strings.ContainsRune("|(){}+?", next):
				// In basic syntax these are operators only when escaped.
				out.WriteRune(next)
			case strings.ContainsRune("wWsSbB", next):
				out.WriteRune('\\')
				out.WriteRune(next)
			case strings.ContainsRune(`.[]*^$\/`, next) || (d == extended && strings.ContainsRune("|(){}+?", next)):
				out.WriteRune('\\')
				out.WriteRune(next)
			default:
				// GNU grep reads other escaped characters as themselves.
				out.WriteString(regexp.QuoteMeta(string(next)))
			}
		default:
			if d == basic && strings.ContainsRune("|(){}+?", r) {
				// Unescaped, these are ordinary characters in basic syntax.
				out.WriteString(regexp.QuoteMeta(string(r)))
				continue
			}
			if d == basic && r == '^' && !(i == 0 || (i >= 2 && runes[i-2] == '\\' && (runes[i-1] == '(' || runes[i-1] == '|'))) {
				// In basic syntax ^ anchors only at the start of the pattern or of a group or branch.
				out.WriteString(`\^`)
				continue
			}
			if d == basic && r == '$' && !(i == len(runes)-1 || (i+2 < len(runes) && runes[i+1] == '\\' && (runes[i+2] == ')' || runes[i+2] == '|'))) {
				out.WriteString(`\$`)
				continue
			}
			if r == '*' && (i == 0 || (d == basic && runes[i-1] == '^' && i == 1)) {
				// A leading * is a literal star in basic syntax.
				out.WriteString(`\*`)
				continue
			}
			out.WriteRune(r)
		}
	}
	if inClass {
		return "", errUntranslatable
	}
	return out.String(), nil
}

// matcher matches one line the way grep's options ask: any of its patterns, case-insensitively, as a word or as the whole line.
type matcher struct {
	re    *regexp.Regexp
	word  bool
	empty bool
}

func newMatcher(patterns []string, d dialect, ignoreCase, word, line bool) (*matcher, error) {
	var parts []string
	empty := false
	for _, pattern := range patterns {
		if pattern == "" {
			empty = true
		}
		translated, err := translate(pattern, d)
		if err != nil {
			return nil, err
		}
		parts = append(parts, "(?:"+translated+")")
	}
	expression := strings.Join(parts, "|")
	if line {
		expression = "^(?:" + expression + ")$"
	}
	if ignoreCase {
		expression = "(?i)" + expression
	}
	re, err := regexp.Compile(expression)
	if err != nil {
		return nil, errUntranslatable
	}
	return &matcher{re: re, word: word, empty: empty && !word && !line}, nil
}

func wordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// matches are the spans of a line's matches; with word, only those with a non-word character or the line's edge on both sides.
func (m *matcher) matches(text string) [][]int {
	found := m.re.FindAllStringIndex(text, -1)
	if !m.word {
		return found
	}
	var kept [][]int
	for _, span := range found {
		before := span[0] == 0 || !wordByte(text[span[0]-1])
		after := span[1] == len(text) || !wordByte(text[span[1]])
		if before && after && span[1] > span[0] {
			kept = append(kept, span)
		}
	}
	return kept
}

func (m *matcher) match(text string) bool {
	if !m.word {
		return m.re.MatchString(text)
	}
	return len(m.matches(text)) > 0
}
