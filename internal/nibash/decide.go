package nibash

import (
	"context"
	"fmt"
	"maps"
	"regexp"
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

// shape cuts over-budget rows, or with a summarizer condenses plain output past summaryLines or summaryBytes: the configured decider
// chooses the middle of plain output, and the fixed cut serves the rest, file rows included, since what nib's own commands show is file
// content the model asked for and counts as seen for edits. With a summarizer, a checked summary and the lines it points at take the place of
// the decider's cut; raw is the log's text. An output whose deduplicated entries fit excerptLines goes to the summarizer whole, with no
// decider asked unless the summary fails; a longer one goes as the excerpt of the decider's picks. Without a summary, output within the
// budget stays whole. A decision model and a summarizer read the output deduplicated (dedupe); the agent still gets its real rows.
func shape(ctx context.Context, rows []row, raw string, log saved, d decision) []row {
	over := len(rows) > budgetLines || size(rows) > budgetBytes
	plain := !slices.ContainsFunc(rows, func(r row) bool { return !r.plain })
	model := d.decider != nil && d.decider.Kind() == decider.OpenRouter
	var lines []string
	var entries []decider.Entry
	var legend decider.Legend
	if plain && (model || d.summarizer != nil) {
		lines = plainLines([]byte(raw))
		entries, legend = dedupe(lines)
	}
	decide := func() decider.Choice {
		middle := rows[decidedHead : len(rows)-decidedTail]
		in := decider.Input{Command: d.command, Purpose: d.purpose, Expect: d.expect, Exit: d.exit, TimedOut: d.timedOut}
		first, last := logLines(middle)
		if !model || first == 0 {
			for _, r := range middle {
				in.Rows = append(in.Rows, r.text)
			}
			return d.decider.Decide(ctx, in)
		}
		var mid []decider.Entry
		for _, e := range entries {
			if e.Last >= first && e.First <= last {
				mid = append(mid, e)
				in.Rows = append(in.Rows, e.Text)
			}
		}
		in.Legend = legend
		return onRows(d.decider.Decide(ctx, in), mid, middle)
	}
	// cut is what the output gets without a summary, choice the decider's answer when it was already asked.
	cut := func(choice *decider.Choice, notes ...string) []row {
		if !over {
			return rows
		}
		if len(rows) <= headRows+tailRows || d.decider == nil || !plain {
			return fixedCut(rows, log, "")
		}
		if choice == nil {
			c := decide()
			choice = &c
		}
		return assemble(rows, *choice, log, d, notes...)
	}
	// A summary is not asked for a run that succeeded with no purpose to serve, nor where the decider judged that every line counts.
	if d.summarizer == nil || d.decider == nil || !plain || d.purpose == "" && d.exit == 0 && !d.timedOut {
		return cut(nil)
	}
	ask := func(chosen []decider.Entry) (decider.Summary, bool) {
		var shown []int
		for _, e := range chosen {
			for n := e.First; n <= e.Last; n++ {
				shown = append(shown, n)
			}
		}
		in := decider.SummaryInput{Command: d.command, Purpose: d.purpose, Expect: d.expect, Exit: d.exit, TimedOut: d.timedOut, Log: lines,
			Excerpt: shown, Entries: chosen, Legend: legend}
		s := d.summarizer.Summarize(ctx, in)
		return s, s.Failure == ""
	}
	if len(entries) <= excerptLines {
		if s, ok := ask(entries); ok {
			return summarized(rows, s, log)
		}
		return cut(nil, "no summary")
	}
	// Collapsed repeats can leave a long log too few rows for a decider; its excerpt is then the start and end alone.
	var choice decider.Choice
	if len(rows) > headRows+tailRows {
		choice = decide()
	}
	if choice.Judged && choice.Whole >= decider.WholeThreshold {
		return cut(&choice)
	}
	if s, ok := ask(excerpt(rows, choice, entries)); ok {
		return summarized(rows, s, log)
	}
	return cut(&choice, "no summary")
}

// onRows turns a choice made over the deduplicated entries of the middle rows into one over the rows: each range covers the rows its entries'
// log lines fall in.
func onRows(choice decider.Choice, entries []decider.Entry, middle []row) decider.Choice {
	convert := func(r decider.Range) decider.Range {
		if r.End <= r.Start || r.End > len(entries) {
			return decider.Range{}
		}
		lo, hi := entries[r.Start].First, entries[r.End-1].Last
		start, end := -1, -1
		for i, row := range middle {
			if row.first == 0 || row.last < lo || row.first > hi {
				continue
			}
			if start < 0 {
				start = i
			}
			end = i + 1
		}
		if start < 0 {
			return decider.Range{}
		}
		return decider.Range{Start: start, End: end}
	}
	for i := range choice.Picks {
		choice.Picks[i].Range = convert(choice.Picks[i].Range)
	}
	for i := range choice.Unjudged {
		choice.Unjudged[i].Range = convert(choice.Unjudged[i].Range)
	}
	return choice
}

// excerpt is the entries a summarizer reads: those of the start and end a decider keeps, and of the passages it picked, best first and
// widened by excerptContext lines, while they fit excerptLines entries.
func excerpt(rows []row, choice decider.Choice, entries []decider.Entry) []decider.Entry {
	lines := 0
	if len(entries) > 0 {
		lines = entries[len(entries)-1].Last
	}
	entryOf := make([]int, lines+1)
	for i, e := range entries {
		for n := e.First; n <= e.Last; n++ {
			entryOf[n] = i
		}
	}
	keep := map[int]bool{}
	span := func(first, last int) {
		for n := max(1, first); n <= min(lines, last); n++ {
			keep[entryOf[n]] = true
		}
	}
	span(1, excerptHead)
	span(lines-excerptTail+1, lines)
	picks := slices.Clone(choice.Picks)
	slices.SortStableFunc(picks, func(a, b decider.Pick) int { return cmpScore(b.Score, a.Score) })
	for _, p := range picks {
		if p.Score < excerptScore || p.End <= p.Start {
			continue
		}
		first, last := logLines(rows[decidedHead+p.Start : decidedHead+p.End])
		if first == 0 {
			continue
		}
		more := map[int]bool{}
		for n := max(1, first-excerptContext); n <= min(lines, last+excerptContext); n++ {
			if !keep[entryOf[n]] {
				more[entryOf[n]] = true
			}
		}
		if len(keep)+len(more) > excerptLines {
			continue
		}
		maps.Copy(keep, more)
	}
	var out []decider.Entry
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

func cmpScore(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// summarized shows a checked summary in place of a decider's cut: the summary as a <summary> element, the rows of the log lines it points
// at with summaryContext lines around each while they fit summaryRows (the first passage always), a read of the passages that did not
// fit, and the gaps named.
func summarized(rows []row, s decider.Summary, log saved) []row {
	keep := make([]bool, len(rows))
	shown := 0
	var rest [][2]int
	for k, r := range s.Ranges {
		var add []int
		for i, row := range rows {
			if !keep[i] && row.first > 0 && row.first <= r[1]+summaryContext && row.last >= r[0]-summaryContext {
				add = append(add, i)
			}
		}
		if k > 0 && shown+len(add) > summaryRows {
			rest = append(rest, r)
			continue
		}
		for _, i := range add {
			keep[i] = true
		}
		shown += len(add)
	}
	out := append([]row{{text: "<summary>" + summaryText(s.Text) + "</summary>", header: -1}}, gapped(rows, keep)...)
	var clauses []string
	if len(rest) > 0 {
		var parts []string
		for _, r := range rest {
			part := fmt.Sprint(r[0])
			if r[1] > r[0] {
				part += fmt.Sprintf("-%d", r[1])
			}
			parts = append(parts, part)
		}
		more := "other passages: log " + spans(rest)
		if log.path != "" {
			more = "other passages: read " + log.path + ":" + strings.Join(parts, ",") + " plain:true"
		}
		clauses = append(clauses, more)
	}
	return append(out, row{text: closing(shown, log, clauses), header: -1})
}

// summaryTag matches the element's own tags in any case, which a summary's text could otherwise use to close it early or fake another.
var summaryTag = regexp.MustCompile(`(?i)</?\s*summary\s*>`)

// summaryText is a summary's text as one line holding neither of its element's tags.
func summaryText(text string) string {
	return strings.Join(strings.Fields(summaryTag.ReplaceAllString(text, " ")), " ")
}

// gapped is the rows keep marks, with a row naming the log lines of each run left out between them.
func gapped(rows []row, keep []bool) []row {
	var out []row
	for i := 0; i < len(rows); {
		if keep[i] {
			out = append(out, rows[i])
			i++
			continue
		}
		j := i
		for j < len(rows) && !keep[j] {
			j++
		}
		out = append(out, row{text: "[nibash: " + leftOut(rows[i:j]) + "]", header: -1})
		i = j
	}
	return out
}

// assemble keeps the start and end and adds the picks that reach the threshold, best first and widened, while they fit the budget; every gap
// is announced with the log lines it leaves out, and a closing note says who chose, with notes added to it.
func assemble(rows []row, choice decider.Choice, log saved, d decision, notes ...string) []row {
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
		return fixedCut(rows, log, strings.Join(append([]string{who + " judged that your description needs every line, so filter or count the whole output instead"}, notes...), "; "))
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
	slices.SortStableFunc(picks, func(a, b decider.Pick) int { return cmpScore(b.Score, a.Score) })
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
		return fixedCut(rows, log, strings.Join(append([]string{who + " found nothing in them" + purpose}, notes...), "; "))
	}
	out := gapped(rows, keep)
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
	return append(out, row{text: closing(shown, log, append(clauses, notes...)), header: -1})
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
	text := fmt.Sprintf("[nibash: %s rows shown of %s log lines; ", thousands(shown), thousands(log.lines))
	for _, clause := range clauses {
		text += clause + "; "
	}
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
