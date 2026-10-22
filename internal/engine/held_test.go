package engine

import (
	"regexp"
	"strings"
	"testing"
)

func TestAMarkerSaysWhenItsLinesWereSeenBeforeTheLastEdit(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 40; i++ {
		content.WriteString(word(i) + "\n")
	}
	e, ctx, path := showFixture(t, content.String())
	ctx.Markers = true
	first := readText(t, e, ctx, path)
	tag := regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(first)[1]
	if strings.Contains(first, "unchanged") {
		t.Fatalf("the first read holds nothing:\n%s", first)
	}
	// Read again before any edit: the marker says nothing of edits.
	if out := readText(t, e, ctx, path); !strings.Contains(out, "\n[lines 1-40 unchanged since you saw them]\n") || strings.Contains(out, "before the last edit") {
		t.Fatalf("before an edit:\n%s", out)
	}
	edited, err := e.Patch(ctx, "[a.py#"+tag+"]\nPUT 1:\n+NEW")
	if err != nil || !strings.Contains(edited.Text, "\n[line 1 as you sent it]\n") {
		t.Fatalf("the edit: %v\n%s", err, edited.Text)
	}
	// After the edit, the lines it left stand as seen before it, which the marker says of them; the row the agent sent counts as seen with the
	// edit.
	out := readText(t, e, ctx, path)
	if !strings.Contains(out, "\n[lines 1-40 unchanged since you saw them, 2-40 of them before the last edit of this file]") {
		t.Fatalf("after the edit:\n%s", out)
	}
	// A later read says it again: the lines were still last seen whole before that edit, which is what the agent needs to know.
	if out := readText(t, e, ctx, path); !strings.Contains(out, "unchanged since you saw them, 2-40 of them before the last edit of this file]") {
		t.Fatalf("after the edit was seen:\n%s", out)
	}
}

func TestRowsTheAgentSentPrintAsOneMarkerAndASubsResultAsItsRow(t *testing.T) {
	e, ctx, path := showFixture(t, "one\ntwo\nthree\nfour\n")
	first := readText(t, e, ctx, path)
	tag := regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(first)[1]
	sent, err := e.Patch(ctx, "[a.py#"+tag+"]\nPUT 2.=3:\n+TWO\n+THREE\n+THREE AND A HALF")
	if err != nil || !strings.Contains(sent.Text, "]\n1:one\n[lines 2-4 as you sent them]\n5:four\n") {
		t.Fatalf("sent rows: %v\n%s", err, sent.Text)
	}
	// A SUB's row is a line nib composed from the agent's words, so it prints whole.
	tag = regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(sent.Text)[1]
	sub, err := e.Patch(ctx, "[a.py#"+tag+"]\nSUB 5: four\n+FOUR")
	if err != nil || !strings.Contains(sub.Text, "]\n4:THREE AND A HALF\n5:FOUR\n") {
		t.Fatalf("sub: %v\n%s", err, sub.Text)
	}
}

// A kept row an edit response cuts at 120 characters shows whole in a plain read, so its note asks for no maxLineLength.
func TestAnEditResponsesCutRowNeedsOnlyAPlainRead(t *testing.T) {
	e, ctx, path := showFixture(t, "one\n"+strings.Repeat("long ", 60)+"\nthree\n")
	tag := regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(readText(t, e, ctx, path))[1]
	edited, err := e.Patch(ctx, "[a.py#"+tag+"]\nPUT 1:\n+ONE")
	if err != nil || !strings.Contains(edited.Text, "\n[line 2, chars 1-120 of 300]|long ") ||
		!strings.Contains(edited.Text, "\n[cut rows: change them in place with SUB N: OLD, or read a.py:N to see one whole]") {
		t.Fatalf("edit: %v\n%s", err, edited.Text)
	}
}

// After an edit that drew warnings, the agent's picture of the file may be off, so its next read shows every line; the one after holds again.
func TestAReadAfterAWarnedEditShowsEveryLine(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 40; i++ {
		content.WriteString(word(i) + " = 1\n")
	}
	e, ctx, path := showFixture(t, content.String())
	ctx.Markers = true
	tag := regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(readText(t, e, ctx, path))[1]
	edited, err := e.Patch(ctx, "[a.py#"+tag+"]\nPUT 20:\n+broken = (")
	if err != nil || !strings.Contains(edited.Text, "Warnings:") {
		t.Fatalf("edit: %v\n%s", err, edited.Text)
	}
	if out := readText(t, e, ctx, path); strings.Contains(out, "unchanged") || !strings.Contains(out, "\n19:"+word(19)+" = 1\n20:broken = (\n") {
		t.Fatalf("after a warned edit:\n%s", out)
	}
	if out := readText(t, e, ctx, path); !strings.Contains(out, "unchanged since you saw them") {
		t.Fatalf("the read after:\n%s", out)
	}
}

// A run an edit moved prints as map rows under its new numbers up to 100 lines, never as earlier numbers to translate.
func TestAMovedRunPrintsAsMapRows(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 60; i++ {
		content.WriteString(word(i) + " = 1\n")
	}
	e, ctx, path := showFixture(t, content.String())
	ctx.Markers = true
	tag := regexp.MustCompile(`#([0-9A-F]{4})\]`).FindStringSubmatch(readText(t, e, ctx, path))[1]
	if _, err := e.Patch(ctx, "[a.py#"+tag+"]\nPUT <1:\n+top = 0"); err != nil {
		t.Fatal(err)
	}
	out := readText(t, e, ctx, path)
	if strings.Contains(out, "your earlier") || !strings.Contains(out, "\n3~"+word(2)+" = 1\n4~"+word(3)+" = 1\n") {
		t.Fatalf("moved run:\n%s", out)
	}
}
