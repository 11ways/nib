//go:build linux

package platform

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// cldStopped is the si_code of a child that a signal stopped.
const cldStopped = 5

// Stopped says whether pid, a program nib started, is stopped now, by asking waitid without reaping it or taking the stop's report; known is
// false when the OS does not tell. One that ended is not stopped.
func Stopped(pid int) (stopped, known bool) {
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WSTOPPED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil {
		return false, err == unix.ECHILD
	}
	return info.Signo == int32(syscall.SIGCHLD) && info.Code == cldStopped, true
}
