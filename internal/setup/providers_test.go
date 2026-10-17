package setup

import (
	"os"
	"path/filepath"
	"strings"
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
	equal(t, report[0], "~/.config/nib/setup.json: remembers zemble=/opt/zemble for later setups")
	equal(t, []string{line(t, report, ".mcp.json"), report[len(report)-2], report[len(report)-1]}, []string{
		".mcp.json: added the nib MCP server; removed the zemble MCP server, now hosted by nib",
		"claude mcp: removed the zemble registration in ~/project, now hosted by nib",
		"claude mcp: zemble is also registered for this user, so Claude Code runs it beside nib here; nib setup --user --provider ... removes it",
	})
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json")), map[string]any{"mcpServers": map[string]any{
		"other": map[string]any{"command": "x"},
		"nib":   map[string]any{"type": "stdio", "command": "nib", "args": []any{"--provider", "zemble=/opt/zemble"}},
	}})
	equal(t, calls, []claudeCall{{directory, []string{"mcp", "remove", "--scope", "local", "zemble"}}})
	// A setup that names no provider keeps the one the entry hosts, and gives a new entry the remembered ones; NAME= stops hosting it.
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{})
	options.Providers = nil
	equal(t, Setup(options)[0], ".mcp.json: already set up")
	other := filepath.Join(home, "other")
	Setup(Options{Directory: other, Command: nib, Home: home, Env: func(string) string { return "" }, Claude: options.Claude})
	equal(t, readJSON(t, filepath.Join(other, ".mcp.json"))["mcpServers"].(map[string]any)["nib"], map[string]any{"type": "stdio", "command": "nib", "args": []any{"--provider", "zemble=/opt/zemble"}})
	options.Providers = []providers.Spec{{Name: "zemble"}}
	report = Setup(options)
	equal(t, report[0], "~/.config/nib/setup.json: remembers no provider for later setups")
	equal(t, line(t, report, ".mcp.json"), ".mcp.json: added nib's command, hosting no provider instead of zemble=/opt/zemble")
	equal(t, readJSON(t, filepath.Join(directory, ".mcp.json"))["mcpServers"].(map[string]any)["nib"], map[string]any{"type": "stdio", "command": "nib", "args": []any{}})
}

func TestUserSetupInstallsTheNibashModOnce(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, "claude"), "XDG_DATA_HOME": filepath.Join(home, "data")}
	writeJSON(t, filepath.Join(home, "claude/settings.json"), map[string]any{"env": map[string]any{pluginDirsVariable: "/elsewhere/mod"}})
	options := Options{User: true, Command: nib, Home: home, Env: func(k string) string { return env[k] }, Claude: func(string, []string) (int, string) { return 0, "" }}
	options.DryRun = true
	equal(t, Setup(options)[0], "~/data/nib/mods/nibash: would install the nibash mod of this nib")
	if _, err := os.Stat(filepath.Join(home, "data/nib/mods/nibash")); err == nil {
		t.Fatal("a dry run must install nothing")
	}
	options.DryRun = false
	equal(t, Setup(options)[0], "~/data/nib/mods/nibash: installed the nibash mod of this nib")
	folder := filepath.Join(home, "data/nib/mods/nibash")
	for _, file := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.tsx", "types/index.d.ts", "tsconfig.json"} {
		if _, err := os.Stat(filepath.Join(folder, file)); err != nil {
			t.Fatalf("missing %s: %v", file, err)
		}
	}
	equal(t, readJSON(t, filepath.Join(home, "claude/settings.json"))["env"], map[string]any{pluginDirsVariable: "/elsewhere/mod:" + folder})
	// The same mod is left alone, types the engine laid beside it included; a changed one is replaced whole.
	writeFile(t, filepath.Join(folder, ".claude-plugin/types/claude-code/index.d.ts"), "// laid by the engine")
	if report := Setup(options); strings.Contains(strings.Join(report, "\n"), "nibash mod") {
		t.Fatalf("an installed mod must be left alone, got %v", report)
	}
	writeFile(t, filepath.Join(folder, "hooks/register.tsx"), "// older")
	equal(t, Setup(options)[0], "~/data/nib/mods/nibash: installed the nibash mod of this nib")
	if data, _ := os.ReadFile(filepath.Join(folder, "hooks/register.tsx")); string(data) == "// older" {
		t.Fatal("a changed mod must be replaced")
	}
}

func TestQuietSetupSaysOnlyWhatChanged(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, "claude")}
	writeJSON(t, filepath.Join(home, "claude/.claude.json"), map[string]any{"mcpServers": map[string]any{"nib": map[string]any{"type": "stdio", "command": "nib", "args": []any{}}}})
	options := Options{User: true, Quiet: true, Command: nib, Home: home, Env: func(k string) string { return env[k] }, Claude: func(string, []string) (int, string) { return 0, "" }}
	if report := Setup(options); len(report) != 2 || !strings.Contains(report[0], "installed the nibash mod") || !strings.HasSuffix(report[1], "and the nibash mod in env.CLAUDE_CODE_PLUGIN_DIRS") {
		t.Fatalf("a first quiet setup says what it did, got %v", report)
	}
	if report := Setup(options); len(report) != 0 {
		t.Fatalf("a quiet setup with nothing to do says nothing, got %v", report)
	}
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
	equal(t, claudeLines(Setup(options)), []string{
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
