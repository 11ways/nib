package nibash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/platform"
)

// identities are the processes kill must not reach as programs: nib, whose pid is $$, the process group nib is in, which mostly is its
// client's too, and the client that started nib, with its process group.
type identities struct {
	self, group, client, clientGroup int
}

func currentIdentities() identities {
	id := identities{self: os.Getpid(), group: platform.Group(), client: os.Getppid()}
	if group, ok := platform.GroupOf(id.client); ok {
		id.clientGroup = group
	}
	return id
}

const killUsage = "kill: usage: kill [-s sigspec | -n signum | -sigspec] pid | jobspec ... or kill -l [sigspec]"

// kill is bash's kill builtin, for real processes and nibash's jobs, with bash's options, signal specs, messages and exit status. Where
// bash's would reach its own shell, the shell is the call (see killShell). It never signals nib's client, its process group or every
// process (-1), which would end the session; signal 0, which only asks whether they exist, goes through.
func (c *call) kill(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	sig, spec, valid := syscall.SIGTERM, "", true
	named, listing := false, false
	name := func(s string) {
		spec, named = s, true
		sig, valid = decodeSignal(s)
	}
options:
	for len(args) > 0 {
		word := args[0]
		switch {
		case word == "-l" || word == "-L":
			listing = true
		case word == "-s" || word == "-n":
			if len(args) < 2 {
				fmt.Fprintf(hc.Stderr, "kill: %s: option requires an argument\n", word)
				return interp.ExitStatus(1)
			}
			name(args[1])
			args = args[1:]
		case len(word) > 2 && (word[:2] == "-s" && isLetter(word[2]) || word[:2] == "-n" && isDigit(word[2])):
			name(word[2:])
		case word == "--":
			args = args[1:]
			break options
		case word == "-?":
			fmt.Fprintln(hc.Stderr, killUsage)
			return interp.ExitStatus(2)
		case strings.HasPrefix(word, "-") && !named:
			// Only the first such word is a signal, so the next can be a process group: kill -9 -1234.
			name(word[1:])
		default:
			break options
		}
		args = args[1:]
	}
	if listing {
		return statusError(listSignals(hc.Stdout, hc.Stderr, args))
	}
	if !valid {
		fmt.Fprintf(hc.Stderr, "kill: %s: invalid signal specification\n", spec)
		return interp.ExitStatus(1)
	}
	if len(args) == 0 {
		fmt.Fprintln(hc.Stderr, killUsage)
		return interp.ExitStatus(2)
	}
	halted := c.halt() != nil
	id := c.session.shell.identify()
	sent := false
	for _, word := range args {
		if err := c.killOne(word, sig, id); err != nil {
			fmt.Fprintf(hc.Stderr, "kill: %v\n", err)
			continue
		}
		sent = true
	}
	// A kill that ended the shell it runs in exits with the shell's status.
	if h := c.halt(); h != nil && !halted {
		return interp.ExitStatus(h.status)
	}
	if !sent {
		return interp.ExitStatus(1)
	}
	return nil
}

// aim is what a kill of a pid reaches.
type aim int

const (
	aimProcess aim = iota
	aimGroup
	aimShell
	aimShellGroup
)

// aimAt is what kill sends sig to for pid, as kill(2) reads it: 0 and nib's own process group are the shell's group, nib's pid is the shell
// (see killShell), and a negative number below -1 a process group; the error refuses every process (-1), nib's client and its group, but
// for signal 0.
func aimAt(pid int, sig syscall.Signal, id identities) (aim, error) {
	switch {
	case pid == 0 || pid < -1 && -pid == id.group:
		return aimShellGroup, nil
	case pid == id.self:
		return aimShell, nil
	case sig == 0:
		// Signal 0 only asks whether the process exists, which may be asked of any.
	case pid == -1:
		return 0, errors.New("(-1) - nibash sends no signal to every process: that would end nib's client and the session")
	case pid == id.client:
		return 0, fmt.Errorf("(%d) - nibash sends no signal to nib's client: that would end the session", pid)
	case pid < -1 && -pid == id.clientGroup:
		return 0, fmt.Errorf("(%d) - nibash sends no signal to the process group of nib's client: that would end the session", pid)
	}
	if pid < -1 {
		return aimGroup, nil
	}
	return aimProcess, nil
}

// killOne sends sig to what word names: a pid, a process group as its negated id, or a job spec; the error is bash's complaint without its
// "kill: ".
func (c *call) killOne(word string, sig syscall.Signal, id identities) error {
	if strings.HasPrefix(word, "%") {
		return c.killJob(word, sig)
	}
	pid, ok := pidArgument(word)
	if !ok {
		return fmt.Errorf("`%s': not a pid or valid job spec", word)
	}
	target, err := aimAt(pid, sig, id)
	switch {
	case err != nil:
		return err
	case target == aimShellGroup:
		err = c.killShell(sig, true)
	case target == aimShell:
		err = c.killShell(sig, false)
	case target == aimGroup:
		err = platform.SignalGroup(-pid, sig)
	default:
		err = platform.Signal(pid, sig)
	}
	if err != nil {
		return fmt.Errorf("(%d) - %s", pid, errorText(err))
	}
	return nil
}

// killJob sends sig to the job spec names; a job that ended is no target, so kill -0 %N says whether it still runs.
func (c *call) killJob(spec string, sig syscall.Signal) error {
	j, problem := c.session.jobs.find(spec)
	if j == nil {
		return errors.New(problem)
	}
	if !j.running() {
		return fmt.Errorf("%s: job has terminated", spec)
	}
	if sig != 0 {
		j.signal(sig)
	}
	return nil
}

// killShell sends sig to the shell, which in nibash is no process but the call whose command line runs kill, the root of a job's: $$ is
// nib's pid, and nib's process group is mostly its client's too. A signal that ends a process ends the call, as it would end bash, so nothing
// after it runs; not SIGQUIT, which bash ignores, nor one that stops a process, since a stopped call would only hang. With group the programs
// the call and the jobs it started still run get sig too, as members of the shell's process group would. The error is ESRCH when the shell
// is a call that ended, as a job's can be.
func (c *call) killShell(sig syscall.Signal, group bool) error {
	root := c.root()
	alive := root.running()
	if !group && !alive {
		return syscall.ESRCH
	}
	if sig == 0 {
		return nil
	}
	if alive && sig != syscall.SIGQUIT && platform.Ends(sig) {
		target := "the shell"
		if group {
			target = "the shell's process group"
		}
		root.haltWith(&stopped{reason: "kill sent " + signalLabel(sig) + " to " + target, status: 128 + int(sig)})
	}
	if !group {
		return nil
	}
	if own := root.ownJob(); own != nil {
		own.signal(sig)
	} else {
		root.signalGroups(sig)
	}
	for _, j := range c.session.jobsUnder(root) {
		j.signal(sig)
	}
	return nil
}

// ownJob is the job c runs, as a job's statement or as a call that went on as one; nil for a call in its request.
func (c *call) ownJob() *job {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.job
}

// signalGroups sends sig to every process group c started that still runs.
func (c *call) signalGroups(sig syscall.Signal) {
	c.mu.Lock()
	groups := make([]int, 0, len(c.groups))
	for pgid := range c.groups {
		groups = append(groups, pgid)
	}
	c.mu.Unlock()
	for _, pgid := range groups {
		_ = platform.SignalGroup(pgid, sig)
	}
}

// jobsUnder are the running jobs a statement of root's command started, or a job of theirs did, root's own job left out.
func (sess *session) jobsUnder(root *call) []*job {
	var under []*job
	for _, j := range sess.jobs.all() {
		if shell := j.runner(); shell != nil && shell != root && j.running() && shell.root() == root {
			under = append(under, j)
		}
	}
	return under
}

// signalNumbers are the signals by their names without SIG.
var signalNumbers = sync.OnceValue(func() map[string]syscall.Signal {
	numbers := map[string]syscall.Signal{}
	for n := syscall.Signal(1); n <= platform.LastSignal(); n++ {
		if name := platform.SignalName(n); name != "" {
			numbers[name] = n
		}
	}
	return numbers
})

// decodeSignal is the signal spec names, as bash's kill reads one: a number up to the OS's last signal, or a name in any case with or without
// SIG, EXIT standing for 0 and RTMIN+N for any real-time signal.
func decodeSignal(spec string) (syscall.Signal, bool) {
	if n, ok := legalNumber(spec); ok {
		return syscall.Signal(n), n >= 0 && n <= int64(platform.LastSignal())
	}
	name := strings.ToUpper(spec)
	if name == "EXIT" {
		return 0, true
	}
	name = strings.TrimPrefix(name, "SIG")
	if offset, ok := strings.CutPrefix(name, "RTMIN+"); ok {
		first, last := platform.Realtime()
		n, ok := legalNumber(offset)
		return first + syscall.Signal(n), first != 0 && ok && n >= 0 && n <= int64(last-first)
	}
	sig, ok := signalNumbers()[name]
	return sig, ok
}

// trapSignals are the names bash's trap knows past the real signals, which kill -l numbers after the last one but kill cannot send.
var trapSignals = []string{"DEBUG", "ERR", "RETURN"}

// listSignals is kill -l: with no specs bash's table of every signal, else each number's name, an exit status past 128 standing for the
// signal that ended a process, and each name's number. It returns 1 when a spec named no signal.
func listSignals(out, errs io.Writer, specs []string) int {
	last := platform.LastSignal()
	if len(specs) == 0 {
		column := 0
		for n := syscall.Signal(1); n <= last; n++ {
			name := platform.SignalName(n)
			if name == "" {
				continue
			}
			fmt.Fprintf(out, "%2d) SIG%s", n, name)
			if column++; column < 5 {
				fmt.Fprint(out, "\t")
			} else {
				fmt.Fprint(out, "\n")
				column = 0
			}
		}
		if column != 0 {
			fmt.Fprint(out, "\n")
		}
		return 0
	}
	status := 0
	invalid := func(spec string) {
		fmt.Fprintf(errs, "kill: %s: invalid signal specification\n", spec)
		status = 1
	}
	for _, spec := range specs {
		if n, ok := legalNumber(spec); ok {
			if n > 128 {
				n -= 128
			}
			switch name := platform.SignalName(syscall.Signal(n)); {
			case n < 0 || n > int64(last):
				invalid(spec)
			case n == 0:
				fmt.Fprintln(out, "EXIT")
			case name != "":
				// A number the OS keeps for itself, as glibc does 32 and 33, prints nothing, as in bash.
				fmt.Fprintln(out, name)
			}
			continue
		}
		if sig, ok := decodeSignal(spec); ok {
			fmt.Fprintln(out, int(sig))
			continue
		}
		if at := indexFold(trapSignals, spec); at >= 0 {
			fmt.Fprintln(out, int(last)+1+at)
			continue
		}
		invalid(spec)
	}
	return status
}

// indexFold is where names holds name, in any case; -1 when it does not.
func indexFold(names []string, name string) int {
	for i, candidate := range names {
		if strings.EqualFold(candidate, name) {
			return i
		}
	}
	return -1
}

// signalLabel names sig as the closing notes do, SIGTERM, or by its number for one the OS does not name.
func signalLabel(sig syscall.Signal) string {
	if name := platform.SignalName(sig); name != "" {
		return "SIG" + name
	}
	return fmt.Sprintf("signal %d", int(sig))
}

// pidArgument is the pid word names as bash's kill reads one: a decimal number legalNumber takes that fits a pid_t, with something after
// its sign.
func pidArgument(word string) (int, bool) {
	if strings.TrimPrefix(word, "-") == "" {
		return 0, false
	}
	n, ok := legalNumber(word)
	if !ok || n < math.MinInt32 || n > math.MaxInt32 {
		return 0, false
	}
	return int(n), true
}

// legalNumber is the integer s holds as bash's legal_number reads one: decimal, as strtoimax takes it after white space and a sign, followed
// by nothing but blanks.
func legalNumber(s string) (int64, bool) {
	i := 0
	for i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0 {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := i
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == digits {
		return 0, false
	}
	n, err := strconv.ParseInt(s[start:i], 10, 64)
	if err != nil {
		return 0, false
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return n, i == len(s)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
