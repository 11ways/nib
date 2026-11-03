package nibash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

const (
	jobsUsage = "jobs: usage: jobs [-lnprs] [jobspec ...] or jobs -x command [args]"
	waitUsage = "wait: usage: wait [-fn] [-p var] [id ...]"
)

// shellJobs are the jobs c's shell knows, as bash's job table holds them: for a call of the session's shell, which a session's calls are
// like the commands of one interactive bash, every job of the session but its own; for a shell of its own, a job's statement or a command
// run with background: true, which bash starts with an empty table, the jobs it started. disown takes a job from them.
func (c *call) shellJobs() []*job {
	var known []*job
	for _, j := range c.session.jobs.all() {
		shell := j.runner()
		c.session.jobs.mu.Lock()
		disowned := j.disowned
		c.session.jobs.mu.Unlock()
		switch {
		case disowned, shell == c:
			continue
		case c.subshell && (shell == nil || shell.origin != c):
			continue
		}
		known = append(known, j)
	}
	return known
}

// findJob is the job spec names among the jobs c's shell knows.
func (c *call) findJob(spec string) (*job, string) {
	return c.session.jobs.findAmong(spec, c.shellJobs())
}

// jobOfPid is the job of c's shell whose program pid leads, with that program; nil for a pid that is no child of the shell's.
func (c *call) jobOfPid(pid int) (*job, *child) {
	for _, j := range c.shellJobs() {
		if p := j.child(pid); p != nil {
			return j, p
		}
	}
	return nil, nil
}

// jobFlags reads the flag words of a builtin taking the single letters in letters, as bash's internal_getopt does, also joined (-lp), up
// to -- or the first word that is no flag; bad is the first letter it does not take.
func jobFlags(args []string, letters string) (set map[byte]bool, rest []string, bad byte) {
	set = map[byte]bool{}
	for len(args) > 0 {
		word := args[0]
		if word == "--" {
			return set, args[1:], 0
		}
		if len(word) < 2 || word[0] != '-' {
			break
		}
		for i := 1; i < len(word); i++ {
			if !strings.ContainsRune(letters, rune(word[i])) {
				return set, args, word[i]
			}
			set[word[i]] = true
		}
		args = args[1:]
	}
	return set, args, 0
}

// listJobs is bash's jobs builtin, in nibash's form: each job with its marker, its state (Running, Stopped, or Done with its exit status),
// its command and its log. -l adds the process groups it still runs, -p prints its spec, which nibash's $! gives too, -r and -s keep the
// running and the stopped jobs, -n the jobs whose end was not told yet, and -x runs a command with each job spec among its words replaced by
// the job's process group. Listing a job that ended tells its end, so no later call reports it again.
func (c *call) listJobs(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	flags, rest, bad := jobFlags(args, "lnprsx")
	if bad != 0 {
		fmt.Fprintf(hc.Stderr, "jobs: -%c: invalid option\n%s\n", bad, jobsUsage)
		return interp.ExitStatus(2)
	}
	if flags['x'] {
		return c.jobsExecute(ctx, rest)
	}
	known := c.shellJobs()
	status := 0
	listed := known
	if len(rest) > 0 {
		listed = nil
		for _, spec := range rest {
			j, problem := c.session.jobs.findAmong(spec, known)
			if j == nil {
				fmt.Fprintf(hc.Stderr, "jobs: %s\n", problem)
				status = 1
				continue
			}
			listed = append(listed, j)
		}
	}
	marks := c.session.jobs.marks(known)
	for _, j := range listed {
		running := j.running()
		stopped := running && j.stopped()
		c.session.jobs.mu.Lock()
		told := j.reported
		c.session.jobs.mu.Unlock()
		switch {
		case flags['r'] && (!running || stopped), flags['s'] && !stopped, flags['n'] && (running || told):
			continue
		case flags['p']:
			fmt.Fprintf(hc.Stdout, "%%%d\n", j.id)
		default:
			mark := marks[j]
			if mark == 0 {
				mark = ' '
			}
			state := "Running"
			switch {
			case stopped:
				state = "Stopped"
			case !running:
				state = fmt.Sprintf("Done (exit %d)", j.exit)
			}
			if flags['l'] {
				state = groupList(j) + " " + state
			}
			fmt.Fprintf(hc.Stdout, "[%d]%c %s  %s  (output: %s)\n", j.id, mark, state, j.command, j.log)
		}
		if !running {
			c.session.jobs.mu.Lock()
			j.reported = true
			c.session.jobs.mu.Unlock()
		}
	}
	return statusError(status)
}

// groupList names the process groups j still runs for jobs -l, the last it ran when none runs, and - for a job that ran no program.
func groupList(j *job) string {
	groups := j.runningPrograms()
	if len(groups) == 0 {
		j.mu.Lock()
		if n := len(j.programs); n > 0 {
			groups = []int{j.programs[n-1].pgid}
		}
		j.mu.Unlock()
	}
	if len(groups) == 0 {
		return "-"
	}
	names := make([]string, len(groups))
	for i, pgid := range groups {
		names[i] = strconv.Itoa(pgid)
	}
	return strings.Join(names, ",")
}

// jobsExecute is jobs -x: the command words run, as eval runs them in the shell, with each spec of a job the shell knows replaced by the
// process group of the job's program that runs, its last when none does; a word naming no job stays as written, as in bash.
func (c *call) jobsExecute(ctx context.Context, words []string) error {
	if len(words) == 0 {
		return nil
	}
	known := c.shellJobs()
	quoted := make([]string, len(words))
	for i, word := range words {
		if strings.HasPrefix(word, "%") {
			if j, _ := c.session.jobs.findAmong(word, known); j != nil {
				if group := strings.Split(groupList(j), ",")[0]; group != "-" {
					word = group
				}
			}
		}
		quoted[i] = singleQuote(word)
	}
	return interp.HandlerCtx(ctx).Builtin(ctx, []string{"eval", strings.Join(quoted, " ")})
}

// waitTarget is one id wait waits for: a job, or the program of a job a pid names; id is how -p names it.
type waitTarget struct {
	job   *job
	child *child
	id    string
}

// done is closed once the target ended.
func (t waitTarget) done() <-chan struct{} {
	if t.child != nil {
		return t.child.done
	}
	return t.job.done
}

// status is the target's exit status, once done.
func (t waitTarget) status() int {
	if t.child != nil {
		return t.child.status
	}
	return t.job.exit
}

// wait is bash's wait builtin: for the named jobs and pids, or for every job of the shell, within the call's time. A pid names the program
// of one of the shell's jobs that leads its process group, as jobs -l lists them. -n returns when the first of them ends, with its status;
// -f is taken, as nibash's wait always waits for an end; -p VAR, which callHandler sends on to saveWaited, names the job or pid whose status it
// returns. A signal the shell's trap takes ends a wait in the shell with 128 plus its number, as in bash.
func (c *call) wait(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	next, variable := false, ""
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		word := args[0]
		args = args[1:]
		if word == "--" {
			break
		}
		for i := 1; i < len(word); i++ {
			switch word[i] {
			case 'f':
			case 'n':
				next = true
			case 'p':
				variable = word[i+1:]
				if variable == "" {
					if len(args) == 0 {
						fmt.Fprintf(hc.Stderr, "wait: -p: option requires an argument\n%s\n", waitUsage)
						return interp.ExitStatus(2)
					}
					variable, args = args[0], args[1:]
				}
				i = len(word)
			default:
				fmt.Fprintf(hc.Stderr, "wait: -%c: invalid option\n%s\n", word[i], waitUsage)
				return interp.ExitStatus(2)
			}
		}
	}
	if variable != "" && !syntax.ValidName(variable) {
		fmt.Fprintf(hc.Stderr, "wait: `%s': not a valid identifier\n", variable)
		return interp.ExitStatus(1)
	}
	c.remember(ctx, "", -1)
	var targets []waitTarget
	status := 0
	if len(args) == 0 {
		for _, j := range c.shellJobs() {
			c.session.jobs.mu.Lock()
			told := j.reported
			c.session.jobs.mu.Unlock()
			// wait -n takes the next job to end, or one that ended and whose end was not told yet.
			if !next || j.running() || !told {
				targets = append(targets, waitTarget{job: j, id: fmt.Sprintf("%%%d", j.id)})
			}
		}
		if !slices.ContainsFunc(targets, func(t waitTarget) bool { return t.job.running() }) {
			c.noteWaitless("no running job")
		}
		if next && len(targets) == 0 {
			return interp.ExitStatus(127)
		}
	}
	for _, id := range args {
		if strings.HasPrefix(id, "%") {
			j, problem := c.findJob(id)
			if j == nil {
				fmt.Fprintf(hc.Stderr, "wait: %s\n", problem)
				c.noteWaitless("no job " + id)
				status = 127
				continue
			}
			targets = append(targets, waitTarget{job: j, id: id})
			continue
		}
		pid, ok := pidArgument(id)
		if !ok || pid <= 0 {
			fmt.Fprintf(hc.Stderr, "wait: `%s': not a pid or valid job spec\n", id)
			status = 127
			continue
		}
		j, p := c.jobOfPid(pid)
		if j == nil {
			fmt.Fprintf(hc.Stderr, "wait: pid %d is not a child of this shell\n", pid)
			status = 127
			continue
		}
		targets = append(targets, waitTarget{job: j, child: p, id: strconv.Itoa(pid)})
	}
	if len(targets) == 0 {
		return statusError(status)
	}
	c.mu.Lock()
	for _, t := range targets {
		c.waited = append(c.waited, t.job)
	}
	c.mu.Unlock()
	stop, signalled := c.stopping(), (<-chan struct{})(nil)
	if c.inShell(ctx) {
		signalled = c.interrupted()
	}
	if next {
		got, err := c.awaitFirst(ctx, targets, stop, signalled)
		if err != nil {
			return c.waitStopped(ctx, err)
		}
		c.remember(ctx, got.id, got.status())
		return statusError(got.status())
	}
	all := len(args) == 0
	for _, t := range targets {
		if err := c.awaitOne(ctx, t, stop, signalled); err != nil {
			return c.waitStopped(ctx, err)
		}
		c.remember(ctx, t.id, t.status())
		if !all {
			status = t.status()
		}
	}
	return statusError(status)
}

// errSignalled is the error of a wait a signal the shell's trap takes ended.
var errSignalled = errors.New("signalled")

// awaitOne waits for t within ctx until signalled is closed, or stop is and the halt reaches the wait's interpreter; a job's end the wait saw
// is told.
func (c *call) awaitOne(ctx context.Context, t waitTarget, stop, signalled <-chan struct{}) error {
	for {
		var err error
		if t.child == nil {
			err = c.session.jobs.await(ctx, t.job, stop, signalled)
		} else {
			select {
			case <-t.child.done:
			case <-ctx.Done():
				err = ctx.Err()
			case <-signalled:
				err = errSignalled
			case <-stop:
				err = errHalted
			}
		}
		if !errors.Is(err, errHalted) || c.haltReaching(ctx) != nil {
			return err
		}
		stop = nil
	}
}

// awaitFirst waits for the first of targets to end, as wait -n does.
func (c *call) awaitFirst(ctx context.Context, targets []waitTarget, stop, signalled <-chan struct{}) (waitTarget, error) {
	for {
		for _, t := range targets {
			if isClosed(t.done()) {
				if t.child == nil {
					c.session.jobs.tell(t.job)
				}
				return t, nil
			}
		}
		ended := make(chan struct{})
		quit := make(chan struct{})
		for _, t := range targets {
			go func(done <-chan struct{}) {
				select {
				case <-done:
					select {
					case ended <- struct{}{}:
					case <-quit:
					}
				case <-quit:
				}
			}(t.done())
		}
		select {
		case <-ended:
			close(quit)
		case <-ctx.Done():
			close(quit)
			return waitTarget{}, ctx.Err()
		case <-signalled:
			close(quit)
			return waitTarget{}, errSignalled
		case <-stop:
			close(quit)
			if c.haltReaching(ctx) != nil {
				return waitTarget{}, errHalted
			}
			stop = nil
		}
	}
}

// waitStopped is how a wait ends that err stopped: a halt ends the interpreter, a signal the trap takes returns 128 plus its number, and the
// call's own end returns 130, as bash's wait does when a signal interrupts it; the interpreter takes an error for an internal one.
func (c *call) waitStopped(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errHalted):
		if h := c.haltReaching(ctx); h != nil {
			return c.exitHere(ctx, h.status)
		}
	case errors.Is(err, errSignalled):
		return interp.ExitStatus(128 + int(c.firstPending()))
	}
	return interp.ExitStatus(waitInterrupted)
}

// remember keeps, for the interpreter ctx runs in, the id the last wait returned the status of, which saveWaited gives wait -p's variable;
// "" for none.
func (c *call) remember(ctx context.Context, id string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitedIDs == nil {
		c.waitedIDs = map[*interp.Runner]string{}
	}
	if id == "" && status < 0 {
		delete(c.waitedIDs, runnerOf(ctx))
		return
	}
	c.waitedIDs[runnerOf(ctx)] = id
}

// waitWithVariable rewrites wait -p VAR into the wait and an assignment to VAR of the id whose status it returned, unset when none, as an
// exec handler cannot set the shell's variables; nil for any other words. args begin with wait's marked name.
func waitWithVariable(args []string) []string {
	variable := ""
	rest := []string{args[0]}
	for i := 1; i < len(args); i++ {
		word := args[i]
		if word == "--" || len(word) < 2 || word[0] != '-' {
			rest = append(rest, args[i:]...)
			break
		}
		if at := strings.IndexByte(word, 'p'); at > 0 {
			name := word[at+1:]
			if name == "" && i+1 < len(args) {
				i++
				name = args[i]
			}
			variable = name
			if before := word[:at]; before != "-" {
				rest = append(rest, before)
			}
			continue
		}
		rest = append(rest, word)
	}
	if variable == "" || !syntax.ValidName(variable) {
		return nil
	}
	quoted := make([]string, len(rest))
	for i, word := range rest {
		quoted[i] = singleQuote(word)
	}
	return []string{"eval", "unset " + variable + "\n" + strings.Join(quoted, " ") + "\n" + singleQuote(marker+internalWaited) + " " + variable}
}

// saveWaited is the assignment wait -p VAR ends with: VAR set to the id the wait before it in ctx's interpreter returned the status of, and
// that status kept for $?.
func (c *call) saveWaited(ctx context.Context, args []string) []string {
	status := interp.HandlerCtx(ctx).LastExitStatus
	c.mu.Lock()
	id, ok := c.waitedIDs[runnerOf(ctx)]
	c.mu.Unlock()
	keep := fmt.Sprintf("(exit %d)", status)
	if !ok || len(args) < 2 || id == "" {
		return []string{"eval", keep}
	}
	return []string{"eval", args[1] + "=" + singleQuote(id) + "\n" + keep}
}

// disown is bash's disown builtin: it takes the named jobs, the current one without a name, or with -a all of the shell's (-r only those
// running) from the shell's table, so no wait, kill or report names them and the session's end does not end them; -h leaves them in the
// table but running past that end. A pid names the job whose program leads it.
func (c *call) disown(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	flags, rest, bad := jobFlags(args, "ahr")
	if bad != 0 {
		fmt.Fprintf(hc.Stderr, "disown: -%c: invalid option\ndisown: usage: disown [-h] [-ar] [jobspec ... | pid ...]\n", bad)
		return interp.ExitStatus(2)
	}
	known := c.shellJobs()
	var chosen []*job
	status := 0
	switch {
	case len(rest) == 0 && (flags['a'] || flags['r']):
		for _, j := range known {
			if !flags['r'] || j.running() {
				chosen = append(chosen, j)
			}
		}
	case len(rest) == 0:
		j, _ := c.session.jobs.findAmong("%+", known)
		if j == nil {
			fmt.Fprintln(hc.Stderr, "disown: current: no such job")
			return interp.ExitStatus(1)
		}
		chosen = []*job{j}
	}
	for _, spec := range rest {
		var j *job
		if pid, ok := pidArgument(spec); ok && !strings.HasPrefix(spec, "%") {
			j, _ = c.jobOfPid(pid)
		} else {
			j, _ = c.session.jobs.findAmong(spec, known)
		}
		if j == nil {
			fmt.Fprintf(hc.Stderr, "disown: %s: no such job\n", spec)
			status = 1
			continue
		}
		chosen = append(chosen, j)
	}
	c.session.jobs.mu.Lock()
	for _, j := range chosen {
		if flags['h'] {
			j.kept = true
		} else {
			j.disowned, j.reported = true, true
		}
	}
	c.session.jobs.mu.Unlock()
	return statusError(status)
}

// noJobControl is fg, bg and suspend, which a bash that runs a command line, as each call is, refuses as nibash does: such a shell has no
// terminal to give a job to.
func noJobControl(ctx context.Context, name string) error {
	message := name + ": no job control"
	if name == internalSuspend {
		message = "suspend: cannot suspend: no job control"
	}
	fmt.Fprintln(interp.HandlerCtx(ctx).Stderr, message)
	return interp.ExitStatus(1)
}

// waitInterrupted is the status of a wait its call stopped: bash's for a wait a signal interrupts, 128 plus SIGINT's number.
const waitInterrupted = 128 + int(syscall.SIGINT)
