package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/11ways/nib/internal/gitform"
)

// revision is what PATH@REV reads: a file as of a commit, or for a directory the commit itself as git shows it, limited to that directory.
type revision struct {
	// t is the path's target with rev set; its canonical path is the file or directory as it is now, which may not exist for a file.
	t *target
	// commit is whether text is the commit's header, file rows and diff rather than a file's content.
	commit bool
	text   []byte
	// selector is what followed REV after a colon, for read's selection; a commit's path selector has already narrowed text.
	selector string
	// key names this text for what an agent read of it: the path and the commit's full hash.
	key string
	// own is whether a commit header left its author out as the repository's own user.email.
	own bool
}

// revisionLimit bounds the text a revision reads, as a file read is bounded.
const revisionLimit = TextLimit

// revisionOf reads entry as PATH@REV[:SELECTOR] when entry names nothing as typed: the repository is the nearest one holding PATH (or its
// nearest existing directory), REV anything git resolves to a commit there, and each @ is tried from the left, so a path holding @ still
// works. nil with no error means entry is no revision; an error says why one that names a repository does not resolve.
func (e *Engine) revisionOf(ctx *Context, entry string) (*revision, error) {
	if !strings.Contains(entry, "@") || exists(ctx.resolve(entry)) {
		return nil, nil
	}
	var failure error
	for i := 1; i < len(entry); i++ {
		if entry[i] != '@' {
			continue
		}
		path, rest := entry[:i], entry[i+1:]
		rev, selector, _ := strings.Cut(rest, ":")
		if rev == "" || strings.HasPrefix(rev, "-") {
			continue
		}
		r, err := e.revisionAt(ctx, path, rev, selector)
		if err != nil {
			var none *noCommit
			if errors.As(err, &none) {
				failure = err
				continue
			}
			return nil, err
		}
		if r != nil {
			return r, nil
		}
	}
	return nil, failure
}

// noCommit is a REV that git does not resolve to a commit in the repository holding a path.
type noCommit struct{ message string }

func (n *noCommit) Error() string { return n.message }

// revisionAt is path at rev, nil when path lies in no repository.
func (e *Engine) revisionAt(ctx *Context, path, rev, selector string) (*revision, error) {
	requested := ctx.resolve(path)
	directory := requested
	for !exists(directory) {
		if filepath.Dir(directory) == directory {
			return nil, nil
		}
		directory = filepath.Dir(directory)
	}
	isDirectory := directory == requested && isDirectoryAt(requested)
	if !isDirectory && directory == requested {
		directory = filepath.Dir(requested)
	}
	anchor, err := resolveTarget(directory, ctx, false)
	if err != nil {
		return nil, err
	}
	repository := repositoryOf(anchor.canonical)
	if repository == "" {
		return nil, nil
	}
	hash, err := gitText(ctx, repository, anchor.canonical, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return nil, &noCommit{fmt.Sprintf("%s@%s: %s is no commit in %s", path, rev, rev, directoryDisplay(ctx, repository))}
	}
	hash = strings.TrimSpace(hash)
	if !isDirectory {
		relative, err := filepath.Rel(directory, requested)
		if err != nil {
			return nil, err
		}
		canonical := filepath.Join(anchor.canonical, relative)
		inRepository, err := filepath.Rel(repository, canonical)
		if err != nil {
			return nil, err
		}
		t := &target{canonical: canonical, display: ctx.display(canonical), rev: "@" + rev}
		content, err := gitOutput(ctx, repository, repository, "cat-file", "blob", hash+":"+filepath.ToSlash(inRepository))
		if err != nil {
			return nil, &noCommit{fmt.Sprintf("%s@%s: %s holds no file %s", path, rev, rev, filepath.ToSlash(inRepository))}
		}
		return &revision{t: t, text: content, selector: selector, key: canonical + "@" + hash}, nil
	}
	t := &target{canonical: anchor.canonical, display: directoryDisplay(ctx, anchor.canonical), rev: "@" + rev, commit: true}
	r := &revision{t: t, commit: true, key: anchor.canonical + "@" + hash}
	subtree, err := filepath.Rel(repository, anchor.canonical)
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.ToSlash(subtree)}
	if selector != "" && !selectorPattern.MatchString("x:"+selector) {
		if paths, err = changedPaths(ctx, repository, hash, paths[0], selector); err != nil {
			return nil, err
		}
		if paths == nil {
			return nil, &noCommit{fmt.Sprintf("%s@%s: %s changed no file %s", path, rev, rev, selector)}
		}
	} else {
		r.selector = selector
	}
	args := append([]string{"show", "--no-ext-diff", "--no-textconv", "--no-color", "--pretty=medium", "--date=default", "--abbrev-commit",
		"--stat", "--patch"}, gitform.StatArgs...)
	shown, err := gitOutput(ctx, repository, repository, append(append(args, hash, "--"), paths...)...)
	if err != nil {
		return nil, err
	}
	var lines []string
	lines, r.own = commitLines(splitText(string(shown)), ownEmail(ctx, repository))
	r.text = []byte(strings.Join(lines, "\n") + "\n")
	return r, nil
}

// changedPaths are the files commit changed under subtree that selector names: the path itself, or a file it ends, as Foo.java names
// src/Foo.java.
func changedPaths(ctx *Context, repository, commit, subtree, selector string) ([]string, error) {
	listed, err := gitText(ctx, repository, repository, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", commit, "--", subtree)
	if err != nil {
		return nil, err
	}
	want := strings.TrimPrefix(filepath.ToSlash(selector), "./")
	var paths []string
	for _, changed := range splitText(listed) {
		inside := strings.TrimPrefix(changed, strings.TrimSuffix(subtree, "/")+"/")
		if subtree == "." {
			inside = changed
		}
		if inside == want || strings.HasSuffix(inside, "/"+want) {
			paths = append(paths, changed)
		}
	}
	return paths, nil
}

// commitLines are git show's lines as read prints them: the header in git's default format as one line with its message unindented, and
// the --stat rows before the diff with their counts; own says a header left its author out.
func commitLines(raw []string, email string) (lines []string, own bool) {
	diff := false
	for i := 0; i < len(raw); i++ {
		line := raw[i]
		switch {
		case diff:
		case isDiffStart(line):
			diff = true
		case gitform.Commit.MatchString(line):
			held := []string{line}
			complete := false
			for i+1 < len(raw) && !complete {
				takes, done := gitform.HeaderTakes(held, raw[i+1])
				if !takes {
					break
				}
				held, complete = append(held, raw[i+1]), done
				i++
			}
			header, ownAuthor, ok := gitform.Header(held, email)
			if !complete || !ok {
				lines = append(lines, held...)
				continue
			}
			own = own || ownAuthor
			lines = append(lines, header)
			for _, h := range held[1:] {
				if gitform.IsMerge(h) {
					lines = append(lines, h)
				}
			}
			for i+1 < len(raw) && strings.HasPrefix(raw[i+1], "    ") {
				lines = append(lines, gitform.Message(raw[i+1]))
				i++
			}
			continue
		default:
			if shown, _, ok := gitform.StatCounts(line); ok {
				line = shown
			}
		}
		lines = append(lines, line)
	}
	return lines, own
}

// ownEmail is the repository's user.email, "" when it has none.
func ownEmail(ctx *Context, repository string) string {
	email, err := gitText(ctx, repository, repository, "config", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(email)
}

func gitText(ctx *Context, repository, directory string, args ...string) (string, error) {
	out, err := gitOutput(ctx, repository, directory, args...)
	return string(out), err
}

// gitOutput runs git in directory of repository, without the caller's GIT_ variables and never running a command the repository's config
// names for core.fsmonitor; past revisionLimit it fails.
func gitOutput(ctx *Context, repository, directory string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx.Ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-C", directory}, args...)...)
	cmd.Env = gitEnvironment(repository)
	out := &limitedBuffer{limit: revisionLimit}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		if out.over {
			return nil, fmt.Errorf("git %s prints more than %d bytes; narrow it with a path", args[0], revisionLimit)
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// limitedBuffer fails a write past limit, which stops the program writing to it.
type limitedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		b.over = true
		return 0, errors.New("output limit")
	}
	return b.Buffer.Write(p)
}

// splitText splits text into lines without their newline.
func splitText(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func isDirectoryAt(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// directoryDisplay is how a directory prints, "." for the server's own.
func directoryDisplay(ctx *Context, canonical string) string {
	if base, err := realpath(ctx.Directory); err == nil && base == canonical {
		return "."
	}
	return ctx.display(canonical)
}

// isDiffStart is whether a line of git show starts one file's part of the diff.
func isDiffStart(line string) bool {
	return strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "diff --cc ") || strings.HasPrefix(line, "diff --combined ")
}

// revisionBlock is read's block for a revision: its lines as rows that are never anchors, a large commit's header and file rows first.
func (e *Engine) revisionBlock(ctx *Context, r *revision, args ReadArgs, maxLineLength int) (*block, error) {
	name := r.t.display + r.t.rev
	if args.Info {
		return nil, fmt.Errorf("%s: a revision reads as rows, so it takes no info", name)
	}
	var sel selection
	if r.selector != "" {
		var err error
		if _, sel, err = splitSelector("@:"+r.selector, ctx); err != nil {
			return nil, err
		}
	}
	if len(r.text) > TextLimit {
		return noteBlock(fmt.Sprintf("[%s] skipped: %.1f MiB, over the 2 MiB text limit", name, float64(len(r.text))/1048576)), nil
	}
	source, err := decodeText(r.text)
	if err != nil {
		return noteBlock(fmt.Sprintf("[%s: %s]", name, err)), nil
	}
	b, err := e.textBlock(ctx, r.t, source, sel, maxLineLength, args.MaxLineLength != nil)
	if err != nil || !r.own || b.finish == nil {
		return b, err
	}
	finish := b.finish
	b.finish = func(shown int) []string {
		notes := finish(shown)
		if shown > 0 && b.rows[0].at == 1 {
			notes = append(notes, e.OwnAuthorNote(ctx)...)
		}
		return notes
	}
	return b, nil
}
