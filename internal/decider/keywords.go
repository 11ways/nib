package decider

import (
	"context"
	"regexp"
)

var (
	// failureWord is a word that names a problem, standing alone: not part of a path or file name (src/errors.ts, error-handling.test.ts).
	failureWord = regexp.MustCompile(`(?i)(?:^|[^\w./-])(?:errors?|fail|fails|failed|failure|failures|failing|panic|panicked|exception|traceback|fatal)(?:$|[^\w./-])`)
	// failureMark is how runners and languages mark a problem without such a word: pytest's E lines, TAP and Go's FAIL, assertion diffs,
	// Bun's cross, and exception class names (TypeError, IOException).
	failureMark = regexp.MustCompile(`^E\s|^(?:FAIL|not ok)\b|^\s*(?:Expected|Received)\b|\x{2717}|\b[A-Z]\w*(?:Error|Exception)\b`)
	// noProblem is a count of none ("0 errors", "no failures"), which names no problem.
	noProblem = regexp.MustCompile(`(?i)\b(?:0|no) (?:errors?|failures?|failed)\b`)
)

// Hit says whether a row looks like it reports a problem.
func Hit(row string) bool {
	return failureMark.MatchString(row) || failureWord.MatchString(noProblem.ReplaceAllString(row, ""))
}

// keywords picks the rows that name a problem.
type keywords struct{}

func (keywords) Kind() Kind { return Keywords }

func (keywords) Decide(_ context.Context, in Input) Choice {
	return Choice{Picks: keywordPicks(in.Rows), Who: "keywords"}
}

// keywordPicks are the hits as one-row picks: the last first, since runners print the reason last, then the rest in output order, since
// compilers print causes first.
func keywordPicks(rows []string) []Pick {
	var hits []Pick
	for i, row := range rows {
		if Hit(row) {
			hits = append(hits, Pick{Range{i, i + 1}, KeywordScore})
		}
	}
	if n := len(hits); n > 1 {
		hits = append([]Pick{hits[n-1]}, hits[:n-1]...)
	}
	return hits
}
