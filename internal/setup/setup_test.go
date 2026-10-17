package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var nib = []string{"nib"}

// TestMain keeps every test away from the real Claude Code and OpenCode configs, which an Options without Home or Env falls back to.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "nib-setup-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "OPENCODE_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		os.Unsetenv(name)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("got  %s\nwant %s", g, w)
	}
}

// line is the report's line about what, a file or "claude mcp", so a test does not count on the place of the others.
func line(t *testing.T, report []string, what string) string {
	t.Helper()
	for _, text := range report {
		if strings.HasPrefix(text, what+":") {
			return text
		}
	}
	t.Fatalf("no line for %s in %v", what, report)
	return ""
}

// claudeLines are the report's lines about what setup did or would do through Claude Code's CLI.
func claudeLines(report []string) []string {
	var lines []string
	for _, text := range report {
		if strings.HasPrefix(text, "claude ") {
			lines = append(lines, text)
		}
	}
	return lines
}

func TestFreshProjectGetsServerHookApprovalAndDenyRules(t *testing.T) {
	directory := t.TempDir()
	equal(t, Setup(Options{Directory: directory, Command: nib}), []string{
		".mcp.json: added the nib MCP server",
		".claude/settings.local.json: added approval of the nib server in .mcp.json and nib's PreToolUse, SessionStart and PostCompact hooks and deny rules for Bash, Grep, Glob, Edit, Write and allow rules for mcp__nib__nibash_status",
	})
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json")), map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": "nib", "args": []any{}}}})
	equal(t, readJSON(t, filepath.Join(directory, ".claude/settings.local.json")), map[string]any{
		"enabledMcpjsonServers": []any{"nib"},
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{"matcher": "Read", "hooks": []any{map[string]any{"type": "command", "command": "nib hook", "timeout": float64(10)}}},
				map[string]any{"matcher": "mcp__nib__.*", "hooks": []any{map[string]any{"type": "command", "command": "nib hook", "timeout": float64(10)}}},
			},
			"SessionStart": []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "nib hook", "timeout": float64(10)}}}},
			"PostCompact":  []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "nib hook", "timeout": float64(10)}}}},
		},
		"permissions": map[string]any{"allow": []any{"mcp__nib__nibash_status"}, "deny": []any{"Bash", "Grep", "Glob", "Edit", "Write"}},
	})
	equal(t, Setup(Options{Directory: directory, Command: nib}), []string{".mcp.json: already set up", ".claude/settings.local.json: already set up"})
}

func TestExistingConfigsKeepTheirOrderAndContent(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, filepath.Join(directory, ".mcp.json"), `{"mcpServers": {"sketerm": {"type": "stdio", "command": "sketerm", "args": ["mcp"]}}}`)
	writeFile(t, filepath.Join(directory, ".claude/settings.local.json"), `{"zeta": 1.50, "enabledMcpjsonServers": ["sketerm"], "permissions": {"deny": ["Bash", "WebFetch"]}, "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "./guard.sh"}]}]}}`)
	writeFile(t, filepath.Join(directory, "opencode.json"), `{"permission": {"bash": {"*": "allow"}}, "mcp": {"zenit-dev": {"type": "local", "command": ["zenit-dev", "mcp"]}}}`)
	install := filepath.Join(directory, "opt/my nib/nib")
	report := Setup(Options{Directory: directory, Command: []string{install}})
	equal(t, report, []string{
		".mcp.json: added the nib MCP server",
		".claude/settings.local.json: added approval of the nib server in .mcp.json and nib's PreToolUse, SessionStart and PostCompact hooks and deny rules for Grep, Glob, Edit, Write and allow rules for mcp__nib__nibash_status",
		"opencode.json: added the nib MCP server and deny rules for read, grep, glob, edit (permissions for shell set there already, so OpenCode keeps those tools as configured)",
	})
	settings, _ := os.ReadFile(filepath.Join(directory, ".claude/settings.local.json"))
	// Keys stay in the order they were written, and numbers as they were written.
	if !strings.HasPrefix(string(settings), "{\n  \"zeta\": 1.50,\n  \"enabledMcpjsonServers\": [") {
		t.Fatalf("order or numbers changed:\n%s", settings)
	}
	hooks := readJSON(t, filepath.Join(directory, ".claude/settings.local.json"))["hooks"].(map[string]any)["PreToolUse"].([]any)
	equal(t, hooks[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"], "'"+install+"' hook")
	opencode := readJSON(t, filepath.Join(directory, "opencode.json"))
	equal(t, opencode["permission"].(map[string]any)["bash"], map[string]any{"*": "allow"})
	equal(t, opencode["mcp"], map[string]any{"zenit-dev": map[string]any{"type": "local", "command": []any{"zenit-dev", "mcp"}}, "servers": map[string]any{"nib": map[string]any{"type": "local", "command": []any{install}, "codemode": false}}})
	equal(t, opencode["permissions"], []any{map[string]any{"action": "read", "resource": "*", "effect": "deny"}, map[string]any{"action": "grep", "resource": "*", "effect": "deny"}, map[string]any{"action": "glob", "resource": "*", "effect": "deny"}, map[string]any{"action": "edit", "resource": "*", "effect": "deny"}})
	// A nib entry that does not start the nib setup registers, such as one whose program is gone, is pointed at it, keeping its other keys.
	stale := "nib's command, replacing a stale one ('" + install + "')"
	equal(t, Setup(Options{Directory: directory, Command: nib}), []string{".mcp.json: added " + stale, ".claude/settings.local.json: already set up", "opencode.json: added " + stale + " (permissions for shell set there already, so OpenCode keeps those tools as configured)"})
	equal(t, Setup(Options{Directory: directory, Command: nib}), []string{".mcp.json: already set up", ".claude/settings.local.json: already set up", "opencode.json: already set up (permissions for shell set there already, so OpenCode keeps those tools as configured)"})
}

func TestDryRunDisabledServerAndNonJSON(t *testing.T) {
	directory := t.TempDir()
	equal(t, Setup(Options{Directory: directory, Command: nib, DryRun: true}), []string{
		".mcp.json: would add the nib MCP server",
		".claude/settings.local.json: would add approval of the nib server in .mcp.json and nib's PreToolUse, SessionStart and PostCompact hooks and deny rules for Bash, Grep, Glob, Edit, Write and allow rules for mcp__nib__nibash_status",
	})
	if _, err := os.Stat(filepath.Join(directory, ".mcp.json")); err == nil {
		t.Fatal("a dry run wrote")
	}
	writeFile(t, filepath.Join(directory, ".claude/settings.local.json"), `{"disabledMcpjsonServers": ["nib"]}`)
	writeFile(t, filepath.Join(directory, ".mcp.json"), "{ // comment\n}")
	equal(t, Setup(Options{Directory: directory, Command: nib}), []string{
		".mcp.json: left untouched, since it is not plain JSON (comments?)",
		".claude/settings.local.json: added nib's PreToolUse, SessionStart and PostCompact hooks and deny rules for Bash, Grep, Glob, Edit, Write and allow rules for mcp__nib__nibash_status (nib is in disabledMcpjsonServers there, so it stays off until you remove it)",
	})
}

func TestUserSetup(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "xdg/opencode/opencode.json"), "{}")
	env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, "configs/nib"), "XDG_CONFIG_HOME": filepath.Join(home, "xdg")}
	var calls [][]string
	claude := func(_ string, args []string) (int, string) {
		calls = append(calls, args)
		return 0, ""
	}
	options := Options{Directory: home, User: true, Command: nib, Home: home, Env: func(k string) string { return env[k] }, Claude: claude}
	equal(t, Setup(options), []string{
		"~/.local/share/nib/mods/nibash: installed the nibash mod of this nib",
		"~/configs/nib/settings.json: added nib's PreToolUse, SessionStart and PostCompact hooks and deny rules for Bash, Grep, Glob, Edit, Write and allow rules for mcp__nib__nibash_status and the nibash mod in env.CLAUDE_CODE_PLUGIN_DIRS",
		"claude mcp: added nib for this user",
		"~/xdg/opencode/opencode.json: added the nib MCP server and deny rules for shell, read, grep, glob, edit",
	})
	equal(t, calls, [][]string{{"mcp", "add", "--scope", "user", "nib", "--", "nib"}})
	// Only a user-scope server counts: one a project registers does not.
	writeFile(t, filepath.Join(home, "configs/nib/.claude.json"), `{"projects": {"x": {"mcpServers": {"nib": {}}}}}`)
	if report := Setup(options); line(t, report, "claude mcp") != "claude mcp: added nib for this user" {
		t.Fatalf("got %v", report)
	}
	writeFile(t, filepath.Join(home, "configs/nib/.claude.json"), `{"mcpServers": {"nib": {"type": "stdio", "command": "nib", "args": []}}}`)
	if report := Setup(options); line(t, report, "claude mcp") != "claude mcp: nib is already registered for this user" {
		t.Fatalf("got %v", report)
	}
}

func TestModesLinksUnreadableFilesAndForeignHooks(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, filepath.Join(directory, ".mcp.json"), `{"mcpServers": {"secret": {"command": "x", "env": {"TOKEN": "t"}}}}`)
	if err := os.Chmod(filepath.Join(directory, ".mcp.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	dotfiles := t.TempDir()
	writeFile(t, filepath.Join(dotfiles, "settings.local.json"), `{"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "bun /opt/other/src/main.ts hook"}]}]}}`)
	if err := os.MkdirAll(filepath.Join(directory, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "settings.local.json"), filepath.Join(directory, ".claude/settings.local.json")); err != nil {
		t.Fatal(err)
	}
	Setup(Options{Directory: directory, Command: nib})
	if info, _ := os.Stat(filepath.Join(directory, ".mcp.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if info, _ := os.Lstat(filepath.Join(directory, ".claude/settings.local.json")); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
	entries := readJSON(t, filepath.Join(dotfiles, "settings.local.json"))["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(entries) != 3 || entries[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "nib hook" {
		t.Fatalf("another program's main.ts hook is not nib's: %v", entries)
	}
	unreadable := t.TempDir()
	writeFile(t, filepath.Join(unreadable, ".mcp.json"), `{"mcpServers": {"keep": {"command": "x"}}}`)
	if err := os.Chmod(filepath.Join(unreadable, ".mcp.json"), 0); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if report := Setup(Options{Directory: unreadable, Command: nib}); report[0] != ".mcp.json: left untouched, since it cannot be read (EACCES)" {
			t.Fatalf("got %v", report)
		}
	}
	_ = os.Chmod(filepath.Join(unreadable, ".mcp.json"), 0o600)
	equal(t, readJSON(t, filepath.Join(unreadable, ".mcp.json"))["mcpServers"], map[string]any{"keep": map[string]any{"command": "x"}})
}

func TestOlderNibHooksCountAsNibs(t *testing.T) {
	install := t.TempDir()
	writeFile(t, filepath.Join(install, "package.json"), `{"name": "@11ways/nib"}`)
	writeFile(t, filepath.Join(install, "src/main.ts"), "")
	for _, command := range []string{"nib hook", "/usr/local/bin/nib hook", "/usr/bin/bun '" + filepath.Join(install, "src/main.ts") + "' hook"} {
		if !isNibHook(command) {
			t.Errorf("%q should be nib's hook", command)
		}
	}
	for _, command := range []string{"bun /opt/other/src/main.ts hook", "./guard.sh", "nib setup"} {
		if isNibHook(command) {
			t.Errorf("%q is not nib's hook", command)
		}
	}
}

func TestOlderHooksGetTheToolsMatcherAndLoseMultiEdit(t *testing.T) {
	directory := t.TempDir()
	settings := filepath.Join(directory, ".claude/settings.local.json")
	// What an older setup wrote: the hook for Read only, through an older matcher, and a deny rule for MultiEdit, kept in its place.
	writeFile(t, settings, `{"enabledMcpjsonServers": ["nib"], "hooks": {"PreToolUse": [{"matcher": "Bash|Read|Grep", "hooks": [{"type": "command", "command": "nib hook"}]}]}, "permissions": {"deny": ["Bash", "MultiEdit", "Grep", "Glob", "Edit", "Write"]}}`)
	writeFile(t, filepath.Join(directory, ".mcp.json"), `{"mcpServers": {"nib": {"command": "nib"}}}`)
	equal(t, Setup(Options{Directory: directory, Command: nib, DryRun: true})[1],
		".claude/settings.local.json: would add nib's PreToolUse for mcp__nib__.*, SessionStart and PostCompact hooks and allow rules for mcp__nib__nibash_status; would remove the deny rule for MultiEdit, a tool Claude Code no longer has")
	equal(t, Setup(Options{Directory: directory, Command: nib})[1],
		".claude/settings.local.json: added nib's PreToolUse for mcp__nib__.*, SessionStart and PostCompact hooks and allow rules for mcp__nib__nibash_status; removed the deny rule for MultiEdit, a tool Claude Code no longer has")
	written := readJSON(t, settings)
	equal(t, written["permissions"], map[string]any{"allow": []any{"mcp__nib__nibash_status"}, "deny": []any{"Bash", "Grep", "Glob", "Edit", "Write"}})
	entries := written["hooks"].(map[string]any)["PreToolUse"].([]any)
	equal(t, len(entries), 2)
	equal(t, entries[1].(map[string]any)["matcher"], "mcp__nib__.*")
	equal(t, Setup(Options{Directory: directory, Command: nib})[1], ".claude/settings.local.json: already set up")
}

func TestContextHooksJoinAnEarlierSetupOnce(t *testing.T) {
	directory := t.TempDir()
	settings := filepath.Join(directory, ".claude/settings.local.json")
	// What setup wrote before nib noticed compactions, plus a SessionStart hook of the user's own, which stays.
	writeFile(t, settings, `{"enabledMcpjsonServers": ["nib"], "hooks": {"PreToolUse": [{"matcher": "Read", "hooks": [{"type": "command", "command": "nib hook", "timeout": 10}]}, {"matcher": "mcp__nib__.*", "hooks": [{"type": "command", "command": "nib hook", "timeout": 10}]}], "SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "echo hi"}]}]}, "permissions": {"allow": ["mcp__nib__nibash_status"], "deny": ["Bash", "Grep", "Glob", "Edit", "Write"]}}`)
	writeFile(t, filepath.Join(directory, ".mcp.json"), `{"mcpServers": {"nib": {"command": "nib"}}}`)
	equal(t, Setup(Options{Directory: directory, Command: nib})[1], ".claude/settings.local.json: added nib's SessionStart and PostCompact hooks")
	hooks := readJSON(t, settings)["hooks"].(map[string]any)
	equal(t, hooks["SessionStart"], []any{
		map[string]any{"matcher": "startup", "hooks": []any{map[string]any{"type": "command", "command": "echo hi"}}},
		map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "nib hook", "timeout": float64(10)}}},
	})
	equal(t, len(hooks["PostCompact"].([]any)), 1)
	equal(t, Setup(Options{Directory: directory, Command: nib})[1], ".claude/settings.local.json: already set up")
}

func TestMatchersFollowClaudeCode(t *testing.T) {
	cases := []struct {
		matcher, tool string
		want          bool
	}{
		{"", "mcp__nib__read", true},
		{"*", "Read", true},
		{"Read", "Read", true},
		{"Bash|Read", "Read", true},
		{"Bash, Read", "Read", true},
		{"Read", "mcp__nib__read", false},
		{"mcp__nib__.*", "mcp__nib__read", true},
		{"mcp__.*", "mcp__nib__read", true},
		{"Read|mcp__nib__.*", "Read", true},
		{"mcp__nib__.*", "Read", false},
		{"Re", "Read", false},
		{"(", "Read", false},
	}
	for _, c := range cases {
		if got := matches(c.matcher, c.tool); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.matcher, c.tool, got, c.want)
		}
	}
}

func TestAnOpenCode1EntryMovesToOpenCode2sForm(t *testing.T) {
	directory := t.TempDir()
	// What an older setup wrote: the server as OpenCode 1 had it, and denials under OpenCode 1's tool names, which OpenCode 2 still reads.
	old := `{"mcp": {"zenit-dev": {"type": "local", "command": ["zenit-dev"]}, "nib": {"type": "local", "command": ["nib"], "environment": {"NIB_SANDBOX": "1"}, "enabled": false, "timeout": 5000}}, "permission": {"bash": "deny", "read": "deny", "grep": "deny", "glob": "deny", "list": "deny", "edit": "deny", "write": "deny", "patch": "deny"}}`
	writeFile(t, filepath.Join(directory, "opencode.json"), old)
	dry := Setup(Options{Directory: directory, Command: nib, DryRun: true})
	equal(t, dry[2], "opencode.json: would add the nib MCP server in OpenCode 2's form, outside code mode; would remove its OpenCode 1 form (mcp.nib)")
	if data, _ := os.ReadFile(filepath.Join(directory, "opencode.json")); string(data) != old {
		t.Fatal("a dry run wrote")
	}
	equal(t, Setup(Options{Directory: directory, Command: nib})[2], "opencode.json: added the nib MCP server in OpenCode 2's form, outside code mode; removed its OpenCode 1 form (mcp.nib)")
	opencode := readJSON(t, filepath.Join(directory, "opencode.json"))
	equal(t, opencode["mcp"], map[string]any{
		"zenit-dev": map[string]any{"type": "local", "command": []any{"zenit-dev"}},
		"servers":   map[string]any{"nib": map[string]any{"type": "local", "command": []any{"nib"}, "environment": map[string]any{"NIB_SANDBOX": "1"}, "disabled": true, "timeout": map[string]any{"catalog": 5000.0, "execution": 5000.0}, "codemode": false}},
	})
	if _, ok := opencode["permissions"]; ok {
		t.Fatal("tools OpenCode 1's permission denies already must get no second rule")
	}
	equal(t, Setup(Options{Directory: directory, Command: nib})[2], "opencode.json: already set up")
}

func TestOpenCodeServersOutsideCodeModeAndForeignForms(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "oc")
	env := map[string]string{"OPENCODE_CONFIG_DIR": config, "CLAUDE_CONFIG_DIR": filepath.Join(home, "claude")}
	options := Options{Directory: home, User: true, Command: nib, Home: home, Env: func(k string) string { return env[k] }, Claude: func(string, []string) (int, string) { return 0, "" }}
	file := filepath.Join(config, "opencode.json")
	// A nib server in OpenCode 2's form with code mode unset gets it turned off; a rule the user wrote for an action keeps it.
	writeFile(t, file, `{"permissions": [{"action": "edit", "resource": "*.md", "effect": "allow"}], "mcp": {"servers": {"nib": {"type": "local", "command": ["/opt/nib"]}}}}`)
	equal(t, line(t, Setup(options), "~/oc/opencode.json"), "~/oc/opencode.json: added nib's command, replacing a stale one (/opt/nib) and code mode off for the nib MCP server and deny rules for shell, read, grep, glob (permissions for edit set there already, so OpenCode keeps those tools as configured)")
	written := readJSON(t, file)
	equal(t, written["mcp"], map[string]any{"servers": map[string]any{"nib": map[string]any{"type": "local", "command": []any{"nib"}, "codemode": false}}})
	equal(t, len(written["permissions"].([]any)), 5)
	// Code mode someone turned on stays on, and an OpenCode 1 entry setup cannot read stays as it is.
	writeFile(t, file, `{"mcp": {"servers": {"nib": {"type": "local", "command": ["nib"], "codemode": true}}}, "tools": {"bash": false, "read": false, "grep": false, "glob": false, "edit": false}}`)
	equal(t, line(t, Setup(options), "~/oc/opencode.json"), "~/oc/opencode.json: already set up (codemode is set for nib there, so OpenCode offers nib's tools as configured)")
	writeFile(t, file, `{"mcp": {"nib": {"type": "remote", "url": "http://x"}}, "permissions": {"x": 1}}`)
	equal(t, line(t, Setup(options), "~/oc/opencode.json"), "~/oc/opencode.json: already set up (its mcp.nib is not a local server in OpenCode 1's form, so it stays as it is; its \"permissions\" is not a list, so OpenCode's own tools stay on)")
}

func executable(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, "#!/bin/sh\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data))
}

type claudeCall struct {
	Dir  string
	Args []string
}

func TestStaleClaudeRegistrationsAreReplaced(t *testing.T) {
	home := t.TempDir()
	mine, other, link := filepath.Join(home, "bin/nib"), filepath.Join(home, "other/nib"), filepath.Join(home, "link/nib")
	executable(t, mine)
	executable(t, other)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mine, link); err != nil {
		t.Fatal(err)
	}
	project, same, remote, gone := filepath.Join(home, "project"), filepath.Join(home, "same"), filepath.Join(home, "remote"), filepath.Join(home, "gone")
	for _, dir := range []string{project, same, remote} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(home, "old/src/main.ts")
	registry := filepath.Join(home, "claude/.claude.json")
	local := func(server map[string]any) map[string]any {
		return map[string]any{"mcpServers": map[string]any{"nib": server}}
	}
	// 1. What the Bun nib left: the user's server runs a main.ts that is gone, a project's runs another nib, and three projects stay as they
	// are: one runs this nib through a link, one is remote, and one is for a folder that is gone.
	writeJSON(t, registry, map[string]any{
		"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": "bun", "args": []any{script}, "alwaysLoad": true}},
		"projects": map[string]any{
			project: local(map[string]any{"type": "stdio", "command": other, "args": []any{}}),
			same:    local(map[string]any{"type": "stdio", "command": link, "args": []any{}}),
			remote:  local(map[string]any{"type": "http", "url": "http://x"}),
			gone:    local(map[string]any{"type": "stdio", "command": "bun", "args": []any{script}}),
		},
	})
	var calls []claudeCall
	env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, "claude")}
	options := Options{User: true, Command: []string{mine}, Home: home, Env: func(k string) string { return env[k] }, Claude: func(dir string, args []string) (int, string) {
		calls = append(calls, claudeCall{dir, args})
		return 0, ""
	}}
	userStale := "the stale nib registration for this user (bun " + script + ")"
	projectStale := "the stale nib registration in ~/project (" + other + ")"
	// 2. A dry run names both, and runs nothing.
	options.DryRun = true
	equal(t, claudeLines(Setup(options)), []string{"claude mcp: would replace " + userStale, "claude mcp: would replace " + projectStale})
	equal(t, len(calls), 0)
	// 3. Each is removed and added again at its own scope, in its folder for a local one, pointed at this nib, with its other keys kept.
	options.DryRun = false
	equal(t, claudeLines(Setup(options)), []string{"claude mcp: replaced " + userStale, "claude mcp: replaced " + projectStale})
	equal(t, calls, []claudeCall{
		{"", []string{"mcp", "remove", "--scope", "user", "nib"}},
		{"", []string{"mcp", "add-json", "--scope", "user", "nib", `{"alwaysLoad":true,"args":[],"command":"` + mine + `","type":"stdio"}`}},
		{project, []string{"mcp", "remove", "--scope", "local", "nib"}},
		{project, []string{"mcp", "add-json", "--scope", "local", "nib", `{"args":[],"command":"` + mine + `","type":"stdio"}`}},
	})
	// 4. Registrations that start this nib, by any path to it, are left alone.
	calls = nil
	writeJSON(t, registry, map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": link}}, "projects": map[string]any{same: local(map[string]any{"command": mine})}})
	equal(t, claudeLines(Setup(options)), []string{"claude mcp: nib is already registered for this user"})
	equal(t, len(calls), 0)
	// 5. A failed removal adds nothing.
	writeJSON(t, registry, map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"command": other}}})
	options.Claude = func(dir string, args []string) (int, string) {
		calls = append(calls, claudeCall{dir, args})
		return 1, "No user-scoped MCP server found with name: nib\n"
	}
	equal(t, claudeLines(Setup(options)), []string{"claude mcp remove failed for this user: No user-scoped MCP server found with name: nib"})
	equal(t, len(calls), 1)
}

func TestAStaleProjectServerIsRepointed(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "project")
	script := filepath.Join(home, "old/src/main.ts")
	writeJSON(t, filepath.Join(directory, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": "bun", "args": []any{script}, "env": map[string]any{"NIB_SANDBOX": "1"}}}})
	// A local-scope registration for the folder would win over .mcp.json, so a stale one is replaced too.
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{"projects": map[string]any{directory: map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"command": "bun", "args": []any{script}}}}}})
	var calls []claudeCall
	options := Options{Directory: directory, Command: nib, Home: home, Env: func(string) string { return "" }, Claude: func(dir string, args []string) (int, string) {
		calls = append(calls, claudeCall{dir, args})
		return 0, ""
	}}
	stale := "nib's command, replacing a stale one (bun " + script + ")"
	local := "the stale nib registration in ~/project (bun " + script + ")"
	// 1. A dry run says what it would change, and changes nothing.
	options.DryRun = true
	report := Setup(options)
	equal(t, []string{report[0], report[len(report)-1]}, []string{".mcp.json: would add " + stale, "claude mcp: would replace " + local})
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json"))["mcpServers"].(map[string]any)["nib"].(map[string]any)["command"], "bun")
	equal(t, len(calls), 0)
	// 2. The entry is pointed at nib with its environment kept, and the local registration is replaced in the folder.
	options.DryRun = false
	report = Setup(options)
	equal(t, []string{report[0], report[len(report)-1]}, []string{".mcp.json: added " + stale, "claude mcp: replaced " + local})
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json")), map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": "nib", "args": []any{}, "env": map[string]any{"NIB_SANDBOX": "1"}}}})
	equal(t, calls, []claudeCall{{directory, []string{"mcp", "remove", "--scope", "local", "nib"}}, {directory, []string{"mcp", "add-json", "--scope", "local", "nib", `{"args":[],"command":"nib"}`}}})
	// 3. A matching entry stays as it is.
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{})
	equal(t, Setup(options)[0], ".mcp.json: already set up")
}
