package nibash

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/platform"
	"github.com/11ways/nib/internal/trash"
)

// trashing runs the commands that delete files, rm, unlink, git rm, git clean and find -delete, so that what they delete goes to the trash,
// and nibash's trash list and trash restore; handled is false for every other command, and for those forms that delete nothing.
func (c *call) trashing(ctx context.Context, mode, name string, args []string) (handled bool, err error) {
	switch filepath.Base(name) {
	case "rm":
		return true, statusError(c.rm(ctx, args))
	case "unlink":
		return true, statusError(c.unlink(ctx, args))
	case "trash":
		if len(args) > 1 && (args[1] == "list" || args[1] == "restore") {
			return true, statusError(c.trashCommand(ctx, args))
		}
	case "git":
		return c.gitTrash(ctx, mode, name, args)
	case "find":
		if slices.Contains(args[1:], "-delete") {
			return c.findDelete(ctx, name, args)
		}
	}
	return false, nil
}

func statusError(status int) error {
	if status == 0 {
		return nil
	}
	return interp.ExitStatus(status)
}

// trashOne moves the file at path, absolute, to the trash, once the sandbox allows its directory; a link goes, never its target.
func (c *call) trashOne(path string) error {
	if c.tools.Sandbox {
		if why, err := c.tools.Refused(filepath.Dir(path)); err != nil || why != "" {
			if err != nil {
				return err
			}
			return errors.New(why)
		}
	}
	if _, err := c.engine.Trash.Put(path, path, c.tools.Sandbox); err != nil {
		return err
	}
	c.mu.Lock()
	c.trashed = append(c.trashed, path)
	c.mu.Unlock()
	return nil
}

// wasTrashed is whether the call moved path to the trash.
func (c *call) wasTrashed(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.trashed, path)
}

// trashLesson tells an agent once what the deleting commands do; later calls get trashNote's short form.
const trashLesson = "moved to the trash, not deleted: %s; trash list shows what this session trashed, trash restore PATH puts one back"

// trashNote is the closing note of a call that trashed paths, "" when it trashed none.
func (c *call) trashNote() string {
	c.mu.Lock()
	paths := slices.Clone(c.trashed)
	c.mu.Unlock()
	if len(paths) == 0 {
		return ""
	}
	what := fmt.Sprintf("%d paths", len(paths))
	if len(paths) <= 3 {
		shown := make([]string, len(paths))
		for i, p := range paths {
			shown[i] = c.tools.Display(p)
		}
		what = strings.Join(shown, ", ")
	}
	if c.engine.Store.Teach(c.tools.Session, "trash") {
		return "[" + fmt.Sprintf(trashLesson, what) + "]"
	}
	return "[in the trash: " + what + "; trash restore PATH]"
}

// quoteShell quotes a file name as coreutils' rm does in its messages: in single quotes, as "..." when it holds a ' and nothing double
// quotes treat specially, and with control characters and bytes that are not UTF-8 written $'\n' or $'\ooo' between the quoted runs.
func quoteShell(name string) string {
	special := func(r rune) bool { return r < 0x20 || r == 0x7f }
	valid := utf8.ValidString(name)
	plain := valid && !strings.ContainsFunc(name, special)
	if plain && strings.Contains(name, "'") && !strings.ContainsAny(name, "$`\\\"!") {
		return `"` + name + `"`
	}
	var b strings.Builder
	b.WriteByte('\'')
	quoted := true
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		if r == utf8.RuneError && size <= 1 || special(r) {
			if quoted {
				b.WriteByte('\'')
				quoted = false
			}
			b.WriteString("$'" + escapeByte(name[i]) + "'")
			i++
			continue
		}
		if !quoted {
			b.WriteByte('\'')
			quoted = true
		}
		if r == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteString(name[i : i+size])
		}
		i += size
	}
	if quoted {
		b.WriteByte('\'')
	}
	return b.String()
}

func escapeByte(ch byte) string {
	switch ch {
	case '\a':
		return `\a`
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '\v':
		return `\v`
	}
	return fmt.Sprintf(`\%03o`, ch)
}

// quoteLocale quotes a file name as findutils does in its messages: in ‘’ under a UTF-8 locale, else in ”, control characters escaped.
func quoteLocale(name string, utf bool) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		ch := name[i]
		switch {
		case ch == '\\':
			b.WriteString(`\\`)
		case ch < 0x20 || ch == 0x7f:
			b.WriteString(escapeByte(ch))
		default:
			b.WriteByte(ch)
		}
	}
	if utf {
		return "‘" + b.String() + "’"
	}
	return "'" + b.String() + "'"
}

// utf8Locale is whether the shell's locale settings name a UTF-8 character set, as messages' quotes depend on.
func utf8Locale(hc interp.HandlerContext) bool {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := hc.Env.Get(name).String(); value != "" {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "utf-8") || strings.Contains(lower, "utf8")
		}
	}
	return false
}

// errText is an error as glibc's strerror words it, which is Go's text with a capital.
func errText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := errno.Error()
		return strings.ToUpper(text[:1]) + text[1:]
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err.Error()
	}
	return err.Error()
}

// rmRun is one rm command: its options, where it writes, and how it answers its prompts.
type rmRun struct {
	call                                               *call
	argv0                                              string
	force, recursive, dir, verbose, oneFS, preserveAll bool
	interactive                                        int
	stdout, stderr                                     io.Writer
	answers                                            *bufio.Reader
	status                                             int
	directory                                          string
}

const (
	interactNever = iota
	interactOnce
	interactAlways
)

// rmLong are rm's long options in the order coreutils lists them, as an ambiguous abbreviation names them.
var rmLong = []string{"dir", "force", "interactive", "one-file-system", "no-preserve-root", "preserve-root", "recursive", "verbose", "help", "version"}

// rm is coreutils' rm, except that what it removes goes to the trash: the same options, messages, prompts and exit status, and an option
// it would take that nibash's does not, --no-preserve-root, refused rather than run.
func (c *call) rm(ctx context.Context, args []string) int {
	hc := interp.HandlerCtx(ctx)
	r := &rmRun{call: c, argv0: args[0], stdout: hc.Stdout, stderr: hc.Stderr, directory: hc.Dir}
	if hc.Stdin != nil {
		r.answers = bufio.NewReader(hc.Stdin)
	}
	try := func() int {
		fmt.Fprintf(r.stderr, "Try '%s --help' for more information.\n", r.argv0)
		return 1
	}
	var operands []string
	ended := false
	for _, a := range args[1:] {
		switch {
		case ended || a == "-" || !strings.HasPrefix(a, "-"):
			operands = append(operands, a)
		case a == "--":
			ended = true
		case strings.HasPrefix(a, "--"):
			name, value, hasValue := strings.Cut(a[2:], "=")
			var matches []string
			for _, long := range rmLong {
				if long == name {
					matches = []string{long}
					break
				}
				if strings.HasPrefix(long, name) {
					matches = append(matches, long)
				}
			}
			switch {
			case len(matches) == 0:
				fmt.Fprintf(r.stderr, "%s: unrecognized option '%s'\n", r.argv0, a)
				return try()
			case len(matches) > 1:
				fmt.Fprintf(r.stderr, "%s: option '--%s' is ambiguous; possibilities:", r.argv0, name)
				for _, m := range matches {
					fmt.Fprintf(r.stderr, " '--%s'", m)
				}
				fmt.Fprintln(r.stderr)
				return try()
			}
			long := matches[0]
			if hasValue && long != "interactive" && long != "preserve-root" {
				fmt.Fprintf(r.stderr, "%s: option '--%s' doesn't allow an argument\n", r.argv0, long)
				return try()
			}
			switch long {
			case "dir":
				r.dir = true
			case "force":
				r.force, r.interactive = true, interactNever
			case "interactive":
				switch {
				case !hasValue || value == "always" || value == "yes":
					r.force, r.interactive = false, interactAlways
				case value == "once":
					r.force, r.interactive = false, interactOnce
				case value == "never" || value == "no" || value == "none":
					r.interactive = interactNever
				default:
					q := func(s string) string { return quoteLocale(s, utf8Locale(hc)) }
					fmt.Fprintf(r.stderr, "rm: invalid argument %s for %s\nValid arguments are:\n  - %s, %s, %s\n  - %s\n  - %s, %s\n", q(value), q("--interactive"), q("never"), q("no"), q("none"), q("once"), q("always"), q("yes"))
					return try()
				}
			case "one-file-system":
				r.oneFS = true
			case "no-preserve-root":
				fmt.Fprintf(r.stderr, "rm: nibash's rm moves what it removes to the trash and does not take --no-preserve-root\n")
				return 1
			case "preserve-root":
				switch {
				case !hasValue:
				case value == "all":
					r.preserveAll = true
				default:
					fmt.Fprintf(r.stderr, "rm: unrecognized --preserve-root argument: %s\n", quoteShell(value))
					return 1
				}
			case "recursive":
				r.recursive = true
			case "verbose":
				r.verbose = true
			case "help", "version":
				// Help and the version delete nothing, so the system program prints them.
				return statusOf(c.runReal(ctx, args))
			}
		default:
			for _, ch := range a[1:] {
				switch ch {
				case 'f':
					r.force, r.interactive = true, interactNever
				case 'i':
					r.force, r.interactive = false, interactAlways
				case 'I':
					r.force, r.interactive = false, interactOnce
				case 'r', 'R':
					r.recursive = true
				case 'd':
					r.dir = true
				case 'v':
					r.verbose = true
				default:
					fmt.Fprintf(r.stderr, "%s: invalid option -- '%c'\n", r.argv0, ch)
					return try()
				}
			}
		}
	}
	if len(operands) == 0 {
		if r.force {
			return 0
		}
		fmt.Fprintln(r.stderr, "rm: missing operand")
		return try()
	}
	if r.interactive == interactOnce && (r.recursive || len(operands) > 3) {
		plural := "s"
		if len(operands) == 1 {
			plural = ""
		}
		how := ""
		if r.recursive {
			how = " recursively"
		}
		if !r.ask(fmt.Sprintf("%s: remove %d argument%s%s? ", r.argv0, len(operands), plural, how)) {
			return 0
		}
	}
	for _, operand := range operands {
		r.one(operand)
	}
	return r.status
}

// ask prompts on standard error and reads a line of standard input, yes when it starts with y; at its end, the answer is no.
func (r *rmRun) ask(prompt string) bool {
	fmt.Fprint(r.stderr, prompt)
	if r.answers == nil {
		return false
	}
	line, err := r.answers.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.HasPrefix(line, "y") || strings.HasPrefix(line, "Y")
}

func (r *rmRun) fail(operand string, err error) {
	fmt.Fprintf(r.stderr, "rm: cannot remove %s: %s\n", quoteShell(operand), errText(err))
	r.status = 1
}

// one removes one operand as rm would, to the trash.
func (r *rmRun) one(operand string) {
	path := absolute(r.directory, operand)
	trimmed := strings.TrimRight(operand, "/")
	if r.recursive && trimmed != "" {
		if last := filepath.Base(trimmed); last == "." || last == ".." {
			fmt.Fprintf(r.stderr, "rm: refusing to remove '.' or '..' directory: skipping %s\n", quoteShell(operand))
			r.status = 1
			return
		}
	}
	info, err := os.Lstat(path)
	if err == nil && strings.HasSuffix(operand, "/") && trimmed != "" {
		// A trailing slash follows a link: rm empties the directory it points to under -r, then cannot remove the link as a directory.
		if info.Mode()&fs.ModeSymlink != 0 {
			target, serr := os.Stat(path + "/")
			switch {
			case serr != nil:
				if !r.force || !errors.Is(serr, fs.ErrNotExist) {
					r.fail(operand, serr)
				}
			case !target.IsDir() || r.dir && !r.recursive:
				r.fail(operand, syscall.ENOTDIR)
			case !r.recursive:
				r.fail(operand, syscall.EISDIR)
			default:
				real, _ := filepath.EvalSymlinks(path)
				for _, name := range dirOrder(real) {
					child, shown := filepath.Join(real, name), operand+name
					if sub, err := os.Lstat(child); err == nil && sub.IsDir() {
						r.tree(shown, child, sub)
					} else if err == nil {
						r.remove(shown, child, false)
					}
				}
				r.fail(operand, syscall.ENOTDIR)
			}
			return
		}
		if !info.IsDir() {
			r.fail(operand, syscall.ENOTDIR)
			return
		}
	}
	if err != nil {
		if r.force && errors.Is(err, fs.ErrNotExist) {
			return
		}
		r.fail(operand, err)
		return
	}
	if info.IsDir() && r.recursive {
		if root, rerr := os.Stat("/"); rerr == nil && os.SameFile(root, info) {
			fmt.Fprintf(r.stderr, "rm: it is dangerous to operate recursively on %s", quoteShell(operand))
			if operand != "/" {
				fmt.Fprint(r.stderr, " (same as '/')")
			}
			fmt.Fprintln(r.stderr, "\nrm: use --no-preserve-root to override this failsafe")
			r.status = 1
			return
		}
		if r.preserveAll {
			if parent, perr := os.Stat(filepath.Dir(path)); perr == nil && device(filepath.Dir(path), parent) != device(path, info) {
				fmt.Fprintf(r.stderr, "rm: skipping %s, since it's on a different device\nrm: and --preserve-root=all is in effect\n", quoteShell(operand))
				r.status = 1
				return
			}
		}
	}
	if info.IsDir() {
		switch {
		case r.recursive:
			r.tree(operand, path, info)
			return
		case !r.dir:
			r.fail(operand, syscall.EISDIR)
			return
		case !empty(path):
			r.fail(operand, syscall.ENOTEMPTY)
			return
		}
	}
	if r.interactive == interactAlways && !r.ask(fmt.Sprintf("%s: remove %s %s? ", r.argv0, describe(path, info), quoteShell(operand))) {
		return
	}
	r.remove(operand, path, info.IsDir())
}

// remove trashes one path and says so under -v.
func (r *rmRun) remove(operand, path string, dir bool) bool {
	if err := r.call.trashOne(path); err != nil {
		r.fail(operand, err)
		return false
	}
	if r.verbose {
		r.removed(operand, dir)
	}
	return true
}

func (r *rmRun) removed(operand string, dir bool) {
	if dir {
		fmt.Fprintf(r.stdout, "removed directory %s\n", quoteShell(operand))
	} else {
		fmt.Fprintf(r.stdout, "removed %s\n", quoteShell(operand))
	}
}

// tree removes a directory and all it holds. With nothing to ask and no other filesystem to skip, the whole directory goes to the trash at
// once, -v naming each entry as rm would have removed it; otherwise entry by entry, as rm walks it.
func (r *rmRun) tree(operand, path string, info fs.FileInfo) {
	mounts := false
	if r.oneFS {
		filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && p != path {
				if sub, err := os.Lstat(p); err == nil && device(p, sub) != device(path, info) {
					mounts = true
					return filepath.SkipAll
				}
			}
			return nil
		})
	}
	if r.interactive == interactAlways || mounts {
		r.walk(operand, path, info)
		return
	}
	if err := r.call.trashOne(path); err != nil {
		r.fail(operand, err)
		return
	}
	if r.verbose {
		r.verboseTree(operand, path)
	}
}

// verboseTree prints -v's lines for a directory already trashed whole, from its copy in the trash: each entry, then the directory.
func (r *rmRun) verboseTree(operand, path string) {
	entries := r.call.engine.Trash.Entries()
	var where string
	for _, e := range entries {
		if e.Original == path {
			where = e.File
		}
	}
	var visit func(dir, shown string)
	visit = func(dir, shown string) {
		for _, name := range dirOrder(dir) {
			child := filepath.Join(dir, name)
			entry := strings.TrimRight(shown, "/") + "/" + name
			if sub, err := os.Lstat(child); err == nil && sub.IsDir() {
				visit(child, entry)
				r.removed(entry, true)
			} else {
				r.removed(entry, false)
			}
		}
	}
	if where != "" {
		visit(where, operand)
	}
	r.removed(operand, true)
}

// walk removes a directory entry by entry, asking when -i asks and skipping directories on other filesystems under --one-file-system; it
// reports whether the directory itself went.
func (r *rmRun) walk(operand, path string, info fs.FileInfo) bool {
	if !empty(path) && r.interactive == interactAlways && !r.ask(fmt.Sprintf("%s: descend into directory %s? ", r.argv0, quoteShell(operand))) {
		return false
	}
	for _, entry := range dirOrder(path) {
		child := filepath.Join(path, entry)
		name := strings.TrimRight(operand, "/") + "/" + entry
		sub, err := os.Lstat(child)
		if err != nil {
			r.fail(name, err)
			continue
		}
		if sub.IsDir() {
			if r.oneFS && device(child, sub) != device(path, info) {
				fmt.Fprintf(r.stderr, "rm: skipping %s, since it's on a different device\n", quoteShell(name))
				r.status = 1
				continue
			}
			r.walk(name, child, sub)
			continue
		}
		if r.interactive == interactAlways && !r.ask(fmt.Sprintf("%s: remove %s %s? ", r.argv0, describe(child, sub), quoteShell(name))) {
			continue
		}
		r.remove(name, child, false)
	}
	if r.interactive == interactAlways && !r.ask(fmt.Sprintf("%s: remove directory %s? ", r.argv0, quoteShell(operand))) {
		return false
	}
	if !empty(path) {
		r.fail(operand, syscall.ENOTEMPTY)
		return false
	}
	return r.remove(operand, path, true)
}

// dirOrder are the names a directory holds in the order it gives them, which is the order rm walks it in.
func dirOrder(dir string) []string {
	handle, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer handle.Close()
	names, _ := handle.Readdirnames(-1)
	return names
}

// device is the filesystem the file at path is on, 0 where the OS cannot tell, so every file counts as on one.
func device(path string, info fs.FileInfo) uint64 {
	dev, _ := platform.Device(path, info)
	return dev
}

func empty(dir string) bool {
	handle, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer handle.Close()
	names, _ := handle.Readdirnames(1)
	return len(names) == 0
}

// describe is how rm's prompts name a file's kind.
func describe(path string, info fs.FileInfo) string {
	kind := "regular file"
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode.IsDir():
		kind = "directory"
	case mode&fs.ModeNamedPipe != 0:
		kind = "fifo"
	case mode&fs.ModeSocket != 0:
		kind = "socket"
	case mode&fs.ModeCharDevice != 0:
		kind = "character special file"
	case mode&fs.ModeDevice != 0:
		kind = "block special file"
	case info.Size() == 0:
		kind = "regular empty file"
	}
	if !platform.Writable(path) {
		kind = "write-protected " + kind
	}
	return kind
}

// unlink is coreutils' unlink, its one file going to the trash.
func (c *call) unlink(ctx context.Context, args []string) int {
	hc := interp.HandlerCtx(ctx)
	try := func() int {
		fmt.Fprintf(hc.Stderr, "Try '%s --help' for more information.\n", args[0])
		return 1
	}
	var operands []string
	ended := false
	for _, a := range args[1:] {
		switch {
		case ended || a == "-" || !strings.HasPrefix(a, "-"):
			operands = append(operands, a)
		case a == "--":
			ended = true
		case a == "--help" || a == "--version":
			return statusOf(c.runReal(ctx, args))
		case strings.HasPrefix(a, "--"):
			fmt.Fprintf(hc.Stderr, "%s: unrecognized option '%s'\n", args[0], a)
			return try()
		default:
			fmt.Fprintf(hc.Stderr, "%s: invalid option -- '%c'\n", args[0], a[1])
			return try()
		}
	}
	switch {
	case len(operands) == 0:
		fmt.Fprintln(hc.Stderr, "unlink: missing operand")
		return try()
	case len(operands) > 1:
		fmt.Fprintf(hc.Stderr, "unlink: extra operand %s\n", quoteShell(operands[1]))
		return try()
	}
	path := absolute(hc.Dir, operands[0])
	info, err := os.Lstat(path)
	if err == nil && info.IsDir() {
		err = syscall.EISDIR
	}
	if err == nil {
		err = c.trashOne(path)
	}
	if err != nil {
		fmt.Fprintf(hc.Stderr, "unlink: cannot unlink %s: %s\n", quoteShell(operands[0]), errText(err))
		return 1
	}
	return 0
}

// trashCommand is nibash's trash list, the paths this session trashed that are still there, and trash restore [-f] PATH..., which puts
// them back.
func (c *call) trashCommand(ctx context.Context, args []string) int {
	hc := interp.HandlerCtx(ctx)
	can := c.engine.Trash
	if args[1] == "list" {
		if len(args) > 2 {
			fmt.Fprintln(hc.Stderr, "trash: list takes no arguments")
			return 2
		}
		entries := can.Entries()
		if len(entries) == 0 {
			fmt.Fprintln(hc.Stdout, "Nothing this session moved to the trash is still there.")
			return 0
		}
		for _, e := range entries {
			kind := ""
			if e.Dir {
				kind = " (directory)"
			}
			fmt.Fprintf(hc.Stdout, "%s  %s%s\n", e.When.Format("2006-01-02 15:04:05"), c.tools.Display(e.Original), kind)
		}
		return 0
	}
	force := false
	var paths []string
	for _, a := range args[2:] {
		switch {
		case a == "-f" || a == "--force":
			force = true
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(hc.Stderr, "trash: restore takes -f and paths, not %s\n", a)
			return 2
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		fmt.Fprintln(hc.Stderr, "trash: restore needs the path a file had; trash list shows them")
		return 2
	}
	status := 0
	for _, p := range paths {
		path := absolute(hc.Dir, p)
		if c.tools.Sandbox {
			if why, err := c.tools.Refused(filepath.Dir(path)); err != nil || why != "" {
				fmt.Fprintf(hc.Stderr, "trash: cannot restore %s: %s\n", quoteShell(p), why)
				status = 1
				continue
			}
		}
		_, err := can.Restore(path, force, c.tools.Sandbox)
		switch {
		case errors.Is(err, trash.ErrExists):
			fmt.Fprintf(hc.Stderr, "trash: cannot restore %s: a file is there now; trash restore -f moves it to the trash first\n", quoteShell(p))
			status = 1
		case errors.Is(err, trash.ErrNotTrashed):
			fmt.Fprintf(hc.Stderr, "trash: cannot restore %s: this session did not move it to the trash, or it left the trash since; trash list shows what is there\n", quoteShell(p))
			status = 1
		case err != nil:
			fmt.Fprintf(hc.Stderr, "trash: cannot restore %s: %s\n", quoteShell(p), errText(err))
			status = 1
		default:
			fmt.Fprintf(hc.Stdout, "restored %s\n", quoteShell(p))
		}
	}
	return status
}

// gitArgs splits a git command line into the options before its subcommand, the -C directories among them, the subcommand and its words;
// ok is false when it names none.
func gitArgs(args []string) (globals, dirs []string, subcommand string, rest []string, ok bool) {
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" || a == "--config-env" || a == "--exec-path" || a == "--super-prefix":
			if i+1 >= len(args) {
				return nil, nil, "", nil, false
			}
			if a == "-C" {
				dirs = append(dirs, args[i+1])
			}
			globals = append(globals, a, args[i+1])
			i++
		case strings.HasPrefix(a, "-"):
			globals = append(globals, a)
		default:
			return globals, dirs, a, args[i+1:], true
		}
	}
	return nil, nil, "", nil, false
}

// capture runs a real program for what it prints, its standard error going to stderr.
func (c *call) capture(ctx context.Context, argv []string, stderr io.Writer) ([]byte, int) {
	var out bytes.Buffer
	status := statusOf(c.runProgramTo(ctx, argv, &out, stderr, nil))
	return out.Bytes(), status
}

// statusOf is a handler's error as an exit status.
func statusOf(err error) int {
	var exit interp.ExitStatus
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return int(exit)
	}
	return 1
}

// gitFlag is whether the words before "--" hold the long option or a bundle of short ones with the letter.
func gitFlag(words []string, long string, short byte) bool {
	for _, w := range words {
		if w == "--" {
			return false
		}
		if w == long || short != 0 && strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.IndexByte(w[1:], short) >= 0 {
			return true
		}
	}
	return false
}

// withoutQuiet are the words with -q and --quiet left out, so a dry run lists what the real one removes.
func withoutQuiet(words []string) []string {
	var kept []string
	ended := false
	for _, w := range words {
		switch {
		case ended:
		case w == "--":
			ended = true
		case w == "--quiet" || w == "-q":
			continue
		case strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w, "q"):
			w = strings.ReplaceAll(w, "q", "")
			if w == "-" {
				continue
			}
		}
		kept = append(kept, w)
	}
	return kept
}

// gitTrash runs git rm and git clean so that the files they delete go to the trash: a dry run with the same words says which, or fails as
// the real run would; for git rm the real run then updates the index, finding the files gone.
func (c *call) gitTrash(ctx context.Context, mode, name string, args []string) (bool, error) {
	globals, dirs, subcommand, rest, ok := gitArgs(args)
	if !ok || gitFlag(rest, "-h", 0) {
		return false, nil
	}
	hc := interp.HandlerCtx(ctx)
	git := append([]string{"git"}, globals...)
	git = append(git, "-c", "core.quotePath=false")
	switch subcommand {
	case "rm":
		if gitFlag(rest, "--cached", 0) || gitFlag(rest, "--dry-run", 'n') {
			return false, nil
		}
		var listed bytes.Buffer
		out, status := c.capture(ctx, append(append(slices.Clone(git), "rm", "-n"), withoutQuiet(rest)...), &listed)
		if status != 0 {
			hc.Stderr.Write(listed.Bytes())
			return true, interp.ExitStatus(status)
		}
		top, status := c.capture(ctx, append(slices.Clone(git), "rev-parse", "--show-toplevel"), io.Discard)
		if status != 0 {
			return false, nil
		}
		root := strings.TrimSpace(string(top))
		for _, line := range strings.Split(string(out), "\n") {
			quoted, found := strings.CutPrefix(line, "rm ")
			if !found {
				continue
			}
			path := filepath.Join(root, unquoteGit(strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'")))
			if _, err := os.Lstat(path); err != nil {
				continue
			}
			if err := c.trashOne(path); err != nil {
				fmt.Fprintf(hc.Stderr, "fatal: git rm: could not move %s to the trash: %s\n", quoteShell(path), errText(err))
				return true, interp.ExitStatus(128)
			}
			// git rm takes away the directories its removals leave empty.
			for dir := filepath.Dir(path); dir != root && strings.HasPrefix(dir, root) && os.Remove(dir) == nil; dir = filepath.Dir(dir) {
			}
		}
		if mode != "" {
			return true, c.runNib(ctx, mode, name, args)
		}
		return true, c.runReal(ctx, args)
	case "clean":
		if gitFlag(rest, "--dry-run", 'n') {
			return false, nil
		}
		if gitFlag(rest, "--interactive", 'i') {
			fmt.Fprintln(hc.Stderr, "fatal: nibash's git clean moves what it removes to the trash and cannot ask -i's questions; list with git clean -n, then remove with -f")
			return true, interp.ExitStatus(128)
		}
		if !gitFlag(rest, "--force", 'f') {
			// Without -f git refuses unless clean.requireForce is off; then it is a forced clean like any other.
			setting, _ := c.capture(ctx, append(slices.Clone(git), "config", "--type=bool", "--get", "clean.requireForce"), io.Discard)
			if strings.TrimSpace(string(setting)) != "false" {
				return false, nil
			}
		}
		var listed bytes.Buffer
		out, status := c.capture(ctx, append(append(slices.Clone(git), "clean", "-n"), withoutQuiet(rest)...), &listed)
		if status != 0 {
			hc.Stderr.Write(listed.Bytes())
			return true, interp.ExitStatus(status)
		}
		base := hc.Dir
		for _, d := range dirs {
			base = absolute(base, d)
		}
		quiet := gitFlag(rest, "--quiet", 'q')
		failed := false
		for _, line := range strings.Split(string(out), "\n") {
			if skipped, found := strings.CutPrefix(line, "Would skip repository "); found {
				if !quiet {
					fmt.Fprintf(hc.Stdout, "Skipping repository %s\n", skipped)
				}
				continue
			}
			shown, found := strings.CutPrefix(line, "Would remove ")
			if !found {
				continue
			}
			path := absolute(base, unquoteGit(shown))
			if err := c.trashOne(path); err != nil {
				fmt.Fprintf(hc.Stderr, "warning: failed to remove %s: %s\n", shown, errText(err))
				failed = true
				continue
			}
			if !quiet {
				fmt.Fprintf(hc.Stdout, "Removing %s\n", shown)
			}
		}
		if failed {
			return true, interp.ExitStatus(1)
		}
		return true, nil
	}
	return false, nil
}

// unquoteGit reads a path git printed in C-style double quotes, as it does for names with control characters even with core.quotePath off.
func unquoteGit(path string) string {
	if len(path) < 2 || path[0] != '"' || path[len(path)-1] != '"' {
		return path
	}
	if value, err := strconv.Unquote(path); err == nil {
		return value
	}
	return path
}

// findDelete runs find with each -delete printing the path it would delete behind a marker instead, as -depth, which -delete implies, set
// before the expression; the paths then go to the trash in find's order, a directory only once empty, as -delete would remove it.
func (c *call) findDelete(ctx context.Context, name string, args []string) (bool, error) {
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
	expression := args[i:]
	// -prune with -delete and no -depth is an error find reports before it deletes anything.
	if slices.Contains(expression, "-prune") && !slices.Contains(expression, "-depth") && !slices.Contains(expression, "-d") {
		return false, nil
	}
	token := make([]byte, 8)
	rand.Read(token)
	marker := "\x00nibtrash" + hex.EncodeToString(token) + "\x00"
	argv := append(slices.Clone(args[:i]), "-depth")
	for _, word := range expression {
		if word == "-delete" {
			argv = append(argv, "-printf", `\0nibtrash`+hex.EncodeToString(token)+`\0%p\0`)
			continue
		}
		argv = append(argv, word)
	}
	argv[0] = name
	hc := interp.HandlerCtx(ctx)
	out, status := c.capture(ctx, argv, hc.Stderr)
	utf := utf8Locale(hc)
	text := string(out)
	for {
		at := strings.Index(text, marker)
		if at < 0 {
			io.WriteString(hc.Stdout, text)
			break
		}
		io.WriteString(hc.Stdout, text[:at])
		text = text[at+len(marker):]
		end := strings.IndexByte(text, 0)
		if end < 0 {
			break
		}
		shown := text[:end]
		text = text[end+1:]
		if filepath.Base(shown) == "." {
			continue
		}
		path := absolute(hc.Dir, shown)
		info, err := os.Lstat(path)
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist):
			continue
		case err == nil && info.IsDir() && !empty(path):
			err = syscall.ENOTEMPTY
		case err == nil:
			err = c.trashOne(path)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: cannot delete %s: %s\n", name, quoteLocale(shown, utf), errText(err))
			status = max(status, 1)
		}
	}
	return true, statusError(status)
}
