//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOutliveTakesASignalItsProgramsGetAtItsDefault(t *testing.T) {
	// SIGUSR2 stands for SIGXFSZ, which the test may inherit ignored from its own parent and then leaves so.
	if signal.Ignored(syscall.SIGUSR2) {
		t.Skip("SIGUSR2 is ignored where the test runs")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	outlive(syscall.SIGUSR2)
	defer signal.Reset(syscall.SIGUSR2)
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	out, err := exec.Command("bash", "-c", "trap -p USR2; kill -USR2 $$; echo survived").CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGUSR2 || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a program nib starts got SIGUSR2 ignored: %q, %v", out, err)
	}
}
