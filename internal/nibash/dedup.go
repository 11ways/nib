package nibash

import (
	"slices"
	"strconv"
	"strings"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
)

// nameMinChars is how long a line must be before a name for it pays: a short line costs about what its name does.
const nameMinChars = 24

// varies marks a legend entry whose occurrences differ in their digits.
const varies = "[numbers vary] "

// dedupe is a log as a model reads it: each line that occurs more than once, and each block engine.Repeats finds recurring, defined once in
// the legend, and every occurrence left in place under its log line numbers, back-to-back runs as one entry. Runs compare by
// engine.MaskedKeys, so a run of lines that differ only in timestamps, ids or durations is one entry under the first line's text; a
// line or block elsewhere is named only where it is the same text, so a copy with other numbers is spelled out and no number is lost.
func dedupe(lines []string) ([]decider.Entry, decider.Legend) {
	plain := make([]engine.PlainLine, len(lines))
	keys := engine.MaskedKeys(lines)
	count := map[string]int{}
	for i, line := range lines {
		plain[i] = engine.PlainLine{At: i + 1, Text: line}
		count[line]++
	}
	steps := engine.RepeatsBy(plain, keys)
	legend := decider.Legend{}
	names := map[string]string{}
	numbered := map[string]int{}
	// name is the legend name of a line (@L) or block (@B) of text, or with vary of every run whose copies differ only in numbers masked
	// alike, under the first copy's text.
	name := func(prefix, text string, vary bool, key string) string {
		if !vary {
			key = text
		} else {
			key = "\x00" + key
		}
		if n, ok := names[key]; ok {
			return n
		}
		numbered[prefix]++
		n := prefix + strconv.Itoa(numbered[prefix])
		names[key] = n
		if vary {
			text = varies + text
		}
		legend[n] = text
		return n
	}
	joined := func(start, length int) string { return strings.Join(lines[start:start+length], "\n") }
	// An original a block recurs from is named too where its lines were shown one by one.
	originals := map[int]int{}
	for _, s := range steps {
		if s.Kind == engine.RepeatEcho && slices.Equal(lines[s.Of:s.Of+s.Length], lines[s.Index:s.Index+s.Length]) {
			originals[s.Of] = max(originals[s.Of], s.Length)
		}
	}
	lineAt := map[int]int{}
	for i, s := range steps {
		if s.Kind == engine.RepeatLine {
			lineAt[s.Index] = i
		}
	}
	// A line is named only where that saves enough: a short line costs about what its name does.
	single := func(index int) decider.Entry {
		text := lines[index]
		if count[text] > 1 && len([]rune(text)) >= nameMinChars {
			text = name("@L", text, false, "")
		}
		return decider.Entry{First: index + 1, Last: index + 1, Text: text}
	}
	var entries []decider.Entry
	spell := func(start, length int) {
		for offset := range length {
			entries = append(entries, single(start+offset))
		}
	}
	for i := 0; i < len(steps); i++ {
		s := steps[i]
		switch s.Kind {
		case engine.RepeatLine:
			if length, ok := originals[s.Index]; ok && shownOneByOne(steps, lineAt, s.Index, length) {
				text := joined(s.Index, length)
				entries = append(entries, decider.Entry{First: s.Index + 1, Last: s.Index + length, Text: name("@B", text, false, "")})
				i += length - 1
				continue
			}
			entries = append(entries, single(s.Index))
		case engine.RepeatRun:
			if !s.Exact && !copyLong(lines, s.Index, s.Period) {
				spell(s.Index, s.Period*s.Copies)
				continue
			}
			prefix := "@L"
			if s.Period > 1 {
				prefix = "@B"
			}
			n := name(prefix, joined(s.Index, s.Period), !s.Exact, strings.Join(keys[s.Index:s.Index+s.Period], "\n"))
			entries = append(entries, decider.Entry{First: s.Index + 1, Last: s.Index + s.Period*s.Copies, Text: n + " x" + strconv.Itoa(s.Copies)})
		case engine.RepeatEcho:
			if !slices.Equal(lines[s.Of:s.Of+s.Length], lines[s.Index:s.Index+s.Length]) {
				spell(s.Index, s.Length)
				continue
			}
			text := joined(s.Index, s.Length)
			entries = append(entries, decider.Entry{First: s.Index + 1, Last: s.Index + s.Length, Text: name("@B", text, false, "")})
		}
	}
	return entries, legend
}

// copyLong says whether the length lines from start average nameMinChars, long enough that copies differing in their digits may share a name.
func copyLong(lines []string, start, length int) bool {
	chars := 0
	for _, line := range lines[start : start+length] {
		chars += len([]rune(line))
	}
	return chars >= nameMinChars*length
}

// shownOneByOne says whether the length lines from index are consecutive steps that each show one line.
func shownOneByOne(steps []engine.Repeat, lineAt map[int]int, index, length int) bool {
	at, ok := lineAt[index]
	if !ok || at+length > len(steps) {
		return false
	}
	for offset := range length {
		s := steps[at+offset]
		if s.Kind != engine.RepeatLine || s.Index != index+offset {
			return false
		}
	}
	return true
}
