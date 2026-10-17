package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func showFixture(t *testing.T, content string) (*Engine, *Context, string) {
	t.Helper()
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(directory, "a.py")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return New(), &Context{Ctx: context.Background(), Directory: directory, Session: "s"}, path
}

func indexed(text string) *string { return &text }

func TestShowPrintsLocationsAsReadRowsThatAnchorEdits(t *testing.T) {
	e, ctx, path := showFixture(t, "def a():\n    return 1\n\n\ndef b():\n    return 2\n")
	got, err := e.Show(ctx, "zemble", []Piece{
		{Text: "1. a.py:1-2 score 0.9"},
		{Path: path, Start: 1, End: 2, Indexed: indexed("def a():\n    return 1")},
		{Path: path, Start: 5, End: 5},
		{Text: "done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tagValue := got.Text[strings.Index(got.Text, "#")+1 : strings.Index(got.Text, "]\n")]
	want := fmt.Sprintf("1. a.py:1-2 score 0.9\n\n[a.py#%s]\n1:def a():\n2:    return 1\n...\n5:def b():\n[6 lines total]\n\ndone", tagValue)
	if got.Text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got.Text, want)
	}
	// Shown rows anchor an edit with no read; a line the answer did not show is refused, as after a partial read.
	if _, err := e.Patch(ctx, fmt.Sprintf("[a.py#%s]\nPUT 2:\n+    return 3", tagValue)); err != nil {
		t.Fatal(err)
	}
	read, _ := e.Read(ctx, ReadArgs{Paths: []string{"a.py:1"}})
	tagValue = read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
	if _, err := e.Patch(ctx, fmt.Sprintf("[a.py#%s]\nPUT 6:\n+    return 4", tagValue)); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("an unshown line must be refused, got %v", err)
	}
}

func TestShowFollowsLinesThatMovedSinceTheProviderIndexedThem(t *testing.T) {
	e, ctx, path := showFixture(t, "import os\n\ndef a():\n    return 1\n")
	got, err := e.Show(ctx, "zemble", []Piece{
		{Path: path, Start: 1, End: 2, Indexed: indexed("def a():\n    return 1")},
		{Path: path, Start: 1, End: 1, Indexed: indexed("def gone():")},
		{Text: "next"},
		{Path: path, Start: 9, End: 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{
		"[a.py changed since zemble indexed it: its lines 1-2 are now 3-4]",
		"[a.py changed since zemble indexed it: lines 1-1 no longer hold what it found]",
		"\n1:import os\n...\n3:def a():\n4:    return 1\n",
		"[a.py has 4 lines now: zemble's lines 9-12 are past its end]",
	} {
		if !strings.Contains(got.Text, part) {
			t.Fatalf("missing %q in:\n%s", part, got.Text)
		}
	}
}

func TestShowKeepsProseFromPassingAsRows(t *testing.T) {
	e, ctx, _ := showFixture(t, "x\n")
	got, err := e.Show(ctx, "zemble", []Piece{{Text: "callers:\n12:foo()"}, {Path: filepath.Join(ctx.Directory, "missing.py"), Start: 1, End: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Text, "[zemble's own text follows, not anchors]\ncallers:\n12:foo()\n\n[") || !strings.Contains(got.Text, "missing.py") {
		t.Fatalf("got:\n%s", got.Text)
	}
	long := strings.Repeat("word ", 20) + "\n"
	got, err = e.Show(ctx, "zemble", []Piece{{Text: strings.Repeat(long, 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Text) > OutputLimit+200 || !strings.Contains(got.Text, "more lines of zemble's answer cut by the output limit]") {
		t.Fatalf("prose must share read's budget, got %d chars ending %q", len(got.Text), got.Text[len(got.Text)-120:])
	}
}
