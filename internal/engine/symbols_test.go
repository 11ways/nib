package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSymbolsFindTheirDeclarationsWithTheirBlocks(t *testing.T) {
	java := strings.Split(`package be.example;

public class Repo {
    private final Store store;

    /**
     * Saves one record.
     */
    @Override
    public void save(Record r) {
        store.save(r);
    }

    void save(Record r, boolean flush) {
        save(r);
        if (flush) {
            store.flush();
        }
    }

    static class Cache {
        void save(Record r) {
            hold(r);
        }
    }
}`, "\n")
	golang := strings.Split(`package x

func (r *Repo) Save(v int) error {
	return r.store.Save(v)
}

const Limit = 10

func helper() {
	Save := 1
	_ = Save
}`, "\n")
	ts := strings.Split(`export function formatAge(time: number) {
  const seconds = (Date.now() - time) / 1000
  return seconds
}

const UNITS = { k: 1024 }

describe("x", () => {
  test("y", () => {})
})`, "\n")
	python := strings.Split(`class Repo:
    def save(self, r):
        self.store.save(r)

    def load(self):
        return save(1)`, "\n")
	for _, c := range []struct {
		name   string
		lines  []string
		symbol string
		want   string
	}{
		// Overloads at the least depth all count, each with its doc comment and annotation; the nested class's save is deeper.
		{"Repo.java", java, "save", "6-12 14-19"},
		{"Repo.java", java, "Repo.save", "6-12 14-19"},
		{"Repo.java", java, "Repo.Cache.save", "22-24"},
		{"Repo.java", java, "Cache", "21-25"},
		// A Go receiver's type is not the name declared; a one-line declaration is its line; a local variable is deeper than the method.
		{"x.go", golang, "Save", "3-5"},
		{"x.go", golang, "Limit", "7-7"},
		{"x.ts", ts, "formatAge", "1-4"},
		{"x.ts", ts, "UNITS", "6-6"},
		{"x.py", python, "Repo.save", "2-3"},
		{"x.py", python, "load", "5-6"},
	} {
		found, err := symbolRanges(c.name, c.lines, c.symbol)
		if err != nil {
			t.Errorf("%s:%s: %v", c.name, c.symbol, err)
			continue
		}
		var got []string
		for _, r := range found {
			got = append(got, fmt.Sprintf("%d-%d", r.start, r.end))
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s:%s: got %v, want %s", c.name, c.symbol, got, c.want)
		}
	}
	for symbol, want := range map[string]string{
		"store":      "",
		"Repo.flush": "Repo.java: nib found no declaration of Repo.flush; grep flush instead",
		"missing":    "Repo.java: nib found no declaration of missing; grep missing instead",
	} {
		_, err := symbolRanges("Repo.java", java, symbol)
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v", symbol, err)
			}
			continue
		}
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", symbol, err, want)
		}
	}
}

func TestACallIsNotADeclaration(t *testing.T) {
	lines := strings.Split("describe(\"x\", () => {\n  test(\"y\", () => {})\n})\n", "\n")
	if _, err := symbolRanges("x.ts", lines, "describe"); err == nil || !strings.Contains(err.Error(), "no declaration of describe") {
		t.Fatalf("got %v", err)
	}
	if _, err := symbolRanges("notes.txt", lines, "describe"); err == nil || !strings.Contains(err.Error(), "has no grammar for .txt files") {
		t.Fatalf("got %v", err)
	}
}

func TestAMarkdownHeadingReadsItsSection(t *testing.T) {
	text := "# Report\nintro\n## Fix round\none\n### Detail\ntwo\n```\n## not a heading\n```\n## Fix round 2\nthree\n# Other\nfour\n"
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for wanted, want := range map[string][]lineRange{
		"## Fix round":   {{3, 9}},
		"## fix round 2": {{10, 11}},
		"## Fix":         {{3, 9}, {10, 11}},
		"### Detail":     {{5, 9}},
		"# Other":        {{12, 13}},
		"# Report":       {{1, 11}},
	} {
		if got, err := symbolRanges("r.md", lines, wanted); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: got %v %v, want %v", wanted, got, err, want)
		}
	}
	if _, err := symbolRanges("r.md", lines, "## Missing"); err == nil || !strings.Contains(err.Error(), `no heading that begins with "## Missing"; of that level it has: ## Fix round | ## Fix round 2`) {
		t.Fatalf("got %v", err)
	}
	// Through a read: the section's rows, and a path that exists under the whole name wins.
	e, ctx, path := showFixture(t, "x\n")
	report := filepath.Join(filepath.Dir(path), "r.md")
	if err := os.WriteFile(report, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := readText(t, e, ctx, report+":## Fix round 2"); !strings.Contains(out, "]\n10:## Fix round 2\n11:three\n") || strings.Contains(out, "12:") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestTemplateSymbolsAreBlocksAndSnippets(t *testing.T) {
	lines := strings.Split(`{% extend "layout" %}
{% block "main" mode=reuse %}
  {% if open %}
    {% block "inner" %}x{% /block %}
  {% /if %}
{% /block %}
{% snippet row(item: Item) %}
  <li>{{ item.name }}</li>
{% /snippet %}`, "\n")
	for symbol, want := range map[string]string{"main": "2-6", "inner": "4-4", "row": "7-9"} {
		found, err := symbolRanges("page.hwk", lines, symbol)
		if err != nil || len(found) != 1 || fmt.Sprintf("%d-%d", found[0].start, found[0].end) != want {
			t.Errorf("%s: got %v %v, want %s", symbol, found, err, want)
		}
	}
	if _, err := symbolRanges("page.hwk", lines, "main.inner"); err == nil {
		t.Error("a template's names stand alone")
	}
	if _, err := symbolRanges("page.hwk", lines, "layout"); err == nil || !strings.Contains(err.Error(), "no block or snippet named layout") {
		t.Errorf("got %v", err)
	}
}
