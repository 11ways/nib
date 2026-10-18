package nibash

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
)

// ShapeLog is what nibash shows at the model's end of a program run as command, with the call's description, that ended with exit and whose
// whole output is raw, kept at path: nib shape prints it for tools that keep their own logs. Its notes name path as the whole output, and it
// prints no exit line of its own.
func ShapeLog(ctx context.Context, raw []byte, path, command, description string, exit int, choose decider.Decider, summarizer decider.Summarizer) string {
	lines := plainLines(raw)
	if len(lines) == 0 {
		return "(no output)"
	}
	d := decision{decider: choose, summarizer: summarizer, command: command, purpose: purpose(description), exit: exit}
	texts, echoed := shapeLines(ctx, lines, d, saved{path: path, lines: len(lines), holds: len(lines)})
	// No agent is known here, so a recurring block's markers are always explained.
	if echoed {
		texts = append(texts, engine.EchoLesson)
	}
	return strings.Join(texts, "\n")
}

// Shape is ShapeLog with this server's deciders, as nib shape asks it through the session's socket: summaries follow the server's switch,
// the band shows the request while it runs, and what it showed of the log counts as every agent's skim of it. path is judged as a read's is.
func (s *Shell) Shape(ctx context.Context, path, command, description string, exit int, tools *engine.Context) (string, error) {
	file, err := s.engine.ReadLog(tools, path, logLimit, func(*engine.LogFile) int64 { return 0 })
	if err != nil {
		return "", err
	}
	end := s.observe("shape", filepath.Base(path))
	defer end()
	if command == "" {
		command = "cat " + path
	}
	text := ShapeLog(ctx, file.Raw, path, command, description, exit, s.decider, s.activeSummarizer())
	if !file.Short && ctx.Err() == nil {
		raw, skipped := file.Raw, 0
		if bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
			raw, skipped = raw[3:], 3
		}
		mark := advanced(file, raw, skipped, 0)
		mark.shaped = true
		s.shapeMu.Lock()
		if s.shaped == nil {
			s.shaped = map[string]logMark{}
		}
		s.shaped[file.Canonical] = mark
		s.shapeMu.Unlock()
	}
	return text, nil
}

// shapedMark is what nib shape last showed of the file at canonical.
func (s *Shell) shapedMark(canonical string) (logMark, bool) {
	s.shapeMu.Lock()
	defer s.shapeMu.Unlock()
	m, ok := s.shaped[canonical]
	return m, ok
}
