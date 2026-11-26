package server

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hookstate"
)

// The timer tool sends Claude Code's main agent a message later, over nib's channel, which is the one way a server reaches an agent
// unasked and reaches only that agent: so the tool is offered only where the channel is on, and a subagent's call of it is refused.
const (
	timerTool = "timer"
	// timersMost bounds the timers of a session.
	timersMost = 8
	// timerLongest is the longest a timer waits, in minutes.
	timerLongest = 24 * 60
	// turnFor bounds how long a turn nib saw begin, and not end, counts as running after the last sign of it: a turn the user interrupts
	// ends with no word to any hook.
	turnFor = 30 * time.Minute
	// timerTick is how often the timers are looked at.
	timerTick = 5 * time.Second
)

const timerDescription = "Main agent only (a subagent's call is refused: the message would land in the main conversation). Send yourself message later, as a channel message that wakes this session. every: minutes between sends. idle: minutes the session must have been idle first (no turn running, no tool call by any agent), sent again after each further idle stretch, so idle: 50 keeps the prompt cache warm without interrupting work. once: only the first send. cancel: a timer's id. No arguments lists the timers."

const timerSchema = `{"type":"object","properties":{"message":{"type":"string"},"every":{"description":"Minutes","type":"number"},"idle":{"description":"Minutes","type":"number"},"once":{"type":"boolean"},"cancel":{"description":"Timer id","type":"integer"}}}`

// timer is one message to send: every so often, or each time the session has been idle for so long.
type timer struct {
	id          int
	message     string
	every, idle time.Duration
	once        bool
	// next is when an every timer sends next.
	next  time.Time
	sends int
}

func (t *timer) row(now time.Time) string {
	when := fmt.Sprintf("after %s idle", minutes(t.idle))
	if t.every > 0 {
		when = fmt.Sprintf("every %s, next in %s", minutes(t.every), elapsed(t.next.Sub(now)))
	}
	if t.once {
		when += ", once"
	}
	return fmt.Sprintf("timer %d: %s, sent %d times: %s", t.id, when, t.sends, t.message)
}

// minutes is a duration as the tool takes it.
func minutes(d time.Duration) string {
	return strconv.FormatFloat(d.Minutes(), 'f', -1, 64) + " min"
}

// activity is what the server knows of the session's work, and the timers that wait on it. The session is busy while a nib call runs or
// the main agent's turn does, and has been idle since the last sign of work otherwise: a nib call, or what the hook tells.
type activity struct {
	mu     sync.Mutex
	now    func() time.Time
	tick   time.Duration
	last   time.Time
	flying int
	turn   bool
	timers []*timer
	next   int
	// push sends a message to the main agent; set by the first timer made.
	push    func(content string, meta map[string]string) error
	running bool
	stop    chan struct{}
}

func newActivity() *activity {
	return &activity{now: time.Now, tick: timerTick, stop: make(chan struct{})}
}

// begin notes a nib call starting, the main agent's own beginning or carrying on its turn, and returns what notes its end.
func (a *activity) begin(main bool) func() {
	a.mu.Lock()
	a.flying++
	a.last = a.now()
	a.turn = a.turn || main
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		a.flying--
		a.last = a.now()
		a.mu.Unlock()
	}
}

// told notes what the session's hook saw: event of the agent named, "" for the main agent.
func (a *activity) told(event, agent string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last = a.now()
	if agent != "" {
		return
	}
	switch event {
	case hookstate.EventPrompt, hookstate.EventTool:
		a.turn = true
	case hookstate.EventStop:
		a.turn = false
	}
}

// idleFor is how long the session has been idle, and false while it is busy.
func (a *activity) idleFor(now time.Time) (time.Duration, bool) {
	quiet := now.Sub(a.last)
	if a.flying > 0 || a.turn && quiet < turnFor {
		return 0, false
	}
	return quiet, true
}

// due takes the messages to send now, in the timers' order.
func (a *activity) due() (sends []*timer, push func(string, map[string]string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	quiet, idle := a.idleFor(now)
	for _, t := range a.timers {
		switch {
		case t.every > 0 && !now.Before(t.next):
			t.next = now.Add(t.every)
		case t.idle > 0 && idle && quiet >= t.idle:
		default:
			continue
		}
		t.sends++
		sends = append(sends, t)
	}
	if len(sends) > 0 {
		// The message begins a turn, and a session that takes none is asked again only after as long.
		a.last, a.turn = now, true
		a.timers = slices.DeleteFunc(a.timers, func(t *timer) bool { return t.once && t.sends > 0 })
	}
	return sends, a.push
}

// run sends what is due until the server closes or no timer is left.
func (a *activity) run() {
	ticker := time.NewTicker(a.tick)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
		}
		sends, push := a.due()
		for _, t := range sends {
			kind := "idle"
			if t.every > 0 {
				kind = "every"
			}
			_ = push(t.message, map[string]string{"timer": strconv.Itoa(t.id), "kind": kind})
		}
		a.mu.Lock()
		if len(a.timers) == 0 {
			a.running = false
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
	}
}

// close ends the timers.
func (a *activity) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.stop:
	default:
		close(a.stop)
	}
}

// list is one row per timer.
func (a *activity) list() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.timers) == 0 {
		return "No timers."
	}
	rows := make([]string, len(a.timers))
	for i, t := range a.timers {
		rows[i] = t.row(a.now())
	}
	return strings.Join(rows, "\n")
}

// timer is the timer tool: it makes one, cancels one, or lists them, for Claude Code's main agent only.
func (s *Server) timer(_ *engine.Context, args arguments, notes *[]string, c *call) (engine.Result, error) {
	push := s.pushes(c)
	if push == nil {
		if c.agent != "" {
			return engine.Result{}, errors.New("timer is the main agent's only: its message lands in the main conversation and never reaches a subagent. Do not retry; wait with sleep in nibash, or tell the agent that started you what to check and when")
		}
		return engine.Result{}, errors.New("timer needs nib's hook, which tells the main agent's calls from a subagent's: run nib setup")
	}
	message, _, err := args.text("message")
	if err != nil {
		return engine.Result{}, err
	}
	numbers := map[string]float64{}
	for _, name := range []string{"every", "idle", "cancel"} {
		if args.has(name) {
			// A number sent as a string is read as the number.
			value, err := strconv.ParseFloat(strings.Trim(string(args[name]), `" `), 64)
			if err != nil || value <= 0 {
				return engine.Result{}, fmt.Errorf("%s is a number above 0, got %s", name, args[name])
			}
			numbers[name] = value
		}
	}
	once, _, err := args.flag("once", notes)
	if err != nil {
		return engine.Result{}, err
	}
	a := s.activity
	if id, ok := numbers["cancel"]; ok {
		a.mu.Lock()
		before := len(a.timers)
		a.timers = slices.DeleteFunc(a.timers, func(t *timer) bool { return float64(t.id) == id })
		gone := before != len(a.timers)
		a.mu.Unlock()
		if !gone {
			return engine.Result{}, fmt.Errorf("no timer %v; %s", id, a.list())
		}
		return engine.Result{Text: fmt.Sprintf("Cancelled timer %v.\n%s", id, a.list())}, nil
	}
	every, idle := numbers["every"], numbers["idle"]
	switch {
	case message == "" && every == 0 && idle == 0:
		return engine.Result{Text: a.list()}, nil
	case message == "":
		return engine.Result{}, errors.New("timer needs the message to send")
	case (every == 0) == (idle == 0):
		return engine.Result{}, errors.New("timer takes one of every and idle, in minutes")
	case every > timerLongest || idle > timerLongest:
		return engine.Result{}, fmt.Errorf("a timer waits %d minutes at most", timerLongest)
	}
	a.mu.Lock()
	if len(a.timers) >= timersMost {
		a.mu.Unlock()
		return engine.Result{}, fmt.Errorf("%d timers are the most; cancel one first:\n%s", timersMost, a.list())
	}
	a.next++
	t := &timer{id: a.next, message: message, once: once, every: time.Duration(every * float64(time.Minute)), idle: time.Duration(idle * float64(time.Minute))}
	t.next = a.now().Add(t.every)
	a.timers = append(a.timers, t)
	a.push = push
	start := !a.running
	a.running = true
	row := t.row(a.now())
	a.mu.Unlock()
	if start {
		go a.run()
	}
	return engine.Result{Text: "Set " + row + "\nIt lasts until cancelled or this session's nib server ends."}, nil
}
