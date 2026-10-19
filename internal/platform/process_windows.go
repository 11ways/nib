//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// groups are the job objects of the programs started with Prepare and Join, by process id.
var groups sync.Map

// ntResumeProcess resumes every thread of a process started suspended; Go keeps the main thread's handle to itself, so the process's own
// handle resumes it.
var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// Prepare makes cmd start suspended in a process group of its own; Join, which must follow Start, puts it in a job object and lets it run,
// so every process it starts is in that job too.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED
}

// Join puts a cmd Prepare started in a job object of its own and resumes it, so SignalGroup ends it with everything it starts; on an error
// the program is still suspended, for the caller to kill.
func Join(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	const suspendResume = 0x0800
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|suspendResume, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(process)); status != 0 {
		windows.CloseHandle(job)
		return fmt.Errorf("resuming the program failed (status %#x)", status)
	}
	groups.Store(cmd.Process.Pid, job)
	return nil
}

// Release closes the job object of the program pid once it ended; what it left running goes on, as a Unix process group's members do.
func Release(pid int) {
	if job, ok := groups.LoadAndDelete(pid); ok {
		windows.CloseHandle(job.(windows.Handle))
	}
}

// ending says whether Windows can deliver sig: it has no signals, only termination, which stands for the signals that end a process.
func ending(sig syscall.Signal) error {
	switch sig {
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM:
		return nil
	}
	return fmt.Errorf("signal %d is not supported on Windows", int(sig))
}

// SignalGroup ends the job object pid's program is in, with the exit status a shell gives a process sig ended (128 plus its number).
func SignalGroup(pid int, sig syscall.Signal) error {
	if err := ending(sig); err != nil {
		return err
	}
	if job, ok := groups.Load(pid); ok {
		return windows.TerminateJobObject(job.(windows.Handle), uint32(128+int(sig)))
	}
	return Signal(pid, sig)
}

// Signal ends one process, with the exit status a shell gives a process sig ended.
func Signal(pid int, sig syscall.Signal) error {
	if err := ending(sig); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.TerminateProcess(process, uint32(128+int(sig)))
}

// Signals are the signals kill knows by name: those Windows can stand for by ending a process.
var Signals = map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM}

// Ignored is empty: Windows sends nib no signal it must ignore.
var Ignored []os.Signal
