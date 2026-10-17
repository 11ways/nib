package decider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	windowRows = 25
	// rowChars cuts each row for the question only; what is shown stays whole.
	rowChars = 400
	// requestBytes keeps a request under the 32,000 bytes past which Jev answers in its slower, less accurate long-context mode.
	requestBytes = 30000
	maxRequests  = 8
	// questionsVersion changes with the wording of the questions, so cached answers to other wording are not reused.
	questionsVersion = 1
	retryWait        = 200 * time.Millisecond
)

// inFlight caps requests across the process, since several agents' shells can render at once.
var inFlight = make(chan struct{}, 16)

// openRouter asks a decision model on OpenRouter which windows of the output matter, and adds the keyword hits to its picks; with several
// models, the next one answers when one fails.
type openRouter struct {
	config Config
	client *http.Client
	cache  *cache
	mu     sync.Mutex
	// off is why a model is off for the rest of the session, after an answer that retrying cannot change (401, 402, 403).
	off map[string]string
}

func newOpenRouter(c Config) *openRouter {
	client := c.Client
	if client == nil {
		client = &http.Client{}
	}
	return &openRouter{config: c, client: client, cache: newCache(c.Cache), off: map[string]string{}}
}

func (o *openRouter) Kind() Kind { return OpenRouter }

// window is a run of candidate rows under one name in the request.
type window struct {
	id   string
	rows Range
	text string
	size int
}

// batch is one request's windows, and whether it carries the whole question.
type batch struct {
	windows []window
	whole   bool
}

// outcome is one batch's answers, or why it has none.
type outcome struct {
	answers map[string]float64
	failure string
	usage   Usage
}

func (o *openRouter) Decide(ctx context.Context, in Input) Choice {
	fallback := Choice{Picks: keywordPicks(in.Rows), Who: "keywords"}
	// Without a purpose, a successful run gives the model nothing to judge by, so its cost buys nothing measured.
	if in.Purpose == "" && in.Exit == 0 && !in.TimedOut {
		return fallback
	}
	if ctx.Err() != nil {
		fallback.Failure = "cancelled"
		return fallback
	}
	started := time.Now()
	deadline, cancel := context.WithTimeout(ctx, o.config.Timeout)
	defer cancel()
	var usage Usage
	var failures []string
	for _, model := range o.config.Models {
		choice, failure := o.decideWith(deadline, in, model, fallback.Picks)
		usage.add(choice.Usage)
		if failure == "" {
			if len(failures) > 0 {
				choice.Who = fmt.Sprintf("%s and keywords (after %s)", model, strings.Join(failures, "; "))
			}
			choice.Usage = usage
			choice.Usage.Elapsed = time.Since(started)
			return choice
		}
		if len(o.config.Models) > 1 {
			failure = model + " failed: " + failure
		}
		failures = append(failures, failure)
		if deadline.Err() != nil {
			break
		}
	}
	fallback.Failure = strings.Join(failures, "; ")
	usage.Elapsed = time.Since(started)
	fallback.Usage = usage
	return fallback
}

// decideWith asks one model; a failure says why it gave no answer at all.
func (o *openRouter) decideWith(ctx context.Context, in Input, model string, hits []Pick) (Choice, string) {
	o.mu.Lock()
	off := o.off[model]
	o.mu.Unlock()
	if off != "" {
		return Choice{}, off
	}
	batches, unjudged := o.plan(in, model, hits)
	outcomes := make([]outcome, len(batches))
	var wait sync.WaitGroup
	for i, b := range batches {
		wait.Go(func() { outcomes[i] = o.ask(ctx, in, model, b) })
	}
	wait.Wait()
	choice := Choice{Who: model + " and keywords", Model: true}
	var failures []string
	for i, b := range batches {
		got := outcomes[i]
		choice.Usage.add(got.usage)
		if got.failure != "" {
			failures = append(failures, got.failure)
			for _, w := range b.windows {
				unjudged = append(unjudged, Unjudged{w.rows, got.failure})
			}
			continue
		}
		for _, w := range b.windows {
			choice.Picks = append(choice.Picks, Pick{w.rows, got.answers[w.id]})
		}
		if b.whole {
			choice.Judged, choice.Whole = true, got.answers["whole"]
		}
	}
	if len(failures) == len(batches) {
		return Choice{Usage: choice.Usage}, failures[0]
	}
	slices.SortStableFunc(choice.Picks, func(a, b Pick) int { return cmpScore(b.Score, a.Score) })
	choice.Picks = append(choice.Picks, hits...)
	slices.SortFunc(unjudged, func(a, b Unjudged) int { return a.Start - b.Start })
	choice.Unjudged = unjudged
	return choice, ""
}

// add counts other's requests and cost into u; the elapsed time is the caller's to set.
func (u *Usage) add(other Usage) {
	u.Requests += other.Requests
	u.Cached += other.Cached
	u.InputTokens += other.InputTokens
	u.Cost += other.Cost
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

// plan splits the rows into windows and packs them into at most maxRequests batches; past that, windows with keyword hits go first, then
// those nearest the end, then those nearest the start, and the rest are not judged.
func (o *openRouter) plan(in Input, model string, hits []Pick) ([]batch, []Unjudged) {
	var windows []window
	for start := 0; start < len(in.Rows); start += windowRows {
		end := min(start+windowRows, len(in.Rows))
		rows := make([]string, 0, end-start)
		for _, row := range in.Rows[start:end] {
			if len([]rune(row)) > rowChars {
				row = string([]rune(row)[:rowChars])
			}
			rows = append(rows, row)
		}
		w := window{id: "w" + strconv.Itoa(len(windows)), rows: Range{start, end}, text: strings.Join(rows, "\n")}
		w.size = jsonSize(w.text) + jsonSize(question(in, w.id)) + 2*len(w.id) + 16
		windows = append(windows, w)
	}
	base := jsonSize(request{Model: model, State: state{Command: in.Command, Purpose: in.Purpose, ExitStatus: in.Exit, TimedOut: in.TimedOut}})
	whole := 0
	if in.Purpose != "" {
		whole = jsonSize(wholeQuestion) + 16
	}
	pack := func(chosen []window) []batch {
		var batches []batch
		used := 0
		for _, w := range chosen {
			if len(batches) == 0 || used+w.size > requestBytes {
				batches = append(batches, batch{whole: len(batches) == 0 && whole > 0})
				used = base
				if len(batches) == 1 {
					used += whole
				}
			}
			last := &batches[len(batches)-1]
			last.windows = append(last.windows, w)
			used += w.size
		}
		return batches
	}
	if all := pack(windows); len(all) <= maxRequests {
		return all, nil
	}
	hit := map[int]bool{}
	for _, p := range hits {
		hit[p.Start/windowRows] = true
	}
	order := make([]int, len(windows))
	for i := range order {
		order[i] = i
	}
	last := len(windows) - 1
	slices.SortStableFunc(order, func(a, b int) int {
		if hit[a] != hit[b] {
			if hit[a] {
				return -1
			}
			return 1
		}
		da, db := min(last-a, a), min(last-b, b)
		if da != db {
			return da - db
		}
		return b - a
	})
	taken := map[int]bool{}
	for _, i := range order {
		taken[i] = true
		var chosen []window
		for j, w := range windows {
			if taken[j] {
				chosen = append(chosen, w)
			}
		}
		if len(pack(chosen)) > maxRequests {
			delete(taken, i)
		}
	}
	var chosen []window
	var unjudged []Unjudged
	for i, w := range windows {
		if taken[i] {
			chosen = append(chosen, w)
		} else if n := len(unjudged); n > 0 && unjudged[n-1].End == w.rows.Start {
			unjudged[n-1].End = w.rows.End
		} else {
			unjudged = append(unjudged, Unjudged{w.rows, fmt.Sprintf("over the %d-request limit", maxRequests)})
		}
	}
	return pack(chosen), unjudged
}

func jsonSize(v any) int {
	data, _ := json.Marshal(v)
	return len(data)
}

// The request, as the Decisions endpoint takes it; output keeps the windows in output order.
type request struct {
	Model     string   `json:"model"`
	Provider  provider `json:"provider"`
	State     state    `json:"state"`
	Questions ordered  `json:"questions"`
}

type provider struct {
	ZDR            bool   `json:"zdr"`
	DataCollection string `json:"data_collection"`
}

type state struct {
	Command    string  `json:"command"`
	Purpose    string  `json:"purpose,omitempty"`
	ExitStatus int     `json:"exit_status"`
	TimedOut   bool    `json:"timed_out,omitempty"`
	Output     ordered `json:"output"`
}

type noul struct {
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Criteria     criteria `json:"criteria"`
}

type criteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

// ordered is a JSON object that keeps its keys in the order they were added.
type ordered []field

type field struct {
	key   string
	value any
}

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(f.key)
		value, err := json.Marshal(f.value)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// question asks whether a window matters: for the purpose when there is one, else for the failure or the timeout.
func question(in Input, id string) noul {
	switch {
	case in.Purpose != "":
		return noul{"noul", fmt.Sprintf("Does `output.%s` contain a line the developer needs for `purpose`?", id), criteria{
			"It has a line that answers or directly bears on the purpose, such as an error, a failure or the entry asked for.",
			"It has only lines unrelated to the purpose, such as routine progress, timings, heartbeats, or warnings and errors that do not bear on it.",
		}}
	case in.TimedOut:
		return noul{"noul", fmt.Sprintf("Does `output.%s` contain a line the developer needs to see to find out why `command` did not finish before its timeout?", id), criteria{
			"It shows what the command was waiting on or stuck in, or an error or failure.",
			"It has only routine progress, timings or passing results.",
		}}
	}
	return noul{"noul", fmt.Sprintf("Does `output.%s` contain a line the developer needs to see to fix the failure of `command`?", id), criteria{
		"It has an error message, a failure, a stack trace or a failing assertion.",
		"It has only routine progress, timing, warnings that did not cause the failure, or passing results.",
	}}
}

var wholeQuestion = noul{"noul", "Does `purpose` need every line of the output, such as counting, totals or a complete list, so that showing only some parts would mislead?", criteria{
	"The purpose is to count, sum, list all, or compare across the whole output.",
	"The purpose is served by finding specific lines, such as an error, a failure or a particular entry.",
}}

func (o *openRouter) body(in Input, model string, b batch) request {
	r := request{Model: model, Provider: provider{ZDR: true, DataCollection: "deny"}, State: state{Command: in.Command, Purpose: in.Purpose, ExitStatus: in.Exit, TimedOut: in.TimedOut}}
	for _, w := range b.windows {
		r.State.Output = append(r.State.Output, field{w.id, w.text})
		r.Questions = append(r.Questions, field{w.id, question(in, w.id)})
	}
	if b.whole {
		r.Questions = append(r.Questions, field{"whole", wholeQuestion})
	}
	return r
}

// ids are the question ids a batch must answer.
func (b batch) ids() []string {
	var ids []string
	for _, w := range b.windows {
		ids = append(ids, w.id)
	}
	if b.whole {
		ids = append(ids, "whole")
	}
	return ids
}

// ask answers one batch from the cache, else from OpenRouter with one retry for answers that may change on their own.
func (o *openRouter) ask(ctx context.Context, in Input, model string, b batch) outcome {
	body, err := json.Marshal(o.body(in, model, b))
	if err != nil {
		return outcome{failure: "invalid request"}
	}
	key := cacheKey(o.config, body)
	if answers, ok := o.cache.get(key, b.ids()); ok {
		return outcome{answers: answers, usage: Usage{Cached: 1}}
	}
	var usage Usage
	for attempt := 0; ; attempt++ {
		answered, retryAfter, failure := o.post(ctx, model, body, b.ids(), &usage)
		if failure == "" {
			o.cache.put(key, entry{Version: questionsVersion, Answers: answered.answers, Model: answered.model, InputTokens: answered.tokens, Cost: answered.cost})
			return outcome{answers: answered.answers, usage: usage}
		}
		if retryAfter < 0 || attempt > 0 {
			return outcome{failure: failure, usage: usage}
		}
		if until, ok := ctx.Deadline(); ok && time.Until(until) < retryAfter+100*time.Millisecond {
			return outcome{failure: failure, usage: usage}
		}
		select {
		case <-ctx.Done():
			return outcome{failure: o.timeout(ctx), usage: usage}
		case <-time.After(retryAfter):
		}
	}
}

type answered struct {
	answers map[string]float64
	model   string
	tokens  int
	cost    float64
}

// response is decoded leniently: only the answers and usage are read, so a change elsewhere in the alpha schema does not matter.
type response struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Noul *float64 `json:"noul"`
	} `json:"answers"`
	Usage struct {
		InputTokens int     `json:"input_tokens"`
		Cost        float64 `json:"cost"`
	} `json:"usage"`
}

// post sends one request; retryAfter is how long to wait before the one retry, negative when retrying cannot help.
func (o *openRouter) post(ctx context.Context, model string, body []byte, ids []string, usage *Usage) (answered, time.Duration, string) {
	select {
	case inFlight <- struct{}{}:
		defer func() { <-inFlight }()
	case <-ctx.Done():
		return answered{}, -1, o.timeout(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.config.URL, bytes.NewReader(body))
	if err != nil {
		return answered{}, -1, "invalid request"
	}
	req.Header.Set("Authorization", "Bearer "+o.config.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "nib")
	usage.Requests++
	resp, err := o.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return answered{}, -1, o.timeout(ctx)
		}
		return answered{}, retryWait, "network error"
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if ctx.Err() != nil {
			return answered{}, -1, o.timeout(ctx)
		}
		return answered{}, retryWait, "network error"
	}
	if resp.StatusCode != http.StatusOK {
		failure := fmt.Sprintf("OpenRouter %d", resp.StatusCode)
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403:
			o.mu.Lock()
			o.off[model] = "the decision model is off for this session after " + failure
			o.mu.Unlock()
			return answered{}, -1, failure
		case resp.StatusCode == 429 || resp.StatusCode >= 500:
			wait := retryWait
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 {
				wait = time.Duration(seconds) * time.Second
			}
			return answered{}, wait, failure
		}
		return answered{}, -1, failure
	}
	var decoded response
	if json.Unmarshal(data, &decoded) != nil {
		return answered{}, -1, "invalid answer"
	}
	usage.InputTokens += decoded.Usage.InputTokens
	usage.Cost += decoded.Usage.Cost
	answers := map[string]float64{}
	for id, a := range decoded.Answers {
		if a.Noul != nil {
			answers[id] = *a.Noul
		}
	}
	if !valid(answers, ids) {
		return answered{}, -1, "invalid answer"
	}
	return answered{answers: answers, model: decoded.Model, tokens: decoded.Usage.InputTokens, cost: decoded.Usage.Cost}, 0, ""
}

// valid says whether answers hold a probability for every id.
func valid(answers map[string]float64, ids []string) bool {
	for _, id := range ids {
		p, ok := answers[id]
		if !ok || math.IsNaN(p) || p < 0 || p > 1 {
			return false
		}
	}
	return true
}

// timeout is the failure for a request its context ended: the deadline, or the call's cancellation.
func (o *openRouter) timeout(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout after " + o.config.Timeout.String()
	}
	return "cancelled"
}
