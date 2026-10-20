package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAListingLongerThanAPageShowsTheShapeOfItsFolders(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	write := func(name string, age time.Duration) {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// A config folder: a transcript just written, and hundreds of plugin files from an hour ago, all in one chain of folders.
	write("projects/session.jsonl", time.Minute)
	write("history.jsonl", 2*time.Minute)
	for i := range 240 {
		write(fmt.Sprintf("plugins/marketplaces/official/plugins/p%02d/file%03d.md", i%12, i), time.Hour)
	}
	ctx := &Context{Ctx: context.Background(), Directory: directory, Session: "s"}
	newest := "modified"
	found, err := New().Find(ctx, FindArgs{Sort: newest})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(found.Text, "\n")
	if lines[0] != "242 files, newest first" || len(lines) > rollupRows+6 {
		t.Fatalf("got:\n%s", found.Text)
	}
	// The newest files come first, whole; the plugins show as their folders, each with its count.
	for _, want := range []string{"history.jsonl (1 line, 2 bytes, ", "projects/session.jsonl (1 line, 2 bytes, ", "plugins/marketplaces/official/plugins/", "p00/ (20 files, newest 1h ago)",
		"[a folder of many files shows as one row with their count; find it for its files, or skip 0 lists every file]"} {
		if !strings.Contains(found.Text, want) {
			t.Fatalf("no %q in:\n%s", want, found.Text)
		}
	}
	if strings.Index(found.Text, "session.jsonl") > strings.Index(found.Text, "plugins/") {
		t.Fatalf("not newest first:\n%s", found.Text)
	}
	// An explicit skip pages through the files themselves.
	zero := 0
	paged, err := New().Find(ctx, FindArgs{Sort: newest, Skip: &zero})
	if err != nil || strings.Contains(paged.Text, "files shows as one row") || !strings.Contains(paged.Text, "[242 files; continue with skip 200]") {
		t.Fatalf("got %v:\n%s", err, paged.Text)
	}
}
