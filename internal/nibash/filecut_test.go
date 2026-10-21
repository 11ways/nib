package nibash

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/server"
)

// diffFixture is a repository with files changed in each of their functions, whose diff is 10 files of 6 hunks, past the budget.
func diffFixture(t *testing.T) *fixture {
	t.Helper()
	files := map[string]string{}
	changed := map[string]string{}
	for k := 0; k < 10; k++ {
		var before, after strings.Builder
		for n := 1; n <= 6; n++ {
			fmt.Fprintf(&before, "func f%d() {\n", n)
			fmt.Fprintf(&after, "func f%d() {\n", n)
			for i := 1; i <= 30; i++ {
				fmt.Fprintf(&before, "\tx%d_%d := %d\n", n, i, i)
				if i == 15 {
					fmt.Fprintf(&after, "\tx%d_%d := %d\n", n, i, i*10)
				} else {
					fmt.Fprintf(&after, "\tx%d_%d := %d\n", n, i, i)
				}
			}
			before.WriteString("}\n\n")
			after.WriteString("}\n\n")
		}
		name := fmt.Sprintf("%c.go", 'a'+k)
		files[name], changed[name] = before.String(), after.String()
	}
	f := gitFixture(t, files)
	for name, content := range changed {
		f.write(name, content)
	}
	return f
}

var (
	hunkHeader = regexp.MustCompile(`(?m)^@@ [-+0-9, ]+ @@ func f(\d)\(\) \{$`)
	addedRow   = regexp.MustCompile(`(?m)^\+\d+:\tx\d_15 := \d+0$`)
)

func TestALongDiffIsCutFileByFileAndHunkByHunk(t *testing.T) {
	f := diffFixture(t)
	out := f.run("git diff")
	for k := 0; k < 10; k++ {
		if n := strings.Count(out, fmt.Sprintf("[%c.go#", 'a'+k)); n != 1 {
			t.Errorf("file %c's header printed %d times", 'a'+k, n)
		}
	}
	// Every hunk shown is whole: its header, its removed line and its added line; each file shows its first three, in order.
	hunks := len(hunkHeader.FindAllString(out, -1))
	if hunks != 30 || strings.Count(out, "\n-|\tx") != hunks || len(addedRow.FindAllString(out, -1)) != hunks {
		t.Errorf("%d hunk headers, %d removed and %d added rows:\n%s", hunks, strings.Count(out, "\n-|\tx"), len(addedRow.FindAllString(out, -1)), out)
	}
	if n := strings.Count(out, "@@ func f1() {\n"); n != 10 {
		t.Errorf("f1's hunk shown %d times:\n%s", n, out)
	}
	// The note under a file's hunks names the rest with git's own names for them.
	if n := strings.Count(out, "\n[nibash: 3 hunks left out, +3 -3: func f4() {, func f5() {, func f6() {]\n"); n != 10 {
		t.Errorf("%d file notes in\n%s", n, out)
	}
	for _, want := range []string{"[nibash: 280 rows shown of 580 log lines; each file's hunks shown from its start, whole after the first, as far as its share of the budget goes; ",
		"for a file left out, run the command for its path; ", " holds the whole output as plain text, not rows]"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out[len(out)-600:])
		}
	}
	if strings.Contains(out, "log lines ") || regexp.MustCompile(`(?m)^\d+\|`).MatchString(out) {
		t.Errorf("rows numbered by log line in\n%s", out)
	}
	if len(out) > budgetBytes+2048 {
		t.Errorf("%d bytes shown", len(out))
	}
}

func TestADecisionModelChoosesADiffsHunksForWhatTheAgentExpects(t *testing.T) {
	f := diffFixture(t)
	model := &fakeDecider{kind: decider.OpenRouter, choose: func(in decider.Input) decider.Choice {
		// The removed and added rows of f5's hunk in c.go, picked without the hunk's header.
		at := -1
		for i, r := range in.Rows {
			if strings.HasPrefix(r, "[c.go#") {
				at = i
			} else if at >= 0 && strings.HasSuffix(r, "@@ func f5() {") {
				return decider.Choice{Picks: []decider.Pick{{Range: decider.Range{Start: i + 4, End: i + 6}, Score: 0.9}}, Who: "model", Model: true, Judged: true}
			}
		}
		t.Fatal("c.go's f5 hunk not among the rows")
		return decider.Choice{}
	}}
	f.shell.Deciding(model)
	out := f.call(server.ShellCall{Command: "git diff", Expect: "the change to f5 in c.go"})
	if len(model.asked) != 1 || model.asked[0].Expect != "the change to f5 in c.go" {
		t.Fatalf("asked %d times: %+v", len(model.asked), model.asked)
	}
	for _, want := range []string{"\n[c.go#", "@@ func f5() {\n", "\n-|\tx5_15 := 15\n+", ":\tx5_15 := 150\n", "\n[nibash: 6 hunks left out, +6 -6: func f1() {, func f2() {, func f3() {, ...]\n",
		"model chose for what you expect, each file within its share of the budget; for a file left out, run the command for its path; "} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if n := strings.Count(out, "\n@@ "); n != 1 {
		t.Errorf("%d hunk headers shown, want c.go's f5 only:\n%s", n, out)
	}
	if n := strings.Count(out, "[nibash: 6 hunks left out"); n != 9 {
		t.Errorf("%d files left out whole, want 9:\n%s", n, out)
	}
}

func TestADiffTheModelJudgesNeededWholeIsCutFileByFile(t *testing.T) {
	f := diffFixture(t)
	whole := &fakeDecider{kind: decider.OpenRouter, choose: func(decider.Input) decider.Choice {
		return decider.Choice{Who: "model", Model: true, Judged: true, Whole: 0.9}
	}}
	f.shell.Deciding(whole)
	out := f.call(server.ShellCall{Command: "git diff", Description: "show the changes"})
	for _, want := range []string{"model judged that your description needs every line; an expect naming what matters lets the decision model choose the rows; for a file left out, run the command for its path; ",
		"each file's hunks shown from its start, whole after the first"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out[len(out)-700:])
		}
	}
}

func TestFileRowsReachOnlyADecisionModelGivenAPurpose(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	f := newFixture(t, map[string]string{"big.txt": content.String()})
	keywords := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "fake"} }}
	f.shell.Deciding(keywords)
	out := f.call(server.ShellCall{Command: "cat big.txt", Description: "see it"})
	if len(keywords.asked) != 0 || !strings.Contains(out, "\n[nibash: 700 rows left out]\n[nibash: 301 rows shown of 1,000 log lines; each file shown from its start as far as its share of the budget goes; for a file left out, run the command for its path; ") {
		t.Fatalf("asked %d times:\n%s", len(keywords.asked), out[len(out)-600:])
	}
	model := &fakeDecider{kind: decider.OpenRouter, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "model", Model: true} }}
	f.shell.Deciding(model)
	out = f.run("cat big.txt")
	if len(model.asked) != 0 || !strings.Contains(out, "; an expect naming what matters lets the decision model choose the rows; ") {
		t.Fatalf("asked %d times without a purpose:\n%s", len(model.asked), out[len(out)-600:])
	}
	out = f.call(server.ShellCall{Command: "cat big.txt", Expect: "line 500"})
	if len(model.asked) != 1 || len(model.asked[0].Rows) != 1001 || model.asked[0].Rows[500] != "500:line 500" || !strings.Contains(out, "model found nothing in them for what you expect") {
		t.Fatalf("asked %d times with a purpose:\n%s", len(model.asked), out[len(out)-600:])
	}
}

func TestAGapNoteClipsAndDedupesHunkNamesAndCountsOneRow(t *testing.T) {
	long := "@@ -1,3 +1,3 @@ func assemble(rows []row, choice decider.Choice, log saved, d decision, notes ...string) []row {"
	rows := []row{{text: long, header: 0}, {text: "+2:a", header: 0}, {text: long, header: 0}, {text: "-|b", header: 0}, {text: "@@ -9,1 +9,1 @@ func b() {", header: 0}}
	if got := leftOutRows(rows); got != "3 hunks left out, +1 -1: func assemble(rows []row, choice decider..., func b() {, ..." {
		t.Errorf("got %q", got)
	}
	if got := leftOutRows([]row{{text: "3:x", header: 0}}); got != "1 row left out" {
		t.Errorf("got %q", got)
	}
}
