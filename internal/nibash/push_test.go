package nibash

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/11ways/nib/internal/server"
)

// pushes collects what jobs push, as the server's channel would send it; with fail set every push fails, as on a closed connection.
type pushes struct {
	mu   sync.Mutex
	got  []string
	meta []map[string]string
	fail bool
	sent chan struct{}
}

func newPushes() *pushes { return &pushes{sent: make(chan struct{}, 16)} }

func (p *pushes) push(content string, meta map[string]string) error {
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		p.sent <- struct{}{}
	}()
	if p.fail {
		return errors.New("no client connected")
	}
	p.got = append(p.got, content)
	p.meta = append(p.meta, meta)
	return nil
}

// next waits for the next push and returns its content and meta.
func (p *pushes) next(t *testing.T) (string, map[string]string) {
	t.Helper()
	select {
	case <-p.sent:
	case <-time.After(10 * time.Second):
		t.Fatal("no push came")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.got) == 0 {
		return "", nil
	}
	return p.got[len(p.got)-1], p.meta[len(p.meta)-1]
}

func (p *pushes) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got)
}

// jobLog is the log a job's note names, after "output: " and up to the next ";" or "]".
func jobLog(t *testing.T, note string) string {
	t.Helper()
	_, after, ok := strings.Cut(note, "output: ")
	if !ok {
		t.Fatalf("no log in %q", note)
	}
	return after[:strings.IndexAny(after, ";],")]
}

// ended waits for job spec of the fixture's session to end.
func ended(t *testing.T, f *fixture, spec string) {
	t.Helper()
	j := f.shell.session("session").jobs.find(spec)
	if j == nil {
		t.Fatalf("no job %s", spec)
	}
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("job %s never ended", spec)
	}
}

func TestABackgroundJobsEndIsPushedOnceWhereAPushReachesTheAgent(t *testing.T) {
	f := newFixture(t, nil)
	p := newPushes()
	started := f.call(server.ShellCall{Command: "echo hi; echo there", Background: true, Push: p.push})
	log := jobLog(t, started)
	expect(t, started, "[job %1 started; output: "+log+"; its end comes to you as a message]")
	content, meta := p.next(t)
	expect(t, content, "[job %1 finished, exit 0; output: "+log+"]\nhi\nthere")
	if !maps.Equal(meta, map[string]string{"job": "1", "exit": "0"}) {
		t.Errorf("meta %v", meta)
	}
	// The push told the end, so no call reports it again.
	expect(t, f.run("echo next"), "next")
	// Without a way to reach the agent, the note and the next call's report stay as they were.
	started = f.call(server.ShellCall{Command: "exit 3", Background: true})
	expect(t, started, "[job %2 started; output: "+jobLog(t, started)+"]")
	ended(t, f, "%2")
	expect(t, f.run("echo next"), "[job %2 finished, exit 3; output: "+jobLog(t, started)+"]\nnext")
	if p.count() != 1 {
		t.Errorf("pushed %d times", p.count())
	}
}

func TestACallThatWentOnAsAJobPushesWhatItPrintedAfterItsCallReturned(t *testing.T) {
	f := newFixture(t, nil)
	p := newPushes()
	limit := 150
	ctx, cancel := context.WithCancel(context.Background())
	result, err := f.shell.Run(ctx, "session", server.ShellCall{Command: "echo before; sleep 0.4; echo after; exit 2", Timeout: &limit, Push: p.push}, f.tools())
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	log := jobLog(t, result.Text)
	expect(t, result.Text, "before\n[nibash: still running after 150ms; it goes on as job %1, output: "+log+"; its end comes to you as a message; kill %1 stops it]")
	content, meta := p.next(t)
	expect(t, content, "[job %1 finished, exit 2; output: "+log+", whose lines 1-1 its call showed]\nafter")
	if meta["exit"] != "2" {
		t.Errorf("meta %v", meta)
	}
	// Without a way to reach the agent, the call names wait and kill as before.
	result, err = f.shell.Run(context.Background(), "session", server.ShellCall{Command: "sleep 0.3", Timeout: &limit}, f.tools())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.Text, "; wait %2 or kill %2]") {
		t.Errorf("got %s", result.Text)
	}
}

func TestAJobsEndAWaitToldIsNeitherPushedNorReportedAgain(t *testing.T) {
	f := newFixture(t, nil)
	f.call(server.ShellCall{Command: "sleep 0.2; echo done", Background: true})
	expect(t, f.run("wait %1; echo status=$?"), "status=0")
	expect(t, f.run("echo next"), "next")
	p := newPushes()
	f.call(server.ShellCall{Command: "sleep 0.3", Background: true, Push: p.push})
	expect(t, f.run("wait %2; echo status=$?"), "status=0")
	// The push would come as the job ends, which the wait saw.
	time.Sleep(200 * time.Millisecond)
	if p.count() != 0 {
		t.Errorf("pushed after a wait told the end: %v", p.got)
	}
	expect(t, f.run("echo next"), "next")
}

func TestAFailedPushLeavesTheEndToTheNextCallsReport(t *testing.T) {
	f := newFixture(t, nil)
	p := newPushes()
	p.fail = true
	started := f.call(server.ShellCall{Command: "echo x", Background: true, Push: p.push})
	p.next(t)
	expect(t, f.run("echo next"), "[job %1 finished, exit 0; output: "+jobLog(t, started)+"]\nnext")
}

func TestAJobStartedWithAmpersandIsNeverPushed(t *testing.T) {
	f := newFixture(t, nil)
	p := newPushes()
	expect(t, f.call(server.ShellCall{Command: "sleep 0.1 &", Push: p.push}), "(no output)")
	ended(t, f, "%1")
	if out := f.run("echo next"); !strings.HasPrefix(out, "[job %1 finished, exit 0; output: ") || !strings.HasSuffix(out, "]\nnext") {
		t.Errorf("got %s", out)
	}
	if p.count() != 0 {
		t.Errorf("pushed %v", p.got)
	}
}
