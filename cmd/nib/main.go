// Command nib is a stdio MCP server that gives coding agents hashline file tools and nibash, plus its setup and Claude Code hook.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/providers"
	"github.com/11ways/nib/internal/server"
	"github.com/11ways/nib/internal/setup"
)

const usage = `nib: hashline file tools and a shell for coding agents
  nib                  run the MCP server on stdio
  nib --provider NAME=PROGRAM
                       also offer the tools of PROGRAM (run as PROGRAM nib-provider) as NAME_TOOL; repeatable
  nib setup            register nib in this folder (.mcp.json, .claude/settings.local.json, opencode.json); --user for every project, --dry-run to preview,
                       --provider NAME=PROGRAM to host a provider (NAME= stops hosting it), --decider KIND[:MODEL,...], --summary LIST and --opencode PROGRAM for every nib
  nib hook             Claude Code PreToolUse hook: refuses the built-in Read for files nib reads better, and tells nib's server which subagent calls it
  nib shape --log PATH --purpose TEXT [--exit N] [--command TEXT]
                       print what nibash would show of a program whose whole output is the log at PATH, for tools that keep their own logs
  nib --version        print the version`

// Keep the release name stable even when Go stamps an untagged checkout with a pseudo-version.
const releaseVersion = "v0.2.0"

func version() string {
	info, _ := debug.ReadBuildInfo()
	return buildVersion(info)
}

func buildVersion(info *debug.BuildInfo) string {
	v := releaseVersion
	if info == nil {
		return v
	}
	revision := ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if len(setting.Value) >= 8 {
				revision = setting.Value[:8]
			}
		case "vcs.modified":
			if setting.Value == "true" {
				v += "+dirty"
			}
		}
	}
	if revision != "" {
		return v + " " + revision
	}
	return v
}

func main() {
	var specs []providers.Spec
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "hook":
			os.Exit(runHook())
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		case "shape":
			os.Exit(runShape(os.Args[2:]))
		case "--version", "-v":
			fmt.Println("nib " + version())
			return
		case "--help", "-h":
			fmt.Println(usage)
			return
		}
		var err error
		if specs, err = serverFlags(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, usage)
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
	choice, err := deciderChoice()
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
	// The names agents got in this directory before, theirs and nib's, are named from the start; NIB_AUTO_NAMES=0 turns both off.
	if file := names.LearnedFile(table.Base); file != "" && os.Getenv("NIB_AUTO_NAMES") != "0" {
		options.NamesFile = file
		table.AddLearned(file)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	options.Providers = startProviders(ctx, specs, directory, options.Sandbox && os.Getenv("NIB_PROVIDERS_SANDBOX") != "1")
	s := newServer(options, choice.New(), choice.Summarizer())
	err = s.Run(ctx, &mcp.StdioTransport{})
	s.Close()
	for _, p := range options.Providers {
		p.Close()
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "nib:", err)
		os.Exit(1)
	}
}

// deciderChoice is the decider and summarizers this environment chooses: a choice nib setup remembered applies unless the environment makes one
// of its own.
func deciderChoice() (decider.Config, error) {
	home, _ := os.UserHomeDir()
	saved, err := setup.RememberedDecider(home, os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nib: the decider choice nib setup remembered is not used:", err)
	}
	return decider.Parse(decider.Layered(os.Getenv, saved))
}

// serverFlags reads the server's flags: --provider NAME=PROGRAM or --provider=NAME=PROGRAM, each name once.
func serverFlags(args []string) ([]providers.Spec, error) {
	var specs []providers.Spec
	for i := 0; i < len(args); i++ {
		value, ok := strings.CutPrefix(args[i], "--provider=")
		if !ok {
			if args[i] != "--provider" || i+1 == len(args) {
				return nil, fmt.Errorf("Unknown command or option %s", args[i])
			}
			i++
			value = args[i]
		}
		spec, err := providers.ParseSpec(value)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(specs, func(s providers.Spec) bool { return s.Name == spec.Name }) {
			return nil, fmt.Errorf("--provider %s is given twice", spec.Name)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// startProviders starts every provider at once, leaving out with a note on stderr each that fails, and all of them when sandboxed.
func startProviders(ctx context.Context, specs []providers.Spec, directory string, sandboxed bool) []*providers.Provider {
	if len(specs) > 0 && sandboxed {
		fmt.Fprintln(os.Stderr, "nib: providers are off under NIB_SANDBOX=1, as they run outside the sandbox; set NIB_PROVIDERS_SANDBOX=1 too to host them")
		return nil
	}
	started := make([]*providers.Provider, len(specs))
	var wait sync.WaitGroup
	for i, spec := range specs {
		wait.Go(func() {
			p, err := providers.Start(ctx, spec, directory)
			if err != nil {
				fmt.Fprintf(os.Stderr, "nib: provider %v; its tools are not offered\n", err)
				return
			}
			for _, skipped := range p.Skipped {
				fmt.Fprintf(os.Stderr, "nib: provider %s's tool %s, so it is not offered\n", p.Name, skipped)
			}
			started[i] = p
		})
	}
	wait.Wait()
	return slices.DeleteFunc(started, func(p *providers.Provider) bool { return p == nil })
}
