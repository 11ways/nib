package trash

import (
	"os"
	"path/filepath"

	"github.com/11ways/nib/internal/platform"
)

// homeTrash is the user's ~/.Trash, where Finder shows what nib trashed, with the .trashinfo records in nib's own data folder so the Trash
// holds only the files; with XDG_DATA_HOME set, the freedesktop trash under it, as on other Unix systems.
func homeTrash() trashDir {
	if filepath.IsAbs(os.Getenv("XDG_DATA_HOME")) {
		return freedesktop(Home(), "")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return freedesktop(Home(), "")
	}
	return trashDir{filepath.Join(home, ".Trash"), filepath.Join(platform.DataHome(os.Getenv), "nib", "trash-info"), ""}
}

// topTrash is never taken: macOS keeps another volume's trash in its .Trashes, which Finder manages, so a file there is copied into the home
// trash.
func topTrash(string, bool) (trashDir, bool) { return trashDir{}, false }
