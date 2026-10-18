package nibash

import (
	"os"
	"path/filepath"
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
