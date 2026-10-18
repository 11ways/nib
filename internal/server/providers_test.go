package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/providers"
	"github.com/11ways/nib/internal/providers/providertest"
	"github.com/11ways/nib/internal/trash"
)

func TestMain(m *testing.M) {
	providertest.Serve()
	cleanup := trash.Isolated()
	status := m.Run()
	cleanup()
	os.Exit(status)
}

// hosting connects a client to a server hosting the test provider as fake, in a directory holding a.py.
func hosting(t *testing.T) (*client, string) { return hostingWith(t, nil) }

// hostingWith is hosting with a shell behind the server, which is what offers nibash_status.
func hostingWith(t *testing.T, shell Shell) (*client, string) {
	t.Helper()
	t.Setenv(providertest.Env, "1")
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(directory, "a.py"), []byte("def a():\n    return 1\n\n\ndef b():\n    return 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := providers.Start(context.Background(), providers.Spec{Name: "fake", Program: self}, directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return connect(t, Options{Directory: directory, Providers: []*providers.Provider{p}, Shell: shell}, nil, nil, false), directory
}

func TestProviderToolsAreOfferedAsNibsOwn(t *testing.T) {
	c, _ := hosting(t)
	listed, err := c.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		if strings.HasPrefix(tool.Name, "fake_") {
			names = append(names, tool.Name)
		}
	}
	if strings.Join(names, ",") != "fake_crash,fake_fails,fake_future,fake_locate,fake_pid,fake_plain,fake_slow" {
		t.Fatalf("tools %v", names)
	}
	if got := c.session.InitializeResult().Instructions; !strings.Contains(got, "Tools named fake_* are hosted by nib") || len(got) > instructionLimit {
		t.Fatalf("instructions: %s", got)
	}
}

func TestProviderLocationsPrintAsRowsAnEditAnchorsOn(t *testing.T) {
	c, directory := hosting(t)
	parts, _ := json.Marshal([]any{
		map[string]any{"text": "1. a.py:5-6"},
		map[string]any{"path": filepath.Join(directory, "a.py"), "start": 5, "end": 6, "indexed": "def b():\n    return 2"},
	})
	got := text(c.call(t, &mcp.CallToolParams{Name: "fake_locate", Arguments: json.RawMessage(`{"parts":` + string(parts) + `}`)}))
	tagValue := got[strings.Index(got, "#")+1 : strings.Index(got, "]\n")]
	if want := fmt.Sprintf("1. a.py:5-6\n\n[a.py#%s]\n5:def b():\n6:    return 2\n[6 lines total]", tagValue); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if seen := c.seen(); !strings.Contains(seen, "seen [5 6]") {
		t.Fatalf("the shown lines must count as seen: %s", seen)
	}
	edited := c.call(t, &mcp.CallToolParams{Name: "edit", Arguments: map[string]any{"input": fmt.Sprintf("[a.py#%s]\nPUT 6:\n+    return 3", tagValue)}})
	if edited.IsError {
		t.Fatalf("edit from a provider's rows: %s", text(edited))
	}
	if data, _ := os.ReadFile(filepath.Join(directory, "a.py")); !strings.HasSuffix(string(data), "    return 3\n") {
		t.Fatalf("file: %q", data)
	}
}

func TestProviderAnswersNibCannotReadShowAsText(t *testing.T) {
	c, _ := hosting(t)
	if got := text(c.call(t, &mcp.CallToolParams{Name: "fake_plain"})); got != "[fake's own text follows, not anchors]\nonly text\n12:looks like a row" {
		t.Fatalf("plain: %q", got)
	}
	if got := text(c.call(t, &mcp.CallToolParams{Name: "fake_future"})); got != "future text" {
		t.Fatalf("future: %q", got)
	}
	failed := c.call(t, &mcp.CallToolParams{Name: "fake_fails"})
	if !failed.IsError || text(failed) != "it failed" {
		t.Fatalf("fails: %v %q", failed.IsError, text(failed))
	}
}

func TestStatusTellsOfAHostedCallWhileItRunsAndOfNoOtherTool(t *testing.T) {
	c, directory := hostingWith(t, &fakeShell{})
	if err := os.WriteFile(filepath.Join(directory, "b.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := func() []any {
		result := c.call(t, &mcp.CallToolParams{Name: "nibash_status"})
		listed, _ := result.StructuredContent.(map[string]any)["calls"].([]any)
		return listed
	}
	if got := calls(); got != nil {
		t.Fatalf("nothing runs yet: %v", got)
	}
	done := make(chan string, 1)
	go func() {
		done <- text(c.call(t, &mcp.CallToolParams{Name: "fake_slow", Arguments: map[string]any{"ms": 1500}}))
	}()
	var running []any
	deadline := time.Now().Add(5 * time.Second)
	for running == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		running = calls()
	}
	if len(running) != 1 {
		t.Fatalf("the hosted call is told of while it runs: %v", running)
	}
	call := running[0].(map[string]any)
	if call["tool"] != "fake_slow" || call["agent"] != "main" || call["hosted"] != true || call["started"].(float64) == 0 {
		t.Fatalf("call %v", call)
	}
	// nib's own tools are not told of, and neither is the status call itself.
	text(c.call(t, readCall(filepath.Join(directory, "b.txt"))))
	if got := calls(); len(got) != 1 {
		t.Fatalf("only the hosted call: %v", got)
	}
	if got := <-done; got != "slept" {
		t.Fatalf("slow: %q", got)
	}
	if got := calls(); got != nil {
		t.Fatalf("an ended call is gone: %v", got)
	}
}
