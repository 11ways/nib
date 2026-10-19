package engine

import (
	"strings"
	"testing"
)

func TestRepeatsCollapseOnlyWhereTheMarkerIsShorterThanWhatItStandsFor(t *testing.T) {
	compressed := func(masked bool, texts ...string) string {
		t.Helper()
		lines := make([]PlainLine, len(texts))
		for i, text := range texts {
			lines[i] = PlainLine{At: i + 1, Text: text}
		}
		rows, _, _ := Compress(lines, masked)
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Text
		}
		return strings.Join(out, "\n")
	}
	check := func(got string, want ...string) {
		t.Helper()
		if w := strings.Join(want, "\n"); got != w {
			t.Errorf("got:\n%s\nwant:\n%s", got, w)
		}
	}
	// Three copies of a short line cost less than the line and its marker, so they print as they are; a long line collapses.
	check(compressed(false, "start", "dot", "dot", "dot", "end"), "start", "dot", "dot", "dot", "end")
	long := "waiting for the build to finish"
	check(compressed(false, "start", long, long, long, "end"), "start", long, "[the line above repeats 2 more times]", "end")
	// A short block that comes back prints whole, since its two markers would cost more; a stack trace gets them.
	check(compressed(false, "E", " at a", " at b", "retry", "E", " at a", " at b", "done"), "E", " at a", " at b", "retry", "E", " at a", " at b", "done")
	trace := []string{"ERROR connection closed by peer", "  at take (queue.go:42)", "  at run (worker.go:17)"}
	check(compressed(false, append(append(append([]string{}, trace...), "retry"), append(trace, "done")...)...),
		"[#1, 3 lines] "+trace[0], trace[1], trace[2], "retry", "[#1 again, 3 lines] "+trace[0], "done")
	// Runs that differ in their digits show their last copy too, so their marker pays only for long lines.
	check(compressed(true, "t1", "t2", "t3", "t4", "t5", "t6"), "t1", "t2", "t3", "t4", "t5", "t6")
	beats := []string{"12:00:01 heartbeat ok from worker", "12:00:02 heartbeat ok from worker", "12:00:03 heartbeat ok from worker", "12:00:04 heartbeat ok from worker"}
	check(compressed(true, beats...), beats[0], "[3 more like the line above; the last one:]", beats[3])
}
