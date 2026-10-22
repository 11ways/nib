package nibash

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/gitform"
	"mvdan.cc/sh/v3/syntax"
)

// errForeign means a command nib's own never handles in that form, such as a git subcommand whose output it does not reshape; the system
// program runs with no note, since nothing was expected of nib's.
var errForeign = errors.New("form nibash does not handle")

// gitPlan runs the real git with the command line's own arguments and reshapes what it prints for the model: path listings with each folder
// once, a diff's lines as rows of the files they belong to, and a commit header in git's default format as one line. Exit status, standard
// error and side effects are git's.
type gitPlan struct {
	args []string
	// dirs are the -C directories in order, which git joins to the working directory.
	dirs []string
	// globals are the options before the subcommand, which a helper run of git gets too.
	globals    []string
	subcommand string
	// patch is whether the output can hold a diff; relative and noPrefix say how its paths read.
	patch, relative, noPrefix bool
	// at is where the subcommand sits in args; wide asks a --stat without a width for one wide enough to print every path whole.
	at   int
	wide bool
	// forms are the kinds of path line the command line asks for, the only lines read as paths.
	forms []*regexp.Regexp
	// diffs is whether the output holds a diff's lines; headers is whether its commits print in git's default format.
	diffs, headers bool
	// columns runs git with COLUMNS wide enough that a diffstat it prints on its own, as merge and pull do, holds every path whole and bars
	// that draw every changed line; tidy hides what git prints for a person at a terminal (see gitTidier).
	columns, tidy bool
	// target is what a merge or pull joins HEAD with, whose divergence from HEAD a failed fast-forward is told with; "" for none.
	target string
}

// gitOptions are the options of a subcommand nib's git handles: names that take no value, names whose value follows "=", and names whose value
// is the next word. A name ending in "*" takes any attached rest, as -U3 or -n5 do. An open subcommand takes any option, since none changes
// the lines nib reads into a form it misreads, except its foreign ones, with which the system git runs.
type gitOptions struct {
	flags, attached, separate []string
	open                      bool
	foreign                   []string
}

var gitSubcommands = map[string]gitOptions{
	"status": {
		flags:    []string{"-s", "--short", "-b", "--branch", "--porcelain", "--long", "-u", "-u*", "--untracked-files", "--ignored", "--no-renames", "--renames", "--ahead-behind", "--no-ahead-behind", "--show-stash"},
		attached: []string{"--porcelain=v1", "--untracked-files=", "--ignored="},
	},
	"add": {flags: []string{"-v", "--verbose", "-A", "--all", "-u", "--update", "-n", "--dry-run", "-f", "--force", "-N", "--intent-to-add", "--no-all", "--ignore-removal", "--ignore-errors"}},
	"rm":  {flags: []string{"-r", "-f", "--force", "--cached", "-n", "--dry-run", "-q", "--quiet", "--ignore-unmatch"}},
	"ls-files": {
		flags:    []string{"-c", "--cached", "-m", "--modified", "-o", "--others", "-d", "--deleted", "-i", "--ignored", "--exclude-standard", "--full-name", "--directory", "--no-empty-directory", "--error-unmatch"},
		attached: []string{"--exclude=", "--exclude-from=", "--exclude-per-directory="},
	},
	"diff": diffOptions,
	"show": diffOptions,
	"log":  diffOptions,
	// The subcommands that change a repository print what they changed: a summary of created and deleted files, a diffstat, conflicts, refs.
	"commit":      {open: true, foreign: []string{"-p", "--patch", "--interactive"}},
	"merge":       {open: true},
	"pull":        {open: true},
	"rebase":      {open: true},
	"cherry-pick": {open: true},
	"revert":      {open: true},
	"push":        {open: true, foreign: []string{"--porcelain"}},
	"fetch":       {open: true, foreign: []string{"--porcelain"}},
	"checkout":    {open: true, foreign: []string{"-p", "--patch"}},
	"switch":      {open: true},
	"reset":       {open: true, foreign: []string{"-p", "--patch"}},
	"stash":       {open: true, foreign: []string{"-p", "--patch"}},
}

// mergeValues are merge's options whose value is the next word, so that word is not taken for what it merges.
var mergeValues = []string{"-m", "-F", "-s", "-X", "--file", "--strategy", "--strategy-option", "--into-name"}

// The forms of the lines naming a path that the subcommands changing a repository print: a commit's summary of created and deleted files and
// changed modes, a conflict, a file merged on both sides; status lines, as a commit with nothing to commit prints; checkout's and reset's
// changed files.
var (
	changeForms = []*regexp.Regexp{gitSummary, gitConflict, gitMerging, gitStat}
	statusForms = []*regexp.Regexp{gitShort, gitLong, gitTabbed}
)

// diffOptions are what diff, show and log share here: none changes the shape of the lines nib reads, or it handles the change (--relative,
// --no-prefix), or the lines it cannot read print as git printed them.
var diffOptions = gitOptions{
	flags: []string{"--stat", "--name-only", "--name-status", "--numstat", "--shortstat", "--summary", "-p", "-u", "--patch", "--cached",
		"--staged", "--no-renames", "-M", "-M*", "--find-renames", "-w", "--ignore-all-space", "-b", "--ignore-space-change", "--ignore-blank-lines",
		"--minimal", "--patience", "--histogram", "--no-color", "--no-ext-diff", "--relative", "--no-prefix", "--oneline", "--abbrev-commit",
		"--no-merges", "--merges", "--first-parent", "--reverse", "--all", "--graph", "--decorate", "--no-decorate", "--full-diff", "--follow",
		"-R", "--text", "-a", "-U*", "-n*", "-[0-9]*"},
	attached: []string{"--stat=", "--unified=", "--format=", "--pretty=", "--date=", "--since=", "--until=", "--after=", "--before=", "--author=",
		"--committer=", "--grep=", "--max-count=", "--find-renames=", "--diff-filter=", "--color=never", "--diff-algorithm=", "--abbrev="},
	separate: []string{"-n"},
}

func prepareGit(_ string, args []string) (plan, error) {
	p := &gitPlan{args: args}
	i := 0
	for ; i < len(args); i++ {
		switch a := args[i]; a {
		case "-C", "-c":
			if i+1 >= len(args) {
				return nil, errForeign
			}
			if a == "-C" {
				p.dirs = append(p.dirs, args[i+1])
			}
			p.globals = append(p.globals, a, args[i+1])
			i++
		case "--no-pager", "-P", "--no-optional-locks":
			p.globals = append(p.globals, a)
		default:
			if strings.HasPrefix(a, "-") || a == expansion {
				return nil, errForeign
			}
			goto subcommand
		}
	}
	return nil, errForeign
subcommand:
	p.subcommand, p.at = args[i], i
	spec, ok := gitSubcommands[p.subcommand]
	if !ok {
		return nil, errForeign
	}
	verbose, shapes, sized, formatted := false, false, false, false
	var operands []string
	rest := args[i+1:]
	for k := 0; k < len(rest); k++ {
		a := rest[k]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			// A show of REV:PATH prints a file's content, not a diff.
			if p.subcommand == "show" && strings.Contains(a, ":") {
				return nil, errForeign
			}
			operands = append(operands, a)
			continue
		}
		if spec.open {
			name, _, _ := strings.Cut(a, "=")
			if slices.Contains(spec.foreign, name) {
				return nil, errForeign
			}
			if p.subcommand == "merge" && slices.Contains(mergeValues, a) {
				k++
			}
			continue
		}
		name, ok := spec.match(a)
		if !ok && p.subcommand == "rm" && !strings.HasPrefix(a, "--") {
			// rm's short options bundle, as -rf.
			ok = strings.Trim(a[1:], "rfnq") == ""
			name = a
		}
		if !ok {
			return nil, errUnsupported
		}
		if spec.takesNext(name) {
			if k+1 >= len(rest) {
				return nil, errUnsupported
			}
			k++
		}
		switch name {
		case "-v", "--verbose", "-n", "--dry-run":
			verbose = true
		case "--relative":
			p.relative = true
		case "--no-prefix":
			p.noPrefix = true
		case "--stat":
			shapes, p.wide = true, true
			p.forms = append(p.forms, gitStat)
		case "--name-only":
			shapes = true
			p.forms = append(p.forms, gitBarePath)
		case "--name-status":
			shapes = true
			p.forms = append(p.forms, gitStatus)
		case "--numstat":
			shapes = true
			p.forms = append(p.forms, gitNumstat)
		case "-p", "-u", "--patch":
			shapes, p.diffs = true, true
		case "--oneline", "--graph":
			formatted = true
		}
		if strings.HasPrefix(name, "--format=") || strings.HasPrefix(name, "--pretty=") || strings.HasPrefix(name, "--date=") {
			formatted = true
		}
		if strings.HasPrefix(name, "-U") || strings.HasPrefix(name, "--unified=") {
			shapes = true
		}
		if strings.HasPrefix(name, "--stat=") {
			shapes, sized = true, true
			p.forms = append(p.forms, gitStat)
		}
	}
	p.wide = p.wide && !sized
	switch p.subcommand {
	case "status":
		p.forms = statusForms
	case "rm":
		p.forms = []*regexp.Regexp{gitVerbose}
	case "ls-files":
		p.forms = []*regexp.Regexp{gitBarePath}
	case "add":
		p.forms = []*regexp.Regexp{gitVerbose}
		// Without -v or -n add prints nothing to reshape.
		if !verbose {
			return nil, errForeign
		}
	case "log":
		p.headers = !formatted
		// A log in another format and without a diff or a file list is commits only, as git prints them.
		if !shapes && !p.headers {
			return nil, errForeign
		}
		p.patch = true
	case "diff", "show":
		p.patch = true
		p.headers = p.subcommand == "show" && !formatted
		// Without a list of files they print the diff itself.
		p.diffs = p.diffs || len(p.forms) == 0
	case "commit", "stash":
		p.forms, p.columns, p.tidy = append(slices.Clone(changeForms), statusForms...), true, true
	case "checkout", "switch", "reset":
		p.forms, p.tidy = []*regexp.Regexp{gitStatus}, true
	case "merge", "pull", "rebase", "cherry-pick", "revert", "push", "fetch":
		p.forms, p.columns, p.tidy = changeForms, true, true
		switch {
		case p.subcommand == "pull" && len(operands) == 0:
			p.target = "@{upstream}"
		case p.subcommand == "pull":
			p.target = "FETCH_HEAD"
		case p.subcommand == "merge" && len(operands) == 1:
			p.target = operands[0]
		}
	}
	return p, nil
}

// match is the option name a matches, as listed: a flag, an attached value's "name=" or a name with "*".
func (o gitOptions) match(a string) (string, bool) {
	for _, f := range o.flags {
		if f == a {
			return f, true
		}
		if stem, ok := strings.CutSuffix(f, "*"); ok {
			if stem == "-[0-9]" {
				if len(a) > 1 && strings.Trim(a[1:], "0123456789") == "" {
					return f, true
				}
				continue
			}
			if strings.HasPrefix(a, stem) && len(a) > len(stem) {
				return a, true
			}
		}
	}
	for _, f := range o.attached {
		if strings.HasSuffix(f, "=") && strings.HasPrefix(a, f) || a == f {
			return a, true
		}
	}
	for _, f := range o.separate {
		if a == f {
			return f, true
		}
	}
	return "", false
}

func (o gitOptions) takesNext(name string) bool {
	for _, f := range o.separate {
		if f == name {
			return true
		}
	}
	return false
}

func (p *gitPlan) run(inv *invocation) int {
	if inv.out.sink == nil && !inv.out.model {
		// Into another nib command, or a pipe nib commands read, go git's own lines, as they come, those that name a path marked so the
		// terminal can lay them out where they reach it; a diff's rows would not be the lines that command must read, so its lines are left
		// unmarked.
		parse := gitParser(p.forms)
		if p.diffs {
			parse = nil
		}
		return streamed(inv, append([]string{"git"}, p.args...), func(line string) record { return listingRecord(line, parse) })
	}
	shaper := newGitShaper(inv, p)
	if inv.out.sink == nil || !p.wide {
		// git streams, so what it printed shows when the call returns while it runs, and nib's own head and tail still stop it early. Each
		// line it prints is one record, so they keep the lines they would of the system git's. A wide stat prints the same lines, one a file
		// in the same order, and only it lays out a stat whose paths git cut, so through them it is the stat that runs.
		if p.columns {
			inv.env = []string{"COLUMNS=" + gitform.StatWidth}
			defer func() { inv.env = nil }()
		}
		stderr := inv.stderr
		var tidied *lineWriter
		if shaper.tidier != nil && reachesModel(stderr) {
			// What git writes to its standard error, as push and fetch write all they print, is tidied the same way where the model reads it.
			tidied = &lineWriter{each: func(line string) bool {
				if shown, keep := shaper.tidier.line(line); keep {
					io.WriteString(stderr, shown+"\n")
				}
				return true
			}}
			inv.stderr = tidied
		}
		w := &lineWriter{each: shaper.line}
		status := runInto(inv, p.command(p.wide), w, false)
		w.flush()
		if tidied != nil {
			tidied.flush()
			inv.stderr = stderr
		}
		shaper.flush()
		if status != 0 && p.target != "" && shaper.tidier.diverged {
			if note := p.divergence(inv); note != "" {
				shaper.emit(record{text: note})
			}
		}
		return status
	}
	// git cuts a path to fit 80 columns, which leaves it out of its folder; asked for a width that prints every path whole, with bars that
	// draw every changed line, it prints the form that is shorter laid out. A stat only reads, so running it twice changes nothing.
	lines, status := captured(inv, p.command(false), false)
	if wide, wideStatus := captured(inv, p.command(true), true); wideStatus == status && p.shown(wide) < p.shown(lines) {
		lines = wide
	}
	for _, line := range lines {
		if !shaper.line(line) {
			break
		}
	}
	shaper.flush()
	return status
}

// command is the git that runs for output the model gets: the command line's own, with --abbrev-commit where nib prints commit headers as
// one line, with a --stat wide enough for every path whole and every changed line its own bar character, when wide, and with bars that wide
// in a diffstat git prints on its own, with columns.
func (p *gitPlan) command(wide bool) []string {
	var extra []string
	if p.headers {
		extra = append(extra, "--abbrev-commit")
	}
	if wide {
		extra = append(extra, gitform.StatArgs...)
	}
	globals := p.args[:p.at]
	if p.columns {
		globals = append(slices.Clone(globals), "-c", "diff.statGraphWidth="+gitform.StatWidth)
	}
	return append(append(append(append([]string{"git"}, globals...), p.args[p.at]), extra...), p.args[p.at+1:]...)
}

// divergence tells how HEAD and target diverged, for a merge or pull that could not fast-forward; "" when git cannot say.
func (p *gitPlan) divergence(inv *invocation) string {
	var out bytes.Buffer
	args := append(append([]string{"git"}, p.globals...), "rev-list", "--left-right", "--count", "HEAD..."+p.target)
	if inv.call.runProgramTo(inv.ctx, args, &out, io.Discard, nil) != nil {
		return ""
	}
	fields := strings.Fields(out.String())
	if len(fields) != 2 {
		return ""
	}
	return fmt.Sprintf("[nibash: HEAD and %s have diverged: %s only on HEAD, %s only on %[1]s]", p.target, commits(fields[0]), commits(fields[1]))
}

// commits is a count of commits in words.
func commits(count string) string {
	if count == "1" {
		return "1 commit"
	}
	return count + " commits"
}

// reachesModel is whether what a command writes to w reaches the model as it is, through the terminal sink, so tidying it hides nothing a file
// or a program would have read.
func reachesModel(w io.Writer) bool {
	switch w := w.(type) {
	case *sink:
		return true
	case *headedWriter:
		return reachesModel(w.w)
	}
	return false
}

// shown is about how many characters lines print as: the lines outside the diff's files laid out by folder, a wide stat's rows with their
// counts, and the rest as they are.
func (p *gitPlan) shown(lines []string) int {
	parse := gitParser(p.forms)
	counted := map[string]listed{}
	total := 0
	var plain []string
	for i := 0; i < len(lines); {
		if !gitDiff.MatchString(lines[i]) {
			line := lines[i]
			if shown, l, ok := p.statCounts(line); ok {
				line = shown
				if plainPath(l.path) {
					counted[shown] = l
				}
			}
			plain = append(plain, line)
			i++
			continue
		}
		end := sectionEnd(lines, i)
		total += textSize(lines[i:end])
		i = end
	}
	return total + textSize(compactListing(plain, func(line string) (listed, bool) {
		if l, ok := counted[line]; ok {
			return l, true
		}
		return parse(line)
	}))
}

// statCounts is gitform.StatCounts for a wide stat or a diffstat git printed with columns, the only ones whose bars nib asked to draw every
// changed line, with the row's path as the listing reads it.
func (p *gitPlan) statCounts(line string) (string, listed, bool) {
	if !p.wide && !p.columns {
		return "", listed{}, false
	}
	shown, path, ok := gitform.StatCounts(line)
	if !ok {
		return "", listed{}, false
	}
	return shown, listed{before: " ", path: path, after: shown[1+len(path):]}, true
}

func textSize(lines []string) int {
	n := 0
	for _, line := range lines {
		n += len(line) + 1
	}
	return n
}

// splitLines splits a program's output into lines without their newline; a carriage return before it stays, for a diff to compare.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// The forms of a git line that names one path, each split into the text before the path, the path, and the text after it.
var (
	gitShort    = regexp.MustCompile(`^([ MADRCUT?!][ MADRCUT?!] )(.+)()$`)
	gitLong     = regexp.MustCompile(`^(\t(?:modified|new file|deleted|renamed|copied|typechange|unmerged|both modified|both added|both deleted|added by us|added by them|deleted by us|deleted by them): +)(.+)()$`)
	gitTabbed   = regexp.MustCompile(`^(\t)([^ \t]+)()$`)
	gitVerbose  = regexp.MustCompile(`^((?:add|remove|rm) ')(.+)(')$`)
	gitStat     = regexp.MustCompile(`^( )(\S(?:.*\S)?) +(\| +(?:\d+(?: [+-]+)?|Bin .*))$`)
	gitStatus   = regexp.MustCompile(`^([ACDMTUXB]\t)(.+)()$`)
	gitNumstat  = regexp.MustCompile(`^((?:\d+|-)\t(?:\d+|-)\t)(.+)()$`)
	gitBarePath = regexp.MustCompile(`^()([^ \t]*[/.][^ \t]*)()$`)
	gitSummary  = regexp.MustCompile(`^( (?:create|delete) mode \d{6} | mode change \d{6} => \d{6} )(.+)()$`)
	gitConflict = regexp.MustCompile(`^(CONFLICT \([^)]+\): Merge conflict in )(.+)()$`)
	gitMerging  = regexp.MustCompile(`^(Auto-merging )(.+)()$`)
)

// gitParser reads the path listing lines of the forms a command line asked git for; other lines, in a commit's message or a file's content,
// never count as paths.
func gitParser(forms []*regexp.Regexp) func(string) (listed, bool) {
	return func(line string) (listed, bool) {
		for _, form := range forms {
			m := form.FindStringSubmatch(line)
			if m == nil || !plainPath(m[2]) {
				continue
			}
			before, after := m[1], m[3]
			if form == gitSummary {
				// A file created or deleted with the usual mode says only that.
				before = strings.Replace(before, " mode 100644 ", " ", 1)
			}
			if form == gitStat {
				// The padding that lines up the bars would no longer line anything up.
				after = " " + after
			}
			return listed{before: before, path: m[2], after: after}, true
		}
		return listed{}, false
	}
}

// plainPath is whether git printed a path as it is: not quoted, not a rename's two paths, not cut short, nothing but printable text.
func plainPath(path string) bool {
	if path == "" || strings.HasPrefix(path, `"`) || strings.HasPrefix(path, "...") || strings.HasPrefix(path, " ") || strings.HasSuffix(path, " ") {
		return false
	}
	if strings.Contains(path, " -> ") || strings.Contains(path, " => ") || strings.ContainsAny(path, "{}\t") {
		return false
	}
	for _, r := range path {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// gitDiff matches the header that starts one file's part of a diff.
var gitDiff = regexp.MustCompile(`^diff --git `)

// gitHunk matches a hunk's header: the old side's start and length, the new side's start and length (a missing length is 1).
var gitHunk = regexp.MustCompile(`^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// gitExtended are the lines git puts between a file's "diff --git" and its first hunk.
var gitExtended = []string{"old mode ", "new mode ", "deleted file mode ", "new file mode ", "copy from ", "copy to ", "rename from ", "rename to ",
	"similarity index ", "dissimilarity index ", "index ", "--- ", "+++ ", "Binary files "}

// sectionEnd is where one file's part of a diff that starts at start ends: after its extended header lines and the hunks their headers count,
// so a log's next commit, whatever its format, is never taken for part of the file.
func sectionEnd(lines []string, start int) int {
	i := start + 1
	for i < len(lines) && slices.ContainsFunc(gitExtended, func(prefix string) bool { return strings.HasPrefix(lines[i], prefix) }) {
		i++
	}
	for i < len(lines) {
		m := gitHunk.FindStringSubmatch(strings.TrimSuffix(lines[i], "\r"))
		if m == nil {
			break
		}
		old, added := hunkLength(m[1]), hunkLength(m[3])
		i++
		for i < len(lines) && (old > 0 || added > 0 || strings.HasPrefix(lines[i], `\`)) {
			switch line := lines[i]; {
			case strings.HasPrefix(line, `\`):
			case strings.HasPrefix(line, "-"):
				old--
			case strings.HasPrefix(line, "+"):
				added--
			case strings.HasPrefix(line, " "), line == "", line == "\r":
				old--
				added--
			default:
				return i
			}
			i++
		}
	}
	return i
}

func hunkLength(text string) int {
	if text == "" {
		return 1
	}
	n, _ := strconv.Atoi(text)
	return n
}

// base is the directory a diff's paths are relative to: the working directory with --relative, else the top of the repository; "" when git
// cannot say.
func (p *gitPlan) base(inv *invocation) string {
	dir := inv.dir
	for _, d := range p.dirs {
		dir = absolute(dir, d)
	}
	if p.relative {
		return dir
	}
	var out bytes.Buffer
	args := append(append([]string{"git"}, p.globals...), "rev-parse", "--show-toplevel")
	if inv.call.runProgramTo(inv.ctx, args, &out, io.Discard, nil) != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// sectionSource is the file one file's part of a diff is about, opened as it is now; nil when that part has no new side nib can name and hold
// as one version, as for a deleted file or a binary one.
func (p *gitPlan) sectionSource(inv *invocation, base string, section []string) *engine.Source {
	if base == "" {
		return nil
	}
	for _, line := range section {
		path, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), "+++ ")
		if !ok {
			continue
		}
		if path == "/dev/null" || !p.noPrefix && !stripPrefix(&path) || !plainPath(path) {
			return nil
		}
		source := inv.open(filepath.Join(base, path), true)
		if source == nil || source.View == nil {
			return nil
		}
		return source
	}
	return nil
}

// stripPrefix removes the side prefix git puts before a new side's path: b/ by default, or a mnemonic one (w/, i/, c/, o/).
func stripPrefix(path *string) bool {
	for _, prefix := range []string{"b/", "w/", "i/", "c/", "o/"} {
		if rest, ok := strings.CutPrefix(*path, prefix); ok {
			*path = rest
			return true
		}
	}
	return false
}

// revisionLesson points an agent that saves git's diff of a commit to a file at reading the commit itself, once per agent.
const revisionLesson = "[read and skim take PATH@REV: a repository's directory for that commit with its diff, a file for it as it was then]"

// savesGitPatch is whether a statement of file writes a git diff, show or format-patch to a file, to read it back.
func savesGitPatch(file *syntax.File) bool {
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok || found {
			return !found
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || !slices.ContainsFunc(stmt.Redirs, func(r *syntax.Redirect) bool {
			return (r.Op == syntax.RdrOut || r.Op == syntax.AppOut) && (r.N == nil || r.N.Value == "1") || r.Op == syntax.RdrAll || r.Op == syntax.AppAll
		}) {
			return true
		}
		var words []string
		for _, w := range call.Args {
			words = append(words, w.Lit())
		}
		if len(words) == 0 || words[0] != "git" {
			return true
		}
		for i := 1; i < len(words); i++ {
			switch words[i] {
			case "-C", "-c":
				i++
			case "diff", "show", "format-patch":
				found = true
				return false
			default:
				if !strings.HasPrefix(words[i], "-") {
					return true
				}
			}
		}
		return true
	})
	return found
}
