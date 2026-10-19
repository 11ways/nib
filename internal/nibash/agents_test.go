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
	run := connectRaw(t, directory, table, agent)
	return func(toolUse, command string) string {
		t.Helper()
		return strings.TrimSpace(withoutGreeting(run(toolUse, command)))
	}
}

// connectRaw is connectNamed with each call's whole output, the note a subagent's first call starts with included.
func connectRaw(t *testing.T, directory string, table *names.Table, agent func(toolUse string) hookstate.Agent) func(toolUse, command string) string {
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
		return result.Content[0].(*mcp.TextContent).Text
	}
}

// greetingEnd ends the note a subagent's first call starts with when nibash is offered.
const greetingEnd = "for you and the subagents you start.\n\n"

// withoutGreeting is a call's output without the note a subagent's first call starts with, which TestAWorktreeSubagentWorksInItsWorktree pins.
func withoutGreeting(text string) string {
	if _, after, ok := strings.Cut(text, greetingEnd); ok {
		return after
	}
	return text
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
	// The function's name is one no program on PATH has: there one would run in its place.
	expect(t, run("toolu_main_x", "cd sub && export WHO=main && nib_test_greet() { echo hi; }"), "(no output)")
	expect(t, run("toolu_main_y", `echo "$PWD ${WHO-unset} $(nib_test_greet 2>/dev/null || echo none)"`), directory+" unset none")
	run("toolu_sub_job", "sleep 30.41 &")
	expect(t, run("toolu_sub_jobs", "jobs | wc -l"), "1")
	expect(t, run("toolu_main_jobs", "jobs | wc -l"), "0")
	// A fork starts with its parent's seen lines, never with its jobs.
	expect(t, run("toolu_f1", `echo "$PWD"; jobs | wc -l`), directory+"\n0")
}

func TestAWorktreeSubagentWorksInItsWorktree(t *testing.T) {
	directory := agentDirectory(t)
	worktree := filepath.Join(directory, ".claude", "worktrees", "agent-w1")
	for path, content := range map[string]string{
		"a.txt": "main\n", "lib/core/b.txt": "main core\n", "only-main/c.txt": "c\n",
		".claude/worktrees/agent-w1/a.txt": "worktree\n", ".claude/worktrees/agent-w1/lib/core/b.txt": "worktree core\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// nib's own names point into the project; in the worktree they point at its copy, and one it lacks is left out.
	table := &names.Table{Base: directory, Entries: []names.Entry{{Name: "core", Directory: filepath.Join(directory, "lib", "core")}, {Name: "only", Directory: filepath.Join(directory, "only-main")}}}
	records := map[string]hookstate.Agent{
		"toolu_w1": {ID: "w1", Spawned: true, Worktree: worktree}, "toolu_w1b": {ID: "w1", Spawned: true, Worktree: worktree},
		"toolu_s1": {ID: "s1", Spawned: true},
	}
	run := connectRaw(t, directory, table, func(toolUse string) hookstate.Agent { return records[toolUse] })
	hint := "Name a directory you will use again with hash -d name=dir in nibash: ~name/ then works in every tool, for you and the subagents you start."
	header := "Named directories: ~name/ paths work in every tool and, unquoted, in nibash; nib prints paths that way. They mean nothing elsewhere."
	got := run("toolu_w1", "pwd; cat a.txt; head -1 ~core/b.txt")
	greeting := "You work in your worktree " + worktree + ": relative paths in every nib tool mean it, and nibash calls start there.\n" + header + "\n~core = lib/core\n" + hint + "\n\n"
	if !strings.HasPrefix(got, greeting) {
		t.Fatalf("a worktree subagent's first call must say where it works and which names it has, got:\n%s", got)
	}
	body := strings.TrimPrefix(got, greeting)
	for _, want := range []string{worktree + "\n", "1:worktree\n", "[~core/b.txt#", "1:worktree core"} {
		if !strings.Contains(body, want) {
			t.Fatalf("a worktree subagent must work in its worktree, missing %q in:\n%s", want, body)
		}
	}
	if got := run("toolu_w1b", "cat a.txt"); strings.Contains(got, "You work in") || !strings.Contains(got, "[a.txt#") || !strings.Contains(got, "1:worktree") {
		t.Fatalf("a later call works in the worktree without the note, got:\n%s", got)
	}
	// Another subagent and the main agent work in nib's directory; only the subagent is told its names, as the main agent has them in the
	// server instructions.
	if got := run("toolu_s1", "cat a.txt"); !strings.HasPrefix(got, header+"\n~only = only-main\n~core = lib/core\n"+hint+"\n\n") || !strings.Contains(got, "1:main") {
		t.Fatalf("a subagent outside a worktree works in nib's directory, got:\n%s", got)
	}
	if got := run("toolu_main", "cat a.txt"); strings.Contains(got, "Named directories") || !strings.Contains(got, "1:main") {
		t.Fatalf("the main agent works in nib's directory and is not told again, got:\n%s", got)
	}
}
