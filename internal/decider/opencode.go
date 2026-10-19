package decider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/11ways/nib/internal/opencode"
	"github.com/11ways/nib/internal/platform"
)

// logins are where an opencode: summarizer finds a provider's login, read only: OpenCode 2's databases, then the auth.json older versions
// kept, which OpenCode 2 imports into its database.
type logins struct {
	databases []string
	auth      string
}

// of is provider's login; found is false when neither place has one.
func (l logins) of(provider string) (login opencode.Login, found bool) {
	if login, found = opencode.Credential(l.databases, provider); found {
		return login, true
	}
	if l.auth == "" {
		return opencode.Login{}, false
	}
	return readOpencodeLogin(l.auth, provider)
}

// openCodeEvent is one line of `opencode run --format json`, read leniently.
type openCodeEvent struct {
	Type string `json:"type"`
	Part struct {
		ID     string  `json:"id"`
		Text   string  `json:"text"`
		Cost   float64 `json:"cost"`
		Tokens struct {
			Input int `json:"input"`
			Cache struct {
				Read int `json:"read"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Type string `json:"type"`
	} `json:"error"`
}

// opencode runs OpenCode 2's `run` as a standalone server of its own, in a folder that holds its data, config, state, cache and temporary
// files and is removed after, with its database in memory, so the user's sessions, settings, MCP servers and plugins are neither read nor
// written. Its one agent carries the prompt and is denied every permission, which leaves it no tools. The provider's API key, read from the
// user's logins, goes in through the config it is given; an OAuth login is refused, since a run that refreshed it could spend the user's
// refresh token. A provider with no login runs as it is, for OpenCode's own free models and keys in the environment.
func (s *summaries) opencode(ctx context.Context, e SummaryEntry, prompt, text string, usage *Usage) (string, string) {
	provider, _, _ := strings.Cut(e.Model, "/")
	config := map[string]any{"agent": map[string]any{openCodeAgent: map[string]any{"mode": "primary", "prompt": prompt,
		"permission": map[string]string{"*": "deny"}}}}
	if login, found := s.config.logins.of(provider); found {
		if login.Kind != "key" {
			return "", fmt.Sprintf("the %s login is %s; opencode: entries take a provider with an API key, since a run could refresh another kind",
				provider, login.Kind)
		}
		config["provider"] = map[string]any{provider: map[string]any{"options": map[string]string{"apiKey": login.Key}}}
	}
	content, err := json.Marshal(config)
	if err != nil {
		return "", "no config for opencode"
	}
	dir, err := os.MkdirTemp("", "nib-summary-")
	if err != nil {
		return "", "no directory to run opencode in"
	}
	defer os.RemoveAll(dir)
	for _, sub := range []string{"data", "config", "state", "cache", "tmp", "work"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return "", "no directory to run opencode in"
		}
	}
	env := openCodeEnv(os.Environ(), dir, string(content))
	if failure := s.openCodeIsTwo(ctx, env, dir); failure != "" {
		return "", failure
	}
	cmd := exec.CommandContext(ctx, s.config.OpenCode, "run", "--standalone", "--agent", openCodeAgent, "--model", e.Model, "--format", "json")
	cmd.Dir = filepath.Join(dir, "work")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(text)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	usage.Requests++
	runErr := runGroup(cmd)
	reply, reported := readOpenCode(stdout.Bytes(), usage)
	switch {
	case reported != "":
		return "", "opencode reported " + reported
	case runErr != nil && ctx.Err() != nil:
		return "", s.ended(ctx, "")
	case runErr != nil:
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return "", fmt.Sprintf("opencode exited with status %d", exit.ExitCode())
		}
		return "", "opencode did not run"
	case reply == "":
		return "", "opencode printed no answer"
	}
	return reply, ""
}

// openCodeIsTwo is "" once program has said it is OpenCode 2, else why the entry cannot run; a program that said so is not asked again.
func (s *summaries) openCodeIsTwo(ctx context.Context, env []string, dir string) string {
	program := s.config.OpenCode
	if _, ok := s.opencodes.Load(program); ok {
		return ""
	}
	cmd := exec.CommandContext(ctx, program, "--version")
	cmd.Dir = filepath.Join(dir, "work")
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := runGroup(cmd); err != nil {
		if ctx.Err() != nil {
			return s.ended(ctx, "")
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return "no OpenCode program " + program
		}
		return program + " --version failed"
	}
	version := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(version, "opencode v2.") {
		if version == "" {
			version = "no version"
		}
		return fmt.Sprintf("%s is %q; opencode: entries need OpenCode 2", program, version)
	}
	s.opencodes.Store(program, true)
	return ""
}

// openCodeEnv is the user's environment without OpenCode's own variables or any that place its files, which all go to dir instead; the
// config it is given is the only one it reads.
func openCodeEnv(environ []string, dir, config string) []string {
	var env []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || name == "TMPDIR" || strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "OPENCODE_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+dir, "TMPDIR="+filepath.Join(dir, "tmp"), "XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"), "OPENCODE_DB=:memory:", "OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_CLAUDE_CODE=1", "OPENCODE_CONFIG_CONTENT="+config)
}

// readOpenCode is the answer in run's JSON events, its text parts joined in order, and what they cost; reported is the type of an error
// event, "" without one.
func readOpenCode(out []byte, usage *Usage) (reply, reported string) {
	var order []string
	texts := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event openCodeEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event.Type {
		case "error":
			if reported = event.Error.Type; reported == "" {
				reported = "an error"
			}
			return "", reported
		case "text":
			if _, seen := texts[event.Part.ID]; !seen {
				order = append(order, event.Part.ID)
			}
			texts[event.Part.ID] = event.Part.Text
		case "step_finish":
			usage.InputTokens += event.Part.Tokens.Input + event.Part.Tokens.Cache.Read
			usage.Cost += event.Part.Cost
		}
	}
	var parts []string
	for _, id := range order {
		parts = append(parts, texts[id])
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), ""
}

// runGroup runs cmd in a process group of its own, killed whole when its context ends, so a server it started cannot keep the call
// waiting.
func runGroup(cmd *exec.Cmd) error {
	platform.Prepare(cmd)
	cmd.Cancel = func() error { return platform.SignalGroup(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := platform.Join(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	defer platform.Release(cmd.Process.Pid)
	return cmd.Wait()
}
