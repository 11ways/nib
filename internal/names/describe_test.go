package names

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDescribeSaysWhereEachNameCameFromAndHowManyPathsPrintedUnderIt(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets, fixed, own := deep(t, root, "widgets"), deep(t, root, "gadgets"), deep(t, root, "fixed"), deep(t, root, "own")
	defined := NewDefined()
	defined.Automatic(filepath.Join(t.TempDir(), "learned.json"), root)
	static := &Table{Entries: []Entry{{"fixed", fixed}}, Base: root}
	main := static.For(defined, "main")
	if err := main.Define("g", gadgets); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		main.Display(filepath.Join(widgets, fmt.Sprintf("W%d.java", i)))
	}
	defined.Announced("main")
	main.Display(filepath.Join(widgets, "Last.java"))
	main.Display(filepath.Join(fixed, "A.java"))
	main.Display(filepath.Join(fixed, "B.java"))
	want := []Described{{"fixed", fixed, "server", 2}, {"g", gadgets, "defined", 0}, {"bewidgets", widgets, "automatic", 1}}
	if got := main.Describe(); !reflect.DeepEqual(got, want) {
		t.Fatalf("main:\n got %v\nwant %v", got, want)
	}
	// A subagent has what its parent had, called inherited, its own names, and counts for itself.
	defined.Inherit("child", "main", time.Time{})
	child := static.For(defined, "child")
	if err := child.Define("own", own); err != nil {
		t.Fatal(err)
	}
	wantChild := []Described{{"fixed", fixed, "server", 0}, {"g", gadgets, "inherited", 0}, {"bewidgets", widgets, "inherited", 0}, {"own", own, "defined", 0}}
	if got := child.Describe(); !reflect.DeepEqual(got, wantChild) {
		t.Fatalf("child:\n got %v\nwant %v", got, wantChild)
	}
	// Describing reads only: it neither counts a path nor makes a name.
	if again := main.Describe(); !reflect.DeepEqual(again, want) {
		t.Fatalf("describing changed it: %v", again)
	}
	if got := (&Table{}).For(nil, "none").Describe(); len(got) != 0 {
		t.Fatalf("no names: %v", got)
	}
}
