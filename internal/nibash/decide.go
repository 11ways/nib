package nibash

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/js"
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
// the decider's cut; raw is the log's text. An output whose deduplicated entries fit (wholeFits) goes to the summarizer whole, with no
// decider asked unless the summary fails; a longer one goes as the excerpt of the decider's picks. Without a summary, output within the
// budget stays whole. A decision model and a summarizer read the output deduplicated (dedupe); the agent still gets its real rows. Joined
// skim output is cut file by file (assemble, fairFixed).
func shape(ctx context.Context, rows []row, raw string, log saved, d decision) []row {
	over := overBudget(rows)
	plain := !slices.ContainsFunc(rows, func(r row) bool { return !r.plain })
	model := d.decider != nil && d.decider.Kind() == decider.OpenRouter
	var lines []string
	var entries []decider.Entry
	var legend decider.Legend
	if plain && (model || d.summarizer != nil) {
		lines = plainLines([]byte(raw))
		entries, legend = dedupe(lines)
	}
	files := grouped(rows, log)
	from, to := chosenSpan(rows, files)
	decide := func() decider.Choice {
		return choose(ctx, rows[from:to], entries, legend, d, model)
	}
	// fileCut is output under file headers cut file by file, with why no decision model chose.
	fileCut := func(notes []string) []row {
		return fairFixed(rows, log, strings.Join(append(notes, expectAdvice(d, model)...), "; "))
	}
	// cut is what the output gets without a summary, choice the decider's answer when it was already asked. A decision model judges rows under
	// file headers only for a purpose, since keywords in code mean nothing; joined skim output is plain and judged as any output.
	cut := func(choice *decider.Choice, notes ...string) []row {
		if !over {
			return rows
		}
		if from == to || d.decider == nil || !plain && (files == nil || files.joined || !model || d.purpose == "" && d.expect == "") {
			switch {
			case files != nil && !files.joined:
				return fileCut(notes)
			case files != nil:
				return fairFixed(rows, log, strings.Join(notes, "; "))
			}
			return fixedCut(rows, log, "")
		}
		if choice == nil {
			c := decide()
			choice = &c
		}
		if !plain && !choice.Model {
			return fileCut(append(notes, "the decision model failed ("+choice.Failure+")"))
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
			Excerpt: shown, Entries: chosen, Legend: legend, Base: log.base, Heads: files.heads(rows)}
		s := d.summarizer.Summarize(ctx, in)
		return s, s.Failure == ""
	}
	// A list of the same few problems at many places, short enough to show whole, stays whole: each place is work the agent must do.
	if !over && problemList(lines) {
		return rows
	}
	if wholeFits(entries, legend) {
		if s, ok := ask(entries); ok {
			return summarized(rows, s, log, lines)
		}
		return cut(nil, "no summary")
	}
	// Collapsed repeats can leave a long log too few rows for a decider; its excerpt is then the start and end alone.
	var choice decider.Choice
	if to > from {
		choice = decide()
	}
	if choice.Judged && choice.Whole >= decider.WholeThreshold {
		return cut(&choice)
	}
	if s, ok := ask(excerpt(rows[from:to], choice, entries, files)); ok {
		return summarized(rows, s, log, lines)
	}
	return cut(&choice, "no summary")
}

// wholeFits says whether a summarizer reads all of a deduplicated output rather than an excerpt: within excerptLines entries, or within
// wholeBytes as it reads them, which many short files a skim joins can take while past excerptLines.
func wholeFits(entries []decider.Entry, legend decider.Legend) bool {
	if len(entries) <= excerptLines {
		return true
	}
	size := 0
	for _, e := range entries {
		size += len(e.Label()) + len(": ") + min(len(e.Text), decider.SummaryRowChars) + 1
	}
	for _, text := range legend {
		size += len(text) + 1
	}
	return size <= wholeBytes
}

// chosenSpan is the rows a decider chooses among: those between the kept start and end, none when output is too short to have been
// decided, and all of output by file, whose rows of nib's own are kept instead.
func chosenSpan(rows []row, files *fileGroups) (from, to int) {
	switch {
	case files != nil:
		return 0, len(rows)
	case len(rows) <= headRows+tailRows:
		return 0, 0
	}
	return decidedHead, len(rows) - decidedTail
}

// choose asks the decider which of middle, rows of plain output, the call needs; a decision model reads them as entries, the deduplicated
// log lines they stand for, and its answer is turned back into ranges of middle. Rows under file headers, which have no entries, it reads
// as they are.
func choose(ctx context.Context, middle []row, entries []decider.Entry, legend decider.Legend, d decision, model bool) decider.Choice {
	in := decider.Input{Command: d.command, Purpose: d.purpose, Expect: d.expect, Exit: d.exit, TimedOut: d.timedOut}
	first, last := logLines(middle)
	if !model || first == 0 || len(entries) == 0 {
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
// picked among middle, widened by excerptContext lines, in novelFirst order while they fit excerptLines entries. Of joined skim output (files
// set, middle all its rows) it holds the lines of nib's own rows instead of a start and end, and each file gets a fair share of the rest.
func excerpt(middle []row, choice decider.Choice, entries []decider.Entry, files *fileGroups) []decider.Entry {
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
	keep := make([]bool, len(entries))
	held := 0
	seen := map[string]bool{}
	hold := func(i int) {
		if !keep[i] {
			keep[i], held = true, held+1
			seen[likeness(entries[i].Text)] = true
		}
	}
	if files == nil {
		for i := range entries {
			if i < excerptHead || i >= len(entries)-excerptTail {
				hold(i)
			}
		}
	} else {
		for _, r := range middle {
			for n := r.first; r.header < 0 && n > 0 && n <= min(r.last, lines); n++ {
				hold(entryOf[n])
			}
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
	if files == nil {
		for _, p := range picks {
			var more []int
			for _, i := range widened(p) {
				if !keep[i] {
					more = append(more, i)
				}
			}
			if held+len(more) > excerptLines {
				continue
			}
			for _, i := range more {
				hold(i)
			}
		}
	} else {
		fileOf := files.ofLines(middle, lines)
		var pieces []piece
		for _, p := range picks {
			pieces = append(pieces, split(widened(p), func(i int) int { return fileOf[entries[i].First] })...)
		}
		fairPieces(pieces, files.count, keep, func(int) int { return 1 }, excerptLines-held)
	}
	var out []decider.Entry
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

// fileGroups is output by file: the file each row belongs to, counted from 0, and how many there are. Joined skim output's files are its
// files; any other output under file headers (a diff, grep's rows) is grouped by them. A row of nib's own (header -1) belongs with the file
// whose head row comes last before it.
type fileGroups struct {
	of    []int
	count int
	// joined says the output is a skim's joined files, cut by their lines; otherwise the files' rows are nib's own file rows, a diff's
	// cut by hunk.
	joined bool
}

// grouped is rows by file when they are joined skim output or sit under file headers, else nil.
func grouped(rows []row, log saved) *fileGroups {
	heads := map[int]bool{}
	for _, r := range rows {
		if r.header >= 0 {
			heads[r.header] = true
		}
	}
	if log.files == 0 && len(heads) == 0 {
		return nil
	}
	g := &fileGroups{of: make([]int, len(rows)), joined: log.files > 0}
	current := -1
	for i := range rows {
		if heads[i] || current < 0 {
			current++
		}
		g.of[i] = current
	}
	g.count = current + 1
	return g
}

// hunkStart says whether a row under a file's header is a diff hunk's header, which starts the hunk's rows.
func hunkStart(r row) bool { return r.header >= 0 && strings.HasPrefix(r.text, "@@ ") }

// hunkName is what git names a hunk by after its header's second @@, the enclosing function or declaration; "" when it names nothing.
func hunkName(text string) string {
	if i := strings.Index(text[3:], "@@"); i >= 0 {
		return strings.TrimSpace(text[3+i+2:])
	}
	return ""
}

// hunkSpans is where each hunk ends, by the row its header is: at the next hunk's header or the next row of nib's own.
func hunkSpans(rows []row) map[int]int {
	spans := map[int]int{}
	next := len(rows)
	for i := len(rows) - 1; i >= 0; i-- {
		switch {
		case rows[i].header < 0:
			next = i
		case hunkStart(rows[i]):
			spans[i], next = next, i
		}
	}
	return spans
}

// notes name what each gap in output under file headers leaves out, by the gap's first row: its hunks, counted with their added and removed
// lines and named as git names them, or its rows. None for joined skim output, whose gaps are log lines.
func (g *fileGroups) notes(rows []row, keep []bool, log saved) map[int]string {
	if g == nil || g.joined {
		return nil
	}
	notes := map[int]string{}
	for i := 0; i < len(rows); {
		if keep[i] {
			i++
			continue
		}
		j := i
		for j < len(rows) && !keep[j] {
			j++
		}
		notes[i] = log.note(leftOutRows(rows[i:j]))
		i = j
	}
	return notes
}

// hunkNamed caps how much of a hunk's name a note shows: git names a Go hunk by its whole function signature.
const hunkNamed = 40

// leftOutRows names rows of one file a gap leaves out: "3 hunks left out, +40 -3: send, sendMany, init", "the rest of this hunk and 2
// more left out, ...", or "12 rows left out" where they are no diff; up to three distinct names, each clipped at hunkNamed.
func leftOutRows(rows []row) string {
	hunks, added, removed := 0, 0, 0
	var names []string
	for _, r := range rows {
		switch {
		case hunkStart(r):
			hunks++
			name := hunkName(r.text)
			if js.Len(name) > hunkNamed {
				name = string([]rune(name)[:hunkNamed]) + "..."
			}
			if name != "" && len(names) < 3 && !slices.Contains(names, name) {
				names = append(names, name)
			}
		case strings.HasPrefix(r.text, "+"):
			added++
		case strings.HasPrefix(r.text, "-|"):
			removed++
		}
	}
	if hunks == 0 && added == 0 && removed == 0 {
		if len(rows) == 1 {
			return "1 row left out"
		}
		return fmt.Sprintf("%s rows left out", thousands(len(rows)))
	}
	text := ""
	switch {
	case hunks > 0 && hunkStart(rows[0]):
		text = fmt.Sprintf("%d hunks", hunks)
		if hunks == 1 {
			text = "1 hunk"
		}
	case hunks > 0:
		text = fmt.Sprintf("the rest of this hunk and %d more", hunks)
	default:
		text = "the rest of this hunk"
	}
	text += fmt.Sprintf(" left out, +%d -%d", added, removed)
	if len(names) > 0 {
		if len(names) < hunks {
			names = append(names, "...")
		}
		text += ": " + strings.Join(names, ", ")
	}
	return text
}

// heads are the log lines the files start at, the first of each file's first row; none for output that is not joined.
func (g *fileGroups) heads(rows []row) []int {
	if g == nil {
		return nil
	}
	var heads []int
	for i, r := range rows {
		if (i == 0 || g.of[i] != g.of[i-1]) && r.first > 0 {
			heads = append(heads, r.first)
		}
	}
	return heads
}

// ofLines is the file of each log line from 1 to lines, rows being all the output's rows; a line no row stands for, one the agent was
// shown before, goes with the line before it.
func (g *fileGroups) ofLines(rows []row, lines int) []int {
	of := make([]int, lines+1)
	at := make([]bool, lines+1)
	for i, r := range rows {
		for n := max(1, r.first); r.first > 0 && n <= min(r.last, lines); n++ {
			of[n], at[n] = g.of[i], true
		}
	}
	for n := 2; n <= lines; n++ {
		if !at[n] {
			of[n] = of[n-1]
		}
	}
	return of
}

// split is rows units [lo, hi) as pieces, one for each run of them in the same file.
func (g *fileGroups) split(lo, hi int) []piece {
	units := make([]int, 0, hi-lo)
	for i := lo; i < hi; i++ {
		units = append(units, i)
	}
	return split(units, func(i int) int { return g.of[i] })
}

// piece is what one pick adds to one file: units are rows or entries, as the caller counts them.
type piece struct {
	group int
	units []int
}

// split is units as pieces, one for each run of them in the same group.
func split(units []int, group func(int) int) []piece {
	var pieces []piece
	for _, u := range units {
		if k := len(pieces) - 1; k >= 0 && pieces[k].group == group(u) {
			pieces[k].units = append(pieces[k].units, u)
			continue
		}
		pieces = append(pieces, piece{group: group(u), units: []int{u}})
	}
	return pieces
}

// fairPieces takes pieces in order while each group stays within its share of budget, the shares split as decider.Shares splits what the groups
// want, so no file's picks crowd out another's; kept marks the units held already and gets those taken, and cost prices one unit. A piece
// past what its group has left gives up units from both ends until the rest fits, since a pick's middle is likeliest to hold what it was
// picked for. left are the runs of units the pieces could not add, and took says whether any piece was taken whole or in part.
func fairPieces(pieces []piece, groups int, kept []bool, cost func(unit int) int, budget int) (left [][]int, took bool) {
	want := make([]int, groups)
	wanted := slices.Clone(kept)
	for _, p := range pieces {
		for _, u := range p.units {
			if !wanted[u] {
				wanted[u] = true
				want[p.group] += cost(u)
			}
		}
	}
	shares := decider.Shares(want, budget, 0)
	used := make([]int, groups)
	for _, p := range pieces {
		var add []int
		size := 0
		for _, u := range p.units {
			if !kept[u] {
				add = append(add, u)
				size += cost(u)
			}
		}
		lo, hi := 0, len(add)
		for size > shares[p.group]-used[p.group] && lo < hi {
			if (hi-lo)%2 == 0 {
				size -= cost(add[lo])
				lo++
			} else {
				hi--
				size -= cost(add[hi])
			}
		}
		for _, u := range add[lo:hi] {
			kept[u] = true
		}
		used[p.group] += size
		took = took || hi > lo || len(add) == 0
		left = append(left, runs(add[:lo])...)
		left = append(left, runs(add[hi:])...)
	}
	return left, took
}

// runs splits ascending units into runs of consecutive ones.
func runs(units []int) [][]int {
	var out [][]int
	for _, u := range units {
		if k := len(out) - 1; k >= 0 && out[k][len(out[k])-1] == u-1 {
			out[k] = append(out[k], u)
			continue
		}
		out = append(out, []int{u})
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

// summarized shows a checked summary in place of a decider's cut: the summary's rows, then a closing note. lines are the log's lines.
func summarized(rows []row, s decider.Summary, log saved, lines []string) []row {
	out, shown := summaryPassages(rows, s, log, lines)
	return append(out, row{text: closing(shown, log, endingOf(nil, shown > 0), nil), header: -1})
}

// encodedRun is a stretch of a line that may be encoded data, base64 or hex, which no agent reads; encodedRuns keeps the ones that are.
var encodedRun = regexp.MustCompile(`[A-Za-z0-9+/_-]{100,}={0,2}`)

// encodedRuns are the byte ranges of line's encoded data: runs of encodedRun that mix letters and digits over at least 16 distinct characters,
// as hex and base64 do and a long word, a separator or a run of one character never does.
func encodedRuns(line string) [][]int {
	var runs [][]int
	for _, run := range encodedRun.FindAllStringIndex(line, -1) {
		text := line[run[0]:run[1]]
		distinct := map[rune]bool{}
		for _, r := range text {
			distinct[r] = true
		}
		if len(distinct) >= 16 && strings.ContainsAny(text, "0123456789") && strings.ContainsFunc(text, unicode.IsLetter) {
			runs = append(runs, run)
		}
	}
	return runs
}

// briefRow is a passage's row of one log line, line, at most width characters: one longer, or holding encoded data, shows as briefLine does.
func briefRow(r row, line string, label, width int) row {
	if !r.capped {
		line = r.text
	}
	if text, cut := briefLine(label, line, width); cut {
		r.text, r.capped = text, true
	}
	return r
}

// briefLine is line numbered label as at most width characters of it outside its encoded runs, a bracketed row naming the stretches it shows,
// which is never an anchor; cut is false, and the line whole, when it is that short and holds no encoded data.
func briefLine(label int, line string, width int) (text string, cut bool) {
	runs := encodedRuns(line)
	if js.Len(line) <= width && len(runs) == 0 {
		return line, false
	}
	var kept []engine.Stretch
	at := 0
	for _, run := range append(runs, []int{len(line), len(line)}) {
		if run[0] > at {
			kept = append(kept, engine.Stretch{Start: js.Len(line[:at]), End: js.Len(line[:run[0]])})
		}
		at = run[1]
	}
	text, _ = engine.KeptRow(label, line, kept, width)
	return text, true
}

// summaryPassages are a checked summary as a <summary> element, then every passage it points at, each widened by summaryContext lines and given
// a share of the rows log.passages gives; a passage longer than its share drops its context lines first, then shows the start of the lines
// pointed at, with a note on the rest. shown counts the passages' rows.
func summaryPassages(rows []row, s decider.Summary, log saved, lines []string) (out []row, shown int) {
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
	sizes := make([]int, len(passages))
	for k, p := range passages {
		sizes[k] = len(p)
	}
	budgetRows, budget := log.passages()
	shares := decider.Shares(sizes, budgetRows, summaryShare)
	keep := make([]bool, len(rows))
	notes := map[int]string{}
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
			notes[add[share]] = log.note("log " + log.span(first, last) + " left out, the rest of this passage")
		}
	}
	// The passages share the budget, each row as wide as its share; a longer line shows briefly, and the log holds it whole.
	width := min(passageColumns, max(passageLeast, budget/max(1, shown)))
	brief := slices.Clone(rows)
	for i, r := range brief {
		if keep[i] && r.plain && r.first > 0 && r.first == r.last && r.first <= len(lines) {
			brief[i] = briefRow(r, lines[r.first-1], log.base+r.first, width)
		}
	}
	out = append([]row{{text: "<summary>" + summaryText(s.Text) + "</summary>", header: -1}}, gapped(brief, keep, notes, log, true)...)
	return out, shown
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

// gapped is the rows keep marks, each numbered with the log lines it stands for when numbered, so the numbers show every gap; a gap that
// starts at a note gets the note, and one whose rows stand for no log line a row counting them. A kept row under a header row (a file's
// head in joined skim output) that is not the last one shown gets it first.
func gapped(rows []row, keep []bool, notes map[int]string, log saved, numbered bool) []row {
	number := func(r row) row {
		if numbered {
			return log.numbered(r)
		}
		return r
	}
	heads := map[int]bool{}
	for _, r := range rows {
		if r.header >= 0 {
			heads[r.header] = true
		}
	}
	var out []row
	under := -1
	for i := 0; i < len(rows); {
		if keep[i] {
			if h := rows[i].header; heads[i] {
				under = i
			} else if h >= 0 && h != under {
				out = append(out, number(rows[h]))
				under = h
			}
			out = append(out, number(rows[i]))
			i++
			continue
		}
		j := i
		for j < len(rows) && !keep[j] {
			j++
		}
		if note, ok := notes[i]; ok {
			out = append(out, row{text: note, header: -1})
		} else if first, _ := logLines(rows[i:j]); first == 0 {
			out = append(out, row{text: log.note(log.leftOut(rows[i:j])), header: -1})
		}
		i = j
	}
	return out
}

// numberedSize is the bytes rows take once numbered with their log lines.
func numberedSize(rows []row, log saved) int {
	n := 0
	for _, r := range rows {
		n += len(log.numbered(r).printed()) + 1
	}
	return n
}

// assemble keeps the start and end and adds the picks that reach the threshold, best first and widened, while they fit the budget; every row
// is numbered with its log lines, so the numbers show each gap, and a closing note says who chose, with notes added to it. Of joined skim
// output it keeps the rows of nib's own instead of a start and end, and each file's picks get a fair share of the budget.
func assemble(rows []row, choice decider.Choice, log saved, d decision, notes ...string) []row {
	who := choice.Who
	if choice.Failure != "" {
		who = "the decision model failed (" + choice.Failure + "), so " + who
	}
	purpose := ""
	if choice.Model {
		purpose = " for the failure"
		if d.purpose != "" || d.expect != "" {
			purpose = " for " + d.asked()
		}
	}
	files := grouped(rows, log)
	fixed := func(because string) []row {
		clauses := append([]string{because}, notes...)
		if files != nil && !files.joined {
			clauses = append(clauses, expectAdvice(d, choice.Model)...)
		}
		because = strings.Join(clauses, "; ")
		if files != nil {
			return fairFixed(rows, log, because)
		}
		return fixedCut(rows, log, because)
	}
	if choice.Judged && choice.Whole >= decider.WholeThreshold {
		if files != nil && !files.joined {
			return fixed(who + " judged that " + d.asked() + " needs every line")
		}
		return fixed(who + " judged that " + d.asked() + " needs every line, so filter or count the " + log.whole() + " instead")
	}
	from, to := chosenSpan(rows, files)
	keep := make([]bool, len(rows))
	seen := map[string]bool{}
	for i, r := range rows {
		if keep[i] = i < from || i >= to || files != nil && r.header < 0; keep[i] {
			seen[likeness(r.text)] = true
		}
	}
	var picks []decider.Pick
	for _, p := range choice.Picks {
		if p.Score >= decider.PickThreshold {
			picks = append(picks, p)
		}
	}
	picks = novelFirst(picks, seen, func(p decider.Pick) []string {
		var texts []string
		for _, r := range rows[from+p.Start : min(to, from+p.End)] {
			texts = append(texts, r.text)
		}
		return texts
	})
	// widen is a pick with pickContext rows on each side, which in joined skim output stop at the pick's own files.
	widen := func(p decider.Pick) (int, int) {
		lo, hi := max(from, from+p.Start-pickContext), min(to, from+p.End+pickContext)
		for files != nil && lo < from+p.Start && files.of[lo] != files.of[from+p.Start] {
			lo++
		}
		for files != nil && hi > from+p.End && files.of[hi-1] != files.of[from+p.End-1] {
			hi--
		}
		return lo, hi
	}
	chose := false
	var missed [][2]int
	if files == nil {
		shown := from + len(rows) - to
		used := numberedSize(rows[:from], log) + numberedSize(rows[to:], log)
		for _, p := range picks {
			lo, hi := widen(p)
			add, addBytes := 0, 0
			for i := lo; i < hi; i++ {
				if !keep[i] {
					add++
					addBytes += numberedSize(rows[i:i+1], log)
				}
			}
			if add == 0 {
				chose = chose || lo < hi
				continue
			}
			if shown+add > headRows+tailRows || used+addBytes > budgetBytes {
				if first, last := logLines(rows[lo:hi]); first > 0 {
					missed = append(missed, [2]int{first, last})
				}
				continue
			}
			for i := lo; i < hi; i++ {
				keep[i] = true
			}
			shown += add
			used += addBytes
			chose = true
		}
	} else {
		var pieces []piece
		spans := hunkSpans(rows)
		for _, p := range picks {
			lo, hi := widen(p)
			// A diff's pick brings its hunk's header, the one row that says where in the file it is.
			for h := from + p.Start; !files.joined && h >= 0 && rows[h].header >= 0; h-- {
				if _, ok := spans[h]; ok {
					if h < lo {
						pieces = append(pieces, piece{group: files.of[h], units: []int{h}})
					}
					break
				}
			}
			pieces = append(pieces, files.split(lo, hi)...)
		}
		cost := rowCost(rows, log)
		budget := budgetBytes
		for i, k := range keep {
			if k {
				budget -= cost(i)
			}
		}
		var left [][]int
		left, chose = fairPieces(pieces, files.count, keep, cost, budget)
		var still []int
		for _, run := range left {
			for _, u := range run {
				if !keep[u] {
					still = append(still, u)
				}
			}
		}
		slices.Sort(still)
		for _, run := range runs(slices.Compact(still)) {
			if first, last := logLines(rowsAt(rows, run)); first > 0 {
				missed = append(missed, [2]int{first, last})
			}
		}
	}
	if !chose && len(missed) == 0 {
		return fixed(who + " found nothing in them" + purpose)
	}
	shown := 0
	for _, k := range keep {
		if k {
			shown++
		}
	}
	out := gapped(rows, keep, files.notes(rows, keep, log), log, files == nil || files.joined)
	clauses := []string{who + " chose the middle" + purpose}
	if files != nil {
		clauses[0] = who + " chose" + purpose + ", each file within its share of the budget"
	}
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
		first, last := logLines(rows[from+u.Start : from+u.End])
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
	return append(out, row{text: closing(shown, log, endingOf(files, true), append(clauses, notes...)), header: -1})
}

// rowCost prices row i of rows as a cut of joined skim output counts it: its bytes once numbered, but at least what keeps a cut within
// the bytes of budgetBytes also within headRows+tailRows rows.
func rowCost(rows []row, log saved) func(i int) int {
	return func(i int) int { return max(numberedSize(rows[i:i+1], log), budgetBytes/(headRows+tailRows)) }
}

// fairFixed is output by file past the budget without a decider's picks: the rows of nib's own, and each file from its start for as long
// as its fair share of the budget goes, a diff's by whole hunks once its first is shown; because, when set, says why no decider chose.
func fairFixed(rows []row, log saved, because string) []row {
	files := grouped(rows, log)
	cost := rowCost(rows, log)
	keep := make([]bool, len(rows))
	want := make([]int, files.count)
	budget := budgetBytes
	for i, r := range rows {
		if r.header < 0 {
			keep[i] = true
			budget -= cost(i)
		} else {
			want[files.of[i]] += cost(i)
		}
	}
	if budget < 0 {
		// What is always kept is past the budget by itself, as a diff of deleted files, which print as plain rows, can be.
		return fixedCut(rows, log, because)
	}
	spans := hunkSpans(rows)
	hunkCost := func(i int) int {
		n := 0
		for k := i; k < spans[i]; k++ {
			n += cost(k)
		}
		return n
	}
	shares := decider.Shares(want, budget, 0)
	used := make([]int, files.count)
	full := make([]bool, files.count)
	begun := make([]bool, files.count)
	shown := 0
	for i, r := range rows {
		g := files.of[i]
		switch {
		case r.header < 0:
		case full[g]:
			continue
		case hunkStart(r) && begun[g] && hunkCost(i) > shares[g]-used[g], used[g]+cost(i) > shares[g]:
			full[g] = true
			continue
		default:
			keep[i] = true
			used[g] += cost(i)
			begun[g] = begun[g] || hunkStart(r)
		}
		shown++
	}
	clauses := []string{"each file shown from its start as far as its share of the budget goes"}
	if len(spans) > 0 {
		clauses[0] = "each file's hunks shown from its start, whole after the first, as far as its share of the budget goes"
	}
	if because != "" {
		clauses = append(clauses, because)
	}
	return append(gapped(rows, keep, files.notes(rows, keep, log), log, files.joined), row{text: closing(shown, log, endingOf(files, true), clauses), header: -1})
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

// How a closing note ends: with where the whole output is; with that and how to read the lines N| numbers; or, for output under file
// headers, whose log is git's or a program's plain text and not rows, with how to get a file left out.
const (
	logEnding = iota
	numberedEnding
	fileEnding
)

// endingOf is the ending for output by file, or numberedEnding when numbered says the rows carry their log lines, else logEnding.
func endingOf(files *fileGroups, numbered bool) int {
	switch {
	case files != nil && !files.joined:
		return fileEnding
	case numbered:
		return numberedEnding
	}
	return logEnding
}

// closing is the note that ends a cut: how much is shown, the clauses about what was chosen, and where the whole output is, as ending says.
func closing(shown int, log saved, ending int, clauses []string) string {
	text := fmt.Sprintf("%s rows shown of %s log lines; ", thousands(shown), thousands(log.lines))
	for _, clause := range clauses {
		text += clause + "; "
	}
	if log.path == "" {
		if ending == numberedEnding {
			text += "N| is a row's log line; "
		}
		return log.note(text + "the whole output could not be saved")
	}
	if log.holds < log.lines {
		limit := thousands(logLimit) + " bytes"
		if logLimit%(1<<20) == 0 {
			limit = fmt.Sprintf("%d MiB", logLimit>>20)
		}
		text += fmt.Sprintf("the log stops at its %s limit, in log line %s; ", limit, thousands(log.holds))
	}
	switch ending {
	case numberedEnding:
		return log.note(text + "N| is a row's line in the " + log.whole() + ": read " + log.path + ":A-B plain:true for others")
	case fileEnding:
		return log.note(text + "for a file left out, run the command for its path; " + log.path + " holds the whole output as plain text, not rows")
	}
	return log.note(text + log.whole() + ": " + log.path)
}

// expectAdvice is the clause telling the agent that an expect lets a decision model choose rows under file headers: given when one is
// configured (model) and the call gave none.
func expectAdvice(d decision, model bool) []string {
	if !model || d.expect != "" {
		return nil
	}
	return []string{"an expect naming what matters lets the decision model choose the rows"}
}
