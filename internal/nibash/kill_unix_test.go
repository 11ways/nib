//go:build unix

package nibash

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killHelper marks the run of the test binary that TestKillOfTheShellsGroupReachesNoProcessBeyondTheCall starts in a process group of its
// own, so a kill that reached the group would end only that run.
const killHelper = "NIB_KILL_HELPER"

func TestKillOfTheShellsGroupReachesNoProcessBeyondTheCall(t *testing.T) {
	if os.Getenv(killHelper) == "1" {
		killShellsGroup(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillOfTheShellsGroupReachesNoProcessBeyondTheCall$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), killHelper+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "survived") {
		t.Fatalf("the run in a group of its own ended with %v:\n%s", err, out)
	}
}

// killShellsGroup sends each kill that names the shell or its process group, which nib's client shares, from a process group of its own.
func killShellsGroup(t *testing.T) {
	f := newFixture(t, nil)
	live, ended := sleeper(t)
	pid, group := live.Process.Pid, syscall.Getpgrp()
	if group != os.Getpid() {
		t.Fatalf("not in a process group of its own: %d", group)
	}
	// The command that ended a session: its kill -0 sent SIGTERM to kill(0), nib's process group and its client's.
	expect(t, f.run(fmt.Sprintf("kill -0 %d 2>/dev/null && echo still || echo stopped", pid)), "still")
	expect(t, f.run(fmt.Sprintf("kill -0 %d; kill -s 0 %d; kill -n 0 %d; kill -0 0; kill -0 $$; kill -0 -- -%d; echo asked", pid, pid, pid, group)),
		"asked")
	note := "[kill sent %s to the shell's process group, so nothing after it ran]\n[exit %d]"
	for command, want := range map[string]string{
		"kill 0; echo after":                             fmt.Sprintf(note, "SIGTERM", 143),
		"kill -9 0; echo after":                          fmt.Sprintf(note, "SIGKILL", 137),
		"kill -- -0; echo after":                         fmt.Sprintf(note, "SIGTERM", 143),
		fmt.Sprintf("kill -KILL -- -%d", group):          fmt.Sprintf(note, "SIGKILL", 137),
		fmt.Sprintf("kill -TERM -%d", group):             fmt.Sprintf(note, "SIGTERM", 143),
		fmt.Sprintf("kill -s HUP -- -%d %d", group, pid): fmt.Sprintf(note, "SIGHUP", 129),
		"kill -INT $$; echo after":                       "[kill sent SIGINT to the shell, so nothing after it ran]\n[exit 130]",
		"kill -9 $$; echo after":                         "[kill sent SIGKILL to the shell, so nothing after it ran]\n[exit 137]",
	} {
		expect(t, f.run(command), want)
	}
	if sig := endedBy(t, ended); sig != syscall.SIGHUP {
		t.Errorf("the program was ended by %v, not by the one kill that named it", sig)
	}
	fmt.Println("survived")
}

func TestKillNeverSignalsNibsClient(t *testing.T) {
	f := newFixture(t, nil)
	client := exec.Command("sleep", "30")
	client.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- client.Wait() }()
	t.Cleanup(func() { _ = client.Process.Kill() })
	pid := client.Process.Pid
	f.shell.identify = func() identities {
		id := currentIdentities()
		id.client, id.clientGroup = pid, pid
		return id
	}
	refused := fmt.Sprintf("kill: (%d) - nibash sends no signal to nib's client: that would end the session\n[exit 1]", pid)
	grouped := fmt.Sprintf("kill: (-%d) - nibash sends no signal to the process group of nib's client: that would end the session\n[exit 1]", pid)
	for command, want := range map[string]string{
		"kill %d":             refused,
		"kill -9 %d":          refused,
		"kill -s HUP %d":      refused,
		"kill -KILL -- -%d":   grouped,
		"kill -TERM -%d":      grouped,
		"kill -0 %d; echo $?": "0",
		"kill -0 -- -%d && kill -EXIT %d && echo asked": "asked",
	} {
		expect(t, f.run(strings.ReplaceAll(command, "%d", fmt.Sprint(pid))), want)
	}
	select {
	case err := <-ended:
		t.Fatalf("the client ended: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}
