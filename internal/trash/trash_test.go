package trash

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func scene(t *testing.T) (*Can, string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := t.TempDir()
	c := New()
	c.now = func() time.Time { return time.Date(2026, 10, 18, 9, 30, 5, 0, time.Local) }
	return c, dir
}

func TestEntriesFollowTheSpecificationAndNamesStayUnique(t *testing.T) {
	c, dir := scene(t)
	for i := range 2 {
		sub := filepath.Join(dir, strings.Repeat("x", i+1))
		os.MkdirAll(sub, 0o755)
		path := filepath.Join(sub, "a b%.txt")
		if err := os.WriteFile(path, []byte("a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Put(path, path, false); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.ReadFile(filepath.Join(Home(), "info", "a b%.txt.2.trashinfo"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[Trash Info]\nPath=" + escape(filepath.ToSlash(dir)) + "/xx/a%20b%25.txt\nDeletionDate=2026-10-18T09:30:05\n"
	if string(info) != want {
		t.Fatalf("got %q, want %q", info, want)
	}
	if data, err := os.ReadFile(filepath.Join(Home(), "files", "a b%.txt")); err != nil || string(data) != "a\n" {
		t.Fatalf("got %q %v", data, err)
	}
	if got := len(c.Entries()); got != 2 {
		t.Fatalf("%d entries", got)
	}
}

func TestAcrossFilesystemsWithoutTheirOwnTrashACheckedCopyGoesHome(t *testing.T) {
	c, dir := scene(t)
	c.rename = func(string, string) error { return &os.LinkError{Op: "rename", Err: syscall.EXDEV} }
	tree := filepath.Join(dir, "tree")
	os.MkdirAll(filepath.Join(tree, "sub"), 0o755)
	os.WriteFile(filepath.Join(tree, "sub", "f.txt"), []byte("f\n"), 0o640)
	os.Symlink("sub/f.txt", filepath.Join(tree, "link"))
	entry, err := c.Put(tree, tree, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatal("the original stayed")
	}
	if data, err := os.ReadFile(filepath.Join(entry.File, "sub", "f.txt")); err != nil || string(data) != "f\n" {
		t.Fatalf("copy: %q %v", data, err)
	}
	if link, err := os.Readlink(filepath.Join(entry.File, "link")); err != nil || link != filepath.FromSlash("sub/f.txt") {
		t.Fatalf("link: %q %v", link, err)
	}
	if _, err := c.Restore(tree, false, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(tree, "sub", "f.txt")); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("restored: %v %v", info, err)
	}
	if len(c.Entries()) != 0 {
		t.Fatal("a restored entry stays listed")
	}
}

func TestRestoreRefusesAPathInUseUnlessForcedAndRemakesParents(t *testing.T) {
	c, dir := scene(t)
	path := filepath.Join(dir, "deep", "er", "a.txt")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("old\n"), 0o644)
	if _, err := c.Put(path, path, false); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dir, "deep"))
	if _, err := c.Restore(path, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(path, path, false); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("new\n"), 0o644)
	if _, err := c.Restore(path, false, false); err != ErrExists {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Restore(path, true, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "old\n" {
		t.Fatalf("got %q", data)
	}
	entries := c.Entries()
	if len(entries) != 1 {
		t.Fatalf("the replaced file is not in the trash: %v", entries)
	}
	if data, _ := os.ReadFile(entries[0].File); string(data) != "new\n" {
		t.Fatalf("got %q", data)
	}
	if _, err := c.Restore(filepath.Join(dir, "never"), false, false); err != ErrNotTrashed {
		t.Fatalf("got %v", err)
	}
	// An entry someone emptied from the trash leaves the list.
	os.Remove(entries[0].File)
	if len(c.Entries()) != 0 {
		t.Fatal("an emptied entry stays listed")
	}
}
