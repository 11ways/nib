package nibash

import (
	"fmt"
	"slices"
	"strings"
	"testing"
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
	want := []string{"1: starting the build", "2-501: @L1 x500", "502-505: @B1", "506: phase alpha", "507-510: @B1 11", "511: phase beta",
		"512-515: @B1 12", "516: phase gamma", "517: @L2", "518: @L2 1", "519: @L2 2", "520: done"}
	if !slices.Equal(texts, want) {
		t.Fatalf("entries:\n%s", strings.Join(texts, "\n"))
	}
	if legend["@L1"] != "Downloading artifact from the repository: ok" || legend["@B1"] != varies+"Gradle Test Executor 10 FAILED\n"+strings.Join(trace, "\n") ||
		legend["@L2"] != varies+"Retrying the connection to the build cache, attempt 0" {
		t.Fatalf("legend %q", legend)
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
