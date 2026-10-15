package names

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefinedNamesAreInheritedAsOfTheSpawnMoment(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(root, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(100, 0)
	defined := NewDefined()
	defined.Now = func() time.Time { return now }
	static := &Table{Entries: []Entry{{"fixed", filepath.Join(root, "c")}}, Base: root}
	parent := static.For(defined, "parent")
	if err := parent.Define("one", filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(200, 0)
	if err := parent.Define("two", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	// A redefinition wins for the definer and for agents spawned after it.
	now = time.Unix(300, 0)
	if err := parent.Define("one", filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	defined.Inherit("early", "parent", time.Unix(150, 0))
	defined.Inherit("middle", "parent", time.Unix(250, 0))
	defined.Inherit("now", "parent", time.Time{})
	for agent, want := range map[string]string{
		"parent": "fixed one=b two=b",
		"early":  "fixed one=a",
		"middle": "fixed one=a two=b",
		"now":    "fixed one=b two=b",
		"other":  "fixed",
	} {
		var got []string
		for _, entry := range static.For(defined, agent).Entries() {
			if entry.Name == "fixed" {
				got = append(got, "fixed")
				continue
			}
			got = append(got, entry.Name+"="+filepath.Base(entry.Directory))
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s sees %v, want %s", agent, got, want)
		}
	}
	// Inheriting happens once; a later call keeps what the agent has.
	defined.Inherit("early", "parent", time.Time{})
	if _, ok := static.For(defined, "early").Home("two"); ok {
		t.Fatal("an agent inherits once")
	}
	if shown, ok := static.For(defined, "early").Display(filepath.Join(root, "a", "x.go")); !ok || shown != "~one/x.go" {
		t.Fatalf("got %q", shown)
	}
	if expanded, ok := static.For(defined, "middle").Expand("~two/y"); !ok || expanded != filepath.Join(root, "b", "y") {
		t.Fatalf("got %q", expanded)
	}
}

func TestDefineRefusesBadNamesAndDirectories(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "f.txt")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	original := isUser
	isUser = func(name string) bool { return name == "someone" }
	t.Cleanup(func() { isUser = original })
	view := (&Table{Entries: []Entry{{"fixed", root}}}).For(NewDefined(), "agent")
	for name, directory := range map[string]string{"Bad": root, "fixed": root, "someone": root, "rel": "relative", "gone": filepath.Join(root, "gone"), "file": file} {
		if view.Define(name, directory) == nil {
			t.Errorf("~%s=%s must be refused", name, directory)
		}
	}
	for i := range DefinedLimit {
		if err := view.Define(fmt.Sprintf("n%d", i), root); err != nil {
			t.Fatal(err)
		}
	}
	if view.Define("more", root) == nil {
		t.Fatal("definitions past the limit must be refused")
	}
	// A nil table still takes definitions.
	if err := (*Table)(nil).For(NewDefined(), "a").Define("ok", root); err != nil {
		t.Fatal(err)
	}
}
