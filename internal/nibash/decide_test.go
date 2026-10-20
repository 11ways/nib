package nibash

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/server"
)

// fakeDecider answers with what choose returns and remembers what it was asked.
type fakeDecider struct {
	kind   decider.Kind
	choose func(in decider.Input) decider.Choice
	asked  []decider.Input
}

func (f *fakeDecider) Kind() decider.Kind { return f.kind }

func (f *fakeDecider) Decide(_ context.Context, in decider.Input) decider.Choice {
	f.asked = append(f.asked, in)
	return f.choose(in)
}

var gapNote = regexp.MustCompile(`^\[nibash: (?:.*?; )?log lines? (\d+)(?:-(\d+))? left out[];]`)

// logOf is the log a cut output names.
func logOf(t *testing.T, out string) []string {
	t.Helper()
	at := strings.LastIndex(out, "whole output: ")
	if at < 0 {
		t.Fatalf("no log in %s", out)
	}
	path := out[at+len("whole output: "):]
	path = path[:strings.Index(path, "]")]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// sedLines is sed -n A,Bp on the log's lines.
func sedLines(log []string, a, b int) []string { return log[a-1 : b] }

func renderSink(t *testing.T, s *sink, d decision) string {
	t.Helper()
	logs := &logDirectory{path: t.TempDir()}
	ctx := &engine.Context{Ctx: context.Background(), Directory: t.TempDir(), Session: "s"}
	return s.render(engine.New(), ctx, logs, d)
}

// mixedSink holds output where rows and log lines differ: a binary chunk with line breaks of its own, grep's separator, bytes without a final
// line break before a record, and a collapsed run, then numbered lines past the budget.
func mixedSink() *sink {
	s := &sink{}
	var head strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&head, "head %d\n", i)
	}
	s.Write([]byte(head.String()))
	s.add(record{text: "record one"})
	s.Write([]byte("bin\x00ary\nmore\nlines\n"))
	s.add(record{text: "--", separator: true})
	s.add(record{text: "record two"})
	s.Write([]byte("unterminated"))
	s.add(record{text: "record three"})
	s.Write([]byte(strings.Repeat("same\n", 30)))
	var body strings.Builder
	for i := 1; i <= 700; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	s.Write([]byte(body.String()))
	return s
}

func TestEveryRowStandsForItsOwnLogLines(t *testing.T) {
	rows, raw, _, _ := mixedSink().collect(engine.New(), &engine.Context{Ctx: context.Background(), Session: "s"})
	log := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	covered := make([]int, len(log)+1)
	previous := 0
	for _, r := range rows {
		if r.first <= previous || r.last < r.first {
			t.Fatalf("row %q stands for lines %d-%d after %d", r.text, r.first, r.last, previous)
		}
		previous = r.last
		for n := r.first; n <= r.last; n++ {
			covered[n]++
		}
		if !strings.HasPrefix(r.text, "[") && r.first == r.last && log[r.first-1] != r.text {
			t.Errorf("row %q is log line %d: %q", r.text, r.first, log[r.first-1])
		}
	}
	for n := 1; n <= len(log); n++ {
		if covered[n] != 1 && log[n-1] != "--" {
			t.Errorf("log line %d %q is covered %d times", n, log[n-1], covered[n])
		}
	}
	// The record after bytes without a line break starts a line of its own in the log, as it does in the rows.
	if !strings.Contains(raw, "unterminated\nrecord three\n") {
		t.Fatalf("log: %q", raw)
	}
}

func TestCutNotesCiteTheLogLinesLeftOut(t *testing.T) {
	out := renderSink(t, mixedSink(), decision{})
	shown := strings.Split(out, "\n")
	log := logOf(t, out)
	var gap []string
	for i, row := range shown {
		m := gapNote.FindStringSubmatch(row)
		if m == nil {
			continue
		}
		a, _ := strconv.Atoi(m[1])
		b := a
		if m[2] != "" {
			b, _ = strconv.Atoi(m[2])
		}
		gap = sedLines(log, a, b)
		// The rows on either side are the log lines on either side of the gap.
		if shown[i-1] != log[a-2] || shown[i+1] != log[b] {
			t.Fatalf("gap %d-%d between %q and %q; log has %q and %q", a, b, shown[i-1], shown[i+1], log[a-2], log[b])
		}
	}
	if len(gap) == 0 || gap[0] != "line 54" || gap[len(gap)-1] != "line 500" {
		t.Fatalf("gap %v in\n%s", gap, out)
	}
}

func TestDecidedCutsKeepPicksAndAnnounceEveryGap(t *testing.T) {
	var lines strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	fake := &fakeDecider{kind: decider.Keywords, choose: func(in decider.Input) decider.Choice {
		return decider.Choice{Who: "fake", Model: true, Picks: []decider.Pick{
			{Range: decider.Range{Start: 10, End: 11}, Score: 0.9},
			{Range: decider.Range{Start: 400, End: 425}, Score: 0.6},
			{Range: decider.Range{Start: 500, End: 501}, Score: 0.4},
		}}
	}}
	s := &sink{}
	s.Write([]byte(lines.String()))
	out := renderSink(t, s, decision{decider: fake, command: "make", purpose: "why", exit: 2})
	if len(fake.asked) != 1 || len(fake.asked[0].Rows) != 870 || fake.asked[0].Rows[0] != "line 31" || fake.asked[0].Purpose != "why" || fake.asked[0].Exit != 2 {
		t.Fatalf("asked %+v", fake.asked)
	}
	rows := strings.Split(out, "\n")
	pairs := [][2]string{{"line 30", "[nibash: log lines 31-37 left out]"}, {"[nibash: log lines 31-37 left out]", "line 38"}, {"line 44", "[nibash: log lines 45-427 left out]"},
		{"[nibash: log lines 45-427 left out]", "line 428"}, {"line 458", "[nibash: log lines 459-900 left out]"}, {"[nibash: log lines 459-900 left out]", "line 901"}}
	for _, pair := range pairs {
		if !strings.Contains(out, "\n"+pair[0]+"\n"+pair[1]+"\n") {
			t.Fatalf("%q is not followed by %q in\n%s", pair[0], pair[1], out)
		}
	}
	last := rows[len(rows)-1]
	if !strings.HasPrefix(last, "[nibash: 168 rows shown of 1,000 log lines; fake chose the middle for your description; whole output: ") {
		t.Fatalf("closing note %q", last)
	}
	// The gaps hold exactly the left-out lines.
	log := logOf(t, out)
	if got := sedLines(log, 45, 427); got[0] != "line 45" || got[len(got)-1] != "line 427" {
		t.Fatalf("gap %q ... %q", got[0], got[len(got)-1])
	}
}

func TestDecidedCutNotes(t *testing.T) {
	var lines strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&lines, "line %d\n", i)
	}
	cases := []struct {
		name   string
		choice decider.Choice
		d      decision
		want   []string
	}{
		{"nothing found falls back to the fixed cut", decider.Choice{Who: "keywords"}, decision{},
			[]string{"[nibash: 300 rows shown of 1,000 log lines; log lines 101-800 left out; keywords found nothing in them; whole output: "}},
		{"a model without a description chooses for the failure", decider.Choice{Who: "m and keywords", Model: true}, decision{exit: 1},
			[]string{"; m and keywords found nothing in them for the failure; "}},
		{"a purpose that needs every line keeps the fixed cut", decider.Choice{Who: "m and keywords", Model: true, Judged: true, Whole: 0.8,
			Picks: []decider.Pick{{Range: decider.Range{Start: 5, End: 6}, Score: 1}}}, decision{purpose: "count"},
			[]string{"; log lines 101-800 left out; m and keywords judged that your description needs every line, so filter or count the whole output instead; whole output: "}},
		{"a failed model leaves keywords to choose", decider.Choice{Who: "keywords", Failure: "OpenRouter 429", Picks: []decider.Pick{{Range: decider.Range{Start: 5, End: 6}, Score: 0.5}}}, decision{},
			[]string{"[nibash: log lines 31-32 left out]", "; the decision model failed (OpenRouter 429), so keywords chose the middle; whole output: "}},
		{"unjudged windows and picks that did not fit are named", decider.Choice{Who: "m and keywords", Model: true,
			Picks:    []decider.Pick{{Range: decider.Range{Start: 0, End: 165}, Score: 0.9}, {Range: decider.Range{Start: 300, End: 301}, Score: 0.8}, {Range: decider.Range{Start: 600, End: 601}, Score: 0.7}},
			Unjudged: []decider.Unjudged{{Range: decider.Range{Start: 700, End: 725}, Reason: "OpenRouter 502"}, {Range: decider.Range{Start: 750, End: 775}, Reason: "OpenRouter 502"}}}, decision{purpose: "p"},
			[]string{"[nibash: 298 rows shown of 1,000 log lines; m and keywords chose the middle for your description; 2 more passages it chose did not fit (log lines 328-334, 628-634); log lines 731-755, 781-805 were not judged (OpenRouter 502); whole output: "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &sink{}
			s.Write([]byte(lines.String()))
			c.d.decider = &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return c.choice }}
			out := renderSink(t, s, c.d)
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in\n%s", want, out[max(0, len(out)-600):])
				}
			}
		})
	}
}

func TestFileRowsNeverReachTheDecider(t *testing.T) {
	var content strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&content, "line %d\n", i)
	}
	f := newFixture(t, map[string]string{"big.txt": content.String()})
	fake := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "fake"} }}
	f.shell.Deciding(fake)
	out := f.run("cat big.txt")
	if len(fake.asked) != 0 || !strings.Contains(out, "; log lines 100-800 left out; whole output: ") {
		t.Fatalf("asked %d times:\n%s", len(fake.asked), out[:200])
	}
}

func TestKeywordsChooseTheErrorInALongRun(t *testing.T) {
	f := newFixture(t, nil)
	f.shell.Deciding(decider.Config{Kind: decider.Keywords}.New())
	out := f.call(server.ShellCall{Command: "seq 1 1500; echo 'error: boom'; seq 1501 3000; exit 1", Description: "find the error"})
	for _, want := range []string{"\n1498\n1499\n1500\nerror: boom\n1501\n1502\n1503\n[nibash: log lines 1505-", "\n30\n[nibash: log lines 31-1497 left out]\n1498\n", "; keywords chose the middle; whole output: ", "\n[exit 1]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in\n%s", want, out)
		}
	}
}

func TestLongPlainRowsAreCutWithTheirLogLine(t *testing.T) {
	f := newFixture(t, nil)
	out := f.run("echo short; printf '%5000s\n' '' | tr ' ' x")
	rows := strings.Split(out, "\n")
	if len(rows) != 3 || rows[0] != "short" || rows[1] != "[line 2, chars 1-2000 of 5000]|"+strings.Repeat("x", 2000) {
		t.Fatalf("got %.300s", out)
	}
	if !strings.HasPrefix(rows[2], "[cut rows: N is the row's line in the whole output; read ") {
		t.Fatalf("footer %q", rows[2])
	}
	path := strings.TrimSuffix(strings.TrimPrefix(rows[2], "[cut rows: N is the row's line in the whole output; read "), ":N with maxLineLength to see one whole]")
	data, err := os.ReadFile(path)
	if err != nil || strings.Split(string(data), "\n")[1] != strings.Repeat("x", 5000) {
		t.Fatalf("log %s: %v", path, err)
	}
	// Taught once, the footer then only names where the whole rows are.
	again := strings.Split(f.run("printf '%5000s\n' '' | tr ' ' y"), "\n")
	if !regexp.MustCompile(`^\[cut rows: read \S+:N whole\]$`).MatchString(again[len(again)-1]) {
		t.Fatalf("second footer %q", again[len(again)-1])
	}
}

func TestACappedLogIsAnnounced(t *testing.T) {
	saved := logLimit
	logLimit = 1000
	t.Cleanup(func() { logLimit = saved })
	f := newFixture(t, nil)
	out := f.run("seq 1 5000")
	if !strings.Contains(out, "; log lines 101-4800 left out; the log stops at its 1,000 bytes limit, in log line 277; whole output: ") {
		t.Fatalf("got %s", out[strings.Index(out, "[nibash"):])
	}
}

func TestDescriptionSaysHowEachDeciderCuts(t *testing.T) {
	for _, kind := range decider.Kinds() {
		for _, summarizing := range []bool{false, true} {
			shell := New(engine.New(), false)
			if kind != decider.Fixed {
				shell.Deciding(&fakeDecider{kind: kind})
			}
			if summarizing {
				shell.Summarizing(&fakeSummarizer{})
			}
			text := shell.Description()
			if len(text) > 2048 || !strings.Contains(text, kind.Cut()) || strings.Contains(text, "%!") {
				t.Errorf("%s, summarizing %v: %d characters:\n%s", kind, summarizing, len(text), text)
			}
		}
	}
	if !strings.Contains(New(engine.New(), false).Description(), "long output cut to its start and end, with the whole kept in a log") {
		t.Error("the fixed cut's description changed")
	}
}
