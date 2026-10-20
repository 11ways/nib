package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/patch"
	"github.com/11ways/nib/internal/snapshots"
)

const grepTimeout = 30 * time.Second

type submatch struct {
	Start       int `json:"start"`
	End         int `json:"end"`
	Replacement *struct {
		Text  *string `json:"text"`
		Bytes *string `json:"bytes"`
	} `json:"replacement"`
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// repositoryOf is the folder of the repository holding location, or "" outside git.
func repositoryOf(location string) string {
	for directory := location; ; directory = filepath.Dir(directory) {
		if exists(filepath.Join(directory, ".git")) {
			return directory
		}
		if filepath.Dir(directory) == directory {
			return ""
		}
	}
}

var gitdirLine = regexp.MustCompile(`(?m)^gitdir:` + jsSpace + `*(.+)$`)

// commonDirectory is the git directory a repository shares with all its worktrees.
func commonDirectory(repository string) string {
	marker := filepath.Join(repository, ".git")
	info, err := os.Lstat(marker)
	if err != nil {
		return ""
	}
	if !info.Mode().IsRegular() {
		if real, err := realpath(marker); err == nil {
			return real
		}
		return marker
	}
	content, _ := os.ReadFile(marker)
	match := gitdirLine.FindStringSubmatch(string(content))
	if match == nil || js.Trim(match[1]) == "" {
		return ""
	}
	gitdir := filepath.Join(repository, js.Trim(match[1]))
	if filepath.IsAbs(js.Trim(match[1])) {
		gitdir = js.Trim(match[1])
	}
	shared := gitdir
	if data, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		name := js.Trim(string(data))
		if filepath.IsAbs(name) {
			shared = name
		} else {
			shared = filepath.Join(gitdir, name)
		}
	}
	if real, err := realpath(shared); err == nil {
		return real
	}
	return shared
}

func gitEnvironment(repository string) []string {
	env := []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(repository), "GIT_LITERAL_PATHSPECS=1"}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return env
}

// gitIn is git run in repository as searches run it: a repository's config can name a command for core.fsmonitor, which git would run
// there, and nib must never run code it finds, nor let the caller's GIT_ variables point git elsewhere.
func gitIn(ctx context.Context, repository string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-C", repository}, args...)...)
	cmd.Env = gitEnvironment(repository)
	return cmd
}

// Ignored reports whether the ignore rules of the repository holding directory, a canonical path, cover it; false outside git, for a
// repository's own folder, or without git.
func Ignored(directory string) bool {
	repository := repositoryOf(directory)
	relative, ok := inside(repository, directory)
	if repository == "" || !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// check-ignore takes paths, not pathspecs, and refuses the literal magic searches ask for.
	cmd := gitIn(ctx, repository, "check-ignore", "-q", "--", filepath.ToSlash(relative)+"/")
	cmd.Env = append(cmd.Env, "GIT_LITERAL_PATHSPECS=0")
	return cmd.Run() == nil
}

// nestedRepositories are the repositories nested in repository (under within, when given) that its ignore rules cover, from git's own list of
// ignored folders, or with ignored false those it neither tracks nor ignores; the folders right inside such a folder are looked at too,
// unless it holds more than containerEntries entries. A worktree of repository is left out, since it repeats the same files.
func nestedRepositories(ctx context.Context, repository, within string, ignored bool) ([]string, error) {
	if within == "" {
		within = "."
	}
	args := []string{"ls-files", "-z", "--others", "--exclude-standard", "--directory"}
	if ignored {
		args = append(args, "--ignored")
	}
	output, err := gitIn(ctx, repository, append(args, "--", within)...).Output()
	if err != nil {
		// Without git, ripgrep's own walk is all there is.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	own := commonDirectory(repository)
	isRepository := func(folder string) bool {
		return exists(filepath.Join(folder, ".git")) && (own == "" || commonDirectory(folder) != own)
	}
	var nested []string
	for _, entry := range strings.Split(string(output), "\x00") {
		if !strings.HasSuffix(entry, "/") {
			continue
		}
		folder := filepath.Join(repository, entry)
		if exists(filepath.Join(folder, ".git")) {
			if isRepository(folder) {
				nested = append(nested, folder)
			}
			continue
		}
		entries, err := os.ReadDir(folder)
		if err != nil || len(entries) > containerEntries {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				if child := filepath.Join(folder, e.Name()); isRepository(child) {
					nested = append(nested, child)
				}
			}
		}
	}
	return nested, nil
}

// listedIn are the folders among folders that ripgrep run with flags from root lists a file right inside of.
func listedIn(ctx context.Context, root string, folders []string, flags []string) map[string]bool {
	depth := 0
	for _, folder := range folders {
		relative, _ := filepath.Rel(root, folder)
		depth = max(depth, len(strings.Split(relative, string(filepath.Separator))))
	}
	args := append([]string{"--files", "--null", "--no-config", "--hidden", "--glob", "!.git", fmt.Sprintf("--max-depth=%d", depth+1)}, flags...)
	args = append(args, "--", root)
	output, _ := exec.CommandContext(ctx, "rg", args...).Output()
	parents := map[string]bool{}
	for _, file := range strings.Split(string(output), "\x00") {
		if file != "" {
			parents[filepath.Dir(file)] = true
		}
	}
	found := map[string]bool{}
	for _, folder := range folders {
		if parents[folder] {
			found[folder] = true
		}
	}
	return found
}

// repositoriesBelow are the repositories under root, a folder outside git, found by walking at most foldersOutsideGit folders without following links.
func repositoriesBelow(ctx context.Context, root string) ([]string, bool, error) {
	var repositories []string
	queue := []string{root}
	for walked := 0; len(queue) > 0; walked++ {
		if walked == foldersOutsideGit {
			return repositories, true, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		folder := queue[0]
		queue = queue[1:]
		entries, _ := os.ReadDir(folder)
		if folder != root && slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == ".git" }) {
			repositories = append(repositories, folder)
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				queue = append(queue, filepath.Join(folder, e.Name()))
			}
		}
	}
	return repositories, false, nil
}

// Repositories are the git repositories at and below root, a canonical folder, sorted by path: root itself when it is a repository's own
// folder, the repositories searches find below it, ignored or untracked ones nested in a repository included, and theirs in turn; capped
// is true when the walk outside git stopped early. A repository's worktrees are left out, as a submodule is.
func Repositories(ctx context.Context, root string) (found []string, capped bool, err error) {
	seen := map[string]bool{}
	var visit func(repository, within string) error
	visit = func(repository, within string) error {
		for _, ignored := range []bool{true, false} {
			nested, err := nestedRepositories(ctx, repository, within, ignored)
			if err != nil {
				return err
			}
			for _, folder := range nested {
				if !seen[folder] {
					seen[folder] = true
					found = append(found, folder)
					if err := visit(folder, ""); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if repository := repositoryOf(root); repository != "" {
		within := ""
		if repository == root {
			seen[root] = true
			found = append(found, root)
		} else {
			within, _ = inside(repository, root)
		}
		err = visit(repository, within)
	} else {
		var below []string
		below, capped, err = repositoriesBelow(ctx, root)
		for _, folder := range below {
			if err != nil {
				break
			}
			if !seen[folder] {
				seen[folder] = true
				found = append(found, folder)
				err = visit(folder, "")
			}
		}
	}
	if err != nil {
		return nil, false, err
	}
	slices.Sort(found)
	return found, capped, nil
}

// hiddenRepositories are the nested repositories under roots that ripgrep would leave out. Searched as roots of their own, they follow their own
// rules. Only git's rules are overruled: a repository that .ignore, .rgignore or one of the search's excluding globs keeps out stays out.
func hiddenRepositories(ctx context.Context, roots []string, globs []string) ([]string, []string, error) {
	hidden := map[string]bool{}
	var notes []string
	visited := map[string]bool{}
	var excluding []string
	for _, glob := range globs {
		if strings.HasPrefix(glob, "!") {
			excluding = append(excluding, "--glob", glob)
		}
	}
	var visit func(root, repository string) error
	visit = func(root, repository string) error {
		if visited[root] {
			return nil
		}
		visited[root] = true
		within := ""
		if root != repository {
			within, _ = filepath.Rel(repository, root)
		}
		nested, err := nestedRepositories(ctx, repository, within, true)
		if err != nil || len(nested) == 0 {
			return err
		}
		entered := listedIn(ctx, root, nested, nil)
		var left []string
		for _, folder := range nested {
			if !entered[folder] {
				left = append(left, folder)
			}
		}
		permitted := map[string]bool{}
		if len(left) > 0 {
			permitted = listedIn(ctx, root, left, append([]string{"--no-ignore-vcs"}, excluding...))
		}
		for folder := range permitted {
			hidden[folder] = true
		}
		for _, folder := range nested {
			if entered[folder] || permitted[folder] {
				if err := visit(folder, folder); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		if repository := repositoryOf(root); repository != "" {
			if err := visit(root, repository); err != nil {
				return nil, nil, err
			}
			continue
		}
		// Outside git ripgrep enters a repository by itself unless an .ignore or a glob keeps it out, and each one's own rules decide what it holds.
		below, capped, err := repositoriesBelow(ctx, root)
		if err != nil {
			return nil, nil, err
		}
		if capped {
			notes = append(notes, fmt.Sprintf("[looked for git repositories in the first %d folders of %s only; one further down may hide its nested repositories]", foldersOutsideGit, root))
		}
		if len(below) > 0 {
			searched := listedIn(ctx, root, below, excluding)
			for _, folder := range below {
				if searched[folder] {
					if err := visit(folder, folder); err != nil {
						return nil, nil, err
					}
				}
			}
		}
	}
	found := sortedKeysString(hidden)
	return found, notes, nil
}

func sortedKeysString(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type ripgrepOutput struct {
	stdout, stderr string
	notes          []string
}

// ripgrep runs rg with args over roots and the nested repositories they hold.
func ripgrep(ctx *Context, signal context.Context, roots []string, args []string) (ripgrepOutput, error) {
	var globs []string
	for index, arg := range args {
		if index > 0 && args[index-1] == "--glob" && arg != "!.git" {
			globs = append(globs, arg)
		}
	}
	var nested, notes []string
	if !slices.Contains(args, "--no-ignore") {
		var err error
		nested, notes, err = hiddenRepositories(signal, roots, globs)
		if err != nil {
			return ripgrepOutput{}, err
		}
	}
	all := append(append(slices.Clone(args), "--"), roots...)
	all = append(all, nested...)
	cmd := exec.CommandContext(signal, "rg", all...)
	cmd.Dir = ctx.Directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if signal.Err() != nil {
		return ripgrepOutput{}, signal.Err()
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return ripgrepOutput{}, fmt.Errorf("rg failed: %w", err)
	}
	if exit != nil && exit.ExitCode() == 2 && stdout.Len() == 0 {
		return ripgrepOutput{}, fmt.Errorf("rg failed: %s", js.Trim(stderr.String()))
	}
	return ripgrepOutput{stdout.String(), stderr.String(), notes}, nil
}

// countsByFolder is match counts by folder, most first, at the deepest folder level that fits in countRows rows; nil when that saves no rows.
func countsByFolder(names []string, count func(string) int, display func(string) string) []string {
	split := func(shown string) []string {
		segments := strings.Split(shown, "/")
		// An absolute path keeps its leading slash on its first segment.
		if segments[0] == "" && len(segments) > 1 {
			return append([]string{"/" + segments[1]}, segments[2:]...)
		}
		return segments
	}
	type item struct {
		name     string
		segments []string
	}
	paths := make([]item, len(names))
	for i, name := range names {
		paths[i] = item{name, split(display(name))}
	}
	// The folder every match is in says nothing, so the levels start below it.
	base := 0
	for {
		all := true
		for _, p := range paths {
			if !(len(p.segments)-1 > base && p.segments[base] == paths[0].segments[base]) {
				all = false
				break
			}
		}
		if !all {
			break
		}
		base++
	}
	group := func(segments []string, level int) string {
		if len(segments) > base+level {
			return strings.Join(segments[:base+level], "/") + "/"
		}
		return strings.Join(segments, "/")
	}
	width := func(level int) int {
		set := map[string]bool{}
		for _, p := range paths {
			set[group(p.segments, level)] = true
		}
		return len(set)
	}
	deepest := 0
	for _, p := range paths {
		deepest = max(deepest, len(p.segments))
	}
	deepest = deepest - 1 - base
	if deepest < 1 || width(1) > countRows {
		return nil
	}
	level := 1
	for level < deepest && width(level+1) <= countRows {
		level++
	}
	if width(level) == len(names) {
		return nil
	}
	type sum struct{ matches, files int }
	groups := map[string]*sum{}
	var keys []string
	for _, p := range paths {
		key := group(p.segments, level)
		if groups[key] == nil {
			groups[key] = &sum{}
			keys = append(keys, key)
		}
		groups[key].matches += count(p.name)
		groups[key].files++
	}
	slices.SortStableFunc(keys, func(a, b string) int {
		if groups[a].matches != groups[b].matches {
			return groups[b].matches - groups[a].matches
		}
		return strings.Compare(a, b)
	})
	rows := make([]string, len(keys))
	for i, key := range keys {
		s := groups[key]
		if strings.HasSuffix(key, "/") {
			plural := "s"
			if s.files == 1 {
				plural = ""
			}
			rows[i] = fmt.Sprintf("%d %s (%d file%s)", s.matches, key, s.files, plural)
		} else {
			rows[i] = fmt.Sprintf("%d %s", s.matches, key)
		}
	}
	return rows
}

// replaced is a matched line as a grep replace would leave it, where its first replacements sit in it, and whether it differs from the line.
type replaced struct {
	text    string
	spans   []span
	changed bool
}

type grepShape struct {
	limit         int
	before, after int
	columns       int
	// explicit is set when the agent chose maxLineLength, so a long matched line is cut only past it.
	explicit        bool
	inside          func(int) bool
	spans           map[int][]span
	replaced        map[int]replaced
	ripgrepFallback func(file string) (map[int]string, error)
}

// grepFile is one file of a grep page, read under its lock; listed holds the lines its block shows.
type grepFile struct {
	name     string
	shape    grepShape
	matches  []int
	target   *target
	failed   string
	anchored *snapshots.View
	untagged string
	lines    []string
	lineAt   func(int) (string, bool)
	shown    []int
	listed   map[int]bool
}

type matchRef struct {
	file *grepFile
	line int
}

// frameMatches lists context lines around the page's shown matches while they come to at most GrepContextLines; past that, definitions first.
// It returns the note saying which matches got context, when not all did.
func frameMatches(files []*grepFile, before, after int, asked bool) string {
	around := func(f *grepFile, line int) []int {
		var found []int
		for index := max(1, line-before); index <= line+after; index++ {
			if _, ok := f.lineAt(index); !f.listed[index] && ok && (f.shape.inside == nil || f.shape.inside(index)) {
				found = append(found, index)
			}
		}
		return found
	}
	var all []matchRef
	wanted := 0
	for _, f := range files {
		set := map[int]bool{}
		for _, line := range f.shown {
			all = append(all, matchRef{f, line})
			for _, index := range around(f, line) {
				set[index] = true
			}
		}
		wanted += len(set)
	}
	if wanted <= GrepContextLines {
		for _, m := range all {
			for _, index := range around(m.file, m.line) {
				m.file.listed[index] = true
			}
		}
		return ""
	}
	defined := map[*grepFile]map[int]bool{}
	for _, f := range files {
		if f.lines != nil {
			defined[f] = definitionLines(f.target.display, f.lines, f.shown, f.shape.spans)
		} else {
			defined[f] = map[int]bool{}
		}
	}
	var definitions []matchRef
	for _, m := range all {
		if defined[m.file][m.line] {
			definitions = append(definitions, m)
		}
	}
	given := map[matchRef]bool{}
	var givenOrder []matchRef
	left := GrepContextLines
	give := func(m matchRef) bool {
		fresh := around(m.file, m.line)
		if len(fresh) > left {
			return false
		}
		left -= len(fresh)
		for _, index := range fresh {
			m.file.listed[index] = true
		}
		given[m] = true
		givenOrder = append(givenOrder, m)
		return true
	}
	if len(definitions) > 0 && asked {
		for _, m := range definitions {
			for _, index := range around(m.file, m.line) {
				m.file.listed[index] = true
			}
			given[m] = true
			givenOrder = append(givenOrder, m)
		}
	} else {
		candidates := definitions
		if len(definitions) == 0 {
			candidates = nil
			if asked {
				candidates = all
			}
		}
		for _, m := range candidates {
			if !give(m) {
				break
			}
		}
	}
	var bare *matchRef
	for i := range all {
		if !given[all[i]] {
			bare = &all[i]
			break
		}
	}
	if bare == nil {
		return ""
	}
	over := fmt.Sprintf("around all %d they would be %d lines, over grep's budget of %d", len(all), wanted, GrepContextLines)
	rangeText := fmt.Sprintf("%s:%d-%d", bare.file.target.display, max(1, bare.line-before), bare.line+after)
	if len(givenOrder) == 0 {
		return fmt.Sprintf("[all %d match rows shown, without context lines: %s; ask with before or after for some around the first matches, or read a range like %s]", len(all), over, rangeText)
	}
	first := givenOrder[0]
	var which string
	switch {
	case len(definitions) == 0:
		which = fmt.Sprintf("the first %d", len(givenOrder))
	case len(givenOrder) == 1:
		which = fmt.Sprintf("the definition at %s:%d", first.file.target.display, first.line)
	case len(givenOrder) < len(definitions):
		which = fmt.Sprintf("the first %d of %d definitions", len(givenOrder), len(definitions))
	default:
		which = fmt.Sprintf("the %d definitions", len(givenOrder))
	}
	return fmt.Sprintf("[all %d match rows shown, context lines only around %s: %s; for another match's, read a range like %s]", len(all), which, over, rangeText)
}

// replaceLine swaps a line's submatches for ripgrep's replacements; ripgrep reports UTF-8 byte offsets.
func replaceLine(lineText string, submatches []submatch) replaced {
	body := lineContent(lineText)
	var composed strings.Builder
	var spans []span
	cursor := 0
	for _, s := range submatches {
		start := min(s.Start, len(body))
		replacement := ""
		if s.Replacement != nil {
			if s.Replacement.Text != nil {
				replacement = *s.Replacement.Text
			} else if s.Replacement.Bytes != nil {
				decoded, _ := base64.StdEncoding.DecodeString(*s.Replacement.Bytes)
				replacement = strings.ToValidUTF8(string(decoded), "\ufffd")
			}
		}
		composed.WriteString(strings.ToValidUTF8(body[cursor:start], "\ufffd"))
		at := js.Len(composed.String())
		spans = append(spans, span{at, at + js.Len(replacement)})
		composed.WriteString(replacement)
		cursor = max(start, min(s.End, len(body)))
	}
	result := composed.String() + strings.ToValidUTF8(body[cursor:], "\ufffd")
	return replaced{text: result, spans: spans[:min(len(spans), windows)], changed: result != body}
}

// replacementRow is a >N| row: a line as a replace would leave it, cut like a match row, so a long one shows its first replacements.
func replacementRow(line int, content string, spans []span, columns int) string {
	shown := visible(content)
	if js.Len(shown) <= columns {
		return fmt.Sprintf(">%d|%s", line, shown)
	}
	if w := matchWindows(content, spans, columns); w != nil {
		return ">" + cutRow(line, w.label, w.text)
	}
	return ">" + cutRow(line, charsLabel([]string{fmt.Sprintf("1-%d", columns)}, js.Len(shown)), js.Slice(shown, 0, columns))
}

// allocate splits budget across demands so small entries get everything and large ones share the rest equally.
func allocate(demands []int, budget int) []int {
	shares := make([]int, len(demands))
	left := budget
	order := make([]int, len(demands))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return demands[a] - demands[b] })
	for position, index := range order {
		shares[index] = min(demands[index], int(math.Floor(float64(left)/float64(len(order)-position))))
		left -= shares[index]
	}
	return shares
}

// GrepArgs are grep's arguments; nil numbers are unset.
type GrepArgs struct {
	Pattern       string
	Paths         []string
	Glob          []string
	IgnoreCase    bool
	Literal       bool
	Before, After *int
	Skip          *int
	Mode          string
	NoIgnore      bool
	MaxLineLength *int
	Replace       *string
}

type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text *string `json:"text"`
		} `json:"path"`
		LineNumber int `json:"line_number"`
		Lines      struct {
			Text  *string `json:"text"`
			Bytes *string `json:"bytes"`
		} `json:"lines"`
		Submatches []submatch `json:"submatches"`
	} `json:"data"`
}

// capped holds an agent's number to limit instead of refusing it, with a note so the agent learns the limit.
func capped(name string, value *int, limit int, notes *[]string) *int {
	if value == nil || *value <= limit {
		return value
	}
	*notes = append(*notes, fmt.Sprintf("[%s %d is over its limit of %d, so %d was used]", name, *value, limit, limit))
	return &limit
}

func valueOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// Grep searches file contents with ripgrep.
func (e *Engine) Grep(ctx *Context, args GrepArgs) (result Result, err error) {
	defer func() { result.Text = e.teachRowNotes(ctx, result.Text) }()
	var caps []string
	multiline := strings.Contains(args.Pattern, "\n") || strings.Contains(args.Pattern, `\n`)
	mode := args.Mode
	if mode == "" {
		mode = "matches"
	}
	if args.Replace != nil && mode != "matches" {
		return Result{}, errors.New("replace previews its changes as rows, so it needs mode matches")
	}
	if args.Replace != nil && multiline {
		return Result{}, errors.New("replace works on matches within one line; change multi-line text with edit")
	}
	// A line selector narrows its file's matches; the same file named again without one searches all of it.
	selections := map[string][]selection{}
	selectionSet := map[string]bool{}
	var rootOrder []string
	var skipped, kept []string
	entries := args.Paths
	if len(entries) == 0 {
		entries = []string{"."}
	}
	for _, entry := range entries {
		file, sel, err := splitSelector(entry, ctx)
		if err == nil && sel.symbols != nil {
			err = fmt.Errorf("%s: grep takes line ranges after a path, not a symbol; read the symbol, or grep its file", entry)
		}
		if err != nil {
			return Result{}, err
		}
		refused, err := outsideSandbox(ctx, file)
		if err != nil {
			return Result{}, err
		}
		if refused != "" {
			skipped = append(skipped, refused)
			continue
		}
		root := ctx.resolve(file)
		chosen := sel.ranges != nil || sel.tail > 0
		found, err := os.Stat(root)
		if err != nil {
			// ripgrep only warns about a missing path, which would read as a search that found nothing.
			skipped = append(skipped, missingPath(entry, ctx))
			continue
		}
		if chosen && found.IsDir() {
			return Result{}, fmt.Errorf("%s: line selectors need a file, not a directory", entry)
		}
		kept = append(kept, entry)
		earlier, had := selections[root]
		if _, seen := selectionSet[root]; !seen {
			rootOrder = append(rootOrder, root)
		}
		if !chosen || (had && earlier == nil) {
			selections[root] = nil
		} else {
			selections[root] = append(earlier, sel)
		}
		selectionSet[root] = true
	}
	// A path that names nothing, or that the sandbox refuses, is noted, and the other paths are searched without it.
	if len(skipped) > 0 {
		if len(kept) == 0 {
			return Result{}, errors.New(strings.Join(skipped, "; "))
		}
		again := args
		again.Paths = kept
		result, err := e.Grep(ctx, again)
		if err != nil {
			return result, err
		}
		result.Text = fmt.Sprintf("[%s; searched the other paths]\n\n%s", strings.Join(skipped, "; "), result.Text)
		return result, nil
	}
	// The engine is ripgrep's own unless the pattern needs PCRE2's look-around or backreferences.
	command := []string{"--json", "--no-config", "--hidden", "--glob", "!.git", "--max-columns", "0", "--engine", "auto"}
	if args.IgnoreCase {
		command = append(command, "--ignore-case")
	}
	if args.Literal {
		command = append(command, "--fixed-strings")
	}
	if args.NoIgnore {
		command = append(command, "--no-ignore")
	}
	if multiline {
		command = append(command, "--multiline")
	}
	if args.Replace != nil {
		command = append(command, "--replace", *args.Replace)
	}
	for _, glob := range args.Glob {
		command = append(command, "--glob", glob)
	}
	command = append(command, "--regexp", args.Pattern)
	signal, cancel := context.WithTimeout(ctx.Ctx, grepTimeout)
	defer cancel()
	out, err := ripgrep(ctx, signal, rootOrder, command)
	if err != nil {
		return Result{}, err
	}
	files := map[string]map[int]bool{}
	spans := map[string]map[int][]span{}
	replacedLines := map[string]map[int]replaced{}
	for _, line := range strings.Split(out.stdout, "\n") {
		if !strings.HasPrefix(line, `{"type":"match"`) {
			continue
		}
		var event rgEvent
		if json.Unmarshal([]byte(line), &event) != nil || event.Data.Path.Text == nil {
			continue
		}
		file := *event.Data.Path.Text
		lineText := ""
		if event.Data.Lines.Text != nil {
			lineText = *event.Data.Lines.Text
		}
		matched := files[file]
		if matched == nil {
			matched = map[int]bool{}
			files[file] = matched
		}
		count := max(1, len(strings.Split(strings.TrimSuffix(lineText, "\n"), "\n")))
		for offset := 0; offset < count; offset++ {
			matched[event.Data.LineNumber+offset] = true
		}
		if found := matchSpans(lineText, event.Data.Submatches); len(found) > 0 {
			if spans[file] == nil {
				spans[file] = map[int][]span{}
			}
			spans[file][event.Data.LineNumber] = found
		}
		if args.Replace != nil && event.Data.Lines.Text != nil {
			if replacedLines[file] == nil {
				replacedLines[file] = map[int]replaced{}
			}
			replacedLines[file][event.Data.LineNumber] = replaceLine(lineText, event.Data.Submatches)
		}
	}
	// ripgrep is handed paths, not descriptors, so in the sandbox a file it found by a link changed since is dropped unless it resolves inside.
	if ctx.Sandbox {
		roots, err := workspaceRoots(ctx)
		if err != nil {
			return Result{}, err
		}
		for file := range files {
			real, err := realpath(file)
			if err != nil {
				real = file
			}
			if !slices.ContainsFunc(roots, func(root string) bool { return within(root, real) }) {
				delete(files, file)
			}
		}
	}
	inside := map[string]func(int) bool{}
	for file, lines := range files {
		chosen := selections[file]
		if chosen == nil {
			continue
		}
		inRange, err := selectedLines(ctx, file, chosen)
		if err != nil {
			return Result{}, err
		}
		inside[file] = inRange
		for line := range lines {
			if !inRange(line) {
				delete(lines, line)
			}
		}
		if len(lines) == 0 {
			delete(files, file)
		}
	}
	if len(files) == 0 {
		head := "No matches found"
		if stderr := js.Trim(out.stderr); stderr != "" {
			head = fmt.Sprintf("No matches found (rg: %s)", strings.Split(stderr, "\n")[0])
		}
		return Result{Text: strings.Join(append([]string{head}, out.notes...), "\n\n")}, nil
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	skip := valueOr(args.Skip, 0)
	if mode != "matches" {
		total := 0
		for _, name := range names {
			total += len(files[name])
		}
		display := func(name string) string { return ctx.relativeDisplay(name) }
		if mode == "count" && skip == 0 && len(names) > countRows {
			ctx.preview(names)
			if grouped := countsByFolder(names, func(name string) int { return len(files[name]) }, display); grouped != nil {
				parts := []string{fmt.Sprintf("%d matches in %d files, by folder", total, len(names)), strings.Join(grouped, "\n"), "[grep one folder for its files' counts]"}
				return Result{Text: strings.Join(append(parts, out.notes...), "\n\n")}, nil
			}
		}
		page := names[min(skip, len(names)):min(skip+grepListFiles, len(names))]
		if len(page) == 0 {
			return Result{Text: fmt.Sprintf("No more results (%d matching files; skip %d is past the end)", len(names), skip)}, nil
		}
		ctx.preview(page)
		rows := make([]listedRow, len(page))
		for i, name := range page {
			rows[i] = listedRow{path: display(name)}
			if mode == "count" {
				rows[i].before = fmt.Sprintf("%d ", len(files[name]))
			}
		}
		under, lines := grouped(rows)
		head := fmt.Sprintf("%d matches in %d files", total, len(names))
		if under != "" {
			head += " under " + under
		}
		parts := []string{head, strings.Join(lines, "\n")}
		if skip+len(page) < len(names) {
			parts = append(parts, fmt.Sprintf("[%d matching files; continue with skip %d]", len(names), skip+len(page)))
		}
		return Result{Text: strings.Join(append(parts, out.notes...), "\n\n")}, nil
	}
	page := names[min(skip, len(names)):min(skip+grepFiles, len(names))]
	if len(page) == 0 {
		return Result{Text: fmt.Sprintf("No more results (%d matching files; skip %d is past the end)", len(names), skip)}, nil
	}
	// Lines from this page on that a replace would change; the token reports those it leaves out.
	changing := 0
	for _, name := range names[skip:] {
		for line := range files[name] {
			if r, ok := replacedLines[name][line]; ok && r.changed {
				changing++
			}
		}
	}
	replacing := args.Replace != nil
	total := 0
	for _, name := range page {
		total += len(files[name])
	}
	defaultBefore, defaultAfter := 1, 1
	if replacing {
		defaultBefore, defaultAfter = 0, 0
	}
	limit := grepPerFile
	if len(names) == 1 {
		limit = grepSingleFile
	}
	shape := grepShape{
		limit:    limit,
		before:   valueOr(capped("before", args.Before, grepContext, &caps), defaultBefore),
		after:    valueOr(capped("after", args.After, grepContext, &caps), defaultAfter),
		columns:  valueOr(capped("maxLineLength", args.MaxLineLength, LineLimit, &caps), grepColumns),
		explicit: args.MaxLineLength != nil,
	}
	// Rows for a file too big to hold, or one still growing, come from ripgrep itself, which reports the context lines too.
	fallback := func(file string) (map[int]string, error) {
		flags := []string{fmt.Sprintf("--before-context=%d", shape.before), fmt.Sprintf("--after-context=%d", shape.after)}
		if inside[file] == nil {
			flags = append(flags, fmt.Sprintf("--max-count=%d", shape.limit))
		}
		result, err := ripgrep(ctx, signal, []string{file}, append(slices.Clone(command), flags...))
		if err != nil {
			return nil, err
		}
		texts := map[int]string{}
		for _, line := range strings.Split(result.stdout, "\n") {
			if !strings.HasPrefix(line, `{"type":"match"`) && !strings.HasPrefix(line, `{"type":"context"`) {
				continue
			}
			var event rgEvent
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			content := ""
			if event.Data.Lines.Text != nil {
				content = *event.Data.Lines.Text
			} else if event.Data.Lines.Bytes != nil {
				decoded, _ := base64.StdEncoding.DecodeString(*event.Data.Lines.Bytes)
				content = strings.ToValidUTF8(string(decoded), "\ufffd")
			}
			for offset, t := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
				texts[event.Data.LineNumber+offset] = strings.TrimSuffix(t, "\r")
			}
		}
		return texts, nil
	}
	canonicalPage := make([]string, 0, len(page))
	for _, name := range page {
		if real, err := realpath(name); err == nil {
			canonicalPage = append(canonicalPage, real)
		}
	}
	ctx.preview(canonicalPage)
	targets := make([]*target, len(page))
	var canonicals []string
	for i, name := range page {
		if t, err := resolveTarget(name, ctx, false); err == nil {
			targets[i] = t
			canonicals = append(canonicals, t.canonical)
		}
	}
	// The page's files are locked together, as an edit locks its files, so each block comes from one snapshot.
	return serializedAll(canonicals, ctx, func() (Result, error) {
		var loaded []*grepFile
		for index, name := range page {
			matches := sortedSet(files[name])
			s := shape
			if replacing {
				s.limit = math.MaxInt
			}
			s.inside, s.spans, s.replaced, s.ripgrepFallback = inside[name], spans[name], replacedLines[name], fallback
			f, err := e.grepLoad(ctx, name, targets[index], matches, s)
			if err != nil {
				return Result{}, err
			}
			loaded = append(loaded, f)
		}
		framing := frameMatches(loaded, shape.before, shape.after, args.Before != nil || args.After != nil)
		left := OutputLimit
		var parts, patches []string
		shownByPreview := map[string]map[int]bool{}
		changes := 0
		for _, f := range loaded {
			block, ok := e.grepBlock(ctx, f, &left, len(parts) == 0)
			if !ok {
				break
			}
			parts = append(parts, block.text)
			if block.patch != "" {
				patches = append(patches, block.patch)
			}
			if block.previewedCanonical != "" {
				set := map[int]bool{}
				for _, line := range block.previewedLines {
					set[line] = true
				}
				shownByPreview[block.previewedCanonical] = set
			}
			changes += block.changes
		}
		var notes []string
		if len(parts) < len(page) {
			notes = append(notes, fmt.Sprintf("[output limit reached after %d files; narrow the search or continue with skip %d]", len(parts), skip+len(parts)))
		} else if skip+len(page) < len(names) {
			notes = append(notes, fmt.Sprintf("[%d matching files; continue with skip %d]", len(names), skip+len(page)))
		}
		if len(patches) > 0 {
			s := "s"
			if changes == 1 {
				s = ""
			}
			sections, err := patch.Parse(strings.Join(patches, "\n"))
			if err != nil {
				return Result{}, err
			}
			token := e.remember(ctx.Session, sections, &replacePreview{shown: shownByPreview, left: max(0, changing-changes)})
			notes = append(notes, fmt.Sprintf("To apply the %d replaced line%s shown (>N| rows), call edit with input: REAPPLY %s", changes, s, token))
		} else if replacing {
			notes = append(notes, "[no replacement to apply: the shown matches stay the same or are in files nib cannot edit]")
		}
		if framing != "" {
			notes = append(notes, framing)
		}
		head := fmt.Sprintf("%d matches in %d files", total, len(page))
		if len(names) > len(page) {
			head += fmt.Sprintf(" (of %d)", len(names))
		}
		all := append([]string{head}, parts...)
		all = append(all, notes...)
		all = append(all, caps...)
		all = append(all, out.notes...)
		return Result{Text: strings.Join(all, "\n\n")}, nil
	})
}

// grepLoad reads one file of a grep page and picks the matches its block shows; the caller holds the file's lock.
func (e *Engine) grepLoad(ctx *Context, file string, t *target, matches []int, shape grepShape) (*grepFile, error) {
	loaded := &grepFile{name: file, shape: shape, matches: matches, target: t, lineAt: func(int) (string, bool) { return "", false }, listed: map[int]bool{}}
	if t == nil {
		loaded.failed = fmt.Sprintf("[%s] vanished before it could be read", file)
		return loaded, nil
	}
	err := func() error {
		handle, err := os.OpenFile(t.canonical, os.O_RDONLY|nonblock, 0)
		if err != nil {
			return err
		}
		defer handle.Close()
		if err := confined(ctx, handle, t.canonical); err != nil {
			return err
		}
		info, err := handle.Stat()
		if err != nil {
			return err
		}
		if info.Size() > TextLimit {
			loaded.untagged = "over 2 MiB"
			return nil
		}
		current, err := snapshot(handle, TextLimit)
		if err != nil {
			return err
		}
		if source, err := decodeText(current.bytes); err == nil {
			view := viewOf(source)
			loaded.anchored = &view
			loaded.lines = view.Lines
		} else {
			raw := strings.Split(strings.ToValidUTF8(string(current.bytes), "\ufffd"), "\n")
			for i, line := range raw {
				raw[i] = strings.TrimSuffix(line, "\r")
			}
			loaded.lines = raw
		}
		return nil
	}()
	if err != nil {
		var changed changedWhileReading
		if !errors.As(err, &changed) {
			loaded.failed = fmt.Sprintf("[%s] skipped: %s", t.display, err.Error())
			return loaded, nil
		}
		loaded.untagged = "changing while read"
	}
	if lines := loaded.lines; lines != nil {
		loaded.lineAt = func(line int) (string, bool) {
			if line < 1 || line > len(lines) {
				return "", false
			}
			return lines[line-1], true
		}
	}
	if loaded.untagged != "" {
		texts, err := shape.ripgrepFallback(file)
		if err != nil {
			return nil, err
		}
		loaded.lineAt = func(line int) (string, bool) {
			t, ok := texts[line]
			return t, ok
		}
	}
	for _, line := range matches {
		if _, ok := loaded.lineAt(line); ok && len(loaded.shown) < shape.limit {
			loaded.shown = append(loaded.shown, line)
		}
	}
	for _, line := range loaded.shown {
		loaded.listed[line] = true
	}
	return loaded, nil
}

type grepBlockResult struct {
	text               string
	patch              string
	changes            int
	previewedCanonical string
	previewedLines     []int
}

// grepBlock is one loaded file's block, recording its fully shown rows as seen; past the budget the file is left out, unless cut lets it stop
// at the budget instead, as the first file of a page must.
func (e *Engine) grepBlock(ctx *Context, f *grepFile, left *int, cut bool) (grepBlockResult, bool) {
	if f.failed != "" {
		return grepBlockResult{text: f.failed}, true
	}
	t := f.target
	hits := map[int]bool{}
	for _, line := range f.shown {
		hits[line] = true
	}
	head := fmt.Sprintf("[%s] (not anchorable)", t.display)
	switch {
	case f.anchored != nil:
		head = fmt.Sprintf("[%s#%s]", t.display, f.anchored.Tag)
	case f.untagged != "":
		head = fmt.Sprintf("[%s] (not anchorable: %s; rows from ripgrep)", t.display, f.untagged)
	}
	type change struct{ content, next string }
	type group struct {
		line   int
		rows   []string
		seen   bool
		mark   *marks
		change *change
		// covers are the lines a held marker or its row-map rows stand for, context lines only: a match row is what the search is for, so it
		// always prints. mapped marks the row-map rows, which need no lesson.
		covers []int
		mapped bool
	}
	var groups []group
	for _, line := range sortedSet(f.listed) {
		g := group{line: line}
		marker := " "
		if hits[line] {
			marker = "*"
		}
		content, _ := f.lineAt(line)
		var next *replaced
		if hits[line] && f.shape.replaced != nil {
			if r, ok := f.shape.replaced[line]; ok {
				next = &r
			}
		}
		// A line a replace changes shows only as it will be: the >N| rows carry the match too.
		if next != nil && next.text != content {
			g.change = &change{content, next.text}
			// An empty line is replaced with a PUT, which needs the line seen, and its row costs a few characters.
			if content == "" && f.untagged == "" {
				g.rows = append(g.rows, fmt.Sprintf("%s%d:", marker, line))
				g.seen = true
			}
			parts := strings.Split(next.text, "\n")
			for _, part := range parts {
				var s []span
				if len(parts) == 1 {
					s = next.spans
				}
				g.rows = append(g.rows, replacementRow(line, part, s, f.shape.columns))
			}
			groups = append(groups, g)
			continue
		}
		// A long matched line shows the stretches around its matches when cutting it at the start would hide them, and past matchColumns,
		// unless maxLineLength asked for lines whole, its start and those stretches only.
		var w *windowed
		if length := js.Len(visible(content)); hits[line] && length > f.shape.columns {
			w = matchWindows(content, f.shape.spans[line], f.shape.columns)
		} else if hits[line] && !f.shape.explicit && length > matchColumns {
			w = headWindows(content, f.shape.spans[line])
		}
		switch {
		case f.untagged != "":
			// Untagged rows use N| so they can never pass for anchors; they are shortened like plain rows.
			shown := visible(content)
			text := shown
			if w != nil {
				text = fmt.Sprintf("[%s] %s", w.label, w.text)
			} else if js.Len(shown) > f.shape.columns {
				text = js.Slice(shown, 0, f.shape.columns) + "..."
			}
			g.rows = append(g.rows, fmt.Sprintf("%s%d|%s", marker, line, text))
		case w != nil:
			g.rows = append(g.rows, marker+cutRow(line, w.label, w.text))
			g.mark = &marks{cut: true}
		default:
			r := lineRow(line, content, f.shape.columns)
			g.rows = append(g.rows, marker+r.text)
			if r.anchor {
				g.seen = true
			} else {
				m := r.marks
				g.mark = &m
			}
		}
		groups = append(groups, g)
	}
	if f.anchored != nil {
		groups = HeldGroups(groups, e.HeldLines(ctx, t.canonical, *f.anchored), func(g group) int {
			if hits[g.line] || g.change != nil || !g.seen || g.mark != nil {
				return 0
			}
			return g.line
		}, func(run []group, r HeldRun) []group {
			if !r.Mapped() {
				return []group{{line: run[0].line, rows: []string{r.Marker()}, covers: r.Lines()}}
			}
			rows := make([]string, 0, len(run))
			for line := r.First; line <= r.Last; line++ {
				rows = append(rows, MapRow(line, lineContent(f.anchored.Lines[line-1])))
			}
			return []group{{line: run[0].line, rows: rows, covers: r.Lines(), mapped: true}}
		})
	}
	notes := func(kept []group, stop int) []string {
		var out []string
		if stop > 0 {
			out = append(out, fmt.Sprintf("[output limit reached; grep %s:%d- to go on from line %d]", t.display, stop, stop))
		}
		if len(f.matches) > len(f.shown) {
			extra := ""
			if f.shape.replaced != nil {
				extra = "; they are not in the token, so grep again after applying it"
			}
			out = append(out, fmt.Sprintf("[%d more matches in this file%s]", len(f.matches)-len(f.shown), extra))
		}
		if f.untagged != "" {
			first := 1
			if len(f.shown) > 0 {
				first = f.shown[0]
			}
			out = append(out, fmt.Sprintf("[to read around a match, use read with plain: true and a range, like %s:%d-%d]", t.display, max(1, first-10), first+10))
			if slices.ContainsFunc(kept, func(g group) bool { return g.change != nil }) {
				out = append(out, "[these replacements are not in the token, since nib cannot hold this file whole]")
			}
		}
		var m []marks
		for _, g := range kept {
			if g.mark != nil {
				m = append(m, *g.mark)
			}
		}
		out = append(out, rowNotes(t.display, m)...)
		// Only a peek: notes are also worked out to size the block, so the lesson counts as taught once the block is final.
		if slices.ContainsFunc(kept, func(g group) bool { return g.covers != nil && !g.mapped }) && !e.Store.Taught(ctx.Session, heldLessonKey) {
			out = append(out, heldLesson)
		}
		return out
	}
	size := func(rows []string) int {
		n := 1
		for _, r := range rows {
			n += len(r) + 1
		}
		return n
	}
	allRows := func(gs []group) []string {
		var rows []string
		for _, g := range gs {
			rows = append(rows, g.rows...)
		}
		return rows
	}
	kept := groups
	stop := 0
	if size(append(append([]string{head}, allRows(groups)...), notes(groups, 0)...)) > *left {
		if !cut {
			return grepBlockResult{}, false
		}
		// Room for the notes the whole file would need, so the ones the kept rows need fit too.
		room := *left - size(append([]string{head}, notes(groups, math.MaxInt32)...))
		count := 0
		for count < len(groups) && size(groups[count].rows)-1 <= room {
			room -= size(groups[count].rows) - 1
			count++
		}
		kept = groups[:max(1, count)]
		if len(kept) < len(groups) {
			stop = groups[len(kept)].line
		}
	}
	block := strings.Join(append(append([]string{head}, allRows(kept)...), notes(kept, stop)...), "\n")
	*left -= len(block) + 2
	if slices.ContainsFunc(kept, func(g group) bool { return g.covers != nil && !g.mapped }) {
		e.Store.Teach(ctx.Session, heldLessonKey)
	}
	if f.anchored != nil {
		seen := []int{}
		for _, g := range kept {
			if g.seen {
				seen = append(seen, g.line)
			}
			seen = append(seen, g.covers...)
		}
		e.Store.Record(ctx.Session, t.canonical, *f.anchored, seen, nil)
	}
	result := grepBlockResult{text: block}
	if f.anchored == nil {
		return result, true
	}
	type applied struct {
		line          int
		content, next string
	}
	var changes []applied
	for _, g := range kept {
		if g.change != nil {
			changes = append(changes, applied{g.line, g.change.content, g.change.next})
		}
	}
	result.changes = len(changes)
	if len(changes) == 0 {
		return result, true
	}
	// Whole-line SUBs replace exactly what the preview showed, even where a row was cut, and need no ~ checks; only an empty line needs a PUT.
	lines := []string{fmt.Sprintf("[%s#%s]", t.display, f.anchored.Tag)}
	for _, c := range changes {
		if c.content != "" {
			lines = append(lines, fmt.Sprintf("SUB %d: %s", c.line, c.content))
		} else {
			lines = append(lines, fmt.Sprintf("PUT %d.=%d:", c.line, c.line))
		}
		for _, part := range strings.Split(c.next, "\n") {
			lines = append(lines, "+"+part)
		}
	}
	result.patch = strings.Join(lines, "\n")
	// A changed line the preview showed whole, as one uncut and uncolored >N| row, is one the agent has seen as it will be.
	result.previewedCanonical = t.canonical
	result.previewedLines = []int{}
	for _, c := range changes {
		if !strings.Contains(c.next, "\n") && visible(c.next) == c.next && js.Len(c.next) <= f.shape.columns {
			result.previewedLines = append(result.previewedLines, c.line)
		}
	}
	return result, true
}
