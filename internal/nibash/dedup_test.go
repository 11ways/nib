package nibash

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
)

func TestDedupeNamesWhatRepeatsAndKeepsEveryLineInPlace(t *testing.T) {
	var lines []string
	add := func(text string) { lines = append(lines, text) }
	trace := []string{"java.lang.IllegalStateException: no live namespace", "    at be.example.Live.open(Live.java:12)", "    at be.example.Suite.run(Suite.java:40)"}
	add("starting the build")
	for range 500 {
		add("Downloading artifact from the repository: ok")
	}
	for i, phase := range []string{"alpha", "beta", "gamma"} {
		add(fmt.Sprintf("Gradle Test Executor %d FAILED", 10+i))
		lines = append(lines, trace...)
		add("phase " + phase)
	}
	for i := range 3 {
		add(fmt.Sprintf("Retrying the connection to the build cache, attempt %d", i))
	}
	add("done")
	entries, legend := dedupe(lines)
	var texts []string
	next := 1
	for _, e := range entries {
		if e.First != next || e.Last < e.First {
			t.Fatalf("entry %+v after line %d", e, next-1)
		}
		next = e.Last + 1
		texts = append(texts, e.Label()+": "+e.Text)
	}
	if next != len(lines)+1 {
		t.Fatalf("entries end at %d of %d lines", next-1, len(lines))
	}
	want := []string{"1: starting the build", "2-501: @L1 x500", "502: Gradle Test Executor 10 FAILED", "503: @L2", "504: @L3", "505: @L4",
		"506: phase alpha", "507: Gradle Test Executor 11 FAILED", "508: @L2", "509: @L3", "510: @L4", "511: phase beta",
		"512: Gradle Test Executor 12 FAILED", "513: @L2", "514: @L3", "515: @L4", "516: phase gamma",
		"517: Retrying the connection to the build cache, attempt 0", "518: Retrying the connection to the build cache, attempt 1",
		"519: Retrying the connection to the build cache, attempt 2", "520: done"}
	if !slices.Equal(texts, want) {
		t.Fatalf("entries:\n%s", strings.Join(texts, "\n"))
	}
	if legend["@L1"] != "Downloading artifact from the repository: ok" || legend["@L2"] != trace[0] || legend["@L4"] != trace[2] || len(legend) != 4 {
		t.Fatalf("legend %q", legend)
	}
}

func TestDedupeRunsKeepNumbersThatTellLinesApart(t *testing.T) {
	var lines []string
	for i := range 40 {
		status := "201"
		if i >= 30 {
			status = "503"
		}
		lines = append(lines, fmt.Sprintf("2026-10-13T09:%02d:%02d.%03dZ INFO  http: POST /v1/payments %s %dms", i/60, i%60, i*7%1000, status, i%9))
	}
	entries, legend := dedupe(lines)
	var texts []string
	for _, e := range entries {
		texts = append(texts, e.Label()+": "+e.Text)
	}
	if want := []string{"1-30: @L1 x30", "31-40: @L2 x10"}; !slices.Equal(texts, want) {
		t.Fatalf("entries:\n%s", strings.Join(texts, "\n"))
	}
	if legend["@L1"] != varies+lines[0] || legend["@L2"] != varies+lines[30] {
		t.Fatalf("legend %q", legend)
	}
}

func TestNovelFirstLetsARepeatedFailureWait(t *testing.T) {
	texts := map[int][]string{
		0: {"ClassNotFoundException: be.example.FirstTest", "Could not execute test class 'be.example.FirstTest'"},
		1: {"ClassNotFoundException: be.example.SecondTest", "Could not execute test class 'be.example.SecondTest'"},
		2: {"Could not record JUnit platform events", "NoSuchFileException: /build/solo.ndjson"},
	}
	picks := []decider.Pick{{Range: decider.Range{Start: 0, End: 1}, Score: 0.99}, {Range: decider.Range{Start: 1, End: 2}, Score: 0.99},
		{Range: decider.Range{Start: 2, End: 3}, Score: 0.9}}
	got := novelFirst(picks, map[string]bool{}, func(p decider.Pick) []string { return texts[p.Start] })
	if got[0].Start != 0 || got[1].Start != 2 || got[2].Start != 1 {
		t.Fatalf("order %+v", got)
	}
}

func TestDedupeKeepsShortNumberedLinesAsTheyAre(t *testing.T) {
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	entries, legend := dedupe(lines)
	if len(entries) != 50 || len(legend) != 0 || entries[9].Text != "line 10" {
		t.Fatalf("entries %+v, legend %q", entries[:3], legend)
	}
}
