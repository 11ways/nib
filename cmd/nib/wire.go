package main

import (
	"fmt"
	"io"
	"os"

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
// where nib's hook files them, and with where each OpenCode session comes from found in OpenCode's database; choose cuts nibash's long output (nil: the
// fixed cut).
func newServer(options server.Options, choose decider.Decider) *server.Server {
	options.Engine = engine.New()
	options.Agent = hookstate.OpenAgents(hookstate.Directory()).Take
	options.Origin = func(session string) (opencode.Origin, bool) {
		return opencode.Of(opencode.Databases(os.Getenv), session)
	}
	if os.Getenv("NIB_SHELL") != "0" {
		options.Shell = nibash.New(options.Engine, options.Sandbox).Deciding(choose)
	}
	return server.New(options)
}

// runHook answers one Claude Code PreToolUse call on stdin; it never fails the call, so a broken hook cannot block a tool.
func runHook() int {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 0
	}
	fmt.Print(hook.Run(input, ""))
	return 0
}

func runSetup(args []string) int { return setup.Main(args) }
