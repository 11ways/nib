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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/11ways/nib/internal/hook"
	"github.com/11ways/nib/internal/providers"
)

// Options configure a setup run; Home, Env and Claude stand in for the real ones in tests.
type Options struct {
	Directory string
	// User registers nib for every project of this user instead of for Directory.
	User    bool
	DryRun  bool
	Command []string
	// Providers are the --provider changes: a program for nib to host under a name, or none to stop hosting it; what an entry hosts
	// already stays.
	Providers []providers.Spec
	Home      string
	Env       func(string) string
	// Claude runs `claude` with arguments in dir ("" for the current one), for the MCP registrations Claude Code's own CLI makes; nil when it
	// is not installed.
	Claude func(dir string, args []string) (int, string)
}

// ClaudeDenied are the built-in tools nib replaces: the shell by nibash, the rest by nib's file tools. Read stays for PDFs and notebooks,
// guarded by the hook, and NotebookEdit stays for notebook cells.
var ClaudeDenied = []string{"Bash", "Grep", "Glob", "Edit", "Write"}

// ClaudeUndenied are deny rules an older setup wrote that setup now removes: Claude Code 2.1 has no MultiEdit, and warns about the rule.
var ClaudeUndenied = []string{"MultiEdit"}

// ClaudeAllowed are nib's tools setup lets run without a prompt: nibash_status only reads what nib runs, and the nibash mod asks it every
// second while something does, where a prompt each time would stop it.
var ClaudeAllowed = []string{"mcp__nib__nibash_status"}

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

// shellLine is words as a shell command line.
func shellLine(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = quote(word)
	}
	return strings.Join(quoted, " ")
}

// program is the command line a local MCP server entry runs, from Claude Code's command and args or OpenCode's command list; local is
// false for a server of another type, such as a remote one.
func program(server object) (words []string, local bool) {
	if kind, _ := server.get("type"); kind != nil && kind != "stdio" && kind != "local" {
		return nil, false
	}
	texts := func(list []any) {
		for _, item := range list {
			text, _ := item.(string)
			words = append(words, text)
		}
	}
	value, _ := server.get("command")
	switch command := value.(type) {
	case string:
		words = append(words, command)
	case []any:
		texts(command)
	}
	args, _ := server.get("args")
	list, _ := args.([]any)
	texts(list)
	return words, true
}

// staleProgram is the command line of a local nib server entry that does not start command: another program or other arguments, or a
// program that is gone, such as Bun running an older nib's main.ts; ok is false for an entry that starts command, by any path to it.
func staleProgram(server object, command []string) (old []string, ok bool) {
	words, local := program(server)
	if !local {
		return nil, false
	}
	return words, differs(words, command)
}

// differs is true when words do not start command, by any path to its program.
func differs(words, command []string) bool {
	if slices.Equal(words, command) {
		return false
	}
	if len(words) != len(command) || !slices.Equal(words[1:], command[1:]) {
		return true
	}
	found, err := resolve(words[0])
	want, wantErr := resolve(command[0])
	return err != nil || wantErr != nil || found != want
}

func resolve(program string) (string, error) {
	path, err := exec.LookPath(program)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func described(words []string) string {
	if len(words) == 0 {
		return "no command"
	}
	return shellLine(words)
}

// repoint points a local nib server entry at command, hosting what it hosts with changes applied, keeping its other keys; list is OpenCode's
// form, a command list without args.
func repoint(server object, command []string, changes []providers.Spec, list bool) []change {
	words, _ := program(server)
	want := wanted(command, words, changes)
	old, ok := staleProgram(server, want)
	if !ok {
		return nil
	}
	if list {
		server.set("command", toAny(want))
	} else {
		server.set("command", want[0])
		server.set("args", toAny(want[1:]))
	}
	return adds(repointed(old, command, want))
}

// repointed says what repointing an entry from old to want does: replace a stale command, or change what it hosts.
func repointed(old, command, want []string) string {
	if rest, _ := split(old); differs(rest, command) {
		return "nib's command, replacing a stale one (" + described(old) + ")"
	}
	return "nib's command, hosting " + hostingText(want) + " instead of " + hostingText(old)
}

// registrations are the servers named name in Claude Code's own file: the user's, nil when there is none, and the local-scope ones by
// project folder.
func registrations(file, name string) (user object, projects map[string]object) {
	projects = map[string]object{}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, projects
	}
	parsed, err := decode(data)
	root, _ := parsed.(object)
	if err != nil || root == nil {
		return nil, projects
	}
	nib := func(holder any) object {
		holderObject, _ := holder.(object)
		if holderObject == nil {
			return nil
		}
		servers, _ := holderObject.get("mcpServers")
		serversObject, _ := servers.(object)
		if serversObject == nil {
			return nil
		}
		server, _ := serversObject.get(name)
		found, _ := server.(object)
		return found
	}
	user = nib(root)
	if folders, _ := root.get("projects"); folders != nil {
		if foldersObject, _ := folders.(object); foldersObject != nil {
			for _, dir := range foldersObject.keys {
				value, _ := foldersObject.get(dir)
				if server := nib(value); server != nil {
					projects[dir] = server
				}
			}
		}
	}
	return user, projects
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

// addServer adds nib to an MCP config's servers, or points a nib entry at command (see repoint), and takes out the entries of the providers
// nib now hosts.
func addServer(json object, key string, command []string, changes []providers.Spec) ([]change, error) {
	servers, err := objectAt(json, key)
	if err != nil {
		return nil, err
	}
	removed := removeHosted(servers, changes)
	if current, ok := servers.get("nib"); ok {
		if server, ok := current.(object); ok {
			return append(repoint(server, command, changes, false), removed...), nil
		}
		return removed, nil
	}
	want := wanted(command, nil, changes)
	servers.set("nib", obj("type", "stdio", "command", want[0], "args", toAny(want[1:])))
	json.set(key, servers)
	return append(adds("the nib MCP server"), removed...), nil
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
	var missing []string
	for _, m := range hook.Matchers {
		if slices.ContainsFunc(nibMatchers, func(matcher string) bool { return matches(matcher, m.Tool) }) {
			continue
		}
		missing = append(missing, m.Matcher)
		entries = append(entries, obj("matcher", m.Matcher, "hooks", []any{obj("type", "command", "command", shellLine(command)+" hook", "timeout", jsonNumber(10))}))
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

// allow adds the allow rules for ClaudeAllowed that are missing.
func allow(json object) ([]change, error) {
	permissions, err := objectAt(json, "permissions")
	if err != nil {
		return nil, err
	}
	rules, err := listAt(permissions, "allow", "permissions.allow")
	if err != nil {
		return nil, err
	}
	var added []string
	for _, tool := range ClaudeAllowed {
		if !slices.Contains(rules, any(tool)) {
			rules = append(rules, tool)
			added = append(added, tool)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	permissions.set("allow", rules)
	json.set("permissions", permissions)
	return adds("allow rules for " + strings.Join(added, ", ")), nil
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
// mode scripts. A nib entry in OpenCode 1's form, as an older setup wrote it, moves there; one that does not start command is pointed at it.
func addOpencodeServer(command []string, changes []providers.Spec) func(object, *[]string) ([]change, error) {
	return func(json object, notes *[]string) ([]change, error) {
		mcp, err := objectAt(json, "mcp")
		if err != nil {
			return nil, err
		}
		servers, err := objectAt(mcp, "servers")
		if err != nil {
			return nil, fmt.Errorf(`its "mcp.servers" is not an object`)
		}
		removed := removeHosted(servers, changes)
		legacy, hasLegacy := mcp.get("nib")
		if current, ok := servers.get("nib"); ok {
			if hasLegacy {
				*notes = append(*notes, "it has nib in both mcp.servers and OpenCode 1's mcp.nib, which OpenCode merges; remove mcp.nib")
			}
			server, ok := current.(object)
			if !ok {
				return removed, nil
			}
			changed := append(repoint(server, command, changes, true), removed...)
			if codemode, set := server.get("codemode"); set {
				if codemode != false {
					*notes = append(*notes, "codemode is set for nib there, so OpenCode offers nib's tools as configured")
				}
				return changed, nil
			}
			server.set("codemode", false)
			return append(changed, adds("code mode off for the nib MCP server")...), nil
		}
		server := obj("type", "local", "command", toAny(wanted(command, nil, changes)), "codemode", false)
		done := adds("the nib MCP server")
		if hasLegacy {
			migrated, ok := migrateServer(legacy)
			if !ok {
				*notes = append(*notes, "its mcp.nib is not a local server in OpenCode 1's form, so it stays as it is")
				return removed, nil
			}
			mcp.remove("nib")
			server = migrated
			done = []change{{what: "the nib MCP server in OpenCode 2's form, outside code mode"}, {what: "its OpenCode 1 form (mcp.nib)", removed: true}}
			done = append(done, repoint(server, command, changes, true)...)
		}
		servers.set("nib", server)
		mcp.set("servers", servers)
		json.set("mcp", mcp)
		return append(done, removed...), nil
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
	tilde := func(file string) string {
		if strings.HasPrefix(file, home+"/") {
			return "~/" + strings.TrimPrefix(file, home+"/")
		}
		return file
	}
	// The providers asked for before are hosted again, so a later setup without --provider, as a script runs it for a new config folder, keeps
	// them; this run's --provider changes what is remembered.
	rememberedPath := rememberedFile(home, env)
	kept, err := remembered(rememberedPath)
	if err != nil {
		report = append(report, fmt.Sprintf("%s: left as it is and not used, since %v", tilde(rememberedPath), err))
	}
	hosting := applied(kept, options.Providers)
	var dropped []providers.Spec
	for _, c := range options.Providers {
		if c.Program == "" {
			dropped = append(dropped, c)
		}
	}
	if len(options.Providers) > 0 && err == nil && !slices.Equal(hosting, kept) {
		verb := "remembers"
		if options.DryRun {
			verb = "would remember"
		}
		line := fmt.Sprintf("%s: %s %s for later setups", tilde(rememberedPath), verb, hostingText(wanted(nil, nil, hosting)))
		if !options.DryRun {
			if err := remember(rememberedPath, hosting); err != nil {
				line = fmt.Sprintf("%s: could not be written: %v", tilde(rememberedPath), err)
			}
		}
		report = append(report, line)
	}
	options.Providers = append(hosting, dropped...)
	display := func(file string) string {
		if !options.User {
			if relative, err := filepath.Rel(options.Directory, file); err == nil && !strings.HasPrefix(relative, "..") {
				return relative
			}
		}
		return tilde(file)
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
	opencodeStep := all(addOpencodeServer(command, options.Providers), denyOpencode)
	hookStep := func(json object, _ *[]string) ([]change, error) { return addHook(json, command) }
	denyStep := func(json object, _ *[]string) ([]change, error) { return deny(json) }
	allowStep := func(json object, _ *[]string) ([]change, error) { return allow(json) }
	// Claude Code keeps user-scope servers, and local-scope ones by project folder, in a file it rewrites while it runs: setup only reads it,
	// and its own CLI changes it.
	registry := filepath.Join(home, ".claude.json")
	if dir := env("CLAUDE_CONFIG_DIR"); dir != "" {
		registry = filepath.Join(dir, ".claude.json")
	}
	user, projects := registrations(registry, "nib")
	claude := options.Claude
	if claude == nil {
		if _, err := exec.LookPath("claude"); err == nil {
			claude = runClaude
		}
	}
	// replace re-registers a nib entry at its scope, pointed at command and hosting what the changes ask, with its other keys (alwaysLoad,
	// env) kept; dir is a local scope's project folder, the one Claude Code's CLI must run in.
	replace := func(scope, dir string, server object) {
		words, _ := program(server)
		want := wanted(command, words, options.Providers)
		old, ok := staleProgram(server, want)
		if !ok {
			return
		}
		where, cd := "for this user", ""
		if dir != "" {
			where, cd = "in "+tilde(dir), "cd "+quote(dir)+" && "
		}
		what := fmt.Sprintf("the stale nib registration %s (%s)", where, described(old))
		if rest, _ := split(old); !differs(rest, command) {
			what = fmt.Sprintf("the nib registration %s, to host %s instead of %s", where, hostingText(want), hostingText(old))
		}
		repoint(server, command, options.Providers, false)
		remove := []string{"mcp", "remove", "--scope", scope, "nib"}
		add := []string{"mcp", "add-json", "--scope", scope, "nib", compact(server)}
		switch {
		case claude == nil:
			report = append(report, fmt.Sprintf("claude is not on PATH; replace %s with: %sclaude %s && claude %s", what, cd, shellLine(remove), shellLine(add)))
		case options.DryRun:
			report = append(report, "claude mcp: would replace "+what)
		default:
			if exit, output := claude(dir, remove); exit != 0 {
				report = append(report, fmt.Sprintf("claude mcp remove failed %s: %s", where, strings.TrimSpace(output)))
			} else if exit, output := claude(dir, add); exit != 0 {
				report = append(report, fmt.Sprintf("claude mcp add-json failed %s after the old registration (%s) was removed, so nib is not registered there now: %s", where, described(old), strings.TrimSpace(output)))
			} else {
				report = append(report, "claude mcp: replaced "+what)
			}
		}
	}
	// A local-scope registration wins over every other in its folder; one for a folder that is gone shadows nothing, and Claude Code's CLI
	// cannot run there to remove it.
	exists := func(dir string) bool {
		info, err := os.Stat(dir)
		return err == nil && info.IsDir()
	}
	replaceLocal := func(dir string) {
		if exists(dir) {
			replace("local", dir, projects[dir])
		}
	}
	// unregister removes a registration of a provider nib now hosts, so Claude Code does not run it beside nib.
	unregister := func(name, scope, dir string) {
		where, cd := "for this user", ""
		if dir != "" {
			where, cd = "in "+tilde(dir), "cd "+quote(dir)+" && "
		}
		remove := []string{"mcp", "remove", "--scope", scope, name}
		what := fmt.Sprintf("the %s registration %s, now hosted by nib", name, where)
		switch {
		case claude == nil:
			report = append(report, fmt.Sprintf("claude is not on PATH; remove %s with: %sclaude %s", what, cd, shellLine(remove)))
		case options.DryRun:
			report = append(report, "claude mcp: would remove "+what)
		default:
			if exit, output := claude(dir, remove); exit != 0 {
				report = append(report, fmt.Sprintf("claude mcp remove failed %s: %s", where, strings.TrimSpace(output)))
			} else {
				report = append(report, "claude mcp: removed "+what)
			}
		}
	}
	var hosted []string
	for _, c := range options.Providers {
		if c.Program != "" && c.Name != "nib" {
			hosted = append(hosted, c.Name)
		}
	}
	if !options.User {
		configure(filepath.Join(options.Directory, ".mcp.json"), func(json object, _ *[]string) ([]change, error) {
			return addServer(json, "mcpServers", command, options.Providers)
		})
		configure(filepath.Join(options.Directory, ".claude", "settings.local.json"), all(approve, hookStep, denyStep, allowStep))
		opencode := filepath.Join(options.Directory, "opencode.json")
		if _, err := os.Stat(opencode); err == nil {
			configure(opencode, opencodeStep)
		}
		if projects[options.Directory] != nil {
			replaceLocal(options.Directory)
		}
		for _, name := range hosted {
			userServer, local := registrations(registry, name)
			if local[options.Directory] != nil && exists(options.Directory) {
				unregister(name, "local", options.Directory)
			}
			if userServer != nil {
				report = append(report, fmt.Sprintf("claude mcp: %s is also registered for this user, so Claude Code runs it beside nib here; nib setup --user --provider ... removes it", name))
			}
		}
		return report
	}
	config := env("CLAUDE_CONFIG_DIR")
	if config == "" {
		config = filepath.Join(home, ".claude")
	}
	// The nibash mod goes where every config folder's sessions load it from; the env block of a user settings file names it to Claude Code.
	mod := modFolder(home, env)
	steps := all(hookStep, denyStep, allowStep)
	switch installed, err := installMod(mod, options.DryRun); {
	case err != nil:
		report = append(report, fmt.Sprintf("%s: the nibash mod could not be installed: %v", tilde(mod), err))
	default:
		if installed {
			verb := "installed"
			if options.DryRun {
				verb = "would install"
			}
			report = append(report, fmt.Sprintf("%s: %s the nibash mod of this nib", tilde(mod), verb))
		}
		steps = all(hookStep, denyStep, allowStep, func(json object, _ *[]string) ([]change, error) { return addPluginDir(json, mod) })
	}
	configure(filepath.Join(config, "settings.json"), steps)
	switch {
	case user != nil:
		userWords, _ := program(user)
		if _, stale := staleProgram(user, wanted(command, userWords, options.Providers)); stale {
			replace("user", "", user)
		} else {
			report = append(report, "claude mcp: nib is already registered for this user")
		}
	case claude == nil:
		report = append(report, "claude is not on PATH; register the server with: claude mcp add --scope user nib -- "+shellLine(wanted(command, nil, options.Providers)))
	case options.DryRun:
		report = append(report, "claude mcp: would add nib for this user")
	default:
		exit, output := claude("", append([]string{"mcp", "add", "--scope", "user", "nib", "--"}, wanted(command, nil, options.Providers)...))
		if exit == 0 {
			report = append(report, "claude mcp: added nib for this user")
		} else {
			report = append(report, "claude mcp add failed: "+strings.TrimSpace(output))
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(projects)) {
		replaceLocal(dir)
	}
	for _, name := range hosted {
		userServer, local := registrations(registry, name)
		if userServer != nil {
			unregister(name, "user", "")
		}
		for _, dir := range slices.Sorted(maps.Keys(local)) {
			if exists(dir) {
				unregister(name, "local", dir)
			}
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

// compact is a value as one line of JSON, its keys in order.
func compact(value any) string {
	var b bytes.Buffer
	_ = json.Compact(&b, []byte(encode(value)))
	return b.String()
}

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

func runClaude(dir string, args []string) (int, string) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
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

var usage = `Usage: nib setup [--user] [--dry-run] [--provider NAME=PROGRAM]...
Registers nib in this folder: the MCP server in .mcp.json; its approval, its Claude Code hook and deny rules for the built-in tools nib replaces
(` + strings.Join(ClaudeDenied, ", ") + `) and an allow rule for ` + strings.Join(ClaudeAllowed, ", ") + `, which the nibash mod polls, in .claude/settings.local.json; and the server, in OpenCode 2's form with OpenCode's own tools denied, in opencode.json
when there is one. With --user, for every project of this user instead. Only what is missing is added; deny rules an older setup wrote for
tools Claude Code no longer has (` + strings.Join(ClaudeUndenied, ", ") + `) are removed.
--provider NAME=PROGRAM makes nib host PROGRAM's tools as NAME_TOOL (see nib --help) and removes the NAME server entries and registrations
it replaces, in this folder or, with --user, for this user; NAME= stops hosting it. The providers an entry hosts already stay, and the
providers asked for are remembered in ~/.config/nib/setup.json, so a later setup without --provider (a new project, a new config folder)
hosts them too. With --user it also installs the nibash mod this nib carries to ~/.local/share/nib/mods/nibash and names that folder in
env.CLAUDE_CODE_PLUGIN_DIRS of the config folder's settings.json.`

// Main is `nib setup` from the command line; it returns the exit code.
func Main(args []string) int {
	user, dryRun := false, false
	var changes []providers.Spec
	directory, _ := os.Getwd()
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value, isProvider := strings.CutPrefix(arg, providerFlag+"=")
		if arg == providerFlag && i+1 < len(args) {
			i++
			value, isProvider = args[i], true
		}
		switch {
		case isProvider:
			change, err := ParseChange(value)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usage)
				return 2
			}
			// A path relative to here would mean another file to a nib started elsewhere; a bare name is looked up on PATH when nib starts.
			if strings.Contains(change.Program, "/") && !filepath.IsAbs(change.Program) {
				change.Program = filepath.Join(directory, change.Program)
			}
			changes = append(changes, change)
		case arg == "--user":
			user = true
		case arg == "--dry-run":
			dryRun = true
		case arg == "--help" || arg == "-h":
			fmt.Println(usage)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "Unknown option %s\n%s\n", arg, usage)
			return 2
		}
	}
	for _, line := range Setup(Options{Directory: directory, User: user, DryRun: dryRun, Providers: changes}) {
		fmt.Println(line)
	}
	return 0
}
