// Package hookstate is what nib's Claude Code hook keeps on disk: its own state, and the agent records it hands to nib's server. Claude Code
// tells only the hook which subagent makes a tool call, and tells the server only the call's tool_use id, so the hook files the agent under
// that id and the server takes it from there. Each kind of record is a private folder of small files named by Claude Code ids.
package hookstate

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RecordAge is how long an agent record waits for its call; a call the user refused or that failed before reaching nib leaves one behind.
const RecordAge = 24 * time.Hour

// Directory is the per-user runtime folder when there is one, else the user's cache; never a folder another user could have made.
func Directory() string {
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		return filepath.Join(runtime, "nib-hook")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "nib", "hook")
}

// Private is true when directory is a real folder of this user's own that no one else may enter; create makes it first when missing.
func Private(directory string, create bool) bool {
	if create {
		_ = os.MkdirAll(directory, 0o700)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// identifier is the shape of Claude Code's tool_use and agent ids; anything else could name a path, and is never filed or looked up.
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// IsID is true for a string of the shape of Claude Code's tool_use and agent ids, which can never name a path.
func IsID(s string) bool { return identifier.MatchString(s) }

// folder is one kind of record: a file per Claude Code id in a private folder under the state folder, each written whole before it appears,
// and dropped once older than RecordAge.
type folder struct{ directory string }

// put files text under name.
func (f folder) put(name, text string, now time.Time) error {
	if !identifier.MatchString(name) {
		return errors.New("not a Claude Code id")
	}
	if !Private(filepath.Dir(f.directory), true) || !Private(f.directory, true) {
		return errors.New("the state folder is not private")
	}
	f.prune(now)
	file, err := os.CreateTemp(f.directory, ".record-*")
	if err != nil {
		return err
	}
	_, err = file.WriteString(text)
	if closed := file.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(f.directory, name))
	}
	if err != nil {
		_ = os.Remove(file.Name())
	}
	return err
}

// get is the text filed under name, ok false when there is none, it is a link or longer than limit; take removes it.
func (f folder) get(name string, limit int, take bool) (text string, ok bool) {
	if !identifier.MatchString(name) || !Private(f.directory, false) {
		return "", false
	}
	path := filepath.Join(f.directory, name)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false
	}
	data := make([]byte, limit+1)
	n, _ := io.ReadFull(file, data)
	file.Close()
	if take {
		_ = os.Remove(path)
	}
	return strings.TrimSpace(string(data[:n])), n <= limit
}

// prune removes records, and temporary files of a hook that died writing one, older than RecordAge.
func (f folder) prune(now time.Time) {
	entries, err := os.ReadDir(f.directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > RecordAge {
			_ = os.Remove(filepath.Join(f.directory, entry.Name()))
		}
	}
}

// Agents are the agent records in one state folder, one file per tool call named by its tool_use id and holding the agent's id, followed for
// an agent whose parent is known by a line "fork" for a fork or "spawn" for any other subagent, with the parent's id after it unless that is
// the main agent, by a line "at" with the spawn moment in Unix nanoseconds when that is known, and by a line "in" with the worktree it was
// spawned in, if any.
type Agents struct{ folder }

// Agent is the agent a call was filed for. Spawned is set when its parent is known, Parent being "" for the main agent, and Forked when it
// is a fork, which starts with its parent's conversation. At is the spawn moment, the first moment its parent's work may be missing from
// what the agent was given; zero when not known. Worktree is the absolute path of the git worktree Claude Code spawned it in, "" for none.
type Agent struct {
	ID       string
	Spawned  bool
	Forked   bool
	Parent   string
	At       time.Time
	Worktree string
}

// OpenAgents are the agent records under state, a folder from Directory.
func OpenAgents(state string) Agents { return Agents{folder{filepath.Join(state, "agents")}} }

// recordLimit bounds what Take reads of a record: two ids, the word spawn, a moment and a path.
const recordLimit = 2*128 + 8 + 32 + 4 + 4096

// IsWorktree is true for a path a record can hold as a worktree: clean, absolute, on one line and at most 4096 bytes.
func IsWorktree(path string) bool {
	return len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\n\r\x00")
}

// Record files agent under toolUse.
func (a Agents) Record(toolUse string, agent Agent, now time.Time) error {
	if !identifier.MatchString(agent.ID) || (agent.Forked && !agent.Spawned) || (agent.Parent != "" && (!agent.Spawned || !identifier.MatchString(agent.Parent))) || (!agent.At.IsZero() && !agent.Spawned) || (agent.Worktree != "" && (!agent.Spawned || !IsWorktree(agent.Worktree))) {
		return errors.New("not a Claude Code id")
	}
	text := agent.ID
	if agent.Spawned {
		kind := "spawn"
		if agent.Forked {
			kind = "fork"
		}
		text += "\n" + strings.TrimSpace(kind+" "+agent.Parent)
		if !agent.At.IsZero() {
			text += "\nat " + strconv.FormatInt(agent.At.UnixNano(), 10)
		}
		if agent.Worktree != "" {
			text += "\nin " + agent.Worktree
		}
	}
	return a.put(toolUse, text, now)
}

// Take is the agent filed under toolUse, removing its record, or the zero Agent when there is none: the main agent's calls, or a hook not
// installed. A parent line it cannot read leaves the agent's parent unknown, a moment it cannot read the spawn moment unknown, and a worktree
// it cannot read the agent in the server's directory.
func (a Agents) Take(toolUse string) Agent {
	text, ok := a.get(toolUse, recordLimit, true)
	lines := strings.Split(text, "\n")
	if !ok || !identifier.MatchString(lines[0]) {
		return Agent{}
	}
	agent := Agent{ID: lines[0]}
	if len(lines) < 2 {
		return agent
	}
	words := strings.Fields(lines[1])
	if len(words) >= 1 && len(words) <= 2 && (words[0] == "fork" || words[0] == "spawn") && (len(words) == 1 || identifier.MatchString(words[1])) {
		agent.Spawned, agent.Forked = true, words[0] == "fork"
		if len(words) == 2 {
			agent.Parent = words[1]
		}
	}
	for _, line := range lines[2:] {
		if !agent.Spawned {
			break
		}
		if at := moment(line, "at "); !at.IsZero() {
			agent.At = at
		} else if path, ok := strings.CutPrefix(line, "in "); ok && IsWorktree(path) {
			agent.Worktree = path
		}
	}
	return agent
}

// moment is the Unix nanoseconds after prefix in text as a time, zero when text is not that.
func moment(text, prefix string) time.Time {
	digits, ok := strings.CutPrefix(text, prefix)
	n, err := strconv.ParseInt(digits, 10, 64)
	if !ok || err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Spawns are the spawn moments the hook found for subagents, one file per agent id, so it reads a parent's transcript once per subagent and
// not on every call the subagent makes.
type Spawns struct{ folder }

// OpenSpawns are the spawn moments under state, a folder from Directory.
func OpenSpawns(state string) Spawns { return Spawns{folder{filepath.Join(state, "spawns")}} }

// Note files the spawn moment of agent, zero when it could not be told, so that is not tried again either.
func (s Spawns) Note(agent string, spawned time.Time, now time.Time) error {
	text := "unknown"
	if !spawned.IsZero() {
		text = strconv.FormatInt(spawned.UnixNano(), 10)
	}
	return s.put(agent, text, now)
}

// Of is the spawn moment filed for agent, zero when it could not be told; filed is false when none is.
func (s Spawns) Of(agent string) (spawned time.Time, filed bool) {
	text, ok := s.get(agent, 32, false)
	if !ok {
		return time.Time{}, false
	}
	return moment(text, ""), true
}

// contextMoments bounds the moments a Contexts file keeps: a fork needs only the latest before its spawn.
const contextMoments = 32

// Contexts are the moments Claude Code dropped what an agent had been shown, one file per agent, oldest first: SessionKey's for a
// session's main agent, whose file SessionStart makes, AgentKey's for a subagent. A session with a file is one whose hooks tell nib when
// that happens; a moment is written as the hook runs, after the compaction or clear it reports.
type Contexts struct{ folder }

// OpenContexts are the context moments under state, a folder from Directory.
func OpenContexts(state string) Contexts { return Contexts{folder{filepath.Join(state, "contexts")}} }

// SessionKey is the Contexts file of a session's main agent.
func SessionKey(session string) string { return "session-" + session }

// AgentKey is the Contexts file of a subagent.
func AgentKey(agent string) string { return "agent-" + agent }

// Drop notes that what the agent of key had been shown was dropped at moment.
func (c Contexts) Drop(key string, moment, now time.Time) error {
	moments, _ := c.Of(key)
	moments = append(moments, moment)
	if len(moments) > contextMoments {
		moments = moments[len(moments)-contextMoments:]
	}
	lines := make([]string, len(moments))
	for i, m := range moments {
		lines[i] = strconv.FormatInt(m.UnixNano(), 10)
	}
	return c.put(key, strings.Join(lines, "\n"), now)
}

// Of are the moments filed for key, oldest first; filed is false when there is no file, so nib cannot tell when that agent's context drops.
func (c Contexts) Of(key string) (moments []time.Time, filed bool) {
	text, ok := c.get(key, contextMoments*21, false)
	if !ok {
		return nil, false
	}
	for _, line := range strings.Split(text, "\n") {
		if m := moment(line, ""); !m.IsZero() {
			moments = append(moments, m)
		}
	}
	return moments, true
}

// Latest is the latest of moments before limit, any when limit is zero; zero when there is none.
func Latest(moments []time.Time, limit time.Time) time.Time {
	var latest time.Time
	for _, m := range moments {
		if (limit.IsZero() || m.Before(limit)) && m.After(latest) {
			latest = m
		}
	}
	return latest
}

// Drops are what the hook knew of a tool call's agent, one file per tool_use id: the latest moment its context was dropped, or "none". A
// call with no file is from an agent whose drops nib cannot tell, as the session's hooks do not report them.
type Drops struct{ folder }

// OpenDrops are the per-call drop records under state, a folder from Directory.
func OpenDrops(state string) Drops { return Drops{folder{filepath.Join(state, "drops")}} }

// Note files the latest moment the context of the agent making toolUse was dropped, zero for never.
func (d Drops) Note(toolUse string, dropped, now time.Time) error {
	text := "none"
	if !dropped.IsZero() {
		text = strconv.FormatInt(dropped.UnixNano(), 10)
	}
	return d.put(toolUse, text, now)
}

// Take is the drop record filed for toolUse, removing it: known is false when there is none, dropped zero when the context never was.
func (d Drops) Take(toolUse string) (dropped time.Time, known bool) {
	text, ok := d.get(toolUse, 32, true)
	if !ok {
		return time.Time{}, false
	}
	if text == "none" {
		return time.Time{}, true
	}
	dropped = moment(text, "")
	return dropped, !dropped.IsZero()
}
