package nibash

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gitFixture is a fixture whose directory is a repository with files committed, so tests see git's real output. nibash's git gets the
// process's environment, so the user's configuration stays out of both it and the fixture's own git.
func gitFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for _, pair := range [][2]string{{"GIT_CONFIG_GLOBAL", "/dev/null"}, {"GIT_CONFIG_SYSTEM", "/dev/null"}, {"GIT_AUTHOR_NAME", "t"},
		{"GIT_AUTHOR_EMAIL", "t@t"}, {"GIT_COMMITTER_NAME", "t"}, {"GIT_COMMITTER_EMAIL", "t@t"}} {
		t.Setenv(pair[0], pair[1])
	}
	f := newFixture(t, files)
	f.git("init", "-q", "-b", "main")
	f.git("add", "-A")
	f.git("commit", "-q", "-m", "first")
	return f
}

// git runs git in the fixture's directory, outside nibash.
func (f *fixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.directory
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (f *fixture) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

const operation = "src/common/java/be/elevenways/zenit/common/operation/"

// operationFixture has a file at the top and three in one deep folder, all changed since the commit.
func operationFixture(t *testing.T) *fixture {
	files := map[string]string{"a.go": "package a\n\nfunc A() {}\n"}
	for _, name := range []string{"Operation", "OperationResult", "ResultStep"} {
		files[operation+name+".java"] = "class " + name + " {}\n"
	}
	f := gitFixture(t, files)
	for _, name := range []string{"Operation", "OperationResult", "ResultStep"} {
		f.write(operation+name+".java", "class "+name+" { int x; }\n")
	}
	f.write("a.go", "package a\n\nfunc A() { return }\n")
	return f
}

func TestGitPathListingsNameEachFolderOnce(t *testing.T) {
	f := operationFixture(t)
	expect(t, f.run("git status --short"), " M a.go\n["+operation+"]\n   M Operation.java\n   M OperationResult.java\n   M ResultStep.java")
	expect(t, f.run("git status --porcelain"), " M a.go\n["+operation+"]\n   M Operation.java\n   M OperationResult.java\n   M ResultStep.java")
	long := f.run("git status")
	if !strings.Contains(long, "\tmodified:   a.go\n["+operation+"]\n  \tmodified:   Operation.java\n  \tmodified:   OperationResult.java\n") ||
		!strings.HasPrefix(long, "On branch main\n") {
		t.Errorf("long status:\n%s", long)
	}
	// --stat prints every path whole, which git would cut to 80 columns, and lines the files of one folder up under it,
	// each with the exact counts its bar draws.
	expect(t, f.run("git diff --stat"), " a.go +1 -1\n["+operation+"]\n   Operation.java +1 -1\n   OperationResult.java +1 -1\n   ResultStep.java +1 -1\n 4 files changed, 4 insertions(+), 4 deletions(-)")
	expect(t, f.run("git diff --name-only"), "a.go\n["+operation+"]\n  Operation.java\n  OperationResult.java\n  ResultStep.java")
	expect(t, f.run("git ls-files"), "a.go\n["+operation+"]\n  Operation.java\n  OperationResult.java\n  ResultStep.java")
	f.git("add", "-A")
	expect(t, f.run("git diff --cached --name-status"), "M\ta.go\n["+operation+"]\n  M\tOperation.java\n  M\tOperationResult.java\n  M\tResultStep.java")
	expect(t, f.run("git rm -q --cached -r src && git rm --cached -r src"), "fatal: pathspec 'src' did not match any files\n[exit 128]")
	f.git("add", "-A")
	expect(t, f.run("git rm --cached -r src"), "["+operation+"]\n  rm 'Operation.java'\n  rm 'OperationResult.java'\n  rm 'ResultStep.java'")
}

func TestGitOutputGoingElsewhereOrInFormsNibDoesNotReadIsGits(t *testing.T) {
	f := operationFixture(t)
	raw := " M a.go\n M " + operation + "Operation.java\n M " + operation + "OperationResult.java\n M " + operation + "ResultStep.java"
	// Into another command or a file, git's own lines go, so what reads them sees what git printed.
	expect(t, f.run("git status --short | sort"), raw)
	// Through nib's own commands, which select from git's own lines, what reaches the model is laid out by folder as git's would be.
	expect(t, f.run("git status --short | cat"), " M a.go\n["+operation+"]\n   M Operation.java\n   M OperationResult.java\n   M ResultStep.java")
	expect(t, f.run("git status --short | head -3"), " M a.go\n["+operation+"]\n   M Operation.java\n   M OperationResult.java")
	expect(t, f.run("git status --short | tail -n 2"), " M "+operation+"OperationResult.java\n M "+operation+"ResultStep.java")
	expect(t, f.run("git status --short | grep -v a.go"), "["+operation+"]\n   M Operation.java\n   M OperationResult.java\n   M ResultStep.java")
	// grep -n changes each line, so they are no longer git's and print as grep printed them.
	expect(t, f.run("git status --short | grep -n Result"), "3: M "+operation+"OperationResult.java\n4: M "+operation+"ResultStep.java")
	// A diff's lines go on as git printed them, so head counts git's lines.
	if out := f.run("git diff | head -2"); !regexp.MustCompile(`^diff --git a/a.go b/a.go\nindex [0-9a-f]+\.\.[0-9a-f]+ 100644$`).MatchString(out) {
		t.Errorf("diff into head: %q", out)
	}
	if out := f.run("git status --short -uno > out.txt; /usr/bin/cat out.txt"); !strings.HasPrefix(out, raw+"\n[changed by this command: out.txt#") {
		t.Errorf("into a file: %q", out)
	}
	// A form nib's git does not print differently runs the system git, with no note; an option it lacks says so.
	if out := f.run("git log --oneline"); strings.Contains(out, "system program") || !strings.HasSuffix(out, " first") {
		t.Errorf("log --oneline: %q", out)
	}
	out := f.run("git status --porcelain=v2")
	if !strings.Contains(out, "1 .M N... ") || !strings.HasSuffix(out, "[git ran as the system program: nib's own does not implement this form of it, so its output is as git printed it]") {
		t.Errorf("porcelain v2: %q", out)
	}
	// A show of a file's content is not a diff, and lines of a commit's message or a file are never taken for a path list.
	expect(t, f.run("git show HEAD:a.go"), "package a\n\nfunc A() {}")
	f.git("commit", "-qam", "list\n\n"+operation+"a.txt\n"+operation+"b.txt\n"+operation+"c.txt")
	if out := f.run("git log -1 -p --format=%B"); !strings.HasPrefix(out, "list\n\n"+operation+"a.txt\n"+operation+"b.txt\n") {
		t.Errorf("message with paths:\n%s", out)
	}
	// git's exit status and standard error are its own.
	expect(t, f.run("git -C missing status --short"), "fatal: cannot change to 'missing': No such file or directory\n[exit 128]")
}

func TestGitDiffPrintsTheChangedFilesRowsThatEditAccepts(t *testing.T) {
	f := gitFixture(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n\nfunc B() {}\n"})
	f.write("a.go", "package a\n\nfunc A() { return }\n\nfunc B() {}\n")
	out := f.run("git diff")
	tag := tagOf(t, out)
	expect(t, out, fmt.Sprintf("[a.go#%s]\n@@ -1,5 +1,5 @@\n 1:package a\n 2:\n-|func A() {}\n+3:func A() { return }\n 4:\n 5:func B() {}", tag))
	// The + row is the file's line 3, shown whole, so an edit anchors on it with no read.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.go#%s]\nPUT 3:\n+func A() { return 1 }", tag)); err != nil {
		t.Fatalf("edit from a diff row: %v", err)
	}
	// Staged, then changed again: the index's line is no longer the file's, so it prints in a form that is never an anchor.
	f.git("add", "a.go")
	f.write("a.go", "package a\n\nfunc A() { return 2 }\n\nfunc B() {}\n")
	out = f.run("git diff --cached")
	expect(t, out, fmt.Sprintf("[a.go#%s]\n@@ -1,5 +1,5 @@\n 1:package a\n 2:\n-|func A() {}\n+3|func A() { return 1 }\n 4:\n 5:func B() {}", tagOf(t, out)))
}

func TestGitDiffRowsTheAgentHoldsPrintAsOneMarkerButAddedOnesAlways(t *testing.T) {
	f := gitFixture(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\n"})
	f.markers = true
	f.run("cat a.txt")
	f.write("a.txt", "one\ntwo\nthree\nFOUR\n")
	f.run("cat a.txt")
	out := f.run("git diff")
	if !strings.Contains(out, "\n[lines 1-3 unchanged since you saw them]\n-|four\n+4:FOUR") {
		t.Errorf("diff with held rows:\n%s", out)
	}
}

func TestGitDiffKnowsNewDeletedAndRenamedFilesAndOtherDirectories(t *testing.T) {
	f := gitFixture(t, map[string]string{"gone.txt": "bye\n", "sub/old.txt": "one\ntwo\nthree\nfour\n"})
	f.write("new.txt", "hello\n")
	f.git("add", "-N", "new.txt")
	f.git("rm", "-q", "gone.txt")
	f.git("mv", "sub/old.txt", "sub/new.txt")
	f.write("sub/new.txt", "one\ntwo\nthree\nFOUR\n")
	f.git("add", "sub/new.txt")
	out := f.run("git diff HEAD")
	// A deleted file has no new side to show, so its part is git's as printed.
	if !strings.Contains(out, "diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\nindex ") || !strings.Contains(out, "-bye\n") {
		t.Errorf("deleted file:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("[new.txt#%s]\nnew file mode 100644\n@@ -0,0 +1 @@\n+1:hello", tagOf(t, out[strings.Index(out, "[new.txt"):]))) {
		t.Errorf("new file:\n%s", out)
	}
	renamed := out[strings.Index(out, "[sub/new.txt"):]
	if !strings.HasPrefix(renamed, fmt.Sprintf("[sub/new.txt#%s]\nsimilarity index 73%%\nrename from sub/old.txt\nrename to sub/new.txt\n@@ -1,4 +1,4 @@\n 1:one\n 2:two\n 3:three\n-|four\n+4:FOUR", tagOf(t, renamed))) {
		t.Errorf("renamed file:\n%s", renamed)
	}
	// Paths in a diff are the repository's, wherever git runs from.
	inner := f.run("cd sub && git diff HEAD -- new.txt")
	if !strings.HasPrefix(inner, "[sub/new.txt#") || !strings.Contains(inner, "+4:FOUR") {
		t.Errorf("from a subdirectory:\n%s", inner)
	}
	outer := f.run("cd /tmp && git -C " + f.directory + "/sub diff HEAD --relative -- new.txt")
	if !strings.Contains(outer, "+4:FOUR") {
		t.Errorf("with -C and --relative:\n%s", outer)
	}
}

func TestGitDiffRowsMatchFilesWithCRLFAndABOM(t *testing.T) {
	f := gitFixture(t, map[string]string{"dos.txt": "one\r\ntwo\r\n", "bom.txt": "\ufefffirst\nsecond\n"})
	f.write("dos.txt", "one\r\nTWO\r\n")
	f.write("bom.txt", "\ufeffFIRST\nsecond\n")
	out := f.run("git diff")
	if !strings.Contains(out, "+2:TWO") || !strings.Contains(out, "+1:FIRST") || !strings.Contains(out, " 1:one") {
		t.Errorf("CRLF and BOM files:\n%s", out)
	}
}

func TestGitLogOfOldCommitsShowsLinesTheFileNoLongerHoldsAsNoAnchors(t *testing.T) {
	f := gitFixture(t, map[string]string{"a.txt": "one\n"})
	f.write("a.txt", "two\n")
	f.git("commit", "-qam", "second")
	f.write("a.txt", "three\n")
	f.git("commit", "-qam", "third")
	out := f.run("git log -p --format=%s")
	// The newest commit's line is the file's line 1; the older one's is not, so it is no anchor.
	if !strings.HasPrefix(out, "third\n\n[a.txt#") || !strings.Contains(out, "+1:three") || !strings.Contains(out, "second\n\n") ||
		!strings.Contains(out, "+1|two") {
		t.Errorf("log -p:\n%s", out)
	}
	// --word-diff prints lines nib does not read, so the system git runs and says so.
	if out := f.run("git diff HEAD~1 --word-diff"); !strings.Contains(out, "[git ran as the system program") {
		t.Errorf("word diff: %q", out)
	}
}
