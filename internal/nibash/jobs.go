package nibash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/engine"
)

// job is work running detached from the calls: a whole command run with background: true, a statement run with &, or a call still running
// at its timeout.
type job struct {
	id      int
	command string
	log     string
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	groups  map[int]bool
	exit    int
	// reported is whether the job's end was told, by a call's report, a wait or a push; waiters counts the waits on it now. The table's
	// lock guards both.
	reported bool
	waiters  int
	// push tells the agent that started the job its end on its own, nil when nothing reaches that agent; purpose is the call's description,
	// and shown how many lines of the log the call that went on as the job already showed.
	push    func(content string, meta map[string]string) error
	purpose string
	shown   int
	// started and ended are when the job began and, once done is closed, when it ended.
	started, ended time.Time
	// killed is the signal kill sent; a process the job starts after it gets it at once, and the job starts nothing more.
	killed syscall.Signal
}

func (j *job) add(pgid int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.groups[pgid] = true
	if j.killed != 0 {
		_ = syscall.Kill(-pgid, j.killed)
	}
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// terminating are the signals that end a job, so its shell starts nothing more after them.
var terminating = map[syscall.Signal]bool{syscall.SIGTERM: true, syscall.SIGKILL: true, syscall.SIGINT: true, syscall.SIGHUP: true, syscall.SIGQUIT: true}

// signal sends sig to every process group the job started; a terminating one also stops the job's shell, also before it started anything.
func (j *job) signal(sig syscall.Signal) {
	j.mu.Lock()
	groups := make([]int, 0, len(j.groups))
	for pgid := range j.groups {
		groups = append(groups, pgid)
	}
	if terminating[sig] {
		j.killed = sig
	}
	j.mu.Unlock()
	for _, pgid := range groups {
		_ = syscall.Kill(-pgid, sig)
	}
	if terminating[sig] {
		j.cancel()
	}
}

type jobTable struct {
	mu   sync.Mutex
	jobs []*job
	next int
}

func newJobTable() *jobTable { return &jobTable{next: 1} }

func (t *jobTable) find(spec string) *job {
	t.mu.Lock()
	defer t.mu.Unlock()
	if spec == "%%" || spec == "%+" || spec == "%" {
		if len(t.jobs) == 0 {
			return nil
		}
		return t.jobs[len(t.jobs)-1]
	}
	id, err := strconv.Atoi(strings.TrimPrefix(spec, "%"))
	if err != nil {
		return nil
	}
	for _, j := range t.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

func (t *jobTable) all() []*job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.jobs)
}

// finished reports, once each, the jobs that ended since the last call.
func (t *jobTable) finished() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var lines []string
	for _, j := range t.jobs {
		if j.running() || j.reported {
			continue
		}
		j.reported = true
		lines = append(lines, fmt.Sprintf("[job %%%d finished, exit %d; output: %s]", j.id, j.exit, j.log))
	}
	return lines
}

// claim takes the telling of j's end for a push: not when it was told already, nor while a wait on it will tell it.
func (t *jobTable) claim(j *job) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j.reported || j.waiters > 0 {
		return false
	}
	j.reported = true
	return true
}

// unclaim gives a push's telling back when the push failed, so the next call reports the end.
func (t *jobTable) unclaim(j *job) {
	t.mu.Lock()
	j.reported = false
	t.mu.Unlock()
}

// await waits for j within ctx; the wait tells its end, so it is neither pushed while the wait runs nor reported again after.
func (t *jobTable) await(ctx context.Context, j *job) error {
	t.mu.Lock()
	j.waiters++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		j.waiters--
		t.mu.Unlock()
	}()
	select {
	case <-j.done:
		t.mu.Lock()
		j.reported = true
		t.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *jobTable) killAll() {
	for _, j := range t.all() {
		if j.running() {
			j.signal(syscall.SIGKILL)
		}
	}
}

// cappedWriter writes to a log until it holds logLimit bytes, then drops the rest.
type cappedWriter struct {
	mu      sync.Mutex
	file    *os.File
	written int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := logLimit - w.written; room > 0 {
		n, err := w.file.Write(p[:min(len(p), room)])
		w.written += n
		if err != nil {
			return n, err
		}
	}
	return len(p), nil
}

// open adds a new job that began at started, ended through cancel, with a new log for its output; finish ends it.
func (t *jobTable) open(logs *logDirectory, command string, cancel context.CancelFunc, started time.Time) (*job, *cappedWriter, error) {
	path, err := logs.file("job")
	if err != nil {
		return nil, nil, err
	}
	logFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	j := &job{id: t.next, command: command, log: path, cancel: cancel, done: make(chan struct{}), groups: map[int]bool{}, started: started}
	t.next++
	t.jobs = append(t.jobs, j)
	return j, &cappedWriter{file: logFile}, nil
}

// finish records how the job's run ended, closes its log and marks it done.
func (j *job) finish(err error, out *cappedWriter) {
	j.mu.Lock()
	killed := j.killed
	j.mu.Unlock()
	var exit interp.ExitStatus
	switch {
	case killed != 0:
		j.exit = 128 + int(killed)
	case errors.As(err, &exit):
		j.exit = int(exit)
	case err != nil:
		fmt.Fprintf(out, "nibash: %v\n", err)
		j.exit = 1
	}
	out.file.Close()
	j.ended = time.Now()
	close(j.done)
}

// launch runs node in runner as a new job, with its output in a log and its processes in groups the job can signal; push, when set, tells the
// agent its end, with purpose as what the call was for.
func (sess *session) launch(runner *interp.Runner, node syntax.Node, command string, tools *engine.Context, names namer, push func(string, map[string]string) error, purpose string) (*job, error) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	base, cancel := context.WithCancel(context.Background())
	j, out, err := sess.jobs.open(sess.shell.logs, command, cancel, time.Now())
	if err == nil {
		j.push, j.purpose = push, purpose
	}
	if err != nil {
		cancel()
		devnull.Close()
		return nil, err
	}
	if err := interp.StdIO(devnull, out, out)(runner); err != nil {
		cancel()
		devnull.Close()
		j.finish(err, out)
		return nil, err
	}
	jobTools := *tools
	c := &call{engine: sess.shell.engine, tools: &jobTools, session: sess, runner: runner, names: names, links: &links{}, logs: sess.shell.logs, job: j, groups: map[int]bool{}, written: map[string]bool{}}
	ctx := context.WithValue(base, callKey{}, c)
	jobTools.Ctx = ctx
	go func() {
		defer devnull.Close()
		j.finish(runner.Run(ctx, node), out)
		sess.shell.pushEnd(sess, j)
	}()
	return j, nil
}

// startJob starts the statement an & put away as a job, in a copy of the shell as it is now, and prints the job's spec for $!.
func (c *call) startJob(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	key, err := strconv.Atoi(args[0])
	stmt := c.jobs[key]
	if err != nil || stmt == nil {
		return interp.ExitStatus(1)
	}
	runner := c.runner.Subshell()
	if err := interp.Dir(hc.Dir)(runner); err != nil {
		return err
	}
	var text strings.Builder
	syntax.NewPrinter().Print(&text, stmt)
	j, err := c.session.launch(runner, stmt, strings.TrimSpace(text.String())+" &", c.tools, c.names, nil, "")
	if err != nil {
		fmt.Fprintf(hc.Stderr, "nibash: %v\n", err)
		return interp.ExitStatus(1)
	}
	fmt.Fprintf(hc.Stdout, "%%%d", j.id)
	return nil
}

var signalNames = map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT}

func parseSignal(name string) (syscall.Signal, bool) {
	name = strings.TrimPrefix(strings.ToUpper(name), "SIG")
	if sig, ok := signalNames[name]; ok {
		return sig, true
	}
	if n, err := strconv.Atoi(name); err == nil && n > 0 && n < 65 {
		return syscall.Signal(n), true
	}
	return 0, false
}

// kill signals jobs (%N) and processes (PID, or -PGID for a group), as bash's kill does.
func (c *call) kill(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	sig := syscall.SIGTERM
	if len(args) > 0 && args[0] == "-l" {
		names := make([]string, 0, len(signalNames))
		for name := range signalNames {
			names = append(names, name)
		}
		slices.Sort(names)
		fmt.Fprintln(hc.Stdout, strings.Join(names, " "))
		return nil
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "--" {
		value := args[0][1:]
		if value == "s" || value == "n" {
			if len(args) < 2 {
				fmt.Fprintln(hc.Stderr, "kill: option requires an argument")
				return interp.ExitStatus(2)
			}
			value, args = args[1], args[1:]
		}
		parsed, ok := parseSignal(value)
		if !ok {
			// A negative number after the signal is a process group.
			if _, err := strconv.Atoi(value); err == nil {
				break
			}
			fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", value)
			return interp.ExitStatus(1)
		}
		sig = parsed
		args = args[1:]
	}
	args = slices.DeleteFunc(args, func(a string) bool { return a == "--" })
	if len(args) == 0 {
		fmt.Fprintln(hc.Stderr, "kill: usage: kill [-s sigspec | -sigspec] pid | jobspec ...")
		return interp.ExitStatus(2)
	}
	status := 0
	for _, target := range args {
		if strings.HasPrefix(target, "%") {
			j := c.session.jobs.find(target)
			if j == nil {
				fmt.Fprintf(hc.Stderr, "kill: %s: no such job\n", target)
				status = 1
				continue
			}
			j.signal(sig)
			continue
		}
		pid, err := strconv.Atoi(target)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "kill: %s: arguments must be process or job IDs\n", target)
			status = 1
			continue
		}
		if err := syscall.Kill(pid, sig); err != nil {
			fmt.Fprintf(hc.Stderr, "kill: (%d) - %s\n", pid, errorText(err))
			status = 1
		}
	}
	if status == 0 {
		// The interpreter takes a zero exit status given as an error for an internal error.
		return nil
	}
	return interp.ExitStatus(status)
}

func errorText(err error) string {
	text := err.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// listJobs prints the session's jobs with their state and log.
func (c *call) listJobs(ctx context.Context) error {
	hc := interp.HandlerCtx(ctx)
	for _, j := range c.session.jobs.all() {
		state := "Running"
		if !j.running() {
			state = fmt.Sprintf("Done (exit %d)", j.exit)
		}
		fmt.Fprintf(hc.Stdout, "[%d]  %s  %s  (output: %s)\n", j.id, state, j.command, j.log)
	}
	return nil
}

// wait waits for the named jobs, or for every job, within the call's time.
func (c *call) wait(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	targets := c.session.jobs.all()
	if len(args) > 0 {
		targets = nil
		for _, spec := range args {
			j := c.session.jobs.find(spec)
			if j == nil {
				fmt.Fprintf(hc.Stderr, "wait: %s: no such job\n", spec)
				c.noteWaitless("no job " + spec)
				return interp.ExitStatus(127)
			}
			targets = append(targets, j)
		}
	} else if !slices.ContainsFunc(targets, (*job).running) {
		c.noteWaitless("no running job")
	}
	c.mu.Lock()
	c.waited = append(c.waited, targets...)
	c.mu.Unlock()
	status := 0
	for _, j := range targets {
		if err := c.session.jobs.await(ctx, j); err != nil {
			// Stopped by its call, wait ends as bash's does when a signal interrupts it; the interpreter takes an error for an internal one.
			return interp.ExitStatus(waitInterrupted)
		}
		status = j.exit
	}
	if status == 0 {
		// The interpreter takes a zero exit status given as an error for an internal error.
		return nil
	}
	return interp.ExitStatus(status)
}

// waitlessCommand is how many characters of another call's command line the note of a wait with nothing to wait for shows.
const waitlessCommand = 60

// noteWaitless tells, once a call, that wait found nothing to wait for while other calls of the agent still run in their requests: such a
// call is no job, so wait cannot reach it, and its result comes back to that call.
func (c *call) noteWaitless(what string) {
	commands := c.session.otherCalls(c)
	if len(commands) == 0 {
		return
	}
	quoted := make([]string, len(commands))
	for i, command := range commands {
		shown := []rune(strings.Join(strings.Fields(command), " "))
		if len(shown) > waitlessCommand {
			shown = append(shown[:waitlessCommand], []rune("...")...)
		}
		quoted[i] = `"` + string(shown) + `"`
	}
	note := fmt.Sprintf("[%s to wait for: the call %s still runs as a call, not a job, and its result comes back to that call]", what, quoted[0])
	if len(quoted) > 1 {
		named := strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
		note = fmt.Sprintf("[%s to wait for: the calls %s still run as calls, not jobs, and their results come back to those calls]", what, named)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitless == "" {
		c.waitless = note
	}
}

var _ io.Writer = (*cappedWriter)(nil)

// waitInterrupted is the status of a wait its call stopped: bash's for a wait a signal interrupts, 128 plus SIGINT's number.
const waitInterrupted = 130
