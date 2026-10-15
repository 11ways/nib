// Command nib is a stdio MCP server that gives coding agents hashline file tools and nibash, plus its setup and Claude Code hook.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/server"
)

const usage = `nib: hashline file tools and a shell for coding agents
  nib                  run the MCP server on stdio
  nib setup            register nib in this folder (.mcp.json, .claude/settings.local.json, opencode.json); --user for every project, --dry-run to preview
  nib hook             Claude Code PreToolUse hook: refuses the built-in Read for files nib reads better, and tells nib's server which subagent calls it
  nib --version        print the version`

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	revision := ""
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 8 {
			revision = setting.Value[:8]
		}
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		v = "devel"
	}
	if revision != "" {
		return v + " " + revision
	}
	return v
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			os.Exit(runHook())
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		case "--version", "-v":
			fmt.Println("nib " + version())
			return
		case "--help", "-h":
			fmt.Println(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "Unknown command %s\n%s\n", os.Args[1], usage)
			os.Exit(2)
		}
	}
	// Under a file size limit a write past it fails with EFBIG; the signal would otherwise end nib mid-write.
	signal.Ignore(syscall.SIGXFSZ)
	directory := os.Getenv("NIB_DIRECTORY")
	if directory == "" {
		directory, _ = os.Getwd()
	}
	table, notes := names.Load(directory)
	for _, note := range notes {
		fmt.Fprintln(os.Stderr, "nib: "+note)
	}
	choice, err := decider.Parse(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nib:", err)
		os.Exit(1)
	}
	options := server.Options{
		Directory:           directory,
		ConfirmOutsideRoots: os.Getenv("NIB_CONFIRM_OUTSIDE_ROOTS") == "1",
		Sandbox:             os.Getenv("NIB_SANDBOX") == "1",
		Names:               table,
		Version:             version(),
	}
	s := newServer(options, choice.New())
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = s.Run(ctx, &mcp.StdioTransport{})
	s.Close()
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "nib:", err)
		os.Exit(1)
	}
}
