//go:build darwin

package platform

import "golang.org/x/sys/unix"

// sstop is the p_stat of a stopped process.
const sstop = 4

// Stopped says whether pid, a program nib started, is stopped now, by its kernel process record; known is false when the OS does not tell.
// One that ended is not stopped.
func Stopped(pid int) (stopped, known bool) {
	record, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, false
	}
	return record.Proc.P_stat == sstop, true
}
