// Package platform is what nib needs from the operating system that each one provides its own way: the facts a file's metadata keeps
// (change time, links, owner, device), open flags, the umask, the folders nib keeps its files in, and the process groups that let a
// timeout or kill end a command with everything it started. Every function has an implementation for Unix and one for Windows, and none
// reads /proc.
package platform

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Home is the user's home folder from getenv, as Go's os.UserHomeDir and Node's os.homedir find it: HOME on Unix, USERPROFILE on Windows;
// "" when it is not set.
func Home(getenv func(string) string) string { return userHome(getenv) }

// xdg is $name when it is an absolute path, else the folder fallback under the home folder; "" without either. Claude Code and OpenCode
// keep their files in dot folders under the home folder on every OS, and nib does the same, honoring the XDG variables wherever they are
// set.
func xdg(getenv func(string) string, name string, fallback ...string) string {
	if dir := getenv(name); filepath.IsAbs(dir) {
		return dir
	}
	base := userHome(getenv)
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, fallback...)...)
}

// ConfigHome is $XDG_CONFIG_HOME, or ~/.config.
func ConfigHome(getenv func(string) string) string { return xdg(getenv, "XDG_CONFIG_HOME", ".config") }

// DataHome is $XDG_DATA_HOME, or ~/.local/share.
func DataHome(getenv func(string) string) string {
	return xdg(getenv, "XDG_DATA_HOME", ".local", "share")
}

// StateHome is $XDG_STATE_HOME, or ~/.local/state.
func StateHome(getenv func(string) string) string {
	return xdg(getenv, "XDG_STATE_HOME", ".local", "state")
}

// CacheHome is $XDG_CACHE_HOME, or ~/.cache.
func CacheHome(getenv func(string) string) string { return xdg(getenv, "XDG_CACHE_HOME", ".cache") }

// TempFolder is a folder of this user's own under the temporary folder, for nib's sockets and logs where no runtime folder is set.
func TempFolder() string { return filepath.Join(os.TempDir(), userTemp()) }

// PrivateDirectory says whether path is a real folder of the current user's own that no other user may enter: on Unix owned by the user
// with no access for group or others, on Windows owned by the user's security identifier.
func PrivateDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().Type() == fs.ModeDir && privateDirectory(path, info)
}

// RawMode is a file's permission bits with setuid, setgid and sticky as the bits chmod takes.
func RawMode(info fs.FileInfo) uint32 {
	mode := info.Mode()
	bits := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// FileMode turns raw permission bits, setuid, setgid and sticky included, into an os.FileMode.
func FileMode(bits uint32) fs.FileMode {
	mode := fs.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}
