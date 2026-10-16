package nibash

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// TestCorpus measures nibash against real commands agents ran, one JSON string per line in NIB_CORPUS (research/bash-usage.ts --dump writes
// it): how many parse, and how many have commands nibash runs as its own. It is a measurement, not a gate, and skips without the file.
func TestCorpus(t *testing.T) {
	path := os.Getenv("NIB_CORPUS")
	if path == "" {
		t.Skip("set NIB_CORPUS to a file of commands")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	total, parsed, withNib := 0, 0, 0
	byCommand := map[string]int{}
	errors := map[string]int{}
	for scanner.Scan() {
		var command string
		if json.Unmarshal(scanner.Bytes(), &command) != nil {
			continue
		}
		total++
		file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
		if err != nil {
			message := err.Error()
			if at := strings.LastIndex(message, ": "); at >= 0 {
				message = message[at+2:]
			}
			errors[message]++
			continue
		}
		parsed++
		classify(file, 0)
		found := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if lit, ok := node.(*syntax.Lit); ok {
				if rest, ok := strings.CutPrefix(lit.Value, marker); ok {
					// The job start an & becomes is nibash's own plumbing, not one of its commands.
					if name := rest[strings.Index(rest, ":")+1:]; name != internalJob {
						found = true
						byCommand[name]++
					}
				}
			}
			return true
		})
		if found {
			withNib++
		}
	}
	percent := func(n int) float64 { return 100 * float64(n) / float64(max(total, 1)) }
	t.Logf("%d commands: %d parse (%.1f%%), %d run at least one nib command (%.1f%%)", total, parsed, percent(parsed), withNib, percent(withNib))
	type count struct {
		name string
		n    int
	}
	sorted := func(m map[string]int, limit int) []count {
		var list []count
		for name, n := range m {
			list = append(list, count{name, n})
		}
		sort.Slice(list, func(a, b int) bool { return list[a].n > list[b].n })
		return list[:min(limit, len(list))]
	}
	for _, c := range sorted(byCommand, 12) {
		t.Logf("  nib %s: %d", c.name, c.n)
	}
	for _, c := range sorted(errors, 8) {
		t.Logf("  parse error %q: %d", c.name, c.n)
	}
}
