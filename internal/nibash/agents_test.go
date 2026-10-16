package nibash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/server"
)

// connectAgents is a client of a server with a real shell whose calls are made by the agents records names by tool_use id.
func connectAgents(t *testing.T, directory string, records map[string]hookstate.Agent) func(toolUse, command string) string {
	t.Helper()
	return connectNamed(t, directory, nil, func(toolUse string) hookstate.Agent { return records[toolUse] })
}

// connectNamed is connectAgents with the server's named directories in table, and agents looked up as they are called.
func connectNamed(t *testing.T, directory string, table *names.Table, agent func(toolUse string) hookstate.Agent) func(toolUse, command string) string {
	t.Helper()
	e := engine.New()
	s := server.New(server.Options{Directory: directory, Engine: e, Shell: New(e, false), Names: table, Agent: agent})
	t.Cleanup(s.Close)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return func(toolUse, command string) string {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"claudecode/toolUseId": toolUse}, Name: "nibash", Arguments: map[string]any{"command": command}})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", command, err, result)
		}
		return strings.TrimSpace(result.Content[0].(*mcp.TextContent).Text)
	}
}

func agentDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestEveryCallStartsFreshAndJobsStayWithTheirAgent(t *testing.T) {
	directory := agentDirectory(t)
	fork := hookstate.Agent{ID: "f1", Spawned: true, Forked: true}
	run := connectAgents(t, directory, map[string]hookstate.Agent{"toolu_sub_job": {ID: "a1"}, "toolu_sub_jobs": {ID: "a1"}, "toolu_f1": fork})
	expect(t, run("toolu_main_x", "cd sub && export WHO=main && greet() { echo hi; }"), "(no output)")
	expect(t, run("toolu_main_y", `echo "$PWD ${WHO-unset} $(greet 2>/dev/null || echo none)"`), directory+" unset none")
	run("toolu_sub_job", "sleep 30.41 &")
	expect(t, run("toolu_sub_jobs", "jobs | wc -l"), "1")
	expect(t, run("toolu_main_jobs", "jobs | wc -l"), "0")
	// A fork starts with its parent's seen lines, never with its jobs.
	expect(t, run("toolu_f1", `echo "$PWD"; jobs | wc -l`), directory+"\n0")
}
