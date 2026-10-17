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
	"syscall"
	"time"
)

const (
	DefaultSummaryURL     = "https://openrouter.ai/api/v1/chat/completions"
	DefaultSummaryTimeout = 30 * time.Second
	// claudePrefix marks an entry run by Claude Code's own `claude -p`, on the user's login.
	claudePrefix = "claude-p:"
	// summaryRowChars cuts each numbered row a summarizer reads.
	summaryRowChars = 300
	// maxRanges and maxRangeLines bound what an answer may point at; longer ranges are dropped, not shortened.
	maxRanges     = 5
	maxRangeLines = 60
	// summaryVersion changes with the prompt, so cached answers to another prompt are not reused.
	summaryVersion = 2
)

// The values an entry's reasoning option takes; "off" is the default, "default" sends none and leaves it to the model.
var reasonings = []string{"off", "default", "minimal", "low", "medium", "high"}

// SummaryEntry is one summarizer of NIB_DECIDER_SUMMARY: an OpenRouter chat model, or `claude-p:MODEL`, with its options.
type SummaryEntry struct {
	// Spec is the entry as written, which notes name it by.
	Spec      string
	Claude    bool
	Model     string
	Reasoning string
}

// parseSummary reads a comma separated list of entries, each MODEL or claude-p:MODEL, an OpenRouter one optionally followed by @reasoning=VALUE.
func parseSummary(list string) ([]SummaryEntry, error) {
	var entries []SummaryEntry
	for _, spec := range strings.Split(list, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.Split(spec, "@")
		e := SummaryEntry{Spec: spec, Model: parts[0], Reasoning: "off"}
		if after, ok := strings.CutPrefix(e.Model, claudePrefix); ok {
			e.Claude, e.Model = true, after
		}
		if e.Model == "" || strings.ContainsAny(e.Model, " \t") || !e.Claude && !strings.Contains(e.Model, "/") {
			return nil, fmt.Errorf("NIB_DECIDER_SUMMARY entry %q is neither an OpenRouter model id (vendor/model) nor claude-p:MODEL", spec)
		}
		for _, option := range parts[1:] {
			name, value, _ := strings.Cut(option, "=")
			if name != "reasoning" || !slices.Contains(reasonings, value) || e.Claude {
				return nil, fmt.Errorf("NIB_DECIDER_SUMMARY entry %q has option %q; the one option is @reasoning=%s, for OpenRouter models", spec, option,
					strings.Join(reasonings, "|"))
			}
			e.Reasoning = value
		}
		if e.Claude {
			if _, err := exec.LookPath("claude"); err != nil {
				return nil, fmt.Errorf("NIB_DECIDER_SUMMARY entry %q needs the claude program on PATH", spec)
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
	Exit     int
	TimedOut bool
	Log      []string
	// Excerpt are the 1-based log lines of the excerpt, ascending: the kept start and end and the passages the decider picked.
	Excerpt []int
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
	config Config
	client *http.Client
	cache  *cache
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
	text := render(in.Log, in.Excerpt)
	prompt := summaryPrompt(in, len(in.Excerpt) < len(in.Log))
	key := summaryKey(e, s.config.SummaryURL, prompt, text)
	var reply string
	var usage Usage
	if cached, ok := s.cache.getText(key); ok {
		reply, usage.Cached = cached, 1
	} else {
		var failure string
		if e.Claude {
			reply, failure = s.claude(ctx, e, prompt, text, &usage)
		} else {
			reply, failure = s.chat(ctx, e, prompt, text, &usage)
		}
		if failure != "" {
			return Summary{Usage: usage}, failure
		}
	}
	got, failure := check(reply, in.Log, in.Excerpt)
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

func render(log []string, lines []int) string {
	var b strings.Builder
	for i, n := range lines {
		if i > 0 && n != lines[i-1]+1 {
			b.WriteString("[lines left out]\n")
		}
		row := []rune(log[n-1])
		if len(row) > summaryRowChars {
			row = row[:summaryRowChars]
		}
		fmt.Fprintf(&b, "%d: %s\n", n, string(row))
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
	text := "You condense the output of a shell command for a coding agent that ran it. The agent reads only your answer and the lines you point " +
		"at, so a wrong statement costs it a wrong fix.\n" +
		"The agent's description of why it ran the command: " + purpose + "\n" +
		"Command: " + in.Command + "\n" +
		"Exit status (from the shell, always true): " + status + "\n" +
		fmt.Sprintf("Output: %d lines, numbered below.\n\n", len(in.Log)) +
		`Answer with JSON only: {"summary": "...", "ranges": [[first, last], ...]}` + "\n" +
		"summary: at most 3 sentences. State only what the numbered lines show; never guess causes the output does not show. Do not count " +
		"lines or entries; the agent gets exact counts from the shell.\n" +
		"ranges: up to 5 line ranges that matter most for the description, most important first, each at most 30 lines."
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

var fence = regexp.MustCompile("^```(?:json)?\\s*|\\s*```$")

// check reads a reply and keeps the ranges inside the log that touch a line the entry read; an answer with no summary, or no range left,
// fails.
func check(reply string, log []string, shown []int) (Summary, string) {
	var a answer
	if json.Unmarshal([]byte(fence.ReplaceAllString(strings.TrimSpace(reply), "")), &a) != nil {
		return Summary{}, "its answer is not JSON"
	}
	got := Summary{Text: strings.Join(strings.Fields(a.Summary), " ")}
	if got.Text == "" {
		return got, "its answer has no summary"
	}
	for i, r := range a.Ranges {
		first, ok1 := 0, false
		last, ok2 := 0, false
		if len(r) == 2 {
			first, ok1 = integer(r[0])
			last, ok2 = integer(r[1])
		}
		touches := ok1 && ok2 && slices.ContainsFunc(shown, func(n int) bool { return n >= first && n <= last })
		if i >= maxRanges || !touches || first < 1 || last < first || last > len(log) || last-first+1 > maxRangeLines {
			got.Dropped++
			continue
		}
		got.Ranges = append(got.Ranges, [2]int{first, last})
	}
	if len(got.Ranges) == 0 {
		return got, "none of its ranges lies in the lines it read"
	}
	return got, ""
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A program of its own that still holds claude's output open cannot keep the call waiting past the kill.
	cmd.WaitDelay = time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	usage.Requests++
	if err := cmd.Run(); err != nil {
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
