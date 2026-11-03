package nibash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"mvdan.cc/sh/v3/interp"
)

// stopped is what ended a call's shell before its command line did, a directory change whose failure the command left unhandled or a signal:
// from then on every command the halt reaches exits with status instead of running.
type stopped struct {
	// reason is what ended the shell, as the call's closing note names it.
	reason string
	status int
	// quiet leaves out the note, for a job a kill ended, whose status says so already.
	quiet bool
	scope haltScope
	// trap is whether the shell's EXIT trap runs after the halt, as bash's does after a failed command under set -e and after the signals it
	// catches to run it (trapsOnDeath).
	trap bool
	// elsewhere is set for a kill another interpreter or call sent, which the shell may meet while it waits for a program.
	elsewhere bool
}

// haltScope is which of a call's interpreters a halt ends: bash's signal to $$ reaches the shell and not its subshells, one to its process
// group reaches them all, and one the shell takes with a trap only the subshells, which bash's do not inherit.
type haltScope int

const (
	everywhere haltScope = iota
	theShell
	itsSubshells
)

func (s *stopped) Error() string {
	return fmt.Sprintf("[%s, so nothing after it ran]", s.reason)
}

// haltWith halts the call with s, unless something halted it already; a halt of the shell or of everything replaces one that reached only
// subshells.
func (c *call) haltWith(s *stopped) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.halted != nil && (c.halted.scope != itsSubshells || s.scope == itsSubshells) {
		return
	}
	c.halted = s
	if c.halting == nil {
		c.halting = make(chan struct{})
	}
	select {
	case <-c.halting:
	default:
		close(c.halting)
	}
}

// stopping is closed once the call is halted, for what waits inside the shell rather than in a program.
func (c *call) stopping() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.halting == nil {
		c.halting = make(chan struct{})
	}
	return c.halting
}

// halt is what halted the call, nil while nothing did.
func (c *call) halt() *stopped {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.halted
}

// haltReaching is the halt a command in ctx's interpreter exits with, nil when it runs: nothing halted the call, the halt does not reach that
// interpreter, or the shell exited already and runs its EXIT trap, which the halt lets run.
func (c *call) haltReaching(ctx context.Context) *stopped {
	shell := c.inShell(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.halted
	switch {
	case h == nil:
		return nil
	case c.exited && h.trap:
		// The shell runs its EXIT trap, and what the trap starts.
		return nil
	case shell && h.scope == itsSubshells, !shell && h.scope == theShell:
		return nil
	}
	return h
}

// exitHere ends the interpreter ctx is in with status as its exit builtin would, so the shell's EXIT trap runs where the halt lets it.
func (c *call) exitHere(ctx context.Context, status int) error {
	if c.inShell(ctx) {
		c.markExited()
	}
	return interp.HandlerCtx(ctx).Builtin(ctx, []string{"exit", strconv.Itoa(status)})
}

// markExited notes that the shell runs its exit, so what runs in it from now on is its EXIT trap.
func (c *call) markExited() {
	c.mu.Lock()
	c.exited = true
	c.mu.Unlock()
}

// haltNote is the note that something halted the call, with the halt's exit status instead of err; "" and err for any other call, and for a
// halt that reached only subshells. A quiet halt has no note and leaves the status of the call's last command, unless the halt refused a
// redirect, whose error the run ends with.
func (c *call) haltNote(err error) (string, error) {
	h := c.halt()
	var refused *stopped
	switch {
	case h == nil || h.scope == itsSubshells:
		return "", err
	case !h.quiet:
		return h.Error(), interp.ExitStatus(h.status)
	case errors.As(err, &refused):
		return "", interp.ExitStatus(h.status)
	}
	return "", err
}

// jobError is err as a job's log ends with it: for a call a halt ended, its note written to out and its status instead; a note of failures
// its exit status does not tell written to out too.
func (c *call) jobError(err error, out io.Writer) error {
	note, err := c.haltNote(err)
	if note != "" {
		fmt.Fprintln(out, note)
	}
	status := 0
	var exit interp.ExitStatus
	if errors.As(err, &exit) {
		status = int(exit)
	}
	if note := c.failureNote(status); note != "" {
		fmt.Fprintln(out, note)
	}
	return err
}
