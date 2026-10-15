package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/11ways/nib/internal/snapshots"
)

// workspaceRoots are the client's roots, or the server's directory, canonicalized.
func workspaceRoots(ctx *Context) ([]string, error) {
	roots := []string{ctx.Directory}
	if ctx.Roots != nil {
		listed, err := ctx.Roots(ctx.Ctx)
		if err != nil {
			return nil, err
		}
		roots = listed
	}
	canonical := make([]string, len(roots))
	for i, root := range roots {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			canonical[i] = real
		} else {
			canonical[i] = root
		}
	}
	return canonical, nil
}

// realpath is fs.realpath: an absolute path with every link resolved.
func realpath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(real)
}

// outsideSandbox says why the sandbox refuses input, if it is on and the path resolves outside every workspace root. Symlinks are followed,
// and a path that does not exist yet is judged by its nearest existing parent, so a link inside cannot lead a read or a new directory out.
func outsideSandbox(ctx *Context, input string) (string, error) {
	if !ctx.Sandbox {
		return "", nil
	}
	existing := ctx.resolve(input)
	var rest []string
	real, err := realpath(existing)
	for err != nil && filepath.Dir(existing) != existing {
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = filepath.Dir(existing)
		real, err = realpath(existing)
	}
	if err != nil {
		real = existing
	}
	canonical := filepath.Join(append([]string{real}, rest...)...)
	roots, err := workspaceRoots(ctx)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if within(root, canonical) {
			return "", nil
		}
	}
	return fmt.Sprintf("%s is outside the sandbox, which allows only %s", input, strings.Join(roots, ", ")), nil
}

var pairSelector = regexp.MustCompile(`^(.*):([1-9]\d*):([1-9]\d*)$`)

// missingPath says why entry names nothing; one written path:A:B gets the range form, since a selector writes A to B as A-B.
func missingPath(entry string, ctx *Context) string {
	pair := pairSelector.FindStringSubmatch(entry)
	if pair == nil {
		return entry + " does not exist"
	}
	if _, err := os.Stat(ctx.resolve(pair[1])); err != nil {
		return entry + " does not exist"
	}
	return fmt.Sprintf("%s does not exist; for lines %s to %s, write %s:%s-%s", entry, pair[2], pair[3], pair[1], pair[2], pair[3])
}

// target is a canonicalized path; check fails when it no longer resolves the same way.
type target struct {
	canonical string
	display   string
	check     func() error
}

// allowed is true when the sandbox is off, or canonical lies inside a workspace root.
func allowed(ctx *Context, canonical string) (bool, error) {
	if !ctx.Sandbox {
		return true, nil
	}
	roots, err := workspaceRoots(ctx)
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if within(root, canonical) {
			return true, nil
		}
	}
	return false, nil
}

// confined fails unless the open file lies inside the sandbox; the path it was opened by may have changed since it was judged.
func confined(ctx *Context, file *os.File, opened string) error {
	if !ctx.Sandbox {
		return nil
	}
	real, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
	if err != nil {
		return err
	}
	ok, err := allowed(ctx, real)
	if err != nil {
		return err
	}
	if !ok {
		return &Outside{opened + " resolves outside the sandbox"}
	}
	return nil
}

// resolveTarget canonicalizes a path; create resolves the parent only, so a final symlink is the target itself rather than followed.
// The sandbox judges the canonical path this returns, so a link changed between the two resolutions cannot lead outside.
func resolveTarget(input string, ctx *Context, create bool) (*target, error) {
	if err := ctx.checkAbort(); err != nil {
		return nil, err
	}
	refused, err := outsideSandbox(ctx, input)
	if err != nil {
		return nil, err
	}
	if refused != "" {
		return nil, &Outside{refused}
	}
	requested := ctx.resolve(input)
	locate := func() (string, error) {
		if create {
			parent, err := realpath(filepath.Dir(requested))
			if err != nil {
				return "", err
			}
			return filepath.Join(parent, filepath.Base(requested)), nil
		}
		return realpath(requested)
	}
	canonical, err := locate()
	if err != nil {
		if missing(err) {
			suffix := ""
			if create {
				suffix = " (parent directory missing)"
			}
			return nil, &NotFound{input + " does not exist" + suffix}
		}
		return nil, err
	}
	ok, err := allowed(ctx, canonical)
	if err != nil {
		return nil, err
	}
	if !ok {
		roots, _ := workspaceRoots(ctx)
		return nil, &Outside{fmt.Sprintf("%s resolves outside the sandbox, which allows only %s", input, strings.Join(roots, ", "))}
	}
	return &target{
		canonical: canonical,
		display:   ctx.display(canonical),
		check: func() error {
			if err := ctx.checkAbort(); err != nil {
				return err
			}
			again, err := locate()
			parent, perr := realpath(filepath.Dir(canonical))
			if err != nil || perr != nil || again != canonical || parent != filepath.Dir(canonical) {
				return errors.New("Path changed during operation; read again")
			}
			return nil
		},
	}, nil
}

// authorizeWrite lets writes inside a workspace root proceed; anything else needs ctx.Authorize, and is refused without it.
func authorizeWrite(ctx *Context, canonical, action string) error {
	roots, err := workspaceRoots(ctx)
	if err != nil {
		return err
	}
	for _, root := range roots {
		if within(root, canonical) {
			return nil
		}
	}
	if ctx.Authorize == nil {
		return fmt.Errorf("%s is outside the workspace roots (%s); refusing to %s it", canonical, strings.Join(roots, ", "), action)
	}
	return ctx.Authorize(ctx.Ctx, canonical, action)
}

// parent is a target's directory pinned by an open descriptor, addressed through /proc/self/fd.
type parent struct {
	directory string
	location  string
	check     func() error
	sync      func() error
}

func statOf(info os.FileInfo) *syscall.Stat_t { return info.Sys().(*syscall.Stat_t) }

func inParent[T any](t *target, run func(p *parent) (T, error)) (T, error) {
	var zero T
	directory := filepath.Dir(t.canonical)
	handle, err := os.OpenFile(directory, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return zero, err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return zero, err
	}
	pinned := fmt.Sprintf("/proc/self/fd/%d", handle.Fd())
	location := pinned + "/."
	if base := filepath.Base(t.canonical); base != "" && base != "/" {
		location = filepath.Join(pinned, base)
	}
	p := &parent{
		directory: pinned,
		location:  location,
		check: func() error {
			if err := t.check(); err != nil {
				return err
			}
			current, err := os.Lstat(directory)
			if err != nil {
				return err
			}
			link, err := os.Readlink(pinned)
			if statOf(current).Dev != statOf(info).Dev || statOf(current).Ino != statOf(info).Ino || err != nil || link != directory {
				return errors.New("Parent directory changed; read again")
			}
			return nil
		},
		sync: func() error { return handle.Sync() },
	}
	if err := p.check(); err != nil {
		return zero, err
	}
	return run(p)
}

func checkDescriptor(file *os.File, location, canonical string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(location)
	if err != nil {
		return err
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
	if statOf(current).Dev != statOf(info).Dev || statOf(current).Ino != statOf(info).Ino || err != nil || link != canonical {
		return errors.New("File descriptor path changed; read again")
	}
	return nil
}

type publishOptions struct {
	replace  os.FileInfo
	mode     uint32
	hasMode  bool
	validate func() error
}

func randomName() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// processUmask is read once at startup, before any goroutine creates files, since reading it means setting it.
var processUmask = func() int {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return mask
}()

// publish stages bytes beside canonical and renames them over it (replace) or links them into place exclusively.
// Linux pins the parent; neither is a sandbox or external CAS.
func publish(p *parent, canonical string, data []byte, ctx *Context, options publishOptions) (err error) {
	if err := p.check(); err != nil {
		return err
	}
	if err := ctx.checkAbort(); err != nil {
		return err
	}
	name := ".nib-" + randomName() + ".tmp"
	location := filepath.Join(p.directory, name)
	file, err := os.OpenFile(location, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		// Never sweep temp-name patterns or remove a replacement someone else put at our unique name.
		cleanup := func() error {
			current, lerr := os.Lstat(location)
			if lerr != nil {
				if missing(lerr) {
					return nil
				}
				return lerr
			}
			owned, serr := file.Stat()
			if serr != nil {
				return serr
			}
			if statOf(current).Dev == statOf(owned).Dev && statOf(current).Ino == statOf(owned).Ino {
				return os.Remove(location)
			}
			return nil
		}
		cerr := cleanup()
		file.Close()
		if cerr != nil {
			if published {
				err = &wrapped{"File was published, but stage cleanup failed", cerr}
			} else if err == nil {
				err = cerr
			}
			return
		}
		if published && err == nil {
			if serr := p.sync(); serr != nil {
				err = &wrapped{"File was published, but parent directory fsync failed", serr}
			}
		}
	}()
	if err = p.check(); err != nil {
		return err
	}
	stagedCanonical := filepath.Join(filepath.Dir(canonical), name)
	if err = checkDescriptor(file, location, stagedCanonical); err != nil {
		return err
	}
	for written := 0; written < len(data); {
		if err = ctx.checkAbort(); err != nil {
			return err
		}
		n, werr := file.WriteAt(data[written:min(len(data), written+64*1024)], int64(written))
		if werr != nil {
			return werr
		}
		if n == 0 {
			return errors.New("Staged write made no progress")
		}
		written += n
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if options.replace != nil {
		want := statOf(options.replace)
		if want.Uid != statOf(info).Uid || want.Gid != statOf(info).Gid {
			if err = file.Chown(int(want.Uid), int(want.Gid)); err != nil {
				return err
			}
		}
	}
	// Replacement preserves POSIX ownership and mode, not ACLs or extended attributes.
	mode := uint32(0o666 &^ processUmask)
	if options.replace != nil {
		mode = statOf(options.replace).Mode & 0o7777
	} else if options.hasMode {
		mode = options.mode
	}
	if err = file.Chmod(fileMode(mode)); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if options.validate != nil {
		if err = options.validate(); err != nil {
			return err
		}
	}
	if err = p.check(); err != nil {
		return err
	}
	if err = checkDescriptor(file, location, stagedCanonical); err != nil {
		return err
	}
	if err = ctx.checkAbort(); err != nil {
		return err
	}
	if options.replace != nil {
		err = os.Rename(location, p.location)
	} else {
		err = os.Link(location, p.location)
	}
	if err != nil {
		return err
	}
	published = true
	return nil
}

// fileMode turns raw permission bits, setuid, setgid and sticky included, into an os.FileMode.
func fileMode(bits uint32) os.FileMode {
	mode := os.FileMode(bits & 0o777)
	if bits&syscall.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if bits&syscall.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if bits&syscall.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

// changedWhileReading is a file that changed while it was read, such as a log still being written; grep falls back to ripgrep's rows for it.
type changedWhileReading struct{}

func (changedWhileReading) Error() string { return "File changed while reading; read again" }

type held struct {
	bytes []byte
	info  os.FileInfo
}

func snapshot(file *os.File, limit int) (*held, error) {
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("Only regular files are supported")
	}
	if before.Size() > int64(limit) {
		return nil, fmt.Errorf("File exceeds %d byte size limit", limit)
	}
	buffer := make([]byte, before.Size()+1)
	length := 0
	for length < len(buffer) {
		n, rerr := file.ReadAt(buffer[length:], int64(length))
		length += n
		if n == 0 || rerr != nil {
			break
		}
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	b, a := statOf(before), statOf(after)
	if int64(length) != before.Size() || b.Mtim != a.Mtim || b.Ctim != a.Ctim {
		return nil, changedWhileReading{}
	}
	return &held{bytes: buffer[:length], info: after}, nil
}

const bom = "\xef\xbb\xbf"

// text is a decoded file: BOM, body without it, raw lines with their line breaks, and the line ending edits write.
type text struct {
	decoded string
	bom     string
	body    string
	lines   []string
	ending  string
}

func decodeText(data []byte) (*text, error) {
	if len(data) > TextLimit {
		return nil, fmt.Errorf("Text exceeds %d byte size limit", TextLimit)
	}
	if mime(data) != "" {
		return nil, errors.New("Binary attachments cannot be edited as text")
	}
	// ESC stays allowed: terminal colors are common in logs, and rows show such lines with the codes stripped.
	for _, b := range data {
		if b < 9 || (b > 13 && b < 32 && b != 27) || b == 127 {
			return nil, errors.New("Binary files are not supported")
		}
	}
	if !utf8.Valid(data) {
		return nil, errors.New("The encoded data was not valid for encoding utf-8")
	}
	decoded := string(data)
	t := &text{decoded: decoded}
	if strings.HasPrefix(decoded, bom) {
		t.bom = bom
	}
	t.body = decoded[len(t.bom):]
	t.lines = rawLines(t.body)
	t.ending = "\n"
	if strings.Contains(t.body, "\r\n") {
		t.ending = "\r\n"
	}
	return t, nil
}

// rawLines is body.match(/[^\n]*\n|[^\n]+$/g) ?? [""]: lines with their line breaks.
func rawLines(body string) []string {
	if body == "" {
		return []string{""}
	}
	var lines []string
	for body != "" {
		at := strings.IndexByte(body, '\n')
		if at < 0 {
			lines = append(lines, body)
			break
		}
		lines = append(lines, body[:at+1])
		body = body[at+1:]
	}
	return lines
}

func mime(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}):
		return "image/png"
	case len(data) >= 3 && data[0] == 255 && data[1] == 216 && data[2] == 255:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 5 && string(data[:5]) == "%PDF-":
		return "application/pdf"
	}
	return ""
}

// lineContent is raw.replace(/\r?\n$/, ""): a lone trailing CR without a newline stays.
func lineContent(raw string) string {
	if strings.HasSuffix(raw, "\r\n") {
		return raw[:len(raw)-2]
	}
	return strings.TrimSuffix(raw, "\n")
}

func viewOf(source *text) snapshots.View {
	lines := make([]string, len(source.lines))
	for i, raw := range source.lines {
		lines[i] = lineContent(raw)
	}
	return snapshots.NewView(source.body, lines)
}

// wrapped is an error whose message is its own, with the cause kept for errors.Is and errors.As.
type wrapped struct {
	message string
	cause   error
}

func (e *wrapped) Error() string { return e.message }
func (e *wrapped) Unwrap() error { return e.cause }

const nonblock = syscall.O_NONBLOCK
