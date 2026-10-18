// Package engine implements nib's file tools: read, grep, find, edit and write.
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const (
	TextLimit   = 2 * 1024 * 1024
	MediaLimit  = 5 * 1024 * 1024
	OutputLimit = 40 * 1024
	LineLimit   = 2000
	ReadLines   = 2000
	// GrepContextLines is the context one grep page shows in all; past it every match gets less, except definitions that asked for theirs.
	GrepContextLines = 2 * grepContext

	previewContext    = 2
	editContext       = 1
	writtenRows       = 5
	revealLines       = 40
	revealContext     = 2
	revealColumns     = 512
	grepColumns       = 512
	grepFiles         = 20
	grepListFiles     = 200
	countRows         = 40
	grepPerFile       = 20
	grepSingleFile    = 100
	foldersOutsideGit = 2000
	containerEntries  = 1000
	grepContext       = 20
	findFiles         = 200
	findSniff         = 8000
	mapColumns        = 30
	mapGap            = 4
	mapStride         = 10
	mapRows           = 300
	editMapRows       = 40
	tokens            = 64
	cadenceLines      = 8
	echoMinLines      = 3
	echoMaxLines      = 200
	echoCandidates    = 16
	windows           = 3
	sinceNotes        = 10
	tokenAlphabet     = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// Context is per-call state. Authorize decides writes outside the workspace roots and returns an error to refuse; with Sandbox, no tool
// touches anything outside them. Names, when set, resolves ~name/ paths and gives every printed path its display form.
type Context struct {
	Ctx       context.Context
	Directory string
	Session   string
	Roots     func(context.Context) ([]string, error)
	Authorize func(ctx context.Context, canonical, action string) error
	Sandbox   bool
	Names     Names
	// Markers lets rows the agent already holds print as one [lines A-B ...] marker: nib learns when the agent's harness drops what it was
	// shown, and the call is not an immediate repeat, which asks for them again.
	Markers bool
}

// Names maps named directories to paths and back; the zero value of an implementation knows no names.
type Names interface {
	// Expand turns a leading ~name/ into its directory; ok is false when input names none.
	Expand(input string) (string, bool)
	// Display is the shortest name for canonical under a named directory; ok is false when none contains it.
	Display(canonical string) (string, bool)
}

// Image is an attached image.
type Image struct {
	Data     []byte
	MimeType string
}

// Result is a tool's output; Structured, when set, is a machine-readable twin of Text for the MCP result's structuredContent.
type Result struct {
	Text       string
	Images     []Image
	Structured any
}

// NotFound is a path that names nothing; read, grep and find note it and still serve their other paths.
type NotFound struct{ message string }

func (e *NotFound) Error() string { return e.message }

// Outside is a path the sandbox refuses; read, grep and find note it like a missing path, and an edit or write fails.
type Outside struct{ message string }

func (e *Outside) Error() string { return e.message }

// Refusal is a refused edit; Retry means it failed only on lines it has now displayed, so the same patch succeeds when sent again.
type Refusal struct {
	message string
	Retry   bool
}

func (e *Refusal) Error() string { return e.message }

// Paused ends a call that needs the client's answer first; Authorize returns it, and the call has then changed and recorded nothing, so the
// client can send it again with the answer. Request is the caller's own description of what to ask.
type Paused struct{ Request any }

func (e *Paused) Error() string { return "the call needs the client's answer first" }

// denied is an authorization refusal, which ends the call at once instead of being collected with a patch's other problems.
type denied struct{ cause error }

func (e *denied) Error() string { return e.cause.Error() }

func (ctx *Context) aborted() error { return ctx.Ctx.Err() }

func (ctx *Context) checkAbort() error {
	if err := ctx.Ctx.Err(); err != nil {
		return err
	}
	return nil
}

type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	ch    chan struct{}
	users int
}

var locks = keyedLocks{locks: map[string]*lockEntry{}}

// serialized runs run holding key's lock; waiting ends early when ctx is cancelled.
func serialized[T any](key string, ctx *Context, run func() (T, error)) (T, error) {
	var zero T
	locks.mu.Lock()
	entry := locks.locks[key]
	if entry == nil {
		entry = &lockEntry{ch: make(chan struct{}, 1)}
		locks.locks[key] = entry
	}
	entry.users++
	locks.mu.Unlock()
	release := func() {
		locks.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(locks.locks, key)
		}
		locks.mu.Unlock()
	}
	select {
	case entry.ch <- struct{}{}:
	case <-ctx.Ctx.Done():
		release()
		return zero, ctx.Ctx.Err()
	}
	defer func() {
		<-entry.ch
		release()
	}()
	if err := ctx.checkAbort(); err != nil {
		return zero, err
	}
	return run()
}

// serializedAll runs run holding the locks of all keys, taken in sorted order like locked's so it cannot deadlock against an edit.
func serializedAll[T any](keys []string, ctx *Context, run func() (T, error)) (T, error) {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var step func(index int) (T, error)
	step = func(index int) (T, error) {
		if index == len(sorted) {
			return run()
		}
		return serialized(sorted[index], ctx, func() (T, error) { return step(index + 1) })
	}
	return step(0)
}

func missing(err error) bool { return errors.Is(err, os.ErrNotExist) }

// expand resolves ~ and ~/ to the home directory and a leading ~name/ to its named directory.
func (ctx *Context) expand(input string) string {
	if input == "~" || strings.HasPrefix(input, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, input[1:])
	}
	if ctx.Names != nil {
		if expanded, ok := ctx.Names.Expand(input); ok {
			return expanded
		}
	}
	return input
}

// resolve is path.resolve(ctx.directory, expanded input).
func (ctx *Context) resolve(input string) string {
	expanded := ctx.expand(input)
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	return filepath.Join(ctx.Directory, expanded)
}

// display is how a canonical path prints: under its deepest named directory, else relative to the server's directory, else whole.
func (ctx *Context) display(canonical string) string {
	if ctx.Names != nil {
		if shown, ok := ctx.Names.Display(canonical); ok {
			return shown
		}
	}
	base, err := filepath.EvalSymlinks(ctx.Directory)
	if err != nil {
		base = ctx.Directory
	}
	if relative, ok := inside(base, canonical); ok {
		return relative
	}
	return canonical
}

// inside is the path of canonical relative to root when it lies strictly below it.
func inside(root, canonical string) (string, bool) {
	relative, err := filepath.Rel(root, canonical)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
		return "", false
	}
	return relative, true
}

// within reports whether canonical is root or lies below it.
func within(root, canonical string) bool {
	relative, err := filepath.Rel(root, canonical)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, "../") && !filepath.IsAbs(relative)))
}

// relativeDisplay is a listed path relative to the server's directory when inside it, as grep and find print paths ripgrep returned.
func (ctx *Context) relativeDisplay(name string) string {
	if ctx.Names != nil {
		if shown, ok := ctx.Names.Display(name); ok {
			return shown
		}
	}
	if within(ctx.Directory, name) {
		relative, _ := filepath.Rel(ctx.Directory, name)
		if relative == "." {
			return ""
		}
		return relative
	}
	return name
}
