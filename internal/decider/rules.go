package decider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RuleJudge says which plain-language rules a command breaks; the OpenRouter decider is one, keywords are not.
type RuleJudge interface {
	Breaks(ctx context.Context, q RuleQuestion) RuleVerdict
}

// RuleQuestion is a command about to run and the rules that may forbid it.
type RuleQuestion struct {
	Command   string
	Purpose   string
	Directory string
	Rules     []string
}

// RuleVerdict is which rules a command breaks, by index; Failure says why no model judged them, and then Broken is empty.
type RuleVerdict struct {
	Broken  []int
	Who     string
	Failure string
	Usage   Usage
}

// ruleState is what a rules request tells the model: the command and the rules by id.
type ruleState struct {
	Command   string  `json:"command"`
	Purpose   string  `json:"purpose,omitempty"`
	Directory string  `json:"directory,omitempty"`
	Rules     ordered `json:"rules"`
}

type ruleRequest struct {
	Model     string    `json:"model"`
	Provider  provider  `json:"provider"`
	State     ruleState `json:"state"`
	Questions ordered   `json:"questions"`
}

// ruleQuestion asks whether running the command breaks one rule.
func ruleQuestion(id string) noul {
	return noul{"noul", fmt.Sprintf("Does running `command` break the rule in `rules.%s`?", id), criteria{
		"Running the command does what the rule forbids, or does it in the way the rule says not to.",
		"The command does not do what the rule is about, or does it the way the rule allows.",
	}}
}

// Breaks asks the models in order which rules q's command breaks; a rule counts as broken from PickThreshold on.
func (o *openRouter) Breaks(ctx context.Context, q RuleQuestion) RuleVerdict {
	if len(q.Rules) == 0 {
		return RuleVerdict{}
	}
	started := time.Now()
	deadline, cancel := context.WithTimeout(ctx, o.config.Timeout)
	defer cancel()
	state := ruleState{Command: q.Command, Purpose: q.Purpose, Directory: q.Directory}
	var questions ordered
	var ids []string
	for i, rule := range q.Rules {
		id := "r" + strconv.Itoa(i)
		ids = append(ids, id)
		state.Rules = append(state.Rules, field{id, rule})
		questions = append(questions, field{id, ruleQuestion(id)})
	}
	var verdict RuleVerdict
	var failures []string
	for _, model := range o.config.Models {
		o.mu.Lock()
		off := o.off[model]
		o.mu.Unlock()
		failure := off
		if failure == "" {
			end := o.begin("rules", model)
			got := o.ask(deadline, model, ruleRequest{Model: model, Provider: provider{ZDR: true, DataCollection: "deny"}, State: state, Questions: questions}, ids)
			end()
			verdict.Usage.add(got.usage)
			if failure = got.failure; failure == "" {
				for i, id := range ids {
					if got.answers[id] >= PickThreshold {
						verdict.Broken = append(verdict.Broken, i)
					}
				}
				verdict.Who = model
				verdict.Usage.Elapsed = time.Since(started)
				return verdict
			}
		}
		if len(o.config.Models) > 1 {
			failure = model + " failed: " + failure
		}
		failures = append(failures, failure)
		if deadline.Err() != nil {
			break
		}
	}
	verdict.Failure = strings.Join(failures, "; ")
	verdict.Usage.Elapsed = time.Since(started)
	return verdict
}
