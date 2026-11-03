package nibash

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// marker starts the renamed first word of a command nibash runs itself, so its exec handler knows it without guessing from a name the
// command line could shadow. No real program name can start with ESC.
const marker = "\x1bnib:"

// Reader modes in a marker: a nib command reading another nib command's records, plain bytes, or bytes among which nib writers send their
// rows in band; or a command that would be nib's own but for an option or pattern it lacks, which runs the system program and, for a row
// command, says so as its output reaches the model (ranAsSystem).
const (
	fromLink  = "L"
	fromBytes = "B"
	fromBand  = "W"
	asSystem  = "S"
)

// bandFlag follows the reader mode in a marker for a command whose rows go in band to a nib reader of its pipe's bytes.
const bandFlag = "+"

// modelFlag ends the marker of a git command whose output reaches the model through nothing but nib's own commands passing whole lines on
// (passesLines), which may then print it reshaped as at the terminal.
const modelFlag = ">"

// internal commands nibash adds for the builtins mvdan does not run for real processes or lacks, for starting a job, and for the end of a
// shell's command line, where it exits as bash does after its last command (see endShell).
const (
	internalJob     = "job"
	internalKill    = "kill"
	internalJobs    = "jobs"
	internalWait    = "wait"
	internalTrap    = "trap"
	internalDisown  = "disown"
	internalFg      = "fg"
	internalBg      = "bg"
	internalSuspend = "suspend"
	internalStart   = "start"
	internalEnd     = "end"
	internalLeave   = "leave"
	internalWaited  = "waited"
)

// jobBuiltins are the builtins nibash runs itself, wherever a command line names them, also through builtin or command.
var jobBuiltins = map[string]bool{internalKill: true, internalJobs: true, internalWait: true, internalTrap: true, internalDisown: true,
	internalFg: true, internalBg: true, internalSuspend: true}

// expansion stands for a word whose value only exists at run time.
const expansion = "\x00"

// jobVariable holds the spec of the last job a statement started with &, which $! is rewritten to.
const jobVariable = "NIBASH_JOB"

// output is where a statement's standard output goes.
type destination int

const (
	toTerminal destination = iota
	// toNib is a pipe a single nib command reads records from.
	toNib
	// toBand is a pipe a nib command reads as bytes, which a compound command, or a command whose form only running it decides, writes; nib's
	// own writers send their rows through it in band.
	toBand
	toOther
)

// verdict is how a command runs.
type verdict int

const (
	// program runs the program, with no note.
	program verdict = iota
	// own runs as nib's own.
	own
	// later is nib's own if the words its expansions give allow it, which only running it tells; else the system program runs, with a note
	// when a row command's output reaches the model (ranAsSystem).
	later
	// system runs the system program for a form nib's own lacks, with a note as a row command's output reaches the model (ranAsSystem).
	system
)

// pipe is a command printing rows that would be nib's own, whose output went into a real program before reaching the model.
type pipe struct{ command, program string }

// classifier marks the commands nibash runs itself.
type classifier struct {
	functions map[string]bool
	// piped are the row commands whose output reached the model through a real program, in the order the command line names them.
	piped []pipe
	// edges is whether the pipe the statements being classified write into in band reaches the model through nothing but nib's own commands
	// passing whole lines on (passesLines).
	edges bool
}

// classify rewrites file in place: its jobs start as startJobs makes them, and nib commands get a marker name. Functions the command defines
// shadow nib commands. piped lists the row commands whose rows went through a real program.
func classify(file *syntax.File) (jobs map[int]*syntax.Stmt, piped []pipe) {
	jobs = startJobs(file)
	c := &classifier{functions: map[string]bool{}}
	syntax.Walk(file, func(node syntax.Node) bool {
		if f, ok := node.(*syntax.FuncDecl); ok {
			c.functions[f.Name.Value] = true
		}
		return true
	})
	c.stmts(file.Stmts, toTerminal)
	return jobs, c.piped
}

// leaveAll ends the subshells of file and of the statements its jobs run (leaveSubshells), each once, as each job's statement is a tree of
// its own.
func leaveAll(file *syntax.File, jobs map[int]*syntax.Stmt) {
	leaveSubshells(file)
	for _, stmt := range jobs {
		leaveSubshells(stmt)
	}
}

// leaveSubshells ends the statements of every subshell, $(...) and <(...) under node with nibash's leave command, which runs the EXIT trap
// the subshell set as it ends, as bash runs it there and mvdan runs a shell's only (see leaving).
func leaveSubshells(node syntax.Node) {
	leave := func() *syntax.Stmt {
		return &syntax.Stmt{Cmd: &syntax.CallExpr{Args: []*syntax.Word{literal(marker + internalLeave)}}}
	}
	syntax.Walk(node, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Subshell:
			n.Stmts = append(n.Stmts, leave())
		case *syntax.CmdSubst:
			n.Stmts = append(n.Stmts, leave())
		case *syntax.ProcSubst:
			n.Stmts = append(n.Stmts, leave())
		}
		return true
	})
}

// startJobs turns every `X &` in file into `NIBASH_JOB=$(job k)`, with X kept in jobs under k, and makes $! read NIBASH_JOB: wherever it
// stands, in a function body, a $(...) or the statement of another job too, so each runs as one of nibash's jobs, which wait, kill and jobs
// know, rather than as mvdan's own background run.
func startJobs(file *syntax.File) map[int]*syntax.Stmt {
	jobs := map[int]*syntax.Stmt{}
	var rewrite func(node syntax.Node)
	rewrite = func(node syntax.Node) {
		syntax.Walk(node, func(node syntax.Node) bool {
			switch n := node.(type) {
			case *syntax.ParamExp:
				if n.Param != nil && n.Param.Value == "!" {
					n.Param.Value = jobVariable
				}
			case *syntax.Stmt:
				if !n.Background {
					return true
				}
				key := len(jobs)
				job := *n
				job.Background = false
				jobs[key] = &job
				rewrite(&job)
				start := &syntax.CallExpr{Args: []*syntax.Word{literal(marker + internalJob), literal(strconv.Itoa(key))}}
				n.Background = false
				n.Redirs = nil
				n.Negated = false
				n.Cmd = &syntax.CallExpr{Assigns: []*syntax.Assign{{
					Name:  &syntax.Lit{Value: jobVariable},
					Value: &syntax.Word{Parts: []syntax.WordPart{&syntax.CmdSubst{Stmts: []*syntax.Stmt{{Cmd: start}}}}},
				}}}
				return false
			}
			return true
		})
	}
	rewrite(file)
	return jobs
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

// mergesError reports whether a statement sends its standard error where its standard output goes (2>&1), which a link cannot carry.
func mergesError(s *syntax.Stmt) bool {
	for _, r := range s.Redirs {
		if r.Op == syntax.DplOut && r.N != nil && r.N.Value == "2" {
			if value, ok := wordLiteral(r.Word); ok && value == "1" {
				return true
			}
		}
	}
	return false
}

func (c *classifier) stmt(s *syntax.Stmt, out destination) {
	if redirectsOutput(s) {
		out = toOther
	}
	switch cmd := s.Cmd.(type) {
	case *syntax.CallExpr:
		c.call(cmd, out)
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

func literal(value string) *syntax.Word {
	return &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: value}}}
}

// stages flattens a pipeline into its stages, left to right, and says for each link whether it carries standard error too (|&, or 2>&1 on the
// stage writing it).
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
	for i := range all {
		all[i] = all[i] || mergesError(list[i])
	}
	return list, all
}

// pipeline classifies right to left. A stage writes to a nib reader only when the next stage is a single nib command: by a link when the stage
// is one too and its standard error stays out of the pipe, else through the pipe with its nib writers' rows in band.
func (c *classifier) pipeline(cmd *syntax.BinaryCmd, out destination) {
	list, all := stages(cmd)
	verdicts := make([]verdict, len(list))
	outs := make([]destination, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		stageOut := out
		if i < len(list)-1 {
			stageOut = toOther
			if verdicts[i+1] == own && !redirectsInput(list[i+1]) && !readsBytes(list[i+1]) {
				stageOut = toBand
			}
		}
		s := list[i]
		call, isCall := s.Cmd.(*syntax.CallExpr)
		if !isCall || s.Background || redirectsOutput(s) {
			if stageOut != toBand || redirectsOutput(s) {
				stageOut = toOther
			}
			outs[i] = stageOut
			// The commands inside write into the pipe in band, and their git may reshape its lines if what reads it passes them on whole.
			edges := c.edges
			c.edges = stageOut == toBand && c.throughLines(list, verdicts, outs, all, i, out)
			c.stmt(s, stageOut)
			c.edges = edges
			continue
		}
		verdicts[i] = c.check(call, stageOut)
		// A stage whose next stage in this pipeline reads it is joined by a link or plain bytes; the last stage writes where the pipeline does,
		// in band when that is a compound command's pipe into a nib reader, which every command inside it shares.
		if i < len(list)-1 {
			switch {
			case stageOut == toBand && verdicts[i] == own && !all[i]:
				stageOut = toNib
			case stageOut == toBand && verdicts[i] != own && verdicts[i] != later:
				stageOut = toOther
			}
		}
		outs[i] = stageOut
		if verdicts[i] == system {
			rename(call, asSystem)
		}
	}
	for i, s := range list {
		if verdicts[i] != own && verdicts[i] != later {
			continue
		}
		mode := fromBytes
		if i > 0 && !redirectsInput(s) {
			switch outs[i-1] {
			case toNib:
				mode = fromLink
			case toBand:
				mode = fromBand
			}
		}
		if outs[i] == toBand {
			mode += bandFlag
		}
		if call := s.Cmd.(*syntax.CallExpr); verdicts[i] == own && isGit(call) && (i == len(list)-1 || outs[i] == toNib) &&
			c.throughLines(list, verdicts, outs, all, i, out) {
			mode += modelFlag
		}
		rename(s.Cmd.(*syntax.CallExpr), mode)
	}
	c.notePiped(list, verdicts, out)
}

// throughLines says whether what stage i of a pipeline writes reaches the model through nothing but the stages after it passing whole lines
// on: each nib's own, reading the one before it whole, and passesLines, and the last writing where the pipeline does, which the model reads.
func (c *classifier) throughLines(list []*syntax.Stmt, verdicts []verdict, outs []destination, all []bool, i int, out destination) bool {
	if out != toTerminal && (out != toBand || !c.edges) {
		return false
	}
	for j := i + 1; j < len(list); j++ {
		if verdicts[j] != own || all[j-1] || j > i+1 && outs[j-1] != toNib || !passesLines(list[j]) {
			return false
		}
	}
	return outs[len(list)-1] == out
}

// passesLines says whether s is nib's own head or tail selecting whole lines of its standard input, or cat passing all of them, each line it
// keeps going on as it came.
func passesLines(s *syntax.Stmt) bool {
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 || redirectsInput(s) || redirectsOutput(s) {
		return false
	}
	words := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, ok := wordLiteral(word)
		if !ok {
			return false
		}
		words[i] = value
	}
	if _, name, marked := parseMarker(words[0]); marked {
		words[0] = name
	}
	stdin := func(operands []string) bool { return len(operands) == 0 || len(operands) == 1 && operands[0] == "-" }
	switch words[0] {
	case "head", "tail":
		p, err := prepareEdge(words[0] == "tail", words[1:])
		return err == nil && !p.(*edgePlan).bytes && stdin(p.(*edgePlan).operands)
	case "cat":
		p, err := prepareCat(words[0], words[1:])
		return err == nil && !p.(*catPlan).number && stdin(p.(*catPlan).operands)
	}
	return false
}

// isGit says whether call runs git by its literal name, or git-each, which runs it.
func isGit(call *syntax.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	name, ok := wordLiteral(call.Args[0])
	return ok && (name == "git" || name == gitEach)
}

// notePiped records each row command that would be nib's own whose output goes into a real program, when what that program prints reaches
// the model.
func (c *classifier) notePiped(list []*syntax.Stmt, verdicts []verdict, out destination) {
	reaches := out == toTerminal || out == toBand
	for i := len(list) - 1; i > 0; i-- {
		reaches = reaches && !redirectsOutput(list[i])
		if !reaches || verdicts[i] == own || verdicts[i] == later {
			continue
		}
		next, ok := list[i].Cmd.(*syntax.CallExpr)
		if !ok || len(next.Args) == 0 {
			continue
		}
		name, ok := wordLiteral(next.Args[0])
		if _, marked, isMarked := parseMarker(name); isMarked {
			name = marked
		}
		if !ok || name == "" {
			continue
		}
		if writer := c.rowWriter(list[i-1]); writer != "" {
			found := pipe{writer, filepath.Base(name)}
			if !slices.Contains(c.piped, found) {
				c.piped = append(c.piped, found)
			}
		}
	}
}

// rowWriter names the first command in s printing rows that would be nib's own if its output reached the model; "" for none.
func (c *classifier) rowWriter(s *syntax.Stmt) string {
	found := ""
	syntax.Walk(s, func(node syntax.Node) bool {
		if found != "" {
			return false
		}
		switch n := node.(type) {
		case *syntax.FuncDecl, *syntax.CmdSubst, *syntax.ProcSubst:
			return false
		case *syntax.Stmt:
			return !redirectsOutput(n)
		case *syntax.CallExpr:
			if len(n.Args) > 0 {
				if name, ok := wordLiteral(n.Args[0]); ok && commands[name].rows {
					if v := c.check(n, toTerminal); (v == own || v == later) && printsRows(n) {
						found = name
					}
				}
			}
			return false
		}
		return true
	})
	return found
}

// readsBytes says whether s is a nib command that counts its input's bytes, which must then come as a program wrote them, not as rows.
func readsBytes(s *syntax.Stmt) bool {
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok {
		return false
	}
	r, isReader := wordsPlan(call).(interface{ readsBytes() bool })
	return isReader && r.readsBytes()
}

// printsRows says whether a call of a row command prints lines as rows, not only names or counts, as grep -l and -c do.
func printsRows(call *syntax.CallExpr) bool {
	p, isPrinter := wordsPlan(call).(interface{ printsRows() bool })
	return !isPrinter || p.printsRows()
}

// wordsPlan is the plan of a nib command for call's words, an expansion standing in for each word only running it gives; nil when it has none.
func wordsPlan(call *syntax.CallExpr) plan {
	if len(call.Args) == 0 {
		return nil
	}
	words := make([]string, len(call.Args))
	for i, word := range call.Args {
		if value, ok := wordLiteral(word); ok {
			words[i] = value
		} else {
			words[i] = expansion
		}
	}
	cmd, found := commands[words[0]]
	if !found {
		return nil
	}
	p, err := cmd.prepare(words[0], words[1:])
	if err != nil {
		return nil
	}
	return p
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
func (c *classifier) call(call *syntax.CallExpr, out destination) {
	switch verdict := c.check(call, out); verdict {
	case own, later:
		mode := fromBytes
		if out == toBand {
			mode += bandFlag
			if verdict == own && c.edges && isGit(call) {
				mode += modelFlag
			}
		}
		rename(call, mode)
	case system:
		rename(call, asSystem)
	}
}

// check says how a command runs: as nib's own when it is a literal nib name no function shadows, with options it implements, and its output
// goes to the model or to another nib command; later when only the words its expansions give decide that; system when its output goes to the
// model but nib's own does not implement its options or pattern.
func (c *classifier) check(call *syntax.CallExpr, out destination) verdict {
	if out == toOther || len(call.Args) == 0 {
		return program
	}
	words := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, ok := wordLiteral(word)
		if !ok {
			if i == 0 {
				return program
			}
			// An expansion is an operand here, as it is to the tool at run time unless it expands to an option, which the command then
			// reports instead of running as the system program.
			value = expansion
		}
		words[i] = value
	}
	name := words[0]
	cmd, ok := commands[name]
	if !ok || c.functions[name] || len(call.Args[0].Parts) != 1 {
		return program
	}
	if _, isLit := call.Args[0].Parts[0].(*syntax.Lit); !isLit {
		return program
	}
	if _, err := cmd.prepare(name, words[1:]); err != nil {
		switch {
		case errors.Is(err, errUnsupported) && slices.Contains(words[1:], expansion):
			return later
		case out == toTerminal && !errors.Is(err, errForeign):
			return system
		}
		return program
	}
	return own
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

// heredocs are the texts the model typed whole: the bodies of the command's here-documents and here-strings when they are plain text, and
// what an echo of plain words prints.
func heredocs(file *syntax.File) []string {
	var bodies []string
	syntax.Walk(file, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok {
			if text, ok := echoed(call); ok {
				bodies = append(bodies, text)
			}
			return true
		}
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

// echoed is what an echo prints when every word is plain text that nothing expands, a glob, brace or tilde included, and no option comes
// first.
func echoed(call *syntax.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	if name, ok := wordLiteral(call.Args[0]); !ok || name != "echo" {
		return "", false
	}
	words := make([]string, 0, len(call.Args)-1)
	for i, arg := range call.Args[1:] {
		value, ok := wordLiteral(arg)
		if !ok || (i == 0 && strings.HasPrefix(value, "-")) {
			return "", false
		}
		for _, part := range arg.Parts {
			if lit, isLit := part.(*syntax.Lit); isLit && strings.ContainsAny(lit.Value, "*?[{~") {
				return "", false
			}
		}
		words = append(words, value)
	}
	return strings.Join(words, " ") + "\n", true
}
