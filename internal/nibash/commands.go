package nibash

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/11ways/nib/internal/engine"
)

// countAfter is how long a writer whose reader stopped keeps counting, for a fact like "20 of 312 matches".
const countAfter = 2 * time.Second

// invocation is one run of a nib command.
type invocation struct {
	ctx    context.Context
	call   *call
	name   string
	dir    string
	stderr io.Writer
	in     *input
	out    *output
	// inLink is the link the command reads, for facts about what its writer saw.
	inLink *link
	// outLink is the link it writes, whose stats it keeps.
	outLink *link
}

// plan is a command whose options nibash implements, ready to run.
type plan interface {
	run(inv *invocation) int
}

// command is one of nibash's own commands; prepare refuses with errUnsupported for anything it does not implement. renders marks a command
// that runs the real program and only reshapes its output, which has no rows to edit from; into another nib command its lines go as the
// program printed them. prepare refuses with errForeign for a form it never handles, which then runs as the system program without a note.
type command struct {
	prepare func(name string, args []string) (plan, error)
	renders bool
}

// commands are nibash's own commands.
var commands = map[string]command{
	"cat":   {prepare: prepareCat},
	"head":  {prepare: prepareHead},
	"tail":  {prepare: prepareTail},
	"grep":  {prepare: prepareGrep},
	"egrep": {prepare: prepareGrep},
	"fgrep": {prepare: prepareGrep},
	"rg":    {prepare: prepareRg},
	"sed":   {prepare: prepareSed},
	"wc":    {prepare: prepareWc},
	"git":   {prepare: prepareGit, renders: true},
	"find":  {prepare: prepareFind, renders: true},
	"ls":    {prepare: prepareLs, renders: true},
}

// fail prints a GNU-style error for a command.
func (inv *invocation) fail(format string, args ...any) {
	fmt.Fprintf(inv.stderr, inv.name+": "+format+"\n", args...)
}

// path is an operand made absolute against the shell's directory.
func (inv *invocation) path(operand string) string {
	if filepath.IsAbs(operand) {
		return operand
	}
	return filepath.Join(inv.dir, operand)
}

// open opens an operand through the engine; on failure it prints the GNU message and returns nil.
func (inv *invocation) open(operand string, quiet bool) *engine.Source {
	source, err := inv.call.engine.OpenSource(inv.call.tools, inv.path(operand))
	if err == nil {
		// A log shows as program output, so its rows reach the deciders and a summary; read still shows it as anchored rows.
		if source.View != nil && looksLikeLog(source.Canonical, source.View.Lines) {
			source.View = nil
		}
		return source
	}
	if !quiet {
		var outside *engine.Outside
		switch {
		case errors.As(err, &outside):
			inv.fail("%s", outside.Error())
		case errors.Is(err, engine.ErrDirectory):
			inv.fail("%s: Is a directory", operand)
		default:
			var missing *engine.NotFound
			if errors.As(err, &missing) || errors.Is(err, os.ErrNotExist) {
				inv.fail("%s: No such file or directory", operand)
			} else {
				inv.fail("%s: %v", operand, err)
			}
		}
	}
	return nil
}

// lines feeds each line of a source as a record with its origin when nib holds the file whole.
func lines(source *engine.Source, each func(record) bool) {
	_ = source.Lines(func(line int, text string) bool {
		r := record{text: text}
		if source.View != nil {
			r.origin = &origin{source: source, line: line}
		}
		return each(r)
	})
}

// stdin feeds standard input's records.
func (inv *invocation) stdin(each func(record) bool) {
	for {
		r, ok := inv.in.next()
		if !ok {
			return
		}
		if !each(r) {
			inv.in.stop()
			return
		}
	}
}

// --- cat

type catPlan struct {
	number   bool
	operands []string
}

func prepareCat(name string, args []string) (plan, error) {
	options, operands, err := parseOptions(optionSpec{flags: "n", long: map[string]string{"number": "n"}}, args)
	if err != nil {
		return nil, err
	}
	p := &catPlan{operands: operands}
	for _, o := range options {
		p.number = p.number || o.name == "n"
	}
	return p, nil
}

func (p *catPlan) run(inv *invocation) int {
	operands := p.operands
	if len(operands) == 0 {
		operands = []string{"-"}
	}
	status, number := 0, 0
	emit := func(r record) bool {
		if p.number {
			number++
			r.text = fmt.Sprintf("%6d\t%s", number, r.text)
		}
		return inv.out.emit(r)
	}
	for _, operand := range operands {
		if inv.out.stopped {
			break
		}
		if operand == "-" {
			inv.stdin(emit)
			continue
		}
		source := inv.open(operand, false)
		if source == nil {
			status = 1
			continue
		}
		total := 0
		lines(source, func(r record) bool {
			total++
			return emit(r)
		})
		if inv.outLink != nil && len(operands) == 1 && source.View != nil {
			inv.outLink.stats = linkStats{kind: "lines", total: len(source.View.Lines), single: source}
		}
	}
	return status
}

// --- head and tail

type edgePlan struct {
	tail     bool
	count    int
	from     bool // tail -n +N
	operands []string
}

func prepareHead(name string, args []string) (plan, error) { return prepareEdge(false, args) }
func prepareTail(name string, args []string) (plan, error) { return prepareEdge(true, args) }

func prepareEdge(tail bool, args []string) (plan, error) {
	options, operands, err := parseOptions(optionSpec{values: "n", numeric: true, long: map[string]string{"lines=": "n"}}, args)
	if err != nil {
		return nil, err
	}
	p := &edgePlan{tail: tail, count: 10, operands: operands}
	for _, o := range options {
		value := o.value
		if tail && strings.HasPrefix(value, "+") {
			p.from = true
			value = value[1:]
		}
		n, ok := count(value)
		if !ok {
			// A negative count (all but the last N) and suffixes like 5K are left to the real program.
			return nil, errUnsupported
		}
		p.count = n
	}
	return p, nil
}

func (p *edgePlan) run(inv *invocation) int {
	operands := p.operands
	if len(operands) == 0 {
		operands = []string{"-"}
	}
	status := 0
	for index, operand := range operands {
		if len(operands) > 1 {
			if index > 0 {
				inv.out.emit(record{text: ""})
			}
			name := operand
			if operand == "-" {
				name = "standard input"
			}
			inv.out.emit(record{text: fmt.Sprintf("==> %s <==", name)})
		}
		if operand == "-" {
			p.edgeOfStdin(inv)
			continue
		}
		source := inv.open(operand, false)
		if source == nil {
			status = 1
			continue
		}
		p.edgeOfFile(inv, source)
	}
	return status
}

func (p *edgePlan) edgeOfFile(inv *invocation, source *engine.Source) {
	var kept []record
	total := 0
	lines(source, func(r record) bool {
		total++
		switch {
		case !p.tail:
			if total > p.count {
				return false
			}
			kept = append(kept, r)
		case p.from:
			if total >= max(1, p.count) {
				kept = append(kept, r)
			}
		default:
			kept = append(kept, r)
			if len(kept) > p.count {
				kept = kept[1:]
			}
		}
		return true
	})
	if !p.tail && total > p.count && source.View != nil {
		total = len(source.View.Lines)
	}
	for _, r := range kept {
		if !inv.out.emit(r) {
			break
		}
	}
	if inv.out.sink != nil && len(kept) > 0 && len(kept) < total {
		first, last := 1, len(kept)
		if p.tail {
			first, last = total-len(kept)+1, total
		}
		display := source.Display
		inv.out.sink.fact(func() string { return fmt.Sprintf("[%s: %s of %s]", display, lineSpan(first, last), thousands(total)) })
	}
}

func (p *edgePlan) edgeOfStdin(inv *invocation) {
	if !p.tail {
		shown := 0
		files := map[string]bool{}
		if p.count > 0 {
			inv.stdin(func(r record) bool {
				shown++
				if r.origin != nil {
					files[r.origin.source.Canonical] = true
				}
				inv.out.emit(r)
				return shown < p.count
			})
		} else {
			inv.in.stop()
		}
		inv.edgeFact(shown, len(files))
		return
	}
	// tail of program output keeps everything it read in a log, so the model can grep it instead of running the command again.
	if inv.in.link == nil && inv.out.sink != nil {
		inv.in.all = &strings.Builder{}
	}
	var kept []record
	total := 0
	inv.stdin(func(r record) bool {
		total++
		if p.from {
			if total >= max(1, p.count) {
				inv.out.emit(r)
			}
			return true
		}
		kept = append(kept, r)
		if len(kept) > p.count {
			kept = kept[1:]
		}
		return true
	})
	for _, r := range kept {
		inv.out.emit(r)
	}
	if inv.in.all != nil && total > len(kept) && !p.from {
		path := inv.call.logs.save(inv.in.all.String()).path
		where := ""
		if path != "" {
			where = "; all of them: " + path
		}
		shown, all := len(kept), total
		inv.out.sink.fact(func() string { return fmt.Sprintf("[tail: last %d of %s lines%s]", shown, thousands(all), where) })
	}
}

// edgeFact says, for head at the terminal reading another nib command, how much its writer had in all.
func (inv *invocation) edgeFact(shown, files int) {
	if inv.out.sink == nil || inv.inLink == nil {
		return
	}
	l := inv.inLink
	inv.out.sink.fact(func() string {
		s := l.stats
		switch s.kind {
		case "grep":
			if s.total <= shown {
				return ""
			}
			atLeast := ""
			if s.partial {
				atLeast = "at least "
			}
			return fmt.Sprintf("[head: %d of %s%s matching lines shown, from %d of %s%d files]", shown, atLeast, thousands(s.total), files, atLeast, s.files)
		case "lines":
			if s.total <= shown || s.single == nil {
				return ""
			}
			return fmt.Sprintf("[%s: %s of %s]", s.single.Display, lineSpan(1, shown), thousands(s.total))
		}
		return ""
	})
}

// lineSpan is "line N" or "lines N-M".
func lineSpan(first, last int) string {
	if first == last {
		return fmt.Sprintf("line %d", first)
	}
	return fmt.Sprintf("lines %d-%d", first, last)
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// --- sed -n

type sedPlan struct {
	ranges   [][2]int // end 0 means to the last line, end -1 means one line
	operands []string
}

func prepareSed(name string, args []string) (plan, error) {
	options, operands, err := parseOptions(optionSpec{flags: "nEr", values: "e", long: map[string]string{"quiet": "n", "silent": "n", "expression=": "e", "regexp-extended": "E"}}, args)
	if err != nil {
		return nil, err
	}
	quiet := false
	var scripts []string
	for _, o := range options {
		switch o.name {
		case "n":
			quiet = true
		case "e":
			scripts = append(scripts, o.value)
		}
	}
	if !quiet {
		return nil, errUnsupported
	}
	if len(scripts) == 0 {
		if len(operands) == 0 {
			return nil, errUnsupported
		}
		scripts, operands = operands[:1], operands[1:]
	}
	p := &sedPlan{operands: operands}
	for _, script := range scripts {
		for _, command := range strings.FieldsFunc(script, func(r rune) bool { return r == ';' || r == '\n' }) {
			command = strings.TrimSpace(command)
			if !strings.HasSuffix(command, "p") {
				return nil, errUnsupported
			}
			address := strings.TrimSpace(strings.TrimSuffix(command, "p"))
			from, to, hasRange := strings.Cut(address, ",")
			start, ok := count(strings.TrimSpace(from))
			if !ok || start == 0 {
				return nil, errUnsupported
			}
			end := -1
			if hasRange {
				to = strings.TrimSpace(to)
				if to == "$" {
					end = 0
				} else if end, ok = count(to); !ok {
					return nil, errUnsupported
				}
			}
			p.ranges = append(p.ranges, [2]int{start, end})
		}
	}
	if len(p.ranges) == 0 {
		return nil, errUnsupported
	}
	return p, nil
}

func (p *sedPlan) selected(line int) bool {
	for _, r := range p.ranges {
		switch {
		case r[1] == -1 && line == r[0]:
			return true
		case r[1] == 0 && line >= r[0]:
			return true
		case r[1] > 0 && line >= r[0] && (line <= r[1] || (r[1] < r[0] && line == r[0])):
			return true
		}
	}
	return false
}

// last is the highest line any range can select, so sed stops reading after it; 0 when a range runs to the end.
func (p *sedPlan) last() int {
	top := 0
	for _, r := range p.ranges {
		switch r[1] {
		case 0:
			return 0
		case -1:
			top = max(top, r[0])
		default:
			top = max(top, r[0], r[1])
		}
	}
	return top
}

func (p *sedPlan) run(inv *invocation) int {
	operands := p.operands
	if len(operands) == 0 {
		operands = []string{"-"}
	}
	// Like GNU sed without -s, the files are one stream and their lines are numbered on.
	line, status, last := 0, 0, p.last()
	each := func(r record) bool {
		line++
		if p.selected(line) {
			if !inv.out.emit(r) {
				return false
			}
		}
		return last == 0 || line < last
	}
	for _, operand := range operands {
		if last != 0 && line >= last {
			break
		}
		if operand == "-" {
			inv.stdin(each)
			continue
		}
		source := inv.open(operand, false)
		if source == nil {
			status = 2
			continue
		}
		lines(source, each)
	}
	return status
}

// --- wc

type wcPlan struct {
	lines, words, bytes bool
	operands            []string
}

func prepareWc(name string, args []string) (plan, error) {
	options, operands, err := parseOptions(optionSpec{flags: "lwc", long: map[string]string{"lines": "l", "words": "w", "bytes": "c"}}, args)
	if err != nil {
		return nil, err
	}
	p := &wcPlan{operands: operands}
	for _, o := range options {
		switch o.name {
		case "l":
			p.lines = true
		case "w":
			p.words = true
		case "c":
			p.bytes = true
		}
	}
	if !p.lines && !p.words && !p.bytes {
		p.lines, p.words, p.bytes = true, true, true
	}
	return p, nil
}

type tally struct{ lines, words, bytes int }

func (t *tally) add(text string, newline bool) {
	if newline {
		t.lines++
		t.bytes++
	}
	t.bytes += len(text)
	t.words += len(strings.Fields(text))
}

func (p *wcPlan) format(t tally, name string, width int) string {
	var parts []string
	for _, field := range []struct {
		on    bool
		value int
	}{{p.lines, t.lines}, {p.words, t.words}, {p.bytes, t.bytes}} {
		if field.on {
			parts = append(parts, fmt.Sprintf("%*d", width, field.value))
		}
	}
	line := strings.Join(parts, " ")
	if name != "" {
		line += " " + name
	}
	return line
}

func (p *wcPlan) run(inv *invocation) int {
	operands := p.operands
	if len(operands) == 0 {
		var t tally
		inv.stdin(func(r record) bool {
			t.add(r.text, true)
			return true
		})
		inv.out.emit(record{text: p.format(t, "", 1)})
		return 0
	}
	status := 0
	var results []tally
	var names []string
	var total tally
	for _, operand := range operands {
		var t tally
		if operand == "-" {
			inv.stdin(func(r record) bool {
				t.add(r.text, true)
				return true
			})
		} else {
			data, err := os.ReadFile(inv.path(operand))
			if err != nil {
				if inv.open(operand, false) == nil {
					status = 1
					continue
				}
			}
			// A file is counted by its bytes, so a missing final newline counts as GNU counts it.
			t.lines = bytes.Count(data, []byte{'\n'})
			t.bytes = len(data)
			t.words = len(bytes.Fields(data))
		}
		results = append(results, t)
		names = append(names, operand)
		total.lines += t.lines
		total.words += t.words
		total.bytes += t.bytes
	}
	if len(results) > 1 {
		results = append(results, total)
		names = append(names, "total")
	}
	width := 1
	if len(results) > 1 || (p.lines && p.words) || (p.lines && p.bytes) || (p.words && p.bytes) {
		width = len(strconv.Itoa(max(total.lines, total.words, total.bytes)))
	}
	for i, t := range results {
		inv.out.emit(record{text: p.format(t, names[i], width)})
	}
	return status
}

// --- grep, egrep, fgrep

type grepPlan struct {
	name                                  string
	dialect                               dialect
	patterns                              []string
	operands                              []string
	number, recursive, invert, ignoreCase bool
	word, line, list, listMissing, count  bool
	only, quiet, silent                   bool
	withName, noName                      bool
	maxCount, before, after               int
	context                               bool
	include, exclude, excludeDir          []string
	matcher                               *matcher
}

var grepSpec = optionSpec{
	flags:  "nrREFGviwxlLchHsqo",
	values: "mABCe",
	long: map[string]string{
		"line-number": "n", "recursive": "r", "dereference-recursive": "R", "extended-regexp": "E", "fixed-strings": "F", "basic-regexp": "G",
		"invert-match": "v", "ignore-case": "i", "word-regexp": "w", "line-regexp": "x", "files-with-matches": "l", "files-without-match": "L",
		"count": "c", "no-filename": "h", "with-filename": "H", "no-messages": "s", "quiet": "q", "silent": "q", "only-matching": "o",
		"max-count=": "m", "after-context=": "A", "before-context=": "B", "context=": "C", "regexp=": "e",
		"include=": "", "exclude=": "", "exclude-dir=": "", "color=": "", "colour=": "",
	},
}

func prepareGrep(name string, args []string) (plan, error) {
	options, operands, err := parseOptions(grepSpec, args)
	if err != nil {
		return nil, err
	}
	p := &grepPlan{name: name, maxCount: -1}
	switch name {
	case "egrep":
		p.dialect = extended
	case "fgrep":
		p.dialect = fixed
	}
	for _, o := range options {
		switch o.name {
		case "n":
			p.number = true
		case "r", "R":
			p.recursive = true
		case "E":
			p.dialect = extended
		case "F":
			p.dialect = fixed
		case "G":
			p.dialect = basic
		case "v":
			p.invert = true
		case "i":
			p.ignoreCase = true
		case "w":
			p.word = true
		case "x":
			p.line = true
		case "l":
			p.list = true
		case "L":
			p.listMissing = true
		case "c":
			p.count = true
		case "h":
			p.noName = true
		case "H":
			p.withName = true
		case "s":
			p.silent = true
		case "q":
			p.quiet = true
		case "o":
			p.only = true
		case "e":
			p.patterns = append(p.patterns, o.value)
		case "m", "A", "B", "C":
			n, ok := count(o.value)
			if !ok {
				return nil, errUnsupported
			}
			switch o.name {
			case "m":
				p.maxCount = n
			case "A":
				p.after = n
			case "B":
				p.before = n
			case "C":
				p.before, p.after = n, n
			}
			if o.name != "m" {
				p.context = true
			}
		case "include":
			p.include = append(p.include, o.value)
		case "exclude":
			p.exclude = append(p.exclude, o.value)
		case "exclude-dir":
			p.excludeDir = append(p.excludeDir, o.value)
		case "color", "colour":
			if o.value != "never" && o.value != "auto" {
				return nil, errUnsupported
			}
		}
	}
	if len(p.patterns) == 0 {
		if len(operands) == 0 {
			return nil, errUnsupported
		}
		p.patterns, operands = strings.Split(operands[0], "\n"), operands[1:]
	}
	p.operands = operands
	if p.matcher, err = newMatcher(p.patterns, p.dialect, p.ignoreCase, p.word, p.line); err != nil {
		if errors.Is(err, errUntranslatable) {
			return nil, err
		}
		return nil, errUnsupported
	}
	if p.recursive {
		if _, err := exec.LookPath("rg"); err != nil {
			return nil, errUnsupported
		}
	}
	return p, nil
}

// grepState is what one grep run has found so far, over all its files.
type grepState struct {
	matched  bool
	status   int
	total    int
	files    int
	counting bool
	deadline time.Time
}

func (p *grepPlan) run(inv *invocation) int {
	state := &grepState{}
	operands := p.operands
	if p.recursive && len(operands) == 0 {
		operands = []string{"."}
	}
	if len(operands) == 0 {
		operands = []string{"-"}
	}
	named := (len(operands) > 1 || p.recursive || p.withName) && !p.noName
	for _, operand := range operands {
		if p.quiet && state.matched {
			break
		}
		if operand == "-" {
			p.grepRecords(inv, state, "(standard input)", named, func(each func(record) bool) { inv.stdin(each) })
			continue
		}
		if p.recursive {
			p.grepTree(inv, state, operand, named)
			continue
		}
		source := inv.open(operand, p.silent)
		if source == nil {
			state.status = 2
			continue
		}
		p.grepSource(inv, state, source, operand, named)
	}
	if inv.outLink != nil {
		inv.outLink.stats.kind = "grep"
		inv.outLink.stats.total = state.total
		inv.outLink.stats.files = state.files
		inv.outLink.stats.partial = state.counting && time.Now().After(state.deadline)
	}
	switch {
	case p.quiet && state.matched:
		return 0
	case state.status != 0 && !(p.quiet && state.matched):
		return state.status
	case state.matched:
		return 0
	}
	return 1
}

// grepTree searches below a directory: ripgrep finds the files that may match, without ignore files as GNU grep -r has none, and each is then
// searched like a named file.
func (p *grepPlan) grepTree(inv *invocation, state *grepState, operand string, named bool) {
	info, err := os.Stat(inv.path(operand))
	if err != nil {
		if !p.silent {
			inv.fail("%s: No such file or directory", operand)
		}
		state.status = 2
		return
	}
	if !info.IsDir() {
		if source := inv.open(operand, p.silent); source != nil {
			p.grepSource(inv, state, source, operand, named)
		}
		return
	}
	args := []string{"--files-with-matches", "--null", "--no-config", "--hidden", "--no-ignore", "--no-messages"}
	if p.ignoreCase {
		args = append(args, "--ignore-case")
	}
	for _, glob := range p.include {
		args = append(args, "--glob", glob)
	}
	for _, glob := range p.exclude {
		args = append(args, "--glob", "!"+glob)
	}
	for _, dir := range p.excludeDir {
		args = append(args, "--glob", "!"+dir)
	}
	// Candidates are found with the translated pattern; -v needs every file, since any line may not match.
	if p.invert || p.listMissing {
		args = []string{"--files", "--null", "--no-config", "--hidden", "--no-ignore", "--no-messages"}
		for _, glob := range p.include {
			args = append(args, "--glob", glob)
		}
		for _, glob := range p.exclude {
			args = append(args, "--glob", "!"+glob)
		}
		for _, dir := range p.excludeDir {
			args = append(args, "--glob", "!"+dir)
		}
	} else {
		args = append(args, "--regexp", p.matcher.re.String())
	}
	cmd := exec.CommandContext(inv.ctx, "rg", append(args, "--", operand)...)
	cmd.Dir = inv.dir
	output, _ := cmd.Output()
	var names []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if p.quiet && state.matched {
			return
		}
		if source := inv.open(name, true); source != nil {
			p.grepSource(inv, state, source, name, named)
		}
	}
}

func binarySource(source *engine.Source, path string) bool {
	if source.View != nil {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	head := make([]byte, 32*1024)
	n, _ := file.Read(head)
	return bytes.IndexByte(head[:n], 0) >= 0
}

func (p *grepPlan) grepSource(inv *invocation, state *grepState, source *engine.Source, name string, named bool) {
	if binarySource(source, inv.path(name)) {
		found := false
		lines(source, func(r record) bool {
			found = p.matcher.match(r.text) != p.invert
			return !found
		})
		if found {
			state.matched = true
			state.total++
			state.files++
			if !p.quiet && !p.count && !p.list {
				inv.out.emit(record{text: fmt.Sprintf("grep: %s: binary file matches", name)})
			} else if p.list {
				inv.out.emit(record{text: name})
			}
		}
		return
	}
	p.grepRecords(inv, state, name, named, func(each func(record) bool) { lines(source, each) })
}

// grepRecords searches one file's records, printing GNU grep's forms: name and number prefixes, -- between groups of context, counts and names.
func (p *grepPlan) grepRecords(inv *invocation, state *grepState, name string, named bool, feed func(func(record) bool)) {
	prefix := func(sep string, line int) string {
		text := ""
		if named {
			text = name + sep
		}
		if p.number {
			text += strconv.Itoa(line) + sep
		}
		return text
	}
	var pending []record // before-context, with their line numbers in text already
	var pendingLines []int
	afterLeft := 0
	lastPrinted := 0
	matches := 0
	line := 0
	stopped := false
	emit := func(r record) {
		if stopped {
			return
		}
		if !inv.out.emit(r) {
			stopped = true
			if inv.outLink != nil && !state.counting {
				// The reader stopped; keep counting a while, without printing, for its fact.
				state.counting = true
				state.deadline = time.Now().Add(countAfter)
			}
		}
	}
	quietDone := false
	feed(func(r record) bool {
		line++
		if state.counting && time.Now().After(state.deadline) {
			return false
		}
		hit := p.matcher.match(r.text) != p.invert
		if !hit {
			if afterLeft > 0 && !p.count && !p.list && !p.listMissing && !p.quiet {
				afterLeft--
				c := r
				c.text = prefix("-", line) + r.text
				c.context = true
				emit(c)
				lastPrinted = line
			} else if p.before > 0 {
				pending = append(pending, r)
				pendingLines = append(pendingLines, line)
				if len(pending) > p.before {
					pending, pendingLines = pending[1:], pendingLines[1:]
				}
			}
			return true
		}
		matches++
		state.total++
		if matches == 1 {
			state.files++
		}
		state.matched = true
		if p.quiet {
			quietDone = true
			return false
		}
		if p.list || p.listMissing {
			return false
		}
		if p.count {
			return p.maxCount < 0 || matches < p.maxCount
		}
		if p.maxCount >= 0 && matches > p.maxCount {
			return false
		}
		if p.context && lastPrinted > 0 && len(pendingLines) > 0 && pendingLines[0] > lastPrinted+1 || p.context && lastPrinted > 0 && len(pendingLines) == 0 && line > lastPrinted+1 {
			emit(record{text: "--", separator: true})
		}
		for i, c := range pending {
			c.text = prefix("-", pendingLines[i]) + c.text
			c.context = true
			emit(c)
		}
		pending, pendingLines = nil, nil
		if p.only {
			for _, span := range p.matcher.matches(r.text) {
				emit(record{text: prefix(":", line) + r.text[span[0]:span[1]]})
			}
		} else {
			m := r
			m.text = prefix(":", line) + r.text
			m.match = p.context
			if !p.context && m.origin != nil {
				m.match = true
			}
			emit(m)
		}
		lastPrinted = line
		afterLeft = p.after
		return p.maxCount < 0 || matches < p.maxCount || afterLeft > 0
	})
	_ = quietDone
	switch {
	case p.count:
		text := strconv.Itoa(matches)
		if named {
			text = name + ":" + text
		}
		inv.out.emit(record{text: text})
	case p.list && matches > 0:
		inv.out.emit(record{text: name})
	case p.listMissing && matches == 0:
		inv.out.emit(record{text: name})
	}
}

// --- rg

type rgPlan struct {
	args     []string
	number   bool
	list     bool
	count    bool
	context  bool
	operands []string
	pattern  string
}

var rgSpec = optionSpec{
	flags:  "niSFwlcvuN",
	values: "gtTABCme",
	long: map[string]string{
		"line-number": "n", "ignore-case": "i", "smart-case": "S", "fixed-strings": "F", "word-regexp": "w", "files-with-matches": "l",
		"count": "c", "invert-match": "v", "hidden": "", "no-ignore": "", "no-line-number": "N", "glob=": "g", "type=": "t", "type-not=": "T",
		"after-context=": "A", "before-context=": "B", "context=": "C", "max-count=": "m", "regexp=": "e", "color=": "", "colour=": "",
	},
}

func prepareRg(name string, args []string) (plan, error) {
	options, operands, err := parseOptions(rgSpec, args)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("rg"); err != nil {
		return nil, errUnsupported
	}
	p := &rgPlan{}
	patterns := 0
	for _, o := range options {
		switch o.name {
		case "n":
			p.number = true
		case "N":
			p.number = false
		case "l":
			p.list = true
		case "c":
			p.count = true
		case "A", "B", "C":
			p.context = true
		case "e":
			patterns++
		case "color", "colour":
			if o.value != "never" && o.value != "auto" {
				return nil, errUnsupported
			}
			continue
		}
		switch {
		case len(o.name) == 1 && strings.Contains(rgSpec.values, o.name):
			p.args = append(p.args, "-"+o.name, o.value)
		case len(o.name) == 1:
			p.args = append(p.args, "-"+o.name)
		default:
			p.args = append(p.args, "--"+o.name)
		}
	}
	if patterns == 0 {
		if len(operands) == 0 {
			return nil, errUnsupported
		}
		p.pattern, operands = operands[0], operands[1:]
	}
	p.operands = operands
	return p, nil
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		LineNumber int `json:"line_number"`
		Lines      struct {
			Text string `json:"text"`
		} `json:"lines"`
		Stats struct {
			MatchedLines int `json:"matched_lines"`
		} `json:"stats"`
	} `json:"data"`
}

// run runs ripgrep itself, so every match and every file it searches is ripgrep's own decision; its rows then take their origin from nib's
// copy of each file.
func (p *rgPlan) run(inv *invocation) int {
	args := append([]string{"--json", "--no-config"}, p.args...)
	if p.pattern != "" {
		args = append(args, "--regexp", p.pattern)
	}
	args = append(append(args, "--"), p.operands...)
	cmd := exec.CommandContext(inv.ctx, "rg", args...)
	cmd.Dir = inv.dir
	cmd.Stderr = inv.stderr
	if inv.in.reader != nil && len(p.operands) == 0 {
		cmd.Stdin = inv.in.file
	} else if inv.in.link != nil && len(p.operands) == 0 {
		// Records from another nib command reach ripgrep as their text.
		var buffer bytes.Buffer
		for {
			r, ok := inv.in.next()
			if !ok {
				break
			}
			buffer.WriteString(r.text + "\n")
		}
		cmd.Stdin = &buffer
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		inv.fail("%v", err)
		return 2
	}
	if err := cmd.Start(); err != nil {
		inv.fail("%v", err)
		return 2
	}
	sources := map[string]*engine.Source{}
	source := func(path string) *engine.Source {
		if s, ok := sources[path]; ok {
			return s
		}
		s := inv.open(path, true)
		sources[path] = s
		return s
	}
	paths := map[string]bool{}
	var order []string
	counts := map[string]int{}
	total, files := 0, 0
	stopped := false
	lastLine := map[string]int{}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	multiple := len(p.operands) != 1
	if len(p.operands) == 1 {
		if info, err := os.Stat(inv.path(p.operands[0])); err == nil && info.IsDir() {
			multiple = true
		}
	}
	for scanner.Scan() {
		var event rgEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Type != "match" && event.Type != "context" {
			continue
		}
		path := event.Data.Path.Text
		if !paths[path] {
			paths[path] = true
			order = append(order, path)
		}
		if event.Type == "match" {
			total++
			counts[path]++
			if counts[path] == 1 {
				files++
			}
		}
		if stopped || p.list || p.count {
			continue
		}
		text := strings.TrimSuffix(strings.TrimSuffix(event.Data.Lines.Text, "\n"), "\r")
		sep := ":"
		if event.Type == "context" {
			sep = "-"
		}
		prefix := ""
		if multiple && path != "" {
			prefix = path + sep
		}
		if p.number {
			prefix += strconv.Itoa(event.Data.LineNumber) + sep
		}
		if p.context && lastLine[path] > 0 && event.Data.LineNumber > lastLine[path]+1 {
			inv.out.emit(record{text: "--", separator: true})
		}
		lastLine[path] = event.Data.LineNumber
		r := record{text: prefix + text, match: event.Type == "match", context: event.Type == "context"}
		if path != "" && !strings.Contains(text, "\n") {
			if s := source(path); s != nil && s.View != nil {
				r.origin = &origin{source: s, line: event.Data.LineNumber}
			}
		}
		if !inv.out.emit(r) {
			stopped = true
		}
	}
	err = cmd.Wait()
	switch {
	case p.list:
		for _, path := range order {
			inv.out.emit(record{text: path})
		}
	case p.count:
		for _, path := range order {
			text := strconv.Itoa(counts[path])
			if multiple {
				text = path + ":" + text
			}
			inv.out.emit(record{text: text})
		}
	}
	if inv.outLink != nil {
		inv.outLink.stats = linkStats{kind: "grep", total: total, files: files}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if total == 0 {
		return 1
	}
	return 0
}
