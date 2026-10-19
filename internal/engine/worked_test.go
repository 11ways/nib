package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// workedNames records the folders the engine tells it the agent works in.
type workedNames struct{ worked []string }

func (w *workedNames) Expand(string) (string, bool)  { return "", false }
func (w *workedNames) Display(string) (string, bool) { return "", false }
func (w *workedNames) Worked(directory string)       { w.worked = append(w.worked, directory) }

func TestAWriteAndAMoveTellTheNamesWhereTheAgentWorks(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	for _, folder := range []string{"src", "dest"} {
		if err := os.Mkdir(filepath.Join(directory, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	names := &workedNames{}
	e, ctx := New(), &Context{Ctx: context.Background(), Directory: directory, Session: "s", Names: names}
	written, err := e.Write(ctx, "src/a.txt", "one\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names.worked, []string{filepath.Join(directory, "src")}) {
		t.Fatalf("after a write: %v", names.worked)
	}
	names.worked = nil
	if _, err := e.Patch(ctx, "[src/a.txt#"+regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(written.Text)[1]+"]\nMV dest/a.txt"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names.worked, []string{filepath.Join(directory, "src"), filepath.Join(directory, "dest")}) {
		t.Fatalf("after a move: %v", names.worked)
	}
}

func TestIgnoredFollowsTheRulesOfTheRepositoryHoldingADirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	if out, err := exec.Command("git", "init", "-q", directory).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, folder := range []string{"build/zenit-dev/copy", "src/main"} {
		if err := os.MkdirAll(filepath.Join(directory, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for folder, want := range map[string]bool{"build/zenit-dev/copy": true, "build": true, "src/main": false, ".": false} {
		if got := Ignored(filepath.Join(directory, folder)); got != want {
			t.Errorf("%s: got %v, want %v", folder, got, want)
		}
	}
	if Ignored(t.TempDir()) {
		t.Error("a folder outside git counts as ignored")
	}
}
