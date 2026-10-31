package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/decider"
)

func TestStatsFlagsReadSpansDatesAndLimits(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	r, err := statsFlags([]string{"--since", "7d", "--limit=3"}, now)
	if err != nil || !r.since.Equal(now.AddDate(0, 0, -7)) || r.limit != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := statsFlags([]string{"--since=12h"}, now); err != nil || !r.since.Equal(now.Add(-12*time.Hour)) || r.limit != 20 {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := statsFlags([]string{"--since", "2026-10-30"}, now); err != nil || r.since.Day() != 30 || r.since.Hour() != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range [][]string{{"--since", "soon"}, {"--limit", "-1"}, {"--since"}, {"--verbose"}} {
		if _, err := statsFlags(bad, now); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}

func TestStatsCountsCallsAndListsRefusalsAndCloseCalls(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 10, 31, 10, minute, 0, 0, time.UTC) }
	whole := 0.9
	pipe := "Do not pipe or filter the output of a zenit-dev command."
	events := []decider.Event{
		{Time: at(0), Call: decider.CallDecider, Who: "typesafe/jev-1.14", Requests: 3, InputTokens: 30000, Cost: 0.0012, Millis: 900, Windows: 20, Picked: 2},
		{Time: at(1), Call: decider.CallDecider, Who: "typesafe/jev-1.14", Requests: 1, Cost: 0.0004, Millis: 500, Windows: 4, Whole: &whole},
		{Time: at(2), Call: decider.CallDecider, Failure: "typesafe/jev-1.14 failed: OpenRouter 429; perplexity/x failed: timeout after 30s", Requests: 2},
		{Time: at(3), Call: decider.CallRules, Who: "typesafe/jev-1.14", Requests: 1, Cost: 0.00002, Rules: []string{pipe}, Scores: []float64{0.95}, Broken: []int{0},
			Command: "cat ~/.config/zenit-dev/config.json | head"},
		{Time: at(4), Call: decider.CallRules, Who: "typesafe/jev-1.14", Cached: 1, Rules: []string{pipe}, Scores: []float64{0.65}, Command: "zenit-dev status"},
		{Time: at(5), Call: decider.CallRules, Who: "typesafe/jev-1.14", Requests: 1, Rules: []string{pipe}, Scores: []float64{0.3}, Command: "ls"},
		{Time: at(6), Call: decider.CallSummary, Who: "deepseek/deepseek-v4.1-flash-1017", Requests: 1, Cost: 0.002, Dropped: 1},
		{Time: at(7), Call: decider.CallSummary, Failure: "deepseek/x: its answer is not JSON", Requests: 1},
	}
	var out bytes.Buffer
	report(&out, "/s/models.jsonl", events, statsRequest{limit: 20})
	text := out.String()
	for _, want := range []string{
		"8 model calls in /s/models.jsonl",
		"decider        3         2       0       1         6         30000     0.0016        500",
		"rules          3         3       1       0         2             0     0.0000          0",
		"summary        2         1       0       1         2             0     0.0020          0",
		"typesafe/jev-1.14                        decider 2, rules 3",
		"decider: 2 decisions picked 1.0 of 12.0 windows on average; 1 picked none; 1 judged that the purpose needs every line",
		"rules: 3 verdicts, 1 refused (33%), 1 close calls",
		"    1 refused by: " + pipe,
		"verdicts by top score: 0.3: 1, 0.6: 1, 0.9: 1",
		"summary: 1 shown, 1 failed; 1 ranges pointed outside",
		"    1  decider: typesafe/jev-1.14 failed: OpenRouter 429",
		"    1  decider: perplexity/x failed: timeout after 30s",
		"refusals, latest first:",
		"0.95  " + pipe + "\n      cat ~/.config/zenit-dev/config.json | head",
		"close calls, latest first:",
		"0.65  " + pipe + "\n      zenit-dev status",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "\n      ls\n") {
		t.Errorf("a verdict far from the threshold is no close call:\n%s", text)
	}
	out.Reset()
	report(&out, "/s/models.jsonl", events, statsRequest{since: at(30), limit: 20})
	if !strings.HasPrefix(out.String(), "no model calls in /s/models.jsonl since ") {
		t.Fatalf("%s", out.String())
	}
}
