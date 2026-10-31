package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/11ways/nib/internal/decider"
)

// closeCall is the top score from which a verdict that let a command run is listed as a close call.
const closeCall = 0.6

// statsRequest is what nib stats is asked: events from since on (zero for all), and how many refusals and close calls to list.
type statsRequest struct {
	since time.Time
	limit int
}

// statsFlags reads --since (7d, 12h or 2026-10-30) and --limit N, each as --name VALUE or --name=VALUE.
func statsFlags(args []string, now time.Time) (statsRequest, error) {
	r := statsRequest{limit: 20}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--since" && name != "--limit" {
			return r, fmt.Errorf("unknown option %s", args[i])
		}
		if !inline {
			if i+1 == len(args) {
				return r, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if name == "--limit" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return r, fmt.Errorf("--limit takes a number, not %q", value)
			}
			r.limit = n
			continue
		}
		since, err := sinceTime(value, now)
		if err != nil {
			return r, err
		}
		r.since = since
	}
	return r, nil
}

// sinceTime reads a span back from now (a Go duration, or a number of days as 7d) or a local date.
func sinceTime(value string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(value, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(value); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since takes a span such as 7d or 12h, or a date such as 2026-10-30, not %q", value)
}

// runStats prints what the decision models and summarizers did, from the ledger every nib appends to.
func runStats(args []string) int {
	r, err := statsFlags(args, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib stats: %v\n", err)
		return 2
	}
	path := decider.DefaultLedger(os.Getenv)
	if path == "" {
		fmt.Fprintln(os.Stderr, "nib stats: NIB_DECIDER_LEDGER is off, or there is no home folder to keep the ledger in")
		return 1
	}
	events, err := decider.ReadLedger(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nib stats: %v\n", err)
		return 1
	}
	report(os.Stdout, path, events, r)
	return 0
}

// callStats are the counts of one kind of call.
type callStats struct {
	calls, answered, cached, failed, requests, tokens int
	cost                                              float64
	millis                                            []int64
}

// report writes the counts of events from r.since on, then the latest refusals and close calls.
func report(w io.Writer, path string, all []decider.Event, r statsRequest) {
	var events []decider.Event
	for _, e := range all {
		if !e.Time.Before(r.since) {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		fmt.Fprintf(w, "no model calls in %s%s\n", path, sinceNote(r.since))
		return
	}
	fmt.Fprintf(w, "%d model calls in %s, %s to %s\n\n", len(events), path, stamp(events[0].Time), stamp(events[len(events)-1].Time))
	kinds := []string{decider.CallDecider, decider.CallRules, decider.CallSummary}
	stats := map[string]*callStats{}
	for _, k := range kinds {
		stats[k] = &callStats{}
	}
	who := map[string]map[string]int{}
	failures := map[string]int{}
	for _, e := range events {
		s := stats[e.Call]
		if s == nil {
			s = &callStats{}
			stats[e.Call] = s
			kinds = append(kinds, e.Call)
		}
		s.calls++
		s.requests += e.Requests
		s.tokens += e.InputTokens
		s.cost += e.Cost
		s.millis = append(s.millis, e.Millis)
		if e.Requests == 0 && e.Cached > 0 {
			s.cached++
		}
		if e.Who != "" {
			s.answered++
			if who[e.Who] == nil {
				who[e.Who] = map[string]int{}
			}
			who[e.Who][e.Call]++
		} else {
			s.failed++
		}
		if e.Failure != "" {
			for _, f := range strings.Split(e.Failure, "; ") {
				failures[e.Call+": "+f]++
			}
		}
	}
	fmt.Fprintf(w, "%-8s %7s %9s %7s %7s %9s %13s %10s %10s\n", "call", "calls", "answered", "cached", "failed", "requests", "input tokens", "cost $", "median ms")
	total := callStats{}
	for _, k := range kinds {
		s := stats[k]
		fmt.Fprintf(w, "%-8s %7d %9d %7d %7d %9d %13d %10.4f %10d\n", k, s.calls, s.answered, s.cached, s.failed, s.requests, s.tokens, s.cost, median(s.millis))
		total.calls += s.calls
		total.cost += s.cost
	}
	fmt.Fprintf(w, "%-8s %7d %58.4f\n\n", "total", total.calls, total.cost)

	fmt.Fprintln(w, "answered by:")
	for _, name := range sortedKeys(who) {
		var parts []string
		for _, k := range kinds {
			if n := who[name][k]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", k, n))
			}
		}
		fmt.Fprintf(w, "  %-40s %s\n", name, strings.Join(parts, ", "))
	}

	var decisions, picked, windows, wholes, none int
	var refused, close []decider.Event
	ruleRefusals := map[string]int{}
	verdicts := 0
	// scores counts verdicts by their top score, a tenth wide each, so how sure the model was shows beside what it refused.
	var scores [11]int
	var summaries, dropped int
	for _, e := range events {
		switch e.Call {
		case decider.CallDecider:
			if e.Who == "" {
				continue
			}
			decisions++
			picked += e.Picked
			windows += e.Windows
			if e.Whole != nil && *e.Whole >= decider.WholeThreshold {
				wholes++
			}
			if e.Picked == 0 {
				none++
			}
		case decider.CallRules:
			if e.Who == "" {
				continue
			}
			verdicts++
			scores[int(top(e.Scores)*10)]++
			if len(e.Broken) > 0 {
				refused = append(refused, e)
				for _, i := range e.Broken {
					if i < len(e.Rules) {
						ruleRefusals[e.Rules[i]]++
					}
				}
			} else if top(e.Scores) >= closeCall {
				close = append(close, e)
			}
		case decider.CallSummary:
			if e.Who != "" {
				summaries++
				dropped += e.Dropped
			}
		}
	}
	fmt.Fprintln(w)
	if decisions > 0 {
		fmt.Fprintf(w, "decider: %d decisions picked %.1f of %.1f windows on average; %d picked none; %d judged that the purpose needs every line\n",
			decisions, float64(picked)/float64(decisions), float64(windows)/float64(decisions), none, wholes)
	}
	if verdicts > 0 {
		fmt.Fprintf(w, "rules: %d verdicts, %d refused (%.0f%%), %d close calls (a score from %.1f to below %.1f that let the command run)\n",
			verdicts, len(refused), 100*float64(len(refused))/float64(verdicts), len(close), closeCall, decider.RuleThreshold)
		for _, rule := range sortedKeys(ruleRefusals) {
			fmt.Fprintf(w, "  %5d refused by: %s\n", ruleRefusals[rule], cut(rule, 100))
		}
		var buckets []string
		for i, n := range scores {
			if n > 0 {
				buckets = append(buckets, fmt.Sprintf("%.1f: %d", min(float64(i)/10, 1), n))
			}
		}
		fmt.Fprintf(w, "  verdicts by top score: %s\n", strings.Join(buckets, ", "))
	}
	if stats[decider.CallSummary].calls > 0 {
		fmt.Fprintf(w, "summary: %d shown, %d failed; %d ranges pointed outside what the summarizer read\n", summaries, stats[decider.CallSummary].failed, dropped)
	}
	if len(failures) > 0 {
		fmt.Fprintln(w, "\nfailures:")
		reasons := sortedKeys(failures)
		slices.SortStableFunc(reasons, func(a, b string) int { return cmp.Compare(failures[b], failures[a]) })
		for _, reason := range reasons {
			fmt.Fprintf(w, "  %5d  %s\n", failures[reason], cut(reason, 140))
		}
	}
	listVerdicts(w, "refusals", refused, r.limit, true)
	listVerdicts(w, "close calls", close, r.limit, false)
}

// listVerdicts lists up to limit rule verdicts, latest first, each with the score of the rule that refused it or came closest.
func listVerdicts(w io.Writer, title string, events []decider.Event, limit int, broken bool) {
	if len(events) == 0 || limit == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s, latest first:\n", title)
	for n, i := 0, len(events)-1; i >= 0 && n < limit; i, n = i-1, n+1 {
		e := events[i]
		best := 0
		for j, s := range e.Scores {
			if s > e.Scores[best] {
				best = j
			}
		}
		if broken && len(e.Broken) > 0 {
			best = e.Broken[0]
		}
		score, rule := 0.0, ""
		if best < len(e.Scores) && best < len(e.Rules) {
			score, rule = e.Scores[best], e.Rules[best]
		}
		fmt.Fprintf(w, "  %s  %.2f  %s\n      %s\n", stamp(e.Time), score, cut(rule, 80), cut(strings.Join(strings.Fields(e.Command), " "), 160))
	}
	if len(events) > limit {
		fmt.Fprintf(w, "  %d more; --limit N lists more\n", len(events)-limit)
	}
}

func top(scores []float64) float64 {
	best := 0.0
	for _, s := range scores {
		best = max(best, s)
	}
	return best
}

func median(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func sinceNote(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return " since " + stamp(since)
}

// cut shortens text to n characters, marking the cut.
func cut(text string, n int) string {
	if r := []rune(text); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return text
}
