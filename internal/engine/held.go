package engine

import (
	"fmt"
	"strings"

	"github.com/11ways/nib/internal/gitform"
	"github.com/11ways/nib/internal/snapshots"
)

// heldRun is the fewest rows an agent already holds that one marker replaces; a shorter run costs about what its marker does.
const heldRun = 3

// heldMapped is the most lines a held run prints as row-map rows instead of one marker: an agent checking a few lines it holds would
// otherwise call again to see them, translating earlier numbers by hand on the way, which is where its line numbers go wrong.
const heldMapped = 30

// heldLessonKey is the lesson heldLesson teaches; heldLesson explains held markers, once per agent and again after its context was dropped.
const heldLessonKey = "held"

const heldLesson = "[a [lines A-B unchanged since you saw them] row stands for lines you were shown whole before and that have not changed, and [lines A-B = your earlier C-D, unchanged] for such lines an edit moved; their numbers stay valid for edits. To print them, send the same call again at once, or read with full: true. Such a row, or an N~ one, means the line is as when you last saw it whole, so an edit you expected there did not happen]"

// HeldRun is a run of rows, Start to End exclusive, that stands for lines First to Last, which the agent saw whole as From onwards; From is
// 0 for lines it knows only the content of, shown by a plain read without numbers, which print as map rows however long the run.
type HeldRun struct{ Start, End, First, Last, From int }

// HeldLines maps each line of view the calling agent already holds, seen whole and unchanged since, to its number when it saw it, or to 0
// when it knows only the content; nil when markers are off for the call. The caller holds the path's lock, as for any read of the store's
// versions.
func (e *Engine) HeldLines(ctx *Context, canonical string, view snapshots.View) map[int]int {
	if !ctx.Markers {
		return nil
	}
	return e.Store.Held(ctx.Session, canonical, view)
}

// HeldRuns are the runs of at least heldRun rows whose lines follow each other and are held at one offset, or all known by content only;
// line gives a row's line, 0 for a row that may not join one.
func HeldRuns(count int, line func(row int) int, held map[int]int) []HeldRun {
	var runs []HeldRun
	for i := 0; i < count; {
		first := line(i)
		from, ok := held[first]
		if first <= 0 || !ok {
			i++
			continue
		}
		j := i + 1
		for ; j < count; j++ {
			next, ok := held[line(j)]
			if line(j) != first+j-i || !ok || (from == 0) != (next == 0) || from > 0 && next != from+j-i {
				break
			}
		}
		if j-i >= heldRun {
			runs = append(runs, HeldRun{i, j, first, first + j - i - 1, from})
		}
		i = j
	}
	return runs
}

// Marker is the row that prints in the run's place; it is bracketed, so never an anchor.
func (r HeldRun) Marker() string {
	if r.From == r.First {
		return fmt.Sprintf("[lines %d-%d unchanged since you saw them]", r.First, r.Last)
	}
	return fmt.Sprintf("[lines %d-%d = your earlier %d-%d, unchanged]", r.First, r.Last, r.From, r.From+r.Last-r.First)
}

// MarkerFor is the run's marker, saying so when its lines, or which of them, were seen whole before the last edit of the file by nib's own
// tools, which left them as they were: an agent reading after an edit it expected there would otherwise take the marker for the edit's
// result.
func (e *Engine) MarkerFor(ctx *Context, canonical string, view snapshots.View, r HeldRun) string {
	marker := r.Marker()
	if r.From == 0 {
		return marker
	}
	wrote, ok := e.lastWrite(canonical)
	if !ok {
		return marker
	}
	moments, ok := e.Store.SeenMoments(ctx.Session, canonical, view, r.Lines())
	if !ok {
		return marker
	}
	var before []int
	for _, line := range r.Lines() {
		if moments[line] < wrote.UnixNano() {
			before = append(before, line)
		}
	}
	switch {
	case len(before) == 0:
		return marker
	case len(before) == r.Last-r.First+1:
		return strings.TrimSuffix(marker, "]") + ", before the last edit of this file]"
	}
	var spans []string
	for i := 0; i < len(before); {
		j := i
		for j+1 < len(before) && before[j+1] == before[j]+1 {
			j++
		}
		if i == j {
			spans = append(spans, fmt.Sprint(before[i]))
		} else {
			spans = append(spans, fmt.Sprintf("%d-%d", before[i], before[j]))
		}
		i = j + 1
	}
	return strings.TrimSuffix(marker, "]") + ", " + strings.Join(spans, ", ") + " of them before the last edit of this file]"
}

// Mapped reports whether the run prints as one MapRow per line, under its current numbers, rather than as its Marker: a short run, or one
// the agent never had numbers for.
func (r HeldRun) Mapped() bool { return r.From == 0 || r.Last-r.First+1 <= heldMapped }

// MapRow is the N~ row of line, whose content the agent saw whole: its indentation and start.
func MapRow(line int, content string) string { return fmt.Sprintf("%d~%s", line, clip(content)) }

// Lines are the lines the run stands for, which stay seen in the version it prints from.
func (r HeldRun) Lines() []int {
	lines := make([]int, 0, r.Last-r.First+1)
	for line := r.First; line <= r.Last; line++ {
		lines = append(lines, line)
	}
	return lines
}

// HeldGroups puts each run of items the agent already holds, by the line each stands for (0 for one that may not join a run), as the items
// marker makes from the run's items.
func HeldGroups[T any](items []T, held map[int]int, line func(T) int, marker func(run []T, r HeldRun) []T) []T {
	if len(held) == 0 {
		return items
	}
	runs := HeldRuns(len(items), func(i int) int { return line(items[i]) }, held)
	if len(runs) == 0 {
		return items
	}
	var out []T
	next := 0
	for _, r := range runs {
		out = append(out, items[next:r.Start]...)
		out = append(out, marker(items[r.Start:r.End], r)...)
		next = r.End
	}
	return append(out, items[next:]...)
}

// HeldNote is heldLesson the first time an agent is shown a held marker, else nothing.
func (e *Engine) HeldNote(ctx *Context) []string {
	if e.Store.Teach(ctx.Session, heldLessonKey) {
		return []string{heldLesson}
	}
	return nil
}

// OwnAuthorNote is gitform.OwnAuthorLesson the first time an agent is shown a commit header without its author, else nothing.
func (e *Engine) OwnAuthorNote(ctx *Context) []string {
	if e.Store.Teach(ctx.Session, "own-author") {
		return []string{gitform.OwnAuthorLesson}
	}
	return nil
}
