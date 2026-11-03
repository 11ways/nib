package nibash

import (
	"context"
	"strings"
	"sync"
	"testing"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func TestRunnerOfTellsTheShellFromItsSubshells(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]*interp.Runner{}
	probe := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if args[0] != "probe" {
				return next(ctx, args)
			}
			mu.Lock()
			seen[args[1]] = runnerOf(ctx)
			mu.Unlock()
			return nil
		}
	}
	runner, err := interp.New(interp.ExecHandlers(probe))
	if err != nil {
		t.Fatal(err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader("probe shell; f() { probe function; }; f; (probe subshell); x=$(probe substitution); probe stage | true; true | probe last"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	// mvdan keeps the interpreter in an unexported field; nil here means it moved and runnerOf must follow it.
	for name, want := range map[string]bool{"shell": true, "function": true, "last": true, "subshell": false, "substitution": false, "stage": false} {
		got := seen[name]
		if got == nil || (got == runner) != want {
			t.Errorf("%s: runner %p, the shell's %p", name, got, runner)
		}
	}
}
