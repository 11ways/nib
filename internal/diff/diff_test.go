package diff

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// testdata/jsdiff.json holds random line arrays with the changes jsdiff 8.0.2's diffArrays returned for them.
func TestArraysMatchesJsdiff(t *testing.T) {
	data, err := os.ReadFile("testdata/jsdiff.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Old, New []string
		Changes  []struct {
			Count          int
			Added, Removed bool
			Value          []string
		}
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		got, ok := Arrays(c.Old, c.New, time.Second)
		if !ok {
			t.Fatalf("case %d timed out", i)
		}
		if len(got) != len(c.Changes) {
			t.Fatalf("case %d: %d changes, want %d\nold %v\nnew %v\ngot %+v", i, len(got), len(c.Changes), c.Old, c.New, got)
		}
		for j, want := range c.Changes {
			g := got[j]
			if g.Count != want.Count || g.Added != want.Added || g.Removed != want.Removed || !reflect.DeepEqual(append([]string{}, g.Value...), append([]string{}, want.Value...)) {
				t.Fatalf("case %d change %d: got %+v, want %+v", i, j, g, want)
			}
		}
	}
}

func TestArraysGivesUpAfterTimeout(t *testing.T) {
	old := make([]string, 4000)
	new := make([]string, 4000)
	for i := range old {
		old[i] = string(rune('a' + i%7))
		new[i] = string(rune('a' + (i*3)%11))
	}
	if _, ok := Arrays(old, new, 0); ok {
		t.Fatal("expected a zero timeout to give up")
	}
}
