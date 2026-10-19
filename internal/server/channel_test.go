package server

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/hookstate"
)

func TestArgvNamesChannelReadsTheOptionAsClaudeCodeDoes(t *testing.T) {
	for _, c := range []struct {
		argv         []string
		named, known bool
	}{
		{[]string{"claude", "--dangerously-load-development-channels", "server:nib"}, true, true},
		{[]string{"claude", "--channels", "server:tg", "server:nib", "--model", "haiku"}, true, true},
		{[]string{"claude", "--channels=plugin:nib@market"}, true, true},
		{[]string{"claude", "--channels", "plugin:nib@m"}, true, true},
		{[]string{"claude", "--dangerously-load-development-channels", "server:sketerm", "server:nib"}, true, true},
		// Another server's channel, a bare name, or an entry after the list ended: a session with channels, not this server's.
		{[]string{"claude", "--channels", "server:telegram"}, false, true},
		{[]string{"claude", "--channels=server:telegram"}, false, true},
		{[]string{"claude", "--channels", "nib"}, false, true},
		{[]string{"claude", "--channels", "server:tg", "--model", "server:nib"}, false, true},
		{[]string{"claude", "--channels", "server:nib2"}, false, true},
		{[]string{"claude", "--channels", "plugin:nib"}, false, true},
		// No channel option: a wrapper, so the search goes on.
		{[]string{"claude", "--model", "server:nib"}, false, false},
		{[]string{"/bin/sh", "-c", "exec nib"}, false, false},
	} {
		if named, known := ArgvNamesChannel(c.argv, "nib"); named != c.named || known != c.known {
			t.Errorf("%q: got %v %v, want %v %v", c.argv, named, known, c.named, c.known)
		}
	}
}

func TestChannelNamedLooksAsFarAsTheNearestChannelOption(t *testing.T) {
	// 40 is a wrapper shell, 30 the Claude Code session, 20 an outer Claude Code that named the channel.
	argvs := map[int][]string{
		40: {"/bin/sh", "-c", "nib"},
		30: {"claude", "--model", "haiku"},
		20: {"claude", "--channels", "server:nib"},
	}
	parents := map[int]int{40: 30, 30: 20, 20: 1}
	argv := func(pid int) ([]string, bool) { a, ok := argvs[pid]; return a, ok }
	parent := func(pid int) (int, bool) { p, ok := parents[pid]; return p, ok }
	if !channelNamed(40, "nib", argv, parent) {
		t.Error("a channel two processes up, past a wrapper, is this session's")
	}
	argvs[30] = []string{"claude", "--channels", "server:other"}
	if channelNamed(40, "nib", argv, parent) {
		t.Error("the nearest channel option names another server, so an outer one does not count")
	}
	if channelNamed(40, "nib", func(int) ([]string, bool) { return nil, false }, parent) {
		t.Error("unreadable processes name no channel")
	}
}

// channelWatch is a client transport that keeps the channel messages the server sends, as Claude Code takes them, and hands on the rest.
type channelWatch struct {
	inner mcp.Transport
	mu    sync.Mutex
	got   []string
}

func (w *channelWatch) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := w.inner.Connect(ctx)
	return &watchedConn{conn, w}, err
}

func (w *channelWatch) messages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.got...)
}

type watchedConn struct {
	mcp.Connection
	watch *channelWatch
}

func (c *watchedConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		message, err := c.Connection.Read(ctx)
		if request, ok := message.(*jsonrpc.Request); err == nil && ok && request.Method == channelMethod && !request.ID.IsValid() {
			c.watch.mu.Lock()
			c.watch.got = append(c.watch.got, string(request.Params))
			c.watch.mu.Unlock()
			continue
		}
		return message, err
	}
}

// connectAs connects a client named name to a server with options, its transport wrapped as nib's main wraps it; answer, when set, answers
// the server's elicitation.
func connectAs(t *testing.T, options Options, name string, answer func(message string) *mcp.ElicitResult) (*Server, *mcp.ClientSession, *channelWatch) {
	t.Helper()
	ctx := context.Background()
	s := New(options)
	clientOptions := &mcp.ClientOptions{}
	if answer != nil {
		clientOptions.ElicitationHandler = func(_ context.Context, request *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return answer(request.Params.Message), nil
		}
	}
	sdk := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, clientOptions)
	sdk.AddRoots(&mcp.Root{URI: (&url.URL{Scheme: "file", Path: options.Directory}).String()})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, s.Transport(serverTransport), nil); err != nil {
		t.Fatal(err)
	}
	watch := &channelWatch{inner: clientTransport}
	session, err := sdk.Connect(ctx, watch, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return s, session, watch
}

func TestOnlyClaudeCodesMainAgentGetsAWayToPush(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	r := &records{byUse: map[string]hookstate.Agent{}}
	command := &mcp.CallToolParams{Name: "nibash", Arguments: map[string]any{"command": "pwd"}}
	pushes := func(options Options, client, agent string) (func(string, map[string]string) error, *mcp.ClientSession, *channelWatch) {
		t.Helper()
		shell := &fakeShell{}
		options.Directory, options.Shell, options.Agent = directory, shell, r.take
		_, session, watch := connectAs(t, options, client, nil)
		if _, err := session.CallTool(context.Background(), r.as(agent, command)); err != nil {
			t.Fatal(err)
		}
		return shell.calls[0].Push, session, watch
	}
	known := (&drops{known: true, session: "s1"}).take
	push, session, watch := pushes(Options{Channel: true, Dropped: known}, channelClient, "")
	if push == nil {
		t.Fatal("Claude Code's main agent, the hook having filed its call, gets a way to push")
	}
	if _, ok := session.InitializeResult().Capabilities.Experimental[channelCapability]; !ok {
		t.Errorf("the server declares %s: %+v", channelCapability, session.InitializeResult().Capabilities)
	}
	// Claude Code registers a channel only on a revision where a server may send a notification unasked.
	if version := session.InitializeResult().ProtocolVersion; version >= channelRevision {
		t.Errorf("a server with a channel negotiated %s", version)
	}
	if err := push("[job %1 finished, exit 0]\nhi", map[string]string{"job": "1", "exit": "0"}); err != nil {
		t.Fatal(err)
	}
	// The in-memory pipe hands the message over before Write returns; a ping makes sure the client read it.
	if err := session.Ping(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	}
	if messages := watch.messages(); len(messages) != 1 || json.Unmarshal([]byte(messages[0]), &got) != nil ||
		got.Content != "[job %1 finished, exit 0]\nhi" || got.Meta["job"] != "1" || got.Meta["exit"] != "0" {
		t.Fatalf("channel messages %v", watch.messages())
	}
	for _, c := range []struct {
		why     string
		options Options
		client  string
		agent   string
	}{
		{"a subagent, whose pushes would land in the main conversation", Options{Channel: true, Dropped: known}, channelClient, "a1"},
		{"another client", Options{Channel: true, Dropped: known}, "opencode", ""},
		{"no channel option naming nib", Options{Dropped: known}, channelClient, ""},
		{"no hook to tell the main agent from a subagent", Options{Channel: true}, channelClient, ""},
	} {
		push, session, _ := pushes(c.options, c.client, c.agent)
		if push != nil {
			t.Errorf("%s gets a way to push", c.why)
		}
		if _, ok := session.InitializeResult().Capabilities.Experimental[channelCapability]; ok != c.options.Channel {
			t.Errorf("%s: capability declared %v", c.why, ok)
		}
		if version := session.InitializeResult().ProtocolVersion; (version < channelRevision) != c.options.Channel {
			t.Errorf("%s: negotiated %s", c.why, version)
		}
	}
}

// A server with a channel speaks an earlier revision, where the SDK asks the client as it did before multi-round trips: an edit outside the
// roots is still confirmed.
func TestAChannelSessionStillConfirmsThroughElicitation(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	outside := resolvedPath(t, t.TempDir())
	var asked []string
	_, session, _ := connectAs(t, Options{Directory: directory, ConfirmOutsideRoots: true, Channel: true}, channelClient, func(message string) *mcp.ElicitResult {
		asked = append(asked, message)
		return accept("this file once")(message)
	})
	path := filepath.Join(outside, "a.txt")
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": "*** Add File: " + path + "\n+first\n"}})
	if err != nil || result.IsError {
		t.Fatalf("%v %v", err, result)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first\n" || len(asked) != 1 {
		t.Fatalf("got %q %v, asked %q", got, err, asked)
	}
}
