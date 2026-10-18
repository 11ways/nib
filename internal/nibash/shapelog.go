package nibash

import (
	"context"
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
