package setup

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/11ways/nib/mods"
)

// pluginDirsVariable is the environment variable Claude Code loads plugin folders from, read from a user settings file's env block too.
const pluginDirsVariable = "CLAUDE_CODE_PLUGIN_DIRS"

// modFolder is where setup installs the nibash mod: nib's folder in the user's data home.
func modFolder(home string, env func(string) string) string {
	data := env("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "nib", "mods", "nibash")
}

// modFiles are the nibash mod's files as this nib carries them, by path below the mod's folder.
func modFiles() (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(mods.Nibash, "nibash", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := mods.Nibash.ReadFile(path)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(path, "nibash/")] = data
		return nil
	})
	return files, err
}

// sameFiles is true when folder holds exactly files, the folder the engine lays its types in aside.
func sameFiles(folder string, files map[string][]byte) bool {
	found := 0
	err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(folder, path)
		if entry.IsDir() {
			if relative == filepath.Join(".claude-plugin", "types") {
				return filepath.SkipDir
			}
			return nil
		}
		want, ok := files[filepath.ToSlash(relative)]
		data, readErr := os.ReadFile(path)
		if !ok || readErr != nil || !bytes.Equal(data, want) {
			return fs.ErrInvalid
		}
		found++
		return nil
	})
	return err == nil && found == len(files)
}

// installMod puts this nib's nibash mod in folder, replacing an older one whole: written beside it first, then swapped in, so Claude Code
// never loads half of one. installed is false when folder already held it.
func installMod(folder string, dryRun bool) (installed bool, err error) {
	files, err := modFiles()
	if err != nil {
		return false, err
	}
	if sameFiles(folder, files) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	staged := folder + ".new-" + hex.EncodeToString(suffix)
	for path, data := range files {
		target := filepath.Join(staged, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return false, err
		}
	}
	old := folder + ".old-" + hex.EncodeToString(suffix)
	if err := os.Rename(folder, old); err != nil && !os.IsNotExist(err) {
		os.RemoveAll(staged)
		return false, err
	}
	if err := os.Rename(staged, folder); err != nil {
		_ = os.Rename(old, folder)
		os.RemoveAll(staged)
		return false, err
	}
	os.RemoveAll(old)
	return true, nil
}

// addPluginDir lists folder in a user settings file's env.CLAUDE_CODE_PLUGIN_DIRS, keeping the folders already there.
func addPluginDir(json object, folder string) ([]change, error) {
	env, err := objectAt(json, "env")
	if err != nil {
		return nil, err
	}
	value, _ := env.get(pluginDirsVariable)
	current, ok := value.(string)
	if value != nil && !ok {
		return nil, fmt.Errorf(`its "env.%s" is not a string`, pluginDirsVariable)
	}
	var folders []string
	for _, listed := range filepath.SplitList(current) {
		if listed == folder {
			return nil, nil
		}
		if listed != "" {
			folders = append(folders, listed)
		}
	}
	env.set(pluginDirsVariable, strings.Join(append(folders, folder), string(filepath.ListSeparator)))
	json.set("env", env)
	return adds("the nibash mod in env." + pluginDirsVariable), nil
}
