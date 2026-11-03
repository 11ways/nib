package nibash

import (
	"context"
	"fmt"
	"maps"

	"slices"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/platform"
)

// trapTable is what one interpreter's traps do, by bash's trap numbers (decodeTrap): the command a trap runs, or "" for a signal it ignores;
// a number it holds nothing for does what it would by default.
type trapTable map[int]string

// trapsOnDeath are the signals bash catches to run its EXIT trap before it dies of them; any other that ends it ends it at once.
var trapsOnDeath = map[string]bool{"HUP": true, "INT": true, "ILL": true, "TRAP": true, "ABRT": true, "EMT": true, "FPE": true, "BUS": true,
	"SEGV": true, "SYS": true, "PIPE": true, "ALRM": true, "TERM": true, "XCPU": true, "XFSZ": true, "VTALRM": true, "LOST": true, "USR1": true,
	"USR2": true}

const trapUsage = "trap: usage: trap [-Plp] [[action] signal_spec ...]"

// decodeTrap is the trap number spec names: 0 for EXIT, a signal's own, and DEBUG, ERR and RETURN numbered after the last signal, as bash
// numbers them.
func decodeTrap(spec string) (int, bool) {
	if at := indexFold(trapSignals, spec); at >= 0 {
		return int(platform.LastSignal()) + 1 + at, true
	}
	sig, ok := decodeSignal(spec)
	return int(sig), ok
}

// trapName names trap number n as trap -p prints it.
func trapName(n int) string {
	last := int(platform.LastSignal())
	switch {
	case n == 0:
		return "EXIT"
	case n > last:
		return trapSignals[n-last-1]
	}
	return signalLabel(syscall.Signal(n))
}

// unrun are the traps mvdan cannot run, DEBUG's and RETURN's, which nibash refuses rather than set traps that never run.
func unrun(n int) bool {
	last := int(platform.LastSignal())
	return n == last+1 || n == last+3
}

// mvdanTrap is the name mvdan's own trap builtin takes for a trap nibash leaves it to run, ERR's; "" for any other. nibash runs the EXIT trap
// itself (exiting).
func mvdanTrap(n int) string {
	if n == int(platform.LastSignal())+2 {
		return "ERR"
	}
	return ""
}

// trap is bash's trap builtin. A trap on a signal is nibash's own and runs when a kill sends its signal to the shell (deliver); an EXIT or ERR
// trap goes to mvdan's builtin too, which runs it; DEBUG and RETURN traps, which mvdan cannot run, are refused.
func (c *call) trap(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	list, show, actions := false, false, false
options:
	for len(args) > 0 {
		word := args[0]
		switch {
		case word == "--":
			args = args[1:]
			break options
		case len(word) < 2 || word[0] != '-':
			break options
		}
		for _, flag := range word[1:] {
			switch flag {
			case 'l':
				list = true
			case 'p':
				show = true
			case 'P':
				actions = true
			default:
				fmt.Fprintf(hc.Stderr, "trap: -%c: invalid option\n%s\n", flag, trapUsage)
				return interp.ExitStatus(2)
			}
		}
		args = args[1:]
	}
	invalid := func(spec string) {
		fmt.Fprintf(hc.Stderr, "trap: %s: invalid signal specification\n", spec)
	}
	switch {
	case list:
		return statusError(listSignals(hc.Stdout, hc.Stderr, nil))
	case actions && len(args) == 0:
		fmt.Fprintln(hc.Stderr, "trap: -P requires at least one signal name")
		return interp.ExitStatus(2)
	case actions || show || len(args) == 0:
		table := c.trapsOf(ctx)
		numbers := slices.Sorted(maps.Keys(table))
		status := 0
		if len(args) > 0 {
			numbers = nil
			for _, spec := range args {
				n, ok := decodeTrap(spec)
				if !ok {
					invalid(spec)
					status = 1
					continue
				}
				numbers = append(numbers, n)
			}
		}
		for _, n := range numbers {
			action, set := table[n]
			switch {
			case !set:
			case actions:
				fmt.Fprintln(hc.Stdout, action)
			default:
				fmt.Fprintf(hc.Stdout, "trap -- %s %s\n", singleQuote(action), trapName(n))
			}
		}
		return statusError(status)
	}
	action, specs, reset := args[0], args[1:], args[0] == "-"
	if len(args) == 1 {
		// A lone signal, as in trap TERM, resets it.
		if _, ok := decodeTrap(args[0]); !ok || args[0] == "-" {
			fmt.Fprintln(hc.Stderr, trapUsage)
			return interp.ExitStatus(2)
		}
		action, specs, reset = "", args, true
	}
	status := 0
	for _, spec := range specs {
		n, ok := decodeTrap(spec)
		switch {
		case !ok:
			invalid(spec)
			status = 1
			continue
		case unrun(n):
			fmt.Fprintf(hc.Stderr, "trap: %s: nibash runs no DEBUG or RETURN trap\n", spec)
			status = 1
			continue
		}
		if name := mvdanTrap(n); name != "" {
			// mvdan takes an empty command as no trap, which runs the same nothing an ignored EXIT or ERR does.
			given := action
			if reset {
				given = "-"
			}
			if err := hc.Builtin(ctx, []string{"trap", "--", given, name}); err != nil {
				return err
			}
		}
		c.setTrap(ctx, n, action, reset)
	}
	return statusError(status)
}

// singleQuote is s as bash quotes it for trap -p: in single quotes, each single quote of its own closed, escaped and opened again.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellKey is the key of the call's own interpreter among its traps.
func (c *call) shellKey() *interp.Runner { return c.runner }

// trapsOf is a copy of the traps of ctx's interpreter: its own, or for a subshell that set none the shell's, as bash's trap shows them there.
func (c *call) trapsOf(ctx context.Context) trapTable {
	c.mu.Lock()
	defer c.mu.Unlock()
	table, ok := c.traps[runnerOf(ctx)]
	if !ok {
		table = c.traps[c.shellKey()]
	}
	copied := trapTable{}
	for n, action := range table {
		copied[n] = action
	}
	return copied
}

// setTrap sets trap number n of ctx's interpreter to action, or resets it; a subshell's first trap starts from a copy of the shell's without
// its EXIT trap, which a bash subshell does not run.
func (c *call) setTrap(ctx context.Context, n int, action string, reset bool) {
	key := runnerOf(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.traps == nil {
		c.traps = map[*interp.Runner]trapTable{}
	}
	table, ok := c.traps[key]
	if !ok {
		table = trapTable{}
		for n, action := range c.traps[c.shellKey()] {
			table[n] = action
		}
		if key != c.shellKey() {
			delete(table, 0)
		}
		c.traps[key] = table
	}
	if reset {
		delete(table, n)
		return
	}
	table[n] = action
}

// exitWithTrap is how a subshell exits with status by exit's words: after the EXIT trap it set, which runs once with $? that status, as
// bash runs it; exit's words themselves for the shell, whose EXIT trap mvdan runs (exiting), and for a subshell that set none.
func (c *call) exitWithTrap(ctx context.Context, exit []string, status int) []string {
	if c.inShell(ctx) {
		return exit
	}
	key := runnerOf(ctx)
	c.mu.Lock()
	action, ok := c.traps[key][0]
	if ok {
		delete(c.traps[key], 0)
	}
	c.mu.Unlock()
	if !ok || action == "" {
		return exit
	}
	quoted := make([]string, len(exit))
	for i, word := range exit {
		quoted[i] = singleQuote(word)
	}
	return []string{"eval", fmt.Sprintf("(exit %d)\n%s\n%s", status, action, strings.Join(quoted, " "))}
}

// inherit gives c's shell the signals the shell of origin ignores, as a bash subshell keeps them while its traps go back to their defaults.
func (c *call) inherit(origin *call) {
	ignored := trapTable{}
	origin.mu.Lock()
	for n, action := range origin.traps[origin.shellKey()] {
		if action == "" && n > 0 && mvdanTrap(n) == "" {
			ignored[n] = ""
		}
	}
	origin.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.traps = map[*interp.Runner]trapTable{c.shellKey(): ignored}
}

// shellTrap is what c's shell does on sig: the command its trap runs, or "" for a signal it ignores; ok is false for one it meets as it would
// by default, as it meets SIGKILL and SIGSTOP, which no trap takes.
func (c *call) shellTrap(sig syscall.Signal) (string, bool) {
	if !platform.Catchable(sig) {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	action, ok := c.traps[c.shellKey()][int(sig)]
	return action, ok
}

// ignoredHere are the signals ctx's interpreter ignores, the shell's or a subshell's own, which the programs it starts ignore too, as a
// program bash starts inherits them.
func (c *call) ignoredHere(ctx context.Context) []syscall.Signal {
	var signals []syscall.Signal
	for n, action := range c.trapsOf(ctx) {
		if sig := syscall.Signal(n); action == "" && n > 0 && n <= int(platform.LastSignal()) && platform.Catchable(sig) {
			signals = append(signals, sig)
		}
	}
	slices.Sort(signals)
	return signals
}

// sending is how a signal reaches a shell: group sends it to the shell's process group, quiet leaves the halt's note out, for a job's shell,
// elsewhere marks a kill another interpreter or call sent, and target names what the kill reached in the note.
type sending struct {
	group, quiet, elsewhere bool
	target                  string
}

// deliver is what sig, sent to c's shell, does: what the shell's trap says, else its default, which ends the shell on a signal that ends a
// process, with its EXIT trap run where bash's would run, but not on SIGQUIT for a shell of its own, which bash ignores. Sent to the group,
// the signal reaches the shell's subshells too, which keep the shell's ignores but not its traps.
func (c *call) deliver(sig syscall.Signal, how sending) {
	if sig == 0 || !c.running() {
		return
	}
	halt := func(scope haltScope) {
		c.haltWith(&stopped{reason: "kill sent " + signalLabel(sig) + " to " + how.target, status: 128 + int(sig), quiet: how.quiet, scope: scope,
			trap: trapsOnDeath[platform.SignalName(sig)], elsewhere: how.elsewhere})
	}
	action, trapped := c.shellTrap(sig)
	switch {
	case trapped && action == "":
	case trapped:
		c.pend(sig)
		if how.group && platform.Ends(sig) {
			halt(itsSubshells)
		}
	case !platform.Ends(sig) || sig == syscall.SIGQUIT && c.origin == nil:
	case how.group:
		halt(everywhere)
	default:
		halt(theShell)
	}
}

// runsPrograms says whether a program c started still runs.
func (c *call) runsPrograms() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.groups) > 0
}

// pend notes a signal the shell's trap takes, which runs at the shell's next command, and wakes what waits in the shell.
func (c *call) pend(sig syscall.Signal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, sig)
	if c.attention != nil {
		close(c.attention)
		c.attention = nil
	}
}

// closed is a channel that is closed, for a wait to find at once.
var closed = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

// interrupted is closed when the shell has a signal its trap takes waiting to run; a wait in the shell returns then, as bash's does.
func (c *call) interrupted() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 {
		return closed
	}
	if c.attention == nil {
		c.attention = make(chan struct{})
	}
	return c.attention
}

// firstPending is the first signal waiting for its trap to run, 0 for none.
func (c *call) firstPending() syscall.Signal {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return 0
	}
	return c.pending[0]
}

// takeTraps is the commands of the traps the shell took signals with since it last ran them, in the order the signals came, each the trap
// set when it runs, as bash's; "" for none.
func (c *call) takeTraps() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return ""
	}
	table := c.traps[c.shellKey()]
	var commands []string
	for _, sig := range c.pending {
		if action := table[int(sig)]; action != "" {
			commands = append(commands, action)
		}
	}
	c.pending = nil
	return strings.Join(commands, "\n")
}

// withTraps is args run after the traps waiting in the shell, as one eval of the traps and args quoted, so a trap's exit ends the shell
// before args runs and args leaves $? as it would; nil when no trap waits.
func (c *call) withTraps(args []string) []string {
	commands := c.takeTraps()
	if commands == "" {
		return nil
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = singleQuote(arg)
	}
	return []string{"eval", commands + "\n" + strings.Join(quoted, " ")}
}

// exiting is what the shell runs as it exits, which mvdan runs nibash's end command for (startShell): the traps of signals it took that have
// not run yet, then its EXIT trap, unless a signal that ends bash before its EXIT trap runs ended it. What runs in the shell from now on is
// the trap's.
func (c *call) exiting(ctx context.Context) []string {
	if !c.inShell(ctx) {
		return []string{":"}
	}
	c.markExited()
	if h := c.halt(); h != nil && !h.trap && h.scope != itsSubshells {
		return []string{":"}
	}
	commands := c.takeTraps()
	if action, _ := c.shellTrap(0); action != "" {
		commands = strings.TrimPrefix(commands+"\n"+action, "\n")
	}
	if commands == "" {
		return []string{":"}
	}
	return []string{"eval", commands}
}
