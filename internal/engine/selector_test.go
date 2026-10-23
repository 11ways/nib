package engine

import (
	"strings"
	"testing"
)

func TestContextSelectorJoinsRangesPerFileAndCapsThem(t *testing.T) {
	a, b := &grepFile{target: &target{display: "a.ts"}}, &grepFile{target: &target{display: "b.ts"}}
	around := func(_ *grepFile, line int) []int { return []int{line + 1, line + 2} }
	// Neighbouring matches join one range, a lone line is named alone, and each file gets one selector.
	got := contextSelector([]matchRef{{a, 1}, {a, 4}, {a, 10}, {b, 7}}, func(f *grepFile, line int) []int {
		if line == 10 {
			return nil
		}
		return around(f, line)
	})
	if want := "a.ts:1-6,10 and b.ts:7-9"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Past selectorRanges ranges, the selector covers the first matches and says how many.
	var many []matchRef
	for i := range selectorRanges + 5 {
		many = append(many, matchRef{a, 1 + i*10})
	}
	got = contextSelector(many, around)
	if !strings.HasPrefix(got, "a.ts:1-3,") || !strings.HasSuffix(got, ",191-193 for the next 20 of 25") {
		t.Fatalf("got %q", got)
	}
}
