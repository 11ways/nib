//go:build unix

package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// NonBlock opens a FIFO without waiting for a writer.
const NonBlock = syscall.O_NONBLOCK

// NoFollow refuses a symbolic link as the last element of the path an open names.
const NoFollow = syscall.O_NOFOLLOW

// OpenFilesBlockReplace is false: a rename replaces a file whatever has it open.
const OpenFilesBlockReplace = false

func stat(info fs.FileInfo) *syscall.Stat_t {
	st, _ := info.Sys().(*syscall.Stat_t)
	return st
}

// ChangeTime is when a file's inode last changed, in nanoseconds; 0 when info carries no such time.
func ChangeTime(info fs.FileInfo) int64 {
	if st := stat(info); st != nil {
		return changeTime(st)
	}
	return 0
}

// Links is how many hard links the open file has; info is its Stat.
func Links(file *os.File, info fs.FileInfo) (uint64, error) {
	if st := stat(info); st != nil {
		return uint64(st.Nlink), nil
	}
	return 0, errors.New("no link count for " + file.Name())
}

// Owner is the user and group that own a file; ok is false where the OS keeps no such pair.
func Owner(info fs.FileInfo) (uid, gid int, ok bool) {
	if st := stat(info); st != nil {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}

// Mine says whether the current user owns a file.
func Mine(info fs.FileInfo) bool {
	st := stat(info)
	return st != nil && int(st.Uid) == os.Getuid()
}

// Device is the filesystem the file at path is on, info its Lstat or Stat; ok is false when it cannot be told.
func Device(_ string, info fs.FileInfo) (uint64, bool) {
	if st := stat(info); st != nil {
		return uint64(st.Dev), true
	}
	return 0, false
}

// Writable says whether the user may write to path, as rm asks before it removes a write-protected file.
func Writable(path string) bool { return syscall.Access(path, 2) == nil }

// CrossDevice says whether err is a rename's refusal to move across filesystems.
func CrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// PipeKey names the pipe an open file is an end of, the same for both its ends; ok is false for anything else.
func PipeKey(file *os.File) (string, bool) {
	info, err := file.Stat()
	if err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		return "", false
	}
	st := stat(info)
	if st == nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), true
}

// Umask is the process's file creation mask, read once at startup, before any goroutine creates files, since reading it means setting it.
var Umask = func() int {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return mask
}()

// SyncDirectory makes a directory's entries durable.
func SyncDirectory(dir *os.File) error { return dir.Sync() }

// RestrictSocket lets only the user connect to the socket at path.
func RestrictSocket(path string) error { return os.Chmod(path, 0o600) }

func userHome(getenv func(string) string) string { return getenv("HOME") }

func userTemp() string { return "nib-" + strconv.Itoa(os.Getuid()) }

func privateDirectory(_ string, info fs.FileInfo) bool {
	return info.Mode().Perm()&0o077 == 0 && Mine(info)
}
