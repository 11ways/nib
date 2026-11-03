//go:build windows

package platform

import "syscall"

// stood are the signals Windows can stand for, by ending a process, with their names.
var stood = map[syscall.Signal]string{syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGQUIT: "QUIT", syscall.SIGKILL: "KILL",
	syscall.SIGTERM: "TERM"}

// Realtime is 0 and 0: Windows has no real-time signals.
func Realtime() (first, last syscall.Signal) { return 0, 0 }

// LastSignal is the highest signal number kill can send.
func LastSignal() syscall.Signal { return syscall.SIGTERM }

// SignalName is sig's name without SIG, for the signals Windows can stand for; "" for any other.
func SignalName(sig syscall.Signal) string { return stood[sig] }

// Ends says whether sig ends a process, as each signal Windows can stand for does.
func Ends(sig syscall.Signal) bool {
	_, ok := stood[sig]
	return ok
}

// Group is 0: Windows has no process groups, and nib is in none.
func Group() int { return 0 }

// GroupOf is false: Windows has no process groups.
func GroupOf(int) (int, bool) { return 0, false }

// ContinueGroup does nothing: no process nib starts can be stopped on Windows.
func ContinueGroup(int) {}

// Catchable is false for SIGKILL, which no trap takes.
func Catchable(sig syscall.Signal) bool { return sig != syscall.SIGKILL }

// Stopped is false: no program nib starts can be stopped on Windows.
func Stopped(int) (stopped, known bool) { return false, true }
