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
	entries, err := parseSummary(" deepseek/deepseek-v4.1-flash-1017 , google/gemini-3.1-flash-lite@reasoning=low,claude-p:haiku")
	if err != nil {
		t.Fatal(err)
	}
	want := []SummaryEntry{
		{Spec: "deepseek/deepseek-v4.1-flash-1017", Model: "deepseek/deepseek-v4.1-flash-1017", Reasoning: "off"},
		{Spec: "google/gemini-3.1-flash-lite@reasoning=low", Model: "google/gemini-3.1-flash-lite", Reasoning: "low"},
		{Spec: "claude-p:haiku", Program: "claude-p", Model: "haiku", Reasoning: "off"},
	}
	if !slices.Equal(entries, want) {
		t.Fatalf("entries %+v", entries)
	}
	for _, bad := range []string{"haiku", "vendor/m@reasoning=lots", "vendor/m@speed=1", "vendor/m@input=whole", "claude-p:haiku@reasoning=low", "claude-p:", " , "} {
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

func TestASummaryFallsThroughTheListAndKeepsOnlyRangesInWhatItRead(t *testing.T) {
	log := logOf(300, map[int]string{120: "FAIL: TestParse expected 3, got 4", 121: "    at parse_test.go:40"})
	f := &chatFake{answer: func(n int, req map[string]any) (int, string) {
		switch req["model"] {
		case "a/down":
			return 500, ""
		case "b/liar":
			return 200, `{"summary": "All passed.", "ranges": [[5, 9]]}`
		}
		return 200, "```json\n" + `{"summary": "TestParse fails:  it expected 3.", "ranges": [[118, 125], [0, 4], [200, 290], ["121", "121"]]}` + "\n```"
	}}
	server := f.serve(t)
	s := summarizerFor(t, server, "a/down,b/liar,c/good@reasoning=low", "")
	in := SummaryInput{Command: "go test", Purpose: "find the failure", Exit: 1, Log: log, Excerpt: []int{1, 2, 3, 120, 121, 300}}
	got := s.Summarize(context.Background(), in)
	if got.Failure != "" || got.Who != "c/good@reasoning=low" || got.Text != "TestParse fails: it expected 3." {
		t.Fatalf("summary %+v", got)
	}
	if !slices.Equal(got.Ranges, [][2]int{{118, 125}, {121, 121}}) || got.Dropped != 2 || got.Usage.Requests != 3 {
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
		!strings.Contains(system, "300 lines") || !strings.Contains(system, "Only an excerpt") || !strings.HasPrefix(user, "1: ok step 1\n2: ") ||
		!strings.Contains(user, "\n120: FAIL: TestParse") {
		t.Fatalf("prompt %q\ninput %q", system, user[:60])
	}
	// Every entry failing says why each did.
	f.answer = func(int, map[string]any) (int, string) { return 200, "not json" }
	none := summarizerFor(t, server, "x/one,y/two", "").Summarize(context.Background(), in)
	if none.Failure != "x/one: its answer is not JSON; y/two: its answer is not JSON" || none.Text != "" {
		t.Fatalf("failure %+v", none)
	}
	// A summary that answers on its own points at no line; ranges that all miss what it read still fail.
	f.answer = func(int, map[string]any) (int, string) { return 200, `{"summary": "TestParse fails.", "ranges": []}` }
	alone := summarizerFor(t, server, "x/one", "").Summarize(context.Background(), in)
	if alone.Failure != "" || alone.Text != "TestParse fails." || len(alone.Ranges) != 0 || alone.Dropped != 0 {
		t.Fatalf("no ranges %+v", alone)
	}
	f.answer = func(int, map[string]any) (int, string) {
		return 200, `{"summary": "TestParse fails.", "ranges": [[5, 9]]}`
	}
	missed := summarizerFor(t, server, "x/one", "").Summarize(context.Background(), in)
	if missed.Failure != "x/one: none of its ranges lies in the lines it read" {
		t.Fatalf("missed %+v", missed)
	}
}

func TestASummarizerReadsOnlyTheExcerptAndAnswersAreCached(t *testing.T) {
	log := logOf(100, map[int]string{41: "error: the one that matters"})
	f := &chatFake{answer: func(int, map[string]any) (int, string) {
		return 200, `{"summary": "One error.", "ranges": [[41, 41], [90, 95]]}`
	}}
	server := f.serve(t)
	cache := t.TempDir()
	in := SummaryInput{Command: "build", Exit: 2, Log: log, Excerpt: []int{1, 2, 40, 41, 42}}
	got := summarizerFor(t, server, "v/m", cache).Summarize(context.Background(), in)
	// The range outside the excerpt is dropped, since the model never read those lines.
	if got.Failure != "" || !slices.Equal(got.Ranges, [][2]int{{41, 41}}) || got.Dropped != 1 {
		t.Fatalf("summary %+v", got)
	}
	messages := f.requests[0]["messages"].([]any)
	if user := messages[1].(map[string]any)["content"].(string); user != fmt.Sprintf("1: %s\n2: %s\n[lines left out]\n40: %s\n41: %s\n42: %s\n", log[0], log[1], log[39], log[40], log[41]) ||
		!strings.Contains(messages[0].(map[string]any)["content"].(string), "Only an excerpt is shown") {
		t.Fatalf("excerpt input %q", user)
	}
	// Another process with the same cache directory answers from it, checked again, with no request.
	again := summarizerFor(t, server, "v/m", cache).Summarize(context.Background(), in)
	if len(f.requests) != 1 || again.Usage.Cached != 1 || again.Text != "One error." {
		t.Fatalf("cached %+v after %d requests", again, len(f.requests))
	}
}

func TestAFileReadFromALaterLineIsNumberedAndAnsweredByTheFilesOwnLines(t *testing.T) {
	log := logOf(100, map[int]string{41: "error: the one that matters"})
	f := &chatFake{answer: func(int, map[string]any) (int, string) {
		return 200, `{"summary": "One error.", "ranges": [[541, 541], [41, 41]]}`
	}}
	server := f.serve(t)
	in := SummaryInput{Command: "cat run.log", Purpose: "why", Log: log, Excerpt: []int{40, 41, 42}, Base: 500}
	got := summarizerFor(t, server, "v/m", "").Summarize(context.Background(), in)
	// The answer's 541 is the log's line 41; its 41, before the log, is dropped.
	if got.Failure != "" || !slices.Equal(got.Ranges, [][2]int{{41, 41}}) || got.Dropped != 1 {
		t.Fatalf("summary %+v", got)
	}
	messages := f.requests[0]["messages"].([]any)
	system, user := messages[0].(map[string]any)["content"].(string), messages[1].(map[string]any)["content"].(string)
	if user != fmt.Sprintf("540: %s\n541: %s\n542: %s\n", log[39], log[40], log[41]) || !strings.Contains(system, "Output: lines 501-600 of a file, numbered below.") {
		t.Fatalf("prompt %q\ninput %q", system, user)
	}
}

func TestClaudePRunsOnTheLoginWithNothingOfTheProjectLoaded(t *testing.T) {
	record := fakeClaude(t, `{"summary": "It hangs.", "ranges": [[2, 2]]}`, 0)
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

func TestAnExpectationLeadsThePromptAndTheQuestion(t *testing.T) {
	in := SummaryInput{Command: "go test ./...", Purpose: "run the tests", Expect: "all pass", Log: []string{"ok"}}
	if p := summaryPrompt(in, false); !strings.Contains(p, "What the agent expected or wanted to find out: all pass\n") || !strings.Contains(p, "Lead with what contradicts the expectation") {
		t.Fatalf("prompt:\n%s", p)
	}
	in.Expect = ""
	if p := summaryPrompt(in, false); strings.Contains(p, "expect") {
		t.Fatalf("prompt without an expectation:\n%s", p)
	}
	question := Input{Purpose: "run the tests", Expect: "all pass"}
	if q := question.state(); q.Expect != "all pass" {
		t.Fatalf("state %+v", q)
	}
	if q := questionFor(question); !strings.Contains(q, "`expectation` did not hold") {
		t.Fatalf("question %q", q)
	}
	if q := questionFor(Input{Purpose: "run the tests"}); strings.Contains(q, "expectation") {
		t.Fatalf("question without an expectation %q", q)
	}
}

func questionFor(in Input) string { return question(in, "w0").Instructions }

// A log joining files asks for a summary of each file that bears on the purpose and keeps each file's ranges within its share of them, so
// files late in the log keep theirs however many an earlier one points at; one output's prompt and limit stay as they were, so the answers
// recorded for it still serve.
func TestALogJoiningFilesAsksAboutEachFileAndKeepsRangesInEach(t *testing.T) {
	log := make([]string, 200)
	shown := make([]int, 200)
	for i := range log {
		log[i], shown[i] = fmt.Sprint("line ", i+1), i+1
	}
	one := SummaryInput{Command: "cat a.md", Purpose: "what changed", Log: log, Excerpt: shown}
	p := summaryPrompt(one, false)
	if !strings.Contains(p, "Output: 200 lines, numbered below.\n") || !strings.Contains(p, "summary: at most 3 sentences. State only") ||
		!strings.Contains(p, "most important first, up to 5 ranges of at most 10 lines each: the line of each error") || strings.Contains(p, "each file") {
		t.Fatalf("one output's prompt:\n%s", p)
	}
	joined := one
	joined.Command, joined.Heads = "cat a.md b.md c.md d.md e.md f.md g.md", []int{1, 30, 60, 90, 120, 150, 180}
	p = summaryPrompt(joined, false)
	if !strings.Contains(p, "Output: 200 lines joining 7 files, each from its line \"[PATH] ...\", numbered below.\n") ||
		!strings.Contains(p, "summary: at most 3 sentences for each file that bears on the description, each naming its file;") ||
		!strings.Contains(p, "up to 30 ranges in all of at most 10 lines each, spread over the files the summary covers: the line of each error") {
		t.Fatalf("joined prompt:\n%s", p)
	}
	// The first file points at 28 lines, each other file at one: they keep theirs, and the first keeps the rest of the 30.
	var ranges []string
	for n := 2; n <= 29; n++ {
		ranges = append(ranges, fmt.Sprintf("[%d,%d]", n, n))
	}
	for _, head := range joined.Heads[1:] {
		ranges = append(ranges, fmt.Sprintf("[%d,%d]", head+1, head+1))
	}
	reply := `{"summary": "Each file renames something.", "ranges": [` + strings.Join(ranges, ",") + `]}`
	got, failure := check(reply, log, shown, 0, joined.Heads)
	if failure != "" || len(got.Ranges) != 30 || got.Dropped != 4 || got.Ranges[23] != [2]int{25, 25} || got.Ranges[24] != [2]int{31, 31} ||
		got.Ranges[29] != [2]int{181, 181} {
		t.Fatalf("joined: %+v %s", got, failure)
	}
	if got, failure := check(reply, log, shown, 0, nil); failure != "" || len(got.Ranges) != 5 || got.Dropped != 29 {
		t.Fatalf("one output: %+v %s", got, failure)
	}
}
