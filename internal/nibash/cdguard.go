package nibash

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// internalStop is the command nibash puts after a directory change whose failure the command line leaves unhandled: it ends the call.
const internalStop = "stop"

// directoryChanges are the builtins that change the shell's directory, after whose failure the rest would run in the directory before.
var directoryChanges = map[string]bool{"cd": true, "pushd": true, "popd": true}

// stopped ends a call after a directory change failed with status, so nothing after it runs where the command did not mean it to.
type stopped struct {
	command string
	status  int
}

func (s *stopped) Error() string {
	return fmt.Sprintf("[%s failed, so nothing after it ran]", s.command)
}

// guardDirectoryChanges makes each cd, pushd and popd whose failure the command line leaves unhandled end the call when it fails. One is
// handled as under bash's set -e: in an if or while condition, before && or ||, after !, or in a pipeline before its last stage.
func guardDirectoryChanges(file *syntax.File) {
	// Function bodies and command substitutions run in a context of their own, unhandled until shown otherwise.
	roots := [][]*syntax.Stmt{file.Stmts}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.CmdSubst:
			roots = append(roots, n.Stmts)
		case *syntax.ProcSubst:
			roots = append(roots, n.Stmts)
		case *syntax.FuncDecl:
			roots = append(roots, []*syntax.Stmt{n.Body})
		}
		return true
	})
	for _, stmts := range roots {
		guardStmts(stmts, false)
	}
}

func guardStmts(list []*syntax.Stmt, handled bool) {
	for _, s := range list {
		guardStmt(s, handled)
	}
}

func guardStmt(s *syntax.Stmt, handled bool) {
	// A change in the background leaves the shell's own directory as it was.
	handled = handled || s.Negated || s.Background
	switch cmd := s.Cmd.(type) {
	case *syntax.CallExpr:
		if !handled && changesDirectory(cmd) {
			status := &syntax.Word{Parts: []syntax.WordPart{&syntax.ParamExp{Short: true, Param: &syntax.Lit{Value: "?"}}}}
			stop := &syntax.CallExpr{Args: []*syntax.Word{literal(marker + internalStop), status}}
			s.Cmd = &syntax.BinaryCmd{Op: syntax.OrStmt, X: &syntax.Stmt{Cmd: cmd}, Y: &syntax.Stmt{Cmd: stop}}
		}
	case *syntax.BinaryCmd:
		guardStmt(cmd.X, true)
		guardStmt(cmd.Y, handled)
	case *syntax.Block:
		guardStmts(cmd.Stmts, handled)
	case *syntax.Subshell:
		guardStmts(cmd.Stmts, handled)
	case *syntax.IfClause:
		for clause := cmd; clause != nil; clause = clause.Else {
			guardStmts(clause.Cond, true)
			guardStmts(clause.Then, handled)
		}
	case *syntax.WhileClause:
		guardStmts(cmd.Cond, true)
		guardStmts(cmd.Do, handled)
	case *syntax.ForClause:
		guardStmts(cmd.Do, handled)
	case *syntax.CaseClause:
		for _, item := range cmd.Items {
			guardStmts(item.Stmts, handled)
		}
	case *syntax.TimeClause:
		if cmd.Stmt != nil {
			guardStmt(cmd.Stmt, handled)
		}
	}
}

// changesDirectory says whether call runs cd, pushd or popd by its literal name, also through builtin or command.
func changesDirectory(call *syntax.CallExpr) bool {
	for _, word := range call.Args {
		name, ok := wordLiteral(word)
		if !ok {
			return false
		}
		if name != "builtin" && name != "command" {
			return directoryChanges[name]
		}
	}
	return false
}

// directoryChange is the words of the directory change args runs, from its name on, also through builtin or command; nil for another command.
func directoryChange(args []string) []string {
	for i, arg := range args {
		if arg != "builtin" && arg != "command" {
			if directoryChanges[arg] {
				return args[i:]
			}
			return nil
		}
	}
	return nil
}

// stop halts the call after the directory change it last ran failed with the exit status args gives: from then on every command the call
// reaches exits with it instead of running, in a subshell, function or command substitution too, where mvdan would let the shell go on.
func (c *call) stop(args []string) error {
	status := 1
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n != 0 {
			status = n
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	command := strings.Join(c.change, " ")
	if command == "" {
		command = "cd"
	}
	if c.halted == nil {
		c.halted = &stopped{command: command, status: status}
	}
	return interp.ExitStatus(status)
}

// halt is what halted the call, nil while nothing did.
func (c *call) halt() *stopped {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.halted
}

// haltNote is the note that a failed directory change halted the call, with err as the halt's exit status; "" and err for any other call.
func (c *call) haltNote(err error) (string, error) {
	if h := c.halt(); h != nil {
		return h.Error(), interp.ExitStatus(h.status)
	}
	return "", err
}

// jobError is err as a job's log ends with it: for a call a failed directory change halted, its note written to out and its status instead;
// a note of failures its exit status does not tell written to out too.
func (c *call) jobError(err error, out io.Writer) error {
	note, err := c.haltNote(err)
	if note != "" {
		fmt.Fprintln(out, note)
	}
	status := 0
	var exit interp.ExitStatus
	if errors.As(err, &exit) {
		status = int(exit)
	}
	if note := c.failureNote(status); note != "" {
		fmt.Fprintln(out, note)
	}
	return err
}
