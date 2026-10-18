package snapshots

import (
	"reflect"
	"strings"
	"testing"
)

func TestSeenFilesListsWhatASessionSawWithoutTouchingTheStore(t *testing.T) {
	s := NewStore()
	lines := strings.Split("a b c d e f g h i", " ")
	s.Record("main", "a.go", NewView(strings.Join(lines, "\n"), lines), []int{1, 2, 3, 7}, []int{9})
	s.Record("main", "b.go", NewView("x", []string{"x"}), nil, nil)
	s.Record("other", "c.go", NewView("y", []string{"y"}), []int{1}, nil)
	got := s.SeenFiles("main")
	if len(got) != 2 || got[0].Path != "b.go" || got[1].Path != "a.go" {
		t.Fatalf("the most recently used file comes first, and only this session's: %+v", got)
	}
	if !got[0].Anywhere || got[0].Lines != 1 || len(got[0].Seen) != 0 {
		t.Fatalf("a file with no record of what was seen allows edits anywhere: %+v", got[0])
	}
	want := SeenFile{Path: "a.go", Tag: got[1].Tag, Lines: 9, Seen: [][2]int{{1, 3}, {7, 7}}, Known: 1}
	if !reflect.DeepEqual(got[1], want) || want.Tag == "" {
		t.Fatalf("got %+v\nwant %+v", got[1], want)
	}
	if again := s.SeenFiles("main"); !reflect.DeepEqual(again, got) {
		t.Fatalf("listing changed the order: %+v", again)
	}
	if none := s.SeenFiles("nobody"); len(none) != 0 {
		t.Fatalf("%+v", none)
	}
}
