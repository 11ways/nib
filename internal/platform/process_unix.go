//go:build unix

package platform

import (
	"os"
	"os/exec"
	"syscall"
)

// Prepare makes cmd start a process group of its own, which SignalGroup reaches with everything the program starts; Join must follow Start.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Join holds the group of a started cmd; a process group needs no holding.
func Join(*exec.Cmd) error { return nil }

// Release lets go of the group pid leads once its program ended.
func Release(int) {}

// SignalGroup sends sig to the process group pid leads.
func SignalGroup(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }

// Signal sends sig to one process.
func Signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// Signals are the signals kill knows by name.
var Signals = map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT}

// Ignored are the signals nib ignores: a write past the file size limit fails with EFBIG rather than ending the server.
var Ignored = []os.Signal{syscall.SIGXFSZ}
