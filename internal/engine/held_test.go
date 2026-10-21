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
	if err != nil || !strings.Contains(edited.Text, "\n1:NEW\n") {
		t.Fatalf("the edit: %v\n%s", err, edited.Text)
	}
	// After the edit, the lines it left stand as seen before it, which the marker says of them; the changed row, seen in the response, was
	// seen after.
	out := readText(t, e, ctx, path)
	if !strings.Contains(out, "\n[lines 1-40 unchanged since you saw them, 2-40 of them before the last edit of this file]") {
		t.Fatalf("after the edit:\n%s", out)
	}
	// A later read says it again: the lines were still last seen whole before that edit, which is what the agent needs to know.
	if out := readText(t, e, ctx, path); !strings.Contains(out, "unchanged since you saw them, 2-40 of them before the last edit of this file]") {
		t.Fatalf("after the edit was seen:\n%s", out)
	}
}
