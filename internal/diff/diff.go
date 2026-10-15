// Package diff ports jsdiff's diffArrays (Myers with jsdiff's diagonal pruning and tie-breaking), so line maps match the TypeScript nib exactly.
package diff

import "time"

// Change is one run of a diff: lines kept, added to the new side, or removed from the old one.
type Change struct {
	Count   int
	Added   bool
	Removed bool
	Value   []string
}

type component struct {
	count          int
	added, removed bool
	previous       *component
}

type path struct {
	oldPos int
	last   *component
}

// Arrays diffs old against new; ok is false when it gives up after timeout, as jsdiff returns undefined.
func Arrays(old, new []string, timeout time.Duration) (changes []Change, ok bool) {
	newLen, oldLen := len(new), len(old)
	editLength := 1
	maxEditLength := newLen + oldLen
	deadline := time.Now().Add(timeout)
	best := map[int]*path{0: {oldPos: -1}}
	newPos := extractCommon(best[0], new, old, 0)
	if best[0].oldPos+1 >= oldLen && newPos+1 >= newLen {
		return build(best[0].last, new, old), true
	}
	minDiagonal, maxDiagonal := -1<<62, 1<<62
	for editLength <= maxEditLength && !time.Now().After(deadline) {
		for diagonal := max(minDiagonal, -editLength); diagonal <= min(maxDiagonal, editLength); diagonal += 2 {
			removePath, addPath := best[diagonal-1], best[diagonal+1]
			if removePath != nil {
				delete(best, diagonal-1)
			}
			canAdd := false
			if addPath != nil {
				addNewPos := addPath.oldPos - diagonal
				canAdd = addNewPos >= 0 && addNewPos < newLen
			}
			canRemove := removePath != nil && removePath.oldPos+1 < oldLen
			if !canAdd && !canRemove {
				delete(best, diagonal)
				continue
			}
			var base *path
			if !canRemove || (canAdd && removePath.oldPos < addPath.oldPos) {
				base = addToPath(addPath, true, false, 0)
			} else {
				base = addToPath(removePath, false, true, 1)
			}
			newPos = extractCommon(base, new, old, diagonal)
			if base.oldPos+1 >= oldLen && newPos+1 >= newLen {
				return build(base.last, new, old), true
			}
			best[diagonal] = base
			if base.oldPos+1 >= oldLen {
				maxDiagonal = min(maxDiagonal, diagonal-1)
			}
			if newPos+1 >= newLen {
				minDiagonal = max(minDiagonal, diagonal+1)
			}
		}
		editLength++
	}
	return nil, false
}

func addToPath(p *path, added, removed bool, oldPosInc int) *path {
	last := p.last
	if last != nil && last.added == added && last.removed == removed {
		return &path{oldPos: p.oldPos + oldPosInc, last: &component{count: last.count + 1, added: added, removed: removed, previous: last.previous}}
	}
	return &path{oldPos: p.oldPos + oldPosInc, last: &component{count: 1, added: added, removed: removed, previous: last}}
}

func extractCommon(base *path, new, old []string, diagonal int) int {
	oldPos := base.oldPos
	newPos := oldPos - diagonal
	common := 0
	for newPos+1 < len(new) && oldPos+1 < len(old) && old[oldPos+1] == new[newPos+1] {
		newPos++
		oldPos++
		common++
	}
	if common > 0 {
		base.last = &component{count: common, previous: base.last}
	}
	base.oldPos = oldPos
	return newPos
}

func build(last *component, new, old []string) []Change {
	var reversed []*component
	for c := last; c != nil; c = c.previous {
		reversed = append(reversed, c)
	}
	changes := make([]Change, 0, len(reversed))
	newPos, oldPos := 0, 0
	for i := len(reversed) - 1; i >= 0; i-- {
		c := reversed[i]
		change := Change{Count: c.count, Added: c.added, Removed: c.removed}
		if !c.removed {
			change.Value = new[newPos : newPos+c.count]
			newPos += c.count
			if !c.added {
				oldPos += c.count
			}
		} else {
			change.Value = old[oldPos : oldPos+c.count]
			oldPos += c.count
		}
		changes = append(changes, change)
	}
	return changes
}
