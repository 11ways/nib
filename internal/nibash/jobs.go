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
	"github.com/11ways/nib/internal/platform"
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
	// killed is the last signal kill sent that ends a process; a process the job starts after it gets it at once.
	killed syscall.Signal
	// call is the call that runs the job's command: a statement's own, or the call that went on as the job.
	call *call
	// programs are the programs the job started, in order, the last programsKept of them, for wait PID and jobs -l.
	programs []*child
	// disowned is set once disown took the job from its shell's table, which no longer names, reports or ends it; kept is set by disown -h,
	// which leaves it in the table but running past the session's end, as bash's sends it no SIGHUP when it exits. The table's lock guards
	// both.
	disowned, kept bool
}

// child is a program a job started: its process group, which its pid leads, and once done is closed its exit status.
type child struct {
	pgid   int
	done   chan struct{}
	status int
}

// programsKept is how many of a job's programs it remembers for wait PID, as bash remembers its children's statuses.
const programsKept = 256

func (j *job) add(pgid int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.groups[pgid] = true
	j.programs = append(j.programs, &child{pgid: pgid, done: make(chan struct{})})
	if len(j.programs) > programsKept {
		j.programs = slices.Delete(j.programs, 0, len(j.programs)-programsKept)
	}
	if j.killed != 0 {
		_ = platform.SignalGroup(pgid, j.killed)
	}
}

// childEnded notes the end of the job's program that leads pgid, with its exit status.
func (j *job) childEnded(pgid, status int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.programs {
		if p.pgid == pgid && !isClosed(p.done) {
			p.status = status
			close(p.done)
		}
	}
}

// child is the program of the job whose process group pid leads, nil when the job started none.
func (j *job) child(pid int) *child {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := len(j.programs) - 1; i >= 0; i-- {
		if j.programs[i].pgid == pid {
			return j.programs[i]
		}
	}
	return nil
}

// runningPrograms are the process groups of the job's programs that still run, in the order they started.
func (j *job) runningPrograms() []int {
	j.mu.Lock()
	defer j.mu.Unlock()
	var groups []int
	for _, p := range j.programs {
		if !isClosed(p.done) {
			groups = append(groups, p.pgid)
		}
	}
	return groups
}

// stopped says whether every program the job still runs is stopped, as bash's jobs calls a job Stopped; false where the OS does not tell.
func (j *job) stopped() bool {
	groups := j.runningPrograms()
	for _, pgid := range groups {
		if stopped, known := platform.Stopped(pgid); !stopped || !known {
			return false
		}
	}
	return len(groups) > 0
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// attach makes c the call that runs j's command; a kill that came first halts it at once.
func (j *job) attach(c *call) {
	j.mu.Lock()
	j.call = c
	killed := j.killed
	j.mu.Unlock()
	if killed != 0 {
		c.deliver(killed, sending{group: true, quiet: true, target: "the job"})
	}
}

// runner is the call that runs j's command, nil until it has one.
func (j *job) runner() *call {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.call
}

func (j *job) running() bool {
	select {
	case <-j.done:
		return false
	default:
		return true
	}
}

// signal is a kill of the job: sig goes to every process group the job started (sendGroups), and the job's shell meets it as its traps say
// (deliver), by default ending on one that ends a process, as bash's subshell would, but leaving its program running to take the signal as
// it will: one that handles it, as a server reloading on SIGHUP does, goes on.
func (j *job) signal(sig syscall.Signal) {
	j.sendGroups(sig)
	if shell := j.runner(); shell != nil {
		shell.deliver(sig, sending{group: true, quiet: true, target: "the job"})
	}
}

// sendGroups sends sig to every process group the job started, and after SIGTERM or SIGHUP SIGCONT to a stopped one, as bash sends it, since a
// stopped program holds the signal until it continues; where the OS does not tell what is stopped, to each, which a running one ignores.
func (j *job) sendGroups(sig syscall.Signal) {
	j.mu.Lock()
	groups := make([]int, 0, len(j.groups))
	for pgid := range j.groups {
		groups = append(groups, pgid)
	}
	if platform.Ends(sig) {
		j.killed = sig
	}
	j.mu.Unlock()
	for _, pgid := range groups {
		_ = platform.SignalGroup(pgid, sig)
		if stopped, known := platform.Stopped(pgid); (sig == syscall.SIGTERM || sig == syscall.SIGHUP) && (stopped || !known) {
			platform.ContinueGroup(pgid)
		}
	}
}

type jobTable struct {
	mu   sync.Mutex
	jobs []*job
	next int
}

func newJobTable() *jobTable { return &jobTable{next: 1} }

// find is the job spec names among every job of the table, as findAmong reads it.
func (t *jobTable) find(spec string) (*job, string) { return t.findAmong(spec, t.all()) }

// findAmong is the job spec names among known, a shell's jobs: %N, %%, %+ or % for the current job, %- for the one before it, %STRING for
// the one whose command starts with STRING and %?STRING for the one whose command holds it, else problem says why none, as bash words it.
// The current and previous jobs and those a string names are among the jobs bash would still hold, those running or whose end was not told
// yet; %N names any. A spec without % is a job's number, as nibash's wait took one before it took pids.
func (t *jobTable) findAmong(spec string, known []*job) (j *job, problem string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	missing := spec + ": no such job"
	var held []*job
	for _, j := range known {
		if j.running() || !j.reported {
			held = append(held, j)
		}
	}
	name, ok := strings.CutPrefix(spec, "%")
	switch {
	case !ok || name != "" && name[0] >= '0' && name[0] <= '9':
		id, err := strconv.Atoi(name)
		if !ok {
			id, err = strconv.Atoi(spec)
		}
		if err == nil {
			for _, j := range known {
				if j.id == id {
					return j, ""
				}
			}
		}
		return nil, missing
	case name == "" || name == "%" || name == "+":
		if len(held) == 0 {
			return nil, missing
		}
		return held[len(held)-1], ""
	case name == "-":
		if len(held) == 0 {
			return nil, missing
		}
		return held[max(len(held)-2, 0)], ""
	}
	text, contains := strings.CutPrefix(name, "?")
	var found *job
	for _, j := range held {
		command := strings.TrimSuffix(j.command, " &")
		if contains && strings.Contains(command, text) || !contains && strings.HasPrefix(command, text) {
			if found != nil {
				return nil, text + ": ambiguous job spec"
			}
			found = j
		}
	}
	if found == nil {
		return nil, missing
	}
	return found, ""
}

// marks are the job markers bash's jobs prints among known: '+' for the current job, '-' for the one before it.
func (t *jobTable) marks(known []*job) map[*job]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	var held []*job
	for _, j := range known {
		if j.running() || !j.reported {
			held = append(held, j)
		}
	}
	marks := map[*job]byte{}
	if len(held) > 1 {
		marks[held[len(held)-2]] = '-'
	}
	if len(held) > 0 {
		marks[held[len(held)-1]] = '+'
	}
	return marks
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

// await waits for j within ctx, or until stop or signalled is closed; the wait tells its end, so it is neither pushed while the wait runs
// nor reported again after.
func (t *jobTable) await(ctx context.Context, j *job, stop, signalled <-chan struct{}) error {
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
		t.tell(j)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-signalled:
		return errSignalled
	case <-stop:
		return errHalted
	}
}

// tell marks j's end told, as a wait that saw it tells it.
func (t *jobTable) tell(j *job) {
	t.mu.Lock()
	j.reported = true
	t.mu.Unlock()
}

// errHalted is the error of a wait its call's halt stopped.
var errHalted = errors.New("halted")

// killAll ends every running job and its shell, as the connection ends, but those disown let run past it.
func (t *jobTable) killAll() {
	for _, j := range t.all() {
		t.mu.Lock()
		spared := j.disowned || j.kept
		t.mu.Unlock()
		if j.running() && !spared {
			j.signal(syscall.SIGKILL)
			j.cancel()
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
	path, err := logs.file(jobKind)
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
	case errors.As(err, &exit):
		j.exit = int(exit)
	case err != nil && killed != 0:
		// A shell killAll cancelled ends with the context's error.
		j.exit = 128 + int(killed)
	case err != nil:
		fmt.Fprintf(out, "nibash: %v\n", err)
		j.exit = 1
	}
	out.file.Close()
	j.ended = time.Now()
	close(j.done)
}

// launch runs node in runner as a new job, with its output in a log and its processes in groups the job can signal; push, when set, tells the
// agent its end, with purpose as what the call was for. jobs are the statements the command line's & started (startJobs), which the job's own
// & starts from; origin is the call whose statement node is, nil for a command run with background: true.
func (sess *session) launch(runner *interp.Runner, node syntax.Node, command string, tools *engine.Context, names namer, push func(string, map[string]string) error, purpose string, origin *call, jobs map[int]*syntax.Stmt) (*job, error) {
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
	c := &call{engine: sess.shell.engine, tools: &jobTools, session: sess, runner: runner, names: names, links: &links{}, logs: sess.shell.logs, job: j, groups: map[int]bool{}, written: map[string]bool{}, origin: origin, jobs: jobs, subshell: true}
	if origin != nil {
		c.inherit(origin)
	}
	sess.shell.register(c)
	j.attach(c)
	ctx := context.WithValue(base, callKey{}, c)
	jobTools.Ctx = ctx
	// The job is a shell of its own, whose exit runs the traps it set.
	stmts, ok := []*syntax.Stmt{}, false
	if file, isFile := node.(*syntax.File); isFile {
		stmts, ok = file.Stmts, true
	}
	if !ok {
		stmts = []*syntax.Stmt{node.(*syntax.Stmt)}
	}
	file := &syntax.File{Stmts: append([]*syntax.Stmt{startShell()}, stmts...)}
	go func() {
		defer devnull.Close()
		j.finish(c.jobError(runner.Run(ctx, file), out), out)
		c.end()
		sess.shell.pushEnd(sess, j)
	}()
	return j, nil
}

// startJob starts the statement an & put away as a job, in a copy of the interpreter the & ran in as it is now, as bash forks, and prints the
// job's spec for $!.
func (c *call) startJob(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	key, err := strconv.Atoi(args[0])
	stmt := c.jobs[key]
	if err != nil || stmt == nil {
		return interp.ExitStatus(1)
	}
	here := runnerOf(ctx)
	if here == nil {
		here = c.runner
	}
	runner := here.Subshell()
	if err := interp.Dir(hc.Dir)(runner); err != nil {
		return err
	}
	var text strings.Builder
	syntax.NewPrinter().Print(&text, stmt)
	j, err := c.session.launch(runner, stmt, strings.TrimSpace(text.String())+" &", c.tools, c.names, nil, "", c, c.jobs)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "nibash: %v\n", err)
		return interp.ExitStatus(1)
	}
	fmt.Fprintf(hc.Stdout, "%%%d", j.id)
	return nil
}

func errorText(err error) string {
	text := err.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
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
