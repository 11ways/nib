package snapshots

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Keep drops saved files older than keptFor and keeps nothing in a folder others may enter.
func TestKeepDropsOldFilesAndNeedsAPrivateFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "seen")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, fresh := filepath.Join(dir, "old.json"), filepath.Join(dir, "fresh.json")
	for _, name := range []string{old, fresh} {
		if err := os.WriteFile(name, []byte(`{"paths":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-keptFor - time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	s.Keep(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("a file older than %v must go: %v", keptFor, err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a recent file must stay: %v", err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.MkdirAll(open, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := NewStore()
	shared.Keep(open)
	shared.Persist("main", "claude:x")
	shared.Record("main", "/a", NewView("a\n", []string{"a"}), []int{1}, nil)
	shared.Flush()
	if entries, _ := os.ReadDir(open); len(entries) != 0 {
		t.Fatalf("a folder others may enter must keep nothing: %v", entries)
	}
}

// A session without a lasting key saves nothing, and one with a key writes only hashes and line numbers.
func TestOnlySessionsWithALastingKeyAreSaved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "seen")
	s := NewStore()
	s.Keep(dir)
	s.Record("other", "/a", NewView("secret text\n", []string{"secret text"}), []int{1}, nil)
	s.Flush()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a session without a key must save nothing: %v", entries)
	}
	s.Persist("main", "claude:x")
	s.Record("main", "/a", NewView("secret text\n", []string{"secret text"}), []int{1}, nil)
	s.Flush()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("one file: %v", entries)
	}
	saved := loadSaved(filepath.Join(dir, entries[0].Name()))
	if p := saved.Paths["/a"]; p == nil || p.Hash != hashOf("secret text\n") || len(p.Seen) != 1 {
		t.Fatalf("saved: %+v", saved.Paths["/a"])
	}
	restored := NewStore()
	restored.Keep(dir)
	restored.Persist("main", "claude:x")
	if v := restored.Record("main", "/a", NewView("secret text\n", []string{"secret text"}), []int{}, nil); !v.Seen[1] {
		t.Fatalf("the restored version must hold line 1: %v", v.Seen)
	}
}
