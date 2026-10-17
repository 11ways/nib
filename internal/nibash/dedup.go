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
// the legend, and every occurrence left in place under its log line numbers, back-to-back runs as one entry; lines compare with their
// digits masked, so the first text stands for variants that differ only in numbers.
func dedupe(lines []string) ([]decider.Entry, decider.Legend) {
	plain := make([]engine.PlainLine, len(lines))
	keys := make([]string, len(lines))
	count := map[string]int{}
	first := map[string]string{}
	differs := map[string]bool{}
	for i, line := range lines {
		plain[i] = engine.PlainLine{At: i + 1, Text: line}
		keys[i] = engine.MaskDigits(line)
		count[keys[i]]++
		if text, ok := first[keys[i]]; !ok {
			first[keys[i]] = line
		} else if text != line {
			differs[keys[i]] = true
		}
	}
	steps := engine.Repeats(plain, true)
	legend := decider.Legend{}
	lineNames := map[string]string{}
	blockNames := map[string]string{}
	lineName := func(index int) string {
		key := keys[index]
		if name, ok := lineNames[key]; ok {
			return name
		}
		name := "@L" + strconv.Itoa(len(lineNames)+1)
		lineNames[key] = name
		text := first[key]
		if differs[key] {
			text = varies + text
		}
		legend[name] = text
		return name
	}
	blockTexts := map[string]string{}
	// blockName names the block of length lines from start; a copy that differs in its digits keeps the digits of the lines that differ.
	blockName := func(start, length int) string {
		key := strings.Join(keys[start:start+length], "\n")
		text := strings.Join(lines[start:start+length], "\n")
		if name, ok := blockNames[key]; ok {
			if blockTexts[key] == text {
				return name
			}
			if !strings.HasPrefix(legend[name], varies) {
				legend[name] = varies + legend[name]
			}
			var numbers []string
			for offset, line := range strings.Split(blockTexts[key], "\n") {
				if lines[start+offset] != line {
					numbers = append(numbers, engine.Numbers(lines[start+offset])...)
				}
			}
			return name + " " + strings.Join(numbers, " ")
		}
		name := "@B" + strconv.Itoa(len(blockNames)+1)
		blockNames[key] = name
		blockTexts[key] = text
		legend[name] = text
		return name
	}
	// An original a block recurs from is named too where its lines were shown one by one.
	named := func(s engine.Repeat) bool {
		return slices.Equal(lines[s.Of:s.Of+s.Length], lines[s.Index:s.Index+s.Length]) || copyLong(lines, s.Index, s.Length)
	}
	originals := map[int]int{}
	for _, s := range steps {
		if s.Kind == engine.RepeatEcho && named(s) {
			originals[s.Of] = max(originals[s.Of], s.Length)
		}
	}
	lineAt := map[int]int{}
	for i, s := range steps {
		if s.Kind == engine.RepeatLine {
			lineAt[s.Index] = i
		}
	}
	// A line is named only where that saves enough: a variant that differs in its digits keeps them after the name, so no number is lost.
	single := func(index int) decider.Entry {
		text := lines[index]
		if count[keys[index]] > 1 && len([]rune(text)) >= nameMinChars {
			name := lineName(index)
			text = name
			if lines[index] != first[keys[index]] {
				text = name + " " + strings.Join(engine.Numbers(lines[index]), " ")
			}
		}
		return decider.Entry{First: index + 1, Last: index + 1, Text: text}
	}
	long := func(start, length int) bool { return copyLong(lines, start, length) }
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
				entries = append(entries, decider.Entry{First: s.Index + 1, Last: s.Index + length, Text: blockName(s.Index, length)})
				i += length - 1
				continue
			}
			entries = append(entries, single(s.Index))
		case engine.RepeatRun:
			if !s.Exact && !long(s.Index, s.Period) {
				spell(s.Index, s.Period*s.Copies)
				continue
			}
			name := lineName(s.Index)
			if s.Period > 1 {
				name = blockName(s.Index, s.Period)
				if !s.Exact && !strings.HasPrefix(legend[name], varies) {
					legend[name] = varies + legend[name]
				}
			}
			entries = append(entries, decider.Entry{First: s.Index + 1, Last: s.Index + s.Period*s.Copies, Text: name + " x" + strconv.Itoa(s.Copies)})
		case engine.RepeatEcho:
			if !named(s) {
				spell(s.Index, s.Length)
				continue
			}
			entries = append(entries, decider.Entry{First: s.Index + 1, Last: s.Index + s.Length, Text: blockName(s.Index, s.Length)})
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
