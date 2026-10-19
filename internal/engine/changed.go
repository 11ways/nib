package engine

import (
	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/snapshots"
)

// removedRow is a line a change removed, as a -| row that is never an anchor, cut past limit.
func removedRow(content string, limit int) string {
	shown := visible(content)
	if js.Len(shown) > limit {
		shown = js.Slice(shown, 0, limit) + "..."
	}
	return "-|" + shown
}

// Change is what a program changed in a file from a version the session held to the one Snapshot just recorded.
type Change struct {
	// Counted is false when the session holds no version tagged as the old one, or the diff timed out; Added and Removed are then zero.
	Counted        bool
	Added, Removed int
	// Rows are the change as -| rows for removed lines and +N: rows of the new version for added ones, with the kept line before a hunk that
	// only removes so its place shows; empty when the agent saw nothing of the old version, the change is over most lines, or over limit rows.
	Rows []string
}

// Changed diffs canonical's version tagged was against view and records the new rows it shows whole as seen.
func (e *Engine) Changed(ctx *Context, canonical, was string, view snapshots.View, most, limit int) Change {
	var old *snapshots.Version
	for _, v := range e.Store.Versions(ctx.Session, canonical) {
		if v.Tag == was {
			old = v
			break
		}
	}
	if old == nil {
		return Change{}
	}
	changes, ok := snapshots.LineChanges(old.Lines, view.Lines)
	if !ok {
		return Change{}
	}
	result := Change{Counted: true}
	for _, change := range changes {
		switch {
		case change.Added:
			result.Added += change.Count
		case change.Removed:
			result.Removed += change.Count
		}
	}
	if len(old.Seen) == 0 && len(old.Known) == 0 || result.Added+result.Removed > most {
		return result
	}
	var rows []string
	var seen []int
	shown := func(prefix string, line int) {
		r := lineRow(line, view.Lines[line-1], editColumns)
		rows = append(rows, prefix+r.text)
		if r.anchor {
			seen = append(seen, line)
		}
	}
	after := 1
	for i, change := range changes {
		switch {
		case change.Added:
			for offset := range change.Count {
				shown("+", after+offset)
			}
			after += change.Count
		case change.Removed:
			alone := (i == 0 || !changes[i-1].Added) && (i == len(changes)-1 || !changes[i+1].Added)
			if alone && after > 1 {
				shown(" ", after-1)
			}
			for _, content := range change.Value {
				rows = append(rows, removedRow(content, editColumns))
			}
			if alone && after == 1 && after <= len(view.Lines) {
				shown(" ", after)
			}
		default:
			after += change.Count
		}
	}
	if len(rows) > limit {
		return result
	}
	result.Rows = rows
	if len(seen) > 0 {
		e.Store.Record(ctx.Session, canonical, view, seen, nil)
	}
	return result
}
