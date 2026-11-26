package nibash

import (
	"regexp"
	"strings"
	"time"

	"github.com/11ways/nib/internal/fallback"
)

// FallingBack sets the log of the forms nib's own commands hand to a real program, from the path fallback.Default gives; "" keeps none.
// Call it before serving.
func (s *Shell) FallingBack(path string) *Shell {
	s.fallbacks = path
	return s
}

// fallbackKey is one form of one command handed to a real program, which a call counts.
type fallbackKey struct {
	command, kind, reason, form string
}

// fellBack counts a form the call handed to a real program, for the log its end writes.
func (c *call) fellBack(command, kind, reason, form string) {
	if c.session == nil || c.session.shell.fallbacks == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fallbacks == nil {
		c.fallbacks = map[fallbackKey]int{}
	}
	c.fallbacks[fallbackKey{command, kind, reason, form}]++
}

// writeFallbacks appends what the call counted to the log, one line per form, as its run ends.
func (c *call) writeFallbacks() {
	c.mu.Lock()
	counted := c.fallbacks
	c.fallbacks = nil
	c.mu.Unlock()
	if len(counted) == 0 {
		return
	}
	now, directory := time.Now().UTC(), ""
	if c.tools != nil {
		directory = c.tools.Directory
	}
	events := make([]fallback.Event, 0, len(counted))
	for key, count := range counted {
		events = append(events, fallback.Event{Time: now, Command: key.command, Kind: key.kind, Reason: key.reason, Form: key.form, Count: count, Directory: directory})
	}
	fallback.Record(c.session.shell.fallbacks, events)
}

// fallbackWord is a run of letters, digits and _ two or more long, which a form's operand text leaves out.
var fallbackWord = regexp.MustCompile(`[A-Za-z0-9_]{2,}`)

// fallbackForm is the shape of a command's arguments for the fallback log: its options as written, an option's =value, a long bundle's tail
// (which may be an attached value, as -epattern) and every operand as ..., and sed's script with each word in it as ..., so a form is counted
// without the data it worked on.
func fallbackForm(name string, args []string) string {
	words := []string{name}
	// sed's script is the value of its first -e, or else its first operand.
	scriptAt := -1
	if name == "sed" {
		for i, arg := range args {
			if (arg == "-e" || arg == "--expression") && i+1 < len(args) {
				scriptAt = i + 1
				break
			}
		}
		for i := 0; scriptAt < 0 && i < len(args); i++ {
			if !strings.HasPrefix(args[i], "-") || args[i] == "-" {
				scriptAt = i
			}
		}
	}
	operands := false
	for i, arg := range args {
		switch {
		case i == scriptAt:
			words = append(words, fallbackWord.ReplaceAllString(arg, "..."))
		case arg == "-" || (arg == "--" && !operands):
			words = append(words, arg)
			operands = operands || arg == "--"
		case operands:
			words = append(words, "...")
		case strings.HasPrefix(arg, "--"):
			option, _, valued := strings.Cut(arg, "=")
			if valued {
				option += "=..."
			}
			words = append(words, option)
		case strings.HasPrefix(arg, "-") && len(arg) > 4:
			words = append(words, arg[:2]+"...")
		case strings.HasPrefix(arg, "-"):
			words = append(words, arg)
		default:
			words = append(words, "...")
		}
	}
	return strings.Join(words, " ")
}

// printfSpecs are a printf format's conversions, % and all, which printf's form keeps.
var printfSpecs = regexp.MustCompile(`%[-+ #0']*(?:\*|[0-9]*)(?:\.(?:\*|[0-9]*))?[hjlLtz]*(?:\([^)]*\)T|.)?`)

// printfForm is printf's shape for the fallback log: -v when given, and the format's conversions without its text, which a %(...)T keeps
// as %(...)T.
func printfForm(variable, format string) string {
	words := []string{printfName}
	if variable != "" {
		words = append(words, "-v ...")
	}
	for _, spec := range printfSpecs.FindAllString(format, -1) {
		if strings.HasPrefix(spec, "%%") {
			continue
		}
		if at := strings.Index(spec, "("); at >= 0 {
			spec = spec[:at] + "(...)T"
		}
		words = append(words, spec)
	}
	if strings.Contains(format, `\u`) || strings.Contains(format, `\U`) {
		words = append(words, `\u`)
	}
	return strings.Join(words, " ")
}
