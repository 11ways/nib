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
		if errors.As(err, &nf) {
			return nil, &NotFound{missingPath(entry, ctx)}
		}
		return nil, err
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
		log := &LogFile{Display: t.display, Canonical: t.canonical, Size: info.Size()}
		head := make([]byte, min(info.Size(), LogHead))
		n, _ := file.ReadAt(head, 0)
		log.Head = head[:n]
		if mime(log.Head) != "" || bytes.IndexByte(log.Head, 0) >= 0 {
			return nil, fmt.Errorf("%s is not a text file", t.display)
		}
		selected := sel.ranges != nil || sel.tail > 0
		if !selected && from != nil {
			log.From = max(0, min(from(log), info.Size()))
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
	})
}
