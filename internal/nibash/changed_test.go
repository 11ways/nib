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

func TestFilesInTheTempFolderTheAgentNeverReadAreCountedNotNamed(t *testing.T) {
	f := newFixture(t, nil)
	temp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temp)
	if out := f.run("echo x > " + temp + "/a.json; echo y > " + temp + "/b.json"); !strings.HasSuffix(out, "[changed by this command: 2 new files in the temp folder]") || strings.Contains(out, ".json") {
		t.Fatalf("got %q", out)
	}
	if out := f.run("echo v > " + temp + "/b.json"); !strings.HasSuffix(out, "[changed by this command: 1 changed file in the temp folder]") {
		t.Fatalf("a rewrite of one never read, got %q", out)
	}
	if out := f.run("echo q > n.txt; echo r > " + temp + "/c.json"); !regexp.MustCompile(`\[changed by this command: n\.txt#[0-9A-F]{4} \(new\), 1 new file in the temp folder\]$`).MatchString(out) {
		t.Fatalf("a new file outside it, got %q", out)
	}
	// Once read, a file there is like any other.
	f.run("cat " + temp + "/a.json")
	if out := f.run("echo w >> " + temp + "/a.json"); !strings.Contains(out, "a.json#") || !strings.Contains(out, "(was #") || strings.Contains(out, "temp folder") {
		t.Fatalf("a file read before, got %q", out)
	}
}

// TestAChangeNibCannotCreditToTheCommandShowsNoRows changes held files while a command sleeps: one outside anything the command reached,
// written by another program, and one in its own directory, written by nib's edit; neither is called the command's, nor shown as its rows.
func TestAChangeNibCannotCreditToTheCommandShowsNoRows(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	elsewhere := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(elsewhere, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run("cat a.txt " + elsewhere)
	while := func(write func()) string {
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(100 * time.Millisecond)
			write()
		}()
		out := f.run("sleep 0.4")
		<-done
		return out
	}
	out := while(func() { _ = os.WriteFile(elsewhere, []byte("one\nTWO\n"), 0o644) })
	if strings.Contains(out, "changed by this command") || strings.Contains(out, "-|two") || !strings.Contains(out, "[changed while this command ran, by it or another writer: ") || !strings.Contains(out, "other.txt#") || !strings.Contains(out, "+1 -1)]") {
		t.Fatalf("a file beyond the command's reach, got %s", out)
	}
	tag := tagOf(t, f.run("cat a.txt"))
	out = while(func() {
		if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 1:\n+ONE", tag)); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(out, "changed by this command") || strings.Contains(out, "+1:ONE") || !strings.Contains(out, "[changed while this command ran, by it or another writer: a.txt#") {
		t.Fatalf("a file nib's edit wrote meanwhile, got %s", out)
	}
	// A job the command itself started is the command's.
	if out := f.run("(sleep 0.1; sed -i s/ONE/one/ a.txt) & wait"); !strings.Contains(out, "[changed by this command: a.txt#") || !strings.Contains(out, "+1:one") {
		t.Fatalf("a job of the command's own, got %s", out)
	}
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

func TestLinesAHereDocumentAddedPrintAsOneMarker(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "one\ntwo\n"})
	f.run("cat a.txt")
	out := f.run("cat >> a.txt <<'EOF'\nthree\nfour\nEOF")
	expect(t, tags.ReplaceAllString(out, "#T"), "[changed by this command: a.txt#T (was #T, +2 -0)]\n[a.txt#T]\n[lines 3-4 as you sent them]")
	// The agent typed them, so an edit replaces one at once.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 4:\n+FOUR", tagOf(t, out))); err != nil {
		t.Fatalf("edit of a typed row: %v", err)
	}
	// Lines a program added print as rows.
	expect(t, tags.ReplaceAllString(f.run("echo five >> a.txt"), "#T"), "[changed by this command: a.txt#T (was #T, +1 -0)]\n[a.txt#T]\n+5:five")
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
