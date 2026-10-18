package engine

import (
	"bufio"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"

	"github.com/11ways/nib/internal/snapshots"
)

// Source is a file opened for nibash's own commands: its lines with their origin when nib can hold it as one version, else streamed plain.
type Source struct {
	Canonical string
	Display   string
	// View is the version the lines come from; nil when the file is over the text limit, binary, still changing or not UTF-8.
	View *snapshots.View
	// Size is the file's size when it was opened.
	Size int64
	// BOM is set when the file starts with a byte order mark, which View's first line leaves out but a program reading the file sees.
	BOM  bool
	path string
}

// ErrDirectory is returned by OpenSource for a directory, which the shell commands report the way GNU tools do.
var ErrDirectory = errors.New("Is a directory")

// OpenSource opens an absolute path the way read does (sandbox, links, named directories) and records its version, with no line seen yet.
func (e *Engine) OpenSource(ctx *Context, path string) (*Source, error) {
	t, err := resolveTarget(path, ctx, false)
	if err != nil {
		return nil, err
	}
	return serialized(t.canonical, ctx, func() (*Source, error) {
		file, err := os.OpenFile(t.canonical, os.O_RDONLY|nonblock, 0)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		if err := confined(ctx, file, t.canonical); err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			return nil, ErrDirectory
		}
		source := &Source{Canonical: t.canonical, Display: t.display, Size: info.Size(), path: t.canonical}
		if info.Size() > TextLimit {
			return source, nil
		}
		current, err := snapshot(file, TextLimit)
		if err != nil {
			var changed changedWhileReading
			if errors.As(err, &changed) {
				return source, nil
			}
			return nil, err
		}
		decoded, err := decodeText(current.bytes)
		if err != nil {
			return source, nil
		}
		view := viewOf(decoded)
		e.Store.Record(ctx.Session, t.canonical, view, []int{}, nil)
		source.View = &view
		source.BOM = decoded.bom != ""
		return source, nil
	})
}

// Lines calls each with every line of the source, numbered from 1, until it returns false; a streamed file's lines are its raw text, invalid
// UTF-8 replaced, without the line break.
func (s *Source) Lines(each func(line int, text string) bool) error {
	return s.lines(true, each)
}

// Raw is Lines with a streamed file's lines as the file holds them, invalid UTF-8 and a carriage return before the newline kept; a held
// file's lines are valid UTF-8 already.
func (s *Source) Raw(each func(line int, text string) bool) error {
	return s.lines(false, each)
}

func (s *Source) lines(valid bool, each func(line int, text string) bool) error {
	if s.View != nil {
		// An empty file holds one empty line as a version, but no line to print.
		if s.View.Key == "" {
			return nil
		}
		for i, text := range s.View.Lines {
			if !each(i+1, text) {
				return nil
			}
		}
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<16)
	for line := 1; ; line++ {
		text, err := reader.ReadString('\n')
		if text == "" && err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		text = strings.TrimSuffix(text, "\n")
		if valid {
			text = strings.ToValidUTF8(strings.TrimSuffix(text, "\r"), "�")
		}
		if !each(line, text) {
			return nil
		}
		if err != nil {
			return nil
		}
	}
}

// Row is how a line prints: an N:TEXT anchor row, or a bracketed row that is never an anchor, as read prints them.
func Row(line int, content string, limit int) (text string, anchor bool) {
	r := lineRow(line, content, limit)
	return r.text, r.anchor
}

// RowShaped matches text that reads like an anchor or row-map row, which output nib does not vouch for must never pass as.
var RowShaped = regexp.MustCompile(`^\*?\d+[:~]`)

// Latest is the newest version of canonical this session holds.
func (e *Engine) Latest(ctx *Context, canonical string) *snapshots.Version {
	versions := e.Store.Versions(ctx.Session, canonical)
	if len(versions) == 0 {
		return nil
	}
	return versions[0]
}

// Seen records lines of a version as displayed whole; known records lines the agent wrote.
func (e *Engine) Seen(ctx *Context, canonical string, view snapshots.View, seen, known []int) {
	e.Store.Record(ctx.Session, canonical, view, nonNil(seen), known)
}

// Display is how nib prints a canonical path.
func (ctx *Context) Display(canonical string) string { return ctx.display(canonical) }

// Resolve is an input path made absolute against the server's directory, with ~ and ~name/ expanded.
func (ctx *Context) Resolve(input string) string { return ctx.resolve(input) }

// Refused says why the sandbox refuses a path, or "" when it allows it or is off.
func (ctx *Context) Refused(path string) (string, error) { return outsideSandbox(ctx, path) }

// Roots are the workspace roots, canonicalized.
func (ctx *Context) WorkspaceRoots() ([]string, error) { return workspaceRoots(ctx) }

// Facts is the size and modification time of a file, for noticing that a command changed it.
type Facts struct {
	Size  int64
	MTime syscall.Timespec
	CTime syscall.Timespec
}

// Stat is a file's facts; ok is false when it is not a regular file.
func Stat(path string) (Facts, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Facts{}, false
	}
	s := statOf(info)
	return Facts{info.Size(), s.Mtim, s.Ctim}, true
}

// Snapshot reads a file nib can hold as one version and records it; known lines are those the agent wrote, all of them when every is set.
func (e *Engine) Snapshot(ctx *Context, canonical string, every bool) (snapshots.View, bool) {
	file, err := os.OpenFile(canonical, os.O_RDONLY|nonblock, 0)
	if err != nil {
		return snapshots.View{}, false
	}
	defer file.Close()
	current, err := snapshot(file, TextLimit)
	if err != nil {
		return snapshots.View{}, false
	}
	decoded, err := decodeText(current.bytes)
	if err != nil {
		return snapshots.View{}, false
	}
	view := viewOf(decoded)
	var known []int
	if every {
		known = allLines(len(view.Lines))
	}
	e.Store.Record(ctx.Session, canonical, view, []int{}, known)
	return view, true
}

// Held are the paths this session holds versions of.
func (e *Engine) Held(ctx *Context) []string { return e.Store.Paths(ctx.Session) }
