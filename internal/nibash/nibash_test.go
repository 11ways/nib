package nibash

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/server"
	"github.com/11ways/nib/internal/snapshots"
)

// fixture is a shell session in a temporary directory, with the engine its commands share.
type fixture struct {
	t         *testing.T
	directory string
	engine    *engine.Engine
	shell     *Shell
	tools     func() *engine.Context
	sandbox   bool
	// names, when set, are the named directories each call's tools carry.
	names *names.View
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := engine.New()
	f := &fixture{t: t, directory: directory, engine: e}
	f.tools = func() *engine.Context {
		tools := &engine.Context{Ctx: context.Background(), Directory: directory, Session: "s", Sandbox: f.sandbox}
		if f.names != nil {
			tools.Names = f.names
		}
		return tools
	}
	f.shell = New(e, false)
	t.Cleanup(func() { f.shell.Close("session") })
	return f
}

// run runs one call and returns its text.
func (f *fixture) run(command string) string {
	f.t.Helper()
	return f.call(server.ShellCall{Command: command})
}

func (f *fixture) call(request server.ShellCall) string {
	f.t.Helper()
	result, err := f.shell.Run(context.Background(), "session", request, f.tools())
	if err != nil {
		f.t.Fatalf("%q: %v", request.Command, err)
	}
	return result.Text
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func tagOf(t *testing.T, text string) string {
	t.Helper()
	at := strings.Index(text, "#")
	if at < 0 || at+5 > len(text) {
		t.Fatalf("no tag in %q", text)
	}
	return text[at+1 : at+5]
}

const numbers = "one\ntwo needle\nthree\nfour needle\nfive\n"

func TestFileViewsPrintAnchoredRowsThatEditAccepts(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	out := f.run("grep -n needle a.txt")
	tag := tagOf(t, out)
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n*2:two needle\n...\n*4:four needle", tag))
	// The rows were shown whole, so an edit anchored on them needs no read first.
	result, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 4:\n+four NEEDLE", tag))
	if err != nil {
		t.Fatalf("edit from grep rows: %v", err)
	}
	if !strings.Contains(result.Text, "4:four NEEDLE") {
		t.Fatalf("edit response: %s", result.Text)
	}
	// A line the shell never showed still needs a read.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 1:\n+ONE", tagOf(t, result.Text))); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("an unseen line must be refused, got %v", err)
	}
}

func TestPipelinesOfNibCommandsKeepOriginAndReportFacts(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers, "sub/b.txt": "needle b\n"})
	out := f.run("cat a.txt | head -2")
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n1:one\n2:two needle\n[a.txt: lines 1-2 of 5]", tagOf(t, out)))
	out = f.run("grep -rn needle . | head -1")
	if !strings.Contains(out, "*2:two needle") || !strings.HasSuffix(out, "[head: 1 of 3 matching lines shown, from 1 of 2 files]") {
		t.Fatalf("got %s", out)
	}
	expect(t, f.run("sed -n '2,3p' a.txt | wc -l"), "2")
	out = f.run("tail -n 2 a.txt")
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n4:four needle\n5:five\n[a.txt: lines 4-5 of 5]", tagOf(t, out)))
}

func TestPlainOutputShapedLikeRowsIsSetApartFromAFilesRows(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	// cut makes grep's output a real program's: lines shaped like anchors, which must not read as more of a.txt's rows.
	out := f.run("sed -n 4,5p a.txt; grep -n needle a.txt | cut -c1-20")
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n4:four needle\n5:five\n[plain program output follows, not anchors]\n2:two needle\n4:four needle", tagOf(t, out)))
	// Plain output not shaped like rows needs nothing between.
	out = f.run("sed -n 5p a.txt; echo done")
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n5:five\ndone", tagOf(t, out)))
}

func TestAViewTheStoreDroppedStillAnchors(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n"})
	// A store that holds one file: showing two drops the first one's view, which is still the version its tag names.
	f.engine.Store = snapshots.NewStoreWith(snapshots.Limits{Paths: 1, Versions: 1, Units: 1 << 30})
	out := f.run("cat a.txt b.txt")
	if strings.Contains(out, "changed since") || !strings.Contains(out, "\n1:alpha\n") || !strings.Contains(out, "\n1:beta") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestGrepFollowsGnuFormsBetweenNibCommands(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers, "b.txt": "needle\n"})
	// The text grep passes on carries its prefixes, so a filter after it sees what GNU grep would print.
	expect(t, f.run("grep -n needle a.txt b.txt | grep -c 'b.txt:1:'"), "1")
	expect(t, f.run("grep -c needle a.txt b.txt"), "a.txt:2\nb.txt:1")
	expect(t, f.run("grep -l needle a.txt b.txt"), "a.txt\nb.txt")
	expect(t, f.run("grep -o 'n[a-z]*le' a.txt"), "needle\nneedle")
	expect(t, f.run("grep -q needle a.txt && echo found"), "found")
	expect(t, f.run(`grep 'two\|four' a.txt | wc -l`), "2")
	expect(t, f.run("grep -E 'two|four' a.txt | wc -l"), "2")
	expect(t, f.run("grep -F 'e(' a.txt"), "[exit 1]")
}

func TestOtherFormsRunTheSystemProgram(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	// Into a real program, into a file, inside $(...), with an option nibash lacks, or by path: plain bytes, as bash gives.
	expect(t, f.run("cat a.txt | tr a-z A-Z | head -1"), "ONE")
	expect(t, f.run("echo $(head -1 a.txt)"), "one")
	// A command that would be nib's own but for an option or pattern says why its output is plain, once and only when it printed.
	system := "[head ran as the system program: nib's own does not implement this form of it, so its lines are plain output, not edit anchors]"
	expect(t, f.run("head -c 3 a.txt; head -c 3 a.txt"), "oneone\n"+system)
	expect(t, f.run("grep '\\<one' a.txt"), "one\n[grep ran as the system program: its pattern needs a feature nib's own cannot run exactly, so its lines are plain output, not edit anchors]")
	expect(t, f.run("head -c 0 a.txt"), "(no output)")
	expect(t, f.run("/usr/bin/head -1 a.txt"), "one")
	// An option only an expansion supplies runs the system program, unless the command is joined to another nib command.
	expect(t, f.run("F=-c; head $F 3 a.txt"), "one\n"+system)
	if out := f.run("F=-c; head $F 3 a.txt | wc -l"); !strings.Contains(out, "head: an option nibash does not implement reached it through an expansion") {
		t.Fatalf("got %s", out)
	}
	out := f.run("head -1 a.txt > first.txt; cat first.txt")
	if !strings.Contains(out, "1:one") || !strings.Contains(out, "first.txt#") {
		t.Fatalf("got %s", out)
	}
}

func TestEveryCallStartsFreshExitStatusAndChangedFiles(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	expect(t, f.run("cd /tmp && export FOO=bar"), "(no output)")
	expect(t, f.run("pwd; echo ${FOO-unset}"), f.directory+"\nunset")
	expect(t, f.run("false"), "[exit 1]")
	f.run("cat a.txt")
	out := f.run("echo six >> a.txt")
	if !strings.HasPrefix(out, "[changed by this command: a.txt#") || !strings.Contains(out, "(was #") {
		t.Fatalf("got %s", out)
	}
	out = f.run("cat > typed.txt <<'EOF'\nfirst\nsecond\nEOF")
	tag := tagOf(t, out)
	expect(t, out, fmt.Sprintf("[changed by this command: typed.txt#%s (new)]", tag))
	// A file the model typed whole is known, so a checked edit needs no read.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[typed.txt#%s]\nPUT 2:\n~second\n+SECOND", tag)); err != nil {
		t.Fatalf("edit of typed file: %v", err)
	}
}

func TestAProgramInADeletedDirectoryIsRefusedWithTheReason(t *testing.T) {
	f := newFixture(t, nil)
	out := f.run("mkdir gone && cd gone && rmdir ../gone && mkdir -p " + f.directory + "/made; cd .. 2>/dev/null; echo still")
	if !strings.Contains(out, "nibash: mkdir: not run, since the working directory "+filepath.Join(f.directory, "gone")+" no longer exists; cd to one that does") || !strings.HasSuffix(out, "still") {
		t.Fatalf("got %s", out)
	}
	// The next call starts in the project directory again.
	expect(t, f.run("mkdir -p made && echo ok"), "ok")
}

func TestOutputIsShapedAndCutWithALog(t *testing.T) {
	f := newFixture(t, nil)
	expect(t, f.run(`printf '\033[31mred\033[0m\nx\rdone\n'`), "red\ndone")
	out := f.run("seq 1 5000")
	lines := strings.Split(out, "\n")
	if len(lines) != 301 || lines[0] != "1" || lines[len(lines)-1] != "5000" {
		t.Fatalf("got %d lines: %q ... %q", len(lines), lines[0], lines[len(lines)-1])
	}
	note := lines[100]
	if !strings.HasPrefix(note, "[nibash: 300 rows shown of 5,000 log lines; log lines 101-4800 left out; whole output: ") {
		t.Fatalf("note %q", note)
	}
	log := strings.TrimSuffix(strings.TrimPrefix(note[strings.Index(note, "whole output: "):], "whole output: "), "]")
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "\n") != 5000 {
		t.Fatalf("log %s: %v", log, err)
	}
	out = f.run("seq 1 5000 | tail -2")
	if !strings.HasPrefix(out, "4999\n5000\n[tail: last 2 of 5,000 lines; all of them: ") {
		t.Fatalf("got %s", out)
	}
}

func TestJobsRunDetachedAndReportWhenDone(t *testing.T) {
	f := newFixture(t, nil)
	out := f.run("sleep 0.2 &\necho $!")
	expect(t, out, "%1")
	expect(t, f.run("wait %1; echo waited $?"), "waited 0")
	if out := f.run("echo next"); !strings.HasPrefix(out, "[job %1 finished, exit 0; output: ") {
		t.Fatalf("got %s", out)
	}
	out = f.run("sleep 30 &\nkill $!; wait $!; echo $?")
	expect(t, out, "143")
	out = f.call(server.ShellCall{Command: "echo detached", Background: true})
	if !strings.HasPrefix(out, "[job %2 finished, exit 143; output: ") || !strings.Contains(out, "\n[job %3 started; output: ") {
		t.Fatalf("got %s", out)
	}
	time.Sleep(200 * time.Millisecond)
	f.run("true")
}

func TestATimedOutCommandGoesOnAsAJob(t *testing.T) {
	f := newFixture(t, nil)
	limit := 300
	start := time.Now()
	out := f.call(server.ShellCall{Command: "echo before; sleep 0.8; echo after", Timeout: &limit})
	if time.Since(start) > 2*time.Second || !strings.HasPrefix(out, "before\n[nibash: still running after 300ms; it goes on as job %1, output: ") {
		t.Fatalf("took %s: %s", time.Since(start), out)
	}
	log := strings.TrimSuffix(strings.SplitN(strings.SplitN(out, "output: ", 2)[1], ";", 2)[0], "]")
	expect(t, f.run("wait %1; echo $?"), "0")
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, string(data), "before\nafter\n")
	if out := f.run("true"); !strings.HasPrefix(out, "[job %1 finished, exit 0; output: "+log+"]") {
		t.Fatalf("got %s", out)
	}
}

func TestCallsRunInParallelAndCancellingStopsAWaitingCall(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan string)
	go func() {
		result, _ := f.shell.Run(ctx, "session", server.ShellCall{Command: "sleep 30.125"}, f.tools())
		stopped <- result.Text
	}()
	alive := func(pattern string) bool { return exec.Command("pgrep", "-f", pattern).Run() == nil }
	for deadline := time.Now().Add(2 * time.Second); !alive("^sleep 30.125$"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first call never started its program")
		}
	}
	start := time.Now()
	expect(t, f.run("echo beside"), "beside")
	if time.Since(start) > time.Second {
		t.Fatalf("a call waited %s behind another", time.Since(start))
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not end the call")
	}
	if alive("^sleep 30.125$") {
		t.Fatal("a cancelled call's program is still running")
	}
}

func TestClosingEndsJobsAndCallsThatWentOnAsJobs(t *testing.T) {
	f := newFixture(t, nil)
	limit := 200
	f.call(server.ShellCall{Command: "sleep 30.123 & sleep 30.124", Timeout: &limit})
	alive := func(pattern string) bool { return exec.Command("pgrep", "-f", pattern).Run() == nil }
	if !alive("^sleep 30.123$") || !alive("^sleep 30.124$") {
		t.Fatal("neither the job nor the timed-out program may be stopped")
	}
	f.shell.Close("session")
	time.Sleep(200 * time.Millisecond)
	if alive("^sleep 30.123$") || alive("^sleep 30.124$") {
		t.Fatal("closing the session must end its jobs")
	}
}

func TestSandboxConfinesShellAndPrograms(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	f := newFixture(t, map[string]string{"a.txt": "inside\n"})
	f.sandbox = true
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cat " + outside + "/secret.txt", "/usr/bin/cat " + outside + "/secret.txt", "echo x > " + outside + "/new.txt", "cd " + outside} {
		if out := f.run(command); strings.Contains(out, "secret\n") || !strings.Contains(out, "[exit ") {
			t.Errorf("%s: %s", command, out)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("a write left the sandbox")
	}
	if out := f.run("/usr/bin/cat a.txt"); out != "inside" {
		t.Fatalf("got %s", out)
	}
}

func TestClassifierDecidesFromTheCommandLine(t *testing.T) {
	// Each command line, with the commands that run as nibash's own and how they read.
	cases := map[string][]string{
		"grep -n x a":           {"B:grep"},
		"grep -P x a":           {"S:grep"},
		"grep x a > out":        nil,
		"grep x a | tr a b":     nil,
		"grep x a | head -1":    {"B:grep", "L:head"},
		"echo $(grep x a)":      nil,
		"grep() { :; }; grep x": nil,
		"/bin/grep x a":         nil,
		"head -c 5 a":           {"S:head"},
		"head -c 5 a | wc":      {"B:wc"},
		"sed -n 1,5p a":         {"B:sed"},
		"sed -i s/a/b/ a":       {"S:sed"},
		"cat a |& head":         {"B:head"},
		"cat a | head < b":      {"B:head"},
		"tr a b < f | head -2":  {"B:head"},
	}
	for command, want := range cases {
		file, err := parse(command)
		if err != nil {
			t.Fatal(err)
		}
		classify(file, 0)
		var got []string
		syntax.Walk(file, func(node syntax.Node) bool {
			if lit, ok := node.(*syntax.Lit); ok {
				if rest, found := strings.CutPrefix(lit.Value, marker); found {
					got = append(got, rest)
				}
			}
			return true
		})
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q: nib commands %v, want %v", command, got, want)
		}
	}
}

func parse(command string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
}
