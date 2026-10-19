package nibash

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/names"
)

// internalHash is nibash's hash -d name=path and bare hash -d, zsh's named directory forms, which bash does not have.
const internalHash = "hash"

// namer is the caller's view of the named directories, which the server passes in each call's tools.
type namer interface {
	Home(name string) (string, bool)
	Expand(input string) (string, bool)
	Entries() []names.Entry
	Define(name, directory string) error
	// Observe counts an absolute path the command used toward the directories nib names for the agent.
	Observe(path string)
	// Worked notes a directory the agent changed into, so a long directory holding it is named at once.
	Worked(directory string)
}

// homePrefix starts the "HOME name" lookups tilde expansion makes for ~name.
const homePrefix = "HOME "

// named answers tilde expansion's "HOME name" lookups with the caller's named directories. It also lists them, never exported, so a subshell
// that copies the environment, a job or a pipeline stage, keeps them: a name in it means what it meant when the subshell started.
type named struct {
	expand.Environ
	names namer
}

func (n named) Get(name string) expand.Variable {
	if user, ok := strings.CutPrefix(name, homePrefix); ok && n.names != nil {
		if directory, ok := n.names.Home(user); ok {
			return expand.Variable{Set: true, Kind: expand.String, Str: directory}
		}
	}
	return n.Environ.Get(name)
}

func (n named) Each(yield func(name string, vr expand.Variable) bool) {
	stopped := false
	n.Environ.Each(func(name string, vr expand.Variable) bool {
		stopped = !yield(name, vr)
		return !stopped
	})
	if stopped || n.names == nil {
		return
	}
	for _, entry := range n.names.Entries() {
		if !yield(homePrefix+entry.Name, expand.Variable{Set: true, Kind: expand.String, Str: entry.Directory}) {
			return
		}
	}
}

// namedHash is true for the hash forms that are nibash's: -d with no operand, or -d with name=path operands only. Bash's own hash -d name,
// which forgets a remembered command, keeps its meaning.
func namedHash(args []string) bool {
	if len(args) < 2 || args[0] != "hash" || args[1] != "-d" {
		return false
	}
	for _, operand := range args[2:] {
		if name, _, ok := strings.Cut(operand, "="); !ok || name == "" || strings.HasPrefix(name, "-") {
			return false
		}
	}
	return true
}

// hash lists the caller's named directories as name=path, or defines each name=path, the path taken from the shell's directory, with ~ and
// ~name/ expanded.
func (c *call) hash(ctx context.Context, operands []string) error {
	hc := interp.HandlerCtx(ctx)
	view := c.names
	if view == nil {
		fmt.Fprintln(hc.Stderr, "hash: named directories are not available here")
		return interp.ExitStatus(1)
	}
	if len(operands) == 0 {
		entries := view.Entries()
		slices.SortFunc(entries, func(a, b names.Entry) int { return strings.Compare(a.Name, b.Name) })
		for _, entry := range entries {
			fmt.Fprintf(hc.Stdout, "%s=%s\n", entry.Name, entry.Directory)
		}
		return nil
	}
	status := 0
	for _, operand := range operands {
		name, path, _ := strings.Cut(operand, "=")
		directory, err := c.namedPath(hc, view, path)
		if err == nil {
			err = view.Define(name, directory)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "hash: ~%s: %v\n", name, err)
			status = 1
			continue
		}
		defined, _ := view.Home(name)
		fmt.Fprintf(hc.Stdout, "[~%s now names %s]\n", name, defined)
	}
	if status != 0 {
		return interp.ExitStatus(status)
	}
	return nil
}

// namedPath is the absolute directory a definition's path names, refused outside the sandbox.
func (c *call) namedPath(hc interp.HandlerContext, view namer, path string) (string, error) {
	switch {
	case path == "":
		return "", fmt.Errorf("no directory given")
	case path == "~" || strings.HasPrefix(path, "~/"):
		path = filepath.Join(hc.Env.Get("HOME").String(), path[1:])
	case strings.HasPrefix(path, "~"):
		expanded, ok := view.Expand(path)
		if !ok {
			return "", fmt.Errorf("%s names no directory", strings.SplitN(path, "/", 2)[0])
		}
		path = expanded
	}
	path = absolute(hc.Dir, path)
	if c.tools.Sandbox {
		if refused, err := c.tools.Refused(path); err != nil || refused != "" {
			if refused == "" {
				refused = err.Error()
			}
			return "", fmt.Errorf("%s", refused)
		}
	}
	return path, nil
}
