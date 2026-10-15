package patch

import (
	"encoding/json"
	"os"
	"reflect"
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

// testdata/corpus.json holds patches with what the TypeScript parser returned for them, or its error message.
func TestParseMatchesTypeScriptParser(t *testing.T) {
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
