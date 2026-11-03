package nibash

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/11ways/nib/internal/platform"
	"github.com/11ways/nib/internal/server"
)

func TestTrapTakesBashsSpecsAndPrintsAsBash(t *testing.T) {
	f := newFixture(t, nil)
	for command, want := range map[string]string{
		`trap "echo x" INT TERM; trap -p`:             "trap -- 'echo x' SIGINT\ntrap -- 'echo x' SIGTERM",
		`trap "echo a" int; trap`:                     "trap -- 'echo a' SIGINT",
		`trap -- "echo b" 0 SIGHUP; trap -p EXIT HUP`: "trap -- 'echo b' EXIT\ntrap -- 'echo b' SIGHUP\nb",
		`trap "echo 'quoted'" EXIT; trap -p EXIT`:     "trap -- 'echo '\\''quoted'\\''' EXIT\nquoted",
		`trap FOO`:                              trapUsage + "\n[exit 2]",
		`trap -`:                                trapUsage + "\n[exit 2]",
		`trap "x" FOO TERM; echo s=$?; trap -p`: "trap: FOO: invalid signal specification\ns=1\ntrap -- 'x' SIGTERM",
		`trap -p FOO`:                           "trap: FOO: invalid signal specification\n[exit 1]",
		`trap - TERM; trap "" INT; trap -p`:     "trap -- '' SIGINT",
		`trap "x" TERM; trap TERM; trap 1; trap -p`: "(no output)",
		`trap -z`:                                "trap: -z: invalid option\n" + trapUsage + "\n[exit 2]",
		`trap -P`:                                "trap: -P requires at least one signal name\n[exit 2]",
		`trap "echo hi" TERM; trap -P TERM INT`:  "echo hi",
		`trap "x" KILL STOP; trap -p`:            "trap -- 'x' SIGKILL\ntrap -- 'x' SIGSTOP",
		`trap "a" DEBUG`:                         "trap: DEBUG: nibash runs no DEBUG or RETURN trap\n[exit 1]",
		`trap "a" return`:                        "trap: return: nibash runs no DEBUG or RETURN trap\n[exit 1]",
		`trap "" ""`:                             "trap: : invalid signal specification\n[exit 1]",
		`trap "x" -- TERM`:                       "trap: --: invalid signal specification\n[exit 1]",
		`trap 'echo err' ERR; false; echo after`: "err\nafter",
		`trap 'echo err' ERR; trap - ERR; false; echo ok`:  "ok",
		`trap "" EXIT; trap -p; trap - EXIT; trap -p EXIT`: "trap -- '' EXIT",
		// A subshell shows the shell's traps until it sets its own, which stay its own.
		`trap "echo one" TERM; (trap -p; trap "echo two" TERM; trap -p TERM); trap -p TERM`: "trap -- 'echo one' SIGTERM\ntrap -- 'echo two' SIGTERM\ntrap -- 'echo one' SIGTERM",
		`command trap "echo c" TERM; builtin trap -p TERM`:                                  "trap -- 'echo c' SIGTERM",
	} {
		expect(t, f.quiet(command), want)
	}
	if out := f.run("trap -l"); !strings.HasPrefix(out, " 1) SIGHUP\t 2) SIGINT\t") {
		t.Errorf("trap -l: %q", out)
	}
}

func TestASubshellRunsTheExitTrapItSetAsItEnds(t *testing.T) {
	f := newFixture(t, nil)
	for command, want := range map[string]string{
		`(trap "echo bye \$?" EXIT; false); echo "after $?"`:                    "bye 1\nafter 1",
		`(trap "echo bye \$?" EXIT; exit 3); echo "after $?"`:                   "bye 3\nafter 3",
		`x=$(trap "echo inner >&2" EXIT; echo hi); echo "x=$x"`:                 "inner\nx=hi",
		`trap "echo main" EXIT; (echo sub); (trap -p EXIT); echo end`:           "sub\ntrap -- 'echo main' EXIT\nend\nmain",
		`(trap "echo once" EXIT; f() { exit 4; }; f; echo no); echo "after $?"`: "once\nafter 4",
		`(trap "" EXIT; exit 5); echo $?`:                                       "5",
		`(exit 300); echo $?`:                                                   "44",
	} {
		expect(t, f.quiet(command), want)
	}
}

func TestTrapsTakeTheSignalsAKillSendsTheShell(t *testing.T) {
	f := newFixture(t, nil)
	for command, want := range map[string]string{
		"trap 'echo caught' TERM; kill $$; echo after":                                  "caught\nafter",
		"trap 'echo caught; exit 3' TERM; kill $$; echo after":                          "caught\n[exit 3]",
		"trap '' TERM; kill $$; echo after":                                             "after",
		"trap 'echo caught' TERM; kill $$":                                              "caught",
		"false; trap 'echo status=$?' USR1; kill -USR1 $$; echo after":                  "status=0\nafter",
		"trap 'echo int' INT; trap 'echo term' TERM; kill -INT $$; kill $$; echo after": "int\nterm\nafter",
		"trap 'echo quit' QUIT; kill -QUIT $$; echo after":                              "quit\nafter",
		"trap 'echo exit' EXIT; trap 'echo term' TERM; kill $$":                         "term\nexit",
		"trap 'echo x' TERM; trap - TERM; kill $$; echo after":                          "[kill sent SIGTERM to the shell, so nothing after it ran]\n[exit 143]",
		// No trap takes SIGKILL.
		"trap 'echo x' KILL; kill -9 $$; echo after": "[kill sent SIGKILL to the shell, so nothing after it ran]\n[exit 137]",
		// bash runs a trap once the foreground program ends, as a signal reaches it while the program runs.
		"trap 'echo late' TERM; (sleep 0.2; kill $$) & sleep 0.5; echo after": "late\nafter",
	} {
		expect(t, f.quiet(command), want)
	}
	// A trapped kill 0 still ends the jobs, which are shells of their own, whose traps bash's subshells do not keep.
	expect(t, f.quiet("trap 'echo caught' TERM; sleep 30 & kill 0; echo after"), "caught\nafter")
	expect(t, waitFor(f, "%2"), "143")
	// A wait in the shell returns at once with 128 plus the signal's number, and then the trap runs.
	expect(t, f.quiet("trap 'echo caught' TERM; sleep 30 & (sleep 0.2; kill $$) & wait %3; echo status=$?; kill %3"), "caught\nstatus=143")
}

func TestJobsListsBashsFormsInNibashs(t *testing.T) {
	f := newFixture(t, nil)
	f.run("sleep 31 & sleep 32 &")
	time.Sleep(100 * time.Millisecond)
	logs := []string{f.jobLog(1), f.jobLog(2)}
	expect(t, f.quiet("jobs"), fmt.Sprintf("[1]- Running  sleep 31 &  (output: %s)\n[2]+ Running  sleep 32 &  (output: %s)", logs[0], logs[1]))
	expect(t, f.quiet("jobs -p"), "%1\n%2")
	pid := f.run("jobs -x echo %1 %9")
	if !regexp.MustCompile(`^\d+ %9$`).MatchString(pid) {
		t.Fatalf("jobs -x: %q", pid)
	}
	group, _ := strconv.Atoi(strings.Fields(pid)[0])
	expect(t, f.quiet("jobs -l %1"), fmt.Sprintf("[1]- %d Running  sleep 31 &  (output: %s)", group, logs[0]))
	expect(t, f.quiet("kill -STOP %1; sleep 0.1; jobs -s; jobs -r -p"), fmt.Sprintf("[1]- Stopped  sleep 31 &  (output: %s)\n%%2", logs[0]))
	for command, want := range map[string]string{
		"jobs %9":    "jobs: %9: no such job\n[exit 1]",
		"jobs -z":    "jobs: -z: invalid option\n" + jobsUsage + "\n[exit 2]",
		"jobs -lz":   "jobs: -z: invalid option\n" + jobsUsage + "\n[exit 2]",
		"jobs -n":    "(no output)",
		"jobs -x":    "(no output)",
		"jobs -- %2": fmt.Sprintf("[2]+ Running  sleep 32 &  (output: %s)", logs[1]),
	} {
		expect(t, f.quiet(command), want)
	}
	// kill $(jobs -p) ends every job, as in bash; a listing of a job that ended tells its end, so no call reports it again.
	expect(t, f.quiet("kill $(jobs -p); wait; echo $?"), "0")
	expect(t, f.quiet("jobs -n"), "(no output)")
	expect(t, f.quiet("jobs"), fmt.Sprintf("[1]  Done (exit 143)  sleep 31 &  (output: %s)\n[2]  Done (exit 143)  sleep 32 &  (output: %s)", logs[0], logs[1]))
	f.run("true &")
	time.Sleep(100 * time.Millisecond)
	if out := f.run("jobs -n"); !strings.HasPrefix(out, "[job %3 finished, exit 0") && !strings.HasPrefix(out, "[3]+ Done (exit 0)") {
		t.Errorf("jobs -n: %q", out)
	}
}

func TestWaitTakesPidsAndBashsOptions(t *testing.T) {
	f := newFixture(t, nil)
	for command, want := range map[string]string{
		"false & wait; echo $?":       "0",
		"(exit 3) & wait $!; echo $?": "3",
		"(exit 4) & wait -n; echo $?": "4",
		"wait -n; echo $?":            "127",
		"wait 1; echo $?":             "wait: pid 1 is not a child of this shell\n127",
		"wait x; echo $?":             "wait: `x': not a pid or valid job spec\n127",
		"wait -p":                     "wait: -p: option requires an argument\n" + waitUsage + "\n[exit 2]",
		"wait -p 1x":                  "wait: `1x': not a valid identifier\n[exit 1]",
		"wait -z":                     "wait: -z: invalid option\n" + waitUsage + "\n[exit 2]",
		"wait %9; echo $?":            "wait: %9: no such job\n127",
		"unset id; wait -p id 1; echo ${id-unset} $?": "wait: pid 1 is not a child of this shell\nunset 127",
		"(exit 5) & wait -fp id $!; echo $id $?":      "%%N 5",
		"(exit 6) & wait -n -p id; echo $id $?":       "%%N 6",
	} {
		out := f.run(command)
		if strings.Contains(want, "%%N") {
			out = regexp.MustCompile(`%\d+`).ReplaceAllString(out, "%%N")
		}
		expect(t, out, want)
	}
	// A pid names a program of a job, as jobs -l lists it, until it ended and after.
	expect(t, f.quiet("sleep 0.3 & sleep 0.1; p=$(jobs -x echo %%); [ -n \"${p##*[!0-9]*}\" ] && wait $p; echo $?; wait $p; echo $?"), "0\n0")
	expect(t, f.quiet("sleep 5 & sleep 0.1; p=$(jobs -x echo %%); kill $p; wait $p; echo $?"), "143")
}

func TestAWaitWaitsOnlyForTheJobsItsShellStarted(t *testing.T) {
	f := newFixture(t, nil)
	// A job's wait once waited for every job of the session, its own included, and so for ever.
	out := f.call(server.ShellCall{Command: "sleep 0.2 & echo started=$!; wait; echo waited; jobs", Background: true})
	log := out[strings.Index(out, "output: ")+len("output: ") : len(out)-1]
	expect(t, f.quiet("wait %1; echo $?"), "0")
	if data, err := os.ReadFile(log); err != nil || !regexp.MustCompile(`^started=%2\nwaited\n\[2\]  Done \(exit 0\)  sleep 0\.2 &  \(output: \S+\)\n$`).Match(data) {
		t.Errorf("the job's log: %q, %v", data, err)
	}
	// A job's shell knows the jobs it started and no others, as a bash subshell starts with an empty table.
	f.run("sleep 30 &")
	expect(t, f.quiet("(jobs; wait; kill %3 2>&1; sleep 0.1 & wait; echo inner) & wait %4; echo $?"), "0")
	if data, err := os.ReadFile(f.jobLog(4)); err != nil || string(data) != "kill: %3: no such job\ninner\n" {
		t.Errorf("job 4's log: %q, %v", data, err)
	}
	expect(t, f.quiet("kill %3; wait %3; echo $?"), "143")
}

func TestAmpersandStartsANibashJobWhereverItStands(t *testing.T) {
	f := newFixture(t, nil)
	// In a function, with its local variables, as bash forks the shell where & runs.
	expect(t, f.quiet("f() { local d=0.1; sleep $d & echo started $!; }; f; wait %1; echo $?"), "started %1\n0")
	expect(t, f.quiet("echo $(sleep 0.1 & echo in $!); wait %2; echo $?"), "in %2\n0")
	expect(t, f.quiet("(sleep 0.1 & wait $!; echo nested $?) & wait %3; echo $?"), "0")
	if data, err := os.ReadFile(f.jobLog(3)); err != nil || string(data) != "nested 0\n" {
		t.Errorf("job 3's log: %q, %v", data, err)
	}
	expect(t, f.quiet("jobs -p"), "%1\n%2\n%3\n%4")
}

func TestDisownLetsAJobOutliveTheSession(t *testing.T) {
	f := newFixture(t, nil)
	for command, want := range map[string]string{
		"disown":    "disown: current: no such job\n[exit 1]",
		"disown %9": "disown: %9: no such job\n[exit 1]",
		"disown 1":  "disown: 1: no such job\n[exit 1]",
		"disown -z": "disown: -z: invalid option\ndisown: usage: disown [-h] [-ar] [jobspec ... | pid ...]\n[exit 2]",
	} {
		expect(t, f.quiet(command), want)
	}
	gone := f.run("sleep 30 & sleep 0.1; jobs -x echo %%; disown; jobs; kill %1 2>&1")
	lines := strings.Split(gone, "\n")
	kept := f.run("sleep 30 & sleep 0.1; jobs -x echo %%; disown -h %2; jobs -p")
	ended := f.run("sleep 30 & sleep 0.1; jobs -x echo %%")
	if len(lines) < 2 || lines[1] != "kill: %1: no such job" || !strings.HasSuffix(kept, "\n%2") {
		t.Fatalf("disowned: %q; kept: %q", gone, kept)
	}
	pids := []string{lines[0], strings.Split(kept, "\n")[0], ended}
	f.shell.Close("session")
	time.Sleep(200 * time.Millisecond)
	for i, text := range pids {
		pid, err := strconv.Atoi(text)
		if err != nil {
			t.Fatalf("pid %q", text)
		}
		alive := platform.Signal(pid, 0) == nil
		if alive != (i < 2) {
			t.Errorf("program %d alive after the session's end: %v", i, alive)
		}
		_ = platform.Signal(pid, syscall.SIGKILL)
	}
}

func TestJobControlBuiltinsSayThereIsNone(t *testing.T) {
	f := newFixture(t, nil)
	expect(t, f.quiet("fg; echo $?; bg %1; echo $?; suspend; echo $?"), "fg: no job control\n1\nbg: no job control\n1\nsuspend: cannot suspend: no job control\n1")
}
