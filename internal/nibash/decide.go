package nibash

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/11ways/nib/internal/decider"
)

// purposeChars caps the description a decider reads as the call's purpose.
const purposeChars = 200

// purpose is the call's description as a decider reads it: trimmed, and cut at purposeChars.
func purpose(description string) string {
	runes := []rune(strings.TrimSpace(description))
	return string(runes[:min(len(runes), purposeChars)])
}

// shape cuts over-budget rows: the configured decider chooses the middle of plain output, and the fixed cut serves the rest, file rows
// included, since what nib's own commands show is file content the model asked for and counts as seen for edits.
func shape(ctx context.Context, rows []row, log saved, d decision) []row {
	if len(rows) <= headRows+tailRows || d.decider == nil || slices.ContainsFunc(rows, func(r row) bool { return !r.plain }) {
		return fixedCut(rows, log, "")
	}
	middle := rows[decidedHead : len(rows)-decidedTail]
	texts := make([]string, len(middle))
	for i, r := range middle {
		texts[i] = r.text
	}
	choice := d.decider.Decide(ctx, decider.Input{Command: d.command, Purpose: d.purpose, Exit: d.exit, TimedOut: d.timedOut, Rows: texts})
	return assemble(rows, choice, log, d)
}

// assemble keeps the start and end and adds the picks that reach the threshold, best first and widened, while they fit the budget; every gap
// is announced with the log lines it leaves out, and a closing note says who chose.
func assemble(rows []row, choice decider.Choice, log saved, d decision) []row {
	who := choice.Who
	if choice.Failure != "" {
		who = "the decision model failed (" + choice.Failure + "), so " + who
	}
	purpose := ""
	if choice.Model {
		purpose = " for the failure"
		if d.purpose != "" {
			purpose = " for your description"
		}
	}
	if choice.Judged && choice.Whole >= decider.WholeThreshold {
		return fixedCut(rows, log, who+" judged that your description needs every line, so filter or count the whole output instead")
	}
	n := len(rows)
	end := n - decidedTail
	keep := make([]bool, n)
	for i := range keep {
		keep[i] = i < decidedHead || i >= end
	}
	shown := decidedHead + decidedTail
	used := size(rows[:decidedHead]) + size(rows[end:])
	var picks []decider.Pick
	for _, p := range choice.Picks {
		if p.Score >= decider.PickThreshold {
			picks = append(picks, p)
		}
	}
	slices.SortStableFunc(picks, func(a, b decider.Pick) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	chose := false
	var missed [][2]int
	for _, p := range picks {
		from, to := max(decidedHead, decidedHead+p.Start-pickContext), min(end, decidedHead+p.End+pickContext)
		add, addBytes := 0, 0
		for i := from; i < to; i++ {
			if !keep[i] {
				add++
				addBytes += len(rows[i].text) + 1
			}
		}
		if add == 0 {
			chose = chose || from < to
			continue
		}
		if shown+add > headRows+tailRows || used+addBytes > budgetBytes {
			if first, last := logLines(rows[from:to]); first > 0 {
				missed = append(missed, [2]int{first, last})
			}
			continue
		}
		for i := from; i < to; i++ {
			keep[i] = true
		}
		shown += add
		used += addBytes
		chose = true
	}
	if !chose && len(missed) == 0 {
		return fixedCut(rows, log, who+" found nothing in them"+purpose)
	}
	var out []row
	for i := 0; i < n; {
		if keep[i] {
			out = append(out, rows[i])
			i++
			continue
		}
		j := i
		for j < n && !keep[j] {
			j++
		}
		out = append(out, row{text: "[nibash: " + leftOut(rows[i:j]) + "]", header: -1})
		i = j
	}
	clauses := []string{who + " chose the middle" + purpose}
	if len(missed) > 0 {
		passages := "passages"
		if len(missed) == 1 {
			passages = "passage"
		}
		clauses = append(clauses, fmt.Sprintf("%d more %s it chose did not fit (log %s)", len(missed), passages, spans(missed)))
	}
	var reasons []string
	byReason := map[string][][2]int{}
	for _, u := range choice.Unjudged {
		first, last := logLines(rows[decidedHead+u.Start : decidedHead+u.End])
		if first == 0 {
			continue
		}
		if byReason[u.Reason] == nil {
			reasons = append(reasons, u.Reason)
		}
		byReason[u.Reason] = append(byReason[u.Reason], [2]int{first, last})
	}
	for _, reason := range reasons {
		clauses = append(clauses, fmt.Sprintf("log %s were not judged (%s)", spans(byReason[reason]), reason))
	}
	return append(out, row{text: closing(shown, log, clauses), header: -1})
}

// logLines are the first and last log lines rows stand for; 0 when none stands for any, as for file headers.
func logLines(rows []row) (int, int) {
	first, last := 0, 0
	for _, r := range rows {
		if r.first == 0 {
			continue
		}
		if first == 0 {
			first = r.first
		}
		last = max(last, r.last)
	}
	return first, last
}

// leftOut names what a gap leaves out by its log lines, so sed -n A,Bp on the log shows exactly that.
func leftOut(rows []row) string {
	first, last := logLines(rows)
	if first == 0 {
		return fmt.Sprintf("%s rows left out", thousands(len(rows)))
	}
	return "log " + lineSpan(first, last) + " left out"
}

// spans lists ranges of log lines, merging overlaps, up to three.
func spans(ranges [][2]int) string {
	slices.SortFunc(ranges, func(a, b [2]int) int { return a[0] - b[0] })
	var merged [][2]int
	for _, r := range ranges {
		if k := len(merged); k > 0 && r[0] <= merged[k-1][1]+1 {
			merged[k-1][1] = max(merged[k-1][1], r[1])
		} else {
			merged = append(merged, r)
		}
	}
	var parts []string
	for i, r := range merged {
		if i == 3 {
			parts = append(parts, "...")
			break
		}
		if r[0] == r[1] {
			parts = append(parts, fmt.Sprint(r[0]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
		}
	}
	if len(merged) == 1 && merged[0][0] == merged[0][1] {
		return "line " + parts[0]
	}
	return "lines " + strings.Join(parts, ", ")
}

// closing is the note that ends a cut: how much is shown, the clauses about what was chosen, and where the whole output is.
func closing(shown int, log saved, clauses []string) string {
	text := fmt.Sprintf("[nibash: %s rows shown of %s log lines; %s; ", thousands(shown), thousands(log.lines), strings.Join(clauses, "; "))
	if log.path == "" {
		return text + "the whole output could not be saved]"
	}
	if log.holds < log.lines {
		limit := thousands(logLimit) + " bytes"
		if logLimit%(1<<20) == 0 {
			limit = fmt.Sprintf("%d MiB", logLimit>>20)
		}
		text += fmt.Sprintf("the log stops at its %s limit, in log line %s; ", limit, thousands(log.holds))
	}
	return text + "whole output: " + log.path + "]"
}
