//go:build unix

package nibash

import (
	"os/exec"
	"testing"
)

func TestProgramsKeepTheSignalsTheShellIgnores(t *testing.T) {
	f := newFixture(t, nil)
	// A job's program ignores the signal as bash's would, and so does the job's shell, which keeps the shell's ignores.
	expect(t, f.quiet("trap '' TERM; sleep 30 & sleep 0.2; kill %1; sleep 0.2; kill -0 %1 && echo kept; kill -9 %1"), "kept")
	// The program sees the name it was called by, not nib's.
	expect(t, f.quiet("trap '' HUP; sh -c 'echo $0'"), "sh")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	expect(t, f.quiet("trap '' HUP INT; bash -c 'trap -p HUP INT TERM'"), "trap -- '' SIGHUP\ntrap -- '' SIGINT")
	expect(t, f.quiet("(trap '' USR1; bash -c 'trap -p USR1'); bash -c 'trap -p USR1'"), "trap -- '' SIGUSR1")
	expect(t, f.quiet("trap '' HUP; trap - HUP; trap 'echo x' INT; bash -c 'trap -p HUP INT'"), "(no output)")
}
