package server

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/opencode"
)

// client is a session of the SDK's own client, on its latest protocol (2026-07-28 or later), against a server in memory.
type client struct {
	server  *Server
	session *mcp.ClientSession
	mu      sync.Mutex
	asked   []string
}

// connect starts a server and a client with roots and, with answer, elicitation; manual turns off the client's own handling of input
// requests, so a test sees them.
func connect(t *testing.T, options Options, roots []string, answer func(message string) *mcp.ElicitResult, manual bool) *client {
	t.Helper()
	ctx := context.Background()
	c := &client{server: New(options)}
	clientOptions := &mcp.ClientOptions{}
	if manual {
		clientOptions.MultiRoundTrip = &mcp.MultiRoundTripOptions{Disabled: true}
	}
	if answer != nil {
		clientOptions.ElicitationHandler = func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			c.mu.Lock()
			c.asked = append(c.asked, request.Params.Message)
			c.mu.Unlock()
			return answer(request.Params.Message), nil
		}
	}
	sdk := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, clientOptions)
	for _, root := range roots {
		sdk.AddRoots(&mcp.Root{URI: (&url.URL{Scheme: "file", Path: root}).String()})
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := c.server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := sdk.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	if version := session.InitializeResult().ProtocolVersion; version < "2026-07-28" {
		t.Fatalf("the client negotiated %s; these tests need 2026-07-28 or later", version)
	}
	c.session = session
	t.Cleanup(func() { session.Close() })
	return c
}

func (c *client) call(t *testing.T, params *mcp.CallToolParams) *mcp.CallToolResult {
	t.Helper()
	result, err := c.session.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("%s: %v", params.Name, err)
	}
	return result
}

func text(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if t, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// seen is the snapshot store's state for the main session: every path with its versions' tags and seen and known lines.
func (c *client) seen() string { return c.seenBy("main") }

// seenBy is the snapshot store's state for one engine session.
func (c *client) seenBy(session string) string {
	store := c.server.Engine().Store
	var out []string
	for _, path := range store.Paths(session) {
		for _, version := range store.Versions(session, path) {
			lines := func(set map[int]bool) []int {
				var sorted []int
				for line := range set {
					sorted = append(sorted, line)
				}
				slices.Sort(sorted)
				return sorted
			}
			out = append(out, fmt.Sprintf("%s#%s seen %v known %v", path, version.Tag, lines(version.Seen), lines(version.Known)))
		}
	}
	return strings.Join(out, "\n")
}

func file(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func accept(scope string) func(string) *mcp.ElicitResult {
	return func(message string) *mcp.ElicitResult {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"scope": scope}}
	}
}

var tagPatternText = regexp.MustCompile(`#([0-9A-F]{4})\]`)

func TestSandboxListsRootsByInputRequest(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	outside := resolvedPath(t, t.TempDir())
	file(t, filepath.Join(directory, "a.txt"), "one\ntwo\n")
	file(t, filepath.Join(outside, "b.txt"), "secret\n")
	c := connect(t, Options{Directory: directory, Sandbox: true}, []string{directory}, nil, false)
	inside := c.call(t, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{filepath.Join(directory, "a.txt")}}})
	if inside.IsError || !strings.Contains(text(inside), "1:one") {
		t.Fatalf("a read inside the roots must work, got %q", text(inside))
	}
	away := c.call(t, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{filepath.Join(outside, "b.txt")}}})
	if strings.Contains(text(away), "secret") || !strings.Contains(text(away), "outside the sandbox") {
		t.Fatalf("a read outside the roots must be refused, got %q", text(away))
	}
	written := c.call(t, &mcp.CallToolParams{Name: "write", Arguments: map[string]any{"path": filepath.Join(directory, "new.txt"), "content": "x\n"}})
	if written.IsError {
		t.Fatalf("a write inside the roots must work, got %q", text(written))
	}
	refused := c.call(t, &mcp.CallToolParams{Name: "write", Arguments: map[string]any{"path": filepath.Join(outside, "new.txt"), "content": "x\n"}})
	if !refused.IsError {
		t.Fatal("a write outside the roots must be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("the refused write must not create the file")
	}
}

func TestPausedCallRecordsNothing(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	file(t, filepath.Join(directory, "a.txt"), "one\ntwo\n")
	c := connect(t, Options{Directory: directory, Sandbox: true}, []string{directory}, nil, true)
	params := &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{filepath.Join(directory, "a.txt")}}}
	first := c.call(t, params)
	request, ok := first.InputRequests["roots"]
	if !first.NeedsInput() || !ok || len(first.Content) != 0 {
		t.Fatalf("the first sandboxed call must ask for the roots and show nothing, got %+v", first)
	}
	if _, ok := request.(*mcp.ListRootsParams); !ok {
		t.Fatalf("the input request must list roots, got %T", request)
	}
	if seen := c.seen(); seen != "" {
		t.Fatalf("a paused call must record nothing as seen, got %s", seen)
	}
	retry := *params
	retry.InputResponses = mcp.InputResponseMap{"roots": &mcp.ListRootsResult{Roots: []*mcp.Root{{URI: (&url.URL{Scheme: "file", Path: directory}).String()}}}}
	retry.RequestState = first.RequestState
	second := c.call(t, &retry)
	if second.NeedsInput() || !strings.Contains(text(second), "1:one") {
		t.Fatalf("the call sent again with the roots must run, got %q", text(second))
	}
	if !strings.Contains(c.seen(), "a.txt") {
		t.Fatal("the completed read must record what it showed")
	}
	third := c.call(t, params)
	if third.NeedsInput() {
		t.Fatal("known roots must not be asked for again")
	}
}

func TestEditOutsideRootsAsksOncePerDirectory(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	one := resolvedPath(t, t.TempDir())
	two := resolvedPath(t, t.TempDir())
	c := connect(t, Options{Directory: directory, ConfirmOutsideRoots: true}, []string{directory}, accept("this file once"), false)
	input := fmt.Sprintf("*** Add File: %s\n+first\n*** Add File: %s\n+second\n", filepath.Join(one, "a.txt"), filepath.Join(two, "b.txt"))
	result := c.call(t, &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": input}})
	if result.IsError {
		t.Fatalf("the edit must apply after both confirmations, got %q", text(result))
	}
	if len(c.asked) != 2 || !strings.Contains(c.asked[0], one) || !strings.Contains(c.asked[1], two) {
		t.Fatalf("each outside file must be confirmed once, in order, got %q", c.asked)
	}
	for path, want := range map[string]string{filepath.Join(one, "a.txt"): "first\n", filepath.Join(two, "b.txt"): "second\n"} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Fatalf("%s: got %q, %v", path, got, err)
		}
	}
}

func TestDeclinedEditWritesNothing(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	one := resolvedPath(t, t.TempDir())
	two := resolvedPath(t, t.TempDir())
	answers := 0
	c := connect(t, Options{Directory: directory, ConfirmOutsideRoots: true}, []string{directory}, func(string) *mcp.ElicitResult {
		answers++
		if answers == 1 {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"scope": "this file once"}}
		}
		return &mcp.ElicitResult{Action: "decline"}
	}, false)
	input := fmt.Sprintf("*** Add File: %s\n+first\n*** Add File: %s\n+second\n", filepath.Join(one, "a.txt"), filepath.Join(two, "b.txt"))
	result := c.call(t, &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": input}})
	if !result.IsError || !strings.Contains(text(result), "declined") {
		t.Fatalf("a declined confirmation must refuse the edit, got %q", text(result))
	}
	for _, path := range []string{filepath.Join(one, "a.txt"), filepath.Join(two, "b.txt")} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s must not be written when a confirmation is declined", path)
		}
	}
}

func TestDirectoryApprovalLastsForTheSession(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	outside := resolvedPath(t, t.TempDir())
	c := connect(t, Options{Directory: directory, ConfirmOutsideRoots: true}, []string{directory}, accept("everything under "+outside+" this session"), false)
	for _, name := range []string{"a.txt", "b.txt"} {
		result := c.call(t, &mcp.CallToolParams{Name: "write", Arguments: map[string]any{"path": filepath.Join(outside, name), "content": "x\n"}})
		if result.IsError {
			t.Fatalf("%s: %q", name, text(result))
		}
	}
	if len(c.asked) != 1 {
		t.Fatalf("an approved directory must not be asked about again, asked %d times", len(c.asked))
	}
}

func TestReapplyTokenSurvivesAConfirmation(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	outside := resolvedPath(t, t.TempDir())
	path := filepath.Join(outside, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\nsix\n")
	c := connect(t, Options{Directory: directory, ConfirmOutsideRoots: true}, []string{directory}, accept("this file once"), true)
	read := c.call(t, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{path + ":1-2"}}})
	tag := tagPatternText.FindStringSubmatch(text(read))
	if tag == nil {
		t.Fatalf("no tag in %q", text(read))
	}
	before := c.seen()
	// Line 5 was never shown, so once confirmed the edit is refused with a token; the confirmation must come first and record nothing.
	params := &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, tag[1])}}
	// answer answers every round the call pauses for, checking each pause showed and recorded nothing; it returns the result and what was asked.
	answer := func(params *mcp.CallToolParams) (*mcp.CallToolResult, []string) {
		t.Helper()
		var asked []string
		result := c.call(t, params)
		for result.NeedsInput() {
			if len(result.InputRequests) != 1 || len(result.Content) != 0 {
				t.Fatalf("a paused call must ask one thing and show nothing, got %+v", result)
			}
			if seen := c.seen(); seen != before {
				t.Fatalf("a paused call must record nothing as seen:\nbefore %s\nafter %s", before, seen)
			}
			retry := *params
			retry.InputResponses = mcp.InputResponseMap{}
			for id, request := range result.InputRequests {
				switch request.(type) {
				case *mcp.ListRootsParams:
					asked = append(asked, "roots")
					retry.InputResponses[id] = &mcp.ListRootsResult{Roots: []*mcp.Root{{URI: (&url.URL{Scheme: "file", Path: directory}).String()}}}
				case *mcp.ElicitParams:
					asked = append(asked, "confirm")
					retry.InputResponses[id] = &mcp.ElicitResult{Action: "accept", Content: map[string]any{"scope": "this file once"}}
				default:
					t.Fatalf("unexpected input request %T", request)
				}
			}
			retry.RequestState = result.RequestState
			result = c.call(t, &retry)
		}
		return result, asked
	}
	refused, asked := answer(params)
	if !slices.Equal(asked, []string{"roots", "confirm"}) {
		t.Fatalf("the first edit must ask for the roots, then the confirmation, got %q and %q", asked, text(refused))
	}
	token := regexp.MustCompile(`REAPPLY (\S+)`).FindStringSubmatch(text(refused))
	if !refused.IsError || token == nil {
		t.Fatalf("the confirmed edit of an unseen line must be refused with a token, got %q", text(refused))
	}
	before = c.seen()
	reapply := &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": "REAPPLY " + token[1]}}
	applied, asked := answer(reapply)
	if !slices.Equal(asked, []string{"confirm"}) {
		t.Fatalf("a file allowed once must be confirmed again, and known roots not asked for, got %q", asked)
	}
	if applied.IsError {
		t.Fatalf("the token must survive the confirmation it paused for, got %q", text(applied))
	}
	if got, _ := os.ReadFile(path); string(got) != "one\ntwo\nthree\nfour\nFIVE\nsix\n" {
		t.Fatalf("got %q", got)
	}
}

// records stands in for the hook's agent records: each tool_use id names an agent once, as Take does.
type records struct {
	mu    sync.Mutex
	byUse map[string]hookstate.Agent
}

func (r *records) take(toolUse string) hookstate.Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent := r.byUse[toolUse]
	delete(r.byUse, toolUse)
	return agent
}

var uses = 0

// as is a call made by agent, "" for the main agent, the way Claude Code sends it: a fresh tool_use id in _meta, filed by the hook for a
// subagent only.
func (r *records) as(agent string, params *mcp.CallToolParams) *mcp.CallToolParams {
	return r.by(hookstate.Agent{ID: agent}, params)
}

// by is a call made by agent as the hook filed it, a fork with its parent.
func (r *records) by(agent hookstate.Agent, params *mcp.CallToolParams) *mcp.CallToolParams {
	uses++
	toolUse := fmt.Sprintf("toolu_%d", uses)
	if agent.ID != "" {
		r.mu.Lock()
		r.byUse[toolUse] = agent
		r.mu.Unlock()
	}
	sent := *params
	sent.Meta = mcp.Meta{"claudecode/toolUseId": toolUse, "progressToken": uses}
	return &sent
}

// inSession is a call the way OpenCode v2 sends it, naming its session in _meta.
func inSession(session string, params *mcp.CallToolParams) *mcp.CallToolParams {
	uses++
	sent := *params
	sent.Meta = mcp.Meta{"ai.opencode/sessionID": session, "progressToken": uses}
	return &sent
}

// latestSeen is the lines one engine session saw of the newest version it holds of path.
func (c *client) latestSeen(session, path string) []int {
	versions := c.server.Engine().Store.Versions(session, path)
	if len(versions) == 0 {
		return nil
	}
	var lines []int
	for line := range versions[0].Seen {
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}

func readCall(path string) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{path}}}
}

func editCall(input string) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": input}}
}

func TestEachAgentEditsOnlyWhatItSaw(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	c := connect(t, Options{Directory: directory, Agent: r.take}, []string{directory}, nil, false)
	main := c.call(t, r.as("", readCall(path+":1-3")))
	tag := tagPatternText.FindStringSubmatch(text(main))[1]
	sub := c.call(t, r.as("a1", readCall(path+":5-6")))
	if !strings.Contains(text(sub), "5:five") {
		t.Fatalf("got %q", text(sub))
	}
	if !strings.Contains(c.seen(), "seen [1 2 3] ") || !strings.Contains(c.seenBy("agent:a1"), "seen [5 6]") {
		t.Fatalf("a subagent's read must count for it alone:\nmain %s\nagent %s", c.seen(), c.seenBy("agent:a1"))
	}
	refused := c.call(t, r.as("", editCall(fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, tag))))
	if !refused.IsError || !strings.Contains(text(refused), "never displayed") {
		t.Fatalf("the main agent must not edit a line only a subagent saw, got %q", text(refused))
	}
	// A session with no version of a file is not restricted, as after a restart, so the second subagent has read part of it.
	c.call(t, r.as("a2", readCall(path+":8")))
	other := c.call(t, r.as("a2", editCall(fmt.Sprintf("[%s#%s]\nPUT 6:\n+SIX", path, tag))))
	if !other.IsError || !strings.Contains(text(other), "never displayed") {
		t.Fatalf("one subagent must not edit a line another saw, got %q", text(other))
	}
	unseen := c.call(t, r.as("a1", editCall(fmt.Sprintf("[%s#%s]\nPUT 2:\n+TWO", path, tag))))
	if !unseen.IsError || !strings.Contains(text(unseen), "never displayed") {
		t.Fatalf("a subagent must not edit a line only the main agent saw, got %q", text(unseen))
	}
	if got, _ := os.ReadFile(path); string(got) != "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n" {
		t.Fatalf("a refused edit wrote %q", got)
	}
	applied := c.call(t, r.as("a1", editCall(fmt.Sprintf("[%s#%s]\nPUT 6:\n+SIX", path, tag))))
	if applied.IsError {
		t.Fatalf("a subagent edits the lines it saw, got %q", text(applied))
	}
	if got, _ := os.ReadFile(path); string(got) != "one\ntwo\nthree\nfour\nfive\nSIX\nseven\neight\n" {
		t.Fatalf("got %q", got)
	}
}

func TestCallsWithoutARecordShareTheMainAgentsState(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	// No hook record, no _meta, or no lookup at all: each is the main agent, as before agents were told apart.
	for _, options := range []Options{{Directory: directory, Agent: r.take}, {Directory: directory}} {
		c := connect(t, options, []string{directory}, nil, false)
		c.call(t, r.as("", readCall(path+":1")))
		c.call(t, readCall(path+":2"))
		if !strings.Contains(c.seen(), "seen [1 2]") {
			t.Fatalf("calls without a record must share the main session, got %s", c.seen())
		}
	}
}

func TestAPausedSubagentCallKeepsItsAgent(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	c := connect(t, Options{Directory: directory, Sandbox: true, Agent: r.take}, []string{directory}, nil, true)
	first := c.call(t, r.as("a1", readCall(path)))
	if !first.NeedsInput() {
		t.Fatalf("the sandboxed call must ask for the roots first, got %q", text(first))
	}
	// The hook filed the call once; the round sent again with the answer has no record of its own.
	retry := *readCall(path)
	retry.Meta = mcp.Meta{"claudecode/toolUseId": "toolu_resent"}
	retry.InputResponses = mcp.InputResponseMap{"roots": &mcp.ListRootsResult{Roots: []*mcp.Root{{URI: (&url.URL{Scheme: "file", Path: directory}).String()}}}}
	retry.RequestState = first.RequestState
	second := c.call(t, &retry)
	if !strings.Contains(text(second), "1:one") {
		t.Fatalf("got %q", text(second))
	}
	if c.seen() != "" || !strings.Contains(c.seenBy("agent:a1"), "seen [1 2]") {
		t.Fatalf("the resumed round must count for the subagent that started it:\nmain %s\nagent %s", c.seen(), c.seenBy("agent:a1"))
	}
}

// fakeShell records which shells are run and closed.
type fakeShell struct {
	mu     sync.Mutex
	ran    []shellKey
	closed []shellKey
}

func (f *fakeShell) Description() string { return "fake" }
func (f *fakeShell) Expects() bool       { return false }

// Status lists one command per key a call ran under, so a test sees which keys a client owns.
func (f *fakeShell) Status(owns func(key any) (string, bool), _ int) ShellStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := ShellStatus{Commands: []RunningCommand{}, Jobs: []ShellJob{}}
	for _, key := range f.ran {
		if agent, ok := owns(key); ok {
			status.Commands = append(status.Commands, RunningCommand{Agent: agent, Command: "fake", Started: time.Now().UnixMilli()})
		}
	}
	return status
}

func (f *fakeShell) Run(_ context.Context, key any, _ ShellCall, _ *engine.Context) (engine.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, key.(shellKey))
	return engine.Result{Text: "ok"}, nil
}

func (f *fakeShell) Close(key any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, key.(shellKey))
}

func (f *fakeShell) closedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var agents []string
	for _, key := range f.closed {
		agents = append(agents, key.agent)
	}
	slices.Sort(agents)
	return agents
}

func TestEachAgentHasJobsEndedWithTheConnection(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	r := &records{byUse: map[string]hookstate.Agent{}}
	shell := &fakeShell{}
	c := connect(t, Options{Directory: directory, Shell: shell, Agent: r.take}, []string{directory}, nil, false)
	command := &mcp.CallToolParams{Name: "nibash", Arguments: map[string]any{"command": "pwd"}}
	for _, agent := range []string{"", "a1", "", "a2", "a1"} {
		c.call(t, r.as(agent, command))
	}
	var agents []string
	for _, key := range shell.ran {
		agents = append(agents, key.agent)
	}
	if !slices.Equal(agents, []string{"", "agent:a1", "", "agent:a2", "agent:a1"}) || shell.ran[0] != shell.ran[2] || shell.ran[1] == shell.ran[3] {
		t.Fatalf("each agent must have its own shell, got %v", shell.ran)
	}
	// nibash_status lists every agent of this client, the main agent by name, in text and as structured content.
	status := c.call(t, r.as("a1", &mcp.CallToolParams{Name: "nibash_status"}))
	if got := text(status); !strings.HasPrefix(got, "[main] running 0:00: fake\n[agent:a1] running 0:00: fake\n") {
		t.Fatalf("status text: %q", got)
	}
	if listed, _ := status.StructuredContent.(map[string]any)["commands"].([]any); len(listed) != 5 {
		t.Fatalf("structured status: %#v", status.StructuredContent)
	}
	// Another connection, with the same shell behind it, sees none of them.
	other := connect(t, Options{Directory: directory, Shell: shell, Agent: r.take}, []string{directory}, nil, false)
	if got := text(other.call(t, &mcp.CallToolParams{Name: "nibash_status"})); got != "Nothing running, and no job ended in the last 10 minutes." {
		t.Fatalf("another client's status: %q", got)
	}
	// However many subagents come and go, none has its jobs ended before the connection ends.
	for i := range 20 {
		c.call(t, r.as(fmt.Sprintf("b%02d", i), command))
	}
	if got := shell.closedAgents(); len(got) != 0 {
		t.Fatalf("no agent's jobs may end while the connection is open, got %v", got)
	}
	c.session.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(shell.closedAgents()) < 23 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := shell.closedAgents(); len(got) != 23 || got[0] != "" || !slices.Contains(got, "agent:a1") {
		t.Fatalf("closing the connection must end the main agent's jobs and every subagent's, got %v", got)
	}
}

func TestAForkWithNoMomentStartsWithACopyOfWhatItsParentSawAtItsFirstCall(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	shell := &fakeShell{}
	c := connect(t, Options{Directory: directory, Shell: shell, Agent: r.take}, []string{directory}, nil, false)
	tag := tagPatternText.FindStringSubmatch(text(c.call(t, r.as("", readCall(path+":1-3")))))[1]
	fork := hookstate.Agent{ID: "f1", Spawned: true, Forked: true}
	// The fork's conversation holds its parent's read, so its first call can edit a line only the parent displayed.
	edited := c.call(t, r.by(fork, editCall(fmt.Sprintf("[%s#%s]\nPUT 2:\n+TWO", path, tag))))
	if edited.IsError {
		t.Fatalf("a fork must edit what its parent saw, got %q", text(edited))
	}
	newTag := tagPatternText.FindStringSubmatch(text(edited))[1]
	if !strings.Contains(c.seenBy("agent:f1"), "#"+tag+" seen [1 2 3] ") || strings.Contains(c.seen(), newTag) {
		t.Fatalf("the fork must start with a copy of the parent's lines and keep its edit to itself:\nmain %s\nfork %s", c.seen(), c.seenBy("agent:f1"))
	}
	// Afterwards each one's reads are its own; the fork is seeded once, so the parent's later read never reaches it.
	c.call(t, r.as("", readCall(path+":5")))
	c.call(t, r.by(fork, readCall(path+":7")))
	if got := c.latestSeen("main", path); !slices.Equal(got, []int{5}) {
		t.Fatalf("main saw %v of the edited file", got)
	}
	if got := c.latestSeen("agent:f1", path); slices.Contains(got, 5) || !slices.Contains(got, 7) {
		t.Fatalf("the fork saw %v of the edited file", got)
	}
	refused := c.call(t, r.as("", editCall(fmt.Sprintf("[%s#%s]\nPUT 7:\n+SEVEN", path, newTag))))
	if !refused.IsError || !strings.Contains(text(refused), "never displayed") {
		t.Fatalf("the parent must not edit a line only its fork saw, got %q", text(refused))
	}
	refused = c.call(t, r.by(fork, editCall(fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, newTag))))
	if !refused.IsError || !strings.Contains(text(refused), "never displayed") {
		t.Fatalf("a fork must not edit a line its parent saw after the fork, got %q", text(refused))
	}
}

func TestAForkOfASubagentStartsFromThatSubagent(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	shell := &fakeShell{}
	c := connect(t, Options{Directory: directory, Shell: shell, Agent: r.take}, []string{directory}, nil, false)
	c.call(t, r.as("a1", readCall(path+":1-2")))
	c.call(t, r.as("", readCall(path+":4-5")))
	c.call(t, r.by(hookstate.Agent{ID: "f2", Spawned: true, Forked: true, Parent: "a1"}, &mcp.CallToolParams{Name: "nibash", Arguments: map[string]any{"command": "pwd"}}))
	if got := c.seenBy("agent:f2"); got != c.seenBy("agent:a1") || !strings.Contains(got, "seen [1 2]") {
		t.Fatalf("the fork of a subagent must start with that subagent's lines, got %q", got)
	}
	shell.mu.Lock()
	defer shell.mu.Unlock()
	if shell.ran[0].agent != "agent:f2" {
		t.Fatalf("the fork must run under its own key, got %v", shell.ran)
	}
}

func TestAForkWhoseParentIsUnknownStartsOnItsOwn(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	shell := &fakeShell{}
	c := connect(t, Options{Directory: directory, Shell: shell, Agent: r.take}, []string{directory}, nil, false)
	c.call(t, r.as("", readCall(path)))
	// The hook files a fork it cannot place like any subagent, which starts with nothing seen.
	c.call(t, r.as("f3", &mcp.CallToolParams{Name: "nibash", Arguments: map[string]any{"command": "pwd"}}))
	if got := c.seenBy("agent:f3"); got != "" || shell.ran[0].agent != "agent:f3" {
		t.Fatalf("an agent with no known parent must start empty, got %q", got)
	}
}

func TestOpenCodeSessionsAreKeptApart(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\nsix\n")
	shell := &fakeShell{}
	// No hook lookup is needed: OpenCode names the session making each call itself.
	c := connect(t, Options{Directory: directory, Shell: shell}, []string{directory}, nil, false)
	tag := tagPatternText.FindStringSubmatch(text(c.call(t, inSession("ses_main", readCall(path+":1-3")))))[1]
	c.call(t, inSession("ses_child", readCall(path+":5-6")))
	if !slices.Equal(c.latestSeen("opencode:ses_main", path), []int{1, 2, 3}) || !slices.Equal(c.latestSeen("opencode:ses_child", path), []int{5, 6}) || c.seen() != "" {
		t.Fatalf("each session's reads must count for it alone:\nmain %s\nses_main %s\nses_child %s", c.seen(), c.seenBy("opencode:ses_main"), c.seenBy("opencode:ses_child"))
	}
	refused := c.call(t, inSession("ses_main", editCall(fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, tag))))
	if !refused.IsError || !strings.Contains(text(refused), "never displayed") {
		t.Fatalf("a session must not edit a line only its child saw, got %q", text(refused))
	}
	// An id nib does not accept counts as the main agent's, as do calls naming no session.
	c.call(t, inSession("../x", readCall(path+":4")))
	c.call(t, readCall(path+":2"))
	if got := c.latestSeen("main", path); !slices.Equal(got, []int{2, 4}) {
		t.Fatalf("calls without a session id must share the main state, got %v", got)
	}
	command := &mcp.CallToolParams{Name: "nibash", Arguments: map[string]any{"command": "pwd"}}
	for _, session := range []string{"ses_main", "ses_child", "ses_main"} {
		c.call(t, inSession(session, command))
	}
	shell.mu.Lock()
	defer shell.mu.Unlock()
	if shell.ran[0].agent != "opencode:ses_main" || shell.ran[0] != shell.ran[2] || shell.ran[1].agent != "opencode:ses_child" {
		t.Fatalf("each session must have its own shell, got %v", shell.ran)
	}
}

// clock is a store clock that reads the moment each test sets, so seen lines get moments a test can place a fork's moment between.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func TestAForkStartsWithWhatItsParentSawBeforeItsMoment(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\nsix\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	c := connect(t, Options{Directory: directory, Agent: r.take}, []string{directory}, nil, false)
	at := &clock{now: time.Unix(100, 0)}
	c.server.Engine().Store.Now = at.read
	tag := tagPatternText.FindStringSubmatch(text(c.call(t, r.as("", readCall(path+":1-3")))))[1]
	// The parent reads line 5 after the fork was spawned, before the fork's first call: the fork's conversation does not hold that read.
	at.set(time.Unix(200, 0))
	c.call(t, r.as("", readCall(path+":5")))
	fork := hookstate.Agent{ID: "f1", Spawned: true, Forked: true, At: time.Unix(150, 0)}
	refused := c.call(t, r.by(fork, editCall(fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, tag))))
	if !refused.IsError || !strings.Contains(text(refused), "never displayed") {
		t.Fatalf("a fork must not edit a line its parent saw after the fork was spawned, got %q", text(refused))
	}
	if edited := c.call(t, r.by(fork, editCall(fmt.Sprintf("[%s#%s]\nPUT 2:\n+TWO", path, tag)))); edited.IsError {
		t.Fatalf("a fork must edit a line its parent saw before it was spawned, got %q", text(edited))
	}
	if !strings.Contains(c.seen(), "seen [1 2 3 5] ") {
		t.Fatalf("the parent keeps everything it saw, got %s", c.seen())
	}
}

// origins is a fake of OpenCode's database: where each session comes from, and how often each session was looked up.
type origins struct {
	mu     sync.Mutex
	of     map[string]opencode.Origin
	looked map[string]int
}

func (f *origins) origin(session string) (opencode.Origin, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looked[session]++
	fork, ok := f.of[session]
	return fork, ok
}

func TestAnOpenCodeForkStartsWithWhatItsParentSawBeforeItsBoundary(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, "one\ntwo\nthree\nfour\nfive\nsix\n")
	f := &origins{looked: map[string]int{}, of: map[string]opencode.Origin{
		"ses_fork": {Parent: "ses_parent", Fork: true, At: time.Unix(150, 0)},
		// A fork of a fork that never called nib: the middle one starts from its own parent first.
		"ses_mid":  {Parent: "ses_parent", Fork: true, At: time.Unix(150, 0)},
		"ses_leaf": {Parent: "ses_mid", Fork: true, At: time.Unix(300, 0)},
		"ses_odd":  {Parent: "../x", Fork: true, At: time.Unix(300, 0)},
		// A subagent's child session inherits no lines and no shell.
		"ses_child": {Parent: "ses_parent", At: time.Unix(300, 0)},
	}}
	shell := &fakeShell{}
	c := connect(t, Options{Directory: directory, Shell: shell, Origin: f.origin}, []string{directory}, nil, false)
	at := &clock{now: time.Unix(100, 0)}
	c.server.Engine().Store.Now = at.read
	tag := tagPatternText.FindStringSubmatch(text(c.call(t, inSession("ses_parent", readCall(path+":1-3")))))[1]
	at.set(time.Unix(200, 0))
	c.call(t, inSession("ses_parent", readCall(path+":5")))
	refused := c.call(t, inSession("ses_fork", editCall(fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, tag))))
	if !refused.IsError || !strings.Contains(text(refused), "never displayed") {
		t.Fatalf("a fork must not edit a line its parent saw after the fork's boundary, got %q", text(refused))
	}
	if edited := c.call(t, inSession("ses_fork", editCall(fmt.Sprintf("[%s#%s]\nPUT 2:\n+TWO", path, tag)))); edited.IsError {
		t.Fatalf("a fork must edit a line its parent saw before its boundary, got %q", text(edited))
	}
	c.call(t, inSession("ses_leaf", readCall(path+":6")))
	if got := c.seenBy("opencode:ses_leaf"); !strings.Contains(got, "#"+tag+" seen [1 2 3] ") {
		t.Fatalf("a fork of a fork must start from its parent as that one started, got %q", got)
	}
	c.call(t, inSession("ses_odd", readCall(path+":6")))
	c.call(t, inSession("ses_child", &mcp.CallToolParams{Name: "nibash", Arguments: map[string]any{"command": "pwd"}}))
	if got := c.seenBy("opencode:ses_child"); got != "" {
		t.Fatalf("a child session must start with nothing seen, got %q", got)
	}
	c.call(t, inSession("ses_parent", readCall(path+":6")))
	if got := c.seenBy("opencode:ses_odd"); strings.Contains(got, "seen [1 2 3") {
		t.Fatalf("a parent id nib does not accept must start the session on its own, got %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for session, want := range map[string]int{"ses_parent": 1, "ses_fork": 1, "ses_mid": 1, "ses_leaf": 1, "ses_odd": 1, "ses_child": 1} {
		if f.looked[session] != want {
			t.Errorf("%s was looked up %d times, want %d", session, f.looked[session], want)
		}
	}
}

func TestEveryToolUsesTheCallersNamedDirectories(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	file(t, filepath.Join(directory, "deep", "inside", "a.txt"), "one\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	f := &origins{looked: map[string]int{}, of: map[string]opencode.Origin{"ses_child": {Parent: "ses_parent", At: time.Now().Add(time.Hour)}}}
	c := connect(t, Options{Directory: directory, Agent: r.take, Origin: f.origin}, []string{directory}, nil, false)
	// The main agent defines ~in, as nibash's hash -d does; a subagent with no known parent, and the main agent's sibling sessions, do not see it.
	if err := c.server.options.Names.For(c.server.defined, "main").Define("in", filepath.Join(directory, "deep", "inside")); err != nil {
		t.Fatal(err)
	}
	if got := text(c.call(t, readCall("~in/a.txt"))); !strings.HasPrefix(got, "[~in/a.txt#") {
		t.Fatalf("the definer's read must expand and display its name, got %q", got)
	}
	if got := c.call(t, r.as("a1", readCall("~in/a.txt"))); !got.IsError {
		t.Fatalf("an agent that does not see a name must not expand it, got %q", text(got))
	}
	// An OpenCode session sees the names its parent session saw when the child session was created.
	c.call(t, inSession("ses_parent", readCall("deep/inside/a.txt")))
	if err := c.server.options.Names.For(c.server.defined, "opencode:ses_parent").Define("mine", filepath.Join(directory, "deep")); err != nil {
		t.Fatal(err)
	}
	// A child session is not given the server instructions, so its first call says which names it has.
	if got := text(c.call(t, inSession("ses_child", readCall("~mine/inside/a.txt")))); !strings.HasPrefix(got, namedHeader+"\n~mine = deep\n\n[~mine/inside/a.txt#") {
		t.Fatalf("a child session must see its parent's names, and be told of them, got %q", got)
	}
	if got := text(c.call(t, inSession("ses_child", readCall("~mine/inside/a.txt")))); !strings.HasPrefix(got, "[~mine/inside/a.txt#") {
		t.Fatalf("only a subagent's first call is told its names, got %q", got)
	}
}

func TestAWorktreeSubagentsFileToolsWorkInItsWorktree(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	worktree := filepath.Join(directory, ".claude", "worktrees", "agent-w1")
	file(t, filepath.Join(directory, "a.txt"), "main\n")
	file(t, filepath.Join(worktree, "a.txt"), "worktree\n")
	r := &records{byUse: map[string]hookstate.Agent{}}
	c := connect(t, Options{Directory: directory, Agent: r.take}, []string{directory}, nil, false)
	w1 := hookstate.Agent{ID: "w1", Spawned: true, Worktree: worktree}
	got := text(c.call(t, r.by(w1, readCall("a.txt"))))
	if !strings.HasPrefix(got, "You work in your worktree "+worktree+": ") || !strings.Contains(got, "\n\n[a.txt#") || !strings.HasSuffix(got, "1:worktree") {
		t.Fatalf("a worktree subagent reads its own copy, shown relative to its worktree, got %q", got)
	}
	tag := tagPatternText.FindStringSubmatch(got)[1]
	if edited := c.call(t, r.by(w1, editCall(fmt.Sprintf("[a.txt#%s]\nPUT 1:\n+WORKTREE", tag)))); edited.IsError {
		t.Fatalf("got %q", text(edited))
	}
	if created := c.call(t, r.by(w1, &mcp.CallToolParams{Name: "write", Arguments: map[string]any{"path": "new.txt", "content": "x\n"}})); created.IsError {
		t.Fatalf("got %q", text(created))
	}
	for path, want := range map[string]string{filepath.Join(worktree, "a.txt"): "WORKTREE\n", filepath.Join(directory, "a.txt"): "main\n", filepath.Join(worktree, "new.txt"): "x\n"} {
		if data, _ := os.ReadFile(path); string(data) != want {
			t.Errorf("%s holds %q, want %q", path, data, want)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "new.txt")); err == nil {
		t.Error("a worktree subagent's relative write must not land in nib's directory")
	}
	if got := text(c.call(t, readCall("a.txt"))); !strings.HasPrefix(got, "[a.txt#") || !strings.HasSuffix(got, "1:main") {
		t.Fatalf("the main agent reads nib's directory, got %q", got)
	}
}

func TestInstructionsSayHowToNameADirectoryWhenTheShellIsOffered(t *testing.T) {
	if got := Instructions(Options{}); strings.Contains(got, "hash -d") || strings.Contains(got, namedHeader) {
		t.Fatalf("without the shell or names, nothing is said of names, got %q", got)
	}
	if got := Instructions(Options{Shell: &fakeShell{}}); !strings.HasSuffix(got, "\n"+namedHint) || strings.Contains(got, namedHeader) {
		t.Fatalf("with the shell and no names, the hint closes the instructions, got %q", got)
	}
	table := &names.Table{Base: "/p", Entries: []names.Entry{{Name: "core", Directory: "/p/lib/core"}}}
	if got := Instructions(Options{Shell: &fakeShell{}, Names: table}); !strings.HasSuffix(got, "\n"+namedHeader+"\n~core = lib/core\n"+namedHint) || len(got) > instructionLimit {
		t.Fatalf("names come before the hint, got %q", got)
	}
}

// drops stands in for the hook's drop records: every call is known, with the latest drop set so far.
type drops struct {
	mu      sync.Mutex
	dropped time.Time
	known   bool
}

func (d *drops) take(string) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dropped, d.known
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d of the file\n", i)
	}
	return b.String()
}

func TestRowsAnAgentHoldsPrintAsMarkersUntilItsContextDrops(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, numbered(20))
	r := &records{byUse: map[string]hookstate.Agent{}}
	d := &drops{known: true}
	c := connect(t, Options{Directory: directory, Agent: r.take, Dropped: d.take}, []string{directory}, nil, false)
	c.call(t, r.as("", readCall(path+":1-10")))
	wider := text(c.call(t, r.as("", readCall(path+":1-12"))))
	tag := tagPatternText.FindStringSubmatch(wider)[1]
	if !strings.Contains(wider, "\n[lines 1-10 unchanged since you saw them]\n11:line 11 of the file\n12:line 12 of the file\n") || !strings.Contains(wider, "[a [lines A-B unchanged") {
		t.Fatalf("a wider read: %s", wider)
	}
	// The same call again at once asks for the rows, and so does full: true.
	if again := text(c.call(t, r.as("", readCall(path+":1-12")))); !strings.Contains(again, "\n1:line 1 of the file\n") || strings.Contains(again, "[lines") {
		t.Fatalf("an immediate repeat: %s", again)
	}
	c.call(t, r.as("", readCall(path+":20")))
	full := &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{path + ":1-12"}, "full": true}}
	if whole := text(c.call(t, r.as("", full))); !strings.Contains(whole, "\n1:line 1 of the file\n") {
		t.Fatalf("full: true: %s", whole)
	}
	// A line put above moves the rest down; the agent wrote the new row, and the rest it saw one line higher.
	edited := text(c.call(t, r.as("", editCall(fmt.Sprintf("[%s#%s]\nPUT <1:\n+first", path, tag)))))
	moved := text(c.call(t, r.as("", readCall(path+":1-14"))))
	if !strings.Contains(moved, "[lines 3-13 = your earlier 2-12, unchanged]\n14:line 13 of the file") || strings.Contains(moved, "[a [lines A-B") {
		t.Fatalf("after the edit %s: %s", edited, moved)
	}
	// A grep's match rows always print; the context it shares with what the agent has is a marker.
	grep := &mcp.CallToolParams{Name: "grep", Arguments: map[string]any{"pattern": "line 6 of", "paths": []string{path}, "before": 4, "after": 4}}
	if found := text(c.call(t, r.as("", grep))); !strings.Contains(found, "\n[lines 3-6 unchanged since you saw them]\n*7:line 6 of the file\n[lines 8-11 unchanged since you saw them]") {
		t.Fatalf("grep: %s", found)
	}
	// Once the agent's context is compacted, what it was shown before is gone: rows print again, and an edit needs them shown first.
	d.mu.Lock()
	d.dropped = time.Now()
	d.mu.Unlock()
	after := text(c.call(t, r.as("", readCall(path+":20-21"))))
	if seen := c.latestSeen("main", path); !slices.Equal(seen, []int{20, 21}) {
		t.Fatalf("after the drop the agent holds %v", seen)
	}
	refused := text(c.call(t, r.as("", editCall(fmt.Sprintf("[%s#%s]\nPUT 5:\n+FIVE", path, tagPatternText.FindStringSubmatch(after)[1])))))
	if !strings.Contains(refused, "never displayed") {
		t.Fatalf("an edit of a line seen only before the drop: %s", refused)
	}
	// Only the rows the refusal showed since are held again, and the markers are explained again.
	if again := text(c.call(t, r.as("", readCall(path+":1-12")))); !strings.Contains(again, "\n2:line 1 of the file\n[lines 3-7 unchanged since you saw them]\n8:line 7 of the file\n") || !strings.Contains(again, "[a [lines A-B") {
		t.Fatalf("after the drop: %s", again)
	}
	// Without the hook telling when contexts drop, nothing prints as a marker.
	plain := connect(t, Options{Directory: directory, Agent: r.take}, []string{directory}, nil, false)
	plain.call(t, r.as("", readCall(path+":1-5")))
	if again := text(plain.call(t, r.as("", readCall(path+":1-6")))); strings.Contains(again, "[lines") {
		t.Fatalf("untracked: %s", again)
	}
}

func TestOpenCodeSessionsHoldRowsUntilOpenCodeCompactsThem(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, numbered(8))
	var mu sync.Mutex
	compacted := map[string]time.Time{}
	c := connect(t, Options{Directory: directory, Compacted: func(session string) (time.Time, bool) {
		mu.Lock()
		defer mu.Unlock()
		at, ok := compacted[session]
		return at, ok || session == "ses_a"
	}}, []string{directory}, nil, false)
	read := func(session, selector string) string {
		return text(c.call(t, inSession(session, readCall(path+selector))))
	}
	read("ses_a", ":1-4")
	if got := read("ses_a", ":1-5"); !strings.Contains(got, "[lines 1-4 unchanged since you saw them]\n5:") {
		t.Fatalf("held: %s", got)
	}
	mu.Lock()
	compacted["ses_a"] = time.Now()
	mu.Unlock()
	if got := read("ses_a", ":1-6"); strings.Contains(got, "[lines") {
		t.Fatalf("after OpenCode compacted the session: %s", got)
	}
	// A session OpenCode's database cannot tell about never gets markers.
	read("ses_b", ":1-4")
	if got := read("ses_b", ":1-5"); strings.Contains(got, "[lines") {
		t.Fatalf("untracked session: %s", got)
	}
}

func TestAForkHoldsWhatItsParentWasShownSinceTheParentsLastDrop(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	path := filepath.Join(directory, "a.txt")
	file(t, path, numbered(8))
	r := &records{byUse: map[string]hookstate.Agent{}}
	d := &drops{known: true}
	c := connect(t, Options{Directory: directory, Agent: r.take, Dropped: d.take}, []string{directory}, nil, false)
	c.call(t, r.as("", readCall(path+":1-4")))
	spawned := time.Now()
	if got := text(c.call(t, r.by(hookstate.Agent{ID: "f1", Spawned: true, Forked: true, At: spawned}, readCall(path+":1-5")))); !strings.Contains(got, "[lines 1-4 unchanged since you saw them]") {
		t.Fatalf("a fork holds its parent's rows: %s", got)
	}
	// The hook files a fork's calls with its parent's compaction before the spawn: the fork's conversation starts from the summary.
	d.mu.Lock()
	d.dropped = spawned
	d.mu.Unlock()
	if got := text(c.call(t, r.by(hookstate.Agent{ID: "f2", Spawned: true, Forked: true, At: time.Now()}, readCall(path+":1-5")))); strings.Contains(got, "[lines") {
		t.Fatalf("a fork of a compacted parent: %s", got)
	}
}
