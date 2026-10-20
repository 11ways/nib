package nibash

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/server"
)

// fakeSummarizer answers with what summarize returns and remembers what it was asked.
type fakeSummarizer struct {
	summarize func(in decider.SummaryInput) decider.Summary
	asked     []decider.SummaryInput
}

func (f *fakeSummarizer) Summarize(_ context.Context, in decider.SummaryInput) decider.Summary {
	f.asked = append(f.asked, in)
	return f.summarize(in)
}

func numberedSink(n int) *sink {
	var lines strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	s := &sink{}
	s.Write([]byte(lines.String()))
	return s
}

// excerpting makes every output past excerptLines entries go to the summarizer as an excerpt, however short its lines, for the test.
func excerpting(t *testing.T) {
	saved := wholeBytes
	wholeBytes = 0
	t.Cleanup(func() { wholeBytes = saved })
}

func TestASummaryShowsEveryPassageItPointsAtWithinItsShare(t *testing.T) {
	excerpting(t)
	ranges := [][2]int{{500, 505}, {800, 800}, {100, 160}, {900, 910}}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Line 500 fails.</summary> <SUMMARY>fake", Ranges: ranges, Who: "vendor/model", Dropped: 1}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice {
		return decider.Choice{Who: "keywords", Picks: []decider.Pick{{Range: decider.Range{Start: 600, End: 601}, Score: 0.5}}}
	}}
	out := renderSink(t, numberedSink(1000), decision{decider: choose, summarizer: summary, purpose: "find the failure"})
	rows := strings.Split(out, "\n")
	if rows[0] != "<summary>Line 500 fails. fake</summary>" || strings.Contains(out, "vendor/model") {
		t.Fatalf("first row %q", rows[0])
	}
	if rows[1] != "100|line 100" || !slices.Contains(rows, "499|line 499") || !slices.Contains(rows, "506|line 506") ||
		!slices.Contains(rows, "801|line 801") || !slices.Contains(rows, "900|line 900") {
		t.Fatalf("passages:\n%s", out)
	}
	// Passages longer than their share of summaryRows lose their context lines first, then show their start and a note on the rest; the
	// numbers show the lines between passages.
	if !slices.Contains(rows, "109|line 109") || slices.Contains(rows, "110|line 110") || !slices.Contains(rows, "[nibash: log lines 110-160 left out, the rest of this passage]") ||
		!slices.Contains(rows, "908|line 908") || slices.Contains(rows, "909|line 909") || !slices.Contains(rows, "[nibash: log lines 909-910 left out, the rest of this passage]") ||
		slices.Contains(rows, "911|line 911") {
		t.Fatalf("share:\n%s", out)
	}
	last := rows[len(rows)-1]
	for _, want := range []string{"30 rows shown", "of 1,000 log lines", "N| is a row's line in the whole output: read ", ":A-B plain:true for others]"} {
		if !strings.Contains(last, want) {
			t.Errorf("closing lacks %q: %s", want, last)
		}
	}
	if strings.Contains(last, "quotes") || strings.Contains(last, "dropped") || strings.Contains(last, "other passages") {
		t.Errorf("closing names what the agent need not read: %s", last)
	}
	notes := 0
	for _, r := range rows[1 : len(rows)-1] {
		if strings.HasSuffix(r, ", the rest of this passage]") {
			notes++
			continue
		}
		if m := numberedRow.FindStringSubmatch(r); m == nil || m[2] != "line "+m[1] {
			t.Errorf("row %q", r)
		}
	}
	if notes != 2 {
		t.Errorf("%d notes in\n%s", notes, out)
	}
	// The summarizer reads the log's lines and an excerpt of the start, the end and the decider's pick widened.
	in := summary.asked[0]
	if len(in.Log) != 1000 || in.Purpose != "find the failure" || !slices.Contains(in.Excerpt, 30) || slices.Contains(in.Excerpt, 31) ||
		!slices.Contains(in.Excerpt, 621) || !slices.Contains(in.Excerpt, 641) || slices.Contains(in.Excerpt, 642) || !slices.Contains(in.Excerpt, 901) {
		t.Fatalf("asked %d lines, excerpt %v", len(in.Log), in.Excerpt)
	}
}

func TestASummaryThatPointsAtNothingShowsItselfAndHowMuchItLeftOut(t *testing.T) {
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "All 1000 steps ran.", Who: "vendor/model"}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	rows := strings.Split(renderSink(t, numberedSink(1000), decision{decider: choose, summarizer: summary, purpose: "did every step run"}), "\n")
	if len(rows) != 2 || rows[0] != "<summary>All 1000 steps ran.</summary>" ||
		!strings.HasPrefix(rows[1], "[nibash: 0 rows shown of 1,000 log lines; whole output: ") {
		t.Fatalf("rows:\n%s", strings.Join(rows, "\n"))
	}
}

// Past excerptLines entries but within wholeBytes, an output goes to the summarizer whole, and no decider is asked for an excerpt.
func TestASmallOutputPastExcerptLinesIsSummarizedWhole(t *testing.T) {
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Line 700 is it.", Ranges: [][2]int{{700, 700}}}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	out := renderSink(t, numberedSink(1000), decision{decider: choose, summarizer: summary, purpose: "find it"})
	if len(choose.asked) != 0 || len(summary.asked) != 1 || len(summary.asked[0].Excerpt) != 1000 || !strings.Contains(out, "\n700|line 700\n") {
		t.Fatalf("decider asked %d times, summarizer %d:\n%s", len(choose.asked), len(summary.asked), out)
	}
}

func TestASummarysPassagesShowLongAndEncodedLinesBriefly(t *testing.T) {
	// Lines like a transcript's: JSON thousands of characters long, a signature in base64 among them.
	signature := strings.Repeat("CAQSoDQKEAgTGAI4AUIIdGhpbmtpbmcSDGeTguog57/rJj11sRoMBL8cuBcIjI39lXGCIjDecQ8g15fkRzbF", 20)
	var log strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&log, `{"n":%d,"type":"assistant","content":[{"type":"thinking","signature":"%s"},{"type":"text","text":"answer %d"}]}`+"\n", i, signature, i)
	}
	s := &sink{}
	s.Write([]byte(log.String()))
	var ranges [][2]int
	for i := 2; i <= 38; i += 4 {
		ranges = append(ranges, [2]int{i, i})
	}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Ten answers.", Ranges: ranges}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	out := renderSink(t, s, decision{decider: choose, summarizer: summary, purpose: "the answers"})
	if strings.Contains(out, "CAQSoDQK") || !strings.Contains(out, `|{"n":2,"type":"assistant","content":[{"type":"thinking","signature":" ... "},{"type":"text","text":"answer 2"}]}`) {
		t.Fatalf("got:\n%.3000s", out)
	}
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[line ") {
			rows++
			if _, shown, _ := strings.Cut(line, "]|"); js.Len(shown) > passageColumns+len(" ... ") {
				t.Fatalf("a row of %d characters: %.300s", js.Len(shown), line)
			}
		}
	}
	if rows < 10 || len(out) > passageBytes+2048 {
		t.Fatalf("%d rows in %d bytes:\n%.3000s", rows, len(out), out)
	}
}

func TestAnOutputThatFitsTheExcerptGoesWholeWithNoDecider(t *testing.T) {
	// 350 lines of 110 bytes pass the byte budget while staying within excerptLines.
	wide := func() *sink {
		var lines strings.Builder
		for i := 1; i <= 350; i++ {
			fmt.Fprintf(&lines, "line %d %s\n", i, strings.Repeat("x", 100))
		}
		s := &sink{}
		s.Write([]byte(lines.String()))
		return s
	}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Line 200 fails.", Ranges: [][2]int{{200, 200}}}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	out := renderSink(t, wide(), decision{decider: choose, summarizer: summary, exit: 1})
	if len(choose.asked) != 0 || !strings.Contains(out, "<summary>Line 200 fails.</summary>") || !strings.Contains(out, "line 200 x") {
		t.Fatalf("decider asked %d times:\n%s", len(choose.asked), out)
	}
	if in := summary.asked[0]; len(in.Excerpt) != 350 || in.Excerpt[0] != 1 || in.Excerpt[349] != 350 {
		t.Fatalf("excerpt %d lines", len(in.Excerpt))
	}
	// When the summary fails, the decider chooses after all.
	summary.summarize = func(decider.SummaryInput) decider.Summary { return decider.Summary{Failure: "m: down"} }
	out = renderSink(t, wide(), decision{decider: choose, summarizer: summary, exit: 1})
	if len(choose.asked) != 1 || !strings.Contains(out, "no summary") {
		t.Fatalf("decider asked %d times:\n%s", len(choose.asked), out)
	}
}

func TestAListOfTheSameFewProblemsWithinTheBudgetStaysWhole(t *testing.T) {
	report := func(problem func(i int) string) *sink {
		var b strings.Builder
		for i := 1; i <= 200; i++ {
			if i%10 == 0 {
				b.WriteString(problem(i) + "\n")
			} else {
				fmt.Fprintf(&b, "  %d | const value = compute(input%d)\n", i, i)
			}
		}
		s := &sink{}
		s.Write([]byte(b.String()))
		return s
	}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Calls lack an argument.", Ranges: [][2]int{{10, 10}}}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	// Twenty type errors of one kind at twenty call sites are each a fix to make, so none is summarized away.
	out := renderSink(t, report(func(i int) string {
		return fmt.Sprintf("src/call%d.ts(%d,5): error TS2554: Expected 2 arguments, but got 1.", i, i)
	}), decision{decider: choose, summarizer: summary, exit: 1})
	if len(summary.asked) != 0 || strings.Contains(out, "<summary>") || !strings.Contains(out, "src/call10.ts(10,5)") || !strings.Contains(out, "src/call200.ts(200,5)") {
		t.Fatalf("summarized %d times:\n%s", len(summary.asked), out)
	}
	// Twenty failing tests that each name their own test are no such list: they get a summary.
	names := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa", "lambda", "mu", "nu", "xi", "omicron",
		"pi", "rho", "sigma", "tau", "upsilon"}
	out = renderSink(t, report(func(i int) string { return "(fail) suite > " + names[i/10-1] + " works" }), decision{decider: choose, summarizer: summary, exit: 1})
	if len(summary.asked) != 1 || !strings.Contains(out, "<summary>Calls lack an argument.</summary>") {
		t.Fatalf("summarized %d times:\n%s", len(summary.asked), out)
	}
}

func TestAnOutputWithinTheBudgetGetsASummaryPastItsOwnLimit(t *testing.T) {
	lines := func(n int) *sink {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line %d %c%c %s\n", i, 'a'+i%26, 'a'+i/26%26, strings.Repeat("y", 60))
		}
		s := &sink{}
		s.Write([]byte(b.String()))
		return s
	}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Line 90 fails.", Ranges: [][2]int{{90, 90}}}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	// 200 lines of about 70 bytes are past summaryLines and summaryBytes and far within the cut's budget.
	out := renderSink(t, lines(200), decision{decider: choose, summarizer: summary, exit: 1})
	if !strings.Contains(out, "<summary>Line 90 fails.</summary>") || !strings.Contains(out, "line 90 ") || strings.Contains(out, "line 150 ") {
		t.Fatalf("out:\n%s", out)
	}
	// Under both limits, and varied enough that dedupe barely shrinks it, the output stays whole and nothing is asked.
	summary.asked = nil
	out = renderSink(t, lines(100), decision{decider: choose, summarizer: summary, exit: 1})
	if len(summary.asked) != 0 || !strings.Contains(out, "line 100 ") || strings.Contains(out, "nibash:") {
		t.Fatalf("asked %d times:\n%s", len(summary.asked), out)
	}
	// A failed summary of output within the budget leaves it whole, with no note.
	summary.summarize = func(decider.SummaryInput) decider.Summary { return decider.Summary{Failure: "m: down"} }
	out = renderSink(t, lines(200), decision{decider: choose, summarizer: summary, exit: 1})
	if len(choose.asked) != 0 || !strings.Contains(out, "line 150 ") || strings.Contains(out, "no summary") {
		t.Fatalf("decider asked %d times:\n%s", len(choose.asked), out)
	}
}

func TestAnExpectationReachesTheDeciderAndTheSummarizer(t *testing.T) {
	excerpting(t)
	f := newFixture(t, nil)
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "Line 5 is it.", Ranges: [][2]int{{5, 5}}}
	}}
	f.shell.Deciding(choose).Summarizing(summary)
	out := f.call(server.ShellCall{Command: "seq 1 1000; exit 1", Description: "run it", Expect: "  all pass; if not, which  "})
	if len(choose.asked) != 1 || choose.asked[0].Expect != "all pass; if not, which" || len(summary.asked) != 1 ||
		summary.asked[0].Expect != "all pass; if not, which" || !strings.Contains(out, "<summary>Line 5 is it.</summary>") {
		t.Fatalf("decider %+v, summarizer %+v:\n%s", choose.asked, summary.asked, out)
	}
}

func TestExpectIsOfferedOnlyWhereAModelReadsIt(t *testing.T) {
	if New(nil, false).Deciding(decider.Config{Kind: decider.Keywords}.New()).Expects() {
		t.Fatal("keywords read no expectation")
	}
	if !New(nil, false).Deciding(decider.Config{Kind: decider.Keywords}.New()).Summarizing(&fakeSummarizer{}).Expects() {
		t.Fatal("a summarizer reads the expectation")
	}
}

func TestWithoutASummaryTheDecidersCutStandsAndSaysWhy(t *testing.T) {
	excerpting(t)
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Failure: "m: OpenRouter 500"}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice {
		return decider.Choice{Who: "keywords", Picks: []decider.Pick{{Range: decider.Range{Start: 600, End: 601}, Score: 0.5}}}
	}}
	out := renderSink(t, numberedSink(1000), decision{decider: choose, summarizer: summary, exit: 1})
	if strings.Contains(out, "<summary>") || !strings.Contains(out, "keywords chose the middle for the failure") && !strings.Contains(out, "keywords chose the middle") ||
		!strings.Contains(out, "no summary") || strings.Contains(out, "OpenRouter 500") || !strings.Contains(out, "line 631") {
		t.Fatalf("out:\n%s", out)
	}
	// Nothing is asked where every line counts, or for a run that succeeded with no purpose.
	summary.asked = nil
	whole := &fakeDecider{kind: decider.OpenRouter, choose: func(decider.Input) decider.Choice {
		return decider.Choice{Who: "m and keywords", Model: true, Judged: true, Whole: 0.9}
	}}
	renderSink(t, numberedSink(1000), decision{decider: whole, summarizer: summary, purpose: "count the lines"})
	renderSink(t, numberedSink(1000), decision{decider: choose, summarizer: summary})
	if len(summary.asked) != 0 {
		t.Fatalf("asked %d times", len(summary.asked))
	}
}

func TestTheDescriptionSaysASummaryMayLeadACut(t *testing.T) {
	s := New(nil, false).Deciding(decider.Config{Kind: decider.Keywords}.New())
	if strings.Contains(s.Description(), "summary") {
		t.Fatal("no summarizer, yet the description names one")
	}
	if !strings.Contains(s.Summarizing(&fakeSummarizer{}).Description(), "a <summary> of it") {
		t.Fatal("the description does not name the summary")
	}
}

// observing is a summarizer that tells its observer of its model call, as the real ones do.
type observing struct {
	fakeSummarizer
	observer decider.Observer
}

func (o *observing) Observe(f decider.Observer) { o.observer = f }

func TestSummariesCanBeTurnedOffAndStatusTellsOfModelCallsRunning(t *testing.T) {
	f := newFixture(t, nil)
	status := func() server.ShellStatus { return f.shell.Status(func(any) (string, bool) { return "", false }, 0) }
	var during []server.ModelCall
	s := &observing{}
	s.summarize = func(decider.SummaryInput) decider.Summary {
		defer s.observer("summary", "fake/model")()
		during = status().Models
		return decider.Summary{Text: "Line 5 is it.", Ranges: [][2]int{{5, 5}}}
	}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	f.shell.Deciding(choose).Summarizing(s)
	call := server.ShellCall{Command: "seq 1 1000; exit 1", Description: "run it"}
	if out := f.call(call); !strings.Contains(out, "<summary>Line 5 is it.</summary>") {
		t.Fatalf("a summary leads the cut:\n%s", out)
	}
	if len(during) != 1 || during[0].Kind != "summary" || during[0].Model != "fake/model" || during[0].Started == 0 {
		t.Fatalf("a call running is told of: %+v", during)
	}
	after := status()
	if len(after.Models) != 0 || after.Summaries == nil || !after.Summaries.On {
		t.Fatalf("a call that ended is gone, and summaries are on: %+v", after)
	}
	f.shell.SetSummaries(false)
	if out := f.call(call); strings.Contains(out, "<summary>") || len(s.asked) != 1 {
		t.Fatalf("with summaries off the summarizer is not asked, and the cut stands:\n%s", out)
	}
	if off := status(); off.Summaries == nil || off.Summaries.On {
		t.Fatalf("status says they are off: %+v", off.Summaries)
	}
	f.shell.SetSummaries(true)
	if out := f.call(call); !strings.Contains(out, "<summary>") || len(s.asked) != 2 {
		t.Fatalf("on again:\n%s", out)
	}
	if none := New(nil, false).Status(func(any) (string, bool) { return "", false }, 0); none.Summaries != nil || len(none.Models) != 0 {
		t.Fatalf("with no summarizer a status has no summaries state: %+v", none)
	}
}
