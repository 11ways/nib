//go:build unix

package platform

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignalNamesAndTheRealtimeRangeAreTheSystemShells(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	out, err := exec.Command("bash", "-c", "for n in $(seq 1 "+strconv.Itoa(int(LastSignal()))+"); do echo \"$n $(kill -l $n)\"; done; kill -l RTMIN RTMAX 2>/dev/null").Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines[:LastSignal()] {
		number, name, _ := strings.Cut(line, " ")
		n, _ := strconv.Atoi(number)
		if got := SignalName(syscall.Signal(n)); got != name {
			t.Errorf("signal %d: %q, bash names it %q", n, got, name)
		}
	}
	if first, last := Realtime(); first != 0 {
		if want := lines[len(lines)-2:]; strconv.Itoa(int(first)) != want[0] || strconv.Itoa(int(last)) != want[1] {
			t.Errorf("real-time signals %d to %d, bash's %v", first, last, want)
		}
	}
}

func TestEndsAndCatchableFollowEachSignalsDefault(t *testing.T) {
	for sig, want := range map[syscall.Signal]bool{syscall.SIGTERM: true, syscall.SIGKILL: true, syscall.SIGQUIT: true, syscall.SIGHUP: true,
		syscall.SIGUSR1: true, syscall.SIGPIPE: true, syscall.SIGCHLD: false, syscall.SIGCONT: false, syscall.SIGSTOP: false, syscall.SIGTSTP: false,
		syscall.SIGWINCH: false, syscall.SIGURG: false, 0: false} {
		if Ends(sig) != want {
			t.Errorf("Ends(%v) = %v", sig, !want)
		}
	}
	if first, _ := Realtime(); first != 0 && !Ends(first) {
		t.Error("a real-time signal ends a process by default")
	}
	if runtime.GOOS == "linux" && !Ends(syscall.Signal(29)) {
		t.Error("SIGIO ends a process on Linux")
	}
	for sig, want := range map[syscall.Signal]bool{syscall.SIGKILL: false, syscall.SIGSTOP: false, syscall.SIGTERM: true, syscall.SIGTSTP: true} {
		if Catchable(sig) != want {
			t.Errorf("Catchable(%v) = %v", sig, !want)
		}
	}
	if group, ok := GroupOf(os.Getpid()); !ok || group != Group() {
		t.Errorf("GroupOf(nib) = %d, %v; Group() = %d", group, ok, Group())
	}
}

func TestStoppedTellsWhetherAChildIsStopped(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill() }()
	if _, known := Stopped(pid); !known {
		t.Skip("this OS does not tell")
	}
	settles := func(want bool) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if stopped, _ := Stopped(pid); stopped == want {
				return
			}
		}
		t.Fatalf("Stopped never became %v", want)
	}
	settles(false)
	_ = cmd.Process.Signal(syscall.SIGSTOP)
	settles(true)
	// Asking again finds it still stopped: the ask takes no report the program's waiter needs.
	settles(true)
	_ = cmd.Process.Signal(syscall.SIGCONT)
	settles(false)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if stopped, known := Stopped(pid); stopped || !known {
		t.Errorf("an ended program: %v, %v", stopped, known)
	}
}
