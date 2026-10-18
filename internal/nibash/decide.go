package nibash

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
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
		return choose(ctx, rows[decidedHead:len(rows)-decidedTail], entries, legend, d, model)
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
			Excerpt: shown, Entries: chosen, Legend: legend, Base: log.base}
		s := d.summarizer.Summarize(ctx, in)
		return s, s.Failure == ""
	}
	// A list of the same few problems at many places, short enough to show whole, stays whole: each place is work the agent must do.
	if !over && problemList(lines) {
		return rows
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
	if s, ok := ask(excerpt(middleOf(rows), choice, entries)); ok {
		return summarized(rows, s, log)
	}
	return cut(&choice, "no summary")
}

// middleOf is the rows a decider chooses among, between the start and end kept anyway; none when output is too short to have been decided.
func middleOf(rows []row) []row {
	if len(rows) <= headRows+tailRows {
		return nil
	}
	return rows[decidedHead : len(rows)-decidedTail]
}

// choose asks the decider which of middle, rows of plain output, the call needs; a decision model reads them as entries, the deduplicated
// log lines they stand for, and its answer is turned back into ranges of middle.
func choose(ctx context.Context, middle []row, entries []decider.Entry, legend decider.Legend, d decision, model bool) decider.Choice {
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

// excerpt is the entries a summarizer reads: the first excerptHead and last excerptTail entries, and those of the passages a decider
// picked among middle, widened by excerptContext lines, in novelFirst order while they fit excerptLines entries.
func excerpt(middle []row, choice decider.Choice, entries []decider.Entry) []decider.Entry {
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
	seen := map[string]bool{}
	for i := range entries {
		if i < excerptHead || i >= len(entries)-excerptTail {
			keep[i] = true
			seen[likeness(entries[i].Text)] = true
		}
	}
	// widened is the entries a pick stands for with excerptContext lines around it; none for a pick outside the log.
	widened := func(p decider.Pick) []int {
		first, last := logLines(middle[p.Start:p.End])
		if first == 0 {
			return nil
		}
		var out []int
		for n := max(1, first-excerptContext); n <= min(lines, last+excerptContext); n++ {
			if i := entryOf[n]; len(out) == 0 || out[len(out)-1] != i {
				out = append(out, i)
			}
		}
		return out
	}
	var picks []decider.Pick
	for _, p := range choice.Picks {
		if p.Score >= excerptScore && p.End > p.Start {
			picks = append(picks, p)
		}
	}
	picks = novelFirst(picks, seen, func(p decider.Pick) []string {
		var texts []string
		for _, i := range widened(p) {
			texts = append(texts, entries[i].Text)
		}
		return texts
	})
	for _, p := range picks {
		var more []int
		for _, i := range widened(p) {
			if !keep[i] {
				more = append(more, i)
			}
		}
		if len(keep)+len(more) > excerptLines {
			continue
		}
		for _, i := range more {
			keep[i] = true
		}
	}
	var out []decider.Entry
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

// novelFirst orders picks best first, except that one whose texts are mostly like those of seen and of the picks before it waits until every
// pick that adds something new has gone, so a failure repeated for hundreds of test classes cannot crowd out the one cause it repeats.
func novelFirst(picks []decider.Pick, seen map[string]bool, texts func(decider.Pick) []string) []decider.Pick {
	picks = slices.Clone(picks)
	slices.SortStableFunc(picks, func(a, b decider.Pick) int { return cmpScore(b.Score, a.Score) })
	seen = maps.Clone(seen)
	var novel, waiting []decider.Pick
	for _, p := range picks {
		likes, news := map[string]bool{}, map[string]bool{}
		for _, text := range texts(p) {
			like := likeness(text)
			likes[like] = true
			if !seen[like] {
				news[like] = true
			}
		}
		if float64(len(news)) < pickNovelty*float64(len(likes)) {
			waiting = append(waiting, p)
			continue
		}
		maps.Copy(seen, news)
		novel = append(novel, p)
	}
	return append(novel, waiting...)
}

// qualified matches dotted names (packages, classes, files), which a log repeats with only the name changed.
var qualified = regexp.MustCompile(`[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)+`)

// likeness is what a row is like with its numbers and dotted names masked: the same failure reported for another test class is alike.
func likeness(text string) string { return qualified.ReplaceAllString(engine.MaskDigits(text), "X") }

// problemList says whether lines report at least listProblems different problem lines that are, on average, listPerKind or more copies of
// each kind: type errors at many call sites are; hundreds of failing tests that each name their own test are not.
func problemList(lines []string) bool {
	distinct, kinds := map[string]bool{}, map[string]bool{}
	for _, line := range lines {
		if decider.Hit(line) {
			distinct[line] = true
			kinds[likeness(line)] = true
		}
	}
	return len(distinct) >= listProblems && len(distinct) >= listPerKind*len(kinds)
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

// summarized shows a checked summary in place of a decider's cut: the summary's rows, then a closing note.
func summarized(rows []row, s decider.Summary, log saved) []row {
	out, shown := summaryPassages(rows, s, log)
	return append(out, row{text: closing(shown, log, nil), header: -1})
}

// summaryPassages are a checked summary as a <summary> element, then every passage it points at, each widened by summaryContext lines and given
// a share of summaryRows; a passage longer than its share drops its context lines first, then shows the start and end of the lines pointed
// at, with the read of what it leaves out in between. shown counts the passages' rows.
func summaryPassages(rows []row, s decider.Summary, log saved) (out []row, shown int) {
	taken := make([]bool, len(rows))
	var passages [][]int
	var pointed [][2]int
	for _, r := range s.Ranges {
		var add []int
		for i, row := range rows {
			if !taken[i] && row.first > 0 && row.first <= r[1]+summaryContext && row.last >= r[0]-summaryContext {
				add = append(add, i)
				taken[i] = true
			}
		}
		if len(add) > 0 {
			passages = append(passages, add)
			pointed = append(pointed, r)
		}
	}
	shares := shared(passages, summaryRows, summaryShare)
	keep := make([]bool, len(rows))
	notes := map[int]passageNote{}
	for k, add := range passages {
		share := shares[k]
		// Context rows sit at a passage's edges; they go before any line the summary pointed at.
		outside := func(i int) bool { return rows[i].last < pointed[k][0] || rows[i].first > pointed[k][1] }
		for len(add) > share && outside(add[0]) {
			add = add[1:]
		}
		for len(add) > share && outside(add[len(add)-1]) {
			add = add[:len(add)-1]
		}
		if share >= len(add) {
			for _, i := range add {
				keep[i] = true
			}
			shown += len(add)
			continue
		}
		for _, i := range add[:share] {
			keep[i] = true
		}
		shown += share
		if first, last := logLines(rowsAt(rows, add[share:])); first > 0 {
			note := "log " + log.span(first, last) + " left out; the rest of this passage"
			if log.path != "" {
				note += fmt.Sprintf(": read %s:%d-%d plain:true", log.path, log.base+first, log.base+last)
			}
			notes[add[share]] = passageNote{text: log.note(note), end: add[len(add)-1] + 1}
		}
	}
	out = append([]row{{text: "<summary>" + summaryText(s.Text) + "</summary>", header: -1}}, gapped(rows, keep, notes, log)...)
	return out, shown
}

// shared splits budget rows among passages of the given sizes: one that fits an even split of what is left keeps all its rows, and the rest
// share what remains, each at least least rows.
func shared(passages [][]int, budget, least int) []int {
	order := make([]int, len(passages))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return len(passages[a]) - len(passages[b]) })
	shares := make([]int, len(passages))
	left := budget
	for k, i := range order {
		even := max(least, left/(len(order)-k))
		shares[i] = min(len(passages[i]), even)
		left = max(0, left-shares[i])
	}
	return shares
}

// rowsAt is the rows at the given indexes.
func rowsAt(rows []row, at []int) []row {
	out := make([]row, 0, len(at))
	for _, i := range at {
		out = append(out, rows[i])
	}
	return out
}

// summaryTag matches the element's own tags in any case, which a summary's text could otherwise use to close it early or fake another.
var summaryTag = regexp.MustCompile(`(?i)</?\s*summary\s*>`)

// summaryText is a summary's text as one line holding neither of its element's tags.
func summaryText(text string) string {
	return strings.Join(strings.Fields(summaryTag.ReplaceAllString(text, " ")), " ")
}

// passageNote stands for a passage's rows from where it is given up to end, the index after the passage's last row.
type passageNote struct {
	text string
	end  int
}

// gapped is the rows keep marks, with a row naming the log lines of each run left out between them; a run that starts at a note gets the
// note for the rows the note covers and a row naming the rest.
func gapped(rows []row, keep []bool, notes map[int]passageNote, log saved) []row {
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
		if n, ok := notes[i]; ok && n.end > i {
			out = append(out, row{text: n.text, header: -1})
			i = min(n.end, j)
			continue
		}
		out = append(out, row{text: log.note(log.leftOut(rows[i:j])), header: -1})
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
			purpose = " for " + d.asked()
		}
	}
	if choice.Judged && choice.Whole >= decider.WholeThreshold {
		return fixedCut(rows, log, strings.Join(append([]string{who + " judged that " + d.asked() + " needs every line, so filter or count the " + log.whole() + " instead"}, notes...), "; "))
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
	seen := map[string]bool{}
	for _, r := range append(slices.Clone(rows[:decidedHead]), rows[end:]...) {
		seen[likeness(r.text)] = true
	}
	picks = novelFirst(picks, seen, func(p decider.Pick) []string {
		var texts []string
		for _, r := range rows[decidedHead+p.Start : min(end, decidedHead+p.End)] {
			texts = append(texts, r.text)
		}
		return texts
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
		return fixedCut(rows, log, strings.Join(append([]string{who + " found nothing in them" + purpose}, notes...), "; "))
	}
	out := gapped(rows, keep, nil, log)
	clauses := []string{who + " chose the middle" + purpose}
	if len(missed) > 0 {
		passages := "passages"
		if len(missed) == 1 {
			passages = "passage"
		}
		clauses = append(clauses, fmt.Sprintf("%d more %s it chose did not fit (log %s)", len(missed), passages, log.spans(missed)))
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
		clauses = append(clauses, fmt.Sprintf("log %s were not judged (%s)", log.spans(byReason[reason]), reason))
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
func (l saved) leftOut(rows []row) string {
	first, last := logLines(rows)
	if first == 0 {
		return fmt.Sprintf("%s rows left out", thousands(len(rows)))
	}
	return "log " + l.span(first, last) + " left out"
}

// spans lists ranges of log lines, merging overlaps, up to three.
func (l saved) spans(ranges [][2]int) string {
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
			parts = append(parts, fmt.Sprint(l.base+r[0]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", l.base+r[0], l.base+r[1]))
		}
	}
	if len(merged) == 1 && merged[0][0] == merged[0][1] {
		return "line " + parts[0]
	}
	return "lines " + strings.Join(parts, ", ")
}

// closing is the note that ends a cut: how much is shown, the clauses about what was chosen, and where the whole output is.
func closing(shown int, log saved, clauses []string) string {
	text := fmt.Sprintf("%s rows shown of %s log lines; ", thousands(shown), thousands(log.lines))
	for _, clause := range clauses {
		text += clause + "; "
	}
	if log.path == "" {
		return log.note(text + "the whole output could not be saved")
	}
	if log.holds < log.lines {
		limit := thousands(logLimit) + " bytes"
		if logLimit%(1<<20) == 0 {
			limit = fmt.Sprintf("%d MiB", logLimit>>20)
		}
		text += fmt.Sprintf("the log stops at its %s limit, in log line %s; ", limit, thousands(log.holds))
	}
	return log.note(text + log.whole() + ": " + log.path)
}
