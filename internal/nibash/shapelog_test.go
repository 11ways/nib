package nibash

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/server"
)

// failedBuild is a log whose cause sits early, before 400 lines of progress, as a build wrapper keeps it.
func failedBuild() string {
	var b strings.Builder
	b.WriteString("> Task :compileJava\nerror: cannot find symbol Foo\n")
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&b, "progress %d\n", i)
	}
	b.WriteString("BUILD FAILED\n")
	return b.String()
}

func TestAShortLogIsShapedWhole(t *testing.T) {
	model := pickText("error:")
	out := ShapeLog(context.Background(), []byte("one\ntwo\n"), "/logs/a.log", "cat /logs/a.log", "run it", 1, model, nil)
	if out != "one\ntwo" || len(model.asked) != 0 {
		t.Fatalf("asked %d:\n%s", len(model.asked), out)
	}
	if out := ShapeLog(context.Background(), nil, "/logs/a.log", "", "", 0, model, nil); out != "(no output)" {
		t.Fatalf("empty log: %q", out)
	}
}

func TestALongFailedLogGetsTheDecidersPicksAndNamesTheLogAsTheWholeOutput(t *testing.T) {
	model := pickText("error:")
	out := ShapeLog(context.Background(), []byte(failedBuild()), "/logs/build.log", "zenit-dev test", "run the tests", 1, model, nil)
	if !strings.Contains(out, "\nerror: cannot find symbol Foo\n") || !strings.Contains(out, "BUILD FAILED") || strings.Contains(out, "[exit") {
		t.Fatalf("shaped:\n%s", out)
	}
	if !regexp.MustCompile(`(?m)^\[nibash: \d+ rows shown of 403 log lines; .*whole output: /logs/build\.log\]$`).MatchString(out) {
		t.Fatalf("closing:\n%s", out)
	}
	if in := model.asked[0]; in.Purpose != "run the tests" || in.Command != "zenit-dev test" || in.Exit != 1 {
		t.Fatalf("asked %+v", in)
	}
}

func TestALongLogGetsASummaryAndFallsBackToTheDeciderWhenTheSummaryFails(t *testing.T) {
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Foo is missing.", Ranges: [][2]int{{2, 2}}, Who: "vendor/model"}
	}}
	model := pickText("BUILD")
	out := ShapeLog(context.Background(), []byte(failedBuild()), "/logs/build.log", "", "why did it fail", 1, model, summary)
	if !strings.HasPrefix(out, "<summary>Foo is missing.</summary>\n") || !strings.Contains(out, "error: cannot find symbol Foo") {
		t.Fatalf("summary:\n%s", out)
	}
	summary.summarize = func(decider.SummaryInput) decider.Summary { return decider.Summary{Failure: "m: down"} }
	out = ShapeLog(context.Background(), []byte(failedBuild()), "/logs/build.log", "", "why did it fail", 1, model, summary)
	if strings.Contains(out, "<summary>") || !strings.Contains(out, "no summary") || !strings.Contains(out, "BUILD FAILED") {
		t.Fatalf("fallback:\n%s", out)
	}
}

// A log shaped by nib shape reads as nibash's own output of the same run, but for the log its notes name.
func TestShapeLogIsWhatNibashShowsOfTheSameOutput(t *testing.T) {
	log := failedBuild()
	f := newFixture(t, map[string]string{"build.log": log})
	model := pickText("error:")
	f.shell.Deciding(model)
	command := "cat build.log; exit 3"
	shown := f.call(server.ShellCall{Command: command, Description: "run the build"})
	shown = strings.TrimSuffix(shown, "\n[exit 3]")
	saved := regexp.MustCompile(`whole output: ([^\]\s]+)\]`).FindStringSubmatch(shown)
	if saved == nil {
		t.Fatalf("nibash cut nothing:\n%s", shown)
	}
	path := filepath.Join(f.directory, "build.log")
	want := strings.ReplaceAll(shown, saved[1], path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := ShapeLog(context.Background(), raw, path, command, "run the build", 3, model, nil); got != want {
		t.Fatalf("nib shape:\n%s\n\nnibash:\n%s", got, want)
	}
}

// A request through the session's socket is shaped as nib shape shapes it alone, shows in the band while it runs, and a later skim of the
// log by any agent leaves out what it showed, naming those lines and how to read them, as no agent is known to have them.
func TestShapeThroughTheServerIsNibShapesViewAndTheSkimLeavesOutWhatItShowed(t *testing.T) {
	f := newFixture(t, map[string]string{"build.log": failedBuild()})
	path := filepath.Join(f.directory, "build.log")
	var during []server.ModelCall
	model := pickText("error:")
	choose := model.choose
	model.choose = func(in decider.Input) decider.Choice {
		during = f.shell.Status(func(any) (string, bool) { return "", false }, 0).Models
		return choose(in)
	}
	f.shell.Deciding(model)
	got, err := f.shell.Shape(context.Background(), path, "zenit-dev test", "run the tests", 1, f.tools())
	if err != nil {
		t.Fatal(err)
	}
	want := ShapeLog(context.Background(), []byte(failedBuild()), path, "zenit-dev test", "run the tests", 1, pickText("error:"), nil)
	if got != want {
		t.Fatalf("through the server:\n%s\n\nalone:\n%s", got, want)
	}
	if len(during) != 1 || during[0].Kind != "shape" || during[0].Model != "build.log" {
		t.Fatalf("the band saw %+v while the log was shaped", during)
	}
	if calls := f.shell.Status(func(any) (string, bool) { return "", false }, 0).Models; len(calls) != 0 {
		t.Fatalf("still running after it ended: %+v", calls)
	}
	appendTo(t, path, "retry 1\nretry 2\nBUILD FAILED again\n")
	out := f.readLog("did the retry pass", "build.log")
	rows := strings.Split(out, "\n")
	if rows[0] != "[build.log] 406 lines, plain" || !regexp.MustCompile(`^\[skim: lines 1-\d+, \d+-403 left out, which a tool's result showed through nib shape at \d\d:\d\d; read build\.log:1-\d+,\d+-403 if you lack them\]$`).MatchString(rows[1]) ||
		!strings.HasSuffix(out, "\nretry 1\nretry 2\nBUILD FAILED again") || slices.Contains(rows, "error: cannot find symbol Foo") {
		t.Fatalf("skim after nib shape:\n%s", out)
	}
	if out := f.readLog("did the retry pass", "build.log"); out != "[build.log] 406 lines, plain: nothing new since your last skim with this expect" {
		t.Fatalf("skim again:\n%s", out)
	}
}

func TestShapeThroughTheServerFollowsItsSummarySwitchAndSandbox(t *testing.T) {
	f := newFixture(t, map[string]string{"build.log": failedBuild()})
	path := filepath.Join(f.directory, "build.log")
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Foo is missing.", Ranges: [][2]int{{2, 2}}}
	}}
	f.shell.Deciding(pickText("error:")).Summarizing(summary)
	f.shell.SetSummaries(false)
	out, err := f.shell.Shape(context.Background(), path, "", "why did it fail", 1, f.tools())
	if err != nil || strings.Contains(out, "<summary>") || len(summary.asked) != 0 {
		t.Fatalf("summaries off, asked %d, %v:\n%s", len(summary.asked), err, out)
	}
	f.shell.SetSummaries(true)
	if out, _ := f.shell.Shape(context.Background(), path, "", "why did it fail", 1, f.tools()); !strings.HasPrefix(out, "<summary>Foo is missing.</summary>") {
		t.Fatalf("summaries on:\n%s", out)
	}
	f.sandbox = true
	outside := filepath.Join(t.TempDir(), "other.log")
	if err := os.WriteFile(outside, []byte(failedBuild()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.shell.Shape(context.Background(), path, "", "why", 1, f.tools()); err != nil {
		t.Fatalf("a log inside the roots under the sandbox: %v", err)
	}
	if out, err := f.shell.Shape(context.Background(), outside, "", "why", 1, f.tools()); err == nil {
		t.Fatalf("a log outside the roots was shaped:\n%s", out)
	}
}
