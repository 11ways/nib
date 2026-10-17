// Package nibash is nib's shell: a bash interpreter (mvdan.cc/sh) whose calls each start afresh and whose own cat, head, tail, grep, rg,
// sed -n and wc pass file lines on with their origin, so what reaches the model can carry edit anchors, while every other command is a real
// program.
package nibash

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

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
const description = `Run a command in nibash, nib's shell: a bash interpreter. Each call starts afresh in the project directory (no cd, variable or function carries over); calls run in parallel. Programs are real processes; stdin is empty.
cat, head, tail, grep, egrep, fgrep, rg, sed -n and wc are nib's own when their output reaches you or another of them and they know each option: file lines then print as [PATH#TAG] and N:TEXT rows you can edit from. Otherwise the system program runs, with a note when it printed to you; written as a path (/usr/bin/grep), it runs without one; type and command -v name it.
Output is shaped: colors stripped, repeats collapsed, %s, with the whole kept in a log you can read. Changed files are listed with new tags.
Unlike bash: a program does not start in a deleted directory; a command still running at its timeout goes on as a job, never stopped, like cmd &; $! is a job's spec (%%N) for kill, wait and jobs, and the call after it ends reports it. ~name/ paths expand only unquoted; hash -d name=dir (zsh) adds one for later calls and subagents, hash -d lists them, ${!HOME*} too. A pipeline's last stage runs in the shell (... | read v sets v).`

// Shell is nibash for an MCP server: one session of jobs per key the server passes, one per agent of each client.
type Shell struct {
	engine     *engine.Engine
	sandbox    bool
	decider    decider.Decider
	summarizer decider.Summarizer
	logs       *logDirectory
	mu         sync.Mutex
	sessions   map[any]*session
}

// New makes the shell with the fixed cut; its commands read files through e, the server's engine, so what they show counts for edits, and
// expand the named directories of the caller's view that each call's tools carry.
func New(e *engine.Engine, sandbox bool) *Shell {
	return &Shell{engine: e, sandbox: sandbox, logs: newLogDirectory(), sessions: map[any]*session{}}
}

// Deciding sets the decider that chooses what long output shows; nil is the fixed cut. Call it before serving.
func (s *Shell) Deciding(d decider.Decider) *Shell {
	s.decider = d
	return s
}

// Summarizing sets the summarizer whose checked summary leads long output; nil is none. Call it before serving.
func (s *Shell) Summarizing(summarizer decider.Summarizer) *Shell {
	s.summarizer = summarizer
	return s
}

// Description is the tool description, which says how the configured decider cuts long output.
func (s *Shell) Description() string {
	kind := decider.Fixed
	if s.decider != nil {
		kind = s.decider.Kind()
	}
	cut := kind.Cut()
	if s.summarizer != nil {
		cut += fmt.Sprintf(", or past %d lines or %d KB to a <summary> of it and the log lines that summary points at", summaryLines, summaryBytes/1024)
	}
	return fmt.Sprintf(description, cut)
}

// Expects says whether a model reads a call's expectation: a decision model or a summarizer.
func (s *Shell) Expects() bool {
	return s.summarizer != nil || s.decider != nil && s.decider.Kind() == decider.OpenRouter
}

// session is one agent's jobs, which outlive the calls that start them, and the calls running now; nothing else of a shell carries over from
// one call to the next.
type session struct {
	jobs  *jobTable
	shell *Shell
	mu    sync.Mutex
	// active are the calls still running in their request, for Status; a call that goes on as a job leaves them.
	active map[*call]running
}

// running is what Status tells of a call in flight.
type running struct {
	command, description string
	started              time.Time
}

func (s *Shell) session(key any) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if found := s.sessions[key]; found != nil {
		return found
	}
	sess := &session{jobs: newJobTable(), shell: s, active: map[*call]running{}}
	s.sessions[key] = sess
	return sess
}

// track lists c as running until untrack.
func (sess *session) track(c *call, request server.ShellCall) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.active[c] = running{request.Command, request.Description, time.Now()}
}

// untrack ends what track began, and says what it was.
func (sess *session) untrack(c *call) (running, bool) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	was, ok := sess.active[c]
	delete(sess.active, c)
	return was, ok
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

// newRunner makes a call's interpreter, in directory with the process environment and the caller's named directories.
func (sess *session) newRunner(directory string, names namer) (*interp.Runner, error) {
	return interp.New(
		interp.Env(named{expand.ListEnviron(environment(os.Environ())...), names}),
		interp.Dir(directory),
		interp.ExecHandlers(sess.execMiddleware),
		interp.CallHandler(sess.callHandler),
		interp.OpenHandler(sess.openHandler),
		interp.StatHandler(sess.statHandler),
		interp.ReadDirHandler2(sess.readDirHandler),
	)
}

// call is one nibash call's state, reached from the handlers through the context.
type call struct {
	engine  *engine.Engine
	tools   *engine.Context
	session *session
	runner  *interp.Runner
	// names are the caller's named directories; nil when the server passes none.
	names namer
	sink  *sink
	links *links
	logs  *logDirectory
	jobs  map[int]*syntax.Stmt
	// mu guards job, groups, written and system, which a call's stages and a job it turned into share.
	mu sync.Mutex
	// job is set for calls that run a job's statement, and for a call that went on as a job; their processes join it.
	job *job
	// groups are the process groups this call started, for killing them when it ends or the client cancels.
	groups map[int]bool
	// written are the files a redirect opened for writing, and whether each existed before.
	written map[string]bool
	// system are the notes on nib commands that ran as the system program with output to the model, in the order they first ran.
	system []string
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

// observePaths counts the absolute paths a command line spells toward the directories nib names for the agent: a path given to a real
// program is never printed by nib, yet costs the agent the same characters each time it is typed. Paths the sandbox refuses are left out.
func observePaths(file *syntax.File, view namer, refused func(string) bool) {
	if view == nil {
		return
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if word, ok := node.(*syntax.Word); ok {
			for _, path := range wordPaths(word) {
				if refused == nil || !refused(path) {
					view.Observe(path)
				}
			}
		}
		return true
	})
}

// unknown stands in a word's text for an expansion, whose value is not known before the command runs.
const unknown = 0

// wordPaths are the absolute paths in a word's literal text: runs of characters a path holds as written that start with a slash, at the
// word's start or after a character that ends one, so a path inside code for another program (python -c) counts up to its closing quote,
// while a run that touches an expansion ($HOME/x, /a/$b) is not a path anyone typed.
func wordPaths(word *syntax.Word) []string {
	var text strings.Builder
	var literal func(parts []syntax.WordPart)
	literal = func(parts []syntax.WordPart) {
		for _, part := range parts {
			switch part := part.(type) {
			case *syntax.Lit:
				text.WriteString(part.Value)
			case *syntax.SglQuoted:
				text.WriteString(part.Value)
			case *syntax.DblQuoted:
				literal(part.Parts)
			default:
				text.WriteByte(unknown)
			}
		}
	}
	literal(word.Parts)
	var paths []string
	for _, run := range strings.FieldsFunc(text.String(), func(r rune) bool { return !pathRune(r) }) {
		if strings.HasPrefix(run, "/") && len(run) > 1 && !strings.ContainsAny(run, "$\x00") {
			paths = append(paths, run)
		}
	}
	return paths
}

// pathRune is true for a character a path holds as written in a command; $, ~ and unknown belong to a run so that it is seen whole and refused.
func pathRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._-+@%$~", r) || r == unknown
}

// Run runs one nibash call in a fresh interpreter; a command still running at the call's timeout goes on as a job.
func (s *Shell) Run(ctx context.Context, key any, request server.ShellCall, tools *engine.Context) (engine.Result, error) {
	sess := s.session(key)
	view, _ := tools.Names.(namer)
	timeout := defaultTimeout
	if request.Timeout != nil {
		timeout = min(time.Duration(*request.Timeout)*time.Millisecond, maxTimeout)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(request.Command), "")
	if err != nil {
		return engine.Result{Text: fmt.Sprintf("nibash: %v\n[exit 2]", err)}, nil
	}
	var refused func(string) bool
	if tools.Sandbox {
		refused = func(path string) bool {
			why, err := tools.Refused(path)
			return err != nil || why != ""
		}
	}
	observePaths(file, view, refused)
	reports := sess.jobs.finished()
	runner, err := sess.newRunner(tools.Directory, view)
	if err != nil {
		return engine.Result{}, err
	}
	if request.Background {
		j, err := sess.launch(runner, file, request.Command, tools, view)
		if err != nil {
			return engine.Result{}, err
		}
		return engine.Result{Text: strings.Join(append(reports, fmt.Sprintf("[job %%%d started; output: %s]", j.id, j.log)), "\n")}, nil
	}
	jobs := classify(file, 0)
	bodies := heredocs(file)
	before := map[string]held{}
	for _, path := range s.engine.Held(tools) {
		if facts, ok := engine.Stat(path); ok {
			if latest := s.engine.Latest(tools, path); latest != nil {
				before[path] = held{facts, latest.Tag}
			}
		}
	}
	// The call's own copy of the tools, as the run may outlive the request when it goes on as a job.
	callTools := *tools
	c := &call{engine: s.engine, tools: &callTools, session: sess, runner: runner, names: view, sink: &sink{}, links: &links{}, logs: s.logs, jobs: jobs, groups: map[int]bool{}, written: map[string]bool{}}
	sess.track(c, request)
	defer func() { sess.untrack(c) }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return engine.Result{}, err
	}
	if err := interp.StdIO(devnull, c.sink, c.sink)(runner); err != nil {
		devnull.Close()
		return engine.Result{}, err
	}
	// The run is not tied to the request: the client cancelling stops it only while the call still waits for it.
	runCtx, cancel := context.WithCancel(context.WithValue(context.WithoutCancel(ctx), callKey{}, c))
	callTools.Ctx = runCtx
	done := make(chan error, 1)
	go func() {
		defer devnull.Close()
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "nib: internal error in nibash: %v\n%s", r, debug.Stack())
				done <- fmt.Errorf("internal error (%v); please report the command", r)
			}
		}()
		done <- runner.Run(runCtx, file)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-ctx.Done():
		cancel()
		err = <-done
	case <-timer.C:
		return s.detach(ctx, c, request, timeout, cancel, done, before, bodies, reports)
	}
	cancel()
	c.killAll()
	status := 0
	var exit interp.ExitStatus
	switch {
	case errors.As(err, &exit):
		status = int(exit)
	case err != nil && ctx.Err() == nil:
		fmt.Fprintf(c.sink, "nibash: %v\n", err)
		status = 1
	}
	callTools.Ctx = ctx
	text := c.sink.render(s.engine, &callTools, s.logs, decision{decider: s.decider, summarizer: s.summarizer, command: request.Command, purpose: purpose(request.Description), expect: purpose(request.Expect), exit: status})
	var tail []string
	if status != 0 {
		tail = append(tail, fmt.Sprintf("[exit %d]", status))
	}
	return c.result(reports, text, tail, before, bodies), nil
}

// detach turns a call still running at its timeout into a job: the call returns the output so far, and the job's log gets all of it, that
// output included, while the run goes on.
func (s *Shell) detach(ctx context.Context, c *call, request server.ShellCall, timeout time.Duration, cancel context.CancelFunc, done <-chan error, before map[string]held, bodies []string, reports []string) (engine.Result, error) {
	// The job carries on the call's command, so it keeps the call's start.
	started := time.Now()
	if was, ok := c.session.untrack(c); ok {
		started = was.started
	}
	j, out, err := c.session.jobs.open(s.logs, request.Command, cancel, started)
	if err != nil {
		// Without a log the run cannot go on unseen, so it ends as a call would on cancellation.
		cancel()
		<-done
		c.killAll()
		return engine.Result{}, err
	}
	c.adopt(j)
	c.sink.detach(out)
	go func() {
		err := <-done
		cancel()
		c.killAll()
		j.finish(err, out)
	}()
	c.tools.Ctx = ctx
	text := c.sink.render(s.engine, c.tools, s.logs, decision{decider: s.decider, summarizer: s.summarizer, command: request.Command, purpose: purpose(request.Description), expect: purpose(request.Expect), timedOut: true})
	tail := []string{fmt.Sprintf("[nibash: still running after %s; it goes on as job %%%d, output: %s; wait %%%d or kill %%%d]", timeout, j.id, j.log, j.id, j.id)}
	return c.result(reports, text, tail, before, bodies), nil
}

// result joins the finished jobs' reports, the output and the closing lines with the files the command changed so far.
func (c *call) result(reports []string, text string, tail []string, before map[string]held, bodies []string) engine.Result {
	if changed := c.changed(before, bodies); changed != "" {
		tail = append(tail, changed)
	}
	parts := reports
	if text != "" {
		parts = append(parts, text)
	}
	c.mu.Lock()
	parts = append(parts, c.system...)
	c.mu.Unlock()
	parts = append(parts, tail...)
	if len(parts) == 0 {
		return engine.Result{Text: "(no output)"}
	}
	return engine.Result{Text: strings.Join(parts, "\n")}
}

// adopt makes j the job of this call's processes, those it started already included.
func (c *call) adopt(j *job) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.job = j
	for pgid := range c.groups {
		j.add(pgid)
	}
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
	c.mu.Lock()
	written := maps.Clone(c.written)
	c.mu.Unlock()
	for path, existed := range written {
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
