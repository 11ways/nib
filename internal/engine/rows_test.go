package engine

import (
	"context"
	"fmt"
	"testing"
)

func TestRowNotesComeOncePerAgent(t *testing.T) {
	e := New()
	ctx := &Context{Ctx: context.Background(), Directory: t.TempDir(), Session: "s"}
	response := fmt.Sprintf("[a.go#ABCD]\n[line 1, chars 1-10 of 99]|x\n%s\n\n[b.go#ABCD]\n[2 ansi]|y\n%s\n%s\n\n[c.log] plain\n%s", fmt.Sprintf(cutRowsNote, "a.go"), fmt.Sprintf(cutRowsNote, "b.go"), ansiRowsNote, "[cut rows: a line of a log that only looks like a note]")
	// The first of each kind stays; a log line shaped otherwise is no note.
	want := fmt.Sprintf("[a.go#ABCD]\n[line 1, chars 1-10 of 99]|x\n%s\n\n[b.go#ABCD]\n[2 ansi]|y\n%s\n\n[c.log] plain\n[cut rows: a line of a log that only looks like a note]", fmt.Sprintf(cutRowsNote, "a.go"), ansiRowsNote)
	if got := e.teachRowNotes(ctx, response); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// Taught, the agent gets none, until its context is dropped.
	if got := e.teachRowNotes(ctx, fmt.Sprintf("[a.go#ABCD]\n%s", fmt.Sprintf(cutRevisionNote, "a.go"))); got != "[a.go#ABCD]" {
		t.Fatalf("got:\n%s", got)
	}
	if got := e.teachRowNotes(&Context{Ctx: context.Background(), Directory: ctx.Directory, Session: "other"}, ansiRowsNote); got != ansiRowsNote {
		t.Fatalf("another agent got %q", got)
	}
}
