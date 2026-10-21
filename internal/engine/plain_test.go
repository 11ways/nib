package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// word names line i by letters, so a plain read collapses none of them as repeats differing in a number.
func word(i int) string { return fmt.Sprintf("w%c%c", 'a'+i/26, 'a'+i%26) }

func TestLinesAPlainReadShowedWholePrintAsMapRowsWhenReadWithNumbers(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 60; i++ {
		if i == 45 {
			content.WriteString(strings.Repeat("x", 2100) + "\n")
			continue
		}
		content.WriteString(word(i) + "\n")
	}
	e, ctx, path := showFixture(t, content.String())
	ctx.Markers = true
	plain, err := e.Read(ctx, ReadArgs{Paths: []string{path + ":1-50"}, Plain: true})
	if err != nil || !strings.HasPrefix(plain.Text, "[a.py] lines 1-50 of 60, plain\nwab\nwac\n") {
		t.Fatalf("plain read: %v\n%s", err, plain.Text)
	}
	// The lines shown whole, however many, print as map rows, since the agent knows their content but never had their numbers; the line
	// shortened at 2,000 characters and the lines not read print whole.
	out := readText(t, e, ctx, path)
	for _, want := range []string{"\n1~wab\n2~wac\n", "\n40~wbo\n41~wbp\n", "\n44~wbs\n[line 45, chars 1-300 of 2100]|xxx", "\n46~wbu\n", "\n50~wby\n51:wbz\n52:wca\n", "\n60:wci\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "unchanged since you saw them") {
		t.Errorf("a marker for lines the agent had no numbers for:\n%s", out)
	}
	// Now seen with numbers, a run past the map limit prints as one marker.
	if out := readText(t, e, ctx, path); !strings.Contains(out, "\n[lines 1-44 unchanged since you saw them]\n") {
		t.Errorf("after the numbered read:\n%s", out)
	}
}

func TestAPlainReadOfAFileOverTheTextLimitRecordsNothing(t *testing.T) {
	e, ctx, path := showFixture(t, strings.Repeat("line\n", 3))
	ctx.Markers = true
	big := strings.Repeat(strings.Repeat("y", 99)+"\n", TextLimit/100+1)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Read(ctx, ReadArgs{Paths: []string{path + ":1-5"}, Plain: true}); err != nil {
		t.Fatal(err)
	}
	if version := e.Latest(ctx, path); version != nil {
		t.Fatalf("a file over the text limit was recorded: %d lines", len(version.Lines))
	}
}
