package engine

import "testing"

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
	if got := jsonWarnings("en.json", []string{`{"a": 1}`}, []string{`{"a": 1, "a": 2}`}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got := jsonWarnings("tsconfig.json", []string{"// comment", "{}"}, []string{"// comment", "{,}"}); got != nil {
		t.Fatalf("a file that never parsed must be left alone, got %v", got)
	}
	if got := jsonWarnings("en.txt", nil, []string{"{,}"}); got != nil {
		t.Fatalf("only .json files are checked, got %v", got)
	}
}
