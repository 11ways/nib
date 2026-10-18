package server

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/trash"
)

// Views are the lists nibash_status gives a client's display besides what nibash runs; each is asked for by name.
var views = []string{"trash", "seen", "names", "tokens"}

// TrashView is what this server moved to the trash that is still there, oldest first.
type TrashView struct {
	Entries []TrashEntry `json:"entries"`
}

// TrashEntry is one path in the trash; times are Unix milliseconds.
type TrashEntry struct {
	// Path is the absolute path it had, which restore takes, and Shown how it reads from the server's directory.
	Path  string `json:"path"`
	Shown string `json:"shown"`
	When  int64  `json:"when"`
	Dir   bool   `json:"dir,omitempty"`
}

// Restored is the answer to a restore: whether the path is back, and what happened in a line.
type Restored struct {
	Path    string `json:"path"`
	Done    bool   `json:"done"`
	Message string `json:"message"`
}

// shown is path as it reads from directory when under it, else the path itself.
func shown(directory, path string) string {
	if rel, err := filepath.Rel(directory, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return rel
	}
	return path
}

// trashView lists what the server's trash can holds, the entries of every agent, as nibash's trash list does.
func (s *Server) trashView(ctx *engine.Context) *TrashView {
	view := &TrashView{Entries: []TrashEntry{}}
	for _, e := range s.engine.Trash.Entries() {
		view.Entries = append(view.Entries, TrashEntry{Path: e.Original, Shown: shown(ctx.Directory, e.Original), When: e.When.UnixMilli(), Dir: e.Dir})
	}
	return view
}

// restore puts a trashed path back through the same trash code as nibash's trash restore, refusing, as it does, a path where a file is
// now; it never replaces one.
func (s *Server) restore(ctx *engine.Context, path string) *Restored {
	got := &Restored{Path: path}
	fail := func(format string, a ...any) *Restored {
		got.Message = fmt.Sprintf(format, a...)
		return got
	}
	if !filepath.IsAbs(path) {
		return fail("%s is not an absolute path", path)
	}
	if ctx.Sandbox {
		if why, err := ctx.Refused(filepath.Dir(path)); err != nil || why != "" {
			if err != nil {
				why = err.Error()
			}
			return fail("cannot restore %s: %s", shown(ctx.Directory, path), why)
		}
	}
	_, err := s.engine.Trash.Restore(path, false, ctx.Sandbox)
	switch {
	case errors.Is(err, trash.ErrExists):
		return fail("cannot restore %s: a file is there now", shown(ctx.Directory, path))
	case errors.Is(err, trash.ErrNotTrashed):
		return fail("cannot restore %s: this session did not move it to the trash, or it left the trash since", shown(ctx.Directory, path))
	case err != nil:
		return fail("cannot restore %s: %v", shown(ctx.Directory, path), err)
	}
	got.Done, got.Message = true, "restored "+shown(ctx.Directory, path)
	return got
}

// when is a Unix millisecond moment as the local time of day.
func when(ms int64) string { return time.UnixMilli(ms).Format("15:04:05") }

// isHosted says whether tool is one a hosted provider offers, named after the provider.
func (s *Server) isHosted(tool string) bool {
	for _, p := range s.options.Providers {
		if strings.HasPrefix(tool, p.Name+"_") {
			return true
		}
	}
	return false
}

// viewLines are the lists a status carries as the model reads them, one row each, for a client that shows only the text.
func viewLines(status ShellStatus) []string {
	var lines []string
	if r := status.Restore; r != nil {
		lines = append(lines, "restore: "+r.Message)
	}
	if t := status.Trash; t != nil {
		lines = append(lines, fmt.Sprintf("trash: %d entries", len(t.Entries)))
		for _, e := range t.Entries {
			lines = append(lines, fmt.Sprintf("  | %s  %s", when(e.When), e.Shown))
		}
	}
	if t := status.Tokens; t != nil {
		lines = append(lines, fmt.Sprintf("tokens: %d characters returned by %d calls", t.Chars, t.Calls))
		for _, a := range t.Agents {
			lines = append(lines, fmt.Sprintf("  | [%s] %d calls, %d characters", a.Agent, a.Calls, a.Chars))
			for _, tool := range a.Tools {
				lines = append(lines, fmt.Sprintf("  |   %s: %d calls, %d characters", tool.Tool, tool.Calls, tool.Chars))
			}
		}
	}
	if v := status.Seen; v != nil {
		for _, a := range v.Agents {
			lines = append(lines, fmt.Sprintf("seen by [%s]: %d files", a.Agent, len(a.Files)+a.More))
			for _, f := range a.Files {
				lines = append(lines, fmt.Sprintf("  | %s#%s %d lines, %d ranges seen", f.Path, f.Tag, f.Lines, len(f.Seen)+f.MoreRanges))
			}
		}
	}
	if n := status.Names; n != nil {
		for _, a := range n.Agents {
			lines = append(lines, fmt.Sprintf("names of [%s]: %d", a.Agent, len(a.Names)))
			for _, name := range a.Names {
				lines = append(lines, fmt.Sprintf("  | ~%s = %s (%s, %d paths)", name.Name, name.Directory, name.Source, name.Uses))
			}
		}
	}
	return lines
}

// NamesView is the named directories each agent that called sees.
type NamesView struct {
	Agents []AgentNames `json:"agents"`
}

// AgentNames are one agent's named directories, nib's own first.
type AgentNames struct {
	Agent string    `json:"agent"`
	Names []NameRow `json:"names"`
}

// NameRow is one named directory.
type NameRow struct {
	Name string `json:"name"`
	// Directory reads from the server's directory when under it.
	Directory string `json:"directory"`
	// Source is server (nib.json, detected source roots, names kept from earlier sessions), defined (by the agent with hash -d),
	// automatic (made by nib) or inherited (from the agent that started this one).
	Source string `json:"source"`
	// Uses counts the paths nib printed under the name.
	Uses int `json:"uses"`
}

// namesView lists the named directories of every agent that called, as each of them sees them.
func (s *Server) namesView(ctx *engine.Context, u *usageLog) *NamesView {
	view := &NamesView{Agents: []AgentNames{}}
	for _, agent := range u.agentList() {
		row := AgentNames{Agent: agent, Names: []NameRow{}}
		for _, d := range s.options.Names.For(s.defined, s.sessionOf(agent)).Describe() {
			row.Names = append(row.Names, NameRow{Name: d.Name, Directory: shown(ctx.Directory, d.Directory), Source: d.Source, Uses: d.Uses})
		}
		view.Agents = append(view.Agents, row)
	}
	return view
}

// sessionOf is the engine session of an agent shown as label: "main" is the client's own.
func (s *Server) sessionOf(label string) string {
	if label == "main" {
		return s.engineSession("")
	}
	return s.engineSession(label)
}

// seenFiles bounds the files listed per agent, and seenRanges the ranges per file.
const (
	seenFiles  = 40
	seenRanges = 24
)

// SeenView is what nib counts each agent as having seen of its files: the lines an edit may anchor on.
type SeenView struct {
	Agents []AgentSeen `json:"agents"`
}

// AgentSeen are one agent's files, the most recently used first.
type AgentSeen struct {
	Agent string    `json:"agent"`
	Files []SeenRow `json:"files"`
	// More counts the files past the ones listed.
	More int `json:"more,omitempty"`
}

// SeenRow is one file as an agent holds it.
type SeenRow struct {
	Path  string `json:"path"`
	Tag   string `json:"tag"`
	Lines int    `json:"lines"`
	// Anywhere says nib has no record of which lines the agent saw, so an edit may anchor on any.
	Anywhere bool `json:"anywhere,omitempty"`
	// Seen are the lines seen in full, as inclusive ranges; MoreRanges counts those past the ones listed.
	Seen       [][2]int `json:"seen"`
	MoreRanges int      `json:"moreRanges,omitempty"`
	// Known counts lines the agent knows by content but does not hold as seen.
	Known int `json:"known,omitempty"`
}

// seenView lists, for each agent that called, the files nib holds a version of and the lines of each it counts as seen. It only reads.
func (s *Server) seenView(ctx *engine.Context, u *usageLog) *SeenView {
	view := &SeenView{Agents: []AgentSeen{}}
	for _, agent := range u.agentList() {
		row := AgentSeen{Agent: agent, Files: []SeenRow{}}
		files := s.engine.Store.SeenFiles(s.sessionOf(agent))
		row.More = max(len(files)-seenFiles, 0)
		for _, f := range files[:min(len(files), seenFiles)] {
			r := SeenRow{Path: shown(ctx.Directory, f.Path), Tag: f.Tag, Lines: f.Lines, Anywhere: f.Anywhere, Seen: f.Seen, Known: f.Known}
			if r.Seen == nil {
				r.Seen = [][2]int{}
			}
			if len(r.Seen) > seenRanges {
				r.MoreRanges, r.Seen = len(r.Seen)-seenRanges, r.Seen[:seenRanges]
			}
			row.Files = append(row.Files, r)
		}
		view.Agents = append(view.Agents, row)
	}
	return view
}

// ModelCall is a decision or summary model being asked now; Started is Unix milliseconds.
type ModelCall struct {
	// Kind is "decider" or "summary", and Model what is asked: a model, or the summary entry; or Kind is "shape", a nib shape request being
	// shaped, and Model the log's file name.
	Kind    string `json:"kind"`
	Model   string `json:"model"`
	Started int64  `json:"started"`
}

// SummaryState says whether summaries of long output are on; a status has it only where a summarizer is set.
type SummaryState struct {
	On bool `json:"on"`
}
