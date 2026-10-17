package names

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deep makes root/zenit/src/server/java/be/elevenways/<leaf> and returns it.
func deep(t *testing.T, root, leaf string) string {
	t.Helper()
	directory := filepath.Join(root, "zenit", "src", "server", "java", "be", "elevenways", leaf)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestADirectoryShownPathsKeepRunningThroughGetsANameOnce(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := deep(t, root, "widgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	defined := NewDefined()
	defined.Automatic(file, root)
	view := (&Table{Base: root}).For(defined, "main")
	for i := range 5 {
		view.Display(filepath.Join(widgets, fmt.Sprintf("W%d.java", i)))
	}
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("named too soon: %v", notes)
	}
	if shown, ok := view.Display(filepath.Join(widgets, "Last.java")); ok {
		t.Fatalf("a name takes effect only once told, so a response prints its paths alike, got %q", shown)
	}
	notes := defined.Announced("main")
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "[nib named ~bewidgets = zenit/src/server/java/be/elevenways/widgets, which 6 of the paths shown to you ran through;") {
		t.Fatalf("notes %v", notes)
	}
	if again := defined.Announced("main"); len(again) != 0 {
		t.Fatalf("a note is told once, got %v", again)
	}
	if shown, _ := view.Display(filepath.Join(widgets, "Last.java")); shown != "~bewidgets/Last.java" {
		t.Fatalf("got %q", shown)
	}
	// Another agent counts for itself.
	if shown, ok := (&Table{Base: root}).For(defined, "other").Display(filepath.Join(widgets, "A.java")); ok {
		t.Fatalf("another agent got the name: %q", shown)
	}
	// The next server of the same directory has it from the start; one of another directory does not.
	next := &Table{Base: root}
	next.AddLearned(file)
	if len(next.Entries) != 1 || next.Entries[0] != (Entry{"bewidgets", widgets}) {
		t.Fatalf("learned %v", next.Entries)
	}
	elsewhere := &Table{Base: filepath.Join(root, "zenit")}
	elsewhere.AddLearned(file)
	if len(elsewhere.Entries) != 0 {
		t.Fatalf("another directory learned %v", elsewhere.Entries)
	}
}

func TestAPrefixTooLongToRepeatIsNamedTheFirstTimeItIsShown(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	skill := filepath.Join(root, "claude-1000", "bundled-skills", "2.1.302", "fc11d6128293e2a1c2935a66fffdd589", "plugin-authoring", "types")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	defined := NewDefined()
	defined.Automatic("", "/elsewhere")
	view := (&Table{Base: "/elsewhere"}).For(defined, "main")
	if shown, ok := view.Display(filepath.Join(skill, "claude-code.d.ts")); ok {
		t.Fatalf("got %q", shown)
	}
	if notes := defined.Announced("main"); len(notes) != 1 || !strings.Contains(notes[0], "/plugin-authoring/types, too long to print again; ~bptypes/ works") {
		t.Fatalf("notes %v", notes)
	}
	if shown, _ := view.Display(filepath.Join(skill, "claude-code.d.ts")); shown != "~bptypes/claude-code.d.ts" {
		t.Fatalf("got %q", shown)
	}
}

func TestLaterNotesAreBriefUntilManyCallsHavePassed(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	defined := NewDefined()
	defined.Automatic("", root)
	view := (&Table{Base: root}).For(defined, "main")
	// name shows the paths of one call that earn leaf's directory a name, and returns the note on it.
	name := func(leaf string) string {
		directory := deep(t, filepath.Join(root, leaf), leaf)
		for i := range 8 {
			view.Display(filepath.Join(directory, fmt.Sprintf("f%d", i)))
		}
		notes := defined.Announced("main")
		if len(notes) != 1 {
			t.Fatalf("notes %v", notes)
		}
		return notes[0]
	}
	if got := name("widgets"); !strings.HasPrefix(got, "[nib named ~bewidgets = ") {
		t.Fatalf("the first note is told in full, got %q", got)
	}
	if got := name("gadgets"); !strings.HasPrefix(got, "[~begadgets = gadgets/zenit/src/server/java/be/elevenways/gadgets, ") || !strings.HasSuffix(got, " paths shown]") {
		t.Fatalf("got %q", got)
	}
	// Calls that show paths count toward telling a note in full again.
	for range fullAgain - 3 {
		view.Display(filepath.Join(root, "x"))
		defined.Announced("main")
	}
	if got := name("doodads"); !strings.HasPrefix(got, "[~bedoodads = ") {
		t.Fatalf("got %q", got)
	}
	if got := name("gizmos"); !strings.HasPrefix(got, "[nib named ~begizmos = ") {
		t.Fatalf("a note is told in full again after %d calls, got %q", fullAgain, got)
	}
}

func TestShortPrefixesAndTakenNamesAndTheLimit(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	defined := NewDefined()
	defined.Automatic("", root)
	// A prefix under 30 characters as printed is never worth a name.
	short := filepath.Join(root, "src", "a")
	if err := os.MkdirAll(short, 0o755); err != nil {
		t.Fatal(err)
	}
	view := (&Table{Base: root, Entries: []Entry{{"bewidgets", short}}}).For(defined, "main")
	for i := range 3 * autoUses {
		view.Display(filepath.Join(root, "src", fmt.Sprintf("f%d", i)))
	}
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("a short prefix got a name: %v", notes)
	}
	// A taken name gets a number after it.
	widgets := deep(t, root, "widgets")
	for i := range 8 {
		view.Display(filepath.Join(widgets, fmt.Sprintf("W%d.java", i)))
	}
	defined.Announced("main")
	if shown, _ := view.Display(filepath.Join(widgets, "X.java")); shown != "~bewidgets2/X.java" {
		t.Fatalf("got %q", shown)
	}
	defined.Announced("main")
	// At most autoLimit names per agent.
	for leaf := range autoLimit + 2 {
		directory := filepath.Join(root, fmt.Sprintf("project%d", leaf), "src", "main", "java", "be", "elevenways", "core")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range 8 {
			view.Display(filepath.Join(directory, fmt.Sprintf("f%d", i)))
		}
	}
	if named := len(defined.Announced("main")); named != autoLimit-1 {
		t.Fatalf("made %d more names, want %d", named, autoLimit-1)
	}
}

func TestNamesAnAgentDefinesAreLearnedToo(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := deep(t, root, "widgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	defined := NewDefined()
	defined.Automatic(file, root)
	if err := (&Table{Base: root}).For(defined, "main").Define("w", widgets); err != nil {
		t.Fatal(err)
	}
	next := &Table{Base: root}
	next.AddLearned(file)
	if len(next.Entries) != 1 || next.Entries[0].Name != "w" {
		t.Fatalf("learned %v", next.Entries)
	}
}
