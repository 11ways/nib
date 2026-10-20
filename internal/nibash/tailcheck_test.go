package nibash

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/server"
)

// build prints its cause early, then 400 lines of noise, and fails; a tail of 5 shows only noise.
const build = "{ echo 'compiling'; echo 'error: missing symbol Foo'; seq 1 400; exit 1; } | tail -5"

// pickText is a decision model that picks the entries whose text holds want, scored 1.
func pickText(want string) *fakeDecider {
	return &fakeDecider{kind: decider.OpenRouter, choose: func(in decider.Input) decider.Choice {
		choice := decider.Choice{Who: "m and keywords", Model: true}
		for i, row := range in.Rows {
			if strings.Contains(row, want) {
				choice.Picks = append(choice.Picks, decider.Pick{Range: decider.Range{Start: i, End: i + 1}, Score: 1})
			}
		}
		return choice
	}}
}

var earlierNote = regexp.MustCompile(`\n\[397 earlier lines in [^;\]]+\]$`)

func TestATailGetsTheCauseItLeftOutAddedAfterItsOwnOutput(t *testing.T) {
	f := newFixture(t, nil)
	model := pickText("error:")
	f.shell.Deciding(model)
	out := f.call(server.ShellCall{Command: build, Description: "run the build"})
	rows := strings.Split(out, "\n")
	if !slices.Equal(rows[:5], []string{"396", "397", "398", "399", "400"}) {
		t.Fatalf("the tail's own output changed:\n%s", out)
	}
	if !regexp.MustCompile(`^\[397 earlier lines in \S+; m and keywords chose these of them for your description; N\| is a row's log line:\]$`).MatchString(rows[5]) {
		t.Fatalf("header %q", rows[5])
	}
	if !slices.Equal(rows[6:], []string{"1|compiling", "2|error: missing symbol Foo", "3|1", "4|2", "5|3"}) {
		t.Fatalf("added:\n%s", out)
	}
	// The model reads only what the tail left out, deduplicated, with the call's description.
	if len(model.asked) != 1 || model.asked[0].Purpose != "run the build" || slices.ContainsFunc(model.asked[0].Rows, func(r string) bool { return r == "400" }) {
		t.Fatalf("asked %+v", model.asked)
	}
}

func TestATailThatHoldsEverythingNeededGetsNothingAdded(t *testing.T) {
	f := newFixture(t, nil)
	model := pickText("no such line")
	f.shell.Deciding(model)
	out := f.call(server.ShellCall{Command: "seq 1 402 | tail -5", Description: "count the lines"})
	if len(model.asked) != 1 || !earlierNote.MatchString(out) || strings.Contains(out, "chose") {
		t.Fatalf("asked %d:\n%s", len(model.asked), out)
	}
}

func TestOnlyADecisionModelOrASummarizerChecksATail(t *testing.T) {
	f := newFixture(t, nil)
	f.shell.Deciding(decider.Config{Kind: decider.Keywords}.New())
	out := f.call(server.ShellCall{Command: build, Description: "run the build"})
	if strings.Contains(out, "error: missing") || !earlierNote.MatchString(out) {
		t.Fatalf("keywords added to the tail:\n%s", out)
	}
	// Without a description, a run that did not fail is not checked either: nothing says what to look for.
	model := pickText("error:")
	f.shell.Deciding(model)
	out = f.call(server.ShellCall{Command: build})
	if len(model.asked) != 0 || strings.Contains(out, "error: missing") {
		t.Fatalf("checked without a purpose:\n%s", out)
	}
}

func TestATailOfAFileHeadAndShortOutputAreNotChecked(t *testing.T) {
	var content strings.Builder
	content.WriteString("error: missing symbol Foo\n")
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&content, "%d\n", i)
	}
	f := newFixture(t, map[string]string{"build.log": content.String()})
	model := pickText("error:")
	f.shell.Deciding(model)
	for _, command := range []string{"tail -5 build.log", "seq 1 20 | tail -5", "{ echo 'error: x'; seq 1 400; } | head -3"} {
		out := f.call(server.ShellCall{Command: command, Description: "run the build"})
		if len(model.asked) != 0 || strings.Contains(out, "chose") {
			t.Fatalf("%s was checked:\n%s", command, out)
		}
	}
}

func TestManyChosenLinesTheTailLeftOutBecomeASummaryOrTheirStart(t *testing.T) {
	everything := &fakeDecider{kind: decider.OpenRouter, choose: func(in decider.Input) decider.Choice {
		return decider.Choice{Who: "m and keywords", Model: true, Picks: []decider.Pick{{Range: decider.Range{Start: 0, End: len(in.Rows)}, Score: 1}}}
	}}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Foo is missing.", Ranges: [][2]int{{2, 2}, {400, 401}}}
	}}
	f := newFixture(t, nil)
	f.shell.Deciding(everything).Summarizing(summary)
	out := f.call(server.ShellCall{Command: build, Description: "run the build"})
	rows := strings.Split(out, "\n")
	if !strings.HasSuffix(rows[5], "; a summary of the whole output, then the lines it points at that the tail left out; N| is a row's log line:]") ||
		rows[6] != "<summary>Foo is missing.</summary>" || !slices.Equal(rows[7:], []string{"1|compiling", "2|error: missing symbol Foo", "3|1"}) {
		t.Fatalf("summary:\n%s", out)
	}
	// The summarizer reads the whole log, the tail's lines too; a passage in the tail is not shown again.
	if len(summary.asked) != 1 || len(summary.asked[0].Log) != 402 || slices.Contains(rows[7:], "399|399") {
		t.Fatalf("asked %+v:\n%s", summary.asked, out)
	}

	// Without a summary the best passage shows as much of its start as fits.
	failing := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary { return decider.Summary{Failure: "OpenRouter 500"} }}
	f.shell.Summarizing(failing)
	out = f.call(server.ShellCall{Command: build, Description: "run the build"})
	rows = strings.Split(out, "\n")
	if !strings.HasSuffix(rows[5], "; m and keywords chose these of them for your description; log lines 31-397 it chose did not fit; no summary; N| is a row's log line:]") ||
		len(rows) != 6+tailAdded || rows[6] != "1|compiling" || rows[len(rows)-1] != "30|28" {
		t.Fatalf("capped:\n%s", out)
	}
}

func TestAFailedModelLeavesTheTailAsItIsAndSaysSo(t *testing.T) {
	f := newFixture(t, nil)
	f.shell.Deciding(&fakeDecider{kind: decider.OpenRouter, choose: func(decider.Input) decider.Choice {
		return decider.Choice{Who: "keywords", Failure: "OpenRouter 429", Picks: []decider.Pick{{Range: decider.Range{Start: 1, End: 2}, Score: 1}}}
	}})
	out := f.call(server.ShellCall{Command: build, Description: "run the build"})
	if !regexp.MustCompile(`\n\[397 earlier lines in \S+; not checked, since the decision model failed \(OpenRouter 429\)\]$`).MatchString(out) ||
		strings.Contains(out, "error: missing") {
		t.Fatalf("failure:\n%s", out)
	}
}
