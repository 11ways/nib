package nibash

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

const (
	// killDelay is how long a process group gets between SIGTERM and SIGKILL.
	killDelay = 3 * time.Second
	// drainDelay bounds the wait for output a daemonised child keeps open after the program itself exited.
	drainDelay = 2 * time.Second
)

// execMiddleware runs nib commands and nibash's own builtins, and every other command as a real program in its own process group.
func (sess *session) execMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) (err error) {
		c := callOf(ctx)
		if c == nil {
			return next(ctx, args)
		}
		// Pipeline stages run on the interpreter's own goroutines, beyond the server's guard, so a panic here ends only this command.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "nib: internal error in nibash: %v\n%s", r, debug.Stack())
				fmt.Fprintf(interp.HandlerCtx(ctx).Stderr, "nibash: internal error (%v); please report the command\n", r)
				err = interp.ExitStatus(2)
			}
		}()
		if mode, name, ok := parseMarker(args[0]); ok {
			switch name {
			case internalJob:
				return c.startJob(ctx, args[1:])
			case internalKill:
				return c.kill(ctx, args[1:])
			case internalJobs:
				return c.listJobs(ctx)
			case internalWait:
				return c.wait(ctx, args[1:])
			case internalHash:
				return c.hash(ctx, args[2:])
			}
			return c.runNib(ctx, mode, name, args)
		}
		return c.runReal(ctx, args)
	}
}

// callHandler sends kill, jobs and wait to nibash, since mvdan's builtins do not act on real processes, and hash -d's named directory forms,
// and refuses a cd out of the sandbox with the reason, which the builtin would report as a missing directory.
func (sess *session) callHandler(ctx context.Context, args []string) ([]string, error) {
	if namedHash(args) {
		return append([]string{marker + internalHash}, args[1:]...), nil
	}
	switch args[0] {
	case "kill", "jobs", "wait":
		renamed := append([]string{marker + args[0]}, args[1:]...)
		return renamed, nil
	case "cd":
		c := callOf(ctx)
		if c == nil || !c.tools.Sandbox || len(args) < 2 || args[1] == "-" {
			return args, nil
		}
		hc := interp.HandlerCtx(ctx)
		target := args[len(args)-1]
		if refused, err := c.tools.Refused(absolute(hc.Dir, target)); err == nil && refused != "" {
			fmt.Fprintf(hc.Stderr, "cd: %s\n", refused)
			return []string{"false"}, nil
		}
	}
	return args, nil
}

// runNib runs one of nibash's own commands with the input and output its classification set up.
func (c *call) runNib(ctx context.Context, mode, name string, args []string) error {
	hc := interp.HandlerCtx(ctx)
	args = append([]string{name}, args[1:]...)
	prepare := commands[name]
	p, err := prepare(name, args[1:])
	if err != nil {
		// An option that came from an expansion, which the classification could not see. With no other nib command reading or writing
		// its records, the system program runs, as it would have; joined to one, it cannot stand in, so the command says why.
		if out, isFile := hc.Stdout.(*os.File); mode != fromLink && !(isFile && c.links.get(out) != nil) {
			return c.runReal(ctx, args)
		}
		fmt.Fprintf(hc.Stderr, "%s: an option nibash does not implement reached it through an expansion, inside a pipeline of nib's own commands; write the option literally, or the command as /usr/bin/%s\n", name, name)
		// The stages it was joined to must still see it end, or they would wait for it.
		if out, isFile := hc.Stdout.(*os.File); isFile {
			if l := c.links.get(out); l != nil {
				close(l.records)
			}
		}
		if in, isFile := hc.Stdin.(*os.File); isFile && mode == fromLink {
			if l := c.links.get(in); l != nil {
				(&input{link: l}).stop()
			}
		}
		return interp.ExitStatus(2)
	}
	inv := &invocation{ctx: ctx, call: c, name: name, dir: hc.Dir, stderr: hc.Stderr}
	inv.in = &input{}
	if file, ok := hc.Stdin.(*os.File); ok && file != nil {
		if mode == fromLink {
			inv.in.link = c.links.get(file)
			inv.inLink = inv.in.link
		} else {
			inv.in.reader = bufio.NewReaderSize(file, 1<<16)
			inv.in.file = file
		}
	}
	inv.out = &output{ctx: ctx}
	switch out := hc.Stdout.(type) {
	case *sink:
		inv.out.sink = out
	case *os.File:
		if l := c.links.get(out); l != nil {
			inv.out.link = l
			inv.outLink = l
		} else {
			inv.out.writer = out
		}
	default:
		inv.out.writer = hc.Stdout
	}
	defer inv.out.close()
	defer func() {
		if inv.in.link != nil {
			inv.in.stop()
		}
	}()
	status := p.run(inv)
	if status != 0 {
		return interp.ExitStatus(status)
	}
	return nil
}

// environ is the variables a child process gets: the shell's exported ones.
func environ(env expand.Environ) []string {
	var pairs []string
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.IsSet() && !strings.Contains(name, " ") {
			pairs = append(pairs, name+"="+vr.String())
		}
		return true
	})
	return pairs
}

// runReal runs a real program: its own process group, killed on timeout or cancellation; output meant for the model is drained into the
// sink before the handler returns, so it keeps its place among the commands around it.
func (c *call) runReal(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(127)
	}
	argv := append([]string{path}, args[1:]...)
	if c.tools.Sandbox {
		wrapped, err := c.sandboxed(hc.Dir, argv)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], err)
			return interp.ExitStatus(126)
		}
		argv = wrapped
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if !c.tools.Sandbox {
		// The program sees the name it was called by, as under bash.
		cmd.Args[0] = args[0]
	}
	cmd.Env = environ(hc.Env)
	cmd.Dir = hc.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if hc.Stdin != nil {
		cmd.Stdin = hc.Stdin
	}
	var drains sync.WaitGroup
	var pipes []*os.File
	attach := func(w io.Writer) (io.Writer, error) {
		s, ok := w.(*sink)
		if !ok {
			return w, nil
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		pipes = append(pipes, writer)
		drains.Add(1)
		go func() {
			defer drains.Done()
			defer reader.Close()
			buffer := make([]byte, 64*1024)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					s.Write(buffer[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		return writer, nil
	}
	stdout, err := attach(hc.Stdout)
	if err != nil {
		return err
	}
	cmd.Stdout = stdout
	if hc.Stderr == hc.Stdout {
		cmd.Stderr = stdout
	} else if cmd.Stderr, err = attach(hc.Stderr); err != nil {
		return err
	}
	cmd.WaitDelay = drainDelay
	if err := cmd.Start(); err != nil {
		for _, p := range pipes {
			p.Close()
		}
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], unwrapStart(err))
		return interp.ExitStatus(126)
	}
	// The child holds its own ends now; closing ours lets the drains end when it does.
	for _, p := range pipes {
		p.Close()
	}
	pgid := cmd.Process.Pid
	c.started(pgid)
	defer c.ended(pgid)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		waitErr = terminate(pgid, waited)
	}

	finished := make(chan struct{})
	go func() {
		drains.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(drainDelay):
	}
	var exit *exec.ExitError
	switch {
	case errors.As(waitErr, &exit):
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return interp.ExitStatus(128 + int(status.Signal()))
		}
		return interp.ExitStatus(exit.ExitCode())
	case waitErr != nil && !errors.Is(waitErr, exec.ErrWaitDelay):
		return waitErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func unwrapStart(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// terminate stops a process group: SIGTERM, then SIGKILL if it has not ended after killDelay; it returns the program's wait result.
func terminate(pgid int, waited <-chan error) error {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case err := <-waited:
		return err
	case <-time.After(killDelay):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return <-waited
	}
}

// killAll ends every process group the call started that is still there, as its time ran out or the client cancelled.
func (c *call) killAll() {
	c.mu.Lock()
	groups := make([]int, 0, len(c.groups))
	for pgid := range c.groups {
		groups = append(groups, pgid)
	}
	c.mu.Unlock()
	for _, pgid := range groups {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	}
	if len(groups) == 0 {
		return
	}
	time.Sleep(100 * time.Millisecond)
	for _, pgid := range groups {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// absolute is a path the shell names, made absolute against its directory.
func absolute(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// allowedPath checks a path the shell touches against the sandbox; device files and process substitution's FIFOs pass.
func (c *call) allowedPath(path string) error {
	if !c.tools.Sandbox || strings.HasPrefix(path, "/dev/") || strings.HasPrefix(path, "/proc/self/fd/") {
		return nil
	}
	refused, err := c.tools.Refused(path)
	if err != nil {
		return err
	}
	if refused != "" {
		return &fs.PathError{Op: "open", Path: path, Err: errors.New("outside the sandbox")}
	}
	return nil
}

// openHandler confines redirects, source and $(<file) to the sandbox and notes files opened for writing.
func (sess *session) openHandler(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	c := callOf(ctx)
	if c != nil {
		full := absolute(interp.HandlerCtx(ctx).Dir, path)
		if err := c.allowedPath(full); err != nil {
			return nil, err
		}
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
			_, err := os.Lstat(full)
			c.wrote(full, err == nil)
		}
	}
	return interp.DefaultOpenHandler()(ctx, path, flag, perm)
}

func (sess *session) statHandler(ctx context.Context, name string, followSymlinks bool) (fs.FileInfo, error) {
	if c := callOf(ctx); c != nil {
		if err := c.allowedPath(absolute(interp.HandlerCtx(ctx).Dir, name)); err != nil {
			return nil, err
		}
	}
	return interp.DefaultStatHandler()(ctx, name, followSymlinks)
}

func (sess *session) readDirHandler(ctx context.Context, path string) ([]fs.DirEntry, error) {
	if c := callOf(ctx); c != nil {
		if err := c.allowedPath(absolute(interp.HandlerCtx(ctx).Dir, path)); err != nil {
			return nil, err
		}
	}
	return interp.DefaultReadDirHandler2()(ctx, path)
}
