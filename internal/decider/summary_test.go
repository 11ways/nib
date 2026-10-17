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

// fakeClaude puts a claude program on PATH that records its arguments, working directory and input, and prints reply as its result.
func fakeClaude(t *testing.T, reply string, status int) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	result, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "result": reply, "usage": map[string]int{"input_tokens": 7}})
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s.args\npwd > %s.dir\ncat > %s.input\nprintf '%%s' '%s'\nexit %d\n", record, record, record,
		strings.ReplaceAll(string(result), "'", `'\''`), status)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func TestSummaryEntriesParseTheirOptionsAndRefuseTheRest(t *testing.T) {
	fakeClaude(t, "", 0)
	entries, err := parseSummary(" deepseek/deepseek-v4.1-flash-1017 , google/gemini-3.1-flash-lite@input=excerpt@reasoning=low,claude-p:haiku,claude-p:sonnet@input=whole")
	if err != nil {
		t.Fatal(err)
	}
	want := []SummaryEntry{
		{Spec: "deepseek/deepseek-v4.1-flash-1017", Model: "deepseek/deepseek-v4.1-flash-1017", Reasoning: "off", Input: "whole"},
		{Spec: "google/gemini-3.1-flash-lite@input=excerpt@reasoning=low", Model: "google/gemini-3.1-flash-lite", Reasoning: "low", Input: "excerpt"},
		{Spec: "claude-p:haiku", Claude: true, Model: "haiku", Reasoning: "off", Input: "excerpt"},
		{Spec: "claude-p:sonnet@input=whole", Claude: true, Model: "sonnet", Reasoning: "off", Input: "whole"},
	}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries %+v", entries)
	}
	for _, bad := range []string{"haiku", "vendor/m@reasoning=lots", "vendor/m@speed=1", "claude-p:haiku@reasoning=low", "claude-p:", " , "} {
		if _, err := parseSummary(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := parseSummary("claude-p:haiku"); err == nil || !strings.Contains(err.Error(), "claude program on PATH") {
		t.Errorf("claude missing: %v", err)
	}
}

func TestParseReadsTheSummaryAndModelLists(t *testing.T) {
	fakeClaude(t, "", 0)
	c, err := Parse(env(map[string]string{"NIB_DECIDER_SUMMARY": "claude-p:haiku", "NIB_DECIDER_SUMMARY_TIMEOUT": "9000"}))
	if err != nil || c.Kind != Keywords || len(c.Summary) != 1 || c.SummaryTimeout != 9*time.Second || c.Key != "" || c.Summarizer() == nil {
		t.Fatalf("claude-p needs no key: %+v %v", c, err)
	}
	c, err = Parse(env(map[string]string{"NIB_DECIDER": "openrouter", "NIB_DECIDER_KEY": "k", "NIB_DECIDER_MODEL": "a/b, c/d ,", "NIB_DECIDER_SUMMARY": "v/m",
		"NIB_DECIDER_SUMMARY_URL": "http://s"}))
	if err != nil || !slices.Equal(c.Models, []string{"a/b", "c/d"}) || c.Timeout != DefaultTimeout || c.SummaryURL != "http://s" || c.Key != "k" {
		t.Fatalf("lists: %+v %v", c, err)
	}
	if c, _ := Parse(env(nil)); c.Summarizer() != nil || c.Timeout != 30*time.Second {
		t.Fatalf("default: %+v", c)
	}
	for _, bad := range []map[string]string{
		{"NIB_DECIDER": "fixed", "NIB_DECIDER_SUMMARY": "claude-p:haiku"},
		{"NIB_DECIDER_SUMMARY": "v/m"},
		{"NIB_DECIDER_SUMMARY": "claude-p:haiku", "NIB_DECIDER_SUMMARY_TIMEOUT": "never"},
		{"NIB_DECIDER": "openrouter", "NIB_DECIDER_KEY": "k", "NIB_DECIDER_MODEL": " , "},
	} {
		if _, err := Parse(env(bad)); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

// chatFake is a chat completions endpoint: answer gets each decoded request and returns a status and the reply's content.
type chatFake struct {
	mu       sync.Mutex
	requests []map[string]any
	answer   func(n int, req map[string]any) (int, string)
}

func (f *chatFake) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(data, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		n := len(f.requests)
		f.mu.Unlock()
		status, content := f.answer(n, req)
		w.WriteHeader(status)
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
			"usage": map[string]any{"prompt_tokens": 50, "cost": 0.001}})
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func logOf(n int, special map[int]string) []string {
	log := make([]string, n)
	for i := range log {
		log[i] = fmt.Sprintf("ok step %d", i+1)
		if text, ok := special[i+1]; ok {
			log[i] = text
		}
	}
	return log
}

func summarizerFor(t *testing.T, server *httptest.Server, list, cache string) Summarizer {
	t.Helper()
	entries, err := parseSummary(list)
	if err != nil {
		t.Fatal(err)
	}
	return Config{Key: "key", Summary: entries, SummaryURL: server.URL, SummaryTimeout: 5 * time.Second, Cache: cache}.Summarizer()
}

func TestASummaryFallsThroughTheListAndKeepsOnlyWhatTheLogBearsOut(t *testing.T) {
	log := logOf(300, map[int]string{120: "FAIL: TestParse expected 3, got 4", 121: "    at parse_test.go:40"})
	f := &chatFake{answer: func(n int, req map[string]any) (int, string) {
		switch req["model"] {
		case "a/down":
			return 500, ""
		case "b/liar":
			return 200, `{"summary": "All passed.", "evidence": [{"line": 5, "text": "PASS everything"}], "ranges": [[1, 3]]}`
		}
		return 200, "```json\n" + `{"summary": "TestParse fails:  it expected 3.", "evidence": [{"line": 120, "text": "FAIL: TestParse expected 3"},` +
			`{"line": "121", "text": "at parse_test.go:40"}, {"line": 7, "text": "made up"}], "ranges": [[118, 125], [0, 4], [200, 290], ["121", "121"]]}` + "\n```"
	}}
	server := f.serve(t)
	s := summarizerFor(t, server, "a/down,b/liar,c/good@reasoning=low", "")
	in := SummaryInput{Command: "go test", Purpose: "find the failure", Exit: 1, Log: log, Excerpt: []int{1, 2, 3, 120, 121, 300}}
	got := s.Summarize(context.Background(), in)
	if got.Failure != "" || got.Who != "c/good@reasoning=low" || got.Text != "TestParse fails: it expected 3." || got.Excerpt {
		t.Fatalf("summary %+v", got)
	}
	if !slices.Equal(got.Ranges, [][2]int{{118, 125}, {121, 121}}) || len(got.Evidence) != 2 || got.Dropped != 3 || got.Usage.Requests != 3 {
		t.Fatalf("checked %+v", got)
	}
	first := f.requests[0]
	if first["reasoning"].(map[string]any)["enabled"] != false || first["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("request %v", first)
	}
	if f.requests[2]["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("reasoning %v", f.requests[2]["reasoning"])
	}
	messages := first["messages"].([]any)
	system, user := messages[0].(map[string]any)["content"].(string), messages[1].(map[string]any)["content"].(string)
	if !strings.Contains(system, "find the failure") || !strings.Contains(system, "Exit status (from the shell, always true): 1") ||
		!strings.Contains(system, "300 lines") || strings.Contains(system, "Only an excerpt") || !strings.HasPrefix(user, "1: ok step 1\n") ||
		!strings.Contains(user, "\n120: FAIL: TestParse") {
		t.Fatalf("prompt %q\ninput %q", system, user[:60])
	}
	// Every entry failing says why each did.
	f.answer = func(int, map[string]any) (int, string) { return 200, "not json" }
	none := summarizerFor(t, server, "x/one,y/two", "").Summarize(context.Background(), in)
	if none.Failure != "x/one: its answer is not JSON; y/two: its answer is not JSON" || none.Text != "" {
		t.Fatalf("failure %+v", none)
	}
}

func TestAnExcerptIsReadPastTheWholeLimitAndAnswersAreCached(t *testing.T) {
	long := strings.Repeat("x", summaryRowChars)
	log := logOf(WholeChars/summaryRowChars+10, nil)
	for i := range log {
		log[i] = long
	}
	log[40] = "error: the one that matters"
	f := &chatFake{answer: func(int, map[string]any) (int, string) {
		return 200, `{"summary": "One error.", "evidence": [{"line": 41, "text": "error: the one that matters"}], "ranges": [[41, 41], [90, 95]]}`
	}}
	server := f.serve(t)
	cache := t.TempDir()
	in := SummaryInput{Command: "build", Exit: 2, Log: log, Excerpt: []int{1, 2, 40, 41, 42}}
	got := summarizerFor(t, server, "v/m", cache).Summarize(context.Background(), in)
	// The range outside the excerpt is dropped, since the model never read those lines.
	if got.Failure != "" || !got.Excerpt || !slices.Equal(got.Ranges, [][2]int{{41, 41}}) || got.Dropped != 1 {
		t.Fatalf("summary %+v", got)
	}
	messages := f.requests[0]["messages"].([]any)
	if user := messages[1].(map[string]any)["content"].(string); user != fmt.Sprintf("1: %s\n2: %s\n[lines left out]\n40: %s\n41: %s\n42: %s\n", long, long, long, log[40], long) ||
		!strings.Contains(messages[0].(map[string]any)["content"].(string), "Only an excerpt is shown") {
		t.Fatalf("excerpt input %q", user)
	}
	// Another process with the same cache directory answers from it, checked again, with no request.
	again := summarizerFor(t, server, "v/m", cache).Summarize(context.Background(), in)
	if len(f.requests) != 1 || again.Usage.Cached != 1 || again.Text != "One error." {
		t.Fatalf("cached %+v after %d requests", again, len(f.requests))
	}
}

func TestClaudePRunsOnTheLoginWithNothingOfTheProjectLoaded(t *testing.T) {
	record := fakeClaude(t, `{"summary": "It hangs.", "evidence": [{"line": 2, "text": "waiting on lock"}], "ranges": [[2, 2]]}`, 0)
	entries, _ := parseSummary("claude-p:haiku")
	s := Config{Summary: entries, SummaryTimeout: 5 * time.Second}.Summarizer()
	in := SummaryInput{Command: "serve", TimedOut: true, Log: []string{"start", "waiting on lock", "tick"}, Excerpt: []int{1, 2, 3}}
	got := s.Summarize(context.Background(), in)
	if got.Failure != "" || got.Who != "claude-p:haiku" || got.Text != "It hangs." || got.Usage.Cost != 0 || got.Usage.InputTokens != 7 {
		t.Fatalf("summary %+v", got)
	}
	args, _ := os.ReadFile(record + ".args")
	for _, want := range []string{"-p\n--model\nhaiku\n--tools\n\n--output-format\njson\n--system-prompt\n", "--no-session-persistence\n--strict-mcp-config\n--setting-sources\n\n--disable-slash-commands\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	dir, _ := os.ReadFile(record + ".dir")
	input, _ := os.ReadFile(record + ".input")
	if !strings.Contains(string(dir), "nib-summary-") || string(input) != "1: start\n2: waiting on lock\n3: tick\n" {
		t.Fatalf("dir %q input %q", dir, input)
	}
	if _, err := os.Stat(strings.TrimSpace(string(dir))); err == nil {
		t.Error("the directory claude ran in is left behind")
	}
	fakeClaude(t, "", 3)
	fresh := Config{Summary: entries, SummaryTimeout: 5 * time.Second}.Summarizer()
	if got := fresh.Summarize(context.Background(), in); got.Failure != "claude-p:haiku: claude exited with status 3" {
		t.Fatalf("failure %+v", got)
	}
}

func TestASummarizerPastItsTimeoutIsStoppedAndNamed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	entries, _ := parseSummary("claude-p:haiku")
	started := time.Now()
	got := Config{Summary: entries, SummaryTimeout: 200 * time.Millisecond}.Summarizer().Summarize(context.Background(),
		SummaryInput{Log: []string{"a"}, Excerpt: []int{1}})
	if got.Failure != "claude-p:haiku: timeout after 200ms" || time.Since(started) > 5*time.Second {
		t.Fatalf("after %s: %+v", time.Since(started), got)
	}
}

func TestTheNextDecisionModelAnswersWhenOneFails(t *testing.T) {
	f := &fake{answer: func(_ int, req map[string]any) (int, string) {
		if req["model"] == "bad/model" {
			return 500, `{}`
		}
		return 200, answerAll(req, func(id string) float64 {
			if id == "w1" {
				return 0.9
			}
			return 0.1
		})
	}}
	server := f.serve(t)
	o := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{"bad/model", DefaultModel}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second})
	choice := o.Decide(context.Background(), Input{Command: "c", Exit: 1, Rows: rowsOf(60, nil)})
	if choice.Failure != "" || choice.Who != DefaultModel+" and keywords (after bad/model failed: OpenRouter 500)" || choice.Picks[0].Start != 25 {
		t.Fatalf("choice %+v", choice)
	}
	only := newOpenRouter(Config{Kind: OpenRouter, Key: "key", Models: []string{"bad/model"}, URL: server.URL + "/api/alpha/decisions", Timeout: 5 * time.Second})
	if got := only.Decide(context.Background(), Input{Command: "c", Exit: 1, Rows: rowsOf(60, nil)}); got.Failure != "OpenRouter 500" || got.Who != "keywords" {
		t.Fatalf("one model %+v", got)
	}
}
