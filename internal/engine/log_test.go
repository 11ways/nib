package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadLogResolvesSelectorsToTheEndAndStartsWhereAskedWithout(t *testing.T) {
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(directory, "run.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	e, ctx := New(), &Context{Ctx: context.Background(), Directory: directory, Session: "s"}
	// An open range runs past read's page of 2000 lines to the end; a selector never asks where to start.
	for entry, want := range map[string][][2]int{"run.log:2400-": {{2400, 2500}}, "run.log:-3": {{2498, 2500}}, "run.log:1-2,5": {{1, 2}, {5, 5}}, "run.log:400-": {{400, 2500}}} {
		log, err := e.ReadLog(ctx, entry, 1<<20, func(*LogFile) int64 { t.Fatalf("%s asked where to start", entry); return 0 })
		if err != nil || !slices.Equal(log.Ranges, want) || log.From != 0 {
			t.Fatalf("%s: %+v %v", entry, log, err)
		}
	}
	log, err := e.ReadLog(ctx, "run.log", 10, func(f *LogFile) int64 { return 4 })
	if err != nil || log.From != 4 || string(log.Raw) != "3\n4\n5\n6\n7\n" || !log.Short || log.Ranges != nil || string(log.Head[:4]) != "1\n2\n" {
		t.Fatalf("from an offset: %+v %v", log, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "image.png"), []byte("\x89PNG\r\n\x1a\nxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	for entry, want := range map[string]string{".": "is a directory", "image.png": "is not a text file", "gone.log": "does not exist", "run.log:Foo": "takes no declared name"} {
		if _, err := e.ReadLog(ctx, entry, 10, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", entry, err)
		}
	}
}
