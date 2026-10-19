//go:build windows

package platform

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NonBlock is 0: Windows has no FIFOs an open could wait on.
const NonBlock = 0

// NoFollow is 0: Windows opens follow no symbolic link nib makes or meets on its own, and callers check what they opened.
const NoFollow = 0

// OpenFilesBlockReplace is true: a file held open stops a rename over it unless the volume and Windows version give POSIX semantics, so
// whoever replaces a file closes its own handle on it first.
const OpenFilesBlockReplace = true

// ChangeTime is when a file last changed, in nanoseconds: Windows keeps no inode change time a FileInfo carries, so its last write time.
func ChangeTime(info fs.FileInfo) int64 {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.LastWriteTime.Nanoseconds()
	}
	return 0
}

// Links is how many hard links the open file has, as its handle's information gives it.
func Links(file *os.File, _ fs.FileInfo) (uint64, error) {
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &data); err != nil {
		return 0, err
	}
	return uint64(data.NumberOfLinks), nil
}

// Owner reports no pair: Windows ownership is a security identifier that chown cannot set, so nothing preserves it.
func Owner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

// Device is the volume the file at path is on, from its handle's information; info says whether path was Lstat'ed (a link) or followed.
func Device(path string, info fs.FileInfo) (uint64, bool) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if info != nil && info.Mode()&fs.ModeSymlink != 0 {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(handle)
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &data); err != nil {
		return 0, false
	}
	return uint64(data.VolumeSerialNumber), true
}

// Writable says whether path is not marked read-only.
func Writable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().Perm()&0o200 != 0
}

// CrossDevice says whether err is a rename's refusal to move across volumes.
func CrossDevice(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) || errors.Is(err, syscall.EXDEV)
}

// PipeKey names the pipe an open file is an end of: Windows makes every anonymous pipe a named pipe, and both its ends report its name.
func PipeKey(file *os.File) (string, bool) {
	handle := windows.Handle(file.Fd())
	if kind, err := windows.GetFileType(handle); err != nil || kind != windows.FILE_TYPE_PIPE {
		return "", false
	}
	var buffer [1024]uint16
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileNameInfo, (*byte)(unsafe.Pointer(&buffer[0])), uint32(len(buffer)*2)); err != nil {
		return "", false
	}
	// FILE_NAME_INFO is the name's length in bytes, then the name.
	length := *(*uint32)(unsafe.Pointer(&buffer[0])) / 2
	if length == 0 || int(length) > len(buffer)-2 {
		return "", false
	}
	return windows.UTF16ToString(buffer[2 : 2+length]), true
}

// Umask is 0: Windows creates files with no mask.
var Umask = 0

// SyncDirectory does nothing: Windows makes a directory's entries durable with the files' own flushes and cannot flush a directory handle.
func SyncDirectory(*os.File) error { return nil }

// RestrictSocket does nothing: a socket's file takes no permission bits on Windows, and the folder it is in is the user's own.
func RestrictSocket(string) error { return nil }

func userHome(getenv func(string) string) string {
	if dir := getenv("USERPROFILE"); dir != "" {
		return dir
	}
	return getenv("HOME")
}

// userTemp is a plain name: the temporary folder on Windows is the user's own already.
func userTemp() string { return "nib" }

// privateDirectory is whether the folder's owner is the user this process runs as; permission bits mean nothing on Windows.
func privateDirectory(path string, _ fs.FileInfo) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && owner.Equals(user.User.Sid)
}
