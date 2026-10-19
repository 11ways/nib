package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func gitInit(t *testing.T, directory string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, append([]string{"init", "-q"}, args...)...)...)
	cmd.Dir = directory
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v\n%s", directory, err, out)
	}
}

func TestRepositoriesFindsEveryRepositoryBelowAFolderButNoWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root, _ := filepath.EvalSymlinks(t.TempDir())
	// Outside git: two repositories, one with an ignored nested repository and an untracked one, and one inside a plain folder.
	gitInit(t, filepath.Join(root, "zenit"))
	gitInit(t, filepath.Join(root, "apps", "arcana"))
	gitInit(t, filepath.Join(root, "zenit", "vendored"))
	gitInit(t, filepath.Join(root, "zenit", "loose"))
	if err := os.WriteFile(filepath.Join(root, "zenit", ".gitignore"), []byte("vendored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A worktree of zenit inside it repeats zenit's files and is no repository of its own.
	for _, args := range [][]string{{"commit", "-q", "--allow-empty", "-m", "x"}, {"worktree", "add", "-q", ".wt/one"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = filepath.Join(root, "zenit")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	want := []string{filepath.Join(root, "apps", "arcana"), filepath.Join(root, "zenit"), filepath.Join(root, "zenit", "loose"),
		filepath.Join(root, "zenit", "vendored")}
	found, capped, err := Repositories(context.Background(), root)
	if err != nil || capped || !slices.Equal(found, want) {
		t.Fatalf("got %v %v %v, want %v", found, capped, err, want)
	}
	// From inside a repository: it and its own nested ones.
	found, _, err = Repositories(context.Background(), filepath.Join(root, "zenit"))
	if err != nil || !slices.Equal(found, want[1:]) {
		t.Fatalf("from zenit got %v %v, want %v", found, err, want[1:])
	}
	// From a folder inside a repository: only what lies below it.
	if err := os.MkdirAll(filepath.Join(root, "zenit", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if found, _, err = Repositories(context.Background(), filepath.Join(root, "zenit", "src")); err != nil || len(found) != 0 {
		t.Fatalf("from zenit/src got %v %v", found, err)
	}
}
