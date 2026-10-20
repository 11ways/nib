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
	if rows[0] != "[build.log] 450 lines, plain" || !strings.Contains(out, "\n50|error: missing symbol Foo\n") {
		t.Fatalf("read:\n%s", out)
	}
	if !regexp.MustCompile(`\n\[` + engine.Skim + `: \d+ rows shown of 450 log lines; m and keywords chose the middle for what you expect; N\| is a row's line in the whole file: read build.log:A-B plain:true for others\]$`).MatchString(out) {
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
	if rows[0] != "[test.log] 200 lines, plain" || rows[1] != "<summary>TestFoo failed.</summary>" || !strings.Contains(out, "\n99|line 99\n100|FAIL TestFoo: want 1, got 2\n101|line 101\n["+engine.Skim+": 3 rows shown of 200 log lines; ") {
		t.Fatalf("read:\n%s", out)
	}
	if !strings.HasSuffix(out, "; N| is a row's line in the whole file: read test.log:A-B plain:true for others]") || len(summary.asked) != 1 || summary.asked[0].Purpose != "which tests failed and why" {
		t.Fatalf("asked %+v:\n%s", summary.asked, out)
	}
	// What the log gained is summarized on its own, the summarizer told which of the file's lines it reads.
	appendTo(t, filepath.Join(f.directory, "test.log"), numbered(201, 299)+"FAIL TestBar: timeout\n"+numbered(301, 400))
	summary.summarize = func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "TestBar timed out.", Ranges: [][2]int{{100, 100}}}
	}
	out = f.readLog("which tests failed and why", "test.log")
	if len(summary.asked) != 2 || summary.asked[1].Base != 200 || len(summary.asked[1].Log) != 200 ||
		!strings.HasPrefix(out, "[test.log] lines 201-400 of 400, plain: new since your last "+engine.Skim+"\n<summary>TestBar timed out.</summary>\n299|line 299\n300|FAIL TestBar: timeout\n301|line 301\n["+engine.Skim+": 3 rows shown of 200 log lines; ") {
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
	if !strings.HasPrefix(out, "[run.log] lines 15-600 of 600, plain: new since your last "+engine.Skim+"\n15|line 15\n") || !strings.Contains(out, "\n44|line 44\n") ||
		strings.Contains(out, "\n45|") || !strings.Contains(out, "\n200|error: disk full\n") || !strings.Contains(out, " rows shown of 586 log lines;") {
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
	if !strings.Contains(first, "\n50|error: missing symbol Foo\n") {
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
	if again := f.readLog("what happened at line 300", "build.log"); strings.Contains(again, "left out, which your earlier") || !strings.Contains(again, "\n1|line 1\n") {
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

// prose is lines first to last of a file named name, each about 80 bytes, with want in place of line at.
func prose(name string, last, at int, want string) string {
	var b strings.Builder
	for i := 1; i <= last; i++ {
		if i == at {
			b.WriteString(want + "\n")
			continue
		}
		fmt.Fprintf(&b, "%s line %d: a sentence of the changelog, long enough to count toward a budget\n", name, i)
	}
	return b.String()
}

// Fourteen short files are each within the limits but together far past them: one decider shapes them as one output, and a row it chose
// stands under its file's head row.
func TestManyShortFilesPastTheBudgetTogetherAreShapedAsOneOutput(t *testing.T) {
	files := map[string]string{}
	var paths []string
	for i := 1; i <= 14; i++ {
		name := fmt.Sprintf("e%02d.md", i)
		files[name] = prose(name, 61, 0, "")
		paths = append(paths, name)
	}
	files["e09.md"] = prose("e09.md", 61, 30, "RENAMED: RegistryFeed is gone")
	f := newFixture(t, files)
	model := pickText("RENAMED")
	f.shell.Deciding(model)
	out := f.readLog("what must a consuming app change", paths...)
	if len(model.asked) != 1 || model.asked[0].Command != "cat "+strings.Join(paths, " ") {
		t.Fatalf("asked %+v", model.asked)
	}
	if len(out) > budgetBytes {
		t.Fatalf("%d bytes, past the budget of %d:\n%s", len(out), budgetBytes, out)
	}
	// Each file is its head row and its 61 lines, so e09.md's head is line 497 of the joined log and its line 30 is line 527. Every head row
	// shows, and of the files' lines only the passage the model chose.
	if !strings.HasPrefix(out, "1|[e01.md] 61 lines, plain\n63|[e02.md] 61 lines, plain\n") ||
		!strings.Contains(out, "\n497|[e09.md] 61 lines, plain\n524|e09.md line 27: ") || !strings.Contains(out, "\n527|RENAMED: RegistryFeed is gone\n") ||
		!strings.Contains(out, "\n530|e09.md line 33: a sentence of the changelog, long enough to count toward a budget\n559|[e10.md] 61 lines, plain\n") {
		t.Fatalf("read:\n%s", out)
	}
	closing := regexp.MustCompile(`\n\[` + engine.Skim + `: 21 rows shown of 868 log lines; m and keywords chose for what you expect, each file within its share of the budget; N\| is a row's line in the joined files: read (\S+):A-B plain:true for others\]$`).FindStringSubmatch(out)
	if closing == nil {
		t.Fatalf("closing note:\n%s", out)
	}
	log, err := os.ReadFile(closing[1])
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(string(log), "\n"); lines[496] != "[e09.md] 61 lines, plain" || lines[526] != "RENAMED: RegistryFeed is gone" {
		t.Fatalf("log lines 497 and 527: %q, %q", lines[496], lines[526])
	}
	// The lines shown count as shown in their own file.
	again := f.readLog("is anything removed", "e09.md")
	if !strings.HasPrefix(again, "[e09.md] 61 lines, plain\n["+engine.Skim+": lines 27-33 left out, which your earlier "+engine.Skim+" showed]\n") {
		t.Fatalf("again:\n%s", again)
	}
}

// Short files within the limits apiece but past the summary's together get one summary of them all, its passage under its file's head row.
func TestShortFilesPastTheSummaryLimitsTogetherGetOneSummary(t *testing.T) {
	files := map[string]string{}
	var paths []string
	for _, name := range []string{"a.md", "b.md", "c.md", "d.md", "e.md", "f.md"} {
		files[name] = prose(name, 40, 0, "")
		paths = append(paths, name)
	}
	files["d.md"] = prose("d.md", 40, 10, "REMOVED: Models.withoutIndexWait")
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "d.md removes Models.withoutIndexWait.", Ranges: [][2]int{{134, 134}}}
	}}
	model := pickText("REMOVED")
	f := newFixture(t, files)
	f.shell.Deciding(model).Summarizing(summary)
	out := f.readLog("what was removed", paths...)
	if len(summary.asked) != 1 || len(summary.asked[0].Log) != 246 || summary.asked[0].Log[133] != "REMOVED: Models.withoutIndexWait" || len(model.asked) != 0 ||
		!slices.Equal(summary.asked[0].Heads, []int{1, 42, 83, 124, 165, 206}) {
		t.Fatalf("asked %+v, decider %+v", summary.asked, model.asked)
	}
	// d.md's head is line 124 of the joined log, as each file is its head row and its 40 lines.
	if !strings.HasPrefix(out, "<summary>d.md removes Models.withoutIndexWait.</summary>\n124|[d.md] 40 lines, plain\n133|d.md line 9: ") ||
		!strings.Contains(out, "\n134|REMOVED: Models.withoutIndexWait\n135|d.md line 11: ") || !strings.Contains(out, "N| is a row's line in the joined files: read ") {
		t.Fatalf("read:\n%s", out)
	}
}

// keywordHits is a decider that picks every row holding want, the way keyword hits come in: one row at a time, in order.
func keywordHits(want string) *fakeDecider {
	return &fakeDecider{kind: decider.Keywords, choose: func(in decider.Input) decider.Choice {
		choice := decider.Choice{Who: "keywords"}
		for i, row := range in.Rows {
			if strings.Contains(row, want) {
				choice.Picks = append(choice.Picks, decider.Pick{Range: decider.Range{Start: i, End: i + 1}, Score: 1})
			}
		}
		return choice
	}}
}

// A file whose every line is a hit cannot crowd out another file's one hit: each file's picks get a fair share of the budget.
func TestEveryJoinedFileGetsAFairShareOfTheCut(t *testing.T) {
	var big strings.Builder
	for i := 1; i <= 600; i++ {
		fmt.Fprintf(&big, "error: step %d of the long run failed again, as the steps before it did\n", i)
	}
	f := newFixture(t, map[string]string{"big.log": big.String(), "small.log": prose("small.log", 60, 30, "error: the one that matters")})
	f.shell.Deciding(keywordHits("error:"))
	out := f.readLog("what failed", "big.log", "small.log")
	// big.log's head and 600 lines come first, so small.log's head is line 602 of the joined log and its line 30 is line 632.
	if !strings.Contains(out, "\n602|[small.log] 60 lines, plain\n629|small.log line 27: ") || !strings.Contains(out, "\n632|error: the one that matters\n") ||
		!strings.Contains(out, "keywords chose, each file within its share of the budget; ") || !strings.Contains(out, " it chose did not fit (log lines ") {
		t.Fatalf("read:\n%s", out)
	}
	if len(out) > budgetBytes {
		t.Fatalf("%d bytes, past the budget", len(out))
	}
}

// Without a decider, files past the budget together each show from their start, as far as an even share of it goes.
func TestJoinedFilesWithoutADeciderEachShowTheirStart(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		files[name] = prose(name, 200, 0, "")
	}
	f := newFixture(t, files)
	out := f.readLog("anything", "a.md", "b.md", "c.md")
	for _, want := range []string{"1|[a.md] 200 lines, plain\n2|a.md line 1: ", "\n202|[b.md] 200 lines, plain\n203|b.md line 1: ", "\n403|[c.md] 200 lines, plain\n404|c.md line 1: "} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q missing:\n%s", want, out)
		}
	}
	closing := regexp.MustCompile(`\n\[` + engine.Skim + `: \d+ rows shown of 603 log lines; each file shown from its start as far as its share of the budget goes; N\| is a row's line in the joined files: read \S+:A-B plain:true for others\]$`)
	if strings.Contains(out, "line 200:") || len(out) > budgetBytes || !closing.MatchString(out) {
		t.Fatalf("read:\n%s", out)
	}
}

// A summary of joined files points at a passage in each, past what one output's passages share, each under its file's head row.
func TestASummaryOfJoinedFilesShowsAPassageOfEach(t *testing.T) {
	files := map[string]string{}
	var paths []string
	var ranges [][2]int
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("f%02d.md", i)
		files[name] = prose(name, 30, 0, "")
		paths = append(paths, name)
		// Each file is its head row and its 30 lines; the summary points at its lines 10 to 14.
		start := (i-1)*31 + 1 + 10
		ranges = append(ranges, [2]int{start, start + 4})
	}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Every file says something.", Ranges: ranges}
	}}
	f := newFixture(t, files)
	f.shell.Deciding(pickText("nothing")).Summarizing(summary)
	out := f.readLog("what does each say", paths...)
	if len(summary.asked) != 1 || len(summary.asked[0].Heads) != 12 {
		t.Fatalf("asked %+v", summary.asked)
	}
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("f%02d.md", i)
		head := (i-1)*31 + 1
		want := fmt.Sprintf("\n%d|[%s] 30 lines, plain\n%d|%s line 9: ", head, name, head+9, name)
		if !strings.Contains(out, want) || !strings.Contains(out, fmt.Sprintf("\n%d|%s line 15: ", head+15, name)) {
			t.Fatalf("%s's passage missing:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "[skim: 84 rows shown of 372 log lines; ") {
		t.Fatalf("closing:\n%s", out)
	}
}
