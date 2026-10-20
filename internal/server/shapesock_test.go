package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/names"
)

// serving is a server answering nib shape requests on the socket known by key, under a private runtime folder of the test's.
func serving(t *testing.T, s *Server, key string) context.CancelFunc {
	t.Helper()
	listener, err := ListenShape(key)
	if err != nil || listener == nil {
		t.Fatalf("listen: %v %v", listener, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.ServeShape(ctx, listener)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return func() {
		cancel()
		<-done
	}
}

// sockets are the socket paths of keys, in order.
func sockets(keys ...string) []string {
	paths := make([]string, len(keys))
	for i, key := range keys {
		paths[i] = SocketPath(key)
	}
	return paths
}

func TestNibShapeAsksTheServerOfTheFirstSocketThatAnswers(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	directory := t.TempDir()
	log := filepath.Join(directory, "build.log")
	file(t, log, "error: Foo\n")
	s := New(Options{Directory: directory, Shell: &fakeShell{}})
	stop := serving(t, s, "claude-s1")
	info, err := os.Stat(SocketPath("claude-s1"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", info, err)
	}
	if again, err := ListenShape("claude-s1"); again != nil || err != nil {
		t.Fatalf("a second server of the same session took the socket: %v %v", again, err)
	}
	answer, ok := AskShape(context.Background(), sockets("claude-none", "claude-s1", "nib-1"), ShapeRequest{Log: log, Purpose: "why", Command: "zenit-dev test", Exit: 1})
	if !ok || answer.Status != 0 || answer.Text != `shaped `+log+` for "why", "zenit-dev test" exit 1` {
		t.Fatalf("answer %+v %v", answer, ok)
	}
	tokens := s.shapes.tokens()
	if tokens.Calls != 1 || len(tokens.Agents) != 1 || tokens.Agents[0].Agent != shapeAgent || tokens.Agents[0].Tools[0].Tool != "shape" || tokens.Chars != len(answer.Text) {
		t.Fatalf("tokens %+v", tokens)
	}
	// A log the server cannot read is refused, not shaped by the caller.
	if answer, ok := AskShape(context.Background(), sockets("claude-s1"), ShapeRequest{Log: filepath.Join(directory, "missing.log")}); !ok || answer.Status != 2 || answer.Error == "" {
		t.Fatalf("missing log %+v %v", answer, ok)
	}
	if answer, ok := AskShape(context.Background(), sockets("claude-s1"), ShapeRequest{Log: "build.log"}); !ok || answer.Status != 2 || !strings.Contains(answer.Error, "absolute") {
		t.Fatalf("relative log %+v %v", answer, ok)
	}
	stop()
	if _, err := os.Lstat(SocketPath("claude-s1")); !os.IsNotExist(err) {
		t.Fatalf("the socket outlived its server: %v", err)
	}
	if _, ok := AskShape(context.Background(), sockets("claude-s1"), ShapeRequest{Log: log}); ok {
		t.Fatal("a server that ended still answered")
	}
}

// The session's socket comes first, as every program of a Claude Code session sees its id; the one the server's own programs are told
// comes next, the only one under another client.
func TestShapeSocketsAreTheSessionsThenTheOneNibNamed(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	named := SocketPath("nib-7")
	if got := ShapeSockets(env(map[string]string{"CLAUDE_CODE_SESSION_ID": "abc-1", ShapeSocketEnv: named})); len(got) != 2 || got[0] != SocketPath("claude-abc-1") || got[1] != named {
		t.Fatalf("both: %v", got)
	}
	if got := ShapeSockets(env(map[string]string{ShapeSocketEnv: named})); len(got) != 1 || got[0] != named {
		t.Fatalf("named only: %v", got)
	}
	// A session id that could name a path, or a relative socket, is never asked.
	if got := ShapeSockets(env(map[string]string{"CLAUDE_CODE_SESSION_ID": "../x", ShapeSocketEnv: "rel.sock"})); len(got) != 0 {
		t.Fatalf("unsafe: %v", got)
	}
	if key := ShapeKey(env(map[string]string{"CLAUDE_CODE_SESSION_ID": "abc-1"})); key != "claude-abc-1" {
		t.Fatalf("session key %q", key)
	}
	if key := ShapeKey(env(nil)); key != fmt.Sprintf("nib-%d", os.Getpid()) {
		t.Fatalf("own key %q", key)
	}
	// Claude Code's process outlives a /clear, which gives hooks and commands a new session id but leaves its servers the old one.
	got := ShapeSockets(env(map[string]string{"CLAUDE_CODE_SESSION_ID": "abc-2", "CLAUDE_PID": "64603", ShapeSocketEnv: named}))
	if len(got) != 3 || got[0] != SocketPath("claude-abc-2") || got[1] != SocketPath("claude-pid-64603") || got[2] != named {
		t.Fatalf("with Claude Code's process: %v", got)
	}
	if got := ShapeSockets(env(map[string]string{"CLAUDE_PID": "../1"})); len(got) != 0 {
		t.Fatalf("a process id that could name a path: %v", got)
	}
	if key := ParentKey(env(map[string]string{"CLAUDE_CODE_SESSION_ID": "abc-1"})); key != fmt.Sprintf("claude-pid-%d", os.Getppid()) {
		t.Fatalf("parent key %q", key)
	}
	if key := ParentKey(env(nil)); key != "" {
		t.Fatalf("outside Claude Code the parent key is %q", key)
	}
}

// nib's hook asks which named directories of a message the main agent must be told, with the moment its context was last dropped.
func TestPromptNamesAfterADrop(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := filepath.Join(directory, "src", "server", "widgets")
	if err := os.MkdirAll(widgets, 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Directory: directory, Names: &names.Table{Base: directory}})
	if err := s.options.Names.For(s.defined, s.engineSession("")).Define("w", widgets); err != nil {
		t.Fatal(err)
	}
	serving(t, s, "claude-pid-1")
	ask := func(dropped time.Time) string {
		t.Helper()
		request := ShapeRequest{Mentioned: []string{"w", "nothing"}}
		if !dropped.IsZero() {
			request.Dropped = dropped.UnixNano()
		}
		answer, ok := AskShape(context.Background(), sockets("claude-pid-1"), request)
		if !ok || answer.Status != 0 {
			t.Fatalf("answer %+v %v", answer, ok)
		}
		return answer.Text
	}
	if text := ask(time.Time{}); text != "" {
		t.Fatalf("a name the agent holds was told: %q", text)
	}
	cleared := time.Now().Add(time.Minute)
	if text := ask(cleared); text != "~w = src/server/widgets" {
		t.Fatalf("after a clear: %q", text)
	}
	if text := ask(cleared); text != "" {
		t.Fatalf("told twice: %q", text)
	}
}

func TestASocketADeadServerLeftIsReplacedAndNeverAnswers(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// The first listener makes the private folder.
	first, err := ListenShape("k1")
	if err != nil || first == nil {
		t.Fatal(err)
	}
	first.Close()
	path := SocketPath("k7")
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	dead.Close()
	if _, ok := AskShape(context.Background(), sockets("k7"), ShapeRequest{Log: "/x.log"}); ok {
		t.Fatal("a stale socket answered")
	}
	directory := t.TempDir()
	log := filepath.Join(directory, "a.log")
	file(t, log, "x\n")
	serving(t, New(Options{Directory: directory, Shell: &fakeShell{}}), "k7")
	if answer, ok := AskShape(context.Background(), sockets("k7"), ShapeRequest{Log: log}); !ok || answer.Status != 0 {
		t.Fatalf("after replacing the stale socket: %+v %v", answer, ok)
	}
}

func TestASocketInAFolderOthersMayEnterIsNeitherOpenedNorAsked(t *testing.T) {
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if err := os.MkdirAll(filepath.Join(runtime, "nib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(runtime, "nib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if listener, err := ListenShape("k9"); listener != nil || err != nil {
		t.Fatalf("listened in a shared folder: %v %v", listener, err)
	}
	if _, ok := AskShape(context.Background(), sockets("k9"), ShapeRequest{Log: "/x.log"}); ok {
		t.Fatal("asked through a shared folder")
	}
}

// Under the sandbox a request is judged by the roots the client's calls listed; before any, nothing is allowed.
func TestShapeRequestsStayInsideTheSandboxRoots(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	directory := resolvedPath(t, t.TempDir())
	inside := filepath.Join(directory, "build.log")
	file(t, inside, "error: Foo\n")
	outside := filepath.Join(resolvedPath(t, t.TempDir()), "other.log")
	file(t, outside, "secret\n")
	c := connect(t, Options{Directory: directory, Sandbox: true, Shell: &fakeShell{}}, []string{directory}, nil, false)
	serving(t, c.server, "k31")
	if answer, ok := AskShape(context.Background(), sockets("k31"), ShapeRequest{Log: inside}); !ok || answer.Status != 2 || !strings.Contains(answer.Error, "not known yet") {
		t.Fatalf("before the roots were listed: %+v %v", answer, ok)
	}
	c.call(t, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"paths": []string{inside}}})
	if answer, ok := AskShape(context.Background(), sockets("k31"), ShapeRequest{Log: inside}); !ok || answer.Status != 0 {
		t.Fatalf("inside the roots: %+v %v", answer, ok)
	}
	if answer, ok := AskShape(context.Background(), sockets("k31"), ShapeRequest{Log: outside}); !ok || answer.Status != 2 || strings.Contains(answer.Text, "secret") {
		t.Fatalf("outside the roots: %+v %v", answer, ok)
	}
}

func TestTheTokenReportAddsWhatNibShapeReturned(t *testing.T) {
	var calls, shapes usageLog
	calls.begin("", "read", false, nil)(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "abc"}}})
	shapes.begin(shapeAgent, "shape", false, []byte(`{"log":"/b.log"}`))(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "abcdef"}}})
	view := calls.tokens().with(shapes.tokens())
	if view.Chars != 9 || view.Calls != 2 || len(view.Agents) != 2 || view.Agents[1].Agent != shapeAgent || view.Largest[0].Tool != "shape" {
		t.Fatalf("view %+v", view)
	}
}

// A request that is not one is refused with a reason, never shaped.
func TestAMalformedRequestIsRefused(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	serving(t, New(Options{Directory: t.TempDir(), Shell: &fakeShell{}}), "k55")
	conn, err := net.Dial("unix", SocketPath("k55"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	var answer ShapeAnswer
	if err := json.NewDecoder(conn).Decode(&answer); err != nil || answer.Status != 2 || answer.Error == "" {
		t.Fatalf("answer %+v %v", answer, err)
	}
}
