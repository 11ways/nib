package nibash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eachFixture is a folder outside git holding three repositories, alpha with a change since its commit, apps/gamma whose nib.json names
// its base, and beta, clean and without one.
func eachFixture(t *testing.T) *fixture {
	f := gitFixture(t, map[string]string{"README": "x\n"})
	for _, name := range []string{".git", "README"} {
		if err := os.RemoveAll(filepath.Join(f.directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, repository := range []string{"alpha", "beta", "apps/gamma"} {
		f.write(repository+"/a.txt", "one\n")
		if repository == "apps/gamma" {
			f.write(repository+"/nib.json", `{"base": "main"}`)
		}
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "first in " + repository}} {
			f.git(append([]string{"-C", repository}, args...)...)
		}
	}
	f.write("alpha/a.txt", "one\ntwo\n")
	return f
}

func TestGitEachPrintsOnlyTheRepositoriesWithOutputEachUnderItsHeaderOrOnItsLine(t *testing.T) {
	f := eachFixture(t)
	expect(t, f.run("git-each status --short"), "==> alpha <==\n M a.txt")
	// Path order, and git's own lines where its output goes to a real program or a file; one plain line alone shows on its header's line.
	f.git("-C", "beta", "commit", "-q", "--allow-empty", "-m", "second in beta")
	expect(t, f.run("git-each log --format=%s -2 | cat -A | sed 's/\\$$//'"), "alpha: first in alpha\napps/gamma: first in apps/gamma\n==> beta <==\nsecond in beta\nfirst in beta")
	f.run("git-each status --short > out.txt")
	if data, _ := os.ReadFile(filepath.Join(f.directory, "out.txt")); string(data) != "==> alpha <==\n M a.txt\n" {
		t.Errorf("into a file: %q", data)
	}
}

func TestGitEachStatusIsOneLinePerCleanRepository(t *testing.T) {
	f := eachFixture(t)
	expect(t, f.run("git-each status -sb"), "==> alpha <==\n## main\n M a.txt\napps/gamma: ## main\nbeta: ## main")
}

func TestGitEachGivesEachRepositoryItsBaseOrSaysWhyItHasNone(t *testing.T) {
	f := eachFixture(t)
	// alpha's base is what origin/HEAD points at; it is one commit ahead of it.
	f.git("-C", "alpha", "update-ref", "refs/remotes/origin/main", "HEAD")
	f.git("-C", "alpha", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	f.git("-C", "alpha", "commit", "-q", "-am", "second in alpha")
	expect(t, f.run("git-each log --format=%s {base}..HEAD"), "alpha: second in alpha\nbeta: [skipped: no \"base\" in its nib.json, no upstream of another name and no origin/HEAD]")
}

func TestGitEachBaseIsTheUpstreamALaneTracksBeforeOriginHEAD(t *testing.T) {
	f := eachFixture(t)
	// alpha's lane tracks rewrite, which has a commit main lacks; origin/HEAD is main.
	skip := "beta: [skipped: no \"base\" in its nib.json, no upstream of another name and no origin/HEAD]"
	f.git("-C", "alpha", "remote", "add", "origin", "../beta")
	f.git("-C", "alpha", "update-ref", "refs/remotes/origin/main", "HEAD")
	f.git("-C", "alpha", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	f.git("-C", "alpha", "commit", "-q", "--allow-empty", "-m", "only on rewrite")
	f.git("-C", "alpha", "update-ref", "refs/remotes/origin/rewrite", "HEAD")
	f.git("-C", "alpha", "checkout", "-q", "-b", "lane/x")
	f.git("-C", "alpha", "branch", "-q", "--set-upstream-to", "origin/rewrite")
	expect(t, f.run("git-each log --format=%s {base}..HEAD"), skip)
	f.git("-C", "alpha", "commit", "-q", "--allow-empty", "-m", "on the lane")
	expect(t, f.run("git-each log --format=%s {base}..HEAD"), "alpha: on the lane\n"+skip)
	// A branch tracking its own name on the remote is not its base: origin/HEAD is.
	f.git("-C", "alpha", "update-ref", "refs/remotes/origin/lane/x", "HEAD")
	f.git("-C", "alpha", "branch", "-q", "--set-upstream-to", "origin/lane/x")
	expect(t, f.run("git-each log --format=%s {base}..HEAD"), "==> alpha <==\non the lane\nonly on rewrite\n"+skip)
}

func TestGitEachFailsWhenAnyRepositorysGitFails(t *testing.T) {
	f := eachFixture(t)
	f.git("-C", "beta", "tag", "v1")
	expect(t, f.run("git-each rev-parse -q --verify v1; echo status $?"), "beta: "+strings.TrimSpace(f.git("-C", "beta", "rev-parse", "v1"))+"\nstatus 1")
	out := f.run("git-each rev-parse --verify nosuch 2>&1; echo status $?")
	if strings.Count(out, "fatal: Needed a single revision") != 3 || !strings.HasPrefix(out, "alpha: fatal:") || !strings.HasSuffix(out, "status 1") {
		t.Errorf("got:\n%s", out)
	}
	// What git rm and git clean delete goes to the trash only one repository at a time.
	if out := f.run("git-each clean -fd; echo status $?"); !strings.Contains(out, "git-each: runs no git clean") || !strings.HasSuffix(out, "status 2") {
		t.Errorf("clean: %s", out)
	}
}

func TestGitEachShapesWhatReachesTheModelAsNibsGitDoes(t *testing.T) {
	f := eachFixture(t)
	out := f.run("git-each show --stat HEAD")
	for _, want := range []string{"==> beta <==\ncommit ", "first in beta", " a.txt +1"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Author:") || strings.Contains(out, "| ") {
		t.Errorf("not shaped:\n%s", out)
	}
}

func TestGitEachIsHintedOnlyWhereSeveralRepositoriesAre(t *testing.T) {
	f := eachFixture(t)
	if hint := f.shell.Hint(f.directory); !strings.Contains(hint, "git-each ARGS") {
		t.Errorf("three repositories, hint %q", hint)
	}
	if hint := f.shell.Hint(filepath.Join(f.directory, "apps")); hint != "" {
		t.Errorf("one repository, hint %q", hint)
	}
}

func TestGitEachIsJudgedAsTheGitItRunsAndByEachRepositorysRules(t *testing.T) {
	f := eachFixture(t)
	f.write("apps/gamma/nib.json", `{"base": "main", "rules": [{"program": "git", "args": "^push", "why": "pushing is the user's"}]}`)
	out := f.run("git-each push")
	if !strings.Contains(out, "nothing ran: git push is against a rule in") {
		t.Errorf("got:\n%s", out)
	}
}
