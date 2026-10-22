package nibash

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/11ways/nib/internal/js"
)

const (
	// recallCalls is how many of an agent's latest calls the fold remembers the output of.
	recallCalls = 50
	// recallRows caps the rows remembered of one call.
	recallRows = 2000
	// foldMin is the fewest rows a block repeated from an earlier call needs before it folds; the marker must cost less than them too.
	foldMin = 5
	// foldFailedMin is foldMin for a call that failed: its error, shown again, costs a few rows, and missed it costs a call to read it.
	foldFailedMin = 20
	// foldNamed caps how much of the block's first row the marker names it by.
	foldNamed = 40
)

// recall is what each agent was shown of program output in its latest calls, as hashes of the rows in order, so a block shown again folds
// into a marker naming the earlier call; it lives in memory only, so a restart forgets it.
type recall struct {
	mu     sync.Mutex
	agents map[string][]shownCall
}

// shownCall is one call's plain rows as the agent saw them, a zero hash standing for any row that was not one (a note, a file's row or a
// fold), which no block runs across.
type shownCall struct {
	at   time.Time
	rows []uint64
}

func newRecall() *recall { return &recall{agents: map[string][]shownCall{}} }

func rowHash(text string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(text))
	if sum := h.Sum64(); sum != 0 {
		return sum
	}
	return 1
}

// foldable says whether r is a row of program output a fold may stand for: plain, shown whole, and no listed path a folder row prints.
func foldable(r row) bool { return r.plain && !r.capped && r.listed == nil && r.first > 0 }

// fold puts each block of at least least rows the agent was shown, identical and in the same order, by one of its earlier calls since its
// context was last dropped (cleared) as one marker naming that call and the block's lines in the log save writes, then remembers what this
// call shows.
func (rc *recall) fold(agent string, cleared time.Time, rows []row, least int, save func() saved) []row {
	if rc == nil || agent == "" {
		return rows
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	calls := rc.agents[agent]
	// A call shown before the agent's context was dropped is no longer in it.
	for len(calls) > 0 && !calls[0].at.After(cleared) {
		calls = calls[1:]
	}
	hashes := make([]uint64, len(rows))
	for i, r := range rows {
		if foldable(r) {
			hashes[i] = rowHash(r.text)
		}
	}
	type place struct{ call, row int }
	starts := map[uint64][]place{}
	for c, call := range calls {
		for i, h := range call.rows {
			if h != 0 {
				starts[h] = append(starts[h], place{c, i})
			}
		}
	}
	var out []row
	var log *saved
	for i := 0; i < len(rows); {
		best, length := place{}, 0
		if hashes[i] != 0 {
			for _, p := range starts[hashes[i]] {
				earlier := calls[p.call].rows
				n := 0
				for i+n < len(rows) && p.row+n < len(earlier) && hashes[i+n] != 0 && hashes[i+n] == earlier[p.row+n] {
					n++
				}
				// The latest call wins a tie, as the agent saw it last.
				if n > length || n == length && p.call > best.call {
					best, length = p, n
				}
			}
		}
		if length >= least {
			if log == nil {
				saved := save()
				log = &saved
			}
			if marker, ok := foldMarker(*log, len(calls)-best.call, rows[i:i+length]); ok {
				out = append(out, row{text: marker, header: -1, first: rows[i].first, last: rows[i+length-1].last})
				i += length
				continue
			}
		}
		out = append(out, rows[i])
		i++
	}
	// What this call shows is remembered with the folded rows as breaks: the agent saw those lines in the earlier call, not in this one.
	shown := make([]uint64, 0, min(len(out), recallRows))
	for _, r := range out {
		if len(shown) == recallRows {
			break
		}
		if foldable(r) {
			shown = append(shown, rowHash(r.text))
		} else {
			shown = append(shown, 0)
		}
	}
	calls = append(calls, shownCall{at: time.Now(), rows: shown})
	if len(calls) > recallCalls {
		calls = calls[len(calls)-recallCalls:]
	}
	rc.agents[agent] = calls
	return out
}

// foldMarker is the row that stands for block, shown before by the agent's call ago calls back, named by the block's first row; ok is
// false when it would not cost less than the block, or the log that must hold the lines could not be written.
func foldMarker(log saved, ago int, block []row) (string, bool) {
	if log.path == "" {
		return "", false
	}
	first := strings.ReplaceAll(block[0].text, "`", "'")
	if js.Len(first) > foldNamed {
		first = string([]rune(first)[:foldNamed]) + "..."
	}
	call := "previous call"
	if ago > 1 {
		call = fmt.Sprintf("call %d calls ago", ago)
	}
	marker := log.note(fmt.Sprintf("the same %d rows from `%s` your %s showed; log %s in %s", len(block), first, call,
		log.span(block[0].first, block[len(block)-1].last), log.path))
	cost := 0
	for _, r := range block {
		cost += js.Len(r.printed()) + 1
	}
	return marker, js.Len(marker)+1 < cost
}
