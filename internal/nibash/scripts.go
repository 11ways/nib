package nibash

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// optionKind is whether a wrapper's long option takes no value, a value attached with = or in the next word, or only an attached one.
type optionKind int

const (
	noValue optionKind = iota
	value
	attachedValue
)

// wrapper is the syntax of a program that runs the command its arguments name after its own options: options that bundle (flags), that
// take a value attached or as the next word (values), or only an attached one (optional); long options; how many operands come between the
// options and the command; whether NAME=VALUE words may precede it; and whether -N is an option, as nice's.
type wrapper struct {
	flags, values, optional string
	long                    map[string]optionKind
	operands                int
	assignments, digits     bool
}

// scriptWrappers are the programs whose command nibash finds for certain, each read as getopt reads it with a leading +, stopping at the
// first operand; the command rules' wrappers are read more loosely, as they only need what a call may run.
var scriptWrappers = map[string]wrapper{
	"timeout": {flags: "fpv", values: "ks", long: map[string]optionKind{"foreground": noValue, "preserve-status": noValue, "verbose": noValue,
		"kill-after": value, "signal": value}, operands: 1},
	"env": {flags: "i0v", values: "uCa", long: map[string]optionKind{"ignore-environment": noValue, "null": noValue, "debug": noValue,
		"list-signal-handling": noValue, "unset": value, "chdir": value, "argv0": value, "env0-from": value, "block-signal": attachedValue,
		"default-signal": attachedValue, "ignore-signal": attachedValue}, assignments: true},
	"nice":   {values: "n", long: map[string]optionKind{"adjustment": value}, digits: true},
	"nohup":  {},
	"stdbuf": {values: "ioe", long: map[string]optionKind{"input": value, "output": value, "error": value}},
	"setsid": {flags: "cfw", long: map[string]optionKind{"ctty": noValue, "fork": noValue, "wait": noValue}},
	"xargs": {flags: "0prtxo", values: "aEILnPsd", optional: "eil", long: map[string]optionKind{"null": noValue, "no-run-if-empty": noValue,
		"verbose": noValue, "interactive": noValue, "exit": noValue, "open-tty": noValue, "show-limits": noValue, "arg-file": value,
		"delimiter": value, "max-args": value, "max-procs": value, "max-chars": value, "max-lines": value, "process-slot-var": value,
		"replace": attachedValue, "eof": attachedValue}},
}

// findActions are find's actions besides -exec and its kind, with how many words follow each; with findTests they are the whole expression
// nibash reads.
var findActions = map[string]int{"-delete": 0, "-print0": 0, "-ls": 0, "-fls": 1, "-fprint": 1, "-fprint0": 1, "-printf": 1, "-fprintf": 2,
	"-fstype": 1, "-context": 1, "-warn": 0, "-nowarn": 0}

// namedScripts is args with ~name/ expanded in each script it hands a shell to run with -c, directly, behind a wrapper, in find's -exec or
// in xargs, where nibash would expand it in the script itself; the shell, a real program, knows no named directories.
func namedScripts(env expand.Environ, args []string) []string {
	at := scriptsIn(args, 0)
	if len(at) == 0 {
		return args
	}
	var out []string
	for _, i := range at {
		script := expandNames(env, args[i])
		if script == args[i] {
			continue
		}
		if out == nil {
			out = slices.Clone(args)
		}
		out[i] = script
	}
	if out == nil {
		return args
	}
	return out
}

// scriptsIn are the indexes, offset by base, of the shell scripts the command args runs; none where nibash cannot tell for certain.
func scriptsIn(args []string, base int) []int {
	if len(args) == 0 {
		return nil
	}
	name := filepath.Base(args[0])
	switch {
	case shells[name]:
		if i, ok := shellScript(args); ok {
			return []int{base + i}
		}
	case name == "find":
		return findScripts(args, base)
	default:
		if w, ok := scriptWrappers[name]; ok {
			if i, ok := w.command(args); ok {
				return scriptsIn(args[i:], base+i)
			}
		}
	}
	return nil
}

// shellScript is the index of the script a shell's -c runs: its first operand, after options that bundle, -o and -O with their value and
// the long options bash takes.
func shellScript(args []string) (int, bool) {
	command := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" || a == "-":
			if command && i+1 < len(args) {
				return i + 1, true
			}
			return 0, false
		case strings.HasPrefix(a, "--"):
			switch a {
			case "--login", "--noprofile", "--norc", "--posix", "--noediting", "--restricted", "--verbose":
			case "--rcfile", "--init-file":
				i++
			default:
				return 0, false
			}
		case len(a) > 1 && (a[0] == '-' || a[0] == '+'):
			for _, letter := range a[1:] {
				switch {
				case letter == 'c':
					if a[0] != '-' {
						return 0, false
					}
					command = true
				case letter == 'o' || letter == 'O':
					i++
				case !('a' <= letter && letter <= 'z' || 'A' <= letter && letter <= 'Z'):
					return 0, false
				}
			}
		default:
			if command {
				return i, true
			}
			return 0, false
		}
	}
	return 0, false
}

// command is the index of the command a wrapper's args run, past its options, operands and assignments.
func (w wrapper) command(args []string) (int, bool) {
	i := 1
options:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i++
			break options
		case a == "-" && w.assignments:
			// env's - is -i.
		case strings.HasPrefix(a, "--"):
			name, _, attached := strings.Cut(a[2:], "=")
			kind, ok := w.long[name]
			if !ok || kind == noValue && attached {
				return 0, false
			}
			if kind == value && !attached {
				i++
			}
		case len(a) > 1 && a[0] == '-':
			if w.digits && strings.Trim(a[1:], "0123456789") == "" {
				continue
			}
			for j := 1; j < len(a); j++ {
				letter := a[j]
				switch {
				case strings.IndexByte(w.flags, letter) >= 0:
				case strings.IndexByte(w.values, letter) >= 0:
					if j == len(a)-1 {
						i++
					}
					j = len(a)
				case strings.IndexByte(w.optional, letter) >= 0:
					j = len(a)
				default:
					return 0, false
				}
			}
		default:
			break options
		}
	}
	i += w.operands
	for w.assignments && i < len(args) && assignment.MatchString(args[i]) {
		i++
	}
	return i, i < len(args)
}

// findScripts are the shell scripts find's -exec, -execdir, -ok and -okdir run, offset by base; none when any word of its expression is one
// nibash does not know, as then it cannot tell which words are a command's.
func findScripts(args []string, base int) []int {
	i := 1
	for i < len(args) && (args[i] == "-P" || args[i] == "-H" || args[i] == "-L" || args[i] == "-D" || strings.HasPrefix(args[i], "-O")) {
		if args[i] == "-D" {
			i++
		}
		i++
	}
	for i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!" {
		i++
	}
	var at []int
	for i < len(args) {
		a := args[i]
		switch a {
		case "-exec", "-execdir", "-ok", "-okdir":
			// The command ends at ; or at a + right after {}.
			start := i + 1
			end := start
			for end < len(args) && args[end] != ";" && !(args[end] == "+" && end > start && args[end-1] == "{}") {
				end++
			}
			if end >= len(args) {
				return nil
			}
			at = append(at, scriptsIn(args[start:end], base+start)...)
			i = end + 1
			continue
		}
		words, ok := findTests[a]
		if !ok {
			words, ok = findActions[a]
		}
		if !ok && strings.HasPrefix(a, "-newer") && len(a) == len("-newerXY") {
			words, ok = 1, true
		}
		if !ok {
			return nil
		}
		i += 1 + words
	}
	return at
}

// expandNames is script with each ~name/ nibash would expand in it replaced by the directory, quoted where the shell would read it otherwise;
// a script that does not parse, and a name that is not defined, stay as they are.
func expandNames(env expand.Environ, script string) string {
	if !strings.Contains(script, "~") {
		return script
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(script), "")
	if err != nil {
		return script
	}
	type change struct {
		at, length int
		text       string
	}
	var changes []change
	documents := map[*syntax.Word]bool{}
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Redirect:
			// A here-document's delimiter and body have no tilde expansion, though words inside the body, as ${x:-~name/}, do.
			if n.Hdoc != nil {
				documents[n.Word], documents[n.Hdoc] = true, true
			}
		case *syntax.Word:
			if documents[n] || len(n.Parts) == 0 {
				return true
			}
			lit, ok := n.Parts[0].(*syntax.Lit)
			if !ok || !strings.HasPrefix(lit.Value, "~") {
				return true
			}
			// As mvdan's tilde expansion: the name runs to the first slash, and a word that goes on past an unslashed name is not expanded.
			name, _, slashed := strings.Cut(lit.Value[1:], "/")
			if name == "" || !slashed && len(n.Parts) > 1 {
				return true
			}
			directory := env.Get(homePrefix + name)
			if !directory.IsSet() {
				return true
			}
			at := int(lit.ValuePos.Offset())
			if at+1+len(name) > len(script) || script[at:at+1+len(name)] != "~"+name {
				return true
			}
			quoted, err := syntax.Quote(directory.String(), syntax.LangPOSIX)
			if err != nil {
				return true
			}
			changes = append(changes, change{at, 1 + len(name), quoted})
		}
		return true
	})
	// Walk visits words in source order but for a few nodes, such as a command's assignments and redirects among its words.
	slices.SortFunc(changes, func(a, b change) int { return a.at - b.at })
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		script = script[:c.at] + c.text + script[c.at+c.length:]
	}
	return script
}
