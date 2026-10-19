package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/gitform"
)

// revisionFixture is a repository whose user.email is t@t with two commits: "first" adds a.go and gone.txt, "second" changes a.go and
// deletes gone.txt.
func revisionFixture(t *testing.T) (*Engine, *Context, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitform.Zone = time.FixedZone("", -4*3600)
	t.Cleanup(func() { gitform.Zone = nil })
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = directory
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_AUTHOR_DATE=2026-10-16T10:38:01+02:00", "GIT_COMMITTER_DATE=2026-10-16T10:38:01+02:00")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	write("a.go", "package a\n\nfunc A() int {\n\treturn 1\n}\n")
	write("gone.txt", "old\n")
	write("sub/b.txt", "b\n")
	git("add", "-A")
	git("commit", "-q", "-m", "first")
	write("a.go", "package a\n\nfunc A() int {\n\treturn 2\n}\n")
	write("sub/b.txt", "b2\n")
	if err := os.Remove(filepath.Join(directory, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "second\n\nwhy it changed")
	return New(), &Context{Ctx: context.Background(), Directory: directory, Session: "s"}, strings.TrimSpace(git("rev-parse", "--short", "HEAD"))
}

func readText(t *testing.T, e *Engine, ctx *Context, paths ...string) string {
	t.Helper()
	got, err := e.Read(ctx, ReadArgs{Paths: paths})
	if err != nil {
		t.Fatal(err)
	}
	return got.Text
}

func TestReadAFileAsOfARevisionPrintsRowsThatAreNoAnchors(t *testing.T) {
	e, ctx, _ := revisionFixture(t)
	expectText(t, readText(t, e, ctx, "a.go@HEAD~1"), "[a.go@HEAD~1]\n1|package a\n2|\n3|func A() int {\n4|\treturn 1\n5|}")
	expectText(t, readText(t, e, ctx, "a.go@HEAD~1:4-5"), "[a.go@HEAD~1]\n4|\treturn 1\n5|}\n[5 lines total]")
	expectText(t, readText(t, e, ctx, "a.go@HEAD~1:A"), "[a.go@HEAD~1]\n3|func A() int {\n4|\treturn 1\n5|}\n[5 lines total]")
	// A file gone since is read as it was.
	expectText(t, readText(t, e, ctx, "gone.txt@HEAD~1"), "[gone.txt@HEAD~1]\n1|old")
	plain, err := e.Read(ctx, ReadArgs{Paths: []string{"a.go@HEAD~1:4"}, Plain: true})
	if err != nil || plain.Text != "[a.go@HEAD~1]\n4|\treturn 1\n[5 lines total]" {
		t.Fatalf("plain: %v\n%s", err, plain.Text)
	}
	// Nothing of a revision counts as seen, so an edit of the file as it is now still needs a read.
	if version := e.Latest(ctx, filepath.Join(ctx.Directory, "a.go")); version != nil {
		t.Fatalf("a revision's rows were recorded for the file as it is: %+v", version)
	}
}

func TestReadADirectoryAsOfARevisionPrintsTheCommit(t *testing.T) {
	e, ctx, short := revisionFixture(t)
	want := "[.@HEAD]\n1|commit " + short + " Fri 2026-10-16 04:38\n2|second\n3|\n4|why it changed\n5|---\n6| a.go +1 -1\n7| gone.txt -1\n" +
		"8| sub/b.txt +1 -1\n9| 3 files changed, 2 insertions(+), 3 deletions(-)\n10|\n11|diff --git a/a.go b/a.go\n"
	got := readText(t, e, ctx, ".@HEAD")
	if !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "\n"+gitform.OwnAuthorLesson) {
		t.Fatalf("got:\n%s\nwant it to start:\n%s", got, want)
	}
	// A directory inside the repository narrows the commit to it, and a path after the colon to one file.
	sub := readText(t, e, ctx, "sub@HEAD")
	if !strings.Contains(sub, "| sub/b.txt +1 -1\n") || strings.Contains(sub, "a.go") {
		t.Fatalf("sub@HEAD:\n%s", sub)
	}
	one := readText(t, e, ctx, ".@HEAD:a.go")
	if !strings.Contains(one, "|-\treturn 1\n") || strings.Contains(one, "gone.txt") {
		t.Fatalf(".@HEAD:a.go:\n%s", one)
	}
	expectText(t, readText(t, e, ctx, ".@HEAD:6-7"), "[.@HEAD]\n6| a.go +1 -1\n7| gone.txt -1\n[35 lines total]")
}

func TestReadOfARevisionThatDoesNotResolveSaysSo(t *testing.T) {
	e, ctx, _ := revisionFixture(t)
	_, err := e.Read(ctx, ReadArgs{Paths: []string{"a.go@nope"}})
	if err == nil || err.Error() != "a.go@nope: nope is no commit in ." {
		t.Fatalf("got %v", err)
	}
	if _, err := e.Read(ctx, ReadArgs{Paths: []string{"sub/missing.txt@HEAD"}}); err == nil || !strings.Contains(err.Error(), "holds no file sub/missing.txt") {
		t.Fatalf("got %v", err)
	}
	// A path that exists as typed is that file.
	if err := os.WriteFile(filepath.Join(ctx.Directory, "x@HEAD"), []byte("plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, e, ctx, "x@HEAD"); !strings.Contains(got, "\n1:plain") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestALargeCommitReadsAsItsHeaderAndFilesFirst(t *testing.T) {
	e, ctx, _ := revisionFixture(t)
	lines := strings.Repeat("line\n", outlineLines+10)
	if err := os.WriteFile(filepath.Join(ctx.Directory, "big.txt"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "commit", "-q", "-m", "big"}} {
		if out, err := exec.Command("git", append([]string{"-C", ctx.Directory}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	got := readText(t, e, ctx, ".@HEAD")
	if !strings.Contains(got, "; header and files only. Read the diff with .@HEAD:7-, or one file's with .@HEAD:PATH.\n") ||
		!strings.Contains(got, "| big.txt +510\n") || strings.Contains(got, "diff --git") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestSkimReadsARevisionAsALog(t *testing.T) {
	e, ctx, _ := revisionFixture(t)
	log, err := e.ReadLog(ctx, ".@HEAD", 1<<20, func(*LogFile) int64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if log.Display != ".@HEAD" || !strings.Contains(string(log.Raw), "\n a.go +1 -1\n") || !strings.Contains(string(log.Raw), "-\treturn 1\n") {
		t.Fatalf("%s:\n%s", log.Display, log.Raw)
	}
	file, err := e.ReadLog(ctx, "a.go@HEAD~1:4", 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(file.Raw) != "package a\n\nfunc A() int {\n\treturn 1\n}\n" || len(file.Ranges) != 1 || file.Ranges[0] != [2]int{4, 4} {
		t.Fatalf("%q %v", file.Raw, file.Ranges)
	}
}

func expectText(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
