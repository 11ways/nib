package server

import (
	"cmp"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bigResults is how many of the largest results a usage log keeps.
const bigResults = 8

// callSummary bounds the arguments kept to say which call made a large result.
const callSummary = 100

// usageLog counts what a client's tool calls returned and which are running, for the mod's token report and its band; the zero value is
// ready. nibash_status is left out: the mod asks it every second.
type usageLog struct {
	mu      sync.Mutex
	next    int
	flights map[int]Flight
	agents  []string
	tools   map[string]*toolUse
	largest []BigResult
}

// toolUse is what one agent's calls of one tool returned.
type toolUse struct {
	agent, tool  string
	calls, chars int
}

// Flight is a tool call still running; times are Unix milliseconds.
type Flight struct {
	Agent   string `json:"agent"`
	Tool    string `json:"tool"`
	Started int64  `json:"started"`
	// Hosted says the tool is a hosted provider's, whose program may take long to start or to answer.
	Hosted bool `json:"hosted,omitempty"`
}

// TokensView is what a client's tools returned in this session, by agent and tool, in characters of text, the unit of nib's output rules.
type TokensView struct {
	Chars   int         `json:"chars"`
	Calls   int         `json:"calls"`
	Agents  []AgentUse  `json:"agents"`
	Largest []BigResult `json:"largest"`
}

// AgentUse is one agent's calls, its heaviest tools first.
type AgentUse struct {
	Agent string    `json:"agent"`
	Calls int       `json:"calls"`
	Chars int       `json:"chars"`
	Tools []ToolRow `json:"tools"`
}

// ToolRow is what the calls of one tool returned.
type ToolRow struct {
	Tool  string `json:"tool"`
	Calls int    `json:"calls"`
	Chars int    `json:"chars"`
}

// BigResult is one of the largest results: who asked for it, with what, and how long it was.
type BigResult struct {
	Agent string `json:"agent"`
	Tool  string `json:"tool"`
	Chars int    `json:"chars"`
	// Call is the start of the call's arguments.
	Call string `json:"call"`
}

// label names an agent for display: "main" for the client's own.
func label(agent string) string {
	if agent == "" {
		return "main"
	}
	return agent
}

// begin notes a call running and returns the function that ends it, given what it returned (nil for nothing countable). Calls that only
// ask nib about itself are not noted.
func (u *usageLog) begin(agent, tool string, hosted bool, arguments []byte) func(result *mcp.CallToolResult) {
	if tool == "nibash_status" {
		return func(*mcp.CallToolResult) {}
	}
	agent = label(agent)
	u.mu.Lock()
	if u.flights == nil {
		u.flights, u.tools = map[int]Flight{}, map[string]*toolUse{}
	}
	if !slices.Contains(u.agents, agent) {
		u.agents = append(u.agents, agent)
	}
	u.next++
	id := u.next
	u.flights[id] = Flight{Agent: agent, Tool: tool, Started: time.Now().UnixMilli(), Hosted: hosted}
	u.mu.Unlock()
	return func(result *mcp.CallToolResult) {
		u.mu.Lock()
		defer u.mu.Unlock()
		delete(u.flights, id)
		if result == nil {
			return
		}
		chars := 0
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				chars += utf8.RuneCountInString(text.Text)
			}
		}
		key := agent + "\x00" + tool
		use := u.tools[key]
		if use == nil {
			use = &toolUse{agent: agent, tool: tool}
			u.tools[key] = use
		}
		use.calls++
		use.chars += chars
		call := string(arguments)
		if r := []rune(call); len(r) > callSummary {
			call = string(r[:callSummary-3]) + "..."
		}
		u.largest = append(u.largest, BigResult{Agent: agent, Tool: tool, Chars: chars, Call: call})
		slices.SortStableFunc(u.largest, func(a, b BigResult) int { return cmp.Compare(b.Chars, a.Chars) })
		if len(u.largest) > bigResults {
			u.largest = u.largest[:bigResults]
		}
	}
}

// flying are the calls running now, oldest first.
func (u *usageLog) flying() []Flight {
	u.mu.Lock()
	defer u.mu.Unlock()
	flights := make([]Flight, 0, len(u.flights))
	for _, f := range u.flights {
		flights = append(flights, f)
	}
	slices.SortStableFunc(flights, func(a, b Flight) int { return cmp.Compare(a.Started, b.Started) })
	return flights
}

// tokens is what the calls so far returned.
func (u *usageLog) tokens() *TokensView {
	u.mu.Lock()
	defer u.mu.Unlock()
	view := &TokensView{Agents: []AgentUse{}, Largest: slices.Clone(u.largest)}
	if view.Largest == nil {
		view.Largest = []BigResult{}
	}
	for _, agent := range u.agents {
		use := AgentUse{Agent: agent, Tools: []ToolRow{}}
		for _, t := range u.tools {
			if t.agent == agent {
				use.Tools = append(use.Tools, ToolRow{Tool: t.tool, Calls: t.calls, Chars: t.chars})
				use.Calls += t.calls
				use.Chars += t.chars
			}
		}
		slices.SortFunc(use.Tools, func(a, b ToolRow) int {
			return cmp.Or(cmp.Compare(b.Chars, a.Chars), cmp.Compare(a.Tool, b.Tool))
		})
		view.Calls += use.Calls
		view.Chars += use.Chars
		view.Agents = append(view.Agents, use)
	}
	return view
}

// agentList are the agents that called, in the order they first did, the main agent as "main".
func (u *usageLog) agentList() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.agents)
}
