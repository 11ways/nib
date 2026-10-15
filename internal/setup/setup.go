// Package setup registers nib where Claude Code and OpenCode look for it, and turns off the built-in tools nib replaces.
package setup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/11ways/nib/internal/hook"
)

// Options configure a setup run; Home, Env and Claude stand in for the real ones in tests.
type Options struct {
	Directory string
	// User registers nib for every project of this user instead of for Directory.
	User    bool
	DryRun  bool
	Command []string
	Home    string
	Env     func(string) string
	// Claude runs `claude` with arguments, for the user-scope MCP registration Claude Code's own CLI makes; nil when it is not installed.
	Claude func(args []string) (int, string)
}

// ClaudeDenied are the built-in tools nib replaces: the shell by nibash, the rest by nib's file tools. Read stays for PDFs and notebooks,
// guarded by the hook, and NotebookEdit stays for notebook cells.
var ClaudeDenied = []string{"Bash", "Grep", "Glob", "Edit", "Write"}

// ClaudeUndenied are deny rules an older setup wrote that setup now removes: Claude Code 2.1 has no MultiEdit, and warns about the rule.
var ClaudeUndenied = []string{"MultiEdit"}

// OpencodeDenied are OpenCode 2's built-in tools nib replaces, by permission action: edit also covers writing and patching, and shell is
// nibash's, which setup's nib server always offers. OpenCode 2 has no list tool.
var OpencodeDenied = []string{"shell", "read", "grep", "glob", "edit"}

// opencodeAction is the OpenCode 2 action an OpenCode 1 permission or tools key names, as OpenCode 2 migrates it.
func opencodeAction(key string) string {
	switch key {
	case "write", "patch":
		return "edit"
	case "bash":
		return "shell"
	case "task":
		return "subagent"
	}
	return key
}

// Command is how to start this copy of nib: the bare nib when the one on PATH is this copy, else its absolute path.
func Command() []string {
	self, err := os.Executable()
	if err != nil {
		return []string{"nib"}
	}
	self, _ = filepath.EvalSymlinks(self)
	if found, err := exec.LookPath("nib"); err == nil {
		if real, err := filepath.EvalSymlinks(found); err == nil && real == self {
			return []string{"nib"}
		}
	}
	return []string{self}
}

var plainWord = regexp.MustCompile(`^[\w./@:+-]+$`)

func quote(word string) string {
	if plainWord.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

var shellWords = regexp.MustCompile(`'((?:[^']|'\\'')*)'|(\S+)`)

// words splits a hook command as a shell splits plain and single-quoted words, the forms setup writes.
func words(command string) []string {
	var out []string
	for _, match := range shellWords.FindAllStringSubmatch(command, -1) {
		if match[2] != "" {
			out = append(out, match[2])
		} else {
			out = append(out, strings.ReplaceAll(match[1], `'\''`, "'"))
		}
	}
	return out
}

// isNibHook is true for a hook command that runs nib's hook: `nib hook` by name or path, or Bun running the main.ts of an older nib install.
func isNibHook(command string) bool {
	parts := words(command)
	if len(parts) == 0 || parts[len(parts)-1] != "hook" {
		return false
	}
	if len(parts) == 2 {
		return filepath.Base(parts[0]) == "nib"
	}
	if len(parts) != 3 || filepath.Base(parts[1]) != "main.ts" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(parts[1]), "..", "package.json"))
	if err != nil {
		return false
	}
	var manifest struct{ Name string }
	return json.Unmarshal(data, &manifest) == nil && manifest.Name == "@11ways/nib"
}

// change is one thing a step does to a config: something added, or with removed, something an older setup wrote that setup now takes out.
type change struct {
	what    string
	removed bool
}

func adds(what string) []change { return []change{{what: what}} }

type object = *ordered

// obj builds an ordered object from key, value pairs.
func obj(pairs ...any) object {
	o := newOrdered()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.set(pairs[i].(string), pairs[i+1])
	}
	return o
}

func objectAt(json object, key string) (object, error) {
	value, ok := json.get(key)
	if !ok || value == nil {
		return newOrdered(), nil
	}
	found, ok := value.(object)
	if !ok {
		return nil, fmt.Errorf(`its "%s" is not an object`, key)
	}
	return found, nil
}

func listAt(json object, key, where string) ([]any, error) {
	value, ok := json.get(key)
	if !ok || value == nil {
		return []any{}, nil
	}
	found, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf(`its "%s" is not a list`, where)
	}
	return found, nil
}

// addServer adds nib to an MCP config's servers unless one named nib is there.
func addServer(json object, key string, server object) ([]change, error) {
	servers, err := objectAt(json, key)
	if err != nil {
		return nil, err
	}
	if _, ok := servers.get("nib"); ok {
		return nil, nil
	}
	servers.set("nib", server)
	json.set(key, servers)
	return adds("the nib MCP server"), nil
}

// plainMatcher is a PreToolUse matcher Claude Code (2.1.301) compares as names separated by | or commas, rather than as a regular expression.
var plainMatcher = regexp.MustCompile(`^[A-Za-z0-9_|, -]+$`)

// matches is true when a hook matcher selects tool: "" and * select every tool, plain names select themselves, and anything else is a
// regular expression, taken here as matching the whole name, so a doubtful one counts as not covering the tool and setup adds its own.
func matches(matcher, tool string) bool {
	switch {
	case matcher == "" || matcher == "*":
		return true
	case plainMatcher.MatchString(matcher):
		for _, name := range strings.FieldsFunc(matcher, func(r rune) bool { return r == '|' || r == ',' }) {
			if strings.TrimSpace(name) == tool {
				return true
			}
		}
		return false
	}
	pattern, err := regexp.Compile("^(?:" + matcher + ")$")
	return err == nil && pattern.MatchString(tool)
}

// addHook registers nib's PreToolUse hook for each of hook.Matchers that no nib hook covers yet, whichever matcher an older setup gave it.
func addHook(json object, command []string) ([]change, error) {
	hooks, err := objectAt(json, "hooks")
	if err != nil {
		return nil, err
	}
	entries, err := listAt(hooks, "PreToolUse", "hooks.PreToolUse")
	if err != nil {
		return nil, err
	}
	var nibMatchers []string
	for _, entry := range entries {
		e, ok := entry.(object)
		if !ok {
			continue
		}
		value, _ := e.get("hooks")
		list, _ := value.([]any)
		for _, h := range list {
			if h, ok := h.(object); ok {
				value, _ := h.get("command")
				if c, ok := value.(string); ok && isNibHook(c) {
					matcher, _ := e.get("matcher")
					text, _ := matcher.(string)
					nibMatchers = append(nibMatchers, text)
				}
			}
		}
	}
	quoted := make([]string, len(command))
	for i, word := range command {
		quoted[i] = quote(word)
	}
	var missing []string
	for _, m := range hook.Matchers {
		if slices.ContainsFunc(nibMatchers, func(matcher string) bool { return matches(matcher, m.Tool) }) {
			continue
		}
		missing = append(missing, m.Matcher)
		entries = append(entries, obj("matcher", m.Matcher, "hooks", []any{obj("type", "command", "command", strings.Join(quoted, " ")+" hook", "timeout", jsonNumber(10))}))
	}
	if len(missing) == 0 {
		return nil, nil
	}
	hooks.set("PreToolUse", entries)
	json.set("hooks", hooks)
	if len(missing) < len(hook.Matchers) {
		return adds("nib's PreToolUse hook for " + strings.Join(missing, ", ")), nil
	}
	return adds("nib's PreToolUse hook"), nil
}

// approve approves the project's nib server so Claude Code does not ask; one the user disabled stays disabled.
func approve(json object, notes *[]string) ([]change, error) {
	value, _ := json.get("disabledMcpjsonServers")
	if disabled, ok := value.([]any); ok {
		for _, name := range disabled {
			if name == "nib" {
				*notes = append(*notes, "nib is in disabledMcpjsonServers there, so it stays off until you remove it")
				return nil, nil
			}
		}
	}
	enabled, err := listAt(json, "enabledMcpjsonServers", "enabledMcpjsonServers")
	if err != nil {
		return nil, err
	}
	allValue, _ := json.get("enableAllProjectMcpServers")
	if all, _ := allValue.(bool); all {
		return nil, nil
	}
	for _, name := range enabled {
		if name == "nib" {
			return nil, nil
		}
	}
	json.set("enabledMcpjsonServers", append(enabled, "nib"))
	return adds("approval of the nib server in .mcp.json"), nil
}

// deny adds Claude Code deny rules for the built-in tools nib replaces, where a bare tool name removes the tool, also in bypass mode, and
// removes the rules of ClaudeUndenied.
func deny(json object) ([]change, error) {
	permissions, err := objectAt(json, "permissions")
	if err != nil {
		return nil, err
	}
	rules, err := listAt(permissions, "deny", "permissions.deny")
	if err != nil {
		return nil, err
	}
	var added, removed []string
	rules = slices.DeleteFunc(rules, func(rule any) bool {
		name, _ := rule.(string)
		if slices.Contains(ClaudeUndenied, name) {
			removed = append(removed, name)
			return true
		}
		return false
	})
	for _, tool := range ClaudeDenied {
		if !slices.Contains(rules, any(tool)) {
			rules = append(rules, tool)
			added = append(added, tool)
		}
	}
	var changes []change
	if len(added) > 0 {
		changes = append(changes, change{what: "deny rules for " + strings.Join(added, ", ")})
	}
	if len(removed) > 0 {
		changes = append(changes, change{what: "the deny rule for " + strings.Join(removed, ", ") + ", a tool Claude Code no longer has", removed: true})
	}
	if len(changes) > 0 {
		permissions.set("deny", rules)
		json.set("permissions", permissions)
	}
	return changes, nil
}

// denyOpencode appends deny rules for OpenCode's tools nib replaces to its permission rules, where the last matching rule wins and a tool
// denied outright is not offered at all. An action any rule names already, in OpenCode 2's permissions or OpenCode 1's permission or tools,
// which OpenCode 2 still reads, stays as configured.
func denyOpencode(json object, notes *[]string) ([]change, error) {
	rules, err := listAt(json, "permissions", "permissions")
	if err != nil {
		*notes = append(*notes, `its "permissions" is not a list, so OpenCode's own tools stay on`)
		return nil, nil
	}
	// denied is, for each action named anywhere, whether every rule naming it denies it everywhere.
	denied := map[string]bool{}
	name := func(action string, deny bool) {
		if previous, seen := denied[action]; seen {
			deny = deny && previous
		}
		denied[action] = deny
	}
	for _, rule := range rules {
		if rule, ok := rule.(object); ok {
			action, _ := rule.get("action")
			resource, _ := rule.get("resource")
			effect, _ := rule.get("effect")
			if action, ok := action.(string); ok {
				name(action, resource == "*" && effect == "deny")
			}
		}
	}
	if permission, ok := json.get("permission"); ok {
		if permission, ok := permission.(object); ok {
			for _, key := range permission.keys {
				value, _ := permission.get(key)
				name(opencodeAction(key), value == "deny")
			}
		}
	}
	if tools, ok := json.get("tools"); ok {
		if tools, ok := tools.(object); ok {
			for _, key := range tools.keys {
				value, _ := tools.get(key)
				name(opencodeAction(key), value == false)
			}
		}
	}
	var added, kept []string
	for _, action := range OpencodeDenied {
		if deny, seen := denied[action]; seen {
			if !deny {
				kept = append(kept, action)
			}
			continue
		}
		rules = append(rules, obj("action", action, "resource", "*", "effect", "deny"))
		added = append(added, action)
	}
	if len(kept) > 0 {
		*notes = append(*notes, fmt.Sprintf("permissions for %s set there already, so OpenCode keeps those tools as configured", strings.Join(kept, ", ")))
	}
	if len(added) == 0 {
		return nil, nil
	}
	json.set("permissions", rules)
	return adds("deny rules for " + strings.Join(added, ", ")), nil
}

// addOpencodeServer adds nib to OpenCode 2's MCP servers, outside code mode, where OpenCode would otherwise offer its tools only inside code
// mode scripts. A nib entry in OpenCode 1's form, as an older setup wrote it, moves there.
func addOpencodeServer(command []string) func(object, *[]string) ([]change, error) {
	return func(json object, notes *[]string) ([]change, error) {
		mcp, err := objectAt(json, "mcp")
		if err != nil {
			return nil, err
		}
		servers, err := objectAt(mcp, "servers")
		if err != nil {
			return nil, fmt.Errorf(`its "mcp.servers" is not an object`)
		}
		legacy, hasLegacy := mcp.get("nib")
		if current, ok := servers.get("nib"); ok {
			if hasLegacy {
				*notes = append(*notes, "it has nib in both mcp.servers and OpenCode 1's mcp.nib, which OpenCode merges; remove mcp.nib")
			}
			server, ok := current.(object)
			if !ok {
				return nil, nil
			}
			if codemode, set := server.get("codemode"); set {
				if codemode != false {
					*notes = append(*notes, "codemode is set for nib there, so OpenCode offers nib's tools as configured")
				}
				return nil, nil
			}
			server.set("codemode", false)
			return adds("code mode off for the nib MCP server"), nil
		}
		server := obj("type", "local", "command", toAny(command), "codemode", false)
		changes := adds("the nib MCP server")
		if hasLegacy {
			migrated, ok := migrateServer(legacy)
			if !ok {
				*notes = append(*notes, "its mcp.nib is not a local server in OpenCode 1's form, so it stays as it is")
				return nil, nil
			}
			mcp.remove("nib")
			server = migrated
			changes = []change{{what: "the nib MCP server in OpenCode 2's form, outside code mode"}, {what: "its OpenCode 1 form (mcp.nib)", removed: true}}
		}
		servers.set("nib", server)
		mcp.set("servers", servers)
		json.set("mcp", mcp)
		return changes, nil
	}
}

// migrateServer is an OpenCode 1 local MCP server in OpenCode 2's form, outside code mode, the way OpenCode 2 migrates one; ok is false for
// anything else, or a key OpenCode 1 did not have.
func migrateServer(value any) (object, bool) {
	old, ok := value.(object)
	if !ok {
		return nil, false
	}
	if kind, _ := old.get("type"); kind != "local" {
		return nil, false
	}
	if command, _ := old.get("command"); command == nil {
		return nil, false
	}
	server := newOrdered()
	for _, key := range old.keys {
		value, _ := old.get(key)
		switch key {
		case "type", "command", "cwd", "environment":
			server.set(key, value)
		case "enabled":
			if value == false {
				server.set("disabled", true)
			}
		case "timeout":
			server.set("timeout", obj("catalog", value, "execution", value))
		default:
			return nil, false
		}
	}
	server.set("codemode", false)
	return server, true
}

// Setup adds only what is missing, so a setting someone changed always wins; it returns one line per file: what changed, or why nothing did.
func Setup(options Options) []string {
	command := options.Command
	if command == nil {
		command = Command()
	}
	home := options.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	env := options.Env
	if env == nil {
		env = os.Getenv
	}
	var report []string
	display := func(file string) string {
		if !options.User {
			if relative, err := filepath.Rel(options.Directory, file); err == nil && !strings.HasPrefix(relative, "..") {
				return relative
			}
		}
		if strings.HasPrefix(file, home+"/") {
			return "~/" + strings.TrimPrefix(file, home+"/")
		}
		return file
	}
	configure := func(file string, update func(json object, notes *[]string) ([]change, error)) {
		// A link is followed, so a config kept elsewhere, as in a dotfiles repository, stays a link to it.
		real, err := filepath.EvalSymlinks(file)
		if err != nil {
			real = file
		}
		data, err := os.ReadFile(real)
		existed := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Only a file that is not there is a fresh start; one that cannot be read must never be replaced by a new one.
			code := "an unknown error"
			var errno syscall.Errno
			if errors.As(err, &errno) {
				code = errnoName(errno)
			}
			report = append(report, fmt.Sprintf("%s: left untouched, since it cannot be read (%s)", display(file), code))
			return
		}
		json := newOrdered()
		if existed && len(bytes.TrimSpace(data)) > 0 {
			parsed, err := decode(data)
			if err != nil {
				report = append(report, fmt.Sprintf("%s: left untouched, since it is not plain JSON (comments?)", display(file)))
				return
			}
			found, ok := parsed.(object)
			if !ok {
				report = append(report, fmt.Sprintf("%s: left untouched, since it does not hold a JSON object", display(file)))
				return
			}
			json = found
		}
		var notes []string
		changes, err := update(json, &notes)
		if err != nil {
			report = append(report, fmt.Sprintf("%s: left untouched, since %v", display(file), err))
			return
		}
		extra := ""
		if len(notes) > 0 {
			extra = " (" + strings.Join(notes, "; ") + ")"
		}
		if len(changes) == 0 {
			report = append(report, fmt.Sprintf("%s: already set up%s", display(file), extra))
			return
		}
		var added, removed []string
		for _, c := range changes {
			if c.removed {
				removed = append(removed, c.what)
			} else {
				added = append(added, c.what)
			}
		}
		add, remove := "added", "removed"
		if options.DryRun {
			add, remove = "would add", "would remove"
		}
		var done []string
		if len(added) > 0 {
			done = append(done, add+" "+strings.Join(added, " and "))
		}
		if len(removed) > 0 {
			done = append(done, remove+" "+strings.Join(removed, " and "))
		}
		report = append(report, fmt.Sprintf("%s: %s%s", display(file), strings.Join(done, "; "), extra))
		if options.DryRun {
			return
		}
		if err := writeConfig(real, json, existed); err != nil {
			report[len(report)-1] = fmt.Sprintf("%s: could not be written: %v", display(file), err)
		}
	}
	all := func(steps ...func(object, *[]string) ([]change, error)) func(object, *[]string) ([]change, error) {
		return func(json object, notes *[]string) ([]change, error) {
			var changes []change
			for _, step := range steps {
				more, err := step(json, notes)
				if err != nil {
					return nil, err
				}
				changes = append(changes, more...)
			}
			return changes, nil
		}
	}
	opencodeStep := all(addOpencodeServer(command), denyOpencode)
	hookStep := func(json object, _ *[]string) ([]change, error) { return addHook(json, command) }
	denyStep := func(json object, _ *[]string) ([]change, error) { return deny(json) }
	if !options.User {
		configure(filepath.Join(options.Directory, ".mcp.json"), func(json object, _ *[]string) ([]change, error) {
			return addServer(json, "mcpServers", obj("type", "stdio", "command", command[0], "args", toAny(command[1:])))
		})
		configure(filepath.Join(options.Directory, ".claude", "settings.local.json"), all(approve, hookStep, denyStep))
		opencode := filepath.Join(options.Directory, "opencode.json")
		if _, err := os.Stat(opencode); err == nil {
			configure(opencode, opencodeStep)
		}
		return report
	}
	config := env("CLAUDE_CONFIG_DIR")
	if config == "" {
		config = filepath.Join(home, ".claude")
	}
	configure(filepath.Join(config, "settings.json"), all(hookStep, denyStep))
	// Claude Code keeps user-scope servers in a file it rewrites while it runs: setup only reads it to see whether nib is there, and its own CLI adds it.
	registry := filepath.Join(home, ".claude.json")
	if dir := env("CLAUDE_CONFIG_DIR"); dir != "" {
		registry = filepath.Join(dir, ".claude.json")
	}
	registered := false
	if data, err := os.ReadFile(registry); err == nil {
		var parsed struct {
			McpServers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(data, &parsed) == nil {
			_, registered = parsed.McpServers["nib"]
		}
	}
	claude := options.Claude
	if claude == nil {
		if _, err := exec.LookPath("claude"); err == nil {
			claude = runClaude
		}
	}
	quoted := make([]string, len(command))
	for i, word := range command {
		quoted[i] = quote(word)
	}
	switch {
	case registered:
		report = append(report, "claude mcp: nib is already registered for this user")
	case claude == nil:
		report = append(report, "claude is not on PATH; register the server with: claude mcp add --scope user nib -- "+strings.Join(quoted, " "))
	case options.DryRun:
		report = append(report, "claude mcp: would add nib for this user")
	default:
		exit, output := claude(append([]string{"mcp", "add", "--scope", "user", "nib", "--"}, command...))
		if exit == 0 {
			report = append(report, "claude mcp: added nib for this user")
		} else {
			report = append(report, "claude mcp add failed: "+strings.TrimSpace(output))
		}
	}
	// OpenCode 2 reads its user config from OPENCODE_CONFIG_DIR when set, else from its folder in the XDG config home.
	folder := env("OPENCODE_CONFIG_DIR")
	if folder == "" {
		xdg := env("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		folder = filepath.Join(xdg, "opencode")
	}
	opencode := filepath.Join(folder, "opencode.json")
	if _, err := os.Stat(opencode); err == nil {
		configure(opencode, opencodeStep)
	}
	return report
}

func jsonNumber(n int) any { return json.Number(fmt.Sprint(n)) }

func toAny(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func errnoName(errno syscall.Errno) string {
	names := map[syscall.Errno]string{syscall.EACCES: "EACCES", syscall.EPERM: "EPERM", syscall.EISDIR: "EISDIR", syscall.ELOOP: "ELOOP", syscall.EIO: "EIO"}
	if name, ok := names[errno]; ok {
		return name
	}
	return errno.Error()
}

// writeConfig writes json beside file under a name no one else holds and renames it over the file, keeping its mode, so a config holding
// credentials stays private.
func writeConfig(file string, json object, existed bool) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		return err
	}
	mode := os.FileMode(0o666 &^ umask())
	if existed {
		info, err := os.Stat(file)
		if err != nil {
			return err
		}
		mode = info.Mode().Perm() | info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	}
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	staged := filepath.Join(filepath.Dir(file), "."+filepath.Base(file)+".nib-"+hex.EncodeToString(suffix))
	handle, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = handle.WriteString(encode(json) + "\n")
	if err == nil {
		err = handle.Sync()
	}
	handle.Close()
	if err == nil {
		err = os.Chmod(staged, mode)
	}
	if err == nil {
		err = os.Rename(staged, file)
	}
	if err != nil {
		os.Remove(staged)
	}
	return err
}

func umask() int {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return mask
}

func runClaude(args []string) (int, string) {
	cmd := exec.Command("claude", args...)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(output)
	}
	if err != nil {
		return 1, err.Error()
	}
	return 0, string(output)
}

var usage = `Usage: nib setup [--user] [--dry-run]
Registers nib in this folder: the MCP server in .mcp.json; its approval, its Claude Code hook and deny rules for the built-in tools nib replaces
(` + strings.Join(ClaudeDenied, ", ") + `) in .claude/settings.local.json; and the server, in OpenCode 2's form with OpenCode's own tools denied, in opencode.json
when there is one. With --user, for every project of this user instead. Only what is missing is added; deny rules an older setup wrote for
tools Claude Code no longer has (` + strings.Join(ClaudeUndenied, ", ") + `) are removed.`

// Main is `nib setup` from the command line; it returns the exit code.
func Main(args []string) int {
	user, dryRun := false, false
	for _, arg := range args {
		switch arg {
		case "--user":
			user = true
		case "--dry-run":
			dryRun = true
		case "--help", "-h":
			fmt.Println(usage)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "Unknown option %s\n%s\n", arg, usage)
			return 2
		}
	}
	directory, _ := os.Getwd()
	for _, line := range Setup(Options{Directory: directory, User: user, DryRun: dryRun}) {
		fmt.Println(line)
	}
	return 0
}
