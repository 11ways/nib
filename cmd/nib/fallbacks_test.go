package main

import (
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/fallback"
)

func TestFallbacksFlagsReadSinceLimitHostsAndJSON(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	r, err := fallbacksFlags([]string{"--since", "2d", "--limit=5", "--host", "darkshire", "--host=dalaran", "--json"}, now)
	if err != nil || !r.since.Equal(now.AddDate(0, 0, -2)) || r.limit != 5 || strings.Join(r.hosts, ",") != "darkshire,dalaran" || !r.json {
		t.Fatalf("got %+v, %v", r, err)
	}
	// The exact time a host is asked from, as the merging nib passes it.
	if r, err := fallbacksFlags([]string{"--since", "2026-11-20T10:00:00Z"}, now); err != nil || !r.since.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("an exact time: %+v, %v", r, err)
	}
	for _, args := range [][]string{{"--host"}, {"--limit", "x"}, {"--what"}} {
		if _, err := fallbacksFlags(args, now); err == nil {
			t.Errorf("%v must be refused", args)
		}
	}
}

func TestFallbacksReportCountsReasonsByKindWithTheirCommonestForms(t *testing.T) {
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	event := func(command, kind, reason, form string, count int, host string) hostedEvent {
		return hostedEvent{fallback.Event{Time: at, Command: command, Kind: kind, Reason: reason, Form: form, Count: count}, host}
	}
	events := []hostedEvent{
		event("grep", fallback.Missing, "option -P", "grep -P ... ...", 5, ""),
		event("grep", fallback.Missing, "option -P", "grep -oP ... ...", 2, "darkshire"),
		event("printf", fallback.Missing, "%(...)T", "printf %(...)T", 1, ""),
		event("printf", fallback.Error, "a %d operand bash reports as invalid or out of range", "printf %d", 4, ""),
		event("sed", fallback.Missing, "sed without -n", "sed s/.../.../ ...", 3, ""),
	}
	var out strings.Builder
	reportFallbacks(&out, []string{"/state/nib/fallbacks.jsonl", "darkshire"}, events, 1)
	text := out.String()
	for _, want := range []string{
		"fallbacks from /state/nib/fallbacks.jsonl, darkshire\n15 fallbacks, ",
		"\nmissing (forms nib's own lacks): 11\n      7  grep    option -P  [darkshire, local]\n               5  grep -P ... ...\n               2  grep -oP ... ...\n  ... 2 more reasons\n",
		"\nerror (forms bash or the system program reports as errors): 4\n      4  printf  a %d operand",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "machine") {
		t.Errorf("a kind with no events is left out:\n%s", text)
	}
	out.Reset()
	reportFallbacks(&out, []string{"x"}, nil, 10)
	if out.String() != "fallbacks from x\nnone recorded\n" {
		t.Errorf("empty: %q", out.String())
	}
}
