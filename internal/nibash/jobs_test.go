package nibash

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/server"
)

// detached runs command as a call that goes on as a job at limit, with a request context cancelled once the call answered, as an MCP
// server cancels a request's, and returns the job's log once the job ended.
func detached(f *fixture, command string, limit int) string {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result, err := f.shell.Run(ctx, "session", server.ShellCall{Command: command, Timeout: &limit}, f.tools())
	cancel()
	if err != nil {
		f.t.Fatal(err)
	}
	_, after, ok := strings.Cut(result.Text, "it goes on as job ")
	if !ok {
		f.t.Fatalf("%q did not go on as a job: %s", command, result.Text)
	}
	spec, rest, _ := strings.Cut(after, ", output: ")
	log, _, _ := strings.Cut(rest, ";")
	f.run("wait " + spec)
	data, err := os.ReadFile(log)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func TestWhatACallRunsAfterItWentOnAsAJobOutlivesItsRequest(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "one\ntwo\n"})
	for _, c := range []struct{ command, want string }{
		{"sleep 0.4; cat a.txt", "one\ntwo\n"},
		{"sleep 0.4; /bin/echo real", "real\n"},
		{"sleep 0.4; cat a.txt | head -1", "one\n"},
		{"sleep 0.4; true && cat a.txt; echo status=$?", "one\ntwo\nstatus=0\n"},
		{"sleep 0.4; grep -c o a.txt", "2\n"},
	} {
		expect(t, detached(f, c.command, 150), c.want)
	}
	// The sketerm session's shape: a call waiting on a job goes on as a job itself, then reads the first job's log.
	started := f.call(server.ShellCall{Command: "sleep 0.5; echo first", Background: true})
	started = started[strings.LastIndex(started, "[job %"):]
	spec, log, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(started, "[job "), "]"), " started; output: ")
	expect(t, detached(f, "wait "+spec+"; cat "+log, 150), "first\n")
}

// alive says whether a process whose command line matches pattern runs.
func alive(pattern string) bool { return exec.Command("pgrep", "-f", pattern).Run() == nil }

// callRunning starts command as a call that stays in its request until the returned stop, and returns once its program runs.
func callRunning(f *fixture, command, program string) (stop func()) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.shell.Run(ctx, "session", server.ShellCall{Command: command}, f.tools())
	}()
	for deadline := time.Now().Add(2 * time.Second); !alive(program); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			f.t.Fatalf("%q never started", command)
		}
	}
	return func() {
		cancel()
		<-done
	}
}

func TestWaitWithNoJobSaysWhichCallIsStillRunning(t *testing.T) {
	f := newFixture(t, nil)
	// Without another call running, wait is bash's alone.
	expect(t, f.run("wait %2 2>/dev/null; echo status=$?"), "status=127")
	expect(t, f.run("wait; echo status=$?"), "status=0")
	stop := callRunning(f, "sleep 30.31; echo done", "^sleep 30.31$")
	defer stop()
	note := `[no job %2 to wait for: the call "sleep 30.31; echo done" still runs as a call, not a job, and its result comes back to that call]`
	// The note is nib's own, so it shows when wait's message goes nowhere.
	expect(t, f.run("wait %2 2>/dev/null; echo status=$?"), "status=127\n"+note)
	expect(t, f.run("wait %2"), "wait: %2: no such job\n"+note+"\n[exit 127]")
	expect(t, f.run("wait; echo status=$?"), "status=0\n"+strings.Replace(note, "no job %2", "no running job", 1))
	// A job that exists is waited for as before, with no note.
	f.call(server.ShellCall{Command: "sleep 0.2", Background: true})
	expect(t, f.run("wait %1; echo status=$?"), "status=0")
}

func TestWaitNamesEveryOtherRunningCallShortened(t *testing.T) {
	f := newFixture(t, nil)
	stopFirst := callRunning(f, "sleep 30.32", "^sleep 30.32$")
	defer stopFirst()
	long := "sleep 30.33; echo " + strings.Repeat("x", 80)
	stopSecond := callRunning(f, long, "^sleep 30.33$")
	defer stopSecond()
	out := f.run("wait %3 2>/dev/null")
	want := `[no job %3 to wait for: the calls "sleep 30.32" and "sleep 30.33; echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx..." still run as calls, not jobs, and their results come back to those calls]`
	expect(t, out, want+"\n[exit 127]")
}

func TestACallThatOnlyWaitsGetsTheLongestTimeout(t *testing.T) {
	limit := 5000
	for _, c := range []struct {
		command string
		timeout *int
		want    time.Duration
	}{
		{"wait %1", nil, maxTimeout},
		{"wait", nil, maxTimeout},
		{"wait %1 %2 2>/dev/null; wait", nil, maxTimeout},
		{"wait %1", &limit, 5 * time.Second},
		{"wait %1; cat job.log", nil, defaultTimeout},
		{"wait %1 && echo done", nil, defaultTimeout},
		{"wait %1 &", nil, defaultTimeout},
		{"sleep 1", nil, defaultTimeout},
	} {
		file, err := syntax.NewParser().Parse(strings.NewReader(c.command), "")
		if err != nil {
			t.Fatal(err)
		}
		if got := callTimeout(server.ShellCall{Command: c.command, Timeout: c.timeout}, onlyWaits(file)); got != c.want {
			t.Errorf("%q: got %s, want %s", c.command, got, c.want)
		}
	}
}

func TestAWaitThatRunsOutNamesItsJobsAndGoesOnAsNoJob(t *testing.T) {
	f := newFixture(t, nil)
	f.run("sleep 30.71 &")
	f.run("sleep 30.72 &")
	limit := 300
	expect(t, f.call(server.ShellCall{Command: "wait %1", Timeout: &limit}), "[job %1 still running after 300ms; wait %1 again]")
	expect(t, f.call(server.ShellCall{Command: "wait", Timeout: &limit}), "[jobs %1, %2 still running after 300ms; wait %1 %2 again]")
	// Both jobs run on, and no job waits for them.
	expect(t, f.run("jobs | wc -l"), "2")
	// A call that does more than wait still goes on as a job.
	if out := f.call(server.ShellCall{Command: "wait %1; echo after", Timeout: &limit}); !strings.Contains(out, "it goes on as job %3") {
		t.Errorf("a wait followed by more went on as no job: %s", out)
	}
	// A wait its request stops ends as bash's does when a signal interrupts it, not as an internal error.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if result, err := f.shell.Run(ctx, "session", server.ShellCall{Command: "wait %1"}, f.tools()); err != nil || result.Text != "[exit 130]" {
		t.Errorf("a cancelled wait: %q, %v", result.Text, err)
	}
	expect(t, f.run("kill %1 %2"), "(no output)")
}
