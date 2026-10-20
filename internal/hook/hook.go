// Package hook is nib's Claude Code hook. With nibash owning the shell and setup denying the other built-in file tools, only the built-in
// Read is left, which stays for PDFs and notebooks: its PreToolUse hook refuses it for any other file, naming the exact nib read to send
// instead. For nib's own tools it only reports: a subagent's call is filed under its tool_use id, with the agent that spawned it, when,
// whether it is a fork and the worktree it was spawned in, so the server keeps that agent's state apart, works in its worktree, and starts it
// with what it inherits: a fork its parent's seen lines, every subagent the named directories its parent had defined when it was spawned.
// SessionStart and PostCompact note when an agent's context was dropped, and every nib call is filed with the latest such moment, so the
// server stops counting lines the agent was shown before it.
package hook

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/platform"
)

// Matcher is a hook registration: the event, the matcher setup writes, and what it must select for the hook to be registered for its job (a
// tool name, or for the other events a trigger or source).
type Matcher struct{ Event, Matcher, Selects string }

// Matchers are the hook's registrations: the built-in Read it guards, nib's own tools, whose calls it files for the server, the events
// that drop an agent's context: every session start (a new conversation, /clear, a resume, or a compaction) and every compaction, whose
// matchers select by trigger, and every message the user sends, whose named directories it tells the agent of.
var Matchers = []Matcher{
	{"PreToolUse", "Read", "Read"},
	{"PreToolUse", "mcp__nib__.*", "mcp__nib__read"},
	{"SessionStart", "", "clear"},
	{"PostCompact", "", "auto"},
	{"UserPromptSubmit", "", "prompt"},
}

// taggedLimit is the engine's text limit: past it nib reads a file only plain.
const taggedLimit = 2 * 1024 * 1024

// Input is the part of Claude Code's hook input the hook uses; AgentID and AgentType are set only for a subagent's call, Prompt only for a
// message the user sent.
type Input struct {
	SessionID      string         `json:"session_id"`
	TranscriptPath string         `json:"transcript_path"`
	Cwd            string         `json:"cwd"`
	HookEventName  string         `json:"hook_event_name"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	ToolUseID      string         `json:"tool_use_id"`
	AgentID        string         `json:"agent_id"`
	AgentType      string         `json:"agent_type"`
	Prompt         string         `json:"prompt"`
}

// Ask asks the session's nib server what the main agent must be told of mentioned, the named directories a message the user sent uses,
// after its context was last dropped at dropped (zero for never): lines ~name = directory, none when no server answered.
type Ask func(mentioned []string, dropped time.Time) []string

// metaLimit bounds the read of a subagent's meta file, a few hundred bytes in Claude Code 2.1.
const metaLimit = 64 * 1024

// origin is where a subagent comes from: its parent, "" for the main agent, the tool_use id of the parent's Agent call that spawned it,
// whether it is a fork, and the worktree Claude Code spawned it in, "" for none.
type origin struct {
	parent, call string
	fork         bool
	worktree     string
}

// originOf is where the subagent making a call comes from; ok is false when its parent is not known. Hook input does not name it: Claude Code
// keeps it in the meta file next to the subagent's transcript, which is not a documented interface, so anything unexpected there leaves the
// subagent on its own.
func originOf(input Input) (found origin, ok bool) {
	if !hookstate.IsID(input.AgentID) || !filepath.IsAbs(input.TranscriptPath) || !strings.HasSuffix(input.TranscriptPath, ".jsonl") {
		return origin{}, false
	}
	file, err := os.Open(filepath.Join(strings.TrimSuffix(input.TranscriptPath, ".jsonl"), "subagents", "agent-"+input.AgentID+".meta.json"))
	if err != nil {
		return origin{}, false
	}
	defer file.Close()
	var meta struct {
		IsFork              bool   `json:"isFork"`
		ParentAgentID       string `json:"parentAgentId"`
		SpawnDepth          int    `json:"spawnDepth"`
		ToolUseID           string `json:"toolUseId"`
		SpawnedWithWorktree bool   `json:"spawnedWithWorktree"`
		WorktreePath        string `json:"worktreePath"`
	}
	if json.NewDecoder(io.LimitReader(file, metaLimit)).Decode(&meta) != nil || meta.IsFork != (input.AgentType == "fork") {
		return origin{}, false
	}
	if !hookstate.IsID(meta.ToolUseID) {
		meta.ToolUseID = ""
	}
	// A worktree that is not a directory now is one nib cannot work in; the agent then stays in the server's directory.
	worktree := ""
	if info, err := os.Stat(meta.WorktreePath); meta.SpawnedWithWorktree && hookstate.IsWorktree(meta.WorktreePath) && err == nil && info.IsDir() {
		worktree = meta.WorktreePath
	}
	// A subagent of the main agent is spawned at depth 1 and names no parent; a deeper one must name its parent.
	switch {
	case meta.SpawnDepth == 1 && meta.ParentAgentID == "":
		return origin{"", meta.ToolUseID, meta.IsFork, worktree}, true
	case meta.SpawnDepth > 1 && hookstate.IsID(meta.ParentAgentID):
		return origin{meta.ParentAgentID, meta.ToolUseID, meta.IsFork, worktree}, true
	}
	return origin{}, false
}

// spawnedAt is the spawn moment of the subagent making a call, as the hook filed it on the subagent's first call, else read from its
// parent's transcript and filed; zero when it cannot be told.
func spawnedAt(input Input, found origin, state string, now time.Time) time.Time {
	spawns := hookstate.OpenSpawns(state)
	if at, filed := spawns.Of(input.AgentID); filed {
		return at
	}
	transcript := input.TranscriptPath
	if found.parent != "" {
		transcript = filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents", "agent-"+found.parent+".jsonl")
	}
	at := spawnMoment(transcript, found.call)
	_ = spawns.Note(input.AgentID, at, now)
	return at
}

// spawnMoment is the first moment the parent's work may be missing from what a subagent its Agent call call spawned was given, read from the
// parent's transcript; zero when it cannot be told. A fork's context is the parent's conversation up to the entry before the assistant
// message holding that call, so no result of a call in that message or later is in it, parallel calls included. Every entry written before
// that one was stamped before the request that produced the message, and every call outside the context started after that request, so the
// moment is one millisecond past the latest stamp up to that entry: Claude Code stamps entries in whole milliseconds, and nib stamps seen
// lines and named directories before the result that shows them is stamped. The transcript is not a documented interface: anything
// unexpected gives zero.
func spawnMoment(transcript, call string) time.Time {
	if call == "" {
		return time.Time{}
	}
	file, err := os.Open(transcript)
	if err != nil {
		return time.Time{}
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	// latest is the latest stamp up to each entry; first is the entry before each assistant message's first entry. A line that is not an
	// entry is skipped: it can only make the moment earlier, which leaves the fork fewer lines, never more.
	latest, first := map[string]time.Time{}, map[string]string{}
	var running time.Time
	for {
		line, err := reader.ReadBytes('\n')
		var entry struct {
			Type       string          `json:"type"`
			UUID       string          `json:"uuid"`
			ParentUUID string          `json:"parentUuid"`
			Timestamp  string          `json:"timestamp"`
			Message    json.RawMessage `json:"message"`
		}
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &entry) == nil {
			if stamp, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil && stamp.After(running) {
				running = stamp
			}
			if entry.UUID != "" {
				latest[entry.UUID] = running
			}
			var message struct {
				ID      string `json:"id"`
				Content []struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"content"`
			}
			if entry.Type == "assistant" && json.Unmarshal(entry.Message, &message) == nil && message.ID != "" {
				if _, ok := first[message.ID]; !ok {
					first[message.ID] = entry.ParentUUID
				}
				for _, block := range message.Content {
					if block.Type != "tool_use" || block.ID != call {
						continue
					}
					at, ok := latest[first[message.ID]]
					if !ok || at.IsZero() {
						return time.Time{}
					}
					return at.Truncate(time.Millisecond).Add(time.Millisecond)
				}
			}
		}
		if err != nil {
			return time.Time{}
		}
	}
}

// edited are the file types an agent reads to change them, so a read of one keeps its numbered rows.
var edited = map[string]bool{}

func init() {
	for _, ext := range strings.Fields("c h cpp cc cxx hpp hh css go html htm java js jsx mjs cjs json kt kts md markdown py pyi rs sh bash toml ts tsx mts cts yaml yml zig xml sql rb php swift vue svelte scss sass less gradle properties ini cfg conf hwk") {
		edited[ext] = true
	}
}

// readOnly are the files only the built-in Read can show.
var readOnly = regexp.MustCompile(`(?i)\.(pdf|ipynb)$`)

var logName = regexp.MustCompile(`(?i)\.(log|out|err)(\.\d+)?$`)

// lookOnly is true for a file read only to look at, which a plain read shows without numbers or tag: a log or output file by its name, a file
// in a log folder that is not code or config, or one too big to tag.
func lookOnly(path string) bool {
	if logName.MatchString(path) {
		return true
	}
	if !edited[strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))] {
		for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
			if segment == "log" || segment == "logs" {
				return true
			}
		}
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() > taggedLimit
}

// shown is a path as the agent should pass it to nib: relative to the session's folder when inside it, with slashes as nib prints paths.
func shown(path, session string) string {
	relative, err := filepath.Rel(session, path)
	switch {
	case err != nil:
		return filepath.ToSlash(path)
	case relative == ".":
		return "."
	case relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)):
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func whole(value any) (int, bool) {
	n, ok := value.(float64)
	return int(n), ok && n >= 0 && n == float64(int(n))
}

// Decide is the refusal reason for a tool call, or "" to let it run: only Read of a file nib reads is refused.
func Decide(input Input) string {
	if input.ToolName != "Read" {
		return ""
	}
	session := input.Cwd
	if session == "" {
		session, _ = os.Getwd()
	}
	file, _ := input.ToolInput["file_path"].(string)
	if file == "" || readOnly.MatchString(file) {
		return ""
	}
	full := file
	if !filepath.IsAbs(full) {
		full = filepath.Join(session, file)
	}
	where := shown(full, session)
	offset, hasOffset := whole(input.ToolInput["offset"])
	limit, hasLimit := whole(input.ToolInput["limit"])
	hasOffset = hasOffset && offset > 0
	hasLimit = hasLimit && limit > 0
	selector := ""
	switch {
	case hasOffset && hasLimit:
		selector = fmt.Sprintf(":%d+%d", offset, limit)
	case hasLimit:
		selector = fmt.Sprintf(":1-%d", limit)
	case hasOffset:
		selector = fmt.Sprintf(":%d-", offset)
	}
	args := map[string]any{"paths": []string{where + selector}}
	if lookOnly(full) {
		args["plain"] = true
	}
	return strings.Join([]string{
		"nib's hook refused Read: nib replaces it with fewer tokens. Use instead:",
		"mcp__nib__read " + marshal(args),
		"(If you need Read itself here, or nib's tools are missing from this session, send the same call again and it runs.)",
	}, "\n")
}

// marshal is JSON.stringify: no escaping of <, > and &, which a path may hold.
func marshal(value any) string {
	var b strings.Builder
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(b.String(), "\n")
}

// identity is what makes two calls the same for the resend rule: the tool and its arguments in key order.
func identity(input Input) string {
	encoded, _ := json.Marshal(input.ToolInput)
	return input.ToolName + "\x00" + string(encoded)
}

var unsafeName = regexp.MustCompile(`[^\w-]`)

// firstTime is true the first time this session makes this call; a resend means the agent has no nib, or a reason to insist. A state folder
// that is not this user's own and private, or a state file that is a link, turns refusals off instead of being trusted.
func firstTime(session, call, directory string) bool {
	if !hookstate.Private(directory, true) {
		return false
	}
	name := unsafeName.ReplaceAllString(session, "_")
	if name == "" {
		name = "session"
	}
	path := filepath.Join(directory, name+".txt")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return false
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|platform.NoFollow, 0o600)
	if err != nil {
		return false
	}
	defer file.Close()
	sum := sha256.Sum256([]byte(call))
	digest := hex.EncodeToString(sum[:])[:16]
	data, err := os.ReadFile(file.Name())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == digest {
			return false
		}
	}
	_, err = file.WriteString(digest + "\n")
	return err == nil
}

// Run reads one hook call and returns Claude Code's deny decision when nib should take the call, or "" to let it run. An MCP tool's call is
// never refused; a subagent's is filed under its tool_use id, with its parent, spawn moment and whether it is a fork when those are known,
// and a failure to file it only leaves that call to the main agent's state. A session start or compaction is noted for the agent it drops
// the context of. A message the user sent gets, as context for the agent, the named directories it uses that ask says to tell.
func Run(stdin []byte, state string, ask Ask) string {
	var input Input
	if json.Unmarshal(stdin, &input) != nil {
		return ""
	}
	if state == "" {
		state = hookstate.Directory()
	}
	now := time.Now()
	switch input.HookEventName {
	case "SessionStart", "PostCompact":
		if key, ok := contextKey(input.SessionID, input.AgentID); ok {
			_ = hookstate.OpenContexts(state).Drop(key, now, now)
		}
		return ""
	case "UserPromptSubmit":
		return prompted(input, state, ask)
	}
	if strings.HasPrefix(input.ToolName, "mcp__") {
		var found origin
		agent := hookstate.Agent{ID: input.AgentID}
		if input.AgentID != "" {
			var ok bool
			if found, ok = originOf(input); ok {
				agent.Spawned, agent.Forked, agent.Parent, agent.At, agent.Worktree = true, found.fork, found.parent, spawnedAt(input, found, state, now), found.worktree
			}
			_ = hookstate.OpenAgents(state).Record(input.ToolUseID, agent, now)
		}
		if dropped, known := droppedAt(input.SessionID, agent, state); known {
			_ = hookstate.OpenDrops(state).Note(input.ToolUseID, dropped, input.SessionID, now)
		}
		return ""
	}
	reason := Decide(input)
	if reason == "" || !firstTime(input.SessionID, identity(input), state) {
		return ""
	}
	encoded, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": reason,
	}})
	return string(encoded)
}

// contextKey is the Contexts file of the agent a hook call comes from: its subagent's, else its session's main agent's.
func contextKey(session, agent string) (string, bool) {
	key := hookstate.SessionKey(session)
	if agent != "" {
		key = hookstate.AgentKey(agent)
	}
	return key, session != "" && hookstate.IsID(session) && (agent == "" || hookstate.IsID(agent)) && hookstate.IsID(key)
}

// prompted is the context a message the user sent gets: the lines ask gives for the named directories it uses, for the session's main agent,
// whose context was dropped when the session's hooks filed it.
func prompted(input Input, state string, ask Ask) string {
	mentioned := names.Mentioned(input.Prompt)
	if ask == nil || len(mentioned) == 0 {
		return ""
	}
	dropped, _ := droppedAt(input.SessionID, hookstate.Agent{}, state)
	lines := ask(mentioned, dropped)
	if len(lines) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "UserPromptSubmit",
		"additionalContext": "nib's named directories in this message, which work in every nib tool and in nibash:\n" + strings.Join(lines, "\n"),
	}})
	return string(encoded)
}

// droppedAt is the latest moment what agent had been shown was dropped, zero for never; known is false when the session's hooks do not report
// it. A fork starts with its parent's conversation as it was at the spawn, so the parent's latest drop before then counts too; without a
// spawn moment, its latest at all, which can only drop more.
func droppedAt(session string, agent hookstate.Agent, state string) (dropped time.Time, known bool) {
	contexts := hookstate.OpenContexts(state)
	main, ok := contextKey(session, "")
	if !ok {
		return time.Time{}, false
	}
	moments, filed := contexts.Of(main)
	if !filed {
		return time.Time{}, false
	}
	if agent.ID == "" {
		return hookstate.Latest(moments, time.Time{}), true
	}
	own, ok := contextKey(session, agent.ID)
	if !ok {
		return time.Time{}, false
	}
	mine, _ := contexts.Of(own)
	dropped = hookstate.Latest(mine, time.Time{})
	if agent.Forked {
		parent := moments
		if agent.Parent != "" {
			key, ok := contextKey(session, agent.Parent)
			if !ok {
				return time.Time{}, false
			}
			parent, _ = contexts.Of(key)
		}
		if inherited := hookstate.Latest(parent, agent.At); inherited.After(dropped) {
			dropped = inherited
		}
	}
	return dropped, true
}
