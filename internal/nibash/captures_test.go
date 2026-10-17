package nibash

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// capture is one recorded program output of the offline check, with the lines a model needs from it, and the needed log lines an arm is
// known to lose that the check would otherwise fail on.
type capture struct {
	Name    string           `json:"name"`
	File    string           `json:"file"`
	Command string           `json:"command"`
	Exit    int              `json:"exit"`
	Purpose string           `json:"purpose"`
	Count   bool             `json:"count"`
	Needs   []string         `json:"needs"`
	Loses   map[string][]int `json:"loses"`
}

// watched remembers the last choice of the decider it wraps.
type watched struct {
	decider.Decider
	last *decider.Choice
}

func (w *watched) Decide(ctx context.Context, in decider.Input) decider.Choice {
	c := w.Decider.Decide(ctx, in)
	w.last = &c
	return c
}

// watchedSummary remembers the last summary of the summarizer it wraps.
type watchedSummary struct {
	decider.Summarizer
	last *decider.Summary
}

func (w *watchedSummary) Summarize(ctx context.Context, in decider.SummaryInput) decider.Summary {
	s := w.Summarizer.Summarize(ctx, in)
	w.last = &s
	return s
}

type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the offline check makes no requests")
}

// scored is what one decider kept of one capture.
type scored struct {
	needed, kept int
	keptLines    map[int]bool
	lostLines    []int
	rows, bytes  int
	choice       *decider.Choice
}

// summaryEntry is the summarizer the offline check records and replays: the one that covered every needed line on the research set.
const summaryEntry = "deepseek/deepseek-v4.1-flash-1017"

// TestCaptures is the offline check: it renders each capture in testdata/captures through the real renderer with every decider, and with the
// OpenRouter decider and summaryEntry, and reports the needed lines and rows each keeps. The models answer from answers recorded in
// testdata/captures/decider and testdata/captures/summary; to record them again, run with NIB_DECIDER_RECORD=1 and NIB_DECIDER_KEY (this
// calls OpenRouter and costs about twenty cents).
func TestCaptures(t *testing.T) {
	data, err := os.ReadFile("testdata/captures/captures.json")
	if err != nil {
		t.Fatal(err)
	}
	var captures []capture
	if err := json.Unmarshal(data, &captures); err != nil {
		t.Fatal(err)
	}
	cache, _ := filepath.Abs("testdata/captures/decider")
	summaries, _ := filepath.Abs("testdata/captures/summary")
	model := decider.Config{Kind: decider.OpenRouter, Key: "offline", Models: []string{decider.DefaultModel}, URL: decider.DefaultURL, Timeout: 10 * time.Second, Cache: cache,
		Client: &http.Client{Transport: offline{}}}
	summary := decider.Config{Key: "offline", Summary: []decider.SummaryEntry{{Spec: summaryEntry, Model: summaryEntry, Reasoning: "off", Input: "whole"}},
		SummaryURL: decider.DefaultSummaryURL, SummaryTimeout: 10 * time.Second, Cache: summaries, Client: &http.Client{Transport: offline{}}}
	recording := os.Getenv("NIB_DECIDER_RECORD") == "1"
	if recording {
		model.Key, model.Client, model.Timeout = os.Getenv("NIB_DECIDER_KEY"), nil, 60*time.Second
		summary.Key, summary.Client, summary.SummaryTimeout = model.Key, nil, 10*time.Minute
		if model.Key == "" {
			t.Fatal("recording needs NIB_DECIDER_KEY")
		}
	}
	arms := []arm{
		{"fixed", func() decider.Decider { return nil }, nil},
		{"keywords", func() decider.Decider { return decider.Config{Kind: decider.Keywords}.New() }, nil},
		{"openrouter", func() decider.Decider { return model.New() }, nil},
		{"summary", func() decider.Decider { return model.New() }, func() decider.Summarizer { return summary.Summarizer() }},
	}
	var cost float64
	for _, c := range captures {
		t.Run(c.Name, func(t *testing.T) { cost += check(t, c, arms) })
	}
	if recording {
		t.Logf("recording cost $%.5f", cost)
	}
}

type arm struct {
	name      string
	make      func() decider.Decider
	summarize func() decider.Summarizer
}

// check scores one capture with every arm and returns what the models' requests cost.
func check(t *testing.T, c capture, arms []arm) float64 {
	cost := 0.0
	raw := readCapture(t, c.File)
	results := map[string]scored{}
	for _, arm := range arms {
		var d decider.Decider
		var w *watched
		if made := arm.make(); made != nil {
			w = &watched{Decider: made}
			d = w
		}
		var s *watchedSummary
		var summarize decider.Summarizer
		if arm.summarize != nil {
			s = &watchedSummary{Summarizer: arm.summarize()}
			summarize = s
		}
		r := score(t, c, raw, d, summarize)
		if w != nil {
			r.choice = w.last
		}
		results[arm.name] = r
		line := fmt.Sprintf("%-14s %-10s needed lines kept %3d of %3d; %3d rows, %5d bytes", c.Name, arm.name, r.kept, r.needed, r.rows, r.bytes)
		if len(r.lostLines) > 0 && len(r.lostLines) <= 3 {
			line += fmt.Sprintf("; lost log lines %v", r.lostLines)
		}
		if r.choice != nil {
			line += fmt.Sprintf("; %s", r.choice.Who)
			if r.choice.Failure != "" {
				line += " (model failed: " + r.choice.Failure + ")"
			}
			if r.choice.Judged {
				line += fmt.Sprintf("; whole %.2f", r.choice.Whole)
			}
			if u := r.choice.Usage; u.Requests+u.Cached > 0 {
				line += fmt.Sprintf("; %d requests, %d cached, %d tokens, $%.5f, %s", u.Requests, u.Cached, u.InputTokens, u.Cost, u.Elapsed.Round(time.Millisecond))
				cost += u.Cost
			}
		}
		if s != nil && s.last != nil {
			if s.last.Failure != "" {
				t.Errorf("%s: no summary (%s); record it again", c.Name, s.last.Failure)
			} else {
				u := s.last.Usage
				line += fmt.Sprintf("; summary by %s, %d ranges, %d dropped, excerpt %v; %d requests, %d cached, %d tokens, $%.5f, %s", s.last.Who, len(s.last.Ranges),
					s.last.Dropped, s.last.Excerpt, u.Requests, u.Cached, u.InputTokens, u.Cost, u.Elapsed.Round(time.Millisecond))
				cost += u.Cost
			}
		}
		t.Log(line)
	}
	if or := results["openrouter"].choice; or != nil && or.Failure != "" && or.Failure != "cancelled" {
		t.Errorf("%s: the recorded answers do not cover this capture (%s); record them again", c.Name, or.Failure)
	}
	if or := results["openrouter"].choice; or != nil {
		for _, u := range or.Unjudged {
			if !strings.HasPrefix(u.Reason, "over the") {
				t.Errorf("%s: windows not judged (%s); record them again", c.Name, u.Reason)
			}
		}
	}
	if c.Count {
		return cost
	}
	// The default never loses a needed line the fixed cut keeps, the decision model never one keywords keep, and a summary never one at all,
	// but for the losses the capture lists, each of which must still be one.
	known := func(arm string, line int) bool { return slices.Contains(c.Loses[arm], line) }
	lost := func(from, to string) {
		for line := range results[from].keptLines {
			if !results[to].keptLines[line] && !known(to, line) {
				t.Errorf("%s: %s loses needed log line %d that %s keeps", c.Name, to, line, from)
			}
		}
	}
	lost("fixed", string(decider.DefaultKind))
	lost("keywords", "openrouter")
	for _, line := range results["summary"].lostLines {
		if !known("summary", line) {
			t.Errorf("%s: the summary loses needed log line %d", c.Name, line)
		}
	}
	for arm, lines := range c.Loses {
		for _, line := range lines {
			if results[arm].keptLines[line] {
				t.Errorf("%s: %s keeps needed log line %d now; take it off the capture's losses", c.Name, arm, line)
			}
		}
	}
	return cost
}

func readCapture(t *testing.T, name string) []byte {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata/captures", name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// score renders raw as a program's output would be, with d choosing, and counts the needed log lines some shown row stands for.
func score(t *testing.T, c capture, raw []byte, d decider.Decider, summarize decider.Summarizer) scored {
	s := &sink{}
	s.Write(raw)
	rows, text, _ := s.collect(engine.New(), &engine.Context{Ctx: context.Background(), Session: "s"})
	shown, _ := fit(context.Background(), rows, text, &logDirectory{path: t.TempDir()}, decision{decider: d, summarizer: summarize, command: c.Command,
		purpose: c.Purpose, exit: c.Exit})
	// NIB_CAPTURES_SHOW=NAME prints what the summary arm shows of that capture.
	if summarize != nil && os.Getenv("NIB_CAPTURES_SHOW") == c.Name {
		var texts []string
		for _, r := range shown {
			texts = append(texts, r.text)
		}
		t.Log("\n" + strings.Join(texts, "\n"))
	}
	var patterns []*regexp.Regexp
	for _, need := range c.Needs {
		patterns = append(patterns, regexp.MustCompile(need))
	}
	r := scored{keptLines: map[int]bool{}, bytes: size(shown)}
	covered := map[int]bool{}
	for _, row := range shown {
		if row.first == 0 {
			continue
		}
		r.rows++
		for n := row.first; n <= row.last; n++ {
			covered[n] = true
		}
	}
	for n, line := range strings.Split(text, "\n") {
		plain := engine.PlainText(line)
		for _, p := range patterns {
			if p.MatchString(plain) {
				r.needed++
				if covered[n+1] {
					r.kept++
					r.keptLines[n+1] = true
				} else {
					r.lostLines = append(r.lostLines, n+1)
				}
				break
			}
		}
	}
	return r
}
