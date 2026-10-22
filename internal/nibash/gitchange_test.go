package nibash

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// remoteFixture is a gitFixture whose main branch tracks a bare remote, and other, a second clone of that remote, which pushes to it as
// another person would.
func remoteFixture(t *testing.T, files map[string]string) (f *fixture, other string) {
	t.Helper()
	f = gitFixture(t, files)
	remote := filepath.Join(t.TempDir(), "remote.git")
	f.git("init", "-q", "--bare", "-b", "main", remote)
	f.git("remote", "add", "origin", remote)
	f.git("push", "-q", "-u", "origin", "main")
	other = filepath.Join(t.TempDir(), "other")
	f.git("clone", "-q", remote, other)
	return f, other
}

// gitAt runs git in dir, outside nibash.
func gitAt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeAt writes a file below dir, making its folders.
func writeAt(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// diverged leaves f's main and the remote's each with a commit of their own, both changing the line of a.txt.
func diverged(t *testing.T) *fixture {
	f, other := remoteFixture(t, map[string]string{"a.txt": "one\n"})
	writeAt(t, other, "a.txt", "theirs\n")
	gitAt(t, other, "commit", "-qam", "theirs")
	gitAt(t, other, "push", "-q")
	f.write("a.txt", "ours\n")
	f.git("commit", "-qam", "ours")
	return f
}

func TestGitCommitSummaryNamesEachFolderOnceAndCreatedFilesBriefly(t *testing.T) {
	f := gitFixture(t, map[string]string{"a.txt": "a\n", "run.sh": "true\n"})
	for _, name := range []string{"A", "B", "C"} {
		f.write(operation+name+".java", "class "+name+" {}\n")
	}
	f.git("add", "-A")
	out := f.run("git commit -m second")
	want := `^\[main [0-9a-f]+\] second\n 3 files changed, 3 insertions\(\+\)\n\[` + regexp.QuoteMeta(operation) +
		`\]\n   create A.java\n   create B.java\n   create C.java$`
	if !regexp.MustCompile(want).MatchString(out) {
		t.Errorf("commit:\n%s", out)
	}
	f.git("rm", "-q", "a.txt")
	f.git("update-index", "--chmod=+x", "run.sh")
	out = f.run("git commit -m third")
	if !strings.HasSuffix(out, "\n delete a.txt\n mode change 100644 => 100755 run.sh") {
		t.Errorf("delete and mode change:\n%s", out)
	}
}

func TestGitPullLaysItsDiffstatOutWithExactCounts(t *testing.T) {
	f, other := remoteFixture(t, map[string]string{operation + "A.java": "class A {}\n"})
	writeAt(t, other, operation+"A.java", "class A { int x; }\n")
	for _, name := range []string{"B", "C"} {
		writeAt(t, other, operation+name+".java", strings.Repeat("// line\n", 60))
	}
	gitAt(t, other, "add", "-A")
	gitAt(t, other, "commit", "-qm", "more")
	gitAt(t, other, "push", "-q")
	out := f.run("git pull")
	// git would cut each path to fit 80 columns and scale the bars; every path is whole, laid out by folder, with its exact counts.
	want := `(?s)Updating [0-9a-f]+\.\.[0-9a-f]+\nFast-forward\n\[` + regexp.QuoteMeta(operation) + `\]\n   A.java \+1 -1\n   B.java \+60\n   C.java \+60\n` +
		` 3 files changed, 121 insertions\(\+\), 1 deletion\(-\)\n create ` + regexp.QuoteMeta(operation) + `B.java\n create ` + regexp.QuoteMeta(operation) + `C.java`
	if !regexp.MustCompile(want).MatchString(out) {
		t.Errorf("pull:\n%s", out)
	}
	// A ref's update loses the padding that lines refs up.
	if !regexp.MustCompile(`\n   [0-9a-f]+\.\.[0-9a-f]+ main -> origin/main\n`).MatchString("\n" + out + "\n") {
		t.Errorf("ref update:\n%s", out)
	}
}

func TestGitHidesHintsAndTellsHowBranchesDiverged(t *testing.T) {
	f := diverged(t)
	f.git("fetch", "-q")
	out := f.run("git merge --ff-only origin/main")
	expect(t, out, "fatal: Not possible to fast-forward, aborting.\n[nibash: HEAD and origin/main have diverged: 1 commit only on HEAD, 1 commit only on origin/main]\n[exit 128]")
	// The hint pull prints whatever GIT_ADVICE says is hidden too.
	out = f.run("git pull")
	if strings.Contains(out, "hint:") || !strings.HasSuffix(out, "fatal: Need to specify how to reconcile divergent branches.\n[nibash: HEAD and @{upstream} have diverged: 1 commit only on HEAD, 1 commit only on @{upstream}]\n[exit 128]") {
		t.Errorf("pull:\n%s", out)
	}
	// What goes to a file is git's own.
	out = f.run("git pull 2>err.txt; /usr/bin/cat err.txt")
	if !strings.Contains(out, "hint: You have divergent branches") {
		t.Errorf("into a file:\n%s", out)
	}
}

func TestGitHidesProgressACarriageReturnWritesOver(t *testing.T) {
	f := diverged(t)
	out := f.run("git pull --rebase 2>&1")
	if strings.Contains(out, "Rebasing") || strings.Contains(out, "hint:") || !strings.Contains(out, "\nAuto-merging a.txt\nCONFLICT (content): Merge conflict in a.txt\n") {
		t.Errorf("rebase:\n%s", out)
	}
}

func TestGitCherryPickLeavesOutTheDateItsSummaryRepeats(t *testing.T) {
	f := gitFixture(t, map[string]string{"a.txt": "a\n"})
	f.git("checkout", "-q", "-b", "side")
	f.write("b.txt", "b\n")
	f.git("add", "b.txt")
	f.git("commit", "-qm", "side", "--date", "2001-01-01T00:00:00")
	f.git("checkout", "-q", "main")
	out := f.run("git cherry-pick side")
	if !regexp.MustCompile(`^\[main [0-9a-f]+\] side\n 1 file changed, 1 insertion\(\+\)\n create b.txt$`).MatchString(out) {
		t.Errorf("cherry-pick:\n%s", out)
	}
}

func TestGitPrintsALineItsSSHRepeatsOnce(t *testing.T) {
	f := gitFixture(t, map[string]string{"a.txt": "a\n"})
	warn := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(warn, []byte("#!/bin/sh\necho 'warning: odd option' >&2\necho 'warning: odd option' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := f.run("git -c core.sshCommand=" + warn + " fetch ssh://example.invalid/repo")
	if strings.Count(out, "warning: odd option") != 1 || !strings.Contains(out, "fatal: Could not read from remote repository.") {
		t.Errorf("fetch:\n%s", out)
	}
}
