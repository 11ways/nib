package decider

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// eventLog is an Observer's record: a start and an end per model call.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) observer(kind, model string) func() {
	l.add("start " + kind + " " + model)
	return func() { l.add("end " + kind + " " + model) }
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func TestASummarizerTellsItsObserverOfEachModelCallAndNotOfCachedAnswers(t *testing.T) {
	var log eventLog
	var during []string
	f := &chatFake{}
	f.answer = func(_ int, req map[string]any) (int, string) {
		if req["model"] == "a/down" {
			during = log.all()
			return 500, ""
		}
		return 200, `{"summary": "Fine.", "ranges": [[2, 2]]}`
	}
	server := f.serve(t)
	cache := t.TempDir()
	in := SummaryInput{Command: "go test", Exit: 1, Log: logOf(10, nil), Excerpt: []int{1, 2, 3}}
	s := summarizerFor(t, server, "a/down,b/ok", cache)
	s.(Observable).Observe(log.observer)
	if got := s.Summarize(context.Background(), in); got.Failure != "" || got.Who != "b/ok" {
		t.Fatalf("%+v", got)
	}
	if !slices.Equal(during, []string{"start summary a/down"}) {
		t.Fatalf("a call is told of while it runs: %v", during)
	}
	want := []string{"start summary a/down", "end summary a/down", "start summary b/ok", "end summary b/ok"}
	if got := log.all(); !slices.Equal(got, want) {
		t.Fatalf("events %v", got)
	}
	// An answer from the cache makes no call, so nothing is told.
	again := summarizerFor(t, server, "b/ok", cache)
	again.(Observable).Observe(log.observer)
	again.Summarize(context.Background(), in)
	if got := log.all(); len(got) != len(want) {
		t.Fatalf("a cached answer was told of: %v", got)
	}
}

func TestADeciderTellsItsObserverOfEachModelItAsks(t *testing.T) {
	var log eventLog
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		if req["model"] == "bad/model" {
			return 500, `{}`
		}
		return 200, answerAll(req, func(string) float64 { return 0.1 })
	}}
	server := f.serve(t)
	o := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{"bad/model", DefaultModel}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second})
	o.Observe(log.observer)
	o.Decide(context.Background(), Input{Command: "c", Exit: 1, Rows: rowsOf(60, nil)})
	want := []string{"start decider bad/model", "end decider bad/model", "start decider " + DefaultModel, "end decider " + DefaultModel}
	if got := log.all(); !slices.Equal(got, want) {
		t.Fatalf("events %v", got)
	}
	// Without an observer nothing happens.
	quiet := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{DefaultModel}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second})
	quiet.Decide(context.Background(), Input{Command: "c", Exit: 1, Rows: rowsOf(60, nil)})
}
