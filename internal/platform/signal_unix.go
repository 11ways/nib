//go:build unix

package platform

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Realtime is the first and last real-time signal as a glibc program numbers them on Linux, where glibc keeps 32 and 33 for its threads;
// 0 and 0 elsewhere, where kill knows no real-time signal.
func Realtime() (first, last syscall.Signal) {
	if runtime.GOOS != "linux" {
		return 0, 0
	}
	if strings.HasPrefix(runtime.GOARCH, "mips") {
		return 34, 127
	}
	return 34, 64
}

// LastSignal is the highest signal number kill can send.
func LastSignal() syscall.Signal {
	if _, last := Realtime(); last != 0 {
		return last
	}
	var highest syscall.Signal
	for n := syscall.Signal(1); n < 128; n++ {
		if unix.SignalName(n) != "" {
			highest = n
		}
	}
	return highest
}

// SignalName is sig's name without SIG as bash's kill -l prints it, with real-time signals counted from the nearer end (RTMIN+3, RTMAX-2);
// "" for a number the OS does not name.
func SignalName(sig syscall.Signal) string {
	if first, last := Realtime(); first != 0 && sig >= first && sig <= last {
		switch {
		case sig == first:
			return "RTMIN"
		case sig == last:
			return "RTMAX"
		case sig-first <= (last-first)/2:
			return fmt.Sprintf("RTMIN+%d", sig-first)
		}
		return fmt.Sprintf("RTMAX-%d", last-sig)
	}
	return strings.TrimPrefix(unix.SignalName(sig), "SIG")
}

// Ends says whether sig's default action ends a process: not for those that are ignored, continue it or stop it.
func Ends(sig syscall.Signal) bool {
	if sig <= 0 || sig > LastSignal() {
		return false
	}
	switch SignalName(sig) {
	case "CHLD", "CONT", "URG", "WINCH", "INFO", "THR", "LIBRT", "STOP", "TSTP", "TTIN", "TTOU":
		return false
	case "IO":
		// Linux ends a process on SIGIO, the BSDs and macOS ignore it.
		return runtime.GOOS == "linux"
	}
	return true
}

// Group is the process group nib is in, which it shares with the client that started it unless that client gave it one of its own.
func Group() int { return syscall.Getpgrp() }

// GroupOf is the process group pid is in; false when the OS does not say.
func GroupOf(pid int) (int, bool) {
	group, err := syscall.Getpgid(pid)
	return group, err == nil
}

// ContinueGroup sends SIGCONT to the process group pid leads, which a stopped process needs before a signal it holds can end it.
func ContinueGroup(pid int) { _ = SignalGroup(pid, syscall.SIGCONT) }
