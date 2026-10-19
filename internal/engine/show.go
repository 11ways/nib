package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Piece is one part of a hosted provider's answer: prose when Path is empty, else lines Start to End of the file at the absolute Path.
type Piece struct {
	Text       string
	Path       string
	Start, End int
	// Indexed is the text of those lines as the provider saw them, joined by newlines; nil when it did not say.
	Indexed *string
}

// Show prints a provider's answer within read's budget: prose as it is, and each run of locations in one file as one read block of the file
// as it is now, so every N:TEXT row is a real line and counts as seen.
func (e *Engine) Show(ctx *Context, provider string, pieces []Piece) (Result, error) {
	var blocks []*block
	var images []Image
	// Every file the answer points at counts toward a name before any prints, so a name they earn holds for all of them.
	var named []string
	for _, piece := range pieces {
		if piece.Path != "" {
			if real, err := realpath(ctx.resolve(piece.Path)); err == nil && !slices.Contains(named, real) {
				named = append(named, real)
			}
		}
	}
	ctx.preview(named)
	for i := 0; i < len(pieces); {
		if pieces[i].Path == "" {
			blocks = append(blocks, proseBlock(provider, pieces[i].Text))
			i++
			continue
		}
		j := i + 1
		for j < len(pieces) && pieces[j].Path == pieces[i].Path {
			j++
		}
		run, err := e.locationBlocks(ctx, provider, pieces[i:j], &images)
		if err != nil {
			return Result{}, err
		}
		blocks = append(blocks, run...)
		i = j
	}
	return Result{Text: strings.Join(fill(blocks, OutputLimit), "\n\n"), Images: images}, nil
}

// proseBlock is a provider's text, one row per line; text shaped like rows first gets a note that it is not anchors.
func proseBlock(provider, text string) *block {
	lines := strings.Split(text, "\n")
	var rows []blockRow
	if slices.ContainsFunc(lines, RowShaped.MatchString) {
		rows = append(rows, blockRow{text: fmt.Sprintf("[%s's own text follows, not anchors]", provider)})
	}
	for _, line := range lines {
		rows = append(rows, blockRow{text: line})
	}
	return &block{rows: rows, finish: func(shown int) []string {
		if shown < len(rows) {
			return []string{fmt.Sprintf("[%d more lines of %s's answer cut by the output limit]", len(rows)-shown, provider)}
		}
		return nil
	}}
}

// locationBlocks reads one file's locations as one block, each moved to where its indexed text is now when the file changed since the
// provider saw it, with a note for every location that moved, no longer matches or lies past the file's end.
func (e *Engine) locationBlocks(ctx *Context, provider string, run []Piece, images *[]Image) ([]*block, error) {
	t, err := resolveTarget(run[0].Path, ctx, false)
	if err != nil {
		var nf *NotFound
		var out *Outside
		switch {
		case errors.As(err, &out):
			return []*block{noteBlock("[" + out.message + "]")}, nil
		case errors.As(err, &nf):
			return []*block{noteBlock("[" + missingPath(run[0].Path, ctx) + "]")}, nil
		}
		return nil, err
	}
	var lines []string
	if source, err := e.OpenSource(ctx, t.canonical); err == nil && source.View != nil {
		lines = source.View.Lines
	}
	var notes []*block
	var sel selection
	for _, p := range run {
		start, end := p.Start, p.End
		if lines != nil && p.Indexed != nil {
			start, end = relocate(lines, start, end, *p.Indexed)
			switch {
			case start == 0:
				start, end = p.Start, p.End
				notes = append(notes, noteBlock(fmt.Sprintf("[%s changed since %s indexed it: lines %d-%d no longer hold what it found]", t.display, provider, p.Start, p.End)))
			case start != p.Start:
				notes = append(notes, noteBlock(fmt.Sprintf("[%s changed since %s indexed it: its lines %d-%d are now %d-%d]", t.display, provider, p.Start, p.End, start, end)))
			}
		}
		if lines != nil && start > len(lines) {
			notes = append(notes, noteBlock(fmt.Sprintf("[%s has %d lines now: %s's lines %d-%d are past its end]", t.display, len(lines), provider, start, end)))
			continue
		}
		sel.ranges = append(sel.ranges, lineRange{start: start, end: end})
	}
	if sel.ranges == nil {
		return notes, nil
	}
	b, err := e.entryBlock(ctx, t, sel, "", ReadArgs{}, LineLimit, images)
	if err != nil {
		return nil, err
	}
	return append(notes, b), nil
}

// relocate is where the indexed text of lines start to end is in lines: there when it still is, else its one other place, else 0, 0;
// trailing whitespace is not compared, as the file tag ignores it too.
func relocate(lines []string, start, end int, indexed string) (int, int) {
	want := strings.Split(indexed, "\n")
	matches := func(at int) bool {
		if at < 1 || at-1+len(want) > len(lines) {
			return false
		}
		for i, line := range want {
			if strings.TrimRight(line, " \t\r") != strings.TrimRight(lines[at-1+i], " \t\r") {
				return false
			}
		}
		return true
	}
	if end-start+1 != len(want) {
		return 0, 0
	}
	if matches(start) {
		return start, end
	}
	found := 0
	for at := 1; at+len(want)-1 <= len(lines); at++ {
		if matches(at) {
			if found != 0 {
				return 0, 0
			}
			found = at
		}
	}
	if found == 0 {
		return 0, 0
	}
	return found, found + len(want) - 1
}
