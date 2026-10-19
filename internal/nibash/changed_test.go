package nibash

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAFileSomethingElseIsWritingIsNotCalledChangedByTheCommand grows a held file from outside the session while commands run: one that only
// reads or waits does not claim the change, one that redirects to it does, and a file nothing else touches still is listed.
func TestAFileSomethingElseIsWritingIsNotCalledChangedByTheCommand(t *testing.T) {
	f := newFixture(t, map[string]string{"grow.txt": "start\n", "a.txt": numbers})
	f.run("cat grow.txt a.txt")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		path := filepath.Join(f.directory, "grow.txt")
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return
			}
			_, _ = file.WriteString("tick\n")
			file.Close()
		}
	}()
	defer func() { close(stop); <-done }()
	time.Sleep(100 * time.Millisecond)
	if out := f.run("sleep 0.3; tail -c 5 grow.txt"); strings.Contains(out, "changed by this command") {
		t.Fatalf("a file another program grows, got %s", out)
	}
	if out := f.run("echo mine >> grow.txt; sleep 0.1"); !strings.Contains(out, "[changed by this command: grow.txt#") {
		t.Fatalf("a redirect to it, got %s", out)
	}
	if out := f.run("sed -i s/one/ONE/ a.txt; sleep 0.1"); !strings.Contains(out, "[changed by this command: a.txt#") || strings.Contains(out, "grow.txt") {
		t.Fatalf("a file only the command writes, got %s", out)
	}
}

// TestAFileARunningJobNamesIsNotCalledChangedByAnotherCommand lets a job of the session write a file another call waits beside.
func TestAFileARunningJobNamesIsNotCalledChangedByAnotherCommand(t *testing.T) {
	f := newFixture(t, map[string]string{"job.txt": "start\n"})
	f.run("cat job.txt")
	f.run("for i in $(seq 1 40); do echo x >> job.txt; sleep 0.05; done &")
	if out := f.run("sleep 0.5"); strings.Contains(out, "changed by this command") {
		t.Fatalf("got %s", out)
	}
	// A bare wait that ends the call returns its zero status as no error.
	expect(t, f.run("wait"), "(no output)")
}

// tags hides the tags in output, which tests compare whole.
var tags = regexp.MustCompile(`#[0-9A-F]{4}`)

func TestAChangedFileTheAgentSawShowsItsChangeAsRows(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	f.run("cat a.txt")
	// A removal shows the kept line before it, so its place shows.
	expect(t, tags.ReplaceAllString(f.run("sed -i 3d a.txt"), "#T"), "[changed by this command: a.txt#T (was #T, +0 -1)]\n[a.txt#T]\n 2:two needle\n-|three")
	out := f.run("sed -i 's/four needle/FOUR/' a.txt")
	expect(t, tags.ReplaceAllString(out, "#T"), "[changed by this command: a.txt#T (was #T, +1 -1)]\n[a.txt#T]\n-|four needle\n+3:FOUR")
	// The new row was shown whole, so an edit replaces it at once.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 3:\n+four", tagOf(t, out))); err != nil {
		t.Fatalf("edit of a shown row: %v", err)
	}
}

func TestALargeChangeOrAnUnseenFileShowsOnlyCounts(t *testing.T) {
	var lines []string
	for i := range 40 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	f := newFixture(t, map[string]string{"big.txt": strings.Join(lines, "\n") + "\n"})
	f.run("cat big.txt")
	out := f.run("sed -i 's/^/x/' big.txt")
	expect(t, tags.ReplaceAllString(out, "#T"), "[changed by this command: big.txt#T (was #T, +40 -40)]")
	// A changed line no row showed needs a read.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[big.txt#%s]\nPUT 1:\n+first", tagOf(t, out))); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("got %v", err)
	}
	f.run("echo hi > made.txt")
	expect(t, tags.ReplaceAllString(f.run("sed -i s/hi/ho/ made.txt"), "#T"), "[changed by this command: made.txt#T (was #T, +1 -1)]")
}

func TestTheRowsOfOneCommandsChangesAreCapped(t *testing.T) {
	files := map[string]string{}
	for i := 1; i <= 4; i++ {
		files[fmt.Sprintf("c%d.txt", i)] = "a\nb\nc\nd\ne\n"
	}
	f := newFixture(t, files)
	f.run("cat c1.txt c2.txt c3.txt c4.txt")
	out := f.run("sed -i 's/^/x/' c1.txt c2.txt c3.txt c4.txt")
	// Each file changes 10 lines; three fill the 30 rows, the fourth shows its counts.
	if strings.Count(out, "\n[c") != 3 || strings.Contains(out, "\n[c4.txt#") || !strings.Contains(out, "c4.txt#") || strings.Count(out, "+5 -5") != 4 {
		t.Fatalf("got %s", out)
	}
}
