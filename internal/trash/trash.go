// Package trash moves what nib deletes into the user's trash, and keeps what this process put there so an agent can list and restore it;
// nothing here ever empties the trash. On Linux and the BSDs that is the freedesktop.org trash, the one file managers and gio read; on
// macOS the user's ~/.Trash, with the freedesktop records beside nib's own files; on Windows a trash in the freedesktop layout under nib's
// data folder, since the Recycle Bin cannot be filled and emptied by entry reliably from outside the shell.
package trash

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/11ways/nib/internal/platform"
)

// ErrExists is a restore onto a path that is there already.
var ErrExists = errors.New("a file is there already")

// ErrNotTrashed is a restore of a path this session did not trash, or whose entry left the trash since.
var ErrNotTrashed = errors.New("not in the trash from this session")

// Entry is one path this process moved to the trash.
type Entry struct {
	// Original is the absolute path it had.
	Original string
	// File is where it is now, under the trash's files directory, and Info its .trashinfo.
	File, Info string
	When       time.Time
	Dir        bool
}

// Can is the trash as this process uses it.
type Can struct {
	mu      sync.Mutex
	entries []Entry
	// rename is os.Rename, which a test makes fail across devices.
	rename func(oldpath, newpath string) error
	now    func() time.Time
}

// New is a can with nothing trashed yet.
func New() *Can { return &Can{rename: os.Rename, now: time.Now} }

// Home is the freedesktop home trash: $XDG_DATA_HOME/Trash, or ~/.local/share/Trash.
func Home() string { return filepath.Join(platform.DataHome(os.Getenv), "Trash") }

// trashDir is a trash: the folders its files and their .trashinfo records go in, and top, the top directory of the filesystem it serves
// ("" for the home trash, whose records hold absolute paths).
type trashDir struct{ files, info, top string }

// freedesktop is the trash in the freedesktop layout at dir.
func freedesktop(dir, top string) trashDir {
	return trashDir{filepath.Join(dir, "files"), filepath.Join(dir, "info"), top}
}

// Isolated points the home trash at a new temporary directory, for a test binary that must never touch the user's; call the result at exit.
func Isolated() func() {
	dir, err := os.MkdirTemp("", "nib-test-data-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_DATA_HOME", dir)
	return func() { os.RemoveAll(dir) }
}

// Put moves what location names to the trash, recording original, its absolute path. A file on another filesystem than the home trash
// goes to that filesystem's own trash, as the specification says, unless homeOnly, as under the sandbox; where neither takes it by
// renaming, it is copied into the home trash and then removed.
func (c *Can) Put(location, original string, homeOnly bool) (Entry, error) {
	return c.PutBy(location, func(target string) error { return c.rename(location, target) }, original, homeOnly)
}

// PutBy is Put with the rename into the trash made by move, as from a directory pinned by its descriptor; location still names the entry,
// for its facts and for a copy across filesystems.
func (c *Can) PutBy(location string, move func(target string) error, original string, homeOnly bool) (Entry, error) {
	info, err := os.Lstat(location)
	if err != nil {
		return Entry{}, err
	}
	home := homeTrash()
	entry, err := c.into(home, location, original, info, move, false)
	if platform.CrossDevice(err) {
		if top, ok := topTrash(original, homeOnly); ok {
			if entry, err = c.into(top, location, original, info, move, false); err == nil {
				return c.keep(entry), nil
			}
		}
		entry, err = c.into(home, location, original, info, move, true)
	}
	if err != nil {
		return Entry{}, err
	}
	return c.keep(entry), nil
}

func (c *Can) keep(entry Entry) Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
	return entry
}

// into moves location into the trash t: its .trashinfo is created first, exclusively, which claims the name, then the file follows, by move,
// or copied and removed when copy is set.
func (c *Can) into(t trashDir, location, original string, info fs.FileInfo, move func(string) error, copy bool) (Entry, error) {
	for _, d := range []string{t.files, t.info} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Entry{}, err
		}
	}
	when := c.now()
	recorded := original
	if t.top != "" {
		recorded = relativeTo(t.top, original)
	}
	body := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", escape(filepath.ToSlash(recorded)), when.Format("2006-01-02T15:04:05"))
	base := filepath.Base(original)
	if base == string(filepath.Separator) || base == "." || base == "" || filepath.VolumeName(base) == base {
		base = "root"
	}
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = base + "." + strconv.Itoa(n)
		}
		infoPath := filepath.Join(t.info, name+".trashinfo")
		target := filepath.Join(t.files, name)
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		handle, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return Entry{}, err
		}
		_, werr := handle.WriteString(body)
		if cerr := handle.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			if copy {
				werr = moveByCopy(location, target)
			} else {
				werr = move(target)
			}
		}
		if werr != nil {
			os.Remove(infoPath)
			return Entry{}, werr
		}
		return Entry{Original: original, File: target, Info: infoPath, When: when, Dir: info.IsDir()}, nil
	}
}

func relativeTo(top, path string) string {
	if rel, err := filepath.Rel(top, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// escape is a path as a .trashinfo holds it: URL-escaped, slashes kept.
func escape(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		ch := path[i]
		if ch == '/' || ch == '-' || ch == '_' || ch == '.' || ch == '~' || 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9' {
			b.WriteByte(ch)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", ch)
	}
	return b.String()
}

// moveByCopy copies a file, directory tree or symlink to target, checks the copy holds every entry at its size, and only then removes the
// original.
func moveByCopy(source, target string) error {
	if err := copyTree(source, target); err != nil {
		os.RemoveAll(target)
		return err
	}
	if err := sameTree(source, target); err != nil {
		os.RemoveAll(target)
		return err
	}
	return os.RemoveAll(source)
}

func copyTree(source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		link, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(link, target)
	case mode.IsDir():
		if err := os.Mkdir(target, mode.Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(source, e.Name()), filepath.Join(target, e.Name())); err != nil {
				return err
			}
		}
		if err := os.Chmod(target, mode.Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	case mode.IsRegular():
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	default:
		return fmt.Errorf("%s cannot be copied to the trash", info.Mode().Type())
	}
}

// sameTree checks that target holds every entry of source, of the same kind and, for files, the same size.
func sameTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		a, err := os.Lstat(path)
		if err != nil {
			return err
		}
		b, err := os.Lstat(filepath.Join(target, rel))
		if err != nil {
			return fmt.Errorf("the copy in the trash lacks %s", rel)
		}
		if a.Mode().Type() != b.Mode().Type() || a.Mode().IsRegular() && a.Size() != b.Size() {
			return fmt.Errorf("the copy in the trash differs at %s", rel)
		}
		return nil
	})
}

// Entries are the paths this process trashed that are still in the trash, oldest first.
func (c *Can) Entries() []Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = slices.DeleteFunc(c.entries, func(e Entry) bool {
		_, ferr := os.Lstat(e.File)
		_, ierr := os.Lstat(e.Info)
		return ferr != nil || ierr != nil
	})
	return slices.Clone(c.entries)
}

// Restore puts the latest entry trashed from original back there, making its parent directories again; a file now at original is refused
// with ErrExists, or with force goes to the trash itself first.
func (c *Can) Restore(original string, force, homeOnly bool) (Entry, error) {
	var found *Entry
	for _, e := range c.Entries() {
		if e.Original == original {
			found = &e
		}
	}
	if found == nil {
		return Entry{}, ErrNotTrashed
	}
	if _, err := os.Lstat(original); err == nil {
		if !force {
			return Entry{}, ErrExists
		}
		if _, err := c.Put(original, original, homeOnly); err != nil {
			return Entry{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(original), 0o777); err != nil {
		return Entry{}, err
	}
	err := c.rename(found.File, original)
	if platform.CrossDevice(err) {
		err = moveByCopy(found.File, original)
	}
	if err != nil {
		return Entry{}, err
	}
	os.Remove(found.Info)
	c.mu.Lock()
	c.entries = slices.DeleteFunc(c.entries, func(e Entry) bool { return e.File == found.File })
	c.mu.Unlock()
	return *found, nil
}
