package engine

import (
	"strings"
	"testing"
)

func TestJSONProblemNamesTheFirstFlawAndItsLine(t *testing.T) {
	for text, want := range map[string]string{
		`{"a": 1, "b": {"a": 2}, "c": [{"a": 3}, {"a": 4}]}`: "",
		"[1, 2]": "",
		"{\n  \"save\": \"Save\",\n  \"open\": \"Open\",\n  \"save\": \"Store\"\n}": `line 4: key "save" repeats in its object (first on line 2)`,
		"{\n  \"a\": {\"x\": 1, \"x\": 2}\n}":                                       `line 2: key "x" repeats in its object (first on line 2)`,
		"{\n  \"a\": 1,\n}":                                                         "line 3: invalid character '}' looking for beginning of object key string",
		"{}\n{}":                                                                    "line 2: a second value follows the first",
	} {
		if got := jsonProblem(text); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
	// Only a .json file that was sound, or is new, gets a warning.
	ctx := &Context{Session: "s"}
	store := New().Store
	if got := dataWarnings(ctx, store, "en.json", []string{`{"a": 1}`}, []string{`{"a": 1, "a": 2}`}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got := dataWarnings(ctx, store, "tsconfig.json", []string{"// comment", "{}"}, []string{"// comment", "{,}"}); got != nil {
		t.Fatalf("a file that never parsed must be left alone, got %v", got)
	}
	if got := dataWarnings(ctx, store, "en.txt", nil, []string{"{,}"}); got != nil {
		t.Fatalf("only data files are checked, got %v", got)
	}
}

func TestYAMLAndTOMLAreCheckedByTheirGrammars(t *testing.T) {
	ctx := &Context{Session: "s"}
	store := New().Store
	if got := dataWarnings(ctx, store, "ci.yml", []string{"a:", "  - 1"}, []string{"a:", "  - 1", "b: [c"}); len(got) != 1 || got[0] != "The file is not valid YAML now: line 3 holds its first error." {
		t.Fatalf("got %v", got)
	}
	if got := dataWarnings(ctx, store, "Cargo.toml", nil, []string{"[package]", `name = "x`}); len(got) != 1 || got[0] != "The file is not valid TOML now: line 2 holds its first error." {
		t.Fatalf("got %v", got)
	}
	if got := dataWarnings(ctx, store, "Cargo.toml", []string{"[package]"}, []string{"[package]", `name = "x"`}); got != nil {
		t.Fatalf("sound TOML, got %v", got)
	}
	// A file too big to parse on every change says so once.
	big := []string{"a: " + strings.Repeat("x", checkBytes)}
	if got := dataWarnings(ctx, store, "big.yaml", big, big); len(got) != 1 || !strings.Contains(got[0], "get no YAML check") {
		t.Fatalf("got %v", got)
	}
	if got := dataWarnings(ctx, store, "big.yaml", big, big); got != nil {
		t.Fatalf("said twice: %v", got)
	}
}
