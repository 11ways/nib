package main

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/server"
)

// alone keeps nib shape from finding a session: the tests' own process tree may hold a real nib server's socket.
func alone(t *testing.T) {
	t.Helper()
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"NIB_DECIDER", "NIB_DECIDER_SUMMARY", "NIB_DECIDER_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	before := shapeSockets
	shapeSockets = func() []string { return nil }
	t.Cleanup(func() { shapeSockets = before })
}

// shaped runs nib shape with args and returns its status and what it printed.
func shaped(t *testing.T, args ...string) (int, string) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	status := runShape(args)
	os.Stdout = stdout
	write.Close()
	out, _ := io.ReadAll(read)
	return status, string(out)
}

// session answers nib shape's requests on the socket known by key as answer says, and records what it was asked.
func session(t *testing.T, key string, answer func(server.ShapeRequest) server.ShapeAnswer) *[]server.ShapeRequest {
	t.Helper()
	if err := os.MkdirAll(server.RuntimeDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", server.SocketPath(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var asked []server.ShapeRequest
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request server.ShapeRequest
			if json.NewDecoder(conn).Decode(&request) == nil {
				asked = append(asked, request)
				_ = json.NewEncoder(conn).Encode(answer(request))
			}
			conn.Close()
		}
	}()
	return &asked
}

func TestShapeAsksTheSessionsServerAndShapesTheLogItselfWithoutOne(t *testing.T) {
	alone(t)
	log := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(log, []byte("error: Foo\nBUILD FAILED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, out := shaped(t, "--log", log, "--purpose", "why"); status != 0 || out != "error: Foo\nBUILD FAILED\n" {
		t.Fatalf("without a session: %d %q", status, out)
	}
	asked := session(t, "claude-s1", func(server.ShapeRequest) server.ShapeAnswer { return server.ShapeAnswer{Text: "from the session"} })
	shapeSockets = func() []string { return []string{server.SocketPath("claude-none"), server.SocketPath("claude-s1")} }
	if status, out := shaped(t, "--log", log, "--purpose", "why", "--exit", "1", "--command", "zenit-dev test"); status != 0 || out != "from the session\n" {
		t.Fatalf("with a session: %d %q", status, out)
	}
	if want := (server.ShapeRequest{Log: log, Purpose: "why", Command: "zenit-dev test", Exit: 1}); len(*asked) != 1 || !reflect.DeepEqual((*asked)[0], want) {
		t.Fatalf("asked %+v", *asked)
	}
	// A refusal is the session's answer: nib shape does not shape the log behind its back.
	refused := session(t, "claude-s2", func(server.ShapeRequest) server.ShapeAnswer {
		return server.ShapeAnswer{Status: 2, Error: "outside the roots"}
	})
	shapeSockets = func() []string { return []string{server.SocketPath("claude-s2")} }
	if status, out := shaped(t, "--log", log, "--purpose", "why"); status != 2 || out != "" || len(*refused) != 1 {
		t.Fatalf("refused: %d %q", status, out)
	}
	// A socket nothing answers on is passed over.
	if err := os.WriteFile(server.SocketPath("claude-s3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	shapeSockets = func() []string { return []string{server.SocketPath("claude-s3")} }
	if status, out := shaped(t, "--log", log, "--purpose", "why"); status != 0 || !strings.Contains(out, "BUILD FAILED") {
		t.Fatalf("dead socket: %d %q", status, out)
	}
}

func TestShapeFlags(t *testing.T) {
	r, err := shapeFlags([]string{"--log", "a.log", "--purpose=run it", "--exit", "3", "--command", "zenit-dev test"})
	if err != nil || r != (shapeRequest{log: "a.log", purpose: "run it", command: "zenit-dev test", exit: 3}) {
		t.Fatalf("%+v %v", r, err)
	}
	for _, args := range [][]string{
		{"--purpose", "x"},
		{"--log", "a.log"},
		{"--log", "a.log", "--purpose", "x", "--exit", "one"},
		{"--log", "a.log", "--purpose", "x", "--log", "b.log"},
		{"--log", "a.log", "--purpose"},
		{"--log", "a.log", "--purpose", "x", "--verbose"},
	} {
		if _, err := shapeFlags(args); err == nil {
			t.Errorf("%q was taken", args)
		}
	}
}

func TestShapeRefusesBadArgumentsAndUnreadableLogsWithStatus2(t *testing.T) {
	alone(t)
	if status := runShape([]string{"--purpose", "x"}); status != 2 {
		t.Fatalf("no log: %d", status)
	}
	if status := runShape([]string{"--log", filepath.Join(t.TempDir(), "missing.log"), "--purpose", "x"}); status != 2 {
		t.Fatalf("missing log: %d", status)
	}
	if status := runShape([]string{"--log", t.TempDir(), "--purpose", "x"}); status != 2 {
		t.Fatalf("directory: %d", status)
	}
}
