// Package patch parses nib's hashline patch language.
package patch

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/snapshots"
)

// Section is one file of a patch: an edit of an existing file (Create false) or a new file.
// Tag is nil for a tagless [PATH] section, which may only append; Move and Copy write the result to another path.
type Section struct {
	Create  bool
	Path    string
	Tag     *string
	Edits   []snapshots.LineEdit
	Remove  bool
	Move    string
	Copy    string
	Content string
	Line    int
}

var (
	header       = regexp.MustCompile(`^\[(.+)#([0-9A-Fa-f]{4})\]$`)
	tagless      = regexp.MustCompile(`^\[(.+)\]$`)
	add          = regexp.MustCompile(`^\*\*\* Add File: (.+)$`)
	rangePut     = regexp.MustCompile(`^PUT ([1-9][0-9]*)\.=([1-9][0-9]*):$`)
	singlePut    = regexp.MustCompile(`^PUT ([1-9][0-9]*):$`)
	colonPut     = regexp.MustCompile(`^PUT ([1-9][0-9]*):=([1-9][0-9]*):$`)
	gapPut       = regexp.MustCompile(`^PUT ([<>])([1-9][0-9]*):$`)
	tailPut      = regexp.MustCompile(`^PUT >\$:$`)
	cut          = regexp.MustCompile(`^CUT ([1-9][0-9]*)(?:\.=([1-9][0-9]*))?$`)
	blockPut     = regexp.MustCompile(`^PUT (>?)([1-9][0-9]*)\*:$`)
	blockCut     = regexp.MustCompile(`^CUT ([1-9][0-9]*)\*$`)
	sub          = regexp.MustCompile(`(?s)^SUB ([1-9][0-9]*): (.+)$`)
	move         = regexp.MustCompile(`^MV (.+)$`)
	copyLine     = regexp.MustCompile(`^CP (.+)$`)
	tagLike      = regexp.MustCompile(`#[0-9A-Za-z]*$`)
	contaminated = []struct {
		pattern *regexp.Regexp
		hint    string
	}{
		{regexp.MustCompile(`^[-+ ]?@@`), "unified-diff hunk headers are not hashline; use PUT/CUT with line numbers"},
		{regexp.MustCompile(`^\*\*\* (Update|Delete) File:`), "apply_patch file headers are not hashline; use a [PATH#TAG] section (REM deletes)"},
		{regexp.MustCompile(`^-`), "- rows only go under a PUT N.=M: or CUT N.=M line; PUT replaces the range with + rows, CUT deletes"},
		{regexp.MustCompile(`^\d+:`), "LINE:TEXT rows are read output; body rows are + followed by the new text"},
		{regexp.MustCompile(`^\d+~`), "N~TEXT rows are row-map output; anchor with PUT or CUT, and check a line with a ~TEXT row under that line"},
		{regexp.MustCompile(`^~`), "~ check rows go right after a PUT or CUT line, before its + rows"},
		{regexp.MustCompile(`^SUB [1-9][0-9]*:\s*$`), "SUB N: is followed on the same line by the exact old text"},
		{regexp.MustCompile(`^PUT ([1-9][0-9]*)(-|\.\.)([1-9][0-9]*):$`), "ranges are written N.=M"},
		{regexp.MustCompile(`^PUT ([1-9][0-9]*(\.=[1-9][0-9]*)?|[<>][1-9][0-9]*|>?[1-9][0-9]*\*|>\$):\s*\S`), "a PUT line ends at its colon, with nothing after it, not even the old text; the new lines go on + rows below it"},
	}
)

func hintFor(line string) (string, bool) {
	for _, c := range contaminated {
		if c.pattern.MatchString(line) {
			return c.hint, true
		}
	}
	return "", false
}

// position orders edits: at a shared gap, PUT >N: content lands before PUT <N+1: content; a tail append always sorts last.
func position(edit snapshots.LineEdit) float64 {
	switch {
	case edit.Edge:
		return 1e300
	case edit.Operation == snapshots.Before:
		return float64(edit.Start) - 0.4
	case edit.Operation == snapshots.After:
		return float64(edit.Start) + 0.4
	}
	return float64(edit.Start)
}

func unquote(value string) string {
	trimmed := js.Trim(value)
	if len(trimmed) > 1 && (trimmed[0] == '"' || trimmed[0] == '\'') && trimmed[len(trimmed)-1] == trimmed[0] {
		return trimmed[1 : len(trimmed)-1]
	}
	return trimmed
}

type lineError struct{ message string }

func (e lineError) Error() string { return e.message }

// Parse splits a patch into per-file sections with edits sorted by original position.
// Sections repeating a path and tag are merged; a missing Begin/End envelope and blank lines between hunks are tolerated.
// The error names every offending patch line, not just the first.
func Parse(input string) ([]*Section, error) {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	first, last := 0, len(lines)
	for first < last && js.Trim(lines[first]) == "" {
		first++
	}
	for last > first && js.Trim(lines[last-1]) == "" {
		last--
	}
	if first < len(lines) && js.Trim(lines[first]) == "*** Begin Patch" {
		first++
	}
	if last > first && js.Trim(lines[last-1]) == "*** End Patch" {
		last--
	}
	fail := func(index int, message string) {
		text := ""
		if index < len(lines) {
			text = lines[index]
		}
		panic(lineError{fmt.Sprintf("Patch line %d: %s; got %s", index+1, message, js.Quote(text))})
	}
	var problems []string
	failed := map[*Section]bool{}
	var sections []*Section
	var current *Section
	index := first
	rows := func(prefix string) []string {
		var found []string
		for index < last && strings.HasPrefix(lines[index], prefix) {
			found = append(found, lines[index][1:])
			index++
		}
		return found
	}
	for index < last {
		line := lines[index]
		if js.Trim(line) == "" {
			index++
			continue
		}
		at := index
		index++
		func() {
			defer func() {
				if r := recover(); r != nil {
					failure, ok := r.(lineError)
					if !ok {
						panic(r)
					}
					problems = append(problems, failure.message)
					if current != nil {
						failed[current] = true
					}
					for index < last && len(lines[index]) > 0 && strings.ContainsRune("-+~", rune(lines[index][0])) {
						index++
					}
				}
			}()
			parseLine(line, at, &current, &sections, rows, fail, &index, last, lines)
		}()
	}
	for _, section := range sections {
		if section.Create || failed[section] {
			continue
		}
		if len(section.Edits) == 0 && !section.Remove && section.Move == "" && section.Copy == "" {
			problems = append(problems, fmt.Sprintf("Section [%s%s] has no PUT, CUT, SUB, REM, MV or CP", section.Path, tagSuffix(section.Tag)))
		}
		slices.SortStableFunc(section.Edits, func(a, b snapshots.LineEdit) int {
			pa, pb := position(a), position(b)
			switch {
			case pa < pb:
				return -1
			case pa > pb:
				return 1
			}
			return 0
		})
	}
	if len(sections) == 0 && len(problems) == 0 {
		problems = append(problems, "Patch contains no sections")
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "\n"))
	}
	return sections, nil
}

func tagSuffix(tag *string) string {
	if tag == nil || *tag == "" {
		return ""
	}
	return "#" + *tag
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func parseLine(line string, at int, current **Section, sections *[]*Section, rows func(string) []string, fail func(int, string), index *int, last int, lines []string) {
	h := header.FindStringSubmatch(line)
	a := add.FindStringSubmatch(line)
	var t []string
	if h == nil && a == nil {
		t = tagless.FindStringSubmatch(line)
	}
	if h != nil || t != nil {
		raw := ""
		var tag *string
		if h != nil {
			raw = h[1]
			upper := strings.ToUpper(h[2])
			tag = &upper
		} else {
			raw = t[1]
		}
		path := unquote(raw)
		var earlier *Section
		for _, s := range *sections {
			if !s.Create && s.Path == path {
				earlier = s
				break
			}
		}
		sameTag := earlier != nil && ((earlier.Tag == nil && tag == nil) || (earlier.Tag != nil && tag != nil && *earlier.Tag == *tag))
		if sameTag && !earlier.Remove && earlier.Move == "" && earlier.Copy == "" {
			*current = earlier
		} else {
			empty := ""
			*current = &Section{Path: path, Tag: &empty, Line: at + 1}
		}
		if t != nil && !(strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, "'")) && tagLike.MatchString(raw) {
			fail(at, "a tag is 4 hex digits, like [PATH#3C4D]")
		}
		if earlier != nil && !sameTag {
			if earlier.Tag != nil && *earlier.Tag != "" {
				fail(at, fmt.Sprintf("%s already has a section with tag %s", path, *earlier.Tag))
			}
			fail(at, fmt.Sprintf("%s already has a section without a tag", path))
		}
		if earlier != nil && (earlier.Remove || earlier.Move != "" || earlier.Copy != "") {
			fail(at, fmt.Sprintf("%s already ends with REM, MV or CP", path))
		}
		if earlier == nil {
			(*current).Tag = tag
			*sections = append(*sections, *current)
		}
		return
	}
	if a != nil {
		*current = &Section{Create: true, Path: unquote(a[1]), Line: at + 1}
		body := rows("+")
		if len(body) > 0 {
			(*current).Content = strings.Join(body, "\n") + "\n"
		}
		*sections = append(*sections, *current)
		return
	}
	if *current == nil {
		empty := ""
		*current = &Section{Path: "", Tag: &empty, Line: at + 1}
		hint, ok := hintFor(line)
		if !ok {
			hint = "expected a [PATH#TAG] header or *** Add File: PATH"
		}
		fail(at, hint)
	}
	if (*current).Create {
		fail(at, "an Add File section takes only + rows")
	}
	section := *current
	if section.Remove || section.Move != "" || section.Copy != "" {
		fail(at, "nothing may follow REM, MV or CP in a section")
	}
	untagged := func(form string) {
		fail(at, fmt.Sprintf("a [PATH] section without a tag may only append with PUT >$:, not %s; copy [PATH#TAG] from your latest read to edit elsewhere", form))
	}
	if js.Trim(line) == "REM" {
		if section.Tag == nil {
			untagged("REM")
		}
		if len(section.Edits) > 0 {
			fail(at, "REM deletes the file and cannot be combined with PUT or CUT")
		}
		section.Remove = true
		return
	}
	mv := move.FindStringSubmatch(line)
	cp := copyLine.FindStringSubmatch(line)
	if mv != nil || cp != nil {
		if section.Tag == nil {
			if mv != nil {
				untagged("MV")
			}
			untagged("CP")
		}
		if mv != nil {
			section.Move = unquote(mv[1])
		} else {
			section.Copy = unquote(cp[1])
		}
		return
	}
	var edit snapshots.LineEdit
	checks := 1
	body := true
	quotes := false
	r := rangePut.FindStringSubmatch(line)
	s := singlePut.FindStringSubmatch(line)
	c := colonPut.FindStringSubmatch(line)
	g := gapPut.FindStringSubmatch(line)
	k := cut.FindStringSubmatch(line)
	b := blockPut.FindStringSubmatch(line)
	bc := blockCut.FindStringSubmatch(line)
	sb := sub.FindStringSubmatch(line)
	isSub := false
	switch {
	case r != nil || s != nil || c != nil:
		switch {
		case r != nil:
			edit = snapshots.LineEdit{Start: atoi(r[1]), End: atoi(r[2]), Operation: snapshots.Replace}
		case c != nil:
			edit = snapshots.LineEdit{Start: atoi(c[1]), End: atoi(c[2]), Operation: snapshots.Replace, Written: line}
		default:
			edit = snapshots.LineEdit{Start: atoi(s[1]), End: atoi(s[1]), Operation: snapshots.Replace}
		}
		if edit.Start < edit.End {
			checks = 2
		}
		quotes = true
	case g != nil:
		op := snapshots.After
		if g[1] == "<" {
			op = snapshots.Before
		}
		edit = snapshots.LineEdit{Start: atoi(g[2]), End: atoi(g[2]), Operation: op}
	case tailPut.MatchString(line):
		edit = snapshots.LineEdit{Start: snapshots.Tail, End: snapshots.Tail, Operation: snapshots.After, Edge: true}
		checks = 0
	case b != nil:
		op := snapshots.Replace
		if b[1] != "" {
			op = snapshots.After
		}
		edit = snapshots.LineEdit{Start: atoi(b[2]), End: atoi(b[2]), Operation: op, Block: true}
	case k != nil:
		end := atoi(k[1])
		if k[2] != "" {
			end = atoi(k[2])
		}
		edit = snapshots.LineEdit{Start: atoi(k[1]), End: end, Operation: snapshots.Replace}
		if edit.Start < edit.End {
			checks = 2
		}
		body = false
		quotes = true
	case bc != nil:
		edit = snapshots.LineEdit{Start: atoi(bc[1]), End: atoi(bc[1]), Operation: snapshots.Replace, Block: true}
		body = false
	case sb != nil:
		old := sb[2]
		edit = snapshots.LineEdit{Start: atoi(sb[1]), End: atoi(sb[1]), Operation: snapshots.Replace, Sub: &old}
		checks = 0
		isSub = true
	default:
		hint, ok := hintFor(line)
		if !ok {
			hint = "expected PUT N.=M:, PUT N:, PUT N*:, PUT <N:, PUT >N:, PUT >N*:, PUT >$:, CUT N.=M, CUT N, CUT N*, SUB N: OLD, REM, MV DEST, CP DEST, or a new section header"
		}
		fail(at, hint)
	}
	if section.Tag == nil && !edit.Edge {
		untagged(strings.SplitN(line, " ", 2)[0])
	}
	found := rows("~")
	if len(found) > checks {
		if checks == 0 {
			fail(at, "this hunk takes no ~ check rows")
		}
		plural := ""
		if checks > 1 {
			plural = "s"
		}
		fail(at, fmt.Sprintf("this hunk takes at most %d ~ check row%s", checks, plural))
	}
	var content, old []string
	for *index < last && (strings.HasPrefix(lines[*index], "-") || (body && strings.HasPrefix(lines[*index], "+"))) {
		row := lines[*index]
		*index++
		if strings.HasPrefix(row, "+") {
			content = append(content, row[1:])
		} else {
			old = append(old, row[1:])
		}
	}
	if len(old) > 0 && !quotes {
		fail(at, "- rows only go under PUT N.=M:, PUT N:, CUT N.=M or CUT N, where they must match the lines being replaced")
	}
	if body && len(content) == 0 {
		if isSub {
			fail(at, "SUB needs + rows with the new text (a lone + deletes the old text)")
		}
		fail(at, "PUT needs at least one + row (use CUT to delete lines)")
	}
	if content == nil {
		content = []string{}
	}
	edit.Content = content
	if len(found) > 0 {
		edit.Checks = found
		edit.HasChecks = true
	}
	if len(old) > 0 {
		edit.Old = old
	}
	section.Edits = append(section.Edits, edit)
}
