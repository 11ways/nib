package nibash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/server"
)

// A rule is one entry of nib.json's "rules": exact when it names programs, which refuses a matching command always, or vague when it is
// text, which a decision model judges each command against.
type rule struct {
	Program programs `json:"program"`
	// Args is a regular expression the command's arguments, joined by spaces, must match; empty matches any.
	Args string `json:"args"`
	Rule string `json:"rule"`
	Why  string `json:"why"`
	args *regexp.Regexp
}

func (r rule) exact() bool { return len(r.Program) > 0 }

// programs is one program name or a list of them, matched by base name.
type programs []string

func (p *programs) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*p = programs{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New(`"program" is a name or a list of names`)
	}
	*p = many
	return nil
}

// ruleFile is the rules of one nib.json, or why they cannot be read.
type ruleFile struct {
	path  string
	rules []rule
	err   error
}

// ruleCache keeps parsed rule files by path while their size and modification time stay the same.
type ruleCache struct {
	mu    sync.Mutex
	files map[string]cachedRules
}

type cachedRules struct {
	size     int64
	modified time.Time
	file     *ruleFile
}

var rulesRead = ruleCache{files: map[string]cachedRules{}}

// load reads path's rules; nil when the file has none.
func (c *ruleCache) load(path string, info os.FileInfo) *ruleFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	if got, ok := c.files[path]; ok && got.size == info.Size() && got.modified.Equal(info.ModTime()) {
		return got.file
	}
	file := parseRules(path)
	c.files[path] = cachedRules{info.Size(), info.ModTime(), file}
	return file
}

func parseRules(path string) *ruleFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return &ruleFile{path: path, err: err}
	}
	var config struct {
		Rules []rule `json:"rules"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return &ruleFile{path: path, err: err}
	}
	if len(config.Rules) == 0 {
		return nil
	}
	for i := range config.Rules {
		r := &config.Rules[i]
		switch {
		case r.exact() == (r.Rule != ""):
			return &ruleFile{path: path, err: fmt.Errorf("rule %d needs either \"program\" or \"rule\"", i+1)}
		case r.exact() && r.Why == "":
			return &ruleFile{path: path, err: fmt.Errorf("rule %d names programs but no \"why\" to tell the agent", i+1)}
		case !r.exact() && r.Args != "":
			return &ruleFile{path: path, err: fmt.Errorf("rule %d has \"args\" but no \"program\"", i+1)}
		}
		if r.Args != "" {
			if r.args, err = regexp.Compile(r.Args); err != nil {
				return &ruleFile{path: path, err: fmt.Errorf("rule %d: %v", i+1, err)}
			}
		}
		for j, p := range r.Program {
			r.Program[j] = filepath.Base(p)
		}
	}
	return &ruleFile{path: path, rules: config.Rules}
}

// governing is the nearest nib.json at or above directory that holds rules; under the sandbox only one inside the roots counts.
func governing(directory string, tools *engine.Context) *ruleFile {
	for d := directory; ; {
		path := filepath.Join(d, names.ConfigFile)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			allowed := true
			if tools.Sandbox {
				why, err := tools.Refused(path)
				allowed = err == nil && why == ""
			}
			if allowed {
				if file := rulesRead.load(path, info); file != nil {
					return file
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return nil
		}
		d = parent
	}
}

// simpleCommand is one command a call may run, as far as its words are known before it runs: unknown words keep their source text.
type simpleCommand struct {
	program string
	args    []string
}

func (c simpleCommand) String() string {
	return strings.Join(append([]string{c.program}, c.args...), " ")
}

// shells are the programs whose -c argument is itself a command line.
var shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true}

// wrappers run the command that follows their own options, and the short options of each that take the next word as their value.
var wrappers = map[string]string{"env": "uSC", "nohup": "", "exec": "a", "command": "", "builtin": "", "nice": "n", "timeout": "sk",
	"stdbuf": "ioe", "setsid": "", "sudo": "ugCDhpr", "xargs": "nIPdLsaE", "time": "fo", "ionice": "cnp", "chrt": ""}

var (
	duration   = regexp.MustCompile(`^[0-9.]+[smhd]?$`)
	assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// scan finds the commands a parsed call may run and the directories it cds into, starting in directory; a cd changes directory for
// what follows it in the source, which is only roughly where those commands run, so the caller applies every directory's rules to the
// whole call.
type scan struct {
	home      string
	view      namer
	commands  []simpleCommand
	dirs      []string
	directory string
}

func (s *scan) node(node syntax.Node, depth int) {
	syntax.Walk(node, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		words := make([]string, len(call.Args))
		known := make([]bool, len(call.Args))
		for i, w := range call.Args {
			words[i], known[i] = s.word(w)
		}
		s.command(words, known, depth)
		return true
	})
}

// command records one command line and looks into what it runs: a cd's directory, a wrapper's command, a shell's -c script, eval's words.
func (s *scan) command(words []string, known []bool, depth int) {
	for len(words) > 0 && known[0] {
		wrapper := filepath.Base(words[0])
		values, ok := wrappers[wrapper]
		if !ok {
			break
		}
		i := 1
		for i < len(words) && known[i] && (strings.HasPrefix(words[i], "-") || assignment.MatchString(words[i])) {
			if len(words[i]) == 2 && words[i][0] == '-' && strings.ContainsRune(values, rune(words[i][1])) {
				i++
			}
			i++
		}
		if wrapper == "timeout" && i < len(words) && duration.MatchString(words[i]) {
			i++
		}
		i = min(i, len(words))
		s.commands = append(s.commands, simpleCommand{wrapper, words[1:i]})
		words, known = words[i:], known[i:]
	}
	if len(words) == 0 {
		return
	}
	program := words[0]
	if known[0] {
		program = filepath.Base(program)
	}
	s.commands = append(s.commands, simpleCommand{program, words[1:]})
	if !known[0] {
		return
	}
	switch {
	case program == "cd" && len(words) > 1 && known[1]:
		target := words[1]
		if !filepath.IsAbs(target) {
			target = filepath.Join(s.directory, target)
		}
		s.directory = filepath.Clean(target)
		s.dirs = append(s.dirs, s.directory)
	case shells[program] && depth < 4:
		for i := 1; i+1 < len(words); i++ {
			if known[i] && strings.HasPrefix(words[i], "-") && !strings.HasPrefix(words[i], "--") && strings.Contains(words[i], "c") {
				if known[i+1] {
					s.source(words[i+1], depth+1)
				}
				break
			}
		}
	case program == "eval" && depth < 4 && !slices.Contains(known[1:], false):
		s.source(strings.Join(words[1:], " "), depth+1)
	}
}

// source scans a command line a command passes to a shell; one that does not parse is left to the shell to refuse.
func (s *scan) source(text string, depth int) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(text), "")
	if err == nil {
		s.node(file, depth)
	}
}

// word is w's value when it is known before the call runs: literals and quotes, with ~ and ~name/ expanded; else its source text.
func (s *scan) word(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			value := p.Value
			if i == 0 && strings.HasPrefix(value, "~") {
				name, rest, _ := strings.Cut(value[1:], "/")
				home := s.home
				if name != "" {
					found, ok := "", false
					if s.view != nil {
						found, ok = s.view.Home(name)
					}
					if !ok {
						return source(w), false
					}
					home = found
				}
				value = home
				if rest != "" || strings.Contains(p.Value, "/") {
					value += "/" + rest
				}
			}
			b.WriteString(value)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return source(w), false
				}
				b.WriteString(lit.Value)
			}
		default:
			return source(w), false
		}
	}
	return b.String(), true
}

func source(node syntax.Node) string {
	var b strings.Builder
	syntax.NewPrinter().Print(&b, node)
	return b.String()
}

// ruleCheck is what the rules say of a call before it runs: why it is refused, or notes to show beside its output.
type ruleCheck struct {
	refusal string
	notes   []string
}

// checkRules judges a parsed call against the rules of the directory it starts in and every directory it cds into, before anything runs.
func (s *Shell) checkRules(ctx context.Context, sess *session, file *syntax.File, request server.ShellCall, tools *engine.Context, view namer) ruleCheck {
	home, _ := os.UserHomeDir()
	sc := &scan{home: home, view: view, directory: tools.Directory}
	sc.node(file, 0)
	var files []*ruleFile
	for _, d := range append([]string{tools.Directory}, sc.dirs...) {
		if f := governing(d, tools); f != nil && !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return ruleCheck{}
	}
	var broken []string
	type vague struct {
		file *ruleFile
		rule rule
	}
	var vagues []vague
	for _, f := range files {
		if f.err != nil {
			return ruleCheck{refusal: fmt.Sprintf("[nothing ran: the rules in %s cannot be read: %v; fix that file]", f.path, f.err)}
		}
		for _, r := range f.rules {
			if !r.exact() {
				vagues = append(vagues, vague{f, r})
				continue
			}
			for _, c := range sc.commands {
				if slices.Contains(r.Program, c.program) && (r.args == nil || r.args.MatchString(strings.Join(c.args, " "))) {
					broken = append(broken, fmt.Sprintf("[nothing ran: %s is against a rule in %s: %s]", c, f.path, r.Why))
					break
				}
			}
		}
	}
	if len(broken) > 0 {
		return ruleCheck{refusal: strings.Join(broken, "\n")}
	}
	if len(vagues) == 0 {
		return ruleCheck{}
	}
	judge, _ := s.decider.(decider.RuleJudge)
	if judge == nil {
		return ruleCheck{notes: sess.uncheckedOnce(vagues[0].file.path, "no decision model is configured")}
	}
	texts := make([]string, len(vagues))
	for i, v := range vagues {
		texts[i] = v.rule.Rule
	}
	verdict := judge.Breaks(ctx, decider.RuleQuestion{Command: request.Command, Purpose: request.Description, Directory: tools.Directory, Rules: texts})
	if verdict.Failure != "" {
		return ruleCheck{notes: sess.uncheckedOnce(vagues[0].file.path, verdict.Failure)}
	}
	for _, i := range verdict.Broken {
		v := vagues[i]
		line := fmt.Sprintf("[nothing ran: %s judged it against a rule in %s: %s", verdict.Who, v.file.path, v.rule.Rule)
		if v.rule.Why != "" {
			line += " " + v.rule.Why
		}
		broken = append(broken, line+"]")
	}
	return ruleCheck{refusal: strings.Join(broken, "\n")}
}

// uncheckedOnce is the note that a file's vague rules went unchecked, shown to an agent once per file and reason.
func (sess *session) uncheckedOnce(path, reason string) []string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	key := path + "\x00" + reason
	if sess.unchecked[key] {
		return nil
	}
	if sess.unchecked == nil {
		sess.unchecked = map[string]bool{}
	}
	sess.unchecked[key] = true
	return []string{fmt.Sprintf("[the vague rules in %s were not checked (%s); the command ran]", path, reason)}
}
