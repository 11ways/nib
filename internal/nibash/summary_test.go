package nibash

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
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

func TestASummaryShowsTheLinesItPointsAtAndReadsTheRest(t *testing.T) {
	ranges := [][2]int{{500, 505}, {800, 800}, {100, 130}, {900, 910}}
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
	if rows[1] != "[nibash: log lines 1-497 left out]" || rows[2] != "line 498" || !slices.Contains(rows, "line 507") || !slices.Contains(rows, "line 802") {
		t.Fatalf("passages:\n%s", out)
	}
	// The third passage would pass summaryRows, so it is read, while the fourth still fits.
	if slices.Contains(rows, "line 110") || !slices.Contains(rows, "line 905") {
		t.Fatalf("budget:\n%s", out)
	}
	last := rows[len(rows)-1]
	for _, want := range []string{":100-130 plain:true", "of 1,000 log lines", "whole output: "} {
		if !strings.Contains(last, want) {
			t.Errorf("closing lacks %q: %s", want, last)
		}
	}
	if strings.Contains(last, "quotes") || strings.Contains(last, "dropped") {
		t.Errorf("closing names checks the agent need not read: %s", last)
	}
	for _, r := range rows {
		if gap := gapNote.FindStringSubmatch(r); gap == nil && strings.HasPrefix(r, "[nibash: log") {
			t.Errorf("gap row %q", r)
		}
	}
	// The summarizer reads the log's lines and an excerpt of the start, the end and the decider's pick widened.
	in := summary.asked[0]
	if len(in.Log) != 1000 || in.Purpose != "find the failure" || !slices.Contains(in.Excerpt, 30) || slices.Contains(in.Excerpt, 31) ||
		!slices.Contains(in.Excerpt, 621) || !slices.Contains(in.Excerpt, 641) || slices.Contains(in.Excerpt, 642) || !slices.Contains(in.Excerpt, 901) {
		t.Fatalf("asked %d lines, excerpt %v", len(in.Log), in.Excerpt)
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

func TestAnOutputWithinTheBudgetGetsASummaryPastItsOwnLimit(t *testing.T) {
	lines := func(n int) *sink {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("y", 60))
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
	if !strings.Contains(out, "<summary>Line 90 fails.</summary>") || !strings.Contains(out, "line 90 y") || strings.Contains(out, "line 150 y") {
		t.Fatalf("out:\n%s", out)
	}
	// Under both limits the output stays whole and nothing is asked.
	summary.asked = nil
	out = renderSink(t, lines(100), decision{decider: choose, summarizer: summary, exit: 1})
	if len(summary.asked) != 0 || !strings.Contains(out, "line 100 y") || strings.Contains(out, "nibash:") {
		t.Fatalf("asked %d times:\n%s", len(summary.asked), out)
	}
	// A failed summary of output within the budget leaves it whole, with no note.
	summary.summarize = func(decider.SummaryInput) decider.Summary { return decider.Summary{Failure: "m: down"} }
	out = renderSink(t, lines(200), decision{decider: choose, summarizer: summary, exit: 1})
	if len(choose.asked) != 0 || !strings.Contains(out, "line 150 y") || strings.Contains(out, "no summary") {
		t.Fatalf("decider asked %d times:\n%s", len(choose.asked), out)
	}
}

func TestWithoutASummaryTheDecidersCutStandsAndSaysWhy(t *testing.T) {
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
