package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/hookstate"
)

func call(directory, tool string, input map[string]any) string {
	reason := Decide(Input{Cwd: directory, ToolName: tool, ToolInput: input})
	if reason == "" {
		return ""
	}
	return strings.Split(reason, "\n")[1]
}

func TestReadBecomesNibReadAndOtherToolsPass(t *testing.T) {
	directory := t.TempDir()
	readme := filepath.Join(directory, "README.md")
	cases := [][2]string{
		{call(directory, "Read", map[string]any{"file_path": readme, "offset": 10.0, "limit": 20.0}), `mcp__nib__read {"paths":["README.md:10+20"]}`},
		{call(directory, "Read", map[string]any{"file_path": readme, "limit": 5.0}), `mcp__nib__read {"paths":["README.md:1-5"]}`},
		{call(directory, "Read", map[string]any{"file_path": readme}), `mcp__nib__read {"paths":["README.md"]}`},
		{call(directory, "Read", map[string]any{"file_path": filepath.Join(directory, "app.log"), "offset": 100.0, "limit": 50.0}), `mcp__nib__read {"paths":["app.log:100+50"],"plain":true}`},
		{call(directory, "Read", map[string]any{"file_path": filepath.Join(directory, "logs/run.txt")}), `mcp__nib__read {"paths":["logs/run.txt"],"plain":true}`},
		// A Go package named log holds code, which keeps its numbers.
		{call(directory, "Read", map[string]any{"file_path": filepath.Join(directory, "src/log/logger.go")}), `mcp__nib__read {"paths":["src/log/logger.go"]}`},
		{call(directory, "Read", map[string]any{"file_path": "/etc/hosts"}), `mcp__nib__read {"paths":["/etc/hosts"]}`},
		{call(directory, "Read", map[string]any{"file_path": filepath.Join(directory, "a.pdf")}), ""},
		{call(directory, "Read", map[string]any{"file_path": filepath.Join(directory, "a.ipynb")}), ""},
		// Older setups registered the hook for these too; with nibash and deny rules they are not the hook's to refuse.
		{call(directory, "Bash", map[string]any{"command": "cat README.md"}), ""},
		{call(directory, "Grep", map[string]any{"pattern": "x"}), ""},
		{call(directory, "Edit", map[string]any{"file_path": readme}), ""},
	}
	for _, c := range cases {
		if c[0] != c[1] {
			t.Errorf("got %q, want %q", c[0], c[1])
		}
	}
	reason := Decide(Input{Cwd: directory, ToolName: "Read", ToolInput: map[string]any{"file_path": readme}})
	if !strings.HasSuffix(reason, "(If you need Read itself here, or nib's tools are missing from this session, send the same call again and it runs.)") {
		t.Fatalf("reason %q", reason)
	}
}

func TestAResendRunsAndStateMustBePrivate(t *testing.T) {
	directory := t.TempDir()
	state := filepath.Join(directory, ".state")
	input := func(session string, args map[string]any) []byte {
		data, _ := json.Marshal(Input{SessionID: session, Cwd: directory, ToolName: "Read", ToolInput: args})
		return data
	}
	first := Run(input("s1", map[string]any{"file_path": "a.ts", "limit": 5}), state)
	var decision struct {
		HookSpecificOutput map[string]string `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(first), &decision); err != nil || decision.HookSpecificOutput["permissionDecision"] != "deny" || decision.HookSpecificOutput["hookEventName"] != "PreToolUse" {
		t.Fatalf("first %q", first)
	}
	// The same call again runs, whatever order its arguments come in; another session is refused once on its own.
	if got := Run(input("s1", map[string]any{"limit": 5, "file_path": "a.ts"}), state); got != "" {
		t.Fatalf("resend %q", got)
	}
	if got := Run(input("s2", map[string]any{"file_path": "a.ts", "limit": 5}), state); !strings.Contains(got, `"deny"`) {
		t.Fatalf("other session %q", got)
	}
	if Run([]byte("not json"), state) != "" {
		t.Fatal("bad input must pass")
	}
	open := filepath.Join(directory, ".open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Run(input("s1", map[string]any{"file_path": "a.ts"}), open); got != "" {
		t.Fatalf("state others can write must turn refusals off, got %q", got)
	}
	private := filepath.Join(directory, ".private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "elsewhere"), filepath.Join(private, "s3.txt")); err != nil {
		t.Fatal(err)
	}
	if got := Run(input("s3", map[string]any{"file_path": "a.ts"}), private); got != "" {
		t.Fatalf("a linked state file must not be written through, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(directory, "elsewhere")); err == nil {
		t.Fatal("wrote through a link")
	}
}

func TestNibToolCallsAreFiledForSubagentsAndNeverRefused(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	input := func(tool, toolUse, agent string) []byte {
		data, _ := json.Marshal(map[string]any{
			"session_id": "6d0b", "hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": map[string]any{"file_path": "a.ts"},
			"tool_use_id": toolUse, "agent_id": agent, "agent_type": "general-purpose",
		})
		return data
	}
	if got := Run(input("mcp__nib__read", "toolu_sub", "a1d576263ade979e4"), state); got != "" {
		t.Fatalf("a subagent's nib call must pass untouched, got %q", got)
	}
	main, _ := json.Marshal(map[string]any{"session_id": "6d0b", "hook_event_name": "PreToolUse", "tool_name": "mcp__nib__read", "tool_input": map[string]any{}, "tool_use_id": "toolu_main"})
	if got := Run(main, state); got != "" {
		t.Fatalf("the main agent's nib call must pass untouched, got %q", got)
	}
	agents := hookstate.OpenAgents(state)
	if got := agents.Take("toolu_main"); got != (hookstate.Agent{}) {
		t.Fatalf("the main agent's call needs no record, got %+v", got)
	}
	if got := agents.Take("toolu_sub"); got != (hookstate.Agent{ID: "a1d576263ade979e4"}) {
		t.Fatalf("the subagent's call must be filed under its tool_use id, got %+v", got)
	}
	// A subagent's Read is still Read: refused once, and filed for nobody.
	if got := Run(input("Read", "toolu_read", "a1d576263ade979e4"), state); !strings.Contains(got, `"deny"`) {
		t.Fatalf("a subagent's Read must be refused like the main agent's, got %q", got)
	}
	if got := agents.Take("toolu_read"); got != (hookstate.Agent{}) {
		t.Fatalf("only MCP calls are filed, got %+v", got)
	}
	// A state folder others can enter files nothing, and the call still passes.
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Run(input("mcp__nib__edit", "toolu_open", "a1"), open); got != "" {
		t.Fatalf("a call the hook cannot file must still pass, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(open, "agents")); err == nil {
		t.Fatal("the hook wrote into a folder others can enter")
	}
}

func TestASubagentIsFiledWithItsParent(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	project := t.TempDir()
	transcript := filepath.Join(project, "f223c386.jsonl")
	// Meta files as Claude Code 2.1.301 writes them, for a fork of the main agent, a fork of a subagent and a subagent, and as it writes them
	// for a subagent spawned in a worktree; one whose worktree is gone, and one not spawned in the worktree it names, work in nib's directory.
	worktree := t.TempDir()
	metas := map[string]string{
		"a3f5b656e725f8dc1": `{"agentType":"fork","isFork":true,"description":"fork probe","toolUseId":"toolu_01WkMMezPxWeDix8eW2wzJ8c","spawnDepth":1,"requestShape":"background","requestNonInteractive":true,"model":"inherit","finished":true}`,
		"a30cd37a40afff1e9": `{"agentType":"fork","isFork":true,"description":"nested fork","toolUseId":"toolu_017RLR5dvnUJ9grghEhsKCwB","parentAgentId":"a83334bbe36e1e7ac","spawnDepth":2,"requestShape":"foreground","requestNonInteractive":true,"model":"inherit","finished":true}`,
		"a83334bbe36e1e7ac": `{"agentType":"general-purpose","launchedWithDefaults":true,"description":"sub probe","toolUseId":"toolu_01NeFzhEw5GoZvXGDPExHSLC","spawnDepth":1,"requestShape":"background","requestNonInteractive":true,"finished":true}`,
		"a5cdeb96678c0dc2c": `{"agentType":"general-purpose","worktreePath":"` + worktree + `","spawnedWithWorktree":true,"worktreeBranch":"worktree-agent-a5cdeb96678c0dc2c","description":"web_act: no scroll when visible","toolUseId":"toolu_016N4K69XZDzPi67xGyLivgm","spawnDepth":1,"requestShape":"background","requestNonInteractive":true,"model":"opus"}`,
		"a5cdeb96678c0dc2d": `{"agentType":"general-purpose","worktreePath":"` + filepath.Join(worktree, "gone") + `","spawnedWithWorktree":true,"spawnDepth":1}`,
		"a5cdeb96678c0dc2e": `{"agentType":"general-purpose","worktreePath":"` + worktree + `","spawnDepth":1}`,
		"a0000000000000001": `{"agentType":"fork","isFork":true,"spawnDepth":2}`,
		"a0000000000000002": `{"agentType":"fork","isFork":true,"parentAgentId":"../x","spawnDepth":2}`,
	}
	for agent, meta := range metas {
		file := filepath.Join(project, "f223c386", "subagents", "agent-"+agent+".meta.json")
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agents := hookstate.OpenAgents(state)
	for _, c := range []struct {
		agent, agentType, transcript string
		want                         hookstate.Agent
	}{
		{"a3f5b656e725f8dc1", "fork", transcript, hookstate.Agent{ID: "a3f5b656e725f8dc1", Spawned: true, Forked: true}},
		{"a30cd37a40afff1e9", "fork", transcript, hookstate.Agent{ID: "a30cd37a40afff1e9", Spawned: true, Forked: true, Parent: "a83334bbe36e1e7ac"}},
		{"a83334bbe36e1e7ac", "general-purpose", transcript, hookstate.Agent{ID: "a83334bbe36e1e7ac", Spawned: true}},
		{"a5cdeb96678c0dc2c", "general-purpose", transcript, hookstate.Agent{ID: "a5cdeb96678c0dc2c", Spawned: true, Worktree: worktree}},
		{"a5cdeb96678c0dc2d", "general-purpose", transcript, hookstate.Agent{ID: "a5cdeb96678c0dc2d", Spawned: true}},
		{"a5cdeb96678c0dc2e", "general-purpose", transcript, hookstate.Agent{ID: "a5cdeb96678c0dc2e", Spawned: true}},
		// A meta file that disagrees with the hook input about being a fork is not trusted.
		{"a3f5b656e725f8dc1", "general-purpose", transcript, hookstate.Agent{ID: "a3f5b656e725f8dc1"}},
		// What the hook cannot tell leaves the agent on its own: no meta file, a deeper agent naming no parent or a bad one, no transcript.
		{"a0000000000000009", "fork", transcript, hookstate.Agent{ID: "a0000000000000009"}},
		{"a0000000000000001", "fork", transcript, hookstate.Agent{ID: "a0000000000000001"}},
		{"a0000000000000002", "fork", transcript, hookstate.Agent{ID: "a0000000000000002"}},
		{"a3f5b656e725f8dc1", "fork", "", hookstate.Agent{ID: "a3f5b656e725f8dc1"}},
	} {
		data, _ := json.Marshal(map[string]any{
			"session_id": "f223c386", "transcript_path": c.transcript, "hook_event_name": "PreToolUse", "tool_name": "mcp__nib__read",
			"tool_input": map[string]any{}, "tool_use_id": "toolu_x", "agent_id": c.agent, "agent_type": c.agentType,
		})
		if got := Run(data, state); got != "" {
			t.Fatalf("a fork's nib call must pass untouched, got %q", got)
		}
		if got := agents.Take("toolu_x"); got != c.want {
			t.Errorf("%s %s: got %+v, want %+v", c.agent, c.transcript, got, c.want)
		}
	}
}

// entry is one transcript line as Claude Code 2.1.301 writes it, with only the fields the hook reads; tools are the tool_use ids of an
// assistant entry.
func entry(kind, uuid, parent, stamp, message string, tools ...string) string {
	line := map[string]any{"type": kind, "timestamp": stamp, "isSidechain": false}
	if uuid != "" {
		line["uuid"], line["parentUuid"] = uuid, parent
	}
	if kind == "assistant" {
		var content []any
		for _, tool := range tools {
			content = append(content, map[string]any{"type": "tool_use", "id": tool, "name": "Agent", "input": map[string]any{"prompt": "toolu_01AGENT"}})
		}
		line["message"] = map[string]any{"id": message, "role": "assistant", "content": content}
	}
	if kind == "user" {
		line["message"] = map[string]any{"role": "user", "content": "result of toolu_01AGENT"}
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func TestAForksSpawnMomentIsReadFromItsParentsTranscript(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	project := t.TempDir()
	transcript := filepath.Join(project, "f223c386.jsonl")
	subagents := filepath.Join(project, "f223c386", "subagents")
	if err := os.MkdirAll(subagents, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The main agent read a file, then sent one message with a parallel nib read and the Agent call; the fork's context ends at the
	// attachment before that message, so the read in it, the request line and everything later are past the moment.
	write(transcript,
		`{"type":"queue-operation","timestamp":"2026-10-15T20:51:51.144Z"}`,
		entry("user", "u1", "", "2026-10-15T20:51:51.166Z", ""),
		`not json`,
		entry("assistant", "a1", "u1", "2026-10-15T20:51:53.516Z", "msg_1", "toolu_01READ"),
		entry("user", "r1", "a1", "2026-10-15T20:51:53.558Z", ""),
		entry("attachment", "t1", "r1", "2026-10-15T20:51:55.641Z", ""),
		`{"type":"api-request","timestamp":"2026-10-15T20:51:55.650Z"}`,
		entry("assistant", "a2", "t1", "2026-10-15T20:51:57.100Z", "msg_2", "toolu_01PARALLEL"),
		entry("assistant", "a3", "a2", "2026-10-15T20:51:57.405Z", "msg_2", "toolu_01AGENT"),
		entry("user", "r2", "a3", "2026-10-15T20:51:57.884Z", ""),
		entry("assistant", "a4", "r2", "2026-10-15T20:51:59.000Z", "msg_3", "toolu_01LATE"),
	)
	// A subagent's own transcript, with an attachment stamped out of order, as Claude Code writes them; it spawned a nested fork.
	write(filepath.Join(subagents, "agent-a83334bbe36e1e7ac.jsonl"),
		entry("user", "s1", "", "2026-10-15T20:52:00.829Z", ""),
		entry("attachment", "s2", "s1", "2026-10-15T20:52:03.754Z", ""),
		entry("assistant", "s3", "s2", "2026-10-15T20:52:03.385Z", "msg_4", "toolu_01SUBREAD"),
		entry("user", "s4", "s3", "2026-10-15T20:52:03.423Z", ""),
		entry("assistant", "s5", "s4", "2026-10-15T20:52:05.568Z", "msg_5", "toolu_01NESTED"),
	)
	metas := map[string]string{
		"a3f5b656e725f8dc1": `{"agentType":"fork","isFork":true,"toolUseId":"toolu_01AGENT","spawnDepth":1}`,
		"a30cd37a40afff1e9": `{"agentType":"fork","isFork":true,"toolUseId":"toolu_01NESTED","parentAgentId":"a83334bbe36e1e7ac","spawnDepth":2}`,
		"a0000000000000003": `{"agentType":"fork","isFork":true,"toolUseId":"toolu_01MISSING","spawnDepth":1}`,
		"a0000000000000004": `{"agentType":"general-purpose","toolUseId":"toolu_01PARALLEL","spawnDepth":1}`,
	}
	for agent, meta := range metas {
		write(filepath.Join(subagents, "agent-"+agent+".meta.json"), meta)
	}
	agents := hookstate.OpenAgents(state)
	run := func(agent string) hookstate.Agent {
		t.Helper()
		kind := "fork"
		if agent == "a0000000000000004" {
			kind = "general-purpose"
		}
		data, _ := json.Marshal(map[string]any{
			"session_id": "f223c386", "transcript_path": transcript, "hook_event_name": "PreToolUse", "tool_name": "mcp__nib__read",
			"tool_input": map[string]any{}, "tool_use_id": "toolu_x", "agent_id": agent, "agent_type": kind,
		})
		if got := Run(data, state); got != "" {
			t.Fatalf("a fork's nib call must pass untouched, got %q", got)
		}
		return agents.Take("toolu_x")
	}
	stamp := func(text string) time.Time {
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	if got := run("a3f5b656e725f8dc1"); !got.At.Equal(stamp("2026-10-15T20:51:55.642Z")) || !got.Forked || got.Parent != "" {
		t.Fatalf("a fork of the main agent must be spawned a millisecond past its context's last stamp, got %+v", got)
	}
	if got := run("a30cd37a40afff1e9"); !got.At.Equal(stamp("2026-10-15T20:52:03.755Z")) || got.Parent != "a83334bbe36e1e7ac" {
		t.Fatalf("a nested fork must be spawned past the latest stamp in its parent's transcript up to its context's end, got %+v", got)
	}
	if got := run("a0000000000000004"); !got.At.Equal(stamp("2026-10-15T20:51:55.642Z")) || got.Forked || !got.Spawned {
		t.Fatalf("a subagent that is no fork must be spawned at the same moment as one spawned in the same message, got %+v", got)
	}
	if got := run("a0000000000000003"); !got.Forked || !got.At.IsZero() {
		t.Fatalf("a fork whose Agent call is not in its parent's transcript must be filed with no moment, got %+v", got)
	}
	// The moment is filed on the fork's first call and read back after: the transcript is not read again.
	write(transcript, "")
	if got := run("a3f5b656e725f8dc1"); !got.At.Equal(stamp("2026-10-15T20:51:55.642Z")) {
		t.Fatalf("a fork's later calls must keep its first moment, got %+v", got)
	}
	if at, filed := hookstate.OpenSpawns(state).Of("a0000000000000003"); !filed || !at.IsZero() {
		t.Fatalf("a moment that cannot be told must be filed as unknown, got %v %v", at, filed)
	}
}

func TestContextDropsAreNotedAndFiledWithEveryNibCall(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	run := func(fields map[string]any) {
		t.Helper()
		data, _ := json.Marshal(fields)
		if got := Run(data, state); got != "" {
			t.Fatalf("%v must pass untouched, got %q", fields, got)
		}
	}
	call := func(toolUse, agent string) {
		run(map[string]any{"session_id": "6d0b", "hook_event_name": "PreToolUse", "tool_name": "mcp__nib__read", "tool_input": map[string]any{}, "tool_use_id": toolUse, "agent_id": agent})
	}
	drops := hookstate.OpenDrops(state)
	// Before a session start was noted, nib cannot tell when the session's contexts drop: no drop record at all.
	call("toolu_before", "")
	if _, known := drops.Take("toolu_before"); known {
		t.Fatal("a session whose start the hook never saw has no drop record")
	}
	before := time.Now()
	run(map[string]any{"session_id": "6d0b", "hook_event_name": "SessionStart", "source": "startup"})
	call("toolu_main", "")
	started, known := drops.Take("toolu_main")
	if !known || started.Before(before) {
		t.Fatalf("the main agent's call carries its session start: %v %v", started, known)
	}
	call("toolu_sub", "a1d576263ade979e4")
	if dropped, known := drops.Take("toolu_sub"); !known || !dropped.IsZero() {
		t.Fatalf("a subagent never compacted has no drop: %v %v", dropped, known)
	}
	run(map[string]any{"session_id": "6d0b", "hook_event_name": "PostCompact", "trigger": "auto", "agent_id": "a1d576263ade979e4"})
	call("toolu_sub", "a1d576263ade979e4")
	if dropped, known := drops.Take("toolu_sub"); !known || !dropped.After(started) {
		t.Fatalf("the subagent's compaction is filed with its call: %v %v", dropped, known)
	}
	call("toolu_main", "")
	if dropped, _ := drops.Take("toolu_main"); !dropped.Equal(started) {
		t.Fatalf("a subagent's compaction is not the main agent's: %v", dropped)
	}
	if _, known := drops.Take("toolu_main"); known {
		t.Fatal("a drop record is taken once")
	}
}

func TestAForkInheritsItsParentsDropsBeforeItsSpawn(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	contexts := hookstate.OpenContexts(state)
	now := time.Now()
	moment := func(seconds int64) time.Time { return time.Unix(seconds, 0) }
	for _, m := range []int64{100, 300} {
		if err := contexts.Drop(hookstate.SessionKey("6d0b"), moment(m), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := contexts.Drop(hookstate.AgentKey("a83334bbe36e1e7ac"), moment(250), now); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		agent hookstate.Agent
		want  time.Time
	}{
		// The main agent's compaction at 300 came after the spawn at 200, so the fork still has what it was shown before it.
		{hookstate.Agent{ID: "a3f5b656e725f8dc1", Spawned: true, Forked: true, At: moment(200)}, moment(100)},
		{hookstate.Agent{ID: "a3f5b656e725f8dc1", Spawned: true, Forked: true}, moment(300)},
		{hookstate.Agent{ID: "a30cd37a40afff1e9", Spawned: true, Forked: true, Parent: "a83334bbe36e1e7ac", At: moment(260)}, moment(250)},
		// A subagent that is not a fork starts with none of its parent's conversation.
		{hookstate.Agent{ID: "a5cdeb96678c0dc2c", Spawned: true, At: moment(400)}, time.Time{}},
	} {
		if got, known := droppedAt("6d0b", c.agent, state); !known || !got.Equal(c.want) {
			t.Errorf("%+v: got %v %v, want %v", c.agent, got, known, c.want)
		}
	}
}
