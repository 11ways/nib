package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hook"
	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/nibash"
	"github.com/11ways/nib/internal/opencode"
	"github.com/11ways/nib/internal/server"
	"github.com/11ways/nib/internal/setup"
)

// newServer builds the server, with nibash unless NIB_SHELL=0 turns the shell off, with each Claude Code subagent apart from the main agent
// where nib's hook files them, with when each Claude Code agent's context was last dropped where the hook files that too, and with where each
// OpenCode session comes from and when it was last compacted found in OpenCode's database; choose cuts nibash's long output (nil: the fixed
// cut), and summarize leads a cut with a checked summary (nil: none).
func newServer(options server.Options, choose decider.Decider, summarize decider.Summarizer) *server.Server {
	options.Engine = engine.New()
	if state := names.StateHome(); state != "" {
		options.Engine.Store.Keep(filepath.Join(state, "seen"))
	}
	options.Agent = hookstate.OpenAgents(hookstate.Directory()).Take
	options.Dropped = hookstate.OpenDrops(hookstate.Directory()).Take
	options.Origin = func(session string) (opencode.Origin, bool) {
		return opencode.Of(opencode.Databases(os.Getenv), session)
	}
	options.Compacted = func(session string) (time.Time, bool) {
		return opencode.Compacted(opencode.Databases(os.Getenv), session)
	}
	if os.Getenv("NIB_SHELL") != "0" {
		options.Shell = nibash.New(options.Engine, options.Sandbox).Deciding(choose).Summarizing(summarize)
	}
	return server.New(options)
}

// runHook answers one Claude Code hook call on stdin; it never fails the call, so a broken hook cannot block a tool or a message.
func runHook() int {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	fmt.Print(hook.Run(input, "", askNames))
	return 0
}

// promptWait bounds how long a message the user sent waits for the session's server to say which named directories it uses.
const promptWait = 3 * time.Second

// askNames asks the session's nib server, found as nib shape finds it, which named directories of a message the main agent must be told.
func askNames(mentioned []string, dropped time.Time) []string {
	ctx, cancel := context.WithTimeout(context.Background(), promptWait)
	defer cancel()
	request := server.ShapeRequest{Mentioned: mentioned}
	if !dropped.IsZero() {
		request.Dropped = dropped.UnixNano()
	}
	answer, ok := server.AskShape(ctx, shapeSockets(), request)
	if !ok || answer.Status != 0 || answer.Text == "" {
		return nil
	}
	return strings.Split(answer.Text, "\n")
}

func runSetup(args []string) int { return setup.Main(args) }
