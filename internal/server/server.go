// Package server exposes nib's engine and nibash as MCP tools over stdio.
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/opencode"
)

// Options configure a server. ConfirmOutsideRoots asks the user through elicitation before writing outside the client's roots; Sandbox refuses
// every path outside them; Shell offers nibash.
type Options struct {
	Directory           string
	Session             string
	ConfirmOutsideRoots bool
	Sandbox             bool
	Shell               Shell
	Names               *names.Table
	Version             string
	// Engine is shared with the shell, whose own commands record what they show; nil makes a new one.
	Engine *engine.Engine
	// Agent is the Claude Code subagent making a call, from the call's tool_use id and the record nib's hook filed under it; the zero Agent is
	// the main agent. Nil, or no id, keeps one agent per client unless the client names its sessions itself.
	Agent func(toolUse string) hookstate.Agent
	// Origin is where an OpenCode session comes from, a fork or a subagent's child session, asked once per session, on the first call nib
	// sees from it; nil, or ok false, starts every session on its own.
	Origin func(session string) (origin opencode.Origin, ok bool)
}

// Shell is the nibash side of the server; nil offers no shell.
type Shell interface {
	// Description is the nibash tool's description.
	Description() string
	// Run executes one nibash call in the shell under key, one per agent of a client; a new key starts a new shell.
	Run(ctx context.Context, key any, call ShellCall, tools *engine.Context) (engine.Result, error)
	// Fork starts the shell under to as a copy of the one under from as its last call left it, without its jobs; it does nothing when to has
	// a shell already, and leaves to to start afresh when from has none.
	Fork(from, to any)
	// Close ends the shell under key and its jobs.
	Close(key any)
}

// ShellCall is one nibash call's arguments.
type ShellCall struct {
	Command     string
	Description string
	Timeout     *int
	Background  bool
}

// session is per-client state: its roots, cached until the client says they changed, the directories the user approved writing to, the
// calls paused to ask the client, under the request state it sends back, and the agents with a shell of their own.
type session struct {
	mu         sync.Mutex
	roots      []string
	known      bool
	generation int
	approved   map[string]bool
	paused     map[string]paused
	order      []string
	// shells are the agents with a nibash shell, least recently used first; the main agent's is not among them, so never evicted.
	shells []string
}

// paused is a paused call's answers so far and the agent making it, whose record the first round took.
type paused struct {
	answers mcp.InputResponseMap
	agent   string
}

// pausedLimit bounds the paused calls a session remembers; a client that never sends one again leaves it to be dropped.
const pausedLimit = 16

// shellLimit bounds the agent shells a session keeps; past it the least recently used one is closed, with its jobs.
const shellLimit = 16

// shellKey is the nibash shell of one agent of one client; agent "" is the main agent.
type shellKey struct {
	session *mcp.ServerSession
	agent   string
}

// An agent is named by how its client tells it apart, and that name is also its engine session; "" is the client's main agent.
const (
	// claudeAgent names a Claude Code subagent by the agent id nib's hook filed for its call.
	claudeAgent = "agent:"
	// openCodeSession names an OpenCode session, a subagent's child session included, by the session id OpenCode sends in _meta.
	openCodeSession = "opencode:"
)

// openCodeID is the shape nib accepts of an OpenCode session id; a call with anything else counts as the main agent's.
var openCodeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Server is nib's MCP server.
type Server struct {
	*mcp.Server
	options  Options
	engine   *engine.Engine
	mu       sync.Mutex
	sessions map[*mcp.ServerSession]*session
	// seeding is held while an agent starts, a fork being given its parent's state, so none of its calls runs before that is done.
	seeding sync.Mutex
	// started are the agents started already, which happens once, on the first call nib sees from each.
	started map[string]bool
	// defined are the named directories agents defined with nibash's hash -d.
	defined *names.Defined
}

// originDepth bounds how far up a chain of OpenCode sessions that never called nib a session's start goes to start them first.
const originDepth = 8

// origin is where a subagent starts from: its parent agent, whether it is a fork, which starts with the parent's conversation, and the first
// moment the parent's work may be missing from what it was given, zero when that is not known.
type origin struct {
	parent string
	fork   bool
	at     time.Time
}

// Instructions are the server instructions for these options: the tool guidance, then the named directories that fit.
func Instructions(options Options) string {
	text := toolInstructions
	if options.Shell != nil {
		text = shellInstructions
	}
	if options.Names == nil || len(options.Names.Entries) == 0 {
		return text
	}
	header := "\n" + namedHeader
	lines := options.Names.Lines(instructionLimit - len(text) - len(header) - 1)
	options.Names.Keep(lines)
	if len(lines) == 0 {
		return text
	}
	return text + header + "\n" + strings.Join(lines, "\n")
}

// New builds the server.
func New(options Options) *Server {
	e := options.Engine
	if e == nil {
		e = engine.New()
	}
	s := &Server{options: options, engine: e, sessions: map[*mcp.ServerSession]*session{}, started: map[string]bool{}, defined: names.NewDefined()}
	instructions := Instructions(options)
	s.Server = mcp.NewServer(&mcp.Implementation{Name: "nib", Version: options.Version}, &mcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &mcp.ServerCapabilities{},
		RootsListChangedHandler: func(ctx context.Context, request *mcp.RootsListChangedRequest) {
			state := s.state(request.Session)
			state.mu.Lock()
			state.known = false
			state.generation++
			state.mu.Unlock()
		},
	})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	destructive := true
	writes := &mcp.ToolAnnotations{DestructiveHint: &destructive}
	always := mcp.Meta{"anthropic/alwaysLoad": true}
	s.AddTool(&mcp.Tool{Name: "read", Description: readDescription, InputSchema: json.RawMessage(readSchema), Annotations: readOnly, Meta: always}, s.handle(s.read, false))
	s.AddTool(&mcp.Tool{Name: "grep", Description: grepDescription, InputSchema: json.RawMessage(grepSchema), Annotations: readOnly, Meta: always}, s.handle(s.grep, false))
	s.AddTool(&mcp.Tool{Name: "find", Description: findDescription, InputSchema: json.RawMessage(findSchema), Annotations: readOnly, Meta: always}, s.handle(s.find, false))
	s.AddTool(&mcp.Tool{Name: "edit", Description: EditDescription, InputSchema: json.RawMessage(editSchema), Annotations: writes, Meta: always}, s.handle(s.edit, true))
	s.AddTool(&mcp.Tool{Name: "write", Description: writeDescription, InputSchema: json.RawMessage(writeSchema), Annotations: writes, Meta: always}, s.handle(s.write, true))
	if options.Shell != nil {
		s.AddTool(&mcp.Tool{Name: "nibash", Description: options.Shell.Description(), InputSchema: json.RawMessage(nibashSchema), Annotations: writes, Meta: always}, s.handle(s.nibash, false))
	}
	return s
}

// Close ends every session's shells.
func (s *Server) Close() {
	s.mu.Lock()
	keys := slices.Collect(maps.Keys(s.sessions))
	s.mu.Unlock()
	for _, key := range keys {
		s.forget(key)
	}
}

// forget ends a client's shells, its subagents' too, and drops its state.
func (s *Server) forget(key *mcp.ServerSession) {
	s.mu.Lock()
	state := s.sessions[key]
	delete(s.sessions, key)
	s.mu.Unlock()
	if state == nil || s.options.Shell == nil {
		return
	}
	state.mu.Lock()
	agents := append([]string{""}, state.shells...)
	state.mu.Unlock()
	for _, agent := range agents {
		s.options.Shell.Close(shellKey{key, agent})
	}
}

// state is a client's state, made on first use; it is forgotten when the client's connection ends.
func (s *Server) state(key *mcp.ServerSession) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.sessions[key]
	if state == nil {
		state = &session{approved: map[string]bool{}, paused: map[string]paused{}}
		s.sessions[key] = state
		go func() {
			_ = key.Wait()
			s.forget(key)
		}()
	}
	return state
}

// shell is the key of the calling agent's shell, marked most recently used; an agent past the limit closes the least recently used one.
// Making the client's state here also makes sure its shells are closed with its connection.
func (s *Server) shell(c *call) shellKey {
	state := s.state(c.key)
	if c.agent == "" {
		return shellKey{c.key, ""}
	}
	state.mu.Lock()
	state.shells = append(slices.DeleteFunc(state.shells, func(agent string) bool { return agent == c.agent }), c.agent)
	var evicted []string
	if len(state.shells) > shellLimit {
		evicted = slices.Clone(state.shells[:len(state.shells)-shellLimit])
		state.shells = slices.Clone(state.shells[len(state.shells)-shellLimit:])
	}
	state.mu.Unlock()
	for _, agent := range evicted {
		s.options.Shell.Close(shellKey{c.key, agent})
	}
	return shellKey{c.key, c.agent}
}

// call is one tool call's exchange with its client: the agent making it, and the answers it carries, from this round and the earlier rounds
// of the same call.
type call struct {
	key     *mcp.ServerSession
	agent   string
	answers mcp.InputResponseMap
}

// question is what a paused call asks the client, under an id its answer comes back with.
type question struct {
	id      string
	request mcp.InputRequest
}

// open starts a call with the agent making it and the answers it brings: those of its earlier rounds, kept under its request state with the
// agent the first round found, and this round's. An agent's first call starts it first.
func (s *Server) open(request *mcp.CallToolRequest) *call {
	c := &call{key: request.Session, answers: mcp.InputResponseMap{}}
	resumed := false
	if token := request.Params.RequestState; token != "" {
		state := s.state(request.Session)
		state.mu.Lock()
		if earlier, ok := state.paused[token]; ok {
			maps.Copy(c.answers, earlier.answers)
			c.agent, resumed = earlier.agent, true
			delete(state.paused, token)
			state.order = slices.DeleteFunc(state.order, func(t string) bool { return t == token })
		}
		state.mu.Unlock()
	}
	if !resumed {
		var from *origin
		c.agent, from = s.identify(request)
		s.seeding.Lock()
		s.start(c.key, c.agent, from, originDepth)
		s.seeding.Unlock()
	}
	maps.Copy(c.answers, request.Params.InputResponses)
	return c
}

// identify names the agent making a first round: an OpenCode session by the id OpenCode sends in the call's _meta, a Claude Code subagent by
// the record nib's hook filed under the tool_use id Claude Code sends there, else the main agent. from is set for a Claude Code subagent
// whose parent is known; an OpenCode session's origin is looked up when it starts.
func (s *Server) identify(request *mcp.CallToolRequest) (agent string, from *origin) {
	meta := request.Params.Meta
	if id, _ := meta["ai.opencode/sessionID"].(string); openCodeID.MatchString(id) {
		return openCodeSession + id, nil
	}
	toolUse, _ := meta["claudecode/toolUseId"].(string)
	if s.options.Agent == nil || toolUse == "" {
		return "", nil
	}
	found := s.options.Agent(toolUse)
	if found.ID == "" {
		return "", nil
	}
	if found.Spawned {
		from = &origin{fork: found.Forked, at: found.At}
		if found.Parent != "" {
			from.parent = claudeAgent + found.Parent
		}
	}
	return claudeAgent + found.ID, from
}

// start gives a subagent, on the first call nib sees from it, what it inherits from its parent: every subagent the named directories its
// parent saw before the subagent's moment, and a fork, which starts with its parent's conversation up to then, a copy of the lines its parent
// had seen before it and a copy of its parent's shell. An OpenCode session's origin is asked of OpenCode only here, and a parent session that
// never called nib is started first. A copy, not a share: neither sees what the other reads or defines later. REAPPLY tokens stay the
// parent's, as each applies once. Without a moment, the parent's lines and names are taken as they are at this call, which may be later than
// the subagent's start; the shell always is. The caller holds seeding.
func (s *Server) start(key *mcp.ServerSession, agent string, from *origin, depth int) {
	if s.started[agent] || (from == nil && !strings.HasPrefix(agent, openCodeSession)) {
		return
	}
	s.started[agent] = true
	if from == nil {
		if s.options.Origin == nil {
			return
		}
		found, ok := s.options.Origin(strings.TrimPrefix(agent, openCodeSession))
		if !ok || !openCodeID.MatchString(found.Parent) {
			return
		}
		from = &origin{openCodeSession + found.Parent, found.Fork, found.At}
		if depth > 0 {
			s.start(key, from.parent, nil, depth-1)
		}
	}
	s.defined.Inherit(s.engineSession(agent), s.engineSession(from.parent), from.at)
	if !from.fork {
		return
	}
	s.engine.Store.Copy(s.engineSession(from.parent), s.engineSession(agent), from.at)
	if s.options.Shell != nil {
		s.options.Shell.Fork(shellKey{key, from.parent}, s.shell(&call{key: key, agent: agent}))
	}
}

// engineSession is the engine session of agent: its name, or for the main agent the server's session.
func (s *Server) engineSession(agent string) string {
	if agent != "" {
		return agent
	}
	if s.options.Session != "" {
		return s.options.Session
	}
	return "main"
}

// pause ends a call with its question; its answers so far and its agent stay with the session under the request state the client sends back.
func (s *Server) pause(c *call, q question) *mcp.CallToolResult {
	token := rand.Text()
	state := s.state(c.key)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.paused[token] = paused{c.answers, c.agent}
	state.order = append(state.order, token)
	for len(state.order) > pausedLimit {
		delete(state.paused, state.order[0])
		state.order = state.order[1:]
	}
	return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{q.id: q.request}, RequestState: token}
}

// multiRound is protocol 2026-07-28 and later, where a server sends no requests of its own: a call that needs the client's input ends with
// input requests, and the client sends it again with the answers.
func multiRound(key *mcp.ServerSession) bool {
	params := key.InitializeParams()
	return params != nil && params.ProtocolVersion >= "2026-07-28"
}

// ask is the client's answer to request: one the call carries, else a request of its own on older protocols; on newer ones the call pauses.
// The SDK could also ask older clients for a paused call, but only once per call, and one edit can need several answers.
func (s *Server) ask(ctx context.Context, c *call, id string, request mcp.InputRequest) (mcp.InputResponse, error) {
	if answer, ok := c.answers[id]; ok {
		return answer, nil
	}
	if multiRound(c.key) {
		return nil, &engine.Paused{Request: question{id, request}}
	}
	switch request := request.(type) {
	case *mcp.ListRootsParams:
		result, err := c.key.ListRoots(ctx, request)
		if err != nil {
			return nil, err
		}
		return result, nil
	case *mcp.ElicitParams:
		result, err := c.key.Elicit(ctx, request)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("nib cannot ask a client for %T", request)
}

// roots are the client's roots, or the server's directory when the client has none. When listing them fails, the last listing that worked
// stays in force; with none yet, the sandbox refuses rather than falling back to the server's directory, which may be wider.
func (s *Server) roots(ctx context.Context, c *call) ([]string, error) {
	params := c.key.InitializeParams()
	if params == nil || params.Capabilities == nil || params.Capabilities.RootsV2 == nil {
		return []string{s.options.Directory}, nil
	}
	state := s.state(c.key)
	state.mu.Lock()
	known, last, generation := state.known, state.roots, state.generation
	state.mu.Unlock()
	if known {
		return last, nil
	}
	answer, err := s.ask(ctx, c, "roots", &mcp.ListRootsParams{})
	var paused *engine.Paused
	if errors.As(err, &paused) {
		return nil, err
	}
	listed, _ := answer.(*mcp.ListRootsResult)
	if err == nil && listed == nil {
		err = fmt.Errorf("the client answered with %T", answer)
	}
	if err != nil {
		if last != nil {
			return last, nil
		}
		if s.options.Sandbox {
			return nil, fmt.Errorf("the client's roots could not be listed (%v), so the sandbox allows nothing yet", err)
		}
		return []string{s.options.Directory}, nil
	}
	var paths []string
	for _, root := range listed.Roots {
		if parsed, err := url.Parse(root.URI); err == nil && parsed.Scheme == "file" {
			paths = append(paths, filepath.FromSlash(parsed.Path))
		}
	}
	if len(paths) == 0 {
		paths = []string{s.options.Directory}
	}
	state.mu.Lock()
	// Roots that changed while they were being listed are used for this call and listed again for the next.
	state.roots, state.known = paths, state.generation == generation
	state.mu.Unlock()
	return paths, nil
}

// authorize asks the user before writing outside the roots; approval of a directory lasts for the session.
func (s *Server) authorize(c *call) func(context.Context, string, string) error {
	return func(ctx context.Context, canonical, action string) error {
		state := s.state(c.key)
		state.mu.Lock()
		for directory := filepath.Dir(canonical); ; directory = filepath.Dir(directory) {
			if state.approved[directory] {
				state.mu.Unlock()
				return nil
			}
			if filepath.Dir(directory) == directory {
				break
			}
		}
		state.mu.Unlock()
		params := c.key.InitializeParams()
		if params == nil || params.Capabilities == nil || params.Capabilities.Elicitation == nil {
			return fmt.Errorf("%s is outside the workspace roots and this client cannot confirm it; refusing to %s it", canonical, action)
		}
		everything := fmt.Sprintf("everything under %s this session", filepath.Dir(canonical))
		answer, err := s.ask(ctx, c, "allow "+action+" "+canonical, &mcp.ElicitParams{
			Mode:    "form",
			Message: fmt.Sprintf("nib wants to %s %s, which is outside the workspace. Allow it?", action, canonical),
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"scope": map[string]any{"type": "string", "title": "Allow", "enum": []string{"this file once", everything}}},
				"required":   []string{"scope"},
			},
		})
		if err != nil {
			return err
		}
		result, _ := answer.(*mcp.ElicitResult)
		if result == nil || result.Action != "accept" {
			return fmt.Errorf("The user declined to let nib %s %s", action, canonical)
		}
		if scope, _ := result.Content["scope"].(string); strings.HasPrefix(scope, "everything") {
			state.mu.Lock()
			state.approved[filepath.Dir(canonical)] = true
			state.mu.Unlock()
		}
		return nil
	}
}

// context is the engine context for one call; roots, when listed for it, stay the same for the whole call. Each agent has its own snapshot
// session, so lines one agent saw never let another edit them, and its own view of the named directories.
func (s *Server) context(ctx context.Context, c *call, roots []string) *engine.Context {
	e := &engine.Context{
		Ctx:       ctx,
		Directory: s.options.Directory,
		Session:   s.engineSession(c.agent),
		Sandbox:   s.options.Sandbox,
	}
	if roots != nil {
		e.Roots = func(context.Context) ([]string, error) { return roots, nil }
	}
	if s.options.ConfirmOutsideRoots {
		e.Authorize = s.authorize(c)
	} else {
		e.Authorize = func(context.Context, string, string) error { return nil }
	}
	e.Names = s.options.Names.For(s.defined, s.engineSession(c.agent))
	return e
}

type action func(ctx *engine.Context, args arguments, notes *[]string, c *call) (engine.Result, error)

// handle runs an action; notes say what nib read differently than sent and lead the response, a failed one too, so the next call can be sent right.
// The roots a call depends on, those of the sandbox and of a write that may need confirming, are listed before any work, so a call that pauses
// to ask for them has recorded nothing as seen; a confirmation pauses before an edit is staged.
func (s *Server) handle(run action, writes bool) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var notes []string
		args := arguments{}
		if len(request.Params.Arguments) > 0 {
			if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
				return failure(nil, errors.New("arguments must be a JSON object")), nil
			}
		}
		c := s.open(request)
		var roots []string
		result, err := protect(func() (engine.Result, error) {
			if s.options.Sandbox || (s.options.ConfirmOutsideRoots && writes) {
				listed, err := s.roots(ctx, c)
				if err != nil {
					return engine.Result{}, err
				}
				roots = listed
			}
			return run(s.context(ctx, c, roots), args, &notes, c)
		})
		var paused *engine.Paused
		if errors.As(err, &paused) {
			if q, ok := paused.Request.(question); ok {
				return s.pause(c, q), nil
			}
		}
		if err != nil {
			return failure(notes, err), nil
		}
		text := result.Text
		if len(notes) > 0 {
			text = strings.Join(notes, "\n") + "\n\n" + text
		}
		content := []mcp.Content{&mcp.TextContent{Text: text}}
		for _, image := range result.Images {
			content = append(content, &mcp.ImageContent{Data: image.Data, MIMEType: image.MimeType})
		}
		return &mcp.CallToolResult{Content: content}, nil
	}
}

// protect turns a panic in one call into that call's error, logged with its stack, so one bad input never ends the server and its other sessions.
func protect(run func() (engine.Result, error)) (result engine.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "nib: internal error: %v\n%s", r, debug.Stack())
			err = fmt.Errorf("nib hit an internal error (%v); nothing more was done. Please report it with the call that caused it", r)
		}
	}()
	return run()
}

func failure(notes []string, err error) *mcp.CallToolResult {
	text := err.Error()
	if len(notes) > 0 {
		text = strings.Join(notes, "\n") + "\n\n" + text
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// arguments are a call's raw arguments.
type arguments map[string]json.RawMessage

func (a arguments) has(name string) bool {
	value, ok := a[name]
	return ok && string(value) != "null"
}

// text is a string argument; ok is false when it is absent.
func (a arguments) text(name string) (string, bool, error) {
	if !a.has(name) {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(a[name], &value); err != nil {
		return "", false, fmt.Errorf("%s must be a string", name)
	}
	return value, true, nil
}

// number is a whole number of at least min; numbers sent as strings are read as numbers.
func (a arguments) number(name string, least int) (*int, error) {
	if !a.has(name) {
		return nil, nil
	}
	raw := strings.TrimSpace(string(a[name]))
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(a[name], &s); err != nil {
			return nil, fmt.Errorf("%s must be a whole number", name)
		}
		raw = strings.TrimSpace(s)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value != math.Trunc(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("%s must be a whole number", name)
	}
	if value < float64(least) {
		return nil, fmt.Errorf("%s must be at least %d", name, least)
	}
	n := int(math.Min(value, math.MaxInt32))
	return &n, nil
}

// flag is a boolean; "true" and "false" sent as strings are read as booleans, with a note.
func (a arguments) flag(name string, notes *[]string) (bool, bool, error) {
	if !a.has(name) {
		return false, false, nil
	}
	var value bool
	if err := json.Unmarshal(a[name], &value); err == nil {
		return value, true, nil
	}
	var s string
	if err := json.Unmarshal(a[name], &s); err == nil && (s == "true" || s == "false") {
		*notes = append(*notes, fmt.Sprintf("[%s was the string %q; it was read as the boolean %s, so send true or false without quotes]", name, s, s))
		return s == "true", true, nil
	}
	return false, false, fmt.Errorf("%s must be true or false", name)
}

// list is a string or a list of strings; a string holding a JSON list, as small models send, is read as the list it spells, with a note, also
// with the end of their tool call's markup after it. Any other text after the list leaves it one path.
func (a arguments) list(name string, notes *[]string) ([]string, error) {
	if !a.has(name) {
		return nil, nil
	}
	var items []string
	if err := json.Unmarshal(a[name], &items); err == nil {
		return items, nil
	}
	var value string
	if err := json.Unmarshal(a[name], &value); err != nil {
		return nil, fmt.Errorf("%s must be a string or a list of strings", name)
	}
	return listOf(name, value, notes), nil
}

func parsedList(text string) []string {
	var items []any
	if json.Unmarshal([]byte(text), &items) != nil || len(items) == 0 {
		return nil
	}
	list := make([]string, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil
		}
		list[i] = s
	}
	return list
}

func listOf(name, value string, notes *[]string) []string {
	if strings.HasPrefix(strings.TrimLeft(value, " \t\n\r"), "[") {
		if whole := parsedList(value); whole != nil {
			*notes = append(*notes, fmt.Sprintf("[%s was a string holding a JSON list; it was read as the list %s, so send the list itself]", name, quoteList(whole)))
			return whole
		}
		for end := strings.Index(value, "]"); end >= 0; {
			rest := value[end+1:]
			if strings.HasPrefix(strings.TrimLeft(rest, " \t\n\r"), ">") {
				if parsed := parsedList(value[:end+1]); parsed != nil {
					shown := rest
					if len([]rune(rest)) > 40 {
						shown = string([]rune(rest)[:40]) + "..."
					}
					quoted, _ := json.Marshal(shown)
					*notes = append(*notes, fmt.Sprintf("[%s was a string holding a JSON list; it was read as the list %s, and the tool-call markup after it (%s) was dropped, so send the list itself]", name, quoteList(parsed), unescapeHTML(string(quoted))))
					return parsed
				}
			}
			next := strings.Index(value[end+1:], "]")
			if next < 0 {
				break
			}
			end += next + 1
		}
	}
	return []string{value}
}

// unescapeHTML undoes encoding/json's escaping of <, > and &, which JSON.stringify leaves alone.
func unescapeHTML(s string) string {
	return strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(s)
}

func quoteList(items []string) string {
	data, _ := json.Marshal(items)
	return unescapeHTML(string(data))
}

var readAliases = []string{"filePath", "file_path", "path", "file"}

// readPaths are read's paths, also from arguments in another read tool's shape: a path under another name, with offset (taken as the first line)
// and limit. A note then shows nib's own shape for the same read.
func readPaths(args arguments, notes *[]string) ([]string, error) {
	if args.has("paths") {
		return args.list("paths", notes)
	}
	name := ""
	for _, key := range readAliases {
		if value, ok, _ := args.text(key); ok && value != "" {
			name = key
			break
		}
	}
	if name == "" {
		return nil, errors.New(`read needs paths, a list like ["src/a.ts", "src/b.ts:120-180"]`)
	}
	file, _, _ := args.text(name)
	offset, _ := args.number("offset", math.MinInt32)
	limit, _ := args.number("limit", math.MinInt32)
	start := 1
	if offset != nil && *offset > 0 {
		start = *offset
	}
	entry := file
	switch {
	case limit != nil && *limit > 0:
		entry = fmt.Sprintf("%s:%d+%d", file, start, *limit)
	case args.has("offset"):
		entry = fmt.Sprintf("%s:%d-", file, start)
	}
	used := []string{name}
	for _, key := range []string{"offset", "limit"} {
		if args.has(key) {
			used = append(used, key)
		}
	}
	verb := "were"
	if len(used) == 1 {
		verb = "was"
	}
	*notes = append(*notes, fmt.Sprintf("[read takes paths, like %s; %s %s read that way]", quoteList([]string{entry}), strings.Join(used, ", "), verb))
	return []string{entry}, nil
}

// patchText is a patch, also when it arrives as one line of JSON escapes; one line can never be a patch, so that is decoded, with a note.
func patchText(input string, notes *[]string) string {
	if strings.Contains(input, "\n") || !strings.Contains(input, `\n`) || reapplyLine(input) {
		return input
	}
	quoted := input
	if !(len(input) > 1 && strings.HasPrefix(input, `"`) && strings.HasSuffix(input, `"`)) {
		quoted = `"` + input + `"`
	}
	var decoded string
	if json.Unmarshal([]byte(quoted), &decoded) != nil {
		return input
	}
	*notes = append(*notes, `[the patch came as one line with escaped line breaks (\n); it was read with real line breaks, so send those]`)
	return decoded
}

func reapplyLine(input string) bool {
	return strings.HasPrefix(strings.TrimLeft(input, " \t\r\n"), "REAPPLY ")
}

// modeAliases are Claude Code's Grep names for grep's modes.
var modeAliases = map[string]string{"content": "matches", "files_with_matches": "files"}

func grepMode(value string, notes *[]string) (string, error) {
	switch value {
	case "", "matches", "files", "count":
		return value, nil
	}
	mode, ok := modeAliases[value]
	if !ok {
		quoted, _ := json.Marshal(value)
		return "", fmt.Errorf("mode %s is not one of matches, files or count", quoted)
	}
	*notes = append(*notes, fmt.Sprintf("[mode %s was read as %s, nib's name for it]", value, mode))
	return mode, nil
}

func (s *Server) read(ctx *engine.Context, args arguments, notes *[]string, _ *call) (engine.Result, error) {
	paths, err := readPaths(args, notes)
	if err != nil {
		return engine.Result{}, err
	}
	maxLineLength, err := args.number("maxLineLength", 1)
	if err != nil {
		return engine.Result{}, err
	}
	plain, _, err := args.flag("plain", notes)
	if err != nil {
		return engine.Result{}, err
	}
	collapse, hasCollapse, err := args.flag("collapse", notes)
	if err != nil {
		return engine.Result{}, err
	}
	info, _, err := args.flag("info", notes)
	if err != nil {
		return engine.Result{}, err
	}
	readArgs := engine.ReadArgs{Paths: paths, MaxLineLength: maxLineLength, Plain: plain, Info: info}
	if hasCollapse {
		readArgs.Collapse = &collapse
	}
	return s.engine.Read(ctx, readArgs)
}

func (s *Server) grep(ctx *engine.Context, args arguments, notes *[]string, _ *call) (engine.Result, error) {
	pattern, ok, err := args.text("pattern")
	if err != nil {
		return engine.Result{}, err
	}
	if !ok || pattern == "" {
		return engine.Result{}, errors.New("grep needs a pattern")
	}
	var paths []string
	// Other grep tools name a single search root path.
	if !args.has("paths") {
		if alias, ok, _ := args.text("path"); ok && alias != "" {
			*notes = append(*notes, fmt.Sprintf("[grep takes paths, a list like %s; path was read that way]", quoteList([]string{alias})))
			paths = []string{alias}
		}
	}
	if paths == nil {
		if paths, err = args.list("paths", notes); err != nil {
			return engine.Result{}, err
		}
	}
	glob, err := args.list("glob", notes)
	if err != nil {
		return engine.Result{}, err
	}
	grepArgs := engine.GrepArgs{Pattern: pattern, Paths: paths, Glob: glob}
	if grepArgs.IgnoreCase, _, err = args.flag("ignoreCase", notes); err != nil {
		return engine.Result{}, err
	}
	if grepArgs.Literal, _, err = args.flag("literal", notes); err != nil {
		return engine.Result{}, err
	}
	if grepArgs.NoIgnore, _, err = args.flag("noIgnore", notes); err != nil {
		return engine.Result{}, err
	}
	for name, target := range map[string]**int{"before": &grepArgs.Before, "after": &grepArgs.After, "skip": &grepArgs.Skip} {
		if *target, err = args.number(name, 0); err != nil {
			return engine.Result{}, err
		}
	}
	if grepArgs.MaxLineLength, err = args.number("maxLineLength", 1); err != nil {
		return engine.Result{}, err
	}
	mode, _, err := args.text("mode")
	if err != nil {
		return engine.Result{}, err
	}
	if grepArgs.Mode, err = grepMode(mode, notes); err != nil {
		return engine.Result{}, err
	}
	if replace, ok, err := args.text("replace"); err != nil {
		return engine.Result{}, err
	} else if ok {
		grepArgs.Replace = &replace
	}
	return s.engine.Grep(ctx, grepArgs)
}

func (s *Server) find(ctx *engine.Context, args arguments, notes *[]string, _ *call) (engine.Result, error) {
	paths, err := args.list("paths", notes)
	if err != nil {
		return engine.Result{}, err
	}
	glob, err := args.list("glob", notes)
	if err != nil {
		return engine.Result{}, err
	}
	findArgs := engine.FindArgs{Paths: paths, Glob: glob}
	if findArgs.NoIgnore, _, err = args.flag("noIgnore", notes); err != nil {
		return engine.Result{}, err
	}
	for name, target := range map[string]**string{"minSize": &findArgs.MinSize, "newer": &findArgs.Newer} {
		if value, ok, err := args.text(name); err != nil {
			return engine.Result{}, err
		} else if ok {
			*target = &value
		}
	}
	sortBy, _, err := args.text("sort")
	if err != nil {
		return engine.Result{}, err
	}
	if sortBy != "" && !slices.Contains([]string{"path", "size", "modified"}, sortBy) {
		return engine.Result{}, fmt.Errorf("sort %q is not one of path, size or modified", sortBy)
	}
	findArgs.Sort = sortBy
	if findArgs.Skip, err = args.number("skip", 0); err != nil {
		return engine.Result{}, err
	}
	return s.engine.Find(ctx, findArgs)
}

func (s *Server) edit(ctx *engine.Context, args arguments, notes *[]string, _ *call) (engine.Result, error) {
	input, ok, err := args.text("input")
	if err != nil {
		return engine.Result{}, err
	}
	if !ok || input == "" {
		return engine.Result{}, errors.New("edit needs input, the complete hashline patch")
	}
	return s.engine.Patch(ctx, patchText(input, notes))
}

var tagPattern = func(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func (s *Server) write(ctx *engine.Context, args arguments, notes *[]string, _ *call) (engine.Result, error) {
	path, ok, err := args.text("path")
	if err != nil {
		return engine.Result{}, err
	}
	if !ok || path == "" {
		return engine.Result{}, errors.New("write needs path")
	}
	content, ok, err := args.text("content")
	if err != nil {
		return engine.Result{}, err
	}
	if !ok {
		return engine.Result{}, errors.New("write needs content")
	}
	if len(content) > engine.TextLimit {
		return engine.Result{}, fmt.Errorf("content exceeds %d bytes", engine.TextLimit)
	}
	tagValue, _, err := args.text("tag")
	if err != nil {
		return engine.Result{}, err
	}
	if tagValue != "" && !tagPattern(tagValue) {
		return engine.Result{}, errors.New("tag is 4 hex digits, like 3C4D")
	}
	return s.engine.Write(ctx, path, content, tagValue)
}

func (s *Server) nibash(ctx *engine.Context, args arguments, notes *[]string, c *call) (engine.Result, error) {
	command, ok, err := args.text("command")
	if err != nil {
		return engine.Result{}, err
	}
	if !ok || strings.TrimSpace(command) == "" {
		return engine.Result{}, errors.New("nibash needs a command")
	}
	call := ShellCall{Command: command}
	if call.Description, _, err = args.text("description"); err != nil {
		return engine.Result{}, err
	}
	if call.Timeout, err = args.number("timeout", 1); err != nil {
		return engine.Result{}, err
	}
	if call.Background, _, err = args.flag("background", notes); err != nil {
		return engine.Result{}, err
	}
	return s.options.Shell.Run(ctx.Ctx, s.shell(c), call, ctx)
}

// Engine is the server's engine, which nibash's own commands share.
func (s *Server) Engine() *engine.Engine { return s.engine }
