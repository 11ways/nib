// Package decider chooses which rows of a long nibash output the model sees: the kinds of decider and their configuration, the keyword
// decider and the OpenRouter decision model. A decider only chooses among real rows; it never writes text the model sees.
package decider

import (
	"cmp"
	"context"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"
)

const (
	// PickThreshold is the score a pick needs to be shown.
	PickThreshold = 0.5
	// WholeThreshold is how sure a model must be that the purpose needs every line before a selection is refused as misleading.
	WholeThreshold = 0.7
	// KeywordScore ranks keyword hits below confident model picks, so a flood of hits cannot displace them.
	KeywordScore = 0.5
)

// Input is one over-budget output: the candidate rows between the start and end that are kept anyway, and what the call was for.
type Input struct {
	Command string
	// Purpose is the call's description; empty without one.
	Purpose string
	// Expect is what the agent said it expects or wants to find out; empty without it.
	Expect   string
	Exit     int
	TimedOut bool
	// Rows are the candidate rows as the model would see them: colorless, collapsed, capped; a row may name a Legend entry instead of its
	// text.
	Rows []string
	// Legend defines the names rows use for lines and blocks that repeat.
	Legend Legend
}

// Legend defines names (@L1 for a line, @B1 for a block) that a deduplicated output uses for what repeats in it.
type Legend map[string]string

// legendName finds the names a row uses.
var legendName = regexp.MustCompile(`@[LB]\d+`)

// Names are the names text uses that the legend defines, in the order the legend numbers them.
func (l Legend) Names(texts ...string) []string {
	seen := map[string]bool{}
	for _, text := range texts {
		for _, name := range legendName.FindAllString(text, -1) {
			if _, ok := l[name]; ok {
				seen[name] = true
			}
		}
	}
	names := slices.Collect(maps.Keys(seen))
	slices.SortFunc(names, func(a, b string) int {
		if a[1] != b[1] {
			return cmp.Compare(b[1], a[1])
		}
		x, _ := strconv.Atoi(a[2:])
		y, _ := strconv.Atoi(b[2:])
		return cmp.Compare(x, y)
	})
	return names
}

// Entry is one line of a deduplicated output: a log line as it is, or log lines First to Last standing for a legend name.
type Entry struct {
	First, Last int
	Text        string
}

// Label is the log lines the entry stands for, as a model reads them in front of it.
func (e Entry) Label() string {
	if e.Last > e.First {
		return strconv.Itoa(e.First) + "-" + strconv.Itoa(e.Last)
	}
	return strconv.Itoa(e.First)
}

// Range is a half-open range of Input.Rows.
type Range struct{ Start, End int }

// Pick is a range of rows and how much it matters, 0 to 1.
type Pick struct {
	Range
	Score float64
}

// Unjudged is a range no answer covers, and why.
type Unjudged struct {
	Range
	Reason string
}

// Usage is what a decision cost; zero without a model.
type Usage struct {
	Requests    int
	Cached      int
	InputTokens int
	Cost        float64
	Elapsed     time.Duration
}

// Choice is a decider's answer. Picks of equal score are taken in the order given.
type Choice struct {
	Picks []Pick
	// Judged says whether Whole, the probability that the purpose needs every line, was answered.
	Judged   bool
	Whole    float64
	Unjudged []Unjudged
	// Who names the decider for notes: "keywords", or the model and keywords.
	Who string
	// Failure is why the model did not decide at all, so keywords did; empty otherwise.
	Failure string
	// Model says a model chose, reading the purpose, so notes say what it chose for.
	Model bool
	Usage Usage
}

// Decider chooses among an output's candidate rows; a nil Decider is the fixed cut.
type Decider interface {
	Decide(ctx context.Context, in Input) Choice
	Kind() Kind
}
