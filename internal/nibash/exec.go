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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/platform"
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
		// A watched command's failure is recorded for the notes that close the call, with the first line of a standard error that went to
		// /dev/null.
		ctx, kept := keepDiscarded(ctx)
		defer func() {
			name := args[0]
			if _, marked, ok := parseMarker(name); ok {
				name = marked
			}
			c.recordStep(interp.HandlerCtx(ctx).Env, name, err, kept.line())
		}()
		c.reachIn(interp.HandlerCtx(ctx).Dir, args[1:])
		name := args[0]
		mode, marked, ok := parseMarker(args[0])
		if ok {
			switch marked {
			case internalJob:
				return c.startJob(ctx, args[1:])
			case internalKill:
				return c.kill(ctx, args[1:])
			case internalJobs:
				return c.listJobs(ctx)
			case internalWait:
				return c.wait(ctx, args[1:])
			case internalStop:
				return c.stop(args[1:])
			case internalHash:
				return c.hash(ctx, args[2:])
			}
			name = marked
		}
		// What a command deletes goes to the trash, wherever its output goes.
		if handled, err := c.trashing(ctx, mode, name, args); handled {
			return err
		}
		// git-each is nib's wherever its output goes; where it reaches neither the model nor a nib command, nothing is shaped.
		if !ok && name == gitEach {
			if handled, err := c.eachAsProgram(ctx, args); handled {
				return err
			}
		}
		if ok {
			return c.runNib(ctx, mode, name, args)
		}
		return c.runReal(ctx, args)
	}
}

// callHandler sends kill, jobs and wait to nibash, since mvdan's builtins do not act on real processes, and hash -d's named directory forms,
// and refuses a cd out of the sandbox with the reason, which the builtin would report as a missing directory.
func (sess *session) callHandler(ctx context.Context, args []string) ([]string, error) {
	// A call a failed directory change halted runs nothing more, wherever the shell would go on.
	if c := callOf(ctx); c != nil {
		if h := c.halt(); h != nil {
			return []string{"exit", strconv.Itoa(h.status)}, nil
		}
	}
	if namedHash(args) {
		return append([]string{marker + internalHash}, args[1:]...), nil
	}
	if change := directoryChange(args); change != nil {
		if c := callOf(ctx); c != nil {
			c.mu.Lock()
			c.change = change
			c.mu.Unlock()
		}
	}
	switch args[0] {
	case "kill", "jobs", "wait":
		renamed := append([]string{marker + args[0]}, args[1:]...)
		return renamed, nil
	case "cd":
		c := callOf(ctx)
		if c == nil || len(args) < 2 || args[1] == "-" {
			return args, nil
		}
		hc := interp.HandlerCtx(ctx)
		target := absolute(hc.Dir, args[len(args)-1])
		if c.tools.Sandbox {
			if refused, err := c.tools.Refused(target); err == nil && refused != "" {
				fmt.Fprintf(hc.Stderr, "cd: %s\n", refused)
				return []string{"false"}, nil
			}
		}
		if view, ok := c.tools.Names.(namer); ok {
			view.Worked(target)
			view.Observe(target)
		}
	}
	return args, nil
}

// ranAsSystem notes once per call that a nib command ran as the system program, whose output reached the model as plain text; a form nib's
// own never handles gets no note, nor does one whose own lines would have had no rows either: wc's, or those of a command naming no file or
// directory, which reads only standard input.
func (c *call) ranAsSystem(name string, err error, dir string, args []string) {
	if errors.Is(err, errForeign) || !commands[name].renders && (!commands[name].rows || !namesPath(dir, args)) {
		return
	}
	why := "nib's own does not implement this form of it"
	switch {
	case errors.Is(err, errUntranslatable):
		why = "its pattern needs a feature nib's own cannot run exactly"
	case errors.Is(err, errUncounted):
		why = "nib could not tell how it counts characters"
	}
	so := "its lines are plain output, not edit anchors"
	if commands[name].renders {
		so = "its output is as " + name + " printed it"
	}
	note := fmt.Sprintf("[%s ran as the system program: %s, so %s]", name, why, so)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.system, note) {
		c.system = append(c.system, note)
	}
}

// namesPath says whether any of a command's arguments, or the value of a --name=value one, is a file or directory that exists.
func namesPath(dir string, args []string) bool {
	for _, arg := range args {
		if _, value, found := strings.Cut(arg, "="); found && strings.HasPrefix(arg, "--") {
			arg = value
		}
		if arg == "" || arg == "-" {
			continue
		}
		if _, err := os.Stat(absolute(dir, arg)); err == nil {
			return true
		}
	}
	return false
}

// pipedNote tells an agent once that rows it piped into a real program reached it as plain text.
func (c *call) pipedNote() string {
	if len(c.piped) == 0 || !c.engine.Store.Teach(c.tools.Session, "rows-piped") {
		return ""
	}
	return pipedText(c.piped[0])
}

// pipedText is what pipedNote says of p.
func pipedText(p pipe) string {
	return fmt.Sprintf("[%s's rows lost their line numbers in %s; read path:A-B,C-D keeps them]", p.command, p.program)
}

// standIn is a plan that checks, once it knows the shell's environment, that it does what the system program would; an error makes the
// system program run instead, as for an option nib's own lacks.
type standIn interface {
	standIn(c *call, hc interp.HandlerContext) error
}

// runNib runs one of nibash's own commands with the input and output its classification set up; a mode with bandFlag sends its rows in band
// to a nib reader of its pipe's bytes, and one ending in modelFlag writes for the model through nib's own head, tail or cat.
func (c *call) runNib(ctx context.Context, mode string, name string, args []string) error {
	mode, model := strings.CutSuffix(mode, modelFlag)
	mode, band := strings.CutSuffix(mode, bandFlag)
	hc := interp.HandlerCtx(ctx)
	args = append([]string{name}, args[1:]...)
	prepare := commands[name].prepare
	p, err := prepare(name, args[1:])
	because := fmt.Sprintf("an option nibash does not implement reached it through an expansion, inside a pipeline of nib's own commands; write the option literally, or the command as /usr/bin/%s", name)
	if s, ok := p.(standIn); err == nil && ok {
		if err = s.standIn(c, hc); err != nil {
			because = fmt.Sprintf("%v, and inside a pipeline of nib's own commands the system program cannot stand in; write the command as /usr/bin/%s", err, name)
		}
	}
	if err != nil {
		// An option the command line or an expansion gave it that nib's own lacks. With no other nib command reading or writing its records,
		// the system program runs, as it would have, and says so when its output reaches the model; joined to one, it cannot stand in, so the
		// command says why. Bytes a nib reader takes in band are such a program's to write.
		out, isFile := hc.Stdout.(*os.File)
		if mode != fromLink && mode != fromBand && (band || !(isFile && c.links.get(out) != nil)) {
			return c.runProgram(ctx, args, func() { c.ranAsSystem(name, err, hc.Dir, args[1:]) })
		}
		fmt.Fprintf(stderrOf(ctx), "%s: %s\n", name, because)
		// The stages it was joined to must still see it end, or they would wait for it.
		if isFile && !band {
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
	inv := &invocation{ctx: ctx, call: c, name: name, dir: hc.Dir, stderr: stderrOf(ctx)}
	inv.in = &input{}
	if file, ok := hc.Stdin.(*os.File); ok && file != nil {
		if mode == fromLink {
			inv.in.link = c.links.get(file)
			inv.inLink = inv.in.link
		} else {
			inv.in.reader = bufio.NewReaderSize(file, 1<<16)
			inv.in.file = file
			if mode == fromBand {
				inv.in.rows = c.band()
			}
		}
	}
	inv.out = &output{ctx: ctx, model: model}
	switch out := hc.Stdout.(type) {
	case *sink:
		inv.out.sink = out
	case *os.File:
		if band {
			inv.out.rows = c.band()
			inv.out.writer = out
		} else if l := c.links.get(out); l != nil {
			inv.out.link = l
			inv.outLink = l
		} else {
			inv.out.writer = out
		}
	default:
		inv.out.writer = hc.Stdout
		if band {
			inv.out.rows = c.band()
		}
	}
	defer inv.out.close()
	defer func() {
		if inv.in.link != nil {
			inv.in.stop()
		}
	}()
	status := p.run(inv)
	// A command whose reader stopped, as head does, fails as the system program would on SIGPIPE.
	if status != 0 && inv.out.stopped {
		status = sigpipe
	}
	if status != 0 {
		return interp.ExitStatus(status)
	}
	return nil
}

// band is the call's in-band carrier of rows, made when a command first needs it.
func (c *call) band() *rowBand {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = newRowBand()
	}
	return c.rows
}

// environ is the variables a child process gets: the shell's exported ones, without the step nibash gives a command it watches.
func environ(env expand.Environ) []string {
	var pairs []string
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.IsSet() && !strings.Contains(name, " ") && name != stepVariable {
			pairs = append(pairs, name+"="+vr.String())
		}
		return true
	})
	return pairs
}

// runReal runs a real program: its own process group, killed on timeout or cancellation; output meant for the model is drained into the
// sink before the handler returns, so it keeps its place among the commands around it.
func (c *call) runReal(ctx context.Context, args []string) error {
	return c.runProgram(ctx, args, nil)
}

// runProgram is runReal calling printed, when not nil, once the program's standard output brings the sink anything.
func (c *call) runProgram(ctx context.Context, args []string, printed func()) error {
	return c.runProgramTo(ctx, args, interp.HandlerCtx(ctx).Stdout, stderrOf(ctx), printed)
}

// runProgramTo is runProgram with its standard output and error going to stdout and stderr, which a nib command that renders a real program's
// output passes; the same writer for both gives the program one descriptor for both, as 2>&1 does.
func (c *call) runProgramTo(ctx context.Context, args []string, stdout, stderr io.Writer, printed func()) error {
	return c.runProgramWith(ctx, args, nil, stdout, stderr, printed)
}

// runProgramWith is runProgramTo with env added to the shell's environment.
func (c *call) runProgramWith(ctx context.Context, args, env []string, stdout, stderr io.Writer, printed func()) error {
	hc := interp.HandlerCtx(ctx)
	// A shell knows no named directories, so a script it gets with -c has them expanded as nibash would expand them there.
	args = namedScripts(hc.Env, args)
	path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
	if err != nil {
		fmt.Fprintln(stderrOf(ctx), err)
		return interp.ExitStatus(127)
	}
	// A new process cannot start in a directory that is gone, where bash's child would inherit it; say so rather than a bare ENOENT.
	if info, err := os.Stat(hc.Dir); err != nil || !info.IsDir() {
		fmt.Fprintf(stderrOf(ctx), "nibash: %s: not run, since the working directory %s no longer exists; cd to one that does\n", args[0], hc.Dir)
		return interp.ExitStatus(126)
	}
	argv := append([]string{path}, args[1:]...)
	if c.tools.Sandbox {
		wrapped, err := c.sandboxed(hc.Dir, argv)
		if err != nil {
			fmt.Fprintf(stderrOf(ctx), "%s: %v\n", args[0], err)
			return interp.ExitStatus(126)
		}
		argv = wrapped
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if !c.tools.Sandbox {
		// The program sees the name it was called by, as under bash.
		cmd.Args[0] = args[0]
	}
	cmd.Env = append(environ(hc.Env), env...)
	cmd.Dir = hc.Dir
	platform.Prepare(cmd)
	if hc.Stdin != nil {
		cmd.Stdin = hc.Stdin
	}
	// kept counts the reading of a kept standard error, whose first line only a failure needs, so a success does not wait for it.
	var drains, kept sync.WaitGroup
	var pipes []*os.File
	// A sink, or a kept standard error, is read through a pipe the program holds as a file, so a daemon it leaves holding it keeps writing
	// without the wait waiting for it.
	attach := func(w io.Writer, printed func()) (io.Writer, error) {
		switch w.(type) {
		case *sink, *keptStderr:
		default:
			return w, nil
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		pipes = append(pipes, writer)
		group := &drains
		if _, ok := w.(*keptStderr); ok {
			group = &kept
		}
		group.Add(1)
		go func() {
			defer group.Done()
			defer reader.Close()
			buffer := make([]byte, 64*1024)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					if printed != nil {
						printed()
						printed = nil
					}
					w.Write(buffer[:n])
				}
				if err != nil {
					return
				}
			}
		}()
		return writer, nil
	}
	out, err := attach(stdout, printed)
	if err != nil {
		return err
	}
	cmd.Stdout = out
	if stderr == stdout {
		cmd.Stderr = out
	} else if cmd.Stderr, err = attach(stderr, nil); err != nil {
		return err
	}
	cmd.WaitDelay = drainDelay
	if err := cmd.Start(); err != nil {
		for _, p := range pipes {
			p.Close()
		}
		fmt.Fprintf(stderrOf(ctx), "%s: %v\n", args[0], unwrapStart(err))
		return interp.ExitStatus(126)
	}
	// The child holds its own ends now; closing ours lets the drains end when it does.
	for _, p := range pipes {
		p.Close()
	}
	pgid := cmd.Process.Pid
	// A program the OS cannot hold as a group could leave what it starts running past a kill, so it is not run at all.
	if err := platform.Join(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		fmt.Fprintf(stderrOf(ctx), "%s: %v\n", args[0], err)
		return interp.ExitStatus(126)
	}
	defer platform.Release(pgid)
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
		if waitErr != nil {
			kept.Wait()
		}
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
	_ = platform.SignalGroup(pgid, syscall.SIGTERM)
	select {
	case err := <-waited:
		return err
	case <-time.After(killDelay):
		_ = platform.SignalGroup(pgid, syscall.SIGKILL)
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
		_ = platform.SignalGroup(pgid, syscall.SIGTERM)
	}
	if len(groups) == 0 {
		return
	}
	time.Sleep(100 * time.Millisecond)
	for _, pgid := range groups {
		_ = platform.SignalGroup(pgid, syscall.SIGKILL)
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
		// A redirect alone runs no command, so a halted call refuses the file itself.
		if h := c.halt(); h != nil {
			return nil, h
		}
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
