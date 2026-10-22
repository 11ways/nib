package nibash

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/gitform"
)

// gitPart is a commit's header or one file's part of a diff in git's output that nib reshapes for the model. Its lines print reshaped only
// as far as the part reached the terminal, through what head or tail kept: a commit header only whole, a diff's own header lines only
// beside a row under the file's header.
type gitPart struct {
	// header is a commit header's one line, standing for the lines git printed it in, which lines counts; "" for a diff's part.
	header string
	lines  int
	// own is whether header leaves the author out, as the repository's own user.email.
	own bool
}

// gitRole is what a line of git's is to its part.
type gitRole int

const (
	// commitLine is a header's "commit" line, which prints the header's one line.
	commitLine gitRole = iota
	// commitMerge is a merge's "Merge:" line, which prints as git printed it.
	commitMerge
	// commitFolded is a header's "Author:" or "Date:" line or the blank line after them, which the header's one line stands for.
	commitFolded
	// commitMessage is a line of the message, which loses git's indent under the header's one line.
	commitMessage
	// diffHeader is a file part's "diff --git", "index", "---" or "+++" line, which the file's [PATH#TAG] header stands for once a line
	// under it prints.
	diffHeader
	// diffRow is a line that prints under the file's header: a row of the file, or shown.
	diffRow
	// shownRow is a line that prints as shown, as a --stat file row with its bar given as counts or a tidied line; it belongs to no part.
	shownRow
	// hiddenRow is a line git prints for a person at a terminal, which the model does not see; the log keeps it.
	hiddenRow
)

// shaped is how a line of git's prints at the terminal: by its role in its part, and as shown when it is a line under a file's header that
// is no row of the file.
type shaped struct {
	part  *gitPart
	role  gitRole
	shown string
}

// counts says whether the line tells how far its part reached the terminal: a commit header's own lines, or a line under a file's header.
func (s *shaped) counts() bool {
	return s.role <= commitFolded || s.role == diffRow
}

// view is the line's text at the terminal, given how many counted lines of each part reached it; ok is false for a line that a commit
// header's one line or a file's header stands for.
func (s *shaped) view(text string, reached map[*gitPart]int) (string, bool) {
	switch s.role {
	case shownRow:
		return s.shown, true
	case hiddenRow:
		return "", false
	}
	whole := s.part.lines > 0 && reached[s.part] == s.part.lines
	switch s.role {
	case commitLine:
		if whole {
			return s.part.header, true
		}
	case commitFolded:
		if whole {
			return "", false
		}
	case commitMessage:
		if whole {
			return gitform.Message(text), true
		}
	case diffHeader:
		if reached[s.part] > 0 {
			return "", false
		}
	case diffRow:
		if s.shown != "" {
			return s.shown, true
		}
	}
	return text, true
}

type shaperState int

const (
	betweenParts shaperState = iota
	inCommitHeader
	inMessage
	inFileHeader
	inHunks
)

// gitShaper turns git's lines, as they come, into the records the terminal reshapes for the model: a commit header in git's default format
// as one line, a diff's lines as rows of the files they belong to, and path lists marked for their folders. Each line git prints is one
// record whose text is that line, so nib's own head and tail between git and the terminal keep exactly the lines they would of the system
// git's; how each prints the terminal tells from what reached it of its part.
type gitShaper struct {
	inv   *invocation
	plan  *gitPlan
	parse func(string) (listed, bool)
	state shaperState
	// held are the lines of a commit header or of a file part's own header, kept until it is known how they print.
	held   []string
	part   *gitPart
	source *engine.Source
	base   string
	based  bool
	// hunk is whether the file's part had a hunk header; next is the new side's line the hunk's next line is, and old and added count
	// the lines left of each side.
	hunk             bool
	next, old, added int
	email            *string
	stopped          bool
	// tidier tidies the lines of a subcommand that changes a repository; nil for the others.
	tidier *gitTidier
}

func newGitShaper(inv *invocation, p *gitPlan) *gitShaper {
	s := &gitShaper{inv: inv, plan: p, parse: gitParser(p.forms)}
	if p.tidy {
		s.tidier = &gitTidier{seen: map[string]bool{}}
	}
	return s
}

// line takes git's next line; false means nothing reads any more.
func (s *gitShaper) line(text string) bool {
	if s.stopped {
		return false
	}
	switch s.state {
	case inCommitHeader:
		return s.headerLine(text)
	case inMessage:
		if strings.HasPrefix(text, "    ") {
			return s.emit(record{text: text, shape: &shaped{part: s.part, role: commitMessage}})
		}
	case inFileHeader:
		if slices.ContainsFunc(gitExtended, func(prefix string) bool { return strings.HasPrefix(text, prefix) }) {
			s.held = append(s.held, text)
			return true
		}
		if !s.openFile() {
			return false
		}
		return s.hunkLine(text)
	case inHunks:
		return s.hunkLine(text)
	}
	return s.between(text)
}

// flush prints what is still held once git printed its last line.
func (s *gitShaper) flush() {
	switch s.state {
	case inCommitHeader:
		s.release()
	case inFileHeader:
		s.openFile()
	}
}

func (s *gitShaper) emit(r record) bool {
	if s.stopped || !s.inv.out.emit(r) {
		s.stopped = true
		return false
	}
	return true
}

// between takes a line outside any part: the start of a commit header or of a file's part, or another line, marked when it names a path.
func (s *gitShaper) between(text string) bool {
	s.state, s.part, s.source, s.hunk = betweenParts, nil, nil, false
	switch {
	case s.plan.patch && gitDiff.MatchString(text):
		s.held, s.state = []string{text}, inFileHeader
		return true
	case s.plan.headers && gitform.Commit.MatchString(text):
		s.held, s.state = []string{text}, inCommitHeader
		return true
	}
	if shown, l, ok := s.plan.statCounts(text); ok {
		r := record{text: text, shape: &shaped{role: shownRow, shown: shown}}
		if plainPath(l.path) {
			r.listed = &listedLine{text: shown, listed: l}
		}
		return s.emit(r)
	}
	if s.tidier != nil {
		return s.tidied(text)
	}
	return s.emit(listingRecord(text, s.parse))
}

// tidied takes a line of a subcommand that changes a repository: hidden, or as the tidier shows it, marked when it names a path.
func (s *gitShaper) tidied(text string) bool {
	shown, keep := s.tidier.line(text)
	if !keep {
		return s.emit(record{text: text, shape: &shaped{role: hiddenRow}})
	}
	r := listingRecord(shown, s.parse)
	r.text = text
	if r.listed != nil {
		shown = r.listed.before + r.listed.path + r.listed.after
		r.listed.text = shown
	}
	if shown != text {
		r.shape = &shaped{role: shownRow, shown: shown}
	}
	return s.emit(r)
}

// gitTidier hides what a subcommand changing a repository prints for a person at a terminal: hints, most of which GIT_ADVICE=0 already keeps
// git from printing, the progress a carriage return writes over, the author date a commit's summary repeats, and an unindented line printed
// already, as ssh repeats a warning for each connection; a ref's update loses the padding that lines it up. Both of git's streams pass it.
type gitTidier struct {
	mu      sync.Mutex
	seen    map[string]bool
	summary bool
	// diverged is whether git refused to join branches that diverged.
	diverged bool
}

var (
	// gitCommitSummary starts the line a new commit's summary starts with, as "[main 3fcda4b] message".
	gitCommitSummary = regexp.MustCompile(`^\[\S+(?: \([^)]*\))? [0-9a-f]{4,64}\] `)
	// gitRefUpdate is a ref's line in what push and fetch print: its flag, its summary, its source and, unless deleted, its destination.
	gitRefUpdate = regexp.MustCompile(`^ ([ +\-*!=t]) (\[[^\]]+\]|[0-9a-f]+\.\.\.?[0-9a-f]+) +(\S+)(?: +-> (\S+))?(.*)$`)
)

// line is text as the model sees it; keep is false for a line it does not see.
func (t *gitTidier) line(text string) (shown string, keep bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at := strings.LastIndexByte(text, '\r'); at >= 0 && at < len(text)-1 {
		text = text[at+1:]
	}
	summary := t.summary
	t.summary = gitCommitSummary.MatchString(text)
	switch {
	case strings.HasPrefix(text, "hint:"), summary && strings.HasPrefix(text, " Date: "):
		return "", false
	case strings.Contains(text, "Not possible to fast-forward") || strings.Contains(text, "reconcile divergent branches"):
		t.diverged = true
	}
	if m := gitRefUpdate.FindStringSubmatch(text); m != nil {
		text = " " + m[1] + " " + m[2] + " " + m[3]
		if m[4] != "" {
			text += " -> " + m[4]
		}
		text += m[5]
	}
	// An indented line, a commit's summary or an empty one may well come again, for the next commit or as a status's layout.
	if text != "" && !strings.HasPrefix(text, " ") && !strings.HasPrefix(text, "\t") && !strings.HasPrefix(text, "[") {
		if t.seen[text] {
			return "", false
		}
		t.seen[text] = true
	}
	return text, true
}

// headerLine takes the next line of a commit header: a merge's parents, the author, the date and the blank line after them, in that order.
func (s *gitShaper) headerLine(text string) bool {
	if takes, complete := gitform.HeaderTakes(s.held, text); takes {
		s.held = append(s.held, text)
		if complete {
			return s.openCommit()
		}
		return true
	}
	if !s.release() {
		return false
	}
	return s.line(text)
}

// release prints held lines as git printed them, as a header that does not read as git's default one.
func (s *gitShaper) release() bool {
	held := s.held
	s.held, s.state = nil, betweenParts
	for _, line := range held {
		if !s.emit(listingRecord(line, s.parse)) {
			return false
		}
	}
	return true
}

// openCommit prints a whole commit header's lines as the part its one line stands for.
func (s *gitShaper) openCommit() bool {
	header, own, ok := gitform.Header(s.held, s.ownEmail())
	if !ok {
		return s.release()
	}
	part := &gitPart{lines: len(s.held), header: header, own: own}
	held := s.held
	s.held, s.part, s.state = nil, part, inMessage
	for i, line := range held {
		role := commitFolded
		switch {
		case i == 0:
			role = commitLine
		case gitform.IsMerge(line):
			role = commitMerge
		}
		if !s.emit(record{text: line, shape: &shaped{part: part, role: role}}) {
			return false
		}
	}
	return true
}

// ownEmail is the user.email of the repository git runs in, with the command's own -C and -c, asked once; "" when it has none.
func (s *gitShaper) ownEmail() string {
	if s.email == nil {
		var out bytes.Buffer
		email := ""
		args := append(append([]string{"git"}, s.plan.globals...), "config", "user.email")
		if s.inv.call.runProgramTo(s.inv.ctx, args, &out, io.Discard, nil) == nil {
			email = strings.TrimSpace(out.String())
		}
		s.email = &email
	}
	return *s.email
}

// openFile prints a file part's own header lines once the part's new side is known: as lines the file's header stands for when nib holds
// it, else as git printed them, like the rest of that part.
func (s *gitShaper) openFile() bool {
	held := s.held
	s.held, s.state, s.hunk = nil, inHunks, false
	if !s.based {
		s.base, s.based = s.plan.base(s.inv), true
	}
	s.source, s.part = s.plan.sectionSource(s.inv, s.base, held), nil
	if s.source != nil {
		s.part = &gitPart{}
	}
	for _, raw := range held {
		line := strings.TrimSuffix(raw, "\r")
		r := record{text: raw}
		switch {
		case s.source == nil:
		case gitDiff.MatchString(line) || strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ "):
			r.shape = &shaped{part: s.part, role: diffHeader}
		default:
			// Mode, rename and similarity lines print under the file's header.
			r = s.display(raw, line)
		}
		if !s.emit(r) {
			return false
		}
	}
	return true
}

// hunkLine takes a line after a file part's own header: a hunk's header or line, or the first line past the part.
func (s *gitShaper) hunkLine(raw string) bool {
	if s.hunk && (s.old > 0 || s.added > 0 || strings.HasPrefix(raw, `\`)) {
		switch {
		case strings.HasPrefix(raw, `\`):
		case strings.HasPrefix(raw, "-"):
			s.old--
		case strings.HasPrefix(raw, "+"):
			s.added--
		case strings.HasPrefix(raw, " "), raw == "", raw == "\r":
			s.old--
			s.added--
		default:
			// A line no hunk holds ends the part, as a log's next commit does, whatever its format.
			return s.between(raw)
		}
		return s.fileLine(raw)
	}
	line := strings.TrimSuffix(raw, "\r")
	m := gitHunk.FindStringSubmatch(line)
	if m == nil {
		return s.between(raw)
	}
	s.hunk, s.old, s.added = true, hunkLength(m[1]), hunkLength(m[3])
	s.next, _ = strconv.Atoi(m[2])
	if s.source == nil {
		return s.emit(record{text: raw})
	}
	return s.emit(s.display(raw, line))
}

// fileLine prints a hunk's line: one the hunk adds or keeps is the file's row where the file holds exactly that text at that line, a removed
// one is -|TEXT, and one the file no longer holds there prints in a form that is never an anchor.
func (s *gitShaper) fileLine(raw string) bool {
	if s.source == nil {
		return s.emit(record{text: raw})
	}
	line := strings.TrimSuffix(raw, "\r")
	if line == "" {
		// A context line of an empty line, as some tools strip its space.
		line = " "
	}
	mark, text := line[0], line[1:]
	switch mark {
	case '\\':
		return s.emit(s.display(raw, line))
	case '-':
		return s.emit(s.display(raw, "-|"+text))
	}
	at := s.next
	s.next++
	if at == 1 {
		text = strings.TrimPrefix(text, string(rune(0xfeff)))
	}
	if view := s.source.View; at >= 1 && at <= len(view.Lines) && view.Lines[at-1] == text {
		return s.emit(record{text: raw, origin: &origin{source: s.source, line: at}, added: mark == '+', context: mark == ' ',
			shape: &shaped{part: s.part, role: diffRow}})
	}
	return s.emit(s.display(raw, fmt.Sprintf("%c%d|%s", mark, at, text)))
}

// display is a line that prints as shown under the file's header but is not the file's line.
func (s *gitShaper) display(raw, shown string) record {
	return record{text: raw, origin: &origin{source: s.source}, display: true, shape: &shaped{part: s.part, role: diffRow, shown: shown}}
}
