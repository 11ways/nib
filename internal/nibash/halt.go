package nibash

import (
	"errors"
	"fmt"
	"io"

	"mvdan.cc/sh/v3/interp"
)

// stopped is what ended a call's shell before its command line did, a directory change whose failure the command left unhandled or a kill
// of the shell: from then on every command the call reaches exits with status instead of running.
type stopped struct {
	// reason is what ended the shell, as the call's closing note names it.
	reason string
	status int
	// quiet leaves out the note, for a job a kill ended, whose status says so already.
	quiet bool
}

func (s *stopped) Error() string {
	return fmt.Sprintf("[%s, so nothing after it ran]", s.reason)
}

// haltWith halts the call with s, unless something halted it already.
func (c *call) haltWith(s *stopped) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.halted != nil {
		return
	}
	c.halted = s
	if c.halting == nil {
		c.halting = make(chan struct{})
	}
	close(c.halting)
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

// haltNote is the note that something halted the call, with the halt's exit status instead of err; "" and err for any other call. A quiet
// halt has no note and leaves the status of the call's last command, unless the halt refused a redirect, whose error the run ends with.
func (c *call) haltNote(err error) (string, error) {
	h := c.halt()
	var refused *stopped
	switch {
	case h == nil:
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
