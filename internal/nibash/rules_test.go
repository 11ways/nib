package nibash

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/server"
)

const buildRules = `{"rules": [
  {"program": "zenit-dev", "args": "^(build|test)( |$)", "why": "builds and tests go through the zd_build / zd_test tools"},
  {"program": ["gradlew", "gradle"], "why": "Gradle runs through zenit-dev's MCP tools"}
]}`

// judge is a decision model for rules: broken picks the rules a command breaks, failure makes it answer none.
type judge struct {
	fakeDecider
	broken  func(q decider.RuleQuestion) []int
	failure string
	asked   []decider.RuleQuestion
}

func (j *judge) Breaks(_ context.Context, q decider.RuleQuestion) decider.RuleVerdict {
	j.asked = append(j.asked, q)
	if j.failure != "" {
		return decider.RuleVerdict{Failure: j.failure}
	}
	return decider.RuleVerdict{Broken: j.broken(q), Who: "typesafe/jev-1.14"}
}

func newJudge(broken func(q decider.RuleQuestion) []int) *judge {
	return &judge{fakeDecider: fakeDecider{kind: decider.OpenRouter, choose: func(decider.Input) decider.Choice { return decider.Choice{} }}, broken: broken}
}

func TestRulesRefuseTheBuildLoopOfAnotherCheckout(t *testing.T) {
	f := newFixture(t, map[string]string{"wording/nib.json": buildRules, "wording/a/x": "", "tools/zenit-dev": "#!/bin/sh\necho ran >> ran.txt\n"})
	t.Setenv("HOME", f.directory)
	os.Chmod(filepath.Join(f.directory, "tools/zenit-dev"), 0o755)
	// The call starts where no nib.json holds rules, and cds into a checkout whose rules forbid it, as the agent's loop did.
	command := `cd ~/wording && S=` + f.directory + `/scratch; : > $S/b11.txt; for d in a b; do (cd $d && ~/tools/zenit-dev build > $S/b11-$(basename $d).log 2>&1; echo "$d $?" >> $S/b11.txt); done`
	out := f.call(server.ShellCall{Command: command, Background: true})
	want := "[nothing ran: zenit-dev build is against a rule in " + filepath.Join(f.directory, "wording/nib.json") + ": builds and tests go through the zd_build / zd_test tools]\n[exit 126]"
	if out != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}
	for _, path := range []string{"scratch", "wording/a/ran.txt"} {
		if _, err := os.Stat(filepath.Join(f.directory, path)); err == nil {
			t.Fatalf("%s exists: something ran", path)
		}
	}
	if jobs := f.run("jobs"); jobs != "(no output)" {
		t.Fatalf("a refused background call must start no job: %q", jobs)
	}
}

func TestRulesFindCommandsInsideShellsSubstitutionsAndWrappers(t *testing.T) {
	f := newFixture(t, map[string]string{"nib.json": buildRules})
	for _, command := range []string{
		"bash -c 'cd sub && zenit-dev test --all'",
		"sh -ec \"zenit-dev build\"",
		"echo $(zenit-dev build)",
		"timeout 60 ./gradlew test",
		"env JAVA_HOME=/opt/java nice -n 5 gradle check",
		"eval zenit-dev build",
		"ls | xargs -n1 /opt/zd/zenit-dev test",
	} {
		if out := f.run(command); !strings.HasPrefix(out, "[nothing ran: ") || !strings.HasSuffix(out, "\n[exit 126]") {
			t.Errorf("%s: %q", command, out)
		}
	}
	for _, command := range []string{"echo zenit-dev build", "zenit-dev status", "grep -r gradle .", "git commit -m 'zenit-dev build'"} {
		if out := f.run(command); strings.Contains(out, "nothing ran") {
			t.Errorf("%s must run: %q", command, out)
		}
	}
}

func TestAllowedCommandsPrintWhatTheyDidWithoutRules(t *testing.T) {
	ruled := newFixture(t, map[string]string{"nib.json": buildRules, "a.txt": "one\ntwo\n"})
	plain := newFixture(t, map[string]string{"a.txt": "one\ntwo\n"})
	for _, command := range []string{"echo hi; printf 'x\\ny\\n'", "wc -l a.txt", "false"} {
		if got, want := ruled.run(command), plain.run(command); got != want {
			t.Errorf("%s: %q, without rules %q", command, got, want)
		}
	}
}

func TestVagueRulesAreJudgedByTheDecisionModel(t *testing.T) {
	rules := `{"rules": [{"rule": "Do not run full test suites or long builds directly.", "why": "Use the zenit-dev MCP tools."}, {"rule": "Never push to master."}]}`
	f := newFixture(t, map[string]string{"nib.json": rules})
	j := newJudge(func(q decider.RuleQuestion) []int {
		if strings.Contains(q.Command, "gradle") {
			return []int{0}
		}
		return nil
	})
	f.shell.Deciding(j)
	out := f.call(server.ShellCall{Command: "gradle test", Description: "run every test"})
	want := "[nothing ran: typesafe/jev-1.14 judged it against a rule in " + filepath.Join(f.directory, "nib.json") + ": Do not run full test suites or long builds directly. Use the zenit-dev MCP tools.]\n[exit 126]"
	if out != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}
	q := j.asked[0]
	if q.Command != "gradle test" || q.Purpose != "run every test" || q.Directory != f.directory || len(q.Rules) != 2 || q.Rules[1] != "Never push to master." {
		t.Fatalf("asked %+v", q)
	}
	if out := f.run("echo fine"); out != "fine" {
		t.Fatalf("a command no rule forbids runs as it would: %q", out)
	}
}

func TestVagueRulesUncheckedRunTheCommandAndSaySoOnce(t *testing.T) {
	rules := `{"rules": [{"rule": "Do not run long builds directly."}]}`
	f := newFixture(t, map[string]string{"nib.json": rules})
	path := filepath.Join(f.directory, "nib.json")
	if out := f.run("echo a"); out != "[the vague rules in "+path+" were not checked (no decision model is configured); the command ran]\na" {
		t.Fatalf("without a decision model: %q", out)
	}
	if out := f.run("echo b"); out != "b" {
		t.Fatalf("the note is shown once: %q", out)
	}
	failing := newFixture(t, map[string]string{"nib.json": rules})
	j := newJudge(nil)
	j.failure = "timeout after 30s"
	failing.shell.Deciding(j)
	if out := failing.run("echo c"); !strings.HasPrefix(out, "[the vague rules in ") || !strings.Contains(out, "(timeout after 30s)") || !strings.HasSuffix(out, "\nc") {
		t.Fatalf("a model failure: %q", out)
	}
}

func TestExactRulesNeedNoModelAndNoRulesAskNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"nib.json": buildRules})
	j := newJudge(func(decider.RuleQuestion) []int { return []int{0} })
	f.shell.Deciding(j)
	if out := f.run("gradle build"); !strings.HasPrefix(out, "[nothing ran: gradle build is against a rule") {
		t.Fatalf("got %q", out)
	}
	bare := newFixture(t, nil)
	bare.shell.Deciding(j)
	if out := bare.run("echo x"); out != "x" {
		t.Fatalf("got %q", out)
	}
	if len(j.asked) != 0 {
		t.Fatalf("no vague rule, no question: %+v", j.asked)
	}
}

func TestARefusedCommandReadingANibLogPointsAtSkim(t *testing.T) {
	rules := `{"rules": [{"program": "awk", "why": "no awk here"}, {"rule": "Do not pipe or filter zenit-dev's output."}]}`
	f := newFixture(t, map[string]string{"nib.json": rules})
	f.shell.Deciding(newJudge(func(q decider.RuleQuestion) []int {
		if strings.Contains(q.Command, "sort") {
			return []int{0}
		}
		return nil
	}))
	first, second := f.shell.logs.save("a 1\nb 2\n"), f.shell.logs.save("c 3\n")
	if first.path == "" || second.path == "" {
		t.Fatal("no log was saved")
	}
	hint := func(paths ...string) string {
		return "\n[" + engine.Skim + " " + strings.Join(paths, ", ") + " with an expect answers this without a pipe]\n[exit 126]"
	}
	for command, want := range map[string]string{
		"awk '{print $1}' " + first.path:                          hint(first.path),
		"sort < " + first.path + " | uniq -c; cat " + second.path: hint(first.path, second.path),
	} {
		if out := f.run(command); !strings.HasPrefix(out, "[nothing ran: ") || !strings.HasSuffix(out, want) || strings.Count(out, engine.Skim+" ") != 1 {
			t.Errorf("%s: %q, want it to end %q", command, out, want)
		}
	}
	// A refusal reading no log of nib's gets no pointer.
	if out := f.run("awk '{print $1}' nib.json"); !strings.HasPrefix(out, "[nothing ran: ") || strings.Contains(out, engine.Skim+" ") {
		t.Errorf("got %q", out)
	}
}

func TestAMalformedRulesFileRefusesNamingIt(t *testing.T) {
	for _, content := range []string{`{"rules": [{"program": "gradle"}]}`, `{"rules": [{"program": "x", "rule": "y", "why": "z"}]}`, `{"rules": [{"program": "x", "args": "(", "why": "z"}]}`, `{"rules": `} {
		f := newFixture(t, map[string]string{"nib.json": content})
		out := f.run("echo x")
		if !strings.HasPrefix(out, "[nothing ran: the rules in "+filepath.Join(f.directory, "nib.json")+" cannot be read: ") || !strings.HasSuffix(out, "\n[exit 126]") {
			t.Errorf("%s: %q", content, out)
		}
	}
}

func TestRulesHoldInTheSandbox(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	f := newFixture(t, map[string]string{"nib.json": buildRules})
	f.sandbox = true
	if out := f.run("gradle build"); !strings.HasPrefix(out, "[nothing ran: gradle build is against a rule") {
		t.Fatalf("got %q", out)
	}
	if out := f.run("echo x"); out != "x" {
		t.Fatalf("got %q", out)
	}
}
