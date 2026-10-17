package setup

import (
	"path/filepath"
	"testing"

	"github.com/11ways/nib/internal/providers"
)

var zemble = providers.Spec{Name: "zemble", Program: "/opt/zemble"}

func TestWantedKeepsHostedProvidersAndAppliesChanges(t *testing.T) {
	words := []string{"nib", "--provider", "zemble=/old/zemble", "--provider=other=other"}
	equal(t, wanted(nib, words, nil), []string{"nib", "--provider", "zemble=/old/zemble", "--provider", "other=other"})
	equal(t, wanted(nib, words, []providers.Spec{zemble}), []string{"nib", "--provider", "zemble=/opt/zemble", "--provider", "other=other"})
	equal(t, wanted(nib, words, []providers.Spec{{Name: "other"}, {Name: "third", Program: "t"}}), []string{"nib", "--provider", "zemble=/old/zemble", "--provider", "third=t"})
	// A --provider setup does not understand stays part of the command, so the entry reads as stale rather than as hosting something.
	rest, hosted := split([]string{"nib", "--provider", "Bad"})
	equal(t, rest, []string{"nib", "--provider", "Bad"})
	equal(t, len(hosted), 0)
	for text, want := range map[string]providers.Spec{"zemble=/opt/zemble": zemble, "zemble=": {Name: "zemble"}} {
		if got, err := ParseChange(text); err != nil || got != want {
			t.Errorf("%s: %v %v", text, got, err)
		}
	}
	for _, bad := range []string{"=", "Zemble=", "zemble"} {
		if _, err := ParseChange(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestAProjectHostsAProviderInsteadOfRunningItBeside(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "project")
	writeJSON(t, filepath.Join(directory, ".mcp.json"), map[string]any{"mcpServers": map[string]any{"zemble": map[string]any{"command": "zemble"}, "other": map[string]any{"command": "x"}}})
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{"zemble": map[string]any{"command": "zemble"}},
		"projects":   map[string]any{directory: map[string]any{"mcpServers": map[string]any{"zemble": map[string]any{"command": "zemble"}}}},
	})
	var calls []claudeCall
	options := Options{Directory: directory, Command: nib, Home: home, Env: func(string) string { return "" }, Providers: []providers.Spec{zemble}, Claude: func(dir string, args []string) (int, string) {
		calls = append(calls, claudeCall{dir, args})
		return 0, ""
	}}
	report := Setup(options)
	equal(t, []string{report[0], report[2], report[3]}, []string{
		".mcp.json: added the nib MCP server; removed the zemble MCP server, now hosted by nib",
		"claude mcp: removed the zemble registration in ~/project, now hosted by nib",
		"claude mcp: zemble is also registered for this user, so Claude Code runs it beside nib here; nib setup --user --provider ... removes it",
	})
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json")), map[string]any{"mcpServers": map[string]any{
		"other": map[string]any{"command": "x"},
		"nib":   map[string]any{"type": "stdio", "command": "nib", "args": []any{"--provider", "zemble=/opt/zemble"}},
	}})
	equal(t, calls, []claudeCall{{directory, []string{"mcp", "remove", "--scope", "local", "zemble"}}})
	// A setup that names no provider keeps the one the entry hosts; NAME= stops hosting it.
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{})
	options.Providers = nil
	equal(t, Setup(options)[0], ".mcp.json: already set up")
	options.Providers = []providers.Spec{{Name: "zemble"}}
	equal(t, Setup(options)[0], ".mcp.json: added nib's command, hosting no provider instead of zemble=/opt/zemble")
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json"))["mcpServers"].(map[string]any)["nib"], map[string]any{"type": "stdio", "command": "nib", "args": []any{}})
}

func TestAUserHostsAProviderInsteadOfRunningItBeside(t *testing.T) {
	home := t.TempDir()
	project, gone := filepath.Join(home, "project"), filepath.Join(home, "gone")
	writeFile(t, filepath.Join(project, "x"), "")
	registry := filepath.Join(home, "claude/.claude.json")
	writeJSON(t, registry, map[string]any{
		"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": "nib", "args": []any{}}, "zemble": map[string]any{"command": "zemble"}},
		"projects": map[string]any{
			project: map[string]any{"mcpServers": map[string]any{"zemble": map[string]any{"command": "zemble"}}},
			gone:    map[string]any{"mcpServers": map[string]any{"zemble": map[string]any{"command": "zemble"}}},
		},
	})
	var calls []claudeCall
	env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, "claude")}
	options := Options{User: true, Command: nib, Home: home, Env: func(k string) string { return env[k] }, Providers: []providers.Spec{zemble}, Claude: func(dir string, args []string) (int, string) {
		calls = append(calls, claudeCall{dir, args})
		return 0, ""
	}}
	options.DryRun = true
	equal(t, Setup(options)[1:], []string{
		"claude mcp: would replace the nib registration for this user, to host zemble=/opt/zemble instead of no provider",
		"claude mcp: would remove the zemble registration for this user, now hosted by nib",
		"claude mcp: would remove the zemble registration in ~/project, now hosted by nib",
	})
	equal(t, len(calls), 0)
	options.DryRun = false
	Setup(options)
	equal(t, calls, []claudeCall{
		{"", []string{"mcp", "remove", "--scope", "user", "nib"}},
		{"", []string{"mcp", "add-json", "--scope", "user", "nib", `{"args":["--provider","zemble=/opt/zemble"],"command":"nib","type":"stdio"}`}},
		{"", []string{"mcp", "remove", "--scope", "user", "zemble"}},
		{project, []string{"mcp", "remove", "--scope", "local", "zemble"}},
	})
}
