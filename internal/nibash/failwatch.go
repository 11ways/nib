package nibash

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/engine"
)

// stepVariable is assigned for one command only, as NIBASH_STEP=3 git push, to each command a call watches for failing, so the exec handler
// knows which one ran; no program sees it.
const stepVariable = "NIBASH_STEP"

// step is a command a call watches for failing: its name and its words as the command line wrote them, whether its standard error goes to
// /dev/null, the commands an && chain skips when it fails, whether what follows it handles its failure, and, for a pipeline's stage before
// its last, the stages after it, whose status the pipeline's is.
type step struct {
	name, text string
	discarded  bool
	skipped    []string
	handled    bool
	piped      string
}

// failedStep is a watched command that failed, by its step, its exit status and, when its standard error went to /dev/null, the first line
// it wrote there.
type failedStep struct {
	step, status int
	stderr       string
}

// predicates are the programs whose exit status 1 answers a question rather than reports a failure: no match, files that differ, a name
// that is no command.
var predicates = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "diff": true, "cmp": true, "which": true}

// watch is what watchFailures found to watch: each step, and the command that runs it, by the step's number.
type watch struct {
	steps []step
	calls []*syntax.CallExpr
	// functions are the names of the functions the command line declares, whose calls run their own watched commands.
	functions map[string]bool
	// commands counts the simple commands of the command line, builtins included.
	commands int
}

// scope is where a statement runs: whether its standard output and error go to /dev/null, whether its status is a condition, and, for the
// last statement of a list, what an && chain skips when it fails and whether what follows handles that; piped is the stages after a
// pipeline's stage, for one before its last.
type scope struct {
	outNull, discarded, condition, handled bool
	skipped                                []string
	piped                                  string
}

// watchFailures reads each program a call should watch for failing, before classify renames nib's own commands; mark then gives each its
// step. Builtins, functions and the stages of a pipeline are not watched.
func watchFailures(file *syntax.File) *watch {
	w := &watch{functions: map[string]bool{}}
	// Function bodies and command substitutions run in a context of their own, as for the guard of directory changes.
	roots := [][]*syntax.Stmt{file.Stmts}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CmdSubst:
			roots = append(roots, n.Stmts)
		case *syntax.ProcSubst:
			roots = append(roots, n.Stmts)
		case *syntax.FuncDecl:
			w.functions[n.Name.Value] = true
			roots = append(roots, []*syntax.Stmt{n.Body})
		case *syntax.CallExpr:
			// The guard's own command after a directory change is none of the command line's.
			if len(n.Args) > 0 {
				if name, _ := wordLiteral(n.Args[0]); !strings.HasPrefix(name, marker) {
					w.commands++
				}
			}
		}
		return true
	})
	for _, stmts := range roots {
		w.list(stmts, scope{})
	}
	return w
}

// list reads statements in order. Only the last one's status is the list's, so only it skips or is handled by what follows the list; one the
// next statement reads $? after is handled.
func (w *watch) list(stmts []*syntax.Stmt, sc scope) {
	for i, s := range stmts {
		inner := sc
		if i < len(stmts)-1 {
			inner.skipped, inner.handled = nil, readsStatus(stmts[i+1])
		}
		w.stmt(s, inner)
	}
}

func (w *watch) stmt(s *syntax.Stmt, sc scope) {
	if s.Background || s.Coprocess {
		return
	}
	sc = sc.redirected(s.Redirs)
	sc.condition = sc.condition || s.Negated
	switch cmd := s.Cmd.(type) {
	case *syntax.CallExpr:
		if !sc.condition {
			w.call(cmd, sc)
		}
	case *syntax.BinaryCmd:
		switch cmd.Op {
		case syntax.AndStmt:
			x := sc
			x.skipped = append([]string{printed(cmd.Y)}, sc.skipped...)
			w.stmt(cmd.X, x)
			w.stmt(cmd.Y, sc)
		case syntax.OrStmt:
			// An || true only silences a failure; anything else after || handles it.
			x := sc
			x.skipped, x.handled = nil, !silences(cmd.Y)
			w.stmt(cmd.X, x)
			w.stmt(cmd.Y, sc)
		case syntax.Pipe, syntax.PipeAll:
			w.pipeline(cmd, sc)
		}
	case *syntax.Block:
		w.list(cmd.Stmts, sc)
	case *syntax.Subshell:
		w.list(cmd.Stmts, sc)
	case *syntax.IfClause:
		for clause := cmd; clause != nil; clause = clause.Else {
			cond := sc
			cond.condition = true
			w.list(clause.Cond, cond)
			w.list(clause.Then, sc)
		}
	case *syntax.WhileClause:
		cond := sc
		cond.condition = true
		w.list(cmd.Cond, cond)
		w.list(cmd.Do, sc)
	case *syntax.ForClause:
		w.list(cmd.Do, sc)
	case *syntax.CaseClause:
		for _, item := range cmd.Items {
			w.list(item.Stmts, sc)
		}
	case *syntax.TimeClause:
		if cmd.Stmt != nil {
			w.stmt(cmd.Stmt, sc)
		}
	}
}

// pipeline watches each stage of a pipeline: the last as any statement, since the pipeline's status is its own, and each before it as a step
// whose failure that status hides, as go test's is in go test | grep FAIL.
func (w *watch) pipeline(cmd *syntax.BinaryCmd, sc scope) {
	list, _ := stages(cmd)
	for i, s := range list {
		if i == len(list)-1 {
			w.stmt(s, sc)
			break
		}
		call, ok := s.Cmd.(*syntax.CallExpr)
		if !ok || sc.condition || s.Background || s.Negated {
			continue
		}
		var after []string
		for _, later := range list[i+1:] {
			after = append(after, printed(later))
		}
		stage := sc.redirected(s.Redirs)
		stage.skipped, stage.handled, stage.piped = nil, false, "| "+strings.Join(after, " | ")
		w.call(call, stage)
	}
}

// call watches a program, which a builtin or a function of the command line is not.
func (w *watch) call(call *syntax.CallExpr, sc scope) {
	if len(call.Args) == 0 {
		return
	}
	name, ok := wordLiteral(call.Args[0])
	if ok && (interp.IsBuiltin(name) || w.functions[name] || strings.HasPrefix(name, marker)) {
		return
	}
	if !ok {
		name = ""
	}
	w.steps = append(w.steps, step{name: name, text: printed(call), discarded: sc.discarded, skipped: sc.skipped, handled: sc.handled, piped: sc.piped})
	w.calls = append(w.calls, call)
}

// silences says whether s is true or :, which after || only makes a failure go unseen.
func silences(s *syntax.Stmt) bool {
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) != 1 || len(s.Redirs) > 0 {
		return false
	}
	name, ok := wordLiteral(call.Args[0])
	return ok && (name == "true" || name == ":")
}

// readsStatus says whether s reads $?, as echo status=$? does.
func readsStatus(s *syntax.Stmt) bool {
	found := false
	syntax.Walk(s, func(node syntax.Node) bool {
		if p, ok := node.(*syntax.ParamExp); ok && p.Param != nil && p.Param.Value == "?" {
			found = true
		}
		return !found
	})
	return found
}

// redirected is the scope inside a statement with redirs, as far as they send standard output or error to /dev/null.
func (sc scope) redirected(redirs []*syntax.Redirect) scope {
	for _, r := range redirs {
		fd := "1"
		if r.N != nil {
			fd = r.N.Value
		}
		target, _ := wordLiteral(r.Word)
		null := target == "/dev/null"
		switch r.Op {
		case syntax.RdrAll, syntax.AppAll:
			sc.outNull, sc.discarded = null, null
		case syntax.RdrOut, syntax.AppOut, syntax.RdrClob:
			switch fd {
			case "1":
				sc.outNull = null
			case "2":
				sc.discarded = null
			}
		case syntax.DplOut:
			switch {
			case fd == "2" && target == "1":
				sc.discarded = sc.outNull
			case fd == "1" && target == "2":
				sc.outNull = sc.discarded
			}
		}
	}
	return sc
}

// printedLimit is how much of a command a note quotes.
const printedLimit = 60

// printed is node as the command line wrote it, its first printedLimit characters.
func printed(node syntax.Node) string {
	var b strings.Builder
	if err := syntax.NewPrinter(syntax.SingleLine(true)).Print(&b, node); err != nil {
		return "?"
	}
	return clip(strings.TrimSpace(b.String()))
}

// mark gives each watched command its step, once classify has read the command line as written.
func (w *watch) mark() {
	for id, call := range w.calls {
		call.Assigns = append(call.Assigns, &syntax.Assign{Name: &syntax.Lit{Value: stepVariable}, Value: literal(strconv.Itoa(id))})
	}
}

// recordStep records that the command name ran as the step env names, when it is that step's and exited with a status other than 0; stderr is
// the first line it wrote to a standard error that went to /dev/null.
func (c *call) recordStep(env expand.Environ, name string, err error, stderr string) {
	status, ok := err.(interp.ExitStatus)
	if !ok || status == 0 {
		return
	}
	id, convErr := strconv.Atoi(env.Get(stepVariable).String())
	if convErr != nil || id < 0 || id >= len(c.steps) {
		return
	}
	// A command run inside one that is watched, as a function's own, inherits its step without being it.
	if want := c.steps[id].name; want != "" && filepath.Base(want) != filepath.Base(name) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = append(c.failures, failedStep{step: id, status: int(status), stderr: stderr})
}

// told is the failure the call's exit status tells: the last one of a command whose status can be the call's, when it failed with that
// status; -1 when none did. A pipeline's stage before its last never is.
func (c *call) told(status int) int {
	for i := len(c.failures) - 1; i >= 0; i-- {
		if c.steps[c.failures[i].step].piped != "" {
			continue
		}
		if status == 0 || c.failures[i].status != status {
			return -1
		}
		return i
	}
	return -1
}

// exitLabel is the closing line of a call that exited with status: [exit N], naming the command that failed with it among several watched,
// and quoting the first line of its standard error when that went to /dev/null.
func (c *call) exitLabel(status int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	told := c.told(status)
	label := fmt.Sprintf("exit %d", status)
	if told >= 0 && c.commands > 1 {
		label += ": " + c.steps[c.failures[told].step].text
	}
	if told >= 0 && c.failures[told].stderr != "" {
		label += discardedLine(c.failures[told].stderr)
	}
	return "[" + label + "]"
}

// discardedLine is the clause that quotes the first line a command wrote to a standard error that went to /dev/null.
func discardedLine(line string) string {
	return fmt.Sprintf(", its stderr discarded: %q", line)
}

// failureNote names the commands that failed where the call's exit status does not say so and nothing after them handles it, while the
// failure could go unnoticed: their standard error went to /dev/null, an && chain skipped what followed, or a later stage of their pipeline
// gave its status; "" for none. An answer, as grep's 1 for no match, is no failure, nor is a stage's end by SIGPIPE when what reads it
// stopped, as head does.
func (c *call) failureNote(status int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	told := c.told(status)
	counts := map[int]int{}
	statuses := map[int]int{}
	lines := map[int]string{}
	var order []int
	for i, f := range c.failures {
		s := c.steps[f.step]
		if i == told || predicates[filepath.Base(s.name)] && f.status == 1 || s.handled || s.piped != "" && f.status == sigpipe ||
			!s.discarded && len(s.skipped) == 0 && s.piped == "" {
			continue
		}
		if counts[f.step] == 0 {
			order = append(order, f.step)
			statuses[f.step] = f.status
			lines[f.step] = f.stderr
		}
		counts[f.step]++
	}
	var clauses []string
	for _, id := range order {
		s := c.steps[id]
		clause := fmt.Sprintf("%s exited %d", s.text, statuses[id])
		if s.piped != "" {
			clause += " before " + clip(s.piped)
		}
		if counts[id] > 1 {
			clause += fmt.Sprintf(" (%d times)", counts[id])
		}
		switch {
		case lines[id] != "":
			clause += discardedLine(lines[id])
		case s.discarded:
			clause += ", its stderr discarded"
		}
		if len(s.skipped) > 0 {
			clause += ", so " + strings.Join(s.skipped, " && ") + " did not run"
		}
		clauses = append(clauses, clause)
	}
	if len(clauses) == 0 {
		return ""
	}
	if len(clauses) > noteFailures {
		clauses = append(clauses[:noteFailures], fmt.Sprintf("%d more failed", len(clauses)-noteFailures))
	}
	return "[nibash: " + strings.Join(clauses, "; ") + "]"
}

// noteFailures is how many failures a note names.
const noteFailures = 3

// sigpipe is the exit status of a program SIGPIPE ended, as one writing into a pipe its reader closed.
const sigpipe = 128 + 13

// clip is text cut to printedLimit characters.
func clip(text string) string {
	if runes := []rune(text); len(runes) > printedLimit {
		return string(runes[:printedLimit-3]) + "..."
	}
	return text
}

// keptLimit is how many bytes of a discarded standard error keptStderr reads for its first line, and keptColumns how much of that line a note
// quotes.
const (
	keptLimit   = 4096
	keptColumns = 200
)

// keptStderr stands for /dev/null as a command's standard error: it discards what is written, as /dev/null would, keeping only the start for
// the first line, which the note on the command's failure quotes.
type keptStderr struct {
	mu    sync.Mutex
	start []byte
}

func (k *keptStderr) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if room := keptLimit - len(k.start); room > 0 {
		k.start = append(k.start, b[:min(len(b), room)]...)
	}
	return len(b), nil
}

// line is the first line written that holds more than white space, without color codes, cut to keptColumns; "" for none.
func (k *keptStderr) line() string {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, line := range strings.Split(strings.ToValidUTF8(string(k.start), "�"), "\n") {
		if line = strings.TrimSpace(engine.PlainText(line)); line != "" {
			if runes := []rune(line); len(runes) > keptColumns {
				return string(runes[:keptColumns-3]) + "..."
			}
			return line
		}
	}
	return ""
}

type keptKey struct{}

// keepDiscarded gives a command whose standard error is /dev/null a keptStderr in its place, through ctx; nil for any other.
func keepDiscarded(ctx context.Context) (context.Context, *keptStderr) {
	if file, ok := interp.HandlerCtx(ctx).Stderr.(*os.File); !ok || file == nil || file.Name() != os.DevNull {
		return ctx, nil
	}
	kept := &keptStderr{}
	return context.WithValue(ctx, keptKey{}, kept), kept
}

// stderrOf is where the command ctx runs writes its standard error: its keptStderr, if keepDiscarded gave it one, else the shell's.
func stderrOf(ctx context.Context) io.Writer {
	if kept, ok := ctx.Value(keptKey{}).(*keptStderr); ok {
		return kept
	}
	return interp.HandlerCtx(ctx).Stderr
}
