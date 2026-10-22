package decider

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBreaksAsksOneQuestionPerRuleAndCachesTheAnswer(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		state := req["state"].(map[string]any)
		if state["command"] != "./gradlew test" || state["purpose"] != "run the tests" || !slices.Equal(state["programs"].([]any), []any{"gradlew"}) {
			t.Errorf("state %v", state)
		}
		rules := state["rules"].(map[string]any)
		if rules["r0"] != "Do not run builds directly." || rules["r1"] != "Do not delete branches." {
			t.Errorf("rules %v", rules)
		}
		question := req["questions"].(map[string]any)["r0"].(map[string]any)
		if !strings.Contains(question["instructions"].(string), "`rules.r0`") || !strings.Contains(question["instructions"].(string), "`programs` are the programs it runs") {
			t.Errorf("question %v", question)
		}
		return 200, answerAll(req, func(id string) float64 {
			if id == "r0" {
				return 0.9
			}
			return 0.1
		})
	}}
	server := f.serve(t)
	o := openRouterFor(server, 5*time.Second, t.TempDir())
	q := RuleQuestion{Command: "./gradlew test", Programs: []string{"gradlew"}, Purpose: "run the tests", Rules: []string{"Do not run builds directly.", "Do not delete branches."}}
	verdict := o.Breaks(context.Background(), q)
	if verdict.Failure != "" || !slices.Equal(verdict.Broken, []int{0}) || verdict.Who != DefaultModel {
		t.Fatalf("verdict %+v", verdict)
	}
	again := o.Breaks(context.Background(), q)
	if f.count() != 1 || !slices.Equal(again.Broken, []int{0}) || again.Usage.Cached != 1 {
		t.Fatalf("the same command and rules must be answered from the cache: %d requests, %+v", f.count(), again)
	}
}

func TestBreaksFallsToTheNextModelAndSaysWhyNoneJudged(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		if req["model"] == "bad/model" {
			return 500, "{}"
		}
		return 200, answerAll(req, func(string) float64 { return 0.2 })
	}}
	server := f.serve(t)
	o := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{"bad/model", DefaultModel}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second})
	verdict := o.Breaks(context.Background(), RuleQuestion{Command: "ls", Rules: []string{"Do not run builds directly."}})
	if verdict.Failure != "" || len(verdict.Broken) != 0 || verdict.Who != DefaultModel {
		t.Fatalf("verdict %+v", verdict)
	}
	only := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{"bad/model"}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second})
	failed := only.Breaks(context.Background(), RuleQuestion{Command: "ls", Rules: []string{"Do not run builds directly."}})
	if failed.Failure != "OpenRouter 500" || failed.Broken != nil {
		t.Fatalf("verdict %+v", failed)
	}
}

func TestARuleCountsAsBrokenOnlyFromRuleThreshold(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		return 200, answerAll(req, func(id string) float64 {
			return map[string]float64{"r0": 0.6, "r1": RuleThreshold, "r2": 0.5}[id]
		})
	}}
	server := f.serve(t)
	o := openRouterFor(server, 5*time.Second, t.TempDir())
	verdict := o.Breaks(context.Background(), RuleQuestion{Command: "ls", Rules: []string{"a", "b", "c"}})
	if verdict.Failure != "" || !slices.Equal(verdict.Broken, []int{1}) {
		t.Fatalf("verdict %+v", verdict)
	}
}
