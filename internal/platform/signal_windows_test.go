//go:build windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestSignalZeroAsksWhetherAProgramRunsOnWindows(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PLATFORM_TEST_ROLE=child")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := Join(cmd); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer Release(pid)
	if err := Signal(pid, 0); err != nil {
		t.Fatalf("signal 0 to a running program: %v", err)
	}
	if err := SignalGroup(pid, 0); err != nil {
		t.Fatalf("signal 0 to a running program's group: %v", err)
	}
	if stopped, known := Stopped(pid); stopped || !known {
		t.Errorf("Stopped: %v, %v", stopped, known)
	}
	if err := SignalGroup(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := Signal(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("signal 0 to an ended program: %v", err)
	}
	if err := Signal(pid, syscall.Signal(5)); err == nil {
		t.Error("a signal Windows cannot stand for went through")
	}
	for sig, want := range map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL", syscall.Signal(5): ""} {
		if got := SignalName(sig); got != want {
			t.Errorf("SignalName(%d) = %q", sig, got)
		}
	}
	if Catchable(syscall.SIGKILL) || !Catchable(syscall.SIGTERM) || !Ends(syscall.SIGHUP) || Ends(syscall.Signal(5)) {
		t.Error("Catchable or Ends")
	}
}
