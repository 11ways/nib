package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The roots answer differently the second time, as a link that changes between two resolutions would make them disagree: the sandbox judges
// the canonical path an operation uses, not only the path it was asked for.
func TestSandboxJudgesTheCanonicalPath(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(directory, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	asked := 0
	ctx := &Context{Ctx: context.Background(), Directory: directory, Session: "s", Sandbox: true, Roots: func(context.Context) ([]string, error) {
		asked++
		if asked == 1 {
			return []string{directory}, nil
		}
		return []string{filepath.Join(directory, "src")}, nil
	}}
	_, err := New().Read(ctx, ReadArgs{Paths: []string{"file.txt"}})
	if err == nil || !strings.Contains(err.Error(), "file.txt resolves outside the sandbox") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancelledCallsStopBeforeWriting(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	file := filepath.Join(directory, "file.txt")
	if err := os.WriteFile(file, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	live := &Context{Ctx: context.Background(), Directory: directory, Session: "s"}
	read, err := e.Read(live, ReadArgs{Paths: []string{"file.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	tagValue := read.Text[strings.Index(read.Text, "#")+1 : strings.Index(read.Text, "]")]
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	dead := &Context{Ctx: cancelled, Directory: directory, Session: "s"}
	if _, err := e.Patch(dead, "[file.txt#"+tagValue+"]\nPUT 1:\n+x"); err == nil {
		t.Fatal("a cancelled edit must fail")
	}
	if data, _ := os.ReadFile(file); string(data) != "one\n" {
		t.Fatalf("file changed: %q", data)
	}
}
