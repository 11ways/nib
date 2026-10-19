package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Skim is the name of the tool that reads files through ReadLog for what the agent expects of them; every use of the name derives from it.
const Skim = "skim"

// LogHead is how much of a file's start a LogFile carries, enough to tell a log that grew from one rewritten since.
const LogHead = 4096

// LogFile is a text file read for a decider: its bytes from an offset on, and the lines its selector chose.
type LogFile struct {
	Display, Canonical string
	Size               int64
	// Head is the file's first LogHead bytes, or all of a smaller file.
	Head []byte
	// Raw is the file from From on; Short says it stops at the limit, after its last whole line, before the file's end.
	From  int64
	Raw   []byte
	Short bool
	// Ranges are the 1-based inclusive lines of Raw a selector chose, sorted and merged; nil without a selector.
	Ranges [][2]int
}

// ReadLog reads entry, a path with an optional line selector, the way read does (sandbox, links, named directories), at most limit bytes of
// it. Without a selector Raw starts where from says, which sees the file as opened with Raw still empty; with one it starts at the file's
// start, and an open range runs to the end rather than read's page of lines.
func (e *Engine) ReadLog(ctx *Context, entry string, limit int, from func(*LogFile) int64) (*LogFile, error) {
	input, sel, err := splitSelector(entry, ctx)
	if err != nil {
		return nil, err
	}
	if sel.symbols != nil {
		return nil, fmt.Errorf("%s: expect reads lines, so it takes no declared name", entry)
	}
	t, err := resolveTarget(input, ctx, false)
	if err != nil {
		var nf *NotFound
		if !errors.As(err, &nf) {
			return nil, err
		}
		rev, revErr := e.revisionOf(ctx, entry)
		var none *noCommit
		switch {
		case errors.As(revErr, &none):
			return nil, &NotFound{none.message}
		case revErr != nil:
			return nil, revErr
		case rev == nil:
			return nil, &NotFound{missingPath(entry, ctx)}
		}
		return revisionLog(ctx, rev, limit, from)
	}
	return serialized(t.canonical, ctx, func() (*LogFile, error) {
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
			return nil, fmt.Errorf("%s is a directory; expect reads files", t.display)
		}
		return readLog(ctx, &LogFile{Display: t.display, Canonical: t.canonical, Size: info.Size()}, file, sel, limit, from)
	})
}

// revisionLog is a revision's text read as a log: a file as of a commit, or the commit with its diff, which only grows when it is another.
func revisionLog(ctx *Context, r *revision, limit int, from func(*LogFile) int64) (*LogFile, error) {
	var sel selection
	if r.selector != "" {
		var err error
		if _, sel, err = splitSelector("@:"+r.selector, ctx); err != nil {
			return nil, err
		}
		if sel.symbols != nil {
			return nil, fmt.Errorf("%s%s:%s: expect reads lines, so it takes no declared name", r.t.display, r.t.rev, r.selector)
		}
	}
	log := &LogFile{Display: r.t.display + r.t.rev, Canonical: r.key, Size: int64(len(r.text))}
	return readLog(ctx, log, bytes.NewReader(r.text), sel, limit, from)
}

// readLog fills log, whose size is known, from file: its head, its bytes from where from says or a selector's lines.
func readLog(ctx *Context, log *LogFile, file io.ReaderAt, sel selection, limit int, from func(*LogFile) int64) (*LogFile, error) {
	head := make([]byte, min(log.Size, LogHead))
	n, _ := file.ReadAt(head, 0)
	log.Head = head[:n]
	if mime(log.Head) != "" || bytes.IndexByte(log.Head, 0) >= 0 {
		return nil, fmt.Errorf("%s is not a text file", log.Display)
	}
	selected := sel.ranges != nil || sel.tail > 0
	if !selected && from != nil {
		log.From = max(0, min(from(log), log.Size))
	}
	raw, err := io.ReadAll(io.NewSectionReader(file, log.From, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		raw = raw[:limit]
		if at := bytes.LastIndexByte(raw, '\n'); at >= 0 {
			raw = raw[:at+1]
		}
		log.Short = true
	}
	log.Raw = raw
	if err := ctx.checkAbort(); err != nil {
		return nil, err
	}
	if !selected {
		return log, nil
	}
	length := bytes.Count(raw, []byte("\n"))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		length++
	}
	if length == 0 {
		return log, nil
	}
	// An open range reads to the end: a decider reads the whole of what it is given, so read's page of lines would only cut it short.
	for i := range sel.ranges {
		if sel.ranges[i].end == 0 {
			sel.ranges[i].end = math.MaxInt
		}
	}
	spans, err := selectLines(sel, length)
	if err != nil {
		return nil, err
	}
	for _, s := range spans {
		log.Ranges = append(log.Ranges, [2]int{s.start, s.end})
	}
	return log, nil
}
