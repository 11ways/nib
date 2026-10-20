package nibash

import (
	"context"
	"fmt"
	"strings"

	"github.com/11ways/nib/internal/decider"
)

const (
	// tailAdded and tailAddedBytes cap the rows a check adds after a tail's own output; past tailAdded, a summary takes their place when a
	// summarizer is configured.
	tailAdded      = 30
	tailAddedBytes = 8 * 1024
	// tailNoted is the fewest lines an agent's own tail must leave out before nib saves them and says so: a few the agent cut on purpose are
	// not worth a note naming a log.
	tailNoted = 10
)

// tailed is a tail at the model's end that left lines of a real program's output out: the whole output, saved to a log, and how many of its
// last lines the tail showed. added is what the check puts after the tail's own output, nil when it adds nothing.
type tailed struct {
	raw          string
	log          saved
	total, shown int
	added        []string
}

// earlier says how many lines the tail left out and where they are.
func (t *tailed) earlier() string {
	earlier := t.total - t.shown
	s := "s"
	if earlier == 1 {
		s = ""
	}
	where := ""
	if t.log.path != "" {
		where = " in " + t.log.path
	}
	return fmt.Sprintf("%s earlier line%s%s", thousands(earlier), s, where)
}

// note is the fact that ends the tail's output: the lines it left out, or in its place what the check added.
func (t *tailed) note() string {
	if t.added != nil {
		return strings.Join(t.added, "\n")
	}
	return "[" + t.earlier() + "]"
}

// check asks the decider which of the lines the tail left out the call needs, as for a long output of its own: only with a decision model or
// a summarizer, when they are past summaryDue's limits, and when the call has a description or failed. A few chosen passages are added as
// their log lines; more than tailAdded rows are a summary of the whole output with a summarizer, or else the best of them while they fit. A
// model that fails leaves the tail as it is, saying so.
func (t *tailed) check(ctx context.Context, d decision) {
	model := d.decider != nil && d.decider.Kind() == decider.OpenRouter
	if d.decider == nil || !model && d.summarizer == nil || d.purpose == "" && d.exit == 0 && !d.timedOut {
		return
	}
	lines := plainLines([]byte(t.raw))
	dropped := len(lines) - t.shown
	if dropped <= 0 {
		return
	}
	rows := make([]row, dropped)
	for i, line := range lines[:dropped] {
		rows[i] = plainRow(line, i+1, i+1, nil)
	}
	if !summaryDue(rows, strings.Join(lines[:dropped], "\n")) {
		return
	}
	entries, legend := dedupe(lines)
	choice := choose(ctx, rows, entries, legend, d, model)
	if choice.Failure != "" {
		t.added = []string{fmt.Sprintf("[%s; not checked, since the decision model failed (%s)]", t.earlier(), choice.Failure)}
		return
	}
	who := choice.Who
	if choice.Judged && choice.Whole >= decider.WholeThreshold {
		t.added = []string{fmt.Sprintf("[%s; %s judged that your description needs every line, so filter or count the log instead]", t.earlier(), who)}
		return
	}
	var picks []decider.Pick
	for _, p := range choice.Picks {
		if p.Score >= decider.PickThreshold && p.End > p.Start && p.End <= dropped {
			picks = append(picks, p)
		}
	}
	widen := func(p decider.Pick) (int, int) { return max(0, p.Start-pickContext), min(dropped, p.End+pickContext) }
	seen := map[string]bool{}
	for _, line := range lines[dropped:] {
		seen[likeness(line)] = true
	}
	picks = novelFirst(picks, seen, func(p decider.Pick) []string {
		from, to := widen(p)
		texts := make([]string, 0, to-from)
		for _, r := range rows[from:to] {
			texts = append(texts, r.text)
		}
		return texts
	})
	wanted := make([]bool, dropped)
	count := 0
	for _, p := range picks {
		from, to := widen(p)
		for i := from; i < to; i++ {
			if !wanted[i] {
				wanted[i] = true
				count++
			}
		}
	}
	if count == 0 {
		return
	}
	var clauses []string
	if count > tailAdded && d.summarizer != nil {
		chosen := entries
		if len(entries) > excerptLines {
			chosen = excerpt(rows, choice, entries)
		}
		var excerpted []int
		for _, e := range chosen {
			for n := e.First; n <= e.Last; n++ {
				excerpted = append(excerpted, n)
			}
		}
		s := d.summarizer.Summarize(ctx, decider.SummaryInput{Command: d.command, Purpose: d.purpose, Expect: d.expect, Exit: d.exit,
			TimedOut: d.timedOut, Log: lines, Excerpt: excerpted, Entries: chosen, Legend: legend})
		if s.Failure == "" {
			out, _ := summaryPassages(rows, s, t.log, lines)
			t.added = append([]string{"[" + t.earlier() + "; a summary of the whole output, then the lines it points at that the tail left out:]",
				out[0].text}, t.texts(inner(out[1:]))...)
			return
		}
		clauses = append(clauses, "no summary")
	}
	// Picks go in best first while they fit; one that does not fit whole shows its start.
	keep := make([]bool, dropped)
	shown, used := 0, 0
	var missed [][2]int
	for _, p := range picks {
		from, to := widen(p)
		for i := from; i < to; i++ {
			if keep[i] {
				continue
			}
			if shown+1 > tailAdded || used+len(rows[i].text)+1 > tailAddedBytes {
				missed = append(missed, [2]int{i + 1, to})
				break
			}
			keep[i] = true
			shown++
			used += len(rows[i].text) + 1
		}
	}
	if len(missed) > 0 {
		clauses = append([]string{fmt.Sprintf("log %s it chose did not fit", t.log.spans(missed))}, clauses...)
	}
	purpose := ""
	if choice.Model {
		purpose = " for the failure"
		if d.purpose != "" {
			purpose = " for your description"
		}
	}
	header := fmt.Sprintf("[%s; %s chose these of them%s", t.earlier(), who, purpose)
	for _, clause := range clauses {
		header += "; " + clause
	}
	t.added = append([]string{header + ":]"}, t.texts(inner(gapped(rows, keep, nil, t.log)))...)
}

// inner drops the rows of nib's own at either end of rows, the gaps before the first chosen line and after the last, which the header covers.
func inner(rows []row) []row {
	for len(rows) > 0 && !rows[0].plain {
		rows = rows[1:]
	}
	for len(rows) > 0 && !rows[len(rows)-1].plain {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// texts are the rows' texts, with the footer that explains cut rows when one is.
func (t *tailed) texts(rows []row) []string {
	out := make([]string, len(rows))
	capped := false
	for i, r := range rows {
		out[i] = r.text
		capped = capped || r.capped
	}
	if capped {
		out = append(out, cutFooter(t.log, nil))
	}
	return out
}
