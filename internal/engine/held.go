package engine

import (
	"fmt"

	"github.com/11ways/nib/internal/snapshots"
)

// heldRun is the fewest rows an agent already holds that one marker replaces; a shorter run saves too little for the look back it asks for.
const heldRun = 8

// heldFew is the most rows of a file one call may show and still print them all: so few rows are a narrow request for exactly those lines.
const heldFew = 60

// heldLessonKey is the lesson heldLesson teaches; heldLesson explains held markers, once per agent and again after its context was dropped.
const heldLessonKey = "held"

const heldLesson = "[a [lines A-B unchanged since you saw them] row stands for lines you were shown whole before and that have not changed, and [lines A-B = your earlier C-D, unchanged] for such lines an edit moved; their numbers stay valid for edits. To print them, send the same call again at once, or read with full: true]"

// HeldRun is a run of rows, Start to End exclusive, that stands for lines First to Last, which the agent saw whole as From onwards.
type HeldRun struct{ Start, End, First, Last, From int }

// HeldLines maps each line of view the calling agent already holds, seen whole and unchanged since, to its number when it saw it; nil when
// markers are off for the call. The caller holds the path's lock, as for any read of the store's versions.
func (e *Engine) HeldLines(ctx *Context, canonical string, view snapshots.View) map[int]int {
	if !ctx.Markers {
		return nil
	}
	return e.Store.Held(ctx.Session, canonical, view)
}

// HeldRuns are the runs of at least heldRun rows whose lines follow each other and are held at one offset; line gives a row's line, 0 for
// a row that may not join one.
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
			if line(j) != first+j-i || !ok || next != from+j-i {
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

// Lines are the lines the run stands for, which stay seen in the version it prints from.
func (r HeldRun) Lines() []int {
	lines := make([]int, 0, r.Last-r.First+1)
	for line := r.First; line <= r.Last; line++ {
		lines = append(lines, line)
	}
	return lines
}

// HeldGroups puts each run of items the agent already holds, by the line each stands for (0 for one that may not join a run), as the one
// item marker makes from the run's items; when at most heldFew items may join, they all stay.
func HeldGroups[T any](items []T, held map[int]int, line func(T) int, marker func(run []T, r HeldRun) T) []T {
	if len(held) == 0 {
		return items
	}
	joinable := 0
	for _, item := range items {
		if line(item) > 0 {
			joinable++
		}
	}
	if joinable <= heldFew {
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
		out = append(out, marker(items[r.Start:r.End], r))
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
