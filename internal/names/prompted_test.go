package names

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestMentionedFindsNamesNotPartsOfWordsOrPaths(t *testing.T) {
	got := Mentioned("see ~w/a.go and (~g), ~my-dir: not a~b, /x/~c, ~/home, ~Foo, ~w_x or ~w again; path:~p/x")
	if want := []string{"w", "g", "my-dir", "p"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAMessageIsToldTheNamesItsAgentDoesNotHold(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets, tools := deep(t, root, "widgets"), deep(t, root, "gadgets"), deep(t, root, "tools")
	file := filepath.Join(t.TempDir(), "learned.json")
	defined := NewDefined()
	defined.Automatic(file, root)
	static := &Table{Base: root, Entries: []Entry{{"s", tools}}}
	main := static.For(defined, "main")
	if err := main.Define("w", widgets); err != nil {
		t.Fatal(err)
	}
	if err := static.For(defined, "agent:a1").Define("g", gadgets); err != nil {
		t.Fatal(err)
	}
	// The instructions' names and one the agent holds need no telling; one nothing knows is left out.
	if got := main.Prompted([]string{"s", "w", "nothing"}); len(got) != 0 {
		t.Fatalf("told %v", got)
	}
	// Another agent's name is told, and works for this agent from then on.
	if got := main.Prompted([]string{"g"}); !slices.Equal(got, []string{"~g = " + filepath.ToSlash(rel(t, root, gadgets))}) {
		t.Fatalf("another agent's name: %v", got)
	}
	if expanded, ok := main.Expand("~g/A.java"); !ok || expanded != filepath.Join(gadgets, "A.java") {
		t.Fatalf("~g expands to %q, %v", expanded, ok)
	}
	// After a drop, a name the agent had is told once, and no note on a path printed under it tells it again.
	defined.Now = func() time.Time { return time.Now().Add(time.Hour) }
	defined.Forget("main", time.Now().Add(time.Minute))
	if got := main.Prompted([]string{"w", "g"}); len(got) != 2 {
		t.Fatalf("after a drop: %v", got)
	}
	if got := main.Prompted([]string{"w"}); len(got) != 0 {
		t.Fatalf("told twice: %v", got)
	}
	main.Display(filepath.Join(widgets, "A.java"))
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("a note retold it: %v", notes)
	}
}

func TestAMessageIsToldTheNamesAnEarlierSessionHad(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets := deep(t, root, "widgets"), deep(t, root, "gadgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	earlier := NewDefined()
	earlier.Automatic(file, root)
	for name, directory := range map[string]string{"w": widgets, "g": gadgets} {
		if err := (&Table{Base: root}).For(earlier, "main").Define(name, directory); err != nil {
			t.Fatal(err)
		}
	}
	// A later server, whose own table gives ~g another directory, as nib.json can.
	defined := NewDefined()
	defined.Automatic(file, root)
	main := (&Table{Base: root, Entries: []Entry{{"g", widgets}}}).For(defined, "main")
	got := main.Prompted([]string{"w", "g"})
	want := []string{
		"~w = " + filepath.ToSlash(rel(t, root, widgets)),
		"~g = " + filepath.ToSlash(rel(t, root, widgets)) + " (in an earlier session ~g was " + filepath.ToSlash(gadgets) + ")",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if expanded, ok := main.Expand("~w"); !ok || expanded != widgets {
		t.Fatalf("~w expands to %q, %v", expanded, ok)
	}
}

func rel(t *testing.T, base, directory string) string {
	t.Helper()
	relative, err := filepath.Rel(base, directory)
	if err != nil {
		t.Fatal(err)
	}
	return relative
}
