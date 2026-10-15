// Package decider chooses which rows of a long nibash output the model sees: the kinds of decider and their configuration, the keyword
// decider and the OpenRouter decision model. A decider only chooses among real rows; it never writes text the model sees.
package decider

import (
	"context"
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
	Purpose  string
	Exit     int
	TimedOut bool
	// Rows are the candidate rows as the model would see them: colorless, collapsed, capped.
	Rows []string
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
