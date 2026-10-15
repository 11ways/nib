// Package nibash is nib's shell: a bash interpreter (mvdan.cc/sh) whose own cat, head, tail, grep, rg, sed -n and wc pass file lines on with
// their origin, so what reaches the model can carry edit anchors, while every other command is a real program.
package nibash

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/server"
)

const (
	defaultTimeout = 120 * time.Second
	maxTimeout     = 600 * time.Second
)

// description is the nibash tool's description, with %s for how long output is cut; a test holds every variant under 2048 characters.
const description = `Run a command in nibash, nib's shell: a bash interpreter (bash dialect) whose directory, variables and functions persist between calls; calls run one at a time. Programs run as real processes; stdin is empty.
cat, head, tail, grep, egrep, fgrep, rg, sed -n and wc are nib's own when their output comes to you or into another of them, and their options are implemented: file lines then print as [PATH#TAG] and N:TEXT rows you can edit from (grep matches as *N:), with facts like [lines 1-20 of 1,234]. Elsewhere, with other options, or written as a path (/usr/bin/grep), the system program runs; type and command -v name that one.
Output is shaped: colors stripped, repeats collapsed, %s, with the whole kept in a log you can read. A nonzero exit ends with [exit N]; files the command changed are listed with their new tags.
Unlike bash: cmd & starts a job, and $! is its spec (%%N) for kill, wait and jobs; finished jobs are reported on the next call. A pipeline's last stage runs in this shell (... | read v sets v). ~name/ paths expand only unquoted; hash -d name=dir (zsh) adds one for you and later subagents, hash -d lists them, ${!HOME*} too.`

// Shell is nibash for an MCP server: one shell session per key the server passes, one per agent of each client.
type Shell struct {
	engine   *engine.Engine
	sandbox  bool
	decider  decider.Decider
	mu       sync.Mutex
	sessions map[any]*session
}

// New makes the shell with the fixed cut; its commands read files through e, the server's engine, so what they show counts for edits, and
// expand the named directories of the caller's view that each call's tools carry.
func New(e *engine.Engine, sandbox bool) *Shell {
	return &Shell{engine: e, sandbox: sandbox, sessions: map[any]*session{}}
}

// Deciding sets the decider that chooses what long output shows; nil is the fixed cut. Call it before serving.
func (s *Shell) Deciding(d decider.Decider) *Shell {
	s.decider = d
	return s
}

// Description is the tool description, which says how the configured decider cuts long output.
func (s *Shell) Description() string {
	kind := decider.Fixed
	if s.decider != nil {
		kind = s.decider.Kind()
	}
	return fmt.Sprintf(description, kind.Cut())
}

// session is one agent's shell: its interpreter, jobs and logs. Calls hold mu, so they run one at a time.
type session struct {
	mu     sync.Mutex
	runner *interp.Runner
	jobs   *jobTable
	logs   *logDirectory
	shell  *Shell
	// settled is the shell as its last call left it, for a fork to start from without waiting for a call that is running; settling guards it.
	settling sync.Mutex
	settled  *settled
	// names are the caller's named directories, set as each call starts; nil when the server passes none.
	names namer
}

// settled is what of a shell a fork takes over: its directory, its variables, unset ones included, and its functions.
type settled struct {
	dir   string
	vars  frozen
	funcs map[string]*syntax.Stmt
}

// frozen are a shell's variables, the base environment of a shell forked from it.
type frozen map[string]expand.Variable

func (f frozen) Get(name string) expand.Variable { return f[name] }

func (f frozen) Each(yield func(name string, vr expand.Variable) bool) {
	for name, vr := range f {
		if !yield(name, vr) {
			return
		}
	}
}

func (s *Shell) session(key any, directory string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if found := s.sessions[key]; found != nil {
		return found, nil
	}
	sess := &session{jobs: newJobTable(), logs: newLogDirectory(), shell: s}
	runner, err := sess.newRunner(directory, expand.ListEnviron(environment(os.Environ())...))
	if err != nil {
		return nil, err
	}
	sess.runner = runner
	s.sessions[key] = sess
	return sess, nil
}

// Fork starts the shell under to as the one under from was when its last call ended, with none of its jobs. Aliases are not carried over.
func (s *Shell) Fork(from, to any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent := s.sessions[from]
	if parent == nil || s.sessions[to] != nil {
		return
	}
	parent.settling.Lock()
	last := parent.settled
	parent.settling.Unlock()
	if last == nil {
		return
	}
	sess := &session{jobs: newJobTable(), logs: newLogDirectory(), shell: s}
	runner, err := sess.newRunner(last.dir, last.vars)
	if err != nil {
		return
	}
	// Run resets a runner only the first time, which would drop the functions set here.
	runner.Reset()
	runner.Funcs = maps.Clone(last.funcs)
	sess.runner = runner
	s.sessions[to] = sess
}

// Close kills a session's jobs and forgets it.
func (s *Shell) Close(key any) {
	s.mu.Lock()
	sess := s.sessions[key]
	delete(s.sessions, key)
	s.mu.Unlock()
	if sess != nil {
		sess.jobs.killAll()
	}
}

// environment is the process environment with the settings that keep programs from waiting on a terminal the model does not have.
func environment(pairs []string) []string {
	quiet := map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "PAGER": "cat", "TERM": "dumb"}
	var env []string
	for _, pair := range pairs {
		name, _, _ := strings.Cut(pair, "=")
		if _, replaced := quiet[name]; !replaced {
			env = append(env, pair)
		}
	}
	for name, value := range quiet {
		env = append(env, name+"="+value)
	}
	slices.Sort(env)
	return env
}

// newRunner makes the session's interpreter on env. It lives as long as the session: Run starts each call afresh, also after exit or a timeout.
func (sess *session) newRunner(directory string, env expand.Environ) (*interp.Runner, error) {
	runner, err := interp.New(
		interp.Env(named{env, sess}),
		interp.Dir(directory),
		interp.ExecHandlers(sess.execMiddleware),
		interp.CallHandler(sess.callHandler),
		interp.OpenHandler(sess.openHandler),
		interp.StatHandler(sess.statHandler),
		interp.ReadDirHandler2(sess.readDirHandler),
	)
	if err != nil {
		return nil, err
	}
	return runner, nil
}

// call is one nibash call's state, reached from the handlers through the context.
type call struct {
	engine  *engine.Engine
	tools   *engine.Context
	session *session
	sink    *sink
	links   *links
	logs    *logDirectory
	jobs    map[int]*syntax.Stmt
	// job is set for calls that run a job's statement, whose processes join the job.
	job *job
	// groups are the process groups this call started, for killing them on timeout or cancellation.
	mu     sync.Mutex
	groups map[int]bool
	// written are the files a redirect opened for writing, and whether each existed before.
	written map[string]bool
}

// held is a file the session held a version of before a call: its facts then, and the tag of that version.
type held struct {
	facts engine.Facts
	tag   string
}

type callKey struct{}

func callOf(ctx context.Context) *call {
	c, _ := ctx.Value(callKey{}).(*call)
	return c
}

func (c *call) started(pgid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.groups[pgid] = true
	if c.job != nil {
		c.job.add(pgid)
	}
}

func (c *call) ended(pgid int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.groups, pgid)
}

func (c *call) wrote(path string, existed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.written[path]; !seen {
		c.written[path] = existed
	}
}

// Run runs one nibash call.
func (s *Shell) Run(ctx context.Context, key any, request server.ShellCall, tools *engine.Context) (engine.Result, error) {
	sess, err := s.session(key, tools.Directory)
	if err != nil {
		return engine.Result{}, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.names, _ = tools.Names.(namer)
	timeout := defaultTimeout
	if request.Timeout != nil {
		timeout = min(time.Duration(*request.Timeout)*time.Millisecond, maxTimeout)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(request.Command), "")
	if err != nil {
		return engine.Result{Text: fmt.Sprintf("nibash: %v\n[exit 2]", err)}, nil
	}
	var reports []string
	reports = append(reports, sess.jobs.finished()...)
	if request.Background {
		j, err := sess.startJob(file, request.Command, tools, sess.runner.Dir)
		if err != nil {
			return engine.Result{}, err
		}
		return engine.Result{Text: strings.Join(append(reports, fmt.Sprintf("[job %%%d started; output: %s]", j.id, j.log)), "\n")}, nil
	}
	functions := map[string]bool{}
	for name := range sess.runner.Funcs {
		functions[name] = true
	}
	jobs := classify(file, functions, 0)
	bodies := heredocs(file)
	before := map[string]held{}
	for _, path := range s.engine.Held(tools) {
		if facts, ok := engine.Stat(path); ok {
			if latest := s.engine.Latest(tools, path); latest != nil {
				before[path] = held{facts, latest.Tag}
			}
		}
	}
	c := &call{engine: s.engine, tools: tools, session: sess, sink: &sink{}, links: &links{}, logs: sess.logs, jobs: jobs, groups: map[int]bool{}, written: map[string]bool{}}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return engine.Result{}, err
	}
	defer devnull.Close()
	if err := interp.StdIO(devnull, c.sink, c.sink)(sess.runner); err != nil {
		return engine.Result{}, err
	}
	runCtx, cancel := context.WithTimeout(context.WithValue(ctx, callKey{}, c), timeout)
	defer cancel()
	tools.Ctx = runCtx
	err = sess.runner.Run(runCtx, file)
	sess.settle()
	c.killAll()
	status := 0
	var exit interp.ExitStatus
	switch {
	case errors.As(err, &exit):
		status = int(exit)
	case err != nil && runCtx.Err() == nil:
		fmt.Fprintf(c.sink, "nibash: %v\n", err)
		status = 1
	}
	tools.Ctx = ctx
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	text := c.sink.render(s.engine, tools, sess.logs, decision{decider: s.decider, command: request.Command, purpose: purpose(request.Description), exit: status, timedOut: timedOut})
	var tail []string
	if timedOut {
		tail = append(tail, fmt.Sprintf("[nibash: stopped after the %s timeout; run long commands with background: true]", timeout))
	}
	if status != 0 {
		tail = append(tail, fmt.Sprintf("[exit %d]", status))
	}
	if changed := c.changed(before, bodies); changed != "" {
		tail = append(tail, changed)
	}
	parts := reports
	if text != "" {
		parts = append(parts, text)
	}
	parts = append(parts, tail...)
	if len(parts) == 0 {
		return engine.Result{Text: "(no output)"}, nil
	}
	return engine.Result{Text: strings.Join(parts, "\n")}, nil
}

// settle keeps the shell as this call left it, for a fork.
func (sess *session) settle() {
	last := &settled{dir: sess.runner.Dir, vars: shellVariables(sess.runner.Vars), funcs: maps.Clone(sess.runner.Funcs)}
	sess.settling.Lock()
	sess.settled = last
	sess.settling.Unlock()
}

// changed lists the files the command created or changed: those the session held versions of, and those it wrote through a redirect.
// A file holding exactly a here-document's text is recorded with every line known, since the model typed it.
func (c *call) changed(before map[string]held, bodies []string) string {
	type candidate struct {
		created bool
		was     string
	}
	paths := map[string]candidate{}
	for path, h := range before {
		if now, ok := engine.Stat(path); !ok || now != h.facts {
			paths[path] = candidate{was: h.tag}
		}
	}
	for path, existed := range c.written {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		found := paths[real]
		if h, ok := before[real]; ok {
			found.was = h.tag
		}
		found.created = found.created || !existed
		paths[real] = found
	}
	sorted := make([]string, 0, len(paths))
	for path := range paths {
		sorted = append(sorted, path)
	}
	slices.Sort(sorted)
	var listed []string
	for _, path := range sorted {
		if strings.HasPrefix(path, "/dev/") || strings.HasPrefix(path, "/proc/") {
			continue
		}
		found := paths[path]
		data, err := os.ReadFile(path)
		if err != nil {
			if found.was != "" {
				listed = append(listed, c.tools.Display(path)+" (deleted)")
			}
			continue
		}
		typed := slices.Contains(bodies, string(data))
		view, ok := c.engine.Snapshot(c.tools, path, typed)
		if !ok {
			continue
		}
		entry := fmt.Sprintf("%s#%s", c.tools.Display(path), view.Tag)
		switch {
		case found.created:
			entry += " (new)"
		case found.was != "" && found.was != view.Tag:
			entry += fmt.Sprintf(" (was #%s)", found.was)
		case found.was == "":
			entry += " (written)"
		default:
			continue
		}
		listed = append(listed, entry)
	}
	if len(listed) == 0 {
		return ""
	}
	return "[changed by this command: " + strings.Join(listed, ", ") + "]"
}
