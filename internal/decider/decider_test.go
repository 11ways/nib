package decider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestParseReadsEveryVariableAndRefusesWhatItCannotHonour(t *testing.T) {
	c, err := Parse(env(nil))
	if err != nil || c.Kind != DefaultKind || c.New() == nil && DefaultKind != Fixed {
		t.Fatalf("default: %+v %v", c, err)
	}
	c, err = Parse(env(map[string]string{"NIB_DECIDER": "openrouter", "OPENROUTER_API_KEY": "k1", "NIB_DECIDER_MODEL": "m", "NIB_DECIDER_URL": "http://x", "NIB_DECIDER_TIMEOUT": "500", "NIB_DECIDER_CACHE": "/c"}))
	if err != nil || c.Key != "k1" || !slices.Equal(c.Models, []string{"m"}) || c.URL != "http://x" || c.Timeout != 500*time.Millisecond || c.Cache != "/c" {
		t.Fatalf("openrouter: %+v %v", c, err)
	}
	if c, _ := Parse(env(map[string]string{"NIB_DECIDER": "openrouter", "OPENROUTER_API_KEY": "k1", "NIB_DECIDER_KEY": "k2"})); c.Key != "k2" {
		t.Fatalf("NIB_DECIDER_KEY must win: %+v", c)
	}
	if c, err := Parse(env(map[string]string{"NIB_DECIDER": "openrouter", "NIB_DECIDER_KEY": "k", "NIB_SANDBOX": "1", "NIB_DECIDER_SANDBOX": "1"})); err != nil || c.Kind != OpenRouter {
		t.Fatalf("sandbox allowed: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"NIB_DECIDER": "magic"},
		{"NIB_DECIDER": "openrouter"},
		{"NIB_DECIDER": "openrouter", "NIB_DECIDER_KEY": "k", "NIB_SANDBOX": "1"},
		{"NIB_DECIDER": "openrouter", "NIB_DECIDER_KEY": "k", "NIB_DECIDER_TIMEOUT": "soon"},
	} {
		if _, err := Parse(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	if _, err := Parse(env(map[string]string{"NIB_DECIDER": "magic"})); !strings.Contains(err.Error(), "fixed, keywords, openrouter") {
		t.Errorf("the error must list the kinds: %v", err)
	}
}

func TestEveryKindBuildsItsDecider(t *testing.T) {
	for _, kind := range Kinds() {
		d := Config{Kind: kind, Timeout: time.Second}.New()
		if kind == Fixed {
			if d != nil {
				t.Errorf("fixed must be nil")
			}
			continue
		}
		if d == nil || d.Kind() != kind || kind.Cut() == "" {
			t.Errorf("%s builds %v", kind, d)
		}
	}
}

func TestKeywordsHitProblemsNotNamesOrZeroCounts(t *testing.T) {
	hits := []string{"error: unsupported format", "[worker] error: x", "Build FAILED", "test failed", "panic: runtime error", "Traceback (most recent call last):",
		"E   assert 1 == 2", "FAIL src/a.test.ts", "not ok 3 - parses", "Expected: 3", "  Received: 4", "\u2717 large reads", "TypeError: x is not a function",
		"java.io.IOException: closed", "fatal: not a git repository", "2 errors"}
	misses := []string{"compiled src/errors.ts", "ok error-handling.test.ts", "0 errors", "found no errors", "warning: unused variable", "Errors.java:12 ok",
		"transform src/module1.ts 82ms", "done: 377 modules"}
	for _, row := range hits {
		if !Hit(row) {
			t.Errorf("%q should hit", row)
		}
	}
	for _, row := range misses {
		if Hit(row) {
			t.Errorf("%q should not hit", row)
		}
	}
	picks := keywordPicks([]string{"a", "error one", "b", "error two", "error three"}, nil)
	if len(picks) != 3 || picks[0].Start != 4 || picks[1].Start != 1 || picks[2].Start != 3 || picks[0].Score != KeywordScore {
		t.Fatalf("picks %+v", picks)
	}
}

// fake is a Decisions endpoint: answer gets each decoded request and returns a status and body.
type fake struct {
	mu       sync.Mutex
	requests []map[string]any
	answer   func(n int, req map[string]any) (int, string)
}

func (f *fake) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(data, &req); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		if r.Header.Get("Authorization") != "Bearer key" || r.URL.Path != "/api/alpha/decisions" {
			t.Errorf("auth %q, path %q", r.Header.Get("Authorization"), r.URL.Path)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		n := len(f.requests)
		f.mu.Unlock()
		status, body := f.answer(n, req)
		if status == 429 {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// answerAll answers every question of a request with p, or with score(id) when given.
func answerAll(req map[string]any, score func(id string) float64) string {
	answers := map[string]any{}
	for id := range req["questions"].(map[string]any) {
		answers[id] = map[string]any{"type": "noul", "noul": score(id)}
	}
	data, _ := json.Marshal(map[string]any{"id": "gen", "model": "typesafe/jev-1.14-20261014", "answers": answers, "usage": map[string]any{"input_tokens": 100, "output_tokens": 5, "cost": 0.0001}})
	return string(data)
}

func rowsOf(n int, special map[int]string) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("transform src/module%d.ts %dms", i, i%90)
		if text, ok := special[i]; ok {
			rows[i] = text
		}
	}
	return rows
}

func openRouterFor(server *httptest.Server, timeout time.Duration, cache string) *openRouter {
	return newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{DefaultModel}, URL: server.URL + "/api/alpha/decisions", Timeout: timeout, Cache: cache})
}

func TestRequestsNameWindowsAndAskTheWholeQuestionOnce(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		return 200, answerAll(req, func(id string) float64 {
			if id == "w4" {
				return 0.97
			}
			return 0.05
		})
	}}
	o := openRouterFor(f.serve(t), 5*time.Second, "")
	rows := rowsOf(3000, map[int]string{110: "[worker] build stopped: format \"cjs\" is not supported"})
	choice := o.Decide(context.Background(), Input{Command: "bun scripts/build.ts", Purpose: "find out why the build fails", Exit: 1, Rows: rows})
	if f.count() < 2 || f.count() > maxRequests {
		t.Fatalf("%d requests", f.count())
	}
	wholes := 0
	for i, req := range f.requests {
		data, _ := json.Marshal(req)
		if len(data) > requestBytes+2000 {
			t.Errorf("request %d is %d bytes", i, len(data))
		}
		if p := req["provider"].(map[string]any); p["zdr"] != true || p["data_collection"] != "deny" {
			t.Errorf("provider %v", p)
		}
		state := req["state"].(map[string]any)
		if state["command"] != "bun scripts/build.ts" || state["purpose"] != "find out why the build fails" || state["exit_status"] != 1.0 {
			t.Errorf("state %v", state)
		}
		questions := req["questions"].(map[string]any)
		if _, ok := questions["whole"]; ok {
			wholes++
		}
		for id, q := range questions {
			if id == "whole" {
				continue
			}
			if !strings.Contains(q.(map[string]any)["instructions"].(string), "`output."+id+"`") {
				t.Errorf("question %s: %v", id, q)
			}
			if _, ok := state["output"].(map[string]any)[id]; !ok {
				t.Errorf("no window %s", id)
			}
		}
	}
	if wholes != 1 || !choice.Judged || choice.Whole != 0.05 || !choice.Model || choice.Who != "typesafe/jev-1.14 and keywords" || choice.Failure != "" {
		t.Fatalf("choice %+v", choice)
	}
	if top := choice.Picks[0]; top.Start != 100 || top.End != 125 || top.Score != 0.97 {
		t.Fatalf("top pick %+v", top)
	}
	if choice.Usage.Requests != f.count() || choice.Usage.InputTokens != 100*f.count() {
		t.Fatalf("usage %+v", choice.Usage)
	}
	// The windows go in output order, as named fields.
	data, _ := json.Marshal(o.body(Input{Command: "c", Rows: rows[:60], Exit: 1}, DefaultModel, batch{windows: []window{{id: "w0", text: "a"}, {id: "w1", text: "b"}, {id: "w10", text: "c"}}}))
	if !strings.Contains(string(data), `"output":{"w0":"a","w1":"b","w10":"c"}`) || strings.Contains(string(data), `"purpose"`) || strings.Contains(string(data), "whole") {
		t.Fatalf("body %s", data)
	}
}

func TestTheRequestCapLeavesWindowsUnjudgedKeywordsFirst(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		return 200, answerAll(req, func(string) float64 { return 0.1 })
	}}
	o := openRouterFor(f.serve(t), 5*time.Second, "")
	wide := strings.Repeat("x", 300)
	rows := make([]string, 6000)
	for i := range rows {
		rows[i] = fmt.Sprintf("%d %s", i, wide)
	}
	rows[2000] = "error: the cause"
	choice := o.Decide(context.Background(), Input{Command: "c", Exit: 1, Rows: rows})
	if f.count() != maxRequests || len(choice.Unjudged) == 0 {
		t.Fatalf("%d requests, unjudged %+v", f.count(), choice.Unjudged)
	}
	for _, u := range choice.Unjudged {
		if u.Start <= 2000 && 2000 < u.End {
			t.Fatalf("the keyword window was not judged: %+v", choice.Unjudged)
		}
		if u.Reason != "over the 8-request limit" {
			t.Fatalf("reason %q", u.Reason)
		}
	}
	judged := map[int]bool{}
	for _, p := range choice.Picks {
		judged[p.Start] = true
	}
	if !judged[0] || !judged[5975] {
		t.Fatalf("the windows nearest the start and end must be judged")
	}
}

func TestFailuresFallBackToKeywordsAndPartialOnesAreNamed(t *testing.T) {
	rows := rowsOf(3000, map[int]string{1500: "error: boom"})
	in := Input{Command: "make", Exit: 2, Rows: rows}
	cases := []struct {
		name     string
		answer   func(n int, req map[string]any) (int, string)
		failure  string
		requests int
	}{
		{"429 is retried once", func(n int, req map[string]any) (int, string) {
			return 429, `{"error":{"code":429,"message":"slow down"}}`
		}, "OpenRouter 429", 2},
		{"400 is not retried", func(int, map[string]any) (int, string) { return 400, `{"error":{"code":400,"message":"bad"}}` }, "OpenRouter 400", 1},
		{"a malformed answer is invalid", func(int, map[string]any) (int, string) { return 200, `{"answers":{"w0":{"noul":"yes"}}}` }, "invalid answer", 1},
		{"a probability out of range is invalid", func(_ int, req map[string]any) (int, string) {
			return 200, answerAll(req, func(string) float64 { return 1.5 })
		}, "invalid answer", 1},
		{"a missing answer is invalid", func(int, map[string]any) (int, string) { return 200, `{"answers":{},"usage":{}}` }, "invalid answer", 1},
		{"502 is retried once", func(int, map[string]any) (int, string) { return 502, "" }, "OpenRouter 502", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fake{answer: c.answer}
			o := openRouterFor(f.serve(t), 5*time.Second, "")
			small := in
			small.Rows = rows[:50]
			choice := o.Decide(context.Background(), small)
			if choice.Failure != c.failure || choice.Who != "keywords" || choice.Model || f.count() != c.requests {
				t.Fatalf("choice %+v after %d requests", choice, f.count())
			}
		})
	}
	t.Run("one failed request leaves its windows unjudged", func(t *testing.T) {
		f := &fake{answer: func(_ int, req map[string]any) (int, string) {
			if _, ok := req["state"].(map[string]any)["output"].(map[string]any)["w0"]; ok {
				return 400, "{}"
			}
			return 200, answerAll(req, func(string) float64 { return 0.9 })
		}}
		o := openRouterFor(f.serve(t), 5*time.Second, "")
		choice := o.Decide(context.Background(), Input{Command: "make", Purpose: "p", Exit: 2, Rows: rows})
		if choice.Failure != "" || !choice.Model || choice.Judged || len(choice.Unjudged) == 0 || choice.Unjudged[0].Start != 0 || choice.Unjudged[0].Reason != "OpenRouter 400" {
			t.Fatalf("choice %+v", choice)
		}
	})
	t.Run("401 turns the model off for the session", func(t *testing.T) {
		f := &fake{answer: func(int, map[string]any) (int, string) { return 401, "{}" }}
		o := openRouterFor(f.serve(t), 5*time.Second, "")
		small := in
		small.Rows = rows[:50]
		o.Decide(context.Background(), small)
		choice := o.Decide(context.Background(), small)
		if f.count() != 1 || choice.Failure != "the decision model is off for this session after OpenRouter 401" {
			t.Fatalf("%d requests, %+v", f.count(), choice)
		}
	})
	t.Run("the deadline cancels requests", func(t *testing.T) {
		f := &fake{answer: func(int, map[string]any) (int, string) {
			time.Sleep(500 * time.Millisecond)
			return 200, "{}"
		}}
		o := openRouterFor(f.serve(t), 100*time.Millisecond, "")
		started := time.Now()
		choice := o.Decide(context.Background(), Input{Command: "make", Exit: 2, Rows: rows[:50]})
		if choice.Failure != "timeout after 100ms" || time.Since(started) > 400*time.Millisecond {
			t.Fatalf("%+v after %s", choice, time.Since(started))
		}
	})
}

func TestTheModelIsAskedOnlyWithSomethingToJudgeBy(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		return 200, answerAll(req, func(string) float64 { return 0.9 })
	}}
	o := openRouterFor(f.serve(t), 5*time.Second, "")
	rows := rowsOf(100, map[int]string{50: "error: x"})
	if c := o.Decide(context.Background(), Input{Command: "seq", Rows: rows}); c.Who != "keywords" || c.Failure != "" || f.count() != 0 {
		t.Fatalf("exit 0 without a purpose: %+v", c)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if c := o.Decide(cancelled, Input{Command: "make", Exit: 1, Rows: rows}); c.Failure != "cancelled" || f.count() != 0 {
		t.Fatalf("cancelled: %+v", c)
	}
	o.Decide(context.Background(), Input{Command: "make", TimedOut: true, Rows: rows})
	if f.count() != 1 || !strings.Contains(fmt.Sprint(f.requests[0]["questions"]), "did not finish before its timeout") || f.requests[0]["state"].(map[string]any)["timed_out"] != true {
		t.Fatalf("a timeout must be judged as one: %v", f.requests)
	}
}

func TestAnswersAreCachedWithoutTheOutputAndCheckedWhenRead(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		return 200, answerAll(req, func(string) float64 { return 0.8 })
	}}
	server := f.serve(t)
	dir := filepath.Join(t.TempDir(), "cache")
	in := Input{Command: "make", Purpose: "find it", Exit: 1, Rows: rowsOf(100, map[int]string{40: "secret-looking row 1234"})}
	first := openRouterFor(server, 5*time.Second, dir).Decide(context.Background(), in)
	second := openRouterFor(server, 5*time.Second, dir).Decide(context.Background(), in)
	if f.count() != 1 || second.Usage.Cached != 1 || second.Usage.Requests != 0 || fmt.Sprint(first.Picks) != fmt.Sprint(second.Picks) {
		t.Fatalf("%d requests; %+v then %+v", f.count(), first.Usage, second.Usage)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d entries", len(entries))
	}
	info, _ := os.Stat(dir)
	path := filepath.Join(dir, entries[0].Name())
	fileInfo, _ := os.Stat(path)
	if info.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes %v %v", info.Mode(), fileInfo.Mode())
	}
	data, _ := os.ReadFile(path)
	for _, text := range []string{"secret-looking", "transform", "make", "find it"} {
		if strings.Contains(string(data), text) {
			t.Fatalf("the entry holds %q: %s", text, data)
		}
	}
	// A tampered entry is not trusted; another endpoint is another key.
	os.WriteFile(path, []byte(strings.Replace(string(data), "0.8", "7", 1)), 0o600)
	openRouterFor(server, 5*time.Second, dir).Decide(context.Background(), in)
	if f.count() != 2 {
		t.Fatalf("a tampered entry was used")
	}
	other := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{DefaultModel}, URL: server.URL + "/api/alpha/decisions?x", Timeout: 5 * time.Second, Cache: dir})
	if cacheKey(other.config, []byte("b")) == cacheKey(openRouterFor(server, time.Second, dir).config, []byte("b")) {
		t.Fatal("the URL must be part of the key")
	}
}

func TestARecordedJevAnswerDecodes(t *testing.T) {
	recorded, err := os.ReadFile("testdata/jev-1.14-response.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{answer: func(int, map[string]any) (int, string) { return 200, string(recorded) }}
	o := openRouterFor(f.serve(t), 5*time.Second, "")
	choice := o.Decide(context.Background(), Input{Command: "bun scripts/build.ts", Exit: 1, Rows: rowsOf(16*windowRows, nil)})
	if choice.Failure != "" || len(choice.Picks) != 16 || choice.Picks[0].Score != 0.74 || choice.Picks[0].Start != 7*windowRows {
		t.Fatalf("choice %+v", choice)
	}
}
