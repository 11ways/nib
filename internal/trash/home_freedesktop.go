//go:build unix && !darwin

package trash

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/11ways/nib/internal/platform"
)

// homeTrash is the freedesktop home trash.
func homeTrash() trashDir { return freedesktop(Home(), "") }

// topTrash is the trash of the filesystem path is on, at its top directory: $topdir/.Trash/$uid when $topdir/.Trash is a sticky directory
// and no symlink, else $topdir/.Trash-$uid, which must be the user's own directory if it is there.
func topTrash(path string, homeOnly bool) (trashDir, bool) {
	if homeOnly {
		return trashDir{}, false
	}
	top, ok := mountTop(filepath.Dir(path))
	if !ok {
		return trashDir{}, false
	}
	uid := strconv.Itoa(os.Getuid())
	if info, err := os.Lstat(filepath.Join(top, ".Trash")); err == nil && info.IsDir() && info.Mode()&fs.ModeSticky != 0 {
		return freedesktop(filepath.Join(top, ".Trash", uid), top), true
	}
	own := filepath.Join(top, ".Trash-"+uid)
	info, err := os.Lstat(own)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if os.Mkdir(own, 0o700) != nil {
			return trashDir{}, false
		}
	case err != nil || !info.IsDir() || !platform.Mine(info):
		return trashDir{}, false
	}
	return freedesktop(own, top), true
}

// mountTop is the highest directory above dir on the same device.
func mountTop(dir string) (string, bool) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", false
	}
	dev, ok := platform.Device(dir, info)
	if !ok {
		return "", false
	}
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir, true
		}
		up, err := os.Stat(parent)
		if err != nil {
			return dir, true
		}
		if other, ok := platform.Device(parent, up); !ok || other != dev {
			return dir, true
		}
		dir = parent
	}
}
