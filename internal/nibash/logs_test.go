package nibash

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
)

func TestALogReadThroughNibsOwnCommandsIsProgramOutput(t *testing.T) {
	var repeated strings.Builder
	for i := range 200 {
		fmt.Fprintf(&repeated, "2026-10-17 12:00:%02d INFO heartbeat from the worker pool ok\n", i%60)
	}
	f := newFixture(t, map[string]string{"build/test.log": "one\ntwo\n", "logs/run.txt": "three\n", "notes.txt": "four\n", "data.txt": repeated.String()})
	for _, command := range []string{"cat build/test.log", "cat logs/run.txt", "head -3 data.txt", "grep two build/test.log"} {
		if out := f.run(command); strings.Contains(out, "#") || strings.Contains(out, "1:") {
			t.Errorf("%s printed anchored rows:\n%s", command, out)
		}
	}
	if out := f.run("cat notes.txt"); !strings.Contains(out, "1:four") {
		t.Errorf("a plain file lost its anchors:\n%s", out)
	}
	// A log git tracks is a file of the project, shown as anchored rows.
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", f.directory, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "build/test.log")
	git("commit", "-qm", "x")
	if out := f.run("cat build/test.log"); !strings.Contains(out, "1:one") {
		t.Errorf("a tracked log lost its anchors:\n%s", out)
	}
}

func TestNoSourceFileOfNibReadsAsALog(t *testing.T) {
	out, err := exec.Command("git", "-C", "../..", "ls-files").Output()
	if err != nil {
		t.Skip("git is not available")
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, _ := filepath.Abs(filepath.Join("../..", name))
		data, err := os.ReadFile(path)
		if err != nil || strings.HasSuffix(name, ".gz") || strings.ContainsRune(string(data), 0) {
			continue
		}
		if looksLikeLog(path, plainLines(data)) {
			t.Errorf("%s reads as a log", name)
		}
	}
}

func TestOutputThatShrinksAGreatDealGetsASummarySooner(t *testing.T) {
	summary := &fakeSummarizer{summarize: func(decider.SummaryInput) decider.Summary {
		return decider.Summary{Text: "It repeats.", Ranges: [][2]int{{1, 1}}}
	}}
	choose := &fakeDecider{kind: decider.Keywords, choose: func(decider.Input) decider.Choice { return decider.Choice{Who: "keywords"} }}
	output := func(repeat bool) *sink {
		var b strings.Builder
		for i := range 100 {
			if repeat {
				fmt.Fprintf(&b, "worker %d reports the queue is still empty, waiting\n", i)
			} else {
				fmt.Fprintf(&b, "%c%c worker reports the queue is still empty, waiting\n", 'a'+i%26, 'a'+i/26)
			}
		}
		s := &sink{}
		s.Write([]byte(b.String()))
		return s
	}
	// 100 lines of about 5 KB are under summaryLines and summaryBytes: repeating ones get a summary, varied ones stay whole.
	if out := renderSink(t, output(true), decision{decider: choose, summarizer: summary, exit: 1}); !strings.Contains(out, "<summary>It repeats.</summary>") {
		t.Fatalf("repeating output:\n%s", out)
	}
	if out := renderSink(t, output(false), decision{decider: choose, summarizer: summary, exit: 1}); strings.Contains(out, "<summary>") {
		t.Fatalf("varied output:\n%s", out)
	}
}
