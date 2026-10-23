package names

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restarted is a new server's names for the same folders, as after a restart.
func restarted(t *testing.T, dir, learned, root string) *Defined {
	t.Helper()
	defined := NewDefined()
	defined.Automatic(learned, root)
	defined.Keep(dir)
	return defined
}

func TestAnAgentKeepsItsNamesUntoldAcrossARestart(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets := deep(t, root, "widgets"), deep(t, root, "gadgets")
	dir, learned := filepath.Join(t.TempDir(), "agents"), filepath.Join(t.TempDir(), "learned.json")
	first := restarted(t, dir, learned, root)
	first.Persist("main", "claude:session")
	view := (&Table{Base: root}).For(first, "main")
	if err := view.Define("w", widgets); err != nil {
		t.Fatal(err)
	}
	untilNamed(t, view, gadgets)
	if notes := first.Announced("main"); len(notes) != 1 || !strings.HasPrefix(notes[0], "[nib named ~") {
		t.Fatalf("first server's notes: %v", notes)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the folder is private: %v %v", info, err)
	}
	second := restarted(t, dir, learned, root)
	second.Persist("main", "claude:session")
	again := (&Table{Base: root}).For(second, "main")
	if shown, _ := again.Display(filepath.Join(widgets, "A.java")); shown != "~w/A.java" {
		t.Fatalf("a defined name works at once: %q", shown)
	}
	for i := range 5 {
		if shown, _ := again.Display(filepath.Join(gadgets, fmt.Sprintf("G%d.java", i))); !strings.HasPrefix(shown, "~") {
			t.Fatalf("a made name works at once: %q", shown)
		}
	}
	if notes := second.Announced("main"); len(notes) != 0 {
		t.Fatalf("names the agent holds are not told again: %v", notes)
	}
	// Another agent, and one with no lasting key, get nothing of them.
	other := restarted(t, dir, learned, root)
	other.Persist("main", "claude:other")
	if shown, _ := (&Table{Base: root}).For(other, "main").Display(filepath.Join(widgets, "A.java")); strings.HasPrefix(shown, "~w") {
		t.Fatalf("another key's agent: %q", shown)
	}
	if shown, _ := (&Table{Base: root}).For(restarted(t, dir, learned, root), "main").Display(filepath.Join(widgets, "A.java")); strings.HasPrefix(shown, "~w") {
		t.Fatalf("an agent with no key: %q", shown)
	}
}

func TestARestoredNameIsToldAgainOnlyWhenADropTookItAfterItWasLastTold(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets := deep(t, root, "widgets"), deep(t, root, "gadgets")
	dir, learned := filepath.Join(t.TempDir(), "agents"), filepath.Join(t.TempDir(), "learned.json")
	now := time.Now()
	clock := func(offset time.Duration) func() time.Time { return func() time.Time { return now.Add(offset) } }
	first := restarted(t, dir, learned, root)
	first.Now = clock(0)
	first.Persist("main", "claude:session")
	view := (&Table{Base: root}).For(first, "main")
	for name, directory := range map[string]string{"w": widgets, "g": gadgets} {
		if err := view.Define(name, directory); err != nil {
			t.Fatal(err)
		}
	}
	// A drop takes both; ~w is told again before the restart, ~g is not.
	dropped := now.Add(time.Second)
	first.Forget("main", dropped)
	first.Now = clock(2 * time.Second)
	view.Display(filepath.Join(widgets, "A.java"))
	if notes := first.Announced("main"); len(notes) != 1 || !strings.HasPrefix(notes[0], "[~w = ") {
		t.Fatalf("retold before the restart: %v", notes)
	}
	second := restarted(t, dir, learned, root)
	second.Persist("main", "claude:session")
	// The harness reports a drop once, so the restarted server learns of it from the saved names alone.
	again := (&Table{Base: root}).For(second, "main")
	again.Display(filepath.Join(widgets, "B.java"))
	again.Display(filepath.Join(gadgets, "C.java"))
	notes := second.Announced("main")
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "[~g = "+filepath.ToSlash(gadgets)) {
		t.Fatalf("only the name the drop took and no one told since is told: %v", notes)
	}
}

func TestPersistLeavesTheNamesAnAgentAlreadyHas(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets := deep(t, root, "widgets"), deep(t, root, "gadgets")
	dir := filepath.Join(t.TempDir(), "agents")
	first := NewDefined()
	first.Keep(dir)
	first.Persist("main", "key")
	if err := (&Table{Base: root}).For(first, "main").Define("w", widgets); err != nil {
		t.Fatal(err)
	}
	second := NewDefined()
	second.Keep(dir)
	view := (&Table{Base: root}).For(second, "main")
	if err := view.Define("g", gadgets); err != nil {
		t.Fatal(err)
	}
	second.Persist("main", "key")
	if shown, _ := view.Display(filepath.Join(widgets, "A.java")); strings.HasPrefix(shown, "~w") {
		t.Fatalf("restored over names the agent got here: %q", shown)
	}
	if shown, _ := view.Display(filepath.Join(gadgets, "A.java")); shown != "~g/A.java" {
		t.Fatalf("its own name: %q", shown)
	}
}
