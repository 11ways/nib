package nibash

import (
	"context"
	"reflect"

	"mvdan.cc/sh/v3/interp"
)

// runnerOf is the interpreter a handler runs in: the call's own, or a copy mvdan made for a subshell, a $(...), or a pipeline stage but the
// last. mvdan names it only in an unexported field of HandlerContext, which TestRunnerOfTellsTheShellFromItsSubshells pins; nil when the field
// cannot be read.
func runnerOf(ctx context.Context) *interp.Runner {
	field := reflect.ValueOf(interp.HandlerCtx(ctx)).FieldByName("runner")
	if !field.IsValid() || field.Type() != reflect.TypeFor[*interp.Runner]() {
		return nil
	}
	return (*interp.Runner)(field.UnsafePointer())
}

// inShell says whether a handler runs in the call's own interpreter, the shell bash's $$ names, rather than in a subshell's copy of it.
func (c *call) inShell(ctx context.Context) bool {
	return c.runner != nil && runnerOf(ctx) == c.runner
}
