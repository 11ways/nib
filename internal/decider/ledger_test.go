package decider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ledgered is the events a ledger at path holds, failing the test when it cannot be read.
func ledgered(t *testing.T, path string) []Event {
	t.Helper()
	events, err := ReadLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestTheLedgerRecordsEachDecisionWithWhoAnsweredAndWhatItCost(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		if req["model"] == "bad/model" {
			return 500, "{}"
		}
		return 200, answerAll(req, func(id string) float64 {
			if id == "w4" {
				return 0.97
			}
			return 0.05
		})
	}}
	server := f.serve(t)
	path := filepath.Join(t.TempDir(), "state", LedgerFile)
	o := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{"bad/model", DefaultModel}, URL: server.URL + "/api/alpha/decisions",
		Timeout: 5 * time.Second, Ledger: path})
	rows := rowsOf(300, map[int]string{110: "error: build stopped"})
	o.Decide(context.Background(), Input{Command: "bun build", Purpose: "why the build fails", Exit: 1, Rows: rows})
	// A run without a purpose that succeeded asks no model, so it is not a model call.
	o.Decide(context.Background(), Input{Command: "ls", Rows: rows})
	events := ledgered(t, path)
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	e := events[0]
	if e.Call != CallDecider || e.Who != DefaultModel || !slices.Equal(e.Asked, []string{"bad/model", DefaultModel}) ||
		e.Failure != "bad/model failed: OpenRouter 500" || e.Command != "bun build" || e.Purpose != "why the build fails" || e.Exit != 1 {
		t.Fatalf("event %+v", e)
	}
	if e.Rows != 300 || e.Windows == 0 || e.Picked != 1 || e.Whole == nil || *e.Whole != 0.05 || e.Requests != f.count() || e.InputTokens == 0 ||
		e.Cost == 0 || e.Time.IsZero() {
		t.Fatalf("event %+v", e)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger %v %v", info, err)
	}
}

func TestTheLedgerRecordsRuleScoresAndRefusalsAndCachedAnswers(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		return 200, answerAll(req, func(id string) float64 { return map[string]float64{"r0": 0.95, "r1": 0.6}[id] })
	}}
	server := f.serve(t)
	path := filepath.Join(t.TempDir(), LedgerFile)
	c := Config{Kind: OpenRouter, Key: "key", Models: []string{DefaultModel}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second,
		Cache: t.TempDir(), Ledger: path}
	o := newOpenRouter(c)
	q := RuleQuestion{Command: "./gradlew test", Directory: "/w", Purpose: "run the tests", Rules: []string{"Do not run builds.", "Do not delete."}}
	o.Breaks(context.Background(), q)
	o.Breaks(context.Background(), q)
	events := ledgered(t, path)
	if len(events) != 2 {
		t.Fatalf("events %+v", events)
	}
	e := events[0]
	if e.Call != CallRules || e.Who != DefaultModel || e.Directory != "/w" || !slices.Equal(e.Rules, q.Rules) || !slices.Equal(e.Scores, []float64{0.95, 0.6}) ||
		!slices.Equal(e.Broken, []int{0}) || e.Requests != 1 || e.Cached != 0 {
		t.Fatalf("event %+v", e)
	}
	if again := events[1]; again.Requests != 0 || again.Cached != 1 || !slices.Equal(again.Broken, []int{0}) {
		t.Fatalf("cached event %+v", again)
	}
	// No model answering is recorded too, with why.
	down := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{DefaultModel}, URL: "http://127.0.0.1:1/x", Timeout: time.Second, Ledger: path})
	down.Breaks(context.Background(), RuleQuestion{Command: "ls", Rules: []string{"a"}})
	events = ledgered(t, path)
	if last := events[len(events)-1]; last.Who != "" || last.Failure == "" || last.Broken != nil {
		t.Fatalf("failed event %+v", last)
	}
}

func TestTheLedgerRecordsSummariesAndWhyOneFailed(t *testing.T) {
	log := logOf(300, map[int]string{120: "FAIL: TestParse expected 3, got 4"})
	f := &chatFake{answer: func(n int, req map[string]any) (int, string) {
		if req["model"] == "a/down" {
			return 500, ""
		}
		return 200, `{"summary": "TestParse fails.", "ranges": [[118, 125], [290, 299]]}`
	}}
	server := f.serve(t)
	path := filepath.Join(t.TempDir(), LedgerFile)
	entries, _ := parseSummary("a/down,c/good")
	s := Config{Key: "key", Summary: entries, SummaryURL: server.URL, SummaryTimeout: 5 * time.Second, Ledger: path}.Summarizer()
	s.Summarize(context.Background(), SummaryInput{Command: "go test", Purpose: "find the failure", Exit: 1, Log: log, Excerpt: []int{1, 2, 120, 121, 122}})
	events := ledgered(t, path)
	if len(events) != 1 {
		t.Fatalf("events %+v", events)
	}
	e := events[0]
	if e.Call != CallSummary || e.Who != "c/good" || !slices.Equal(e.Asked, []string{"a/down", "c/good"}) || !strings.HasPrefix(e.Failure, "a/down: ") ||
		e.LogLines != 300 || e.ReadLines != 5 || e.Ranges != 1 || e.Dropped != 1 || e.Chars != len("TestParse fails.") || e.Requests != 2 {
		t.Fatalf("event %+v", e)
	}
}

func TestTheLedgerIsInTheStateFolderUnlessTurnedOffOrMoved(t *testing.T) {
	cases := []struct {
		vars map[string]string
		want string
		bad  bool
	}{
		{map[string]string{"XDG_STATE_HOME": "/s"}, "/s/nib/" + LedgerFile, false},
		{map[string]string{"HOME": "/h"}, "/h/.local/state/nib/" + LedgerFile, false},
		{map[string]string{"HOME": "/h", "NIB_DECIDER_LEDGER": "off"}, "", false},
		{map[string]string{"HOME": "/h", "NIB_DECIDER_LEDGER": "/l/models.jsonl"}, "/l/models.jsonl", false},
		{map[string]string{"NIB_DECIDER_LEDGER": "relative.jsonl"}, "", true},
		{nil, "", false},
	}
	for _, c := range cases {
		got, err := Parse(env(c.vars))
		if c.bad {
			if err == nil || !strings.Contains(err.Error(), "NIB_DECIDER_LEDGER") {
				t.Errorf("%v: %v", c.vars, err)
			}
			continue
		}
		if err != nil || filepath.ToSlash(got.Ledger) != c.want {
			t.Errorf("%v: %q %v", c.vars, got.Ledger, err)
		}
	}
	// A line that is not an event, as a partly written one, is skipped.
	path := filepath.Join(t.TempDir(), LedgerFile)
	os.WriteFile(path, []byte("{\"call\":\"rules\",\"who\":\"m\"}\nnot json\n{\"time\":\"2026-10-31T00:00:00Z\"}\n"), 0o600)
	if events := ledgered(t, path); len(events) != 1 || events[0].Who != "m" {
		t.Fatalf("events %+v", events)
	}
	if events, err := ReadLedger(filepath.Join(t.TempDir(), "none.jsonl")); err != nil || events != nil {
		t.Fatalf("a missing ledger has no events: %v %v", events, err)
	}
}
