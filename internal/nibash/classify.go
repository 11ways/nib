package nibash

import (
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// marker starts the renamed first word of a command nibash runs itself, so its exec handler knows it without guessing from a name the
// command line could shadow. No real program name can start with ESC.
const marker = "\x1bnib:"

// Reader modes in a marker: a nib command reading another nib command's records, or plain bytes.
const (
	fromLink  = "L"
	fromBytes = "B"
)

// internal commands nibash adds for builtins mvdan does not run for real processes, and for starting a job.
const (
	internalJob  = "job"
	internalKill = "kill"
	internalJobs = "jobs"
	internalWait = "wait"
)

// expansion stands for a word whose value only exists at run time.
const expansion = "\x00"

// jobVariable holds the spec of the last job a statement started with &, which $! is rewritten to.
const jobVariable = "NIBASH_JOB"

// output is where a statement's standard output goes.
type destination int

const (
	toTerminal destination = iota
	toNib
	toOther
)

// classifier marks the commands nibash runs itself and rewrites background statements into jobs.
type classifier struct {
	functions map[string]bool
	// jobs holds the statements & started, by the key their job-start command carries.
	jobs map[int]*syntax.Stmt
	next int
}

// classify rewrites file in place: nib commands get a marker name, `X &` becomes `NIBASH_JOB=$(job k)` with X kept in jobs, and $! reads
// NIBASH_JOB. functions are the names the session already defines, which shadow nib commands.
func classify(file *syntax.File, functions map[string]bool, firstJob int) map[int]*syntax.Stmt {
	c := &classifier{functions: map[string]bool{}, jobs: map[int]*syntax.Stmt{}, next: firstJob}
	for name := range functions {
		c.functions[name] = true
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if f, ok := node.(*syntax.FuncDecl); ok {
			c.functions[f.Name.Value] = true
		}
		return true
	})
	c.stmts(file.Stmts, toTerminal)
	syntax.Walk(file, func(node syntax.Node) bool {
		if p, ok := node.(*syntax.ParamExp); ok && p.Param != nil && p.Param.Value == "!" {
			p.Param.Value = jobVariable
		}
		return true
	})
	return c.jobs
}

func (c *classifier) stmts(list []*syntax.Stmt, out destination) {
	for _, s := range list {
		c.stmt(s, out)
	}
}

// redirectsOutput reports whether a statement sends its standard output somewhere of its own.
func redirectsOutput(s *syntax.Stmt) bool {
	for _, r := range s.Redirs {
		fd := "1"
		if r.N != nil {
			fd = r.N.Value
		}
		switch r.Op {
		case syntax.RdrAll, syntax.AppAll:
			return true
		case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.DplOut:
			if fd == "1" {
				return true
			}
		}
	}
	return false
}

func (c *classifier) stmt(s *syntax.Stmt, out destination) {
	if s.Background {
		c.background(s)
		return
	}
	if redirectsOutput(s) {
		out = toOther
	}
	switch cmd := s.Cmd.(type) {
	case *syntax.CallExpr:
		c.call(cmd, out, false)
	case *syntax.BinaryCmd:
		switch cmd.Op {
		case syntax.Pipe, syntax.PipeAll:
			c.pipeline(cmd, out)
		default:
			c.stmt(cmd.X, out)
			c.stmt(cmd.Y, out)
		}
	case *syntax.Block:
		c.stmts(cmd.Stmts, out)
	case *syntax.Subshell:
		c.stmts(cmd.Stmts, out)
	case *syntax.IfClause:
		for clause := cmd; clause != nil; clause = clause.Else {
			c.stmts(clause.Cond, out)
			c.stmts(clause.Then, out)
		}
	case *syntax.WhileClause:
		c.stmts(cmd.Cond, out)
		c.stmts(cmd.Do, out)
	case *syntax.ForClause:
		c.stmts(cmd.Do, out)
	case *syntax.CaseClause:
		for _, item := range cmd.Items {
			c.stmts(item.Stmts, out)
		}
	case *syntax.TimeClause:
		if cmd.Stmt != nil {
			c.stmt(cmd.Stmt, out)
		}
	}
	// Function bodies run wherever they are called, so their commands stay real; so do commands inside $(...) and <(...).
}

// background turns `X &` into `NIBASH_JOB=$(job k)`: the job command starts X as a job and prints its spec.
func (c *classifier) background(s *syntax.Stmt) {
	key := c.next
	c.next++
	job := *s
	job.Background = false
	c.jobs[key] = &job
	start := &syntax.CallExpr{Args: []*syntax.Word{literal(marker + internalJob), literal(strconv.Itoa(key))}}
	s.Background = false
	s.Redirs = nil
	s.Negated = false
	s.Cmd = &syntax.CallExpr{Assigns: []*syntax.Assign{{
		Name:  &syntax.Lit{Value: jobVariable},
		Value: &syntax.Word{Parts: []syntax.WordPart{&syntax.CmdSubst{Stmts: []*syntax.Stmt{{Cmd: start}}}}},
	}}}
}

func literal(value string) *syntax.Word {
	return &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: value}}}
}

// stages flattens a pipeline into its stages, left to right, and says for each link whether it carries standard error too (|&).
func stages(cmd *syntax.BinaryCmd) ([]*syntax.Stmt, []bool) {
	var list []*syntax.Stmt
	var all []bool
	var walk func(s *syntax.Stmt)
	walk = func(s *syntax.Stmt) {
		if b, ok := s.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) && !s.Background && !s.Negated && len(s.Redirs) == 0 {
			walk(b.X)
			all = append(all, b.Op == syntax.PipeAll)
			walk(b.Y)
			return
		}
		list = append(list, s)
	}
	walk(&syntax.Stmt{Cmd: cmd})
	return list, all
}

// pipeline classifies right to left: a stage writes to a nib reader only when the next stage is a single nib command.
func (c *classifier) pipeline(cmd *syntax.BinaryCmd, out destination) {
	list, all := stages(cmd)
	nib := make([]bool, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		stageOut := out
		if i < len(list)-1 {
			stageOut = toOther
			if nib[i+1] && !all[i] && !redirectsInput(list[i+1]) {
				stageOut = toNib
			}
		}
		s := list[i]
		if call, ok := s.Cmd.(*syntax.CallExpr); ok && !s.Background && !redirectsOutput(s) {
			nib[i] = c.check(call, stageOut)
			continue
		}
		c.stmt(s, toOther)
	}
	for i, s := range list {
		if !nib[i] {
			continue
		}
		mode := fromBytes
		if i > 0 && nib[i-1] && !all[i-1] && !redirectsInput(s) {
			mode = fromLink
		}
		rename(s.Cmd.(*syntax.CallExpr), mode)
	}
}

func redirectsInput(s *syntax.Stmt) bool {
	for _, r := range s.Redirs {
		switch r.Op {
		case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc, syntax.DplIn, syntax.RdrInOut:
			if r.N == nil || r.N.Value == "0" {
				return true
			}
		}
	}
	return false
}

// call classifies a command outside a pipeline.
func (c *classifier) call(call *syntax.CallExpr, out destination, _ bool) {
	if c.check(call, out) {
		rename(call, fromBytes)
	}
}

// check reports whether a command runs as nib's own: a literal nib name no function shadows, every word literal, options it implements, and
// output that goes to the model or to another nib command.
func (c *classifier) check(call *syntax.CallExpr, out destination) bool {
	if out == toOther || len(call.Args) == 0 {
		return false
	}
	words := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, ok := wordLiteral(word)
		if !ok {
			if i == 0 {
				return false
			}
			// An expansion is an operand here, as it is to the tool at run time unless it expands to an option, which the command then
			// reports instead of running as the system program.
			value = expansion
		}
		words[i] = value
	}
	name := words[0]
	prepare, ok := commands[name]
	if !ok || c.functions[name] || len(call.Args[0].Parts) != 1 {
		return false
	}
	if _, isLit := call.Args[0].Parts[0].(*syntax.Lit); !isLit {
		return false
	}
	_, err := prepare(name, words[1:])
	return err == nil
}

func rename(call *syntax.CallExpr, mode string) {
	name := call.Args[0].Parts[0].(*syntax.Lit).Value
	call.Args[0].Parts[0] = &syntax.Lit{Value: marker + mode + ":" + name}
}

// wordLiteral is a word's value when it has no expansion: plain text, single quotes, and double quotes around plain text. Glob characters stay,
// since they only make file operands, never options.
func wordLiteral(word *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(unescape(p.Value, false))
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				return "", false
			}
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(unescape(lit.Value, true))
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}

// unescape removes the backslashes bash removes: before any character outside quotes, and before $ ` " \ and newline inside double quotes.
func unescape(value string, quoted bool) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			next := value[i+1]
			if !quoted || strings.IndexByte("$`\"\\\n", next) >= 0 {
				if next != '\n' {
					b.WriteByte(next)
				}
				i++
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

// parseMarker splits a renamed command into its reader mode and name; ok is false for any other command.
func parseMarker(name string) (mode, command string, ok bool) {
	rest, found := strings.CutPrefix(name, marker)
	if !found {
		return "", "", false
	}
	mode, command, found = strings.Cut(rest, ":")
	if !found {
		return "", rest, true
	}
	return mode, command, true
}

// heredocs are the bodies of the command's here-documents and here-strings when they are plain text, which the model typed whole.
func heredocs(file *syntax.File) []string {
	var bodies []string
	syntax.Walk(file, func(node syntax.Node) bool {
		r, ok := node.(*syntax.Redirect)
		if !ok {
			return true
		}
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			if r.Hdoc != nil {
				if value, ok := wordLiteral(r.Hdoc); ok {
					bodies = append(bodies, value)
				}
			}
		case syntax.WordHdoc:
			if value, ok := wordLiteral(r.Word); ok {
				bodies = append(bodies, value+"\n")
			}
		}
		return true
	})
	return bodies
}
