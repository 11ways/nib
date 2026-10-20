package nibash

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
)

// ShapeLog is what nibash shows at the model's end of a program run as command, with the call's description, that ended with exit and whose
// whole output is raw, kept at path: nib shape prints it for tools that keep their own logs. Its notes name path as the whole output, and it
// prints no exit line of its own.
func ShapeLog(ctx context.Context, raw []byte, path, command, description string, exit int, choose decider.Decider, summarizer decider.Summarizer) string {
	text, _ := shapeLog(ctx, raw, path, command, description, exit, choose, summarizer)
	return text
}

// shapeLog is ShapeLog with the log's lines it shows.
func shapeLog(ctx context.Context, raw []byte, path, command, description string, exit int, choose decider.Decider, summarizer decider.Summarizer) (string, lineSet) {
	lines := plainLines(raw)
	if len(lines) == 0 {
		return "(no output)", nil
	}
	d := decision{decider: choose, summarizer: summarizer, command: command, purpose: purpose(description), exit: exit}
	rows, footer, echoed := shapeLines(ctx, lines, d, saved{path: path, lines: len(lines), holds: len(lines)})
	texts := append(rowTexts(rows), footer...)
	// No agent is known here, so a recurring block's markers are always explained.
	if echoed {
		texts = append(texts, engine.EchoLesson)
	}
	return strings.Join(texts, "\n"), shownIn(rows, 0, len(lines), 0, len(lines), nil)
}

// Shape is ShapeLog with this server's deciders, as nib shape asks it through the session's socket: summaries follow the server's switch,
// and the band shows the request while it runs. The lines it showed went to a tool's result no agent can be told apart for, so a later
// skim folds them into a row that says how to read them. path is judged as a read's is.
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
	text, shown := shapeLog(ctx, file.Raw, path, command, description, exit, s.decider, s.activeSummarizer())
	if ctx.Err() == nil {
		s.shapeMu.Lock()
		if s.shaped == nil {
			s.shaped = map[string]logMark{}
		}
		s.shaped[file.Canonical] = logMark{size: file.Size, head: file.Head, shown: shown, at: time.Now()}
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
