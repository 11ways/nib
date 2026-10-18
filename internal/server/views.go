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
	return lines
}
