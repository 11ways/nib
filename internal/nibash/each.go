package nibash

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/names"
)

// gitEach is the name of nibash's command that runs one git command line in every repository below the shell's directory; no system
// program has it, and one that does wins.
const gitEach = "git-each"

// eachBase is the word git-each replaces in its arguments with each repository's integration branch.
const eachBase = "{base}"

// eachPlan runs git with args in each repository at and below the shell's directory, in path order, and prints only those whose git
// printed anything, each under a header naming it; through nib's git, so what reaches the model is shaped as git's own output is.
type eachPlan struct {
	args []string
}

func prepareGitEach(_ string, args []string) (plan, error) {
	if _, err := exec.LookPath(gitEach); err == nil {
		return nil, errForeign
	}
	return &eachPlan{args: args}, nil
}

func (p *eachPlan) run(inv *invocation) int {
	if len(p.args) == 0 {
		inv.fail("usage: git-each GIT-ARGUMENTS... (%s stands for each repository's integration branch)", eachBase)
		return 2
	}
	// git rm and git clean send what they delete to the trash only as nibash's own git runs them, one repository at a time.
	if _, _, subcommand, _, ok := gitArgs(append([]string{"git"}, p.args...)); ok && (subcommand == "rm" || subcommand == "clean") {
		inv.fail("runs no git %s: what it deletes goes to the trash only when it runs in one repository, as git -C REPOSITORY %s", subcommand, subcommand)
		return 2
	}
	repositories, capped, err := engine.Repositories(inv.ctx, inv.dir)
	if err != nil {
		inv.fail("%v", err)
		return 1
	}
	if capped {
		inv.fail("looked for repositories in the first folders below %s only", inv.dir)
	}
	stderr := inv.stderr
	defer func() { inv.stderr = stderr }()
	status := 0
	for _, repository := range repositories {
		// A repository whose git printed one line shows it on its header's line, so a one-line answer per repository reads as a table.
		shown := p.shown(inv, repository)
		inv.out.pending, inv.out.fold = []record{{text: "==> " + shown + " <=="}}, shown
		args, skipped := p.argsFor(inv, repository)
		if skipped != "" {
			if !inv.out.emit(record{text: "[skipped: " + skipped + "]"}) || !inv.out.unfold() {
				break
			}
			continue
		}
		inv.stderr = &headedWriter{out: inv.out, w: stderr}
		if p.runOne(inv, append([]string{"-C", repository}, args...)) != 0 {
			status = 1
		}
		if !inv.out.unfold() || inv.out.stopped || inv.ctx.Err() != nil {
			break
		}
	}
	return status
}

// runOne runs git with args as nib's git would, or as the system git where nib's does not take the form.
func (p *eachPlan) runOne(inv *invocation, args []string) int {
	if g, err := prepareGit("git", args); err == nil {
		return g.run(inv)
	}
	return streamed(inv, append([]string{"git"}, args...), func(line string) record { return record{text: line} })
}

// shown is how the header names repository: relative to the shell's directory, or as nib prints the path when that is shorter.
func (p *eachPlan) shown(inv *invocation, repository string) string {
	relative, err := filepath.Rel(inv.dir, repository)
	if err != nil {
		relative = repository
	}
	relative = filepath.ToSlash(relative)
	if display := inv.call.tools.Display(repository); len(display) < len(relative) {
		return display
	}
	return relative
}

// argsFor are the arguments with {base} replaced by repository's integration branch: what its nib.json names, else what origin/HEAD
// points at; skipped says why there is none when an argument needs it.
func (p *eachPlan) argsFor(inv *invocation, repository string) (args []string, skipped string) {
	base := ""
	for _, a := range p.args {
		if strings.Contains(a, eachBase) && base == "" {
			if base = baseBranch(inv, repository); base == "" {
				return nil, "no \"base\" in its " + names.ConfigFile + " and no origin/HEAD"
			}
		}
		args = append(args, strings.ReplaceAll(a, eachBase, base))
	}
	return args, ""
}

// baseBranch is repository's integration branch, "" when it has none.
func baseBranch(inv *invocation, repository string) string {
	if base := names.BaseBranch(repository); base != "" {
		return base
	}
	var out bytes.Buffer
	args := []string{"git", "-C", repository, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"}
	if inv.call.runProgramTo(inv.ctx, args, &out, io.Discard, nil) != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// headedWriter is a command's standard error that sends its output's pending header first, so an error shows under its repository.
type headedWriter struct {
	out *output
	w   io.Writer
	mu  sync.Mutex
}

func (h *headedWriter) Write(b []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.out.flushPending()
	return h.w.Write(b)
}

// eachAsProgram runs git-each where its output goes neither to the model nor to a nib command, as into a file or a real program, which
// gets git's own lines under the headers; handled is false when a real program of that name runs instead.
func (c *call) eachAsProgram(ctx context.Context, args []string) (handled bool, err error) {
	hc := interp.HandlerCtx(ctx)
	if _, err := interp.LookPathDir(hc.Dir, hc.Env, gitEach); err == nil {
		return false, nil
	}
	inv := &invocation{ctx: ctx, call: c, name: gitEach, dir: hc.Dir, stderr: hc.Stderr, in: &input{}, out: &output{ctx: ctx, writer: hc.Stdout}}
	if status := (&eachPlan{args: args[1:]}).run(inv); status != 0 {
		return true, interp.ExitStatus(status)
	}
	return true, nil
}
