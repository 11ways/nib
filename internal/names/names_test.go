package names

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJvmSourceRootsAreNamedByModuleAndSet(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	isUser = func(name string) bool { return name == "root" }
	t.Cleanup(func() { isUser = func(string) bool { return false } })
	touch(t, filepath.Join(base, "zenit/src/server/java/be/elevenways/zenit/server/Foo.java"))
	touch(t, filepath.Join(base, "zenit/src/server/java/be/elevenways/zenit/server/orm/Bar.java"))
	touch(t, filepath.Join(base, "protoblast/src/main/java/be/elevenways/protoblast/A.java"))
	// Too short a prefix to be worth a name.
	touch(t, filepath.Join(base, "x/src/main/java/A.java"))
	// Build output is never walked.
	touch(t, filepath.Join(base, "zenit/build/src/main/java/be/elevenways/generated/G.java"))
	table, notes := Load(base)
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
	got := map[string]string{}
	for _, entry := range table.Entries {
		got[entry.Name], _ = filepath.Rel(base, entry.Directory)
	}
	want := map[string]string{"zenit-server": "zenit/src/server/java/be/elevenways/zenit/server", "protoblast": "protoblast/src/main/java/be/elevenways/protoblast"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for name, path := range want {
		if got[name] != path {
			t.Fatalf("~%s = %q, want %q", name, got[name], path)
		}
	}
	file := filepath.Join(base, "zenit/src/server/java/be/elevenways/zenit/server/orm/Bar.java")
	if shown, ok := table.Display(file); !ok || shown != "~zenit-server/orm/Bar.java" {
		t.Fatalf("display %q", shown)
	}
	if expanded, ok := table.Expand("~zenit-server/orm/Bar.java"); !ok || expanded != file {
		t.Fatalf("expand %q", expanded)
	}
	if _, ok := table.Expand("~/x"); ok {
		t.Fatal("~/ is the home directory, not a name")
	}
	lines := table.Lines(1000)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "~protoblast = protoblast/src/main/java/") {
		t.Fatalf("lines %v", lines)
	}
}

func TestConfiguredNamesWinAndBadOnesAreNoted(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	isUser = func(name string) bool { return name == "root" }
	t.Cleanup(func() { isUser = func(string) bool { return false } })
	mkdir(t, filepath.Join(base, "a/very/long/path/to/the/orm/package"))
	config := `{"directories": {"orm": "a/very/long/path/to/the/orm/package", "root": "a", "Bad": "a", "gone": "nope"}, "autoDirectories": false}`
	if err := os.WriteFile(filepath.Join(base, "nib.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	table, notes := Load(base)
	if len(table.Entries) != 1 || table.Entries[0].Name != "orm" {
		t.Fatalf("entries %v", table.Entries)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"~Bad skipped", "~root skipped: it is a user", "~gone skipped"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes lack %q: %s", want, joined)
		}
	}
	// Only the names that fit the instructions resolve.
	table.Keep(table.Lines(5))
	if len(table.Entries) != 0 {
		t.Fatal("a name the model was never told must not resolve")
	}
}
