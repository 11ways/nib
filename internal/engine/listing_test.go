package engine

import (
	"fmt"
	"strings"
	"testing"
)

func TestListingsNameEachFolderOnce(t *testing.T) {
	root := "/home/skerit/projects/nib/"
	paths := []string{"README.md", "bench/run.ts", "bench/runners.ts", "docs/design-decider.md", "internal/decider/config.go", "internal/decider/decider_test.go", "internal/decider/openrouter.go", "internal/decider/testdata/jev-1.14-response.json"}
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("internal/nibash/testdata/captures/decider/%064x.json", i))
	}
	paths = append(paths, "test/bench.test.ts", "test/decider.test.ts")
	var rows []listedRow
	for _, path := range paths {
		rows = append(rows, listedRow{path: root + path})
	}
	under, lines := grouped(rows)
	if under != root {
		t.Fatalf("the folder every row lies in is named once, got %q", under)
	}
	want := []string{"README.md", "bench/", "  run.ts", "  runners.ts", "docs/design-decider.md", "internal/decider/", "  config.go", "  decider_test.go", "  openrouter.go", "internal/decider/testdata/jev-1.14-response.json", "internal/nibash/testdata/captures/decider/ (12)"}
	for i := range 12 {
		want = append(want, fmt.Sprintf("  %064x.json", i))
	}
	want = append(want, "test/bench.test.ts", "test/decider.test.ts")
	if got := strings.Join(lines, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func TestListingsGroupOnlyWhereThatIsShorter(t *testing.T) {
	for _, c := range []struct {
		paths       []string
		under, want string
	}{
		// One row keeps its whole path; rows in no shared folder stay whole; the root alone is never named.
		{[]string{"src/a/b.go"}, "", "src/a/b.go"},
		{[]string{"a.go", "b/c.go"}, "", "a.go\nb/c.go"},
		{[]string{"/etc/a", "/usr/b"}, "", "/etc/a\n/usr/b"},
		// Two files in a short folder cost more grouped.
		{[]string{"x.go", "a/b.go", "a/c.go"}, "", "x.go\na/b.go\na/c.go"},
		// Named directories group like any folder.
		{[]string{"~core/workflow/Repo.java", "~core/workflow/Table.java", "~core/web/App.java"}, "~core/", "workflow/\n  Repo.java\n  Table.java\nweb/App.java"},
	} {
		var rows []listedRow
		for _, path := range c.paths {
			rows = append(rows, listedRow{before: "", path: path, after: ""})
		}
		under, lines := grouped(rows)
		if under != c.under || strings.Join(lines, "\n") != c.want {
			t.Errorf("%v: got %q and\n%s", c.paths, under, strings.Join(lines, "\n"))
		}
	}
	// Text around a path stays with it, indented rows included.
	_, lines := grouped([]listedRow{{"3 ", "src/main/a.go", ""}, {"1 ", "src/main/b.go", ""}, {"", "src/main/c.go", " (2 lines, 9 B, 1h ago)"}})
	if got := strings.Join(lines, "\n"); got != "3 a.go\n1 b.go\nc.go (2 lines, 9 B, 1h ago)" {
		t.Fatalf("got:\n%s", got)
	}
}
