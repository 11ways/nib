package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/hookstate"
)

// timerScene is a Claude Code session with nib's channel and hook, whose server's clock the test moves; sent waits for the channel
// messages to number n and returns their contents.
func timerScene(t *testing.T) (s *Server, r *records, call func(agent string, arguments map[string]any) *mcp.CallToolResult, pass func(time.Duration), sent func(n int) []string) {
	t.Helper()
	directory := resolvedPath(t, t.TempDir())
	r = &records{byUse: map[string]hookstate.Agent{}}
	s, session, watch := connectAs(t, Options{Directory: directory, Shell: &fakeShell{}, Agent: r.take, Channel: true, Dropped: (&drops{known: true, session: "s1"}).take}, channelClient, nil)
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	pass = func(d time.Duration) { c.set(c.read().Add(d)) }
	s.activity.now, s.activity.tick = c.read, time.Millisecond
	t.Cleanup(s.Close)
	call = func(agent string, arguments map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(context.Background(), r.as(agent, &mcp.CallToolParams{Name: timerTool, Arguments: arguments}))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	sent = func(n int) []string {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			messages := watch.messages()
			if len(messages) >= n || time.Now().After(deadline) {
				// A little longer shows a message that should not have come.
				time.Sleep(20 * time.Millisecond)
				messages = watch.messages()
				contents := make([]string, len(messages))
				for i, message := range messages {
					var got struct {
						Content string            `json:"content"`
						Meta    map[string]string `json:"meta"`
					}
					_ = json.Unmarshal([]byte(message), &got)
					contents[i] = got.Content + " " + got.Meta["kind"] + got.Meta["timer"]
				}
				if len(contents) != n {
					t.Fatalf("%d channel messages, want %d: %v", len(contents), n, contents)
				}
				return contents
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	return s, r, call, pass, sent
}

func TestTheTimerToolIsOfferedOnlyWithAChannelAndOnlyToTheMainAgent(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	_, session, _ := connectAs(t, Options{Directory: directory, Shell: &fakeShell{}}, channelClient, nil)
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == timerTool {
			t.Fatal("without a channel no message can be sent, so no timer is offered")
		}
	}
	_, _, call, _, _ := timerScene(t)
	refused := call("a1", map[string]any{"message": "check the build", "every": 5})
	if !refused.IsError || !strings.Contains(text(refused), "timer is the main agent's only") || !strings.Contains(text(refused), "Do not retry") {
		t.Fatalf("a subagent's timer: %q", text(refused))
	}
	if listed := call("", nil); text(listed) != "No timers." {
		t.Fatalf("the subagent's call must have set nothing: %q", text(listed))
	}
	for _, arguments := range []map[string]any{{"message": "x"}, {"message": "x", "every": 5, "idle": 5}, {"every": 5}, {"message": "x", "idle": 0}, {"message": "x", "every": 100000}, {"cancel": 7}} {
		if result := call("", arguments); !result.IsError {
			t.Errorf("%v must be refused, got %q", arguments, text(result))
		}
	}
	if len(timerDescription) > 2048 {
		t.Fatalf("the description is %d characters", len(timerDescription))
	}
}

func TestAnEveryTimerSendsAtItsIntervalUntilCancelled(t *testing.T) {
	_, _, call, pass, sent := timerScene(t)
	if set := call("", map[string]any{"message": "look at the queue", "every": 50}); set.IsError || !strings.HasPrefix(text(set), "Set timer 1: every 50 min, next in 50:00, sent 0 times: look at the queue") {
		t.Fatalf("got %q", text(set))
	}
	pass(49 * time.Minute)
	sent(0)
	pass(time.Minute)
	sent(1)
	pass(50 * time.Minute)
	if got := sent(2); got[1] != "look at the queue every1" {
		t.Fatalf("got %v", got)
	}
	if cancelled := call("", map[string]any{"cancel": 1}); cancelled.IsError || !strings.HasSuffix(text(cancelled), "No timers.") {
		t.Fatalf("got %q", text(cancelled))
	}
	pass(3 * time.Hour)
	sent(2)
}

func TestAnIdleTimerWaitsForTheSessionToBeIdleAndSendsAgainAfterEachIdleStretch(t *testing.T) {
	s, _, call, pass, sent := timerScene(t)
	if set := call("", map[string]any{"message": "keep warm", "idle": "5"}); set.IsError || !strings.Contains(text(set), "after 5 min idle") {
		t.Fatalf("got %q", text(set))
	}
	// The call that set it was the main agent's, so its turn runs: quiet minutes are not idle ones until the turn ends.
	pass(10 * time.Minute)
	sent(0)
	s.activity.told(hookstate.EventStop, "")
	pass(4 * time.Minute)
	sent(0)
	// A subagent's tool call ending is work: the idle minutes start over.
	s.activity.told(hookstate.EventTool, "a1")
	pass(4 * time.Minute)
	sent(0)
	pass(time.Minute)
	sent(1)
	// The message began a turn; once that ends, another idle stretch sends it again.
	pass(20 * time.Minute)
	sent(1)
	s.activity.told(hookstate.EventStop, "")
	pass(5 * time.Minute)
	if got := sent(2); got[1] != "keep warm idle1" {
		t.Fatalf("got %v", got)
	}
	// A turn whose end no hook told, as one the user interrupted, stops counting as running after turnFor.
	pass(turnFor)
	sent(3)
	// A nib call in flight is work however long it takes.
	s.activity.told(hookstate.EventStop, "")
	end := s.activity.begin(false)
	pass(time.Hour)
	sent(3)
	end()
	pass(5 * time.Minute)
	sent(4)
}

func TestAOnceTimerIsGoneAfterItsMessage(t *testing.T) {
	s, _, call, pass, sent := timerScene(t)
	call("", map[string]any{"message": "the deploy should be done", "every": 10, "once": true})
	s.activity.told(hookstate.EventStop, "")
	pass(10 * time.Minute)
	sent(1)
	pass(time.Hour)
	sent(1)
	if listed := call("", nil); text(listed) != "No timers." {
		t.Fatalf("got %q", text(listed))
	}
}
