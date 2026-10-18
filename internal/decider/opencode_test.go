package decider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeOpenCode writes an opencode program that answers --version with version and otherwise records its arguments, environment, folder
// and input, writes a file into the data folder it was given as OpenCode would, prints events and exits with status. Every --version call
// appends a line to RECORD.versions.
func fakeOpenCode(t *testing.T, version, events string, status int) (program, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	program = filepath.Join(dir, "opencode")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = --version ]; then echo x >> %[1]s.versions; printf '%%s\n' '%[2]s'; exit 0; fi
printf '%%s\n' "$@" > %[1]s.args
env > %[1]s.env
pwd > %[1]s.dir
cat > %[1]s.input
mkdir -p "$XDG_DATA_HOME/opencode" && echo session > "$XDG_DATA_HOME/opencode/opencode.db"
printf '%%s' '%[3]s'
exit %[4]d
`, record, version, strings.ReplaceAll(events, "'", `'\''`), status)
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return program, record
}

// answerEvents are the events `opencode run --format json` prints for a reply, in two text parts split before its ranges, the second
// streamed in two updates.
func answerEvents(reply string) string {
	half := max(strings.Index(reply, `"ranges"`), 0)
	lines := []map[string]any{
		{"type": "step_start", "part": map[string]any{"id": "p0", "type": "step-start"}},
		{"type": "text", "part": map[string]any{"id": "p1", "type": "text", "text": reply[:half]}},
		{"type": "text", "part": map[string]any{"id": "p2", "type": "text", "text": "partial"}},
		{"type": "text", "part": map[string]any{"id": "p2", "type": "text", "text": reply[half:]}},
		{"type": "step_finish", "part": map[string]any{"id": "p3", "type": "step-finish", "cost": 0.002,
			"tokens": map[string]any{"input": 40, "output": 9, "cache": map[string]any{"read": 2}}}},
	}
	var out strings.Builder
	for _, line := range lines {
		data, _ := json.Marshal(line)
		out.Write(data)
		out.WriteString("\n")
	}
	return out.String()
}

// userData is a stand-in for the user's own OpenCode data folder with an auth.json: an API key for deepseek and an OAuth login for openai.
func userData(t *testing.T) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "opencode")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := `{"deepseek": {"type": "api", "key": "sk-test"}, "openai": {"type": "oauth", "refresh": "r", "access": "a", "expires": 1}}`
	if err := os.WriteFile(filepath.Join(data, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func openCodeSummarizer(t *testing.T, program, list, data string, timeout time.Duration) Summarizer {
	t.Helper()
	entries, err := parseSummary(list)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Summary: entries, SummaryTimeout: timeout, OpenCode: program, logins: logins{auth: filepath.Join(data, "auth.json")}}.Summarizer()
}

// listing is every file under dir, for comparing a folder before and after.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, _ := os.ReadFile(path)
			files = append(files, path+"="+string(data))
		}
		return nil
	})
	return files
}

func TestOpenCodeEntriesParseAndNeedNoOpenRouterKey(t *testing.T) {
	fakeClaude(t, "", 0)
	entries, err := parseSummary("opencode:deepseek/deepseek-flash#high, claude-p:haiku")
	if err != nil {
		t.Fatal(err)
	}
	want := []SummaryEntry{
		{Spec: "opencode:deepseek/deepseek-flash#high", Program: "opencode", Model: "deepseek/deepseek-flash#high", Reasoning: "off"},
		{Spec: "claude-p:haiku", Program: "claude-p", Model: "haiku", Reasoning: "off"},
	}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries %+v", entries)
	}
	for _, bad := range []string{"opencode:haiku", "opencode:", "opencode:a/b@reasoning=low"} {
		if _, err := parseSummary(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	// opencode is looked for only when an entry runs, so a machine without it still starts.
	t.Setenv("PATH", t.TempDir())
	c, err := Parse(env(map[string]string{"NIB_DECIDER_SUMMARY": "opencode:a/b"}))
	if err != nil || c.Key != "" || c.OpenCode != "opencode" {
		t.Fatalf("default program: %+v %v", c, err)
	}
	c, err = Parse(env(map[string]string{"NIB_DECIDER_SUMMARY": "opencode:a/b", "NIB_DECIDER_OPENCODE": "/opt/oc2/opencode", "HOME": "/h"}))
	if err != nil || c.OpenCode != "/opt/oc2/opencode" || c.logins.auth != "/h/.local/share/opencode/auth.json" ||
		!slices.Contains(c.logins.databases, "/h/.local/share/opencode/opencode.db") {
		t.Fatalf("named program: %+v %v", c, err)
	}
}

func TestOpenCodeRunsStandaloneInAFolderOfItsOwnWithTheKeyAndNoTools(t *testing.T) {
	data := userData(t)
	before := listing(t, filepath.Dir(data))
	program, record := fakeOpenCode(t, "opencode v2.0.30", answerEvents(`{"summary": "It hangs.", "ranges": [[2, 2]]}`), 0)
	// The user's own OpenCode settings in the environment must not reach the run.
	t.Setenv("OPENCODE_CONFIG", "/user/opencode.json")
	t.Setenv("XDG_DATA_HOME", filepath.Dir(data))
	s := openCodeSummarizer(t, program, "opencode:deepseek/deepseek-flash", data, 5*time.Second)
	in := SummaryInput{Command: "serve", TimedOut: true, Log: []string{"start", "waiting on lock", "tick"}, Excerpt: []int{1, 2, 3}}
	got := s.Summarize(context.Background(), in)
	if got.Failure != "" || got.Who != "opencode:deepseek/deepseek-flash" || got.Text != "It hangs." || got.Usage.InputTokens != 42 ||
		got.Usage.Cost != 0.002 {
		t.Fatalf("summary %+v", got)
	}
	args, _ := os.ReadFile(record + ".args")
	if string(args) != "run\n--standalone\n--agent\nnib-summary\n--model\ndeepseek/deepseek-flash\n--format\njson\n" {
		t.Fatalf("args %q", args)
	}
	input, _ := os.ReadFile(record + ".input")
	if string(input) != "1: start\n2: waiting on lock\n3: tick\n" {
		t.Fatalf("input %q", input)
	}
	dir, _ := os.ReadFile(record + ".dir")
	home := filepath.Dir(strings.TrimSpace(string(dir)))
	if !strings.Contains(home, "nib-summary-") {
		t.Fatalf("ran in %q", dir)
	}
	vars := map[string]string{}
	environ, _ := os.ReadFile(record + ".env")
	for _, line := range strings.Split(string(environ), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			vars[name] = value
		}
	}
	for name, want := range map[string]string{"HOME": home, "XDG_DATA_HOME": home + "/data", "XDG_CONFIG_HOME": home + "/config",
		"XDG_STATE_HOME": home + "/state", "XDG_CACHE_HOME": home + "/cache", "TMPDIR": home + "/tmp", "OPENCODE_DB": ":memory:",
		"OPENCODE_DISABLE_PROJECT_CONFIG": "1", "OPENCODE_CONFIG": ""} {
		if vars[name] != want {
			t.Errorf("%s=%q, want %q", name, vars[name], want)
		}
	}
	var config struct {
		Agent    map[string]map[string]any `json:"agent"`
		Provider map[string]struct {
			Options map[string]string `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(vars["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatalf("config %q: %v", vars["OPENCODE_CONFIG_CONTENT"], err)
	}
	agent := config.Agent["nib-summary"]
	if agent["mode"] != "primary" || !strings.Contains(agent["prompt"].(string), "Output: 3 lines") ||
		fmt.Sprint(agent["permission"]) != "map[*:deny]" || config.Provider["deepseek"].Options["apiKey"] != "sk-test" || len(config.Provider) != 1 {
		t.Fatalf("config %+v", config)
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("the folder opencode ran in is left behind")
	}
	if after := listing(t, filepath.Dir(data)); !slices.Equal(before, after) {
		t.Fatalf("the user's data changed:\n%v\n%v", before, after)
	}
	// The version is asked once per program.
	s.Summarize(context.Background(), SummaryInput{Log: []string{"other"}, Excerpt: []int{1}})
	if versions, _ := os.ReadFile(record + ".versions"); string(versions) != "x\n" {
		t.Fatalf("version asked %q", versions)
	}
}

func TestAnOpenCodeEntryThatCannotRunFailsSoTheNextAnswers(t *testing.T) {
	data := userData(t)
	in := SummaryInput{Command: "build", Exit: 1, Log: []string{"compiling", "error: boom"}, Excerpt: []int{1, 2}}
	for _, c := range []struct {
		name, version, events, list, failure string
		status                               int
	}{
		{"version one", "1.18.35", "", "opencode:deepseek/x", `is "1.18.35"; opencode: entries need OpenCode 2`, 0},
		{"error event", "opencode v2.0.30", `{"type":"error","error":{"type":"provider.auth","message":"key ****"}}` + "\n", "opencode:deepseek/x",
			"opencode reported provider.auth", 1},
		{"no answer", "opencode v2.0.30", "Usage: opencode run\n", "opencode:deepseek/x", "opencode printed no answer", 0},
		{"exit", "opencode v2.0.30", "", "opencode:deepseek/x", "opencode exited with status 2", 2},
		{"oauth", "opencode v2.0.30", answerEvents(`{"summary": "s", "ranges": [[2, 2]]}`), "opencode:openai/gpt-6",
			"the openai login is oauth; opencode: entries take a provider with an API key", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			program, record := fakeOpenCode(t, c.version, c.events, c.status)
			got := openCodeSummarizer(t, program, c.list, data, 5*time.Second).Summarize(context.Background(), in)
			if !strings.Contains(got.Failure, c.failure) || got.Text != "" {
				t.Fatalf("failure %q", got.Failure)
			}
			if c.name == "oauth" {
				if _, err := os.Stat(record + ".args"); err == nil {
					t.Error("an OAuth login still ran opencode")
				}
			}
		})
	}
	got := openCodeSummarizer(t, "/nonexistent/opencode", "opencode:deepseek/x", data, 5*time.Second).Summarize(context.Background(), in)
	if got.Failure != "opencode:deepseek/x: no OpenCode program /nonexistent/opencode" {
		t.Fatalf("missing: %q", got.Failure)
	}
	// The next summarizer answers when an opencode entry cannot run.
	fakeClaude(t, `{"summary": "Boom.", "ranges": [[2, 2]]}`, 0)
	program, _ := fakeOpenCode(t, "opencode 1.18.35", "", 0)
	got = openCodeSummarizer(t, program, "opencode:deepseek/x,claude-p:haiku", data, 5*time.Second).Summarize(context.Background(), in)
	if got.Failure != "" || got.Who != "claude-p:haiku" || got.Text != "Boom." {
		t.Fatalf("fallback %+v", got)
	}
}

func TestAnOpenCodeRunPastItsTimeoutIsStopped(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'opencode v2.0.30'; exit 0; fi\nsleep 30 &\nwait\n"
	if err := os.WriteFile(program, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	got := openCodeSummarizer(t, program, "opencode:deepseek/x", userData(t), 300*time.Millisecond).Summarize(context.Background(),
		SummaryInput{Log: []string{"a"}, Excerpt: []int{1}})
	if got.Failure != "opencode:deepseek/x: timeout after 300ms" || time.Since(started) > 5*time.Second {
		t.Fatalf("after %s: %+v", time.Since(started), got)
	}
}

func TestLoginsReadEveryKindOfAuthJSONEntry(t *testing.T) {
	data := t.TempDir()
	auth := filepath.Join(data, "auth.json")
	content := `{"deepseek": {"type": "api", "key": " sk-file "}, "gitlab": {"type": "wellknown", "key": "k", "token": "tok"},
		"openai": {"type": "oauth", "refresh": "r"}, "odd": {"type": "api"}}`
	if err := os.WriteFile(auth, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	l := logins{auth: auth}
	for provider, want := range map[string]string{"deepseek": "key sk-file", "gitlab": "key tok", "openai": "oauth ", "odd": "", "mistral": ""} {
		login, found := l.of(provider)
		if got := login.Kind + " " + login.Key; found != (want != "") || found && got != want {
			t.Errorf("%s: %q %v, want %q", provider, got, found, want)
		}
	}
	// No auth.json and no database is no login.
	if _, found := (logins{}).of("deepseek"); found {
		t.Error("a login from nowhere")
	}
}
