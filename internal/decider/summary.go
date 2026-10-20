package decider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSummaryURL     = "https://openrouter.ai/api/v1/chat/completions"
	DefaultSummaryTimeout = 30 * time.Second
	// openCodeAgent is the one agent an OpenCode run is given: the summary prompt, every permission denied, so it has no tools.
	openCodeAgent = "nib-summary"
	// SummaryRowChars cuts each numbered row a summarizer reads.
	SummaryRowChars = 300
	// maxRanges and maxRangeLines bound what an answer may point at; longer ranges are dropped, not shortened. An answer about a log joining
	// files may point at maxRanges for each file, at most maxJoinedRanges in all, each file keeping its share (Shares) of them.
	maxRanges       = 5
	maxJoinedRanges = 30
	maxRangeLines   = 60
	// summaryVersion changes with the prompt, so cached answers to another prompt are not reused.
	summaryVersion = 4
)

// The values an entry's reasoning option takes; "off" is the default, "default" sends none and leaves it to the model.
var reasonings = []string{"off", "default", "minimal", "low", "medium", "high"}

// program is a summarizer run as a program on the user's own login, named by an entry's prefix NAME:; an entry without one is an OpenRouter
// chat model.
type program struct {
	name string
	// form is how an error names such an entry.
	form string
	// slash says the model is PROVIDER/MODEL.
	slash bool
	// onPath is the program parsing needs on PATH; "" when it is looked for only when it runs.
	onPath string
	run    func(s *summaries, ctx context.Context, e SummaryEntry, prompt, text string, usage *Usage) (string, string)
}

// programs are every program an entry may name; the only list of them.
var programs = []program{
	{name: "claude-p", form: "claude-p:MODEL", onPath: "claude", run: (*summaries).claude},
	{name: "opencode", form: "opencode:PROVIDER/MODEL", slash: true, run: (*summaries).opencode},
}

// SummaryEntry is one summarizer of NIB_DECIDER_SUMMARY: an OpenRouter chat model, or a program's, with its options.
type SummaryEntry struct {
	// Spec is the entry as written, which notes name it by.
	Spec string
	// Program is the name of the program that runs it, "" for an OpenRouter model.
	Program   string
	Model     string
	Reasoning string
}

// program is the program that runs e, nil for an OpenRouter model.
func (e SummaryEntry) program() *program {
	for i := range programs {
		if programs[i].name == e.Program {
			return &programs[i]
		}
	}
	return nil
}

// parseSummary reads a comma separated list of entries, each MODEL or a program's NAME:MODEL, an OpenRouter one optionally followed by
// @reasoning=VALUE.
func parseSummary(list string) ([]SummaryEntry, error) {
	forms := []string{"an OpenRouter model id (vendor/model)"}
	for _, p := range programs {
		forms = append(forms, p.form)
	}
	var entries []SummaryEntry
	for _, spec := range strings.Split(list, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.Split(spec, "@")
		e := SummaryEntry{Spec: spec, Model: parts[0], Reasoning: "off"}
		for _, p := range programs {
			if after, ok := strings.CutPrefix(e.Model, p.name+":"); ok {
				e.Program, e.Model = p.name, after
			}
		}
		p := e.program()
		if e.Model == "" || strings.ContainsAny(e.Model, " \t") || (p == nil || p.slash) && !strings.Contains(e.Model, "/") {
			return nil, fmt.Errorf("NIB_DECIDER_SUMMARY entry %q is none of %s", spec, strings.Join(forms, ", "))
		}
		for _, option := range parts[1:] {
			name, value, _ := strings.Cut(option, "=")
			if name != "reasoning" || !slices.Contains(reasonings, value) || p != nil {
				return nil, fmt.Errorf("NIB_DECIDER_SUMMARY entry %q has option %q; the one option is @reasoning=%s, for OpenRouter models", spec, option,
					strings.Join(reasonings, "|"))
			}
			e.Reasoning = value
		}
		if p != nil && p.onPath != "" {
			if _, err := exec.LookPath(p.onPath); err != nil {
				return nil, fmt.Errorf("NIB_DECIDER_SUMMARY entry %q needs the %s program on PATH", spec, p.onPath)
			}
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("NIB_DECIDER_SUMMARY is %q; give one or more entries, separated by commas", list)
	}
	return entries, nil
}

// SummaryInput is one cut output to summarize: nib's facts about the call, the log's lines without colors, and the log lines of the excerpt,
// which is all a summarizer reads.
type SummaryInput struct {
	Command  string
	Purpose  string
	Expect   string
	Exit     int
	TimedOut bool
	Log      []string
	// Excerpt are the 1-based log lines of the excerpt, ascending: the kept start and end and the passages the decider picked.
	Excerpt []int
	// Entries, when set, are what the summarizer reads instead of the excerpt's lines: the same lines deduplicated, each repeated line or
	// block named once in Legend.
	Entries []Entry
	Legend  Legend
	// Base is how many lines of a file come before the log, a file read from its line Base+1 on: the model reads, and answers in, the
	// file's own numbers, while Excerpt, Entries and the ranges of the Summary count from the log's start.
	Base int
	// Heads are the log lines a skim's joined files start at, each the file's head line "[PATH] ..."; none for one output or file.
	Heads []int
}

// Summary is a summarizer's checked answer: its prose, the ranges that lie in the lines it read, and how many ranges did not.
type Summary struct {
	Text string
	// Ranges are 1-based inclusive log lines, most important first.
	Ranges [][2]int
	// Dropped counts ranges outside the log or the lines the entry read.
	Dropped int
	// Who is the entry that answered.
	Who string
	// Failure is why no entry gave a usable answer; the other fields are empty then.
	Failure string
	Usage   Usage
}

// Summarizer condenses a cut output into a checked summary.
type Summarizer interface {
	Summarize(ctx context.Context, in SummaryInput) Summary
}

// Summarizer is the summarizer c describes; nil without entries.
func (c Config) Summarizer() Summarizer {
	if len(c.Summary) == 0 {
		return nil
	}
	client := c.Client
	if client == nil {
		client = &http.Client{}
	}
	return &summaries{config: c, client: client, cache: newCache(c.Cache)}
}

type summaries struct {
	observers
	config Config
	client *http.Client
	cache  *cache
	// opencodes are the OpenCode programs found to be OpenCode 2, so each is asked its version once.
	opencodes sync.Map
}

// answer is a summarizer's reply as JSON, read leniently since models write numbers as strings at times.
type answer struct {
	Summary string  `json:"summary"`
	Ranges  [][]any `json:"ranges"`
}

func (s *summaries) Summarize(ctx context.Context, in SummaryInput) Summary {
	started := time.Now()
	deadline, cancel := context.WithTimeout(ctx, s.config.SummaryTimeout)
	defer cancel()
	var usage Usage
	var failures []string
	for _, e := range s.config.Summary {
		got, failure := s.with(deadline, e, in)
		usage.add(got.Usage)
		if failure == "" {
			got.Usage = usage
			got.Usage.Elapsed = time.Since(started)
			return got
		}
		failures = append(failures, e.Spec+": "+failure)
		if deadline.Err() != nil {
			break
		}
	}
	usage.Elapsed = time.Since(started)
	return Summary{Failure: strings.Join(failures, "; "), Usage: usage}
}

// with asks one entry, from the cache when it answered this before, and checks the answer against the log.
func (s *summaries) with(ctx context.Context, e SummaryEntry, in SummaryInput) (Summary, string) {
	text := render(in)
	prompt := summaryPrompt(in, len(in.Excerpt) < len(in.Log))
	key := summaryKey(e, s.config.SummaryURL, prompt, text)
	var reply string
	var usage Usage
	if cached, ok := s.cache.getText(key); ok {
		reply, usage.Cached = cached, 1
	} else {
		var failure string
		end := s.begin("summary", e.Spec)
		if p := e.program(); p != nil {
			reply, failure = p.run(s, ctx, e, prompt, text, &usage)
		} else {
			reply, failure = s.chat(ctx, e, prompt, text, &usage)
		}
		end()
		if failure != "" {
			return Summary{Usage: usage}, failure
		}
	}
	got, failure := check(reply, in.Log, in.Excerpt, in.Base, in.Heads)
	got.Usage = usage
	if failure != "" {
		return got, failure
	}
	if usage.Cached == 0 {
		s.cache.putText(key, reply)
	}
	got.Who = e.Spec
	return got, ""
}

func render(in SummaryInput) string {
	var b strings.Builder
	cut := func(text string) string {
		row := []rune(text)
		if len(row) > SummaryRowChars {
			row = row[:SummaryRowChars]
		}
		return string(row)
	}
	if in.Entries == nil {
		for i, n := range in.Excerpt {
			if i > 0 && n != in.Excerpt[i-1]+1 {
				b.WriteString("[lines left out]\n")
			}
			fmt.Fprintf(&b, "%d: %s\n", in.Base+n, cut(in.Log[n-1]))
		}
		return b.String()
	}
	var texts []string
	for _, e := range in.Entries {
		texts = append(texts, e.Text)
	}
	if names := in.Legend.Names(texts...); len(names) > 0 {
		b.WriteString("Legend:\n")
		for _, name := range names {
			lines := strings.Split(in.Legend[name], "\n")
			if name[1] == 'L' {
				fmt.Fprintf(&b, "%s = %s\n", name, cut(lines[0]))
				continue
			}
			fmt.Fprintf(&b, "%s =\n", name)
			for _, line := range lines {
				fmt.Fprintf(&b, "  %s\n", cut(line))
			}
		}
		b.WriteString("\nOutput:\n")
	}
	for i, e := range in.Entries {
		if i > 0 && e.First != in.Entries[i-1].Last+1 {
			b.WriteString("[lines left out]\n")
		}
		fmt.Fprintf(&b, "%s: %s\n", Entry{First: in.Base + e.First, Last: in.Base + e.Last}.Label(), cut(e.Text))
	}
	return b.String()
}

func summaryPrompt(in SummaryInput, excerpt bool) string {
	purpose := in.Purpose
	if purpose == "" {
		purpose = "(none given)"
	}
	status := strconv.Itoa(in.Exit)
	if in.TimedOut {
		status = "none: the command was still running at its timeout"
	}
	text := "You condense the output of a shell command for a coding agent that ran it. The agent reads only your answer and the few lines you " +
		"point at, so a wrong statement costs it a wrong fix.\n" +
		"The agent's description of why it ran the command: " + purpose + "\n"
	if in.Expect != "" {
		text += "What the agent expected or wanted to find out: " + in.Expect + "\n"
	}
	text += "Command: " + in.Command + "\n" +
		"Exit status (from the shell, always true): " + status + "\n" +
		output(in) +
		`Answer with JSON only: {"summary": "...", "ranges": [[first, last], ...]}` + "\n"
	if len(in.Heads) > 1 {
		text += "summary: at most 3 sentences for each file that bears on the description, each naming its file; leave out the files that do " +
			"not. State only what the numbered lines show; never guess causes the output does not show. Do not count lines or entries; the " +
			"agent gets exact counts from the shell.\n" +
			"ranges: only the lines the agent must read as printed, never prose the summary already tells, most important first, up to " +
			strconv.Itoa(joinedRanges(len(in.Heads))) + " ranges in all of at most 10 lines each, spread over the files the summary covers: the " +
			"line of each error or failure the summary names, and the lines whose exact text the agent must copy, such as a path, a value, a new " +
			"signature or the code before and after a change. When the summary answers the description on its own, ranges is []."
	} else {
		text += "summary: at most 3 sentences. State only what the numbered lines show; never guess causes the output does not show. Do not count " +
			"lines or entries; the agent gets exact counts from the shell.\n" +
			"ranges: only the lines the agent must read as printed, most important first, up to 5 ranges of at most 10 lines each: the line of " +
			"each error, failure or cause the summary names, and a line whose exact text the agent needs, such as a path, a stack frame or a " +
			"value the summary cites. Point at no line only for context; when the summary answers the description on its own, ranges is []."
	}
	if in.Expect != "" {
		text += "\nLead with what contradicts the expectation; if the output meets it, one short sentence saying so is the whole summary."
	}
	if in.Entries != nil && len(in.Legend) > 0 {
		text += "\nWhat repeats is defined once under Legend and named where it occurs: 1203-2372: @L1 x1170 is the line @L1, 1170 times in a " +
			"row; 5001-5014: @B1 is the block @B1. Ranges are log line numbers as shown."
	}
	if excerpt {
		text += "\nOnly an excerpt is shown: the start, the end, and the passages a relevance model judged most likely to matter, each with its " +
			"real line number. Lines not shown exist; do not guess what they hold."
	}
	return text
}

// summaryKey covers everything an answer depends on: the entry with its options, the endpoint, the prompt and the numbered output.
func summaryKey(e SummaryEntry, url, prompt, text string) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "summary\x00%d\x00%s\x00%s\x00%s\x00", summaryVersion, e.Spec, url, prompt)
	sum.Write([]byte(text))
	return "summary-" + hex.EncodeToString(sum.Sum(nil))
}

// output is the prompt's line about the lines it numbers: their count, or for a file read from a later line on, their numbers.
func output(in SummaryInput) string {
	switch {
	case in.Base > 0:
		return fmt.Sprintf("Output: lines %d-%d of a file, numbered below.\n\n", in.Base+1, in.Base+len(in.Log))
	case len(in.Heads) > 1:
		return fmt.Sprintf("Output: %d lines joining %d files, each from its line \"[PATH] ...\", numbered below.\n\n", len(in.Log), len(in.Heads))
	}
	return fmt.Sprintf("Output: %d lines, numbered below.\n\n", len(in.Log))
}

// joinedRanges is how many ranges an answer about a log joining files may point at.
func joinedRanges(files int) int { return min(maxRanges*files, maxJoinedRanges) }

var fence = regexp.MustCompile("^```(?:json)?\\s*|\\s*```$")

// check reads a reply, whose ranges count from base+1, and keeps the ranges inside the log that touch a line the entry read, counted from
// the log's start: the first maxRanges of them, or of a log joining files at heads the first of each file's within its share of
// joinedRanges, a range being in the file its first line is in. An answer with no summary fails, and so does one whose ranges were all
// unusable, but an empty list is an answer.
func check(reply string, log []string, shown []int, base int, heads []int) (Summary, string) {
	var a answer
	if json.Unmarshal([]byte(fence.ReplaceAllString(strings.TrimSpace(reply), "")), &a) != nil {
		return Summary{}, "its answer is not JSON"
	}
	got := Summary{Text: strings.Join(strings.Fields(a.Summary), " ")}
	if got.Text == "" {
		return got, "its answer has no summary"
	}
	joined := len(heads) > 1
	for i, r := range a.Ranges {
		first, ok1 := 0, false
		last, ok2 := 0, false
		if len(r) == 2 {
			first, ok1 = integer(r[0])
			last, ok2 = integer(r[1])
			first, last = first-base, last-base
		}
		touches := ok1 && ok2 && slices.ContainsFunc(shown, func(n int) bool { return n >= first && n <= last })
		if !joined && i >= maxRanges || !touches || first < 1 || last < first || last > len(log) || last-first+1 > maxRangeLines {
			got.Dropped++
			continue
		}
		got.Ranges = append(got.Ranges, [2]int{first, last})
	}
	if joined {
		file := func(r [2]int) int {
			at, _ := slices.BinarySearch(heads, r[0]+1)
			return at
		}
		counts := make([]int, len(heads)+1)
		for _, r := range got.Ranges {
			counts[file(r)]++
		}
		shares := Shares(counts, joinedRanges(len(heads)), 0)
		kept := got.Ranges[:0]
		for _, r := range got.Ranges {
			if f := file(r); shares[f] > 0 {
				shares[f]--
				kept = append(kept, r)
				continue
			}
			got.Dropped++
		}
		got.Ranges = kept
	}
	if len(got.Ranges) == 0 && got.Dropped > 0 {
		return got, "none of its ranges lies in the lines it read"
	}
	return got, ""
}

// Shares splits budget among parts that want the given sizes: one that fits an even split of what is left gets all it wants, and the rest
// share what remains, each at least least.
func Shares(sizes []int, budget, least int) []int {
	order := make([]int, len(sizes))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return sizes[a] - sizes[b] })
	shares := make([]int, len(sizes))
	left := max(0, budget)
	for k, i := range order {
		even := max(least, left/(len(order)-k))
		shares[i] = min(sizes[i], even)
		left = max(0, left-shares[i])
	}
	return shares
}

func integer(v any) (int, bool) {
	switch v := v.(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

// chatRequest is OpenRouter's chat completions request; reasoning is left out when the entry leaves it to the model.
type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	ResponseFormat map[string]string `json:"response_format"`
	Temperature    float64           `json:"temperature"`
	Usage          map[string]bool   `json:"usage"`
	Provider       provider          `json:"provider"`
	Reasoning      map[string]any    `json:"reasoning,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is read leniently; OpenRouter reports a provider's failure as a 200 with an error and no choices.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage struct {
		PromptTokens int     `json:"prompt_tokens"`
		Cost         float64 `json:"cost"`
		BYOK         bool    `json:"is_byok"`
		CostDetails  struct {
			Upstream float64 `json:"upstream_inference_cost"`
		} `json:"cost_details"`
	} `json:"usage"`
}

// chat asks an OpenRouter chat model once.
func (s *summaries) chat(ctx context.Context, e SummaryEntry, prompt, text string, usage *Usage) (string, string) {
	req := chatRequest{Model: e.Model, Messages: []chatMessage{{"system", prompt}, {"user", text}}, ResponseFormat: map[string]string{"type": "json_object"},
		Usage: map[string]bool{"include": true}, Provider: provider{DataCollection: "deny"}}
	switch e.Reasoning {
	case "off":
		req.Reasoning = map[string]any{"enabled": false}
	case "default":
	default:
		req.Reasoning = map[string]any{"effort": e.Reasoning}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", "invalid request"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.SummaryURL, bytes.NewReader(body))
	if err != nil {
		return "", "invalid request"
	}
	request.Header.Set("Authorization", "Bearer "+s.config.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Title", "nib")
	usage.Requests++
	resp, err := s.client.Do(request)
	if err != nil {
		return "", s.ended(ctx, "network error")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", s.ended(ctx, "network error")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Sprintf("OpenRouter %d", resp.StatusCode)
	}
	var decoded chatResponse
	if json.Unmarshal(data, &decoded) != nil {
		return "", "invalid answer"
	}
	usage.InputTokens += decoded.Usage.PromptTokens
	usage.Cost += decoded.Usage.Cost
	if decoded.Usage.BYOK {
		usage.Cost += decoded.Usage.CostDetails.Upstream
	}
	if len(decoded.Choices) == 0 {
		if decoded.Error != nil {
			return "", "OpenRouter: " + decoded.Error.Message
		}
		return "", "invalid answer"
	}
	return decoded.Choices[0].Message.Content, ""
}

// claudeResult is what `claude -p --output-format json` prints.
type claudeResult struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens   int `json:"input_tokens"`
		CacheCreation int `json:"cache_creation_input_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// claude runs `claude -p` on the user's login, in an empty directory, with no tools, MCP servers, settings or session kept, so it loads
// neither nib nor hooks; its cost is the subscription's, so none is counted.
func (s *summaries) claude(ctx context.Context, e SummaryEntry, prompt, text string, usage *Usage) (string, string) {
	dir, err := os.MkdirTemp("", "nib-summary-")
	if err != nil {
		return "", "no directory to run claude in"
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", e.Model, "--tools", "", "--output-format", "json", "--system-prompt", prompt,
		"--no-session-persistence", "--strict-mcp-config", "--setting-sources", "", "--disable-slash-commands")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(text)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	usage.Requests++
	if err := runGroup(cmd); err != nil {
		if ctx.Err() != nil {
			return "", s.ended(ctx, "")
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Sprintf("claude exited with status %d", exit.ExitCode())
		}
		return "", "claude did not run"
	}
	var result claudeResult
	if json.Unmarshal(stdout.Bytes(), &result) != nil {
		return "", "claude printed no result"
	}
	usage.InputTokens += result.Usage.InputTokens + result.Usage.CacheCreation + result.Usage.CacheRead
	if result.IsError {
		return "", "claude reported an error"
	}
	return result.Result, ""
}

// ended is the failure for a request whose context ended, or otherwise when it did not.
func (s *summaries) ended(ctx context.Context, otherwise string) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout after " + s.config.SummaryTimeout.String()
	case ctx.Err() != nil:
		return "cancelled"
	}
	return otherwise
}
