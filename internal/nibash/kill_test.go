package nibash

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/11ways/nib/internal/platform"
	"github.com/11ways/nib/internal/server"
)

// bashSignals skips a test whose expected text is bash 5.3's on Linux, with signal numbers mips does not share.
func bashSignals(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || strings.HasPrefix(runtime.GOARCH, "mips") {
		t.Skip("the expected text is bash's on Linux")
	}
}

// deadPid is the pid of a program that ran and was reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// sleeper starts a program that runs until the test ends, and returns it with the error its wait gave, once it ended.
func sleeper(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, ended
}

// endedBy says which signal ended a program whose wait gave err, waiting for it at most a few seconds.
func endedBy(t *testing.T, ended <-chan error) syscall.Signal {
	t.Helper()
	select {
	case err := <-ended:
		if exit, ok := err.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return status.Signal()
			}
		}
		t.Fatalf("the program ended without a signal: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the program still runs")
	}
	return 0
}

func TestKillReadsSignalSpecsAsBash(t *testing.T) {
	bashSignals(t)
	for spec, want := range map[string]int{
		"TERM": 15, "term": 15, "Term": 15, "SIGTERM": 15, "sigterm": 15, "Sigterm": 15, "9": 9, " 9": 9, "9 ": 9, "+2": 2, "02": 2, "0": 0,
		"00": 0, "EXIT": 0, "exit": 0, "32": 32, "64": 64, "RTMIN": 34, "rtmin+1": 35, "SIGRTMIN+3": 37, "RTMIN+16": 50, "RTMIN+30": 64,
		"RTMAX": 64, "RTMAX-14": 50, "SIGRTMAX-2": 62, "IO": 29, "PWR": 30, "STKFLT": 16,
	} {
		if sig, ok := decodeSignal(spec); !ok || int(sig) != want {
			t.Errorf("%q: got %d, %v; want %d", spec, sig, ok, want)
		}
	}
	for _, spec := range []string{"", "FOO", "SIG", "SIG0", "SIGEXIT", "65", "-1", "RTMIN+", "RTMIN+x", "RTMIN+31", "RTMIN-1", "RTMAX+0",
		"RTMAX-0", "RTMAX-15", "IOT", "POLL", "CLD", "0x10", "1.5", "DEBUG", "99999999999999999999"} {
		if sig, ok := decodeSignal(spec); ok {
			t.Errorf("%q: decoded as %d", spec, sig)
		}
	}
}

// bashTable is what bash 5.3's kill -l prints on Linux.
const bashTable = " 1) SIGHUP\t 2) SIGINT\t 3) SIGQUIT\t 4) SIGILL\t 5) SIGTRAP\n" +
	" 6) SIGABRT\t 7) SIGBUS\t 8) SIGFPE\t 9) SIGKILL\t10) SIGUSR1\n" +
	"11) SIGSEGV\t12) SIGUSR2\t13) SIGPIPE\t14) SIGALRM\t15) SIGTERM\n" +
	"16) SIGSTKFLT\t17) SIGCHLD\t18) SIGCONT\t19) SIGSTOP\t20) SIGTSTP\n" +
	"21) SIGTTIN\t22) SIGTTOU\t23) SIGURG\t24) SIGXCPU\t25) SIGXFSZ\n" +
	"26) SIGVTALRM\t27) SIGPROF\t28) SIGWINCH\t29) SIGIO\t30) SIGPWR\n" +
	"31) SIGSYS\t34) SIGRTMIN\t35) SIGRTMIN+1\t36) SIGRTMIN+2\t37) SIGRTMIN+3\n" +
	"38) SIGRTMIN+4\t39) SIGRTMIN+5\t40) SIGRTMIN+6\t41) SIGRTMIN+7\t42) SIGRTMIN+8\n" +
	"43) SIGRTMIN+9\t44) SIGRTMIN+10\t45) SIGRTMIN+11\t46) SIGRTMIN+12\t47) SIGRTMIN+13\n" +
	"48) SIGRTMIN+14\t49) SIGRTMIN+15\t50) SIGRTMAX-14\t51) SIGRTMAX-13\t52) SIGRTMAX-12\n" +
	"53) SIGRTMAX-11\t54) SIGRTMAX-10\t55) SIGRTMAX-9\t56) SIGRTMAX-8\t57) SIGRTMAX-7\n" +
	"58) SIGRTMAX-6\t59) SIGRTMAX-5\t60) SIGRTMAX-4\t61) SIGRTMAX-3\t62) SIGRTMAX-2\n" +
	"63) SIGRTMAX-1\t64) SIGRTMAX\t\n"

func TestKillListsSignalsAsBash(t *testing.T) {
	bashSignals(t)
	f := newFixture(t, nil)
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(f.directory + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, command := range []string{"kill -l", "kill -L", "kill -s TERM -l", "kill -l -9", "kill -FOO -l"} {
		if out := f.run(command + " > table.txt; echo $?"); !strings.HasPrefix(out+"\n", "0\n") {
			t.Errorf("%s: %s", command, out)
		}
		if got := read("table.txt"); got != bashTable {
			t.Errorf("%s:\n%q", command, got)
		}
	}
	specs := "15 TERM SIGTERM 143 term 0 999 RTMIN+3 37 sigkill FOO 2 32 33 128 129 300 EXIT SIGEXIT rtmin+1 sigint Int debug SIGDEBUG Err " +
		"return 65 64 191 192 193"
	if out := f.run("kill -l " + specs + " > out.txt 2> err.txt; echo $?"); !strings.HasPrefix(out, "1\n") {
		t.Errorf("status: %s", out)
	}
	expect(t, read("out.txt"), "TERM\n15\n15\nTERM\n15\nEXIT\n37\nRTMIN+3\n9\nINT\nHUP\n0\n35\n2\n2\n65\n66\n67\nRTMAX\nRTMAX-1\nRTMAX\n")
	var invalid strings.Builder
	for _, spec := range []string{"999", "FOO", "128", "300", "SIGEXIT", "SIGDEBUG", "65", "193"} {
		fmt.Fprintf(&invalid, "kill: %s: invalid signal specification\n", spec)
	}
	expect(t, read("err.txt"), invalid.String())
	expect(t, f.run("kill -l -- 9; kill -9 -l 2; kill -L 15 9"), "KILL\nINT\nTERM\nKILL")
}

func TestKillParsesItsOptionsAsBash(t *testing.T) {
	f := newFixture(t, nil)
	dead := deadPid(t)
	gone := fmt.Sprintf("kill: (%d) - No such process", dead)
	for command, want := range map[string]string{
		"kill":                    killUsage + "\n[exit 2]",
		"kill --":                 killUsage + "\n[exit 2]",
		"kill -s TERM":            killUsage + "\n[exit 2]",
		"kill -TERM":              killUsage + "\n[exit 2]",
		"kill -1":                 killUsage + "\n[exit 2]",
		"kill -?":                 killUsage + "\n[exit 2]",
		"kill -s":                 "kill: -s: option requires an argument\n[exit 1]",
		"kill -n":                 "kill: -n: option requires an argument\n[exit 1]",
		"kill -l -s":              "kill: -s: option requires an argument\n[exit 1]",
		"kill -FOO 1":             "kill: FOO: invalid signal specification\n[exit 1]",
		"kill -FOO":               "kill: FOO: invalid signal specification\n[exit 1]",
		"kill -x 1":               "kill: x: invalid signal specification\n[exit 1]",
		"kill -nTERM 1":           "kill: nTERM: invalid signal specification\n[exit 1]",
		"kill -s15 1":             "kill: s15: invalid signal specification\n[exit 1]",
		"kill -sigterm 1":         "kill: igterm: invalid signal specification\n[exit 1]",
		"kill -SIG 1":             "kill: SIG: invalid signal specification\n[exit 1]",
		"kill -65 1":              "kill: 65: invalid signal specification\n[exit 1]",
		"kill -1234":              "kill: 1234: invalid signal specification\n[exit 1]",
		"kill abc":                "kill: `abc': not a pid or valid job spec\n[exit 1]",
		"kill ''":                 "kill: `': not a pid or valid job spec\n[exit 1]",
		"kill 1.5":                "kill: `1.5': not a pid or valid job spec\n[exit 1]",
		"kill 0x10":               "kill: `0x10': not a pid or valid job spec\n[exit 1]",
		"kill 4294967296":         "kill: `4294967296': not a pid or valid job spec\n[exit 1]",
		"kill -- -":               "kill: `-': not a pid or valid job spec\n[exit 1]",
		"kill %1":                 "kill: %1: no such job\n[exit 1]",
		"kill -TERM -TERM DEAD":   "kill: `-TERM': not a pid or valid job spec\n" + gone + "\n[exit 1]",
		"kill -- -s DEAD":         "kill: `-s': not a pid or valid job spec\n" + gone + "\n[exit 1]",
		"kill DEAD":               gone + "\n[exit 1]",
		"kill -9 DEAD":            gone + "\n[exit 1]",
		"kill -sTERM DEAD":        gone + "\n[exit 1]",
		"kill -n9 DEAD":           gone + "\n[exit 1]",
		"kill -n TERM DEAD":       gone + "\n[exit 1]",
		"kill -n ' 15' DEAD":      gone + "\n[exit 1]",
		"kill -Term ' DEAD'":      gone + "\n[exit 1]",
		"kill -RTMIN+1 +DEAD":     gone + "\n[exit 1]",
		"kill -INT -- DEAD -DEAD": gone + "\n" + strings.Replace(gone, "(", "(-", 1) + "\n[exit 1]",
	} {
		if runtime.GOOS != "linux" && strings.Contains(command, "RTMIN") {
			continue
		}
		expect(t, f.run(strings.ReplaceAll(command, "DEAD", fmt.Sprint(dead))), want)
	}
}

func TestKillZeroAsksWhetherAProcessRunsAndSignalsNothing(t *testing.T) {
	f := newFixture(t, nil)
	live, ended := sleeper(t)
	pid, dead := live.Process.Pid, deadPid(t)
	// The kill that ended a session: -0 once read as a process group after the default SIGTERM, which reached kill(0) and nib's client.
	expect(t, f.run(fmt.Sprintf("kill -0 %d; echo $?", pid)), "0")
	expect(t, f.run(fmt.Sprintf("kill -s 0 %d && kill -n 0 %d && kill -EXIT %d && kill -0 -- %d && kill -00 %d && echo running", pid, pid, pid, pid,
		pid)), "running")
	expect(t, f.run(fmt.Sprintf("kill -0 %d 2>/dev/null && echo still || echo stopped", dead)), "stopped")
	expect(t, f.run(fmt.Sprintf("kill -0 %d", dead)), fmt.Sprintf("kill: (%d) - No such process\n[exit 1]", dead))
	select {
	case err := <-ended:
		t.Fatalf("signal 0 ended the program: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	// One target that went is enough for a zero status, as in bash.
	expect(t, f.run(fmt.Sprintf("kill %d %d; echo $?", dead, pid)), fmt.Sprintf("kill: (%d) - No such process\n0", dead))
	if sig := endedBy(t, ended); sig != syscall.SIGTERM {
		t.Errorf("ended by %v", sig)
	}
}

func TestKillAimsAtTheShellAndRefusesTheClient(t *testing.T) {
	id := identities{self: 100, group: 50, client: 40, clientGroup: 30}
	for _, c := range []struct {
		pid     int
		sig     syscall.Signal
		want    aim
		refused bool
	}{
		{0, syscall.SIGTERM, aimShellGroup, false},
		{0, 0, aimShellGroup, false},
		{-50, syscall.SIGKILL, aimShellGroup, false},
		{100, syscall.SIGTERM, aimShell, false},
		{100, 0, aimShell, false},
		{-100, syscall.SIGTERM, aimGroup, false},
		{-1, syscall.SIGTERM, 0, true},
		{-1, syscall.SIGKILL, 0, true},
		{-1, 0, aimProcess, false},
		{40, syscall.SIGTERM, 0, true},
		{40, syscall.SIGHUP, 0, true},
		{40, 0, aimProcess, false},
		{-30, syscall.SIGKILL, 0, true},
		{-30, 0, aimGroup, false},
		{-31, syscall.SIGTERM, aimGroup, false},
		{31, syscall.SIGTERM, aimProcess, false},
		{50, syscall.SIGTERM, aimProcess, false},
	} {
		got, err := aimAt(c.pid, c.sig, id)
		if (err != nil) != c.refused || err == nil && got != c.want {
			t.Errorf("kill -%d %d: got %v, %v", c.sig, c.pid, got, err)
		}
	}
	// A client that shares nib's process group, as Claude Code does, makes that group the shell's.
	if got, err := aimAt(-50, syscall.SIGTERM, identities{self: 100, group: 50, client: 40, clientGroup: 50}); err != nil || got != aimShellGroup {
		t.Errorf("the shared group: %v, %v", got, err)
	}
}

func TestKillOfTheShellEndsTheCall(t *testing.T) {
	f := newFixture(t, nil)
	shell := func(signal string, status int) string {
		return fmt.Sprintf("[kill sent %s to the shell, so nothing after it ran]\n[exit %d]", signal, status)
	}
	group := fmt.Sprint(platform.Group())
	for command, want := range map[string]string{
		"echo a; kill $$; echo b":                  "a\n" + shell("SIGTERM", 143),
		"kill -9 $$; echo b":                       shell("SIGKILL", 137),
		"kill -USR1 $$; echo b":                    shell("SIGUSR1", 138),
		"kill -s INT $$; echo b":                   shell("SIGINT", 130),
		"kill $$ || echo handled":                  shell("SIGTERM", 143),
		"f() { kill $$; echo in; }; f; echo after": shell("SIGTERM", 143),
		// A subshell is no process bash's kill of $$ reaches, so it goes on; the shell ends after it.
		"(kill $$; echo in); echo after":              "in\n" + shell("SIGTERM", 143),
		"echo $(kill $$; echo in); echo after":        shell("SIGTERM", 143),
		"for i in 1 2; do echo $i; kill $$; done":     "1\n" + shell("SIGTERM", 143),
		"echo a | while read l; do kill $$; done; ls": shell("SIGTERM", 143),
		// bash ignores SIGQUIT, and those that end no process; a stopped call would only hang.
		"kill -QUIT $$; echo b": "b",
		"kill -CHLD $$; kill -CONT $$; kill -WINCH $$; kill -URG $$; echo b": "b",
		"kill -STOP $$; kill -TSTP $$; echo b":                               "b",
		"kill -0 $$ && kill -0 0 && kill -0 -- -" + group + " && echo alive": "alive",
		"command kill $$; echo b":                                            shell("SIGTERM", 143),
		"builtin kill -HUP $$; echo b":                                       shell("SIGHUP", 129),
		// The EXIT trap runs as bash runs it before SIGTERM ends it, but not before SIGKILL, which no shell outlives.
		"trap 'echo bye' EXIT; kill $$; echo after":    "bye\n" + shell("SIGTERM", 143),
		"trap 'echo bye' EXIT; kill -9 $$; echo after": shell("SIGKILL", 137),
		"trap 'echo bye' EXIT; kill -PROF $$":          shell("SIGPROF", 155),
	} {
		expect(t, f.run(command), want)
	}
	expect(t, f.run("command -v kill; type kill"), "kill\nkill is a shell builtin")
}

func TestKillZeroEndsTheCallWithWhatItStarted(t *testing.T) {
	f := newFixture(t, nil)
	note := func(signal string, status int) string {
		return fmt.Sprintf("[kill sent %s to the shell's process group, so nothing after it ran]\n[exit %d]", signal, status)
	}
	expect(t, f.run("sleep 30 & echo started; kill 0; echo after"), "started\n"+note("SIGTERM", 143))
	expect(t, waitFor(f, "%1"), "143")
	expect(t, f.run(fmt.Sprintf("sleep 30 & kill -KILL -- -%d; echo after", platform.Group())), note("SIGKILL", 137))
	expect(t, waitFor(f, "%2"), "137")
	expect(t, f.run("(sleep 30; echo late) & kill -s HUP -0; echo after"), note("SIGHUP", 129))
	expect(t, waitFor(f, "%3"), "129")
	// The idiom that ends a script's programs as it exits, which ends bash with 143 too.
	expect(t, f.run("trap 'kill 0' EXIT; sleep 30 & echo started"), "started\n"+note("SIGTERM", 143))
	expect(t, waitFor(f, "%4"), "143")
	// A signal that ends no process reaches the programs and leaves the call running.
	expect(t, f.run("sleep 30 & kill -STOP 0; echo after; kill -CONT 0; kill %5; wait %5; echo $?"), "after\n143")
}

// waitFor waits for the job spec names in a call of its own and returns its status.
func waitFor(f *fixture, spec string) string {
	f.t.Helper()
	return f.quiet("wait " + spec + "; echo $?")
}

// quiet runs command as run does, without the reports of jobs that ended before it that a call opens with.
func (f *fixture) quiet(command string) string {
	f.t.Helper()
	out := f.run(command)
	for strings.HasPrefix(out, "[job %") && strings.Contains(out, " finished, exit ") {
		end := strings.Index(out, "\n")
		if end < 0 {
			return "(no output)"
		}
		out = out[end+1:]
	}
	return out
}

func TestKillOfTheShellFromAJob(t *testing.T) {
	f := newFixture(t, nil)
	start := time.Now()
	// bash would die at once and leave its program running: the call returns, and what it still runs goes on as a job.
	out := f.run("(sleep 0.3; kill $$) & sleep 1; echo after")
	log := f.jobLog(2)
	expect(t, out, "[kill sent SIGTERM to the shell, so nothing after it ran; what it still runs goes on as job %2, output: "+log+"; wait %2 or kill %2]\n[exit 143]")
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Errorf("the call waited for its program, %s", took)
	}
	expect(t, waitFor(f, "%2"), "143")
	if data, err := os.ReadFile(log); err != nil || string(data) != "[kill sent SIGTERM to the shell, so nothing after it ran]\n" {
		t.Errorf("the job's log: %q, %v", data, err)
	}
	// The job of a call that ended has no shell to signal, as bash's would find its parent gone.
	f.run("(sleep 0.3; kill -0 $$ 2>/dev/null; echo alive=$? > status.txt) &")
	expect(t, f.quiet("wait %3; echo $(cat status.txt)"), "alive=1")
}

// jobLog is the log of job id in the fixture's session.
func (f *fixture) jobLog(id int) string {
	f.t.Helper()
	j, _ := f.shell.session("session").jobs.find(fmt.Sprintf("%%%d", id))
	if j == nil {
		f.t.Fatalf("no job %d", id)
	}
	return j.log
}

func TestKillTakesBashsJobSpecs(t *testing.T) {
	f := newFixture(t, nil)
	f.run("sleep 31 & sleep 32 & sleep 33 &")
	expect(t, f.run("kill -0 %1 && kill -0 %% && kill -0 %+ && kill -0 % && kill -0 %- && kill -0 %?32 && kill -0 %sleep\\ 31 && echo held"), "held")
	for command, want := range map[string]string{
		"kill %sleep":   "kill: sleep: ambiguous job spec\n[exit 1]",
		"kill %?3":      "kill: 3: ambiguous job spec\n[exit 1]",
		"kill %?":       "kill: : ambiguous job spec\n[exit 1]",
		"kill %?xyz":    "kill: %?xyz: no such job\n[exit 1]",
		"kill %xyz":     "kill: %xyz: no such job\n[exit 1]",
		"kill %0":       "kill: %0: no such job\n[exit 1]",
		"kill %9":       "kill: %9: no such job\n[exit 1]",
		"wait %sleep":   "wait: sleep: ambiguous job spec\n[exit 127]",
		"kill -- %9 %1": "kill: %9: no such job\n",
	} {
		if strings.HasSuffix(want, "\n") {
			// One target that went is enough for a zero status; this one ends job 1, the others go on.
			expect(t, f.run(command+"; wait %1; echo $?"), want+"143")
			continue
		}
		expect(t, f.run(command), want)
	}
	// Job 1's end was told, so the current job is 3 and the previous one 2.
	expect(t, f.run("kill %-; wait %2; echo $?"), "143")
	expect(t, f.run("kill %%; wait %3; echo $?"), "143")
	// A job that ended is no target, so a wait for it with kill -0 ends.
	expect(t, f.run("kill -0 %1"), "kill: %1: job has terminated\n[exit 1]")
	expect(t, f.run("kill %3"), "kill: %3: job has terminated\n[exit 1]")
	expect(t, f.run("kill %"), "kill: %: no such job\n[exit 1]")
	expect(t, f.run("sleep 0.2 & while kill -0 $! 2>/dev/null; do sleep 0.05; done; echo gone"), "gone")
}

func TestKillOfAJobLeavesAProgramThatHandlesItRunning(t *testing.T) {
	f := newFixture(t, nil)
	// A server that reloads on SIGHUP goes on; once nibash stopped the job's shell instead, with SIGTERM and then SIGKILL.
	f.run(`sh -c 'trap "echo reload" HUP; while :; do sleep 0.05; done' &`)
	time.Sleep(200 * time.Millisecond)
	expect(t, f.run("kill -HUP %1; sleep 0.5; kill -0 %1 && echo running"), "running")
	expect(t, f.run("kill -KILL %1; wait %1; echo $?"), "137")
	if log, err := os.ReadFile(f.jobLog(1)); err != nil || !strings.Contains(string(log), "reload") {
		t.Errorf("the program took no SIGHUP: %q, %v", log, err)
	}
	// A job's shell starts nothing after a signal that ends a process, as bash's subshell would end.
	f.run(`(sh -c 'trap "" TERM; sleep 0.5'; echo next > next.txt) &`)
	time.Sleep(200 * time.Millisecond)
	expect(t, f.run("kill %2; wait %2; echo $?; cat next.txt"), "143\ncat: next.txt: No such file or directory\n[exit 1: cat]")
	// A stopped job gets SIGCONT after SIGTERM, as bash sends it, so the signal can end it.
	f.run("sleep 30 &")
	expect(t, f.run("kill -STOP %3"), "(no output)")
	limit := 5000
	expect(t, f.call(server.ShellCall{Command: "kill %3; wait %3; echo $?", Timeout: &limit}), "143")
	expect(t, f.run("sleep 30 & command kill %4; wait %4; echo $?"), "143")
}
