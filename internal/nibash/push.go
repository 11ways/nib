package nibash

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/11ways/nib/internal/engine"
)

// pushTimeout bounds the decision and summary models shaping a pushed job's output.
const pushTimeout = 2 * time.Minute

// pushEnd tells the agent that started j its end on its own, when j holds a way to reach it and nothing told the end yet: the line a call's
// report gives, then what nibash shows of what the job printed after its call returned, shaped as long output is. A failed push leaves the end
// to the next call's report.
func (s *Shell) pushEnd(sess *session, j *job) {
	if j.push == nil || !sess.jobs.claim(j) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	head := fmt.Sprintf("[job %%%d finished, exit %d; output: %s]", j.id, j.exit, j.log)
	if j.shown > 0 {
		head = fmt.Sprintf("[job %%%d finished, exit %d; output: %s, whose lines 1-%d its call showed]", j.id, j.exit, j.log, j.shown)
	}
	raw, err := os.ReadFile(j.log)
	lines := plainLines(raw)
	body := "(no output)"
	switch {
	case err != nil:
		body = fmt.Sprintf("[nibash: the log cannot be read: %v]", err)
	case j.shown > 0 && j.shown >= len(lines):
		body = "(no output after its call returned)"
	case len(lines) > 0:
		d := decision{decider: s.decider, summarizer: s.activeSummarizer(), command: j.command, purpose: purpose(j.purpose), exit: j.exit}
		texts, echoed := shapeLines(ctx, lines[j.shown:], d, saved{path: j.log, lines: len(lines), holds: len(lines), base: j.shown})
		if echoed {
			texts = append(texts, engine.EchoLesson)
		}
		body = strings.Join(texts, "\n")
	}
	if j.push(head+"\n"+body, map[string]string{"job": strconv.Itoa(j.id), "exit": strconv.Itoa(j.exit)}) != nil {
		sess.jobs.unclaim(j)
	}
}
