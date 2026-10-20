package patch

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// shape renders sections the way the TypeScript parser's JSON did, tail edges at -1.
func shape(sections []*Section) []any {
	var out []any
	for _, s := range sections {
		if s.Create {
			out = append(out, map[string]any{"kind": "create", "path": s.Path, "content": s.Content, "line": float64(s.Line)})
			continue
		}
		section := map[string]any{"kind": "edit", "path": s.Path, "line": float64(s.Line)}
		if s.Tag != nil {
			section["tag"] = *s.Tag
		}
		if s.Remove {
			section["remove"] = true
		}
		if s.Move != "" {
			section["move"] = s.Move
		}
		if s.Copy != "" {
			section["copy"] = s.Copy
		}
		edits := []any{}
		for _, e := range s.Edits {
			edit := map[string]any{"start": float64(e.Start), "end": float64(e.End), "operation": [...]string{"replace", "before", "after"}[e.Operation], "content": toAny(e.Content)}
			if e.Edge {
				edit["start"], edit["end"], edit["edge"] = float64(-1), float64(-1), "tail"
			}
			if e.Block {
				edit["block"] = true
			}
			if e.HasChecks {
				edit["checks"] = toAny(e.Checks)
			}
			if e.Sub != nil {
				edit["sub"] = *e.Sub
			}
			if len(e.Old) > 0 {
				edit["old"] = toAny(e.Old)
			}
			if e.Written != "" {
				edit["written"] = e.Written
			}
			if e.Import != "" {
				edit["import"] = e.Import
			}
			edits = append(edits, edit)
		}
		section["edits"] = edits
		out = append(out, section)
	}
	return out
}

func toAny(lines []string) []any {
	out := []any{}
	for _, line := range lines {
		out = append(out, line)
	}
	return out
}

// testdata/corpus.json holds patches with what nib's parser returns for them, or its error message; it began as the TypeScript parser's
// answers, which nib's language has grown past.
func TestParseMatchesCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Input    string
		Sections []any
		Error    *string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		sections, err := Parse(c.Input)
		if c.Error != nil {
			if err == nil || err.Error() != *c.Error {
				t.Errorf("Parse(%q) error = %v, want %q", c.Input, err, *c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) failed: %v", c.Input, err)
			continue
		}
		if got := shape(sections); !reflect.DeepEqual(got, c.Sections) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(c.Sections)
			t.Errorf("Parse(%q)\n got %s\nwant %s", c.Input, gotJSON, wantJSON)
		}
	}
}

func TestImportTakesNamesAndNeedsNoTag(t *testing.T) {
	sections, err := Parse("[A.java]\nIMPORT java.util.List, static org.junit.Assert.assertEquals\n[B.java#ABCD]\nPUT 2:\n+x\nIMPORT a.B")
	if err != nil {
		t.Fatal(err)
	}
	got := shape(sections)
	want := []any{
		map[string]any{"kind": "edit", "path": "A.java", "line": float64(1), "edits": []any{
			map[string]any{"start": float64(-1), "end": float64(-1), "edge": "tail", "operation": "after", "content": []any{}, "import": "java.util.List"},
			map[string]any{"start": float64(-1), "end": float64(-1), "edge": "tail", "operation": "after", "content": []any{}, "import": "static org.junit.Assert.assertEquals"},
		}},
		map[string]any{"kind": "edit", "path": "B.java", "tag": "ABCD", "line": float64(3), "edits": []any{
			map[string]any{"start": float64(2), "end": float64(2), "operation": "replace", "content": []any{"x"}},
			map[string]any{"start": float64(-1), "end": float64(-1), "edge": "tail", "operation": "after", "content": []any{}, "import": "a.B"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("got %s", gotJSON)
	}
	for input, message := range map[string]string{
		"[A.java]\nIMPORT a.B\n+import a.B;": "IMPORT takes no + - or ~ rows",
		"[A.java]\nIMPORT a.B,,c.D":          "IMPORT takes names with commas between them",
	} {
		if _, err := Parse(input); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%q: %v", input, err)
		}
	}
}
