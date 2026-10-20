package nibash

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
)

// readLog reads paths with expect as the skim tool does, for the fixture's agent.
func (f *fixture) readLog(expect string, paths ...string) string {
	f.t.Helper()
	result, err := f.shell.ReadLog(context.Background(), "session", paths, expect, f.tools())
	if err != nil {
		f.t.Fatalf("%v: %v", paths, err)
	}
	return result.Text
}

// numbered is lines first to last as "line N", each on a line of its own.
func numbered(first, last int) string {
	var b strings.Builder
	for i := first; i <= last; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestALongLogReadWithExpectGetsTheLinesTheModelChoseForIt(t *testing.T) {
	f := newFixture(t, map[string]string{"build.log": numbered(1, 49) + "error: missing symbol Foo\n" + numbered(51, 450)})
	model := pickText("error:")
	f.shell.Deciding(model)
	out := f.readLog("did the build pass", "build.log")
	rows := strings.Split(out, "\n")
	if rows[0] != "[build.log] 450 lines, plain" || !strings.Contains(out, "\nerror: missing symbol Foo\n") {
		t.Fatalf("read:\n%s", out)
	}
	if !regexp.MustCompile(`\n\[` + engine.Skim + `: \d+ rows shown of 450 log lines; m and keywords chose the middle for what you expect; whole file: build.log\]$`).MatchString(out) {
		t.Fatalf("closing note:\n%s", out)
	}
	if len(model.asked) != 1 || model.asked[0].Purpose != "did the build pass" || model.asked[0].Command != "cat build.log" {
		t.Fatalf("asked %+v", model.asked)
	}
}

func TestALogPastTheSummaryLimitsLeadsWithASummaryOfIt(t *testing.T) {
	f := newFixture(t, map[string]string{"test.log": numbered(1, 99) + "FAIL TestFoo: want 1, got 2\n" + numbered(101, 200)})
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "TestFoo failed.", Ranges: [][2]int{{100, 100}}}
	}}
	f.shell.Deciding(pickText("FAIL")).Summarizing(summary)
	out := f.readLog("which tests failed and why", "test.log")
	rows := strings.Split(out, "\n")
	if rows[0] != "[test.log] 200 lines, plain" || rows[1] != "<summary>TestFoo failed.</summary>" || !strings.Contains(out, "\nline 99\nFAIL TestFoo: want 1, got 2\nline 101\n[skim: log lines 102-200 left out]\n") {
		t.Fatalf("read:\n%s", out)
	}
	if !strings.HasSuffix(out, "; whole file: test.log]") || len(summary.asked) != 1 || summary.asked[0].Purpose != "which tests failed and why" {
		t.Fatalf("asked %+v:\n%s", summary.asked, out)
	}
	// What the log gained is summarized on its own, the summarizer told which of the file's lines it reads.
	appendTo(t, filepath.Join(f.directory, "test.log"), numbered(201, 299)+"FAIL TestBar: timeout\n"+numbered(301, 400))
	summary.summarize = func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "TestBar timed out.", Ranges: [][2]int{{100, 100}}}
	}
	out = f.readLog("which tests failed and why", "test.log")
	if len(summary.asked) != 2 || summary.asked[1].Base != 200 || len(summary.asked[1].Log) != 200 ||
		!strings.HasPrefix(out, "[test.log] lines 201-400 of 400, plain: new since your last "+engine.Skim+"\n<summary>TestBar timed out.</summary>\n["+engine.Skim+": log lines 201-298 left out]\nline 299\nFAIL TestBar: timeout\nline 301\n["+engine.Skim+": log lines 302-400 left out]\n") {
		t.Fatalf("asked %+v:\n%s", summary.asked, out)
	}
}

func TestAShortLogIsJustItsLines(t *testing.T) {
	f := newFixture(t, map[string]string{"a.log": "one\n\x1b[31mtwo\x1b[0m\nthree\n"})
	model := pickText("two")
	f.shell.Deciding(model)
	expect(t, f.readLog("anything wrong", "a.log"), "[a.log] 3 lines, plain\none\ntwo\nthree")
	if len(model.asked) != 0 {
		t.Fatalf("a short log asks no model: %+v", model.asked)
	}
}

func TestASkimCollapsesARunOnlyWhereItsMarkerIsShorter(t *testing.T) {
	f := newFixture(t, map[string]string{"a.log": numbered(1, 21) + "ok\nok\nok\n" + strings.Repeat("waiting for the build to finish\n", 3)})
	f.shell.Deciding(pickText("ok"))
	got := f.readLog("did it finish", "a.log")
	if !strings.HasSuffix(got, "\nline 21\nok\nok\nok\nwaiting for the build to finish\n[the line above repeats 2 more times]") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestAGrowingLogShowsOnlyWhatWasAddedSinceTheLastRead(t *testing.T) {
	f := newFixture(t, map[string]string{"run.log": numbered(1, 10)})
	path := filepath.Join(f.directory, "run.log")
	if out := f.readLog("progress", "run.log"); !strings.HasPrefix(out, "[run.log] 10 lines, plain\nline 1\n") {
		t.Fatalf("first read:\n%s", out)
	}
	appendTo(t, path, numbered(11, 12)+"half a li")
	expect(t, f.readLog("progress", "run.log"), "[run.log] lines 11-13 of 13, plain: new since your last "+engine.Skim+"\nline 11\nline 12\nhalf a li")
	expect(t, f.readLog("progress", "run.log"), "[run.log] 13 lines, plain: nothing new since your last "+engine.Skim+" with this expect")
	appendTo(t, path, "ne\n"+strings.Repeat("x", 2100)+"\n")
	out := f.readLog("progress", "run.log")
	if !strings.HasPrefix(out, "[run.log] lines 13-14 of 14, plain: new since your last "+engine.Skim+"\nhalf a line\n[line 14, chars 1-2000 of 2100]|xxx") ||
		!strings.HasSuffix(out, "[cut rows: N is the row's line in the whole file; read run.log:N with maxLineLength to see one whole]") {
		t.Fatalf("a finished line and a cut row number by the file:\n%s", out)
	}
	// A long addition is shaped as a log of its own, every note naming the file's lines.
	model := pickText("error:")
	f.shell.Deciding(model)
	appendTo(t, path, numbered(15, 199)+"error: disk full\n"+numbered(201, 600))
	out = f.readLog("progress", "run.log")
	if !strings.HasPrefix(out, "[run.log] lines 15-600 of 600, plain: new since your last "+engine.Skim+"\nline 15\n") || !strings.Contains(out, "\n["+engine.Skim+": log lines 45-") ||
		!strings.Contains(out, "\nerror: disk full\n") || !strings.Contains(out, " rows shown of 586 log lines;") {
		t.Fatalf("long addition:\n%s", out)
	}
	if rows := model.asked[0].Rows; len(rows) == 0 || rows[0] != "line 45" {
		t.Fatalf("the model reads only the new lines: %+v", model.asked)
	}
	// A selector reads those lines, and leaves where the next read goes on alone.
	expect(t, f.readLog("progress", "run.log:2-3"), "[run.log] lines 2-3 of 600, plain\nline 2\nline 3")
	expect(t, f.readLog("progress", "run.log"), "[run.log] 600 lines, plain: nothing new since your last "+engine.Skim+" with this expect")
}

// Another question reads the file whole again, but no line the agent was shown before: those are named, and the budget goes to the rest.
func TestASkimForAnotherQuestionShowsOnlyLinesTheAgentWasNotShown(t *testing.T) {
	f := newFixture(t, map[string]string{"build.log": numbered(1, 49) + "error: missing symbol Foo\n" + numbered(51, 450)})
	f.shell.Deciding(pickText("error:"))
	first := f.readLog("did the build pass", "build.log")
	if !strings.Contains(first, "\nerror: missing symbol Foo\n") {
		t.Fatalf("first read:\n%s", first)
	}
	shown := map[string]bool{}
	for _, row := range strings.Split(first, "\n")[1:] {
		if !strings.HasPrefix(row, "[") {
			shown[row] = true
		}
	}
	f.shell.Deciding(pickText("line 300"))
	second := f.readLog("what happened at line 300", "build.log")
	rows := strings.Split(second, "\n")
	if rows[0] != "[build.log] 450 lines, plain" || !strings.HasPrefix(rows[1], "["+engine.Skim+": lines 1-") || !strings.HasSuffix(rows[1], " left out, which your earlier "+engine.Skim+" showed]") ||
		!strings.Contains(second, "\nline 300\n") {
		t.Fatalf("second read:\n%s", second)
	}
	for _, row := range rows[2:] {
		if shown[row] {
			t.Fatalf("%q was shown again:\n%s", row, second)
		}
	}
	expect(t, f.readLog("What happened at  line 300", "build.log"), "[build.log] 450 lines, plain: nothing new since your last "+engine.Skim+" with this expect")
	// Once the agent's context is cleared, it holds none of those lines.
	f.engine.Store.Forget("s", time.Now())
	if again := f.readLog("what happened at line 300", "build.log"); strings.Contains(again, "left out, which your earlier") || !strings.Contains(again, "\nline 1\n") {
		t.Fatalf("after a clear:\n%s", again)
	}
}

func TestASkimFoldsWhatAnotherQuestionShowedOfAShortLog(t *testing.T) {
	f := newFixture(t, map[string]string{"a.log": numbered(1, 3)})
	f.readLog("anything wrong", "a.log")
	expect(t, f.readLog("did it finish", "a.log"), "[a.log] 3 lines, plain\n["+engine.Skim+": lines 1-3 left out, which your earlier "+engine.Skim+" showed]")
	// A selector asks for lines again, and counts them shown.
	expect(t, f.readLog("x", "a.log:2"), "[a.log] lines 2 of 3, plain\nline 2")
}

func TestALogRewrittenSinceTheLastReadIsReadWhole(t *testing.T) {
	f := newFixture(t, map[string]string{"run.log": numbered(1, 5)})
	f.readLog("progress", "run.log")
	if err := os.WriteFile(filepath.Join(f.directory, "run.log"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.readLog("progress", "run.log"), "[run.log] 1 lines, plain: rewritten since your last "+engine.Skim+", so all of it\nfresh")
	// Same size or larger but another start counts as rewritten too.
	if err := os.WriteFile(filepath.Join(f.directory, "run.log"), []byte("again\nand more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, f.readLog("progress", "run.log"), "[run.log] 2 lines, plain: rewritten since your last "+engine.Skim+", so all of it\nagain\nand more")
}

func TestSeveralLogsReadInOneCallAndAMissingOneIsNotedInItsPlace(t *testing.T) {
	f := newFixture(t, map[string]string{"a.log": "a\n", "b.log": "b\n"})
	expect(t, f.readLog("x", "a.log", "gone.log", "b.log"), "[a.log] 1 lines, plain\na\n\n[gone.log does not exist]\n\n[b.log] 1 lines, plain\nb")
	if _, err := f.shell.ReadLog(context.Background(), "session", []string{"gone.log"}, "x", f.tools()); err == nil || !strings.Contains(err.Error(), "gone.log does not exist") {
		t.Fatalf("all missing must fail, got %v", err)
	}
}
