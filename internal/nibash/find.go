package nibash

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/11ways/nib/internal/decider"
)

// ShapeFind reuses the log deciders for a complete inventory, retaining it so selected paths never hide the other matches.
func (s *Shell) ShapeFind(ctx context.Context, lines []string, expect string, limit int) string {
	if len(lines) <= limit || s.activeSummarizer() == nil && (s.decider == nil || s.decider.Kind() != decider.OpenRouter) {
		return ""
	}
	raw := strings.Join(lines, "\n")
	if len(raw) > logLimit {
		return ""
	}
	log := s.logs.save(raw)
	if log.path == "" {
		return ""
	}
	log.limit, log.tool = limit, "find"
	rows := make([]row, len(lines))
	for i, line := range lines {
		rows[i] = plainRow(line, i+1, i+1, nil)
	}
	d := decision{decider: s.decider, summarizer: s.activeSummarizer(), command: "find", expect: expect,
		purpose: fmt.Sprintf("Locate files for the request, targeting %d output lines including the summary and cited paths", limit), limit: limit}
	out := shape(ctx, rows, raw, log, d)
	if len(out) == 0 {
		return ""
	}
	text := strings.Join(rowTexts(out), "\n")
	if slices.ContainsFunc(out, func(r row) bool { return r.capped }) {
		text += "\n" + cutFooter(log, nil)
	}
	return text
}

// findSelection shows only a model's selected real paths; failure or a request needing every path keeps the engine's overview instead.
func findSelection(rows []row, choice *decider.Choice, log saved, limit int) []row {
	if choice == nil || !choice.Model || choice.Failure != "" || choice.Judged && choice.Whole >= decider.WholeThreshold {
		return nil
	}
	keep := make([]bool, len(rows))
	shown, bytes := 0, 0
	for _, pick := range choice.Picks {
		if pick.Score < decider.PickThreshold {
			continue
		}
		for i := max(0, pick.Start); i < min(len(rows), pick.End) && shown < limit; i++ {
			cost := numberedSize(rows[i:i+1], log)
			if !keep[i] && bytes+cost <= budgetBytes {
				keep[i], shown, bytes = true, shown+1, bytes+cost
			}
		}
	}
	if shown == 0 {
		return nil
	}
	notes := []string{choice.Who + " selected paths for what you expect"}
	if len(choice.Unjudged) > 0 {
		notes = append(notes, "some ranges were not judged; the complete inventory is retained")
	}
	out := gapped(rows, keep, nil, log, true)
	return append(out, row{text: closing(shown, log, numberedEnding, notes), header: -1})
}
