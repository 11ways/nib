package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hook"
	"github.com/11ways/nib/internal/hookstate"
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

// runHook answers one Claude Code hook call on stdin; it never fails the call, so a broken hook cannot block a tool.
func runHook() int {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	fmt.Print(hook.Run(input, ""))
	return 0
}

func runSetup(args []string) int { return setup.Main(args) }
