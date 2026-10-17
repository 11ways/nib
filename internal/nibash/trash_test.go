package nibash

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/trash"
)

// notes are the lines nibash adds at an output's end: its closing notes and the files a command changed.
var notes = regexp.MustCompile(`(?m)^\[(in the trash|moved to the trash|changed by this command|exit )[^\n]*\]\n?`)

// tree lists what a directory holds, one relative path a line, a directory with a slash.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var paths []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			rel += "/"
		}
		paths = append(paths, rel)
		return nil
	})
	return strings.Join(paths, "\n")
}

// rmScene makes the same files in dir each time: plain, empty, a directory tree, an empty directory and links.
func rmScene(t *testing.T, dir string) {
	t.Helper()
	for name, content := range map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n", "d.txt": "d\n", "tree/x/y.txt": "y\n", "tree/z.txt": "z\n", "full/f.txt": "f\n", "it's": "q\n", "a b": "s\n"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"empty"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "zero"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	os.Symlink("tree", filepath.Join(dir, "dirlink"))
	os.Symlink("a.txt", filepath.Join(dir, "filelink"))
	os.Symlink("nowhere", filepath.Join(dir, "dangling"))
}

// TestRmSaysAndDoesWhatCoreutilsRmDoesWithItsFilesInTheTrash runs each command under bash with the system rm and under nibash in the same
// scene, and compares what each printed, its exit status and what was left.
func TestRmSaysAndDoesWhatCoreutilsRmDoesWithItsFilesInTheTrash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	if out, err := exec.Command("rm", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU coreutils") {
		t.Skip("rm is not coreutils' rm")
	}
	commands := []string{
		"rm a.txt", "rm missing", "rm -f missing", "rm", "rm -f", "rm tree", "rm -d empty", "rm -d full", "rm -dv empty",
		"rm -r .", "rm -r ..", "rm -r tree/..", "rm .", "rm -r /", "rm -x a.txt", "rm --bogus a.txt", "rm --ver a.txt", "rm --force=1 a.txt",
		"rm --interactive=bad a.txt", "rm -i a.txt </dev/null", "rm -i zero </dev/null", "rm -I a.txt b.txt c.txt d.txt </dev/null",
		"rm -I a.txt b.txt c.txt", "rm -rI tree </dev/null", "rm -rv tree", "rm -v a.txt b.txt", "rm a.txt/", "rm -r dirlink/",
		"rm dirlink", "rm -v filelink dangling", "rm \"it's\" 'a b' 'q\"x'", "rm -- -x", "rm a.txt -v", "rm --rec --verb tree",
		"rm --interactive=never -r tree", "yes | rm -i a.txt b.txt", "rm -v a.txt | cat", "rm -ri tree </dev/null", "rm --preserve-root=all -r tree",
		"rm --preserve-root=x a.txt", "rm --one-file-system -rv tree",
	}
	for _, command := range commands {
		real, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rmScene(t, real)
		cmd := exec.Command("bash", "-c", command+" 2>&1; echo \"status=$?\"")
		cmd.Dir = real
		want, _ := cmd.CombinedOutput()

		f := newFixture(t, nil)
		rmScene(t, f.directory)
		got := notes.ReplaceAllString(f.run(command+" 2>&1; echo \"status=$?\""), "")
		if strings.TrimRight(got, "\n") != strings.TrimRight(string(want), "\n") {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", command, got, want)
		}
		if g, w := tree(t, f.directory), tree(t, real); g != w {
			t.Errorf("%s left:\n%s\nwant:\n%s", command, g, w)
		}
	}
}

func trashed(t *testing.T) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(trash.Home(), "info"))
	var names []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(trash.Home(), "info", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if path, ok := strings.CutPrefix(line, "Path="); ok {
				names = append(names, path)
			}
		}
	}
	slices.Sort(names)
	return names
}

func TestDeletedFilesGoToTheTrashAndComeBack(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	f := newFixture(t, map[string]string{"keep/a.txt": "a\n", "keep/b.txt": "b\n", "it's here.txt": "c\n"})
	out := f.run("rm -r keep \"it's here.txt\"")
	if !strings.Contains(out, "[moved to the trash, not deleted: keep, it's here.txt; trash list shows what this session trashed, trash restore PATH puts one back]") {
		t.Fatalf("got %s", out)
	}
	want := []string{escapeForTest(filepath.Join(f.directory, "it's here.txt")), escapeForTest(filepath.Join(f.directory, "keep"))}
	if got := trashed(t); !slices.Equal(got, want) {
		t.Fatalf("trash holds %v, want %v", got, want)
	}
	if data, err := os.ReadFile(filepath.Join(trash.Home(), "files", "keep", "a.txt")); err != nil || string(data) != "a\n" {
		t.Fatalf("the directory went to the trash whole: %q %v", data, err)
	}
	list := f.run("trash list")
	if !strings.Contains(list, "  keep (directory)") || !strings.Contains(list, "  it's here.txt") {
		t.Fatalf("list: %s", list)
	}
	f.write("keep", "new\n")
	if out := f.run("trash restore keep"); !strings.Contains(out, "a file is there now; trash restore -f moves it to the trash first") || !strings.Contains(out, "[exit 1]") {
		t.Fatalf("restore over a file: %s", out)
	}
	if out := f.run("trash restore -f keep"); !strings.HasPrefix(out, "restored 'keep'") {
		t.Fatalf("forced restore: %s", out)
	}
	if data, err := os.ReadFile(filepath.Join(f.directory, "keep", "b.txt")); err != nil || string(data) != "b\n" {
		t.Fatalf("restored: %q %v", data, err)
	}
	if !strings.Contains(f.run("trash list"), "  keep\n") && !strings.HasSuffix(f.run("trash list"), "  keep") {
		t.Fatalf("the file the restore replaced went to the trash: %s", f.run("trash list"))
	}
	if out := f.run("trash restore nothing.txt"); !strings.Contains(out, "this session did not move it to the trash") {
		t.Fatalf("got %s", out)
	}
	// A later note is the short one.
	f.write("x.txt", "x\n")
	if out := f.run("rm x.txt"); out != "[in the trash: x.txt; trash restore PATH]" {
		t.Fatalf("got %q", out)
	}
	if out := f.run("trash restore x.txt && cat x.txt"); !strings.Contains(out, "restored 'x.txt'") || !strings.Contains(out, "1:x") {
		t.Fatalf("got %s", out)
	}
}

func escapeForTest(path string) string {
	return strings.NewReplacer("'", "%27", " ", "%20").Replace(path)
}

func TestALinkGoesToTheTrashNotWhatItPointsAt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	f := newFixture(t, map[string]string{"target/a.txt": "a\n"})
	os.Symlink("target", filepath.Join(f.directory, "link"))
	f.run("rm link")
	if _, err := os.Stat(filepath.Join(f.directory, "target", "a.txt")); err != nil {
		t.Fatal("the link's target went too")
	}
	if info, err := os.Lstat(filepath.Join(trash.Home(), "files", "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the trash holds no link: %v", err)
	}
}

func TestUnlinkGoesToTheTrash(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	f := newFixture(t, map[string]string{"a.txt": "a\n", "d/b.txt": "b\n"})
	expect(t, notes.ReplaceAllString(f.run("unlink a.txt; unlink d 2>&1; unlink 2>&1; unlink a b 2>&1"), ""),
		"unlink: cannot unlink 'd': Is a directory\nunlink: missing operand\nTry 'unlink --help' for more information.\nunlink: extra operand 'b'\nTry 'unlink --help' for more information.\n")
	if got := trashed(t); len(got) != 1 || !strings.HasSuffix(got[0], "/a.txt") {
		t.Fatalf("trash holds %v", got)
	}
}

func TestFindDeleteSendsWhatItWouldDeleteToTheTrash(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	f := newFixture(t, map[string]string{"a/b/c.log": "c\n", "a/x.log": "x\n", "a/keep.txt": "k\n"})
	out := notes.ReplaceAllString(f.run("find a -name '*.log' -print -delete; find a -name b -delete 2>&1; echo status=$?"), "")
	expect(t, out, "a/b/c.log\na/x.log\nstatus=0\n")
	if tree(t, filepath.Join(f.directory, "a")) != "keep.txt" {
		t.Fatalf("left %s", tree(t, filepath.Join(f.directory, "a")))
	}
	if got := trashed(t); len(got) != 3 {
		t.Fatalf("trash holds %v", got)
	}
	f.write("a/full/z.txt", "z\n")
	if out := f.run("find a -name full -delete"); !strings.Contains(out, "find: cannot delete \u2018a/full\u2019: Directory not empty") && !strings.Contains(out, "find: cannot delete 'a/full': Directory not empty") {
		t.Fatalf("got %s", out)
	}
	if out := f.run("find a -name z.txt -prune -delete"); !strings.Contains(out, "-prune does nothing when -depth is in effect") {
		t.Fatalf("got %s", out)
	}
}

func TestGitRmAndCleanSendTheirFilesToTheTrash(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	f := gitFixture(t, map[string]string{"d/e/f.txt": "f\n", "d/g.txt": "g\n", "top.txt": "t\n"})
	f.write("d/untracked.txt", "u\n")
	expect(t, notes.ReplaceAllString(f.run("git rm -r -q d && git status --short"), ""), "D  d/e/f.txt\nD  d/g.txt\n?? d/\n")
	if tree(t, filepath.Join(f.directory, "d")) != "untracked.txt" {
		t.Fatalf("left %s", tree(t, filepath.Join(f.directory, "d")))
	}
	if got := trashed(t); len(got) != 2 {
		t.Fatalf("trash holds %v", got)
	}
	f.write("top.txt", "changed\n")
	if out := f.run("git rm top.txt"); !strings.Contains(out, "error: the following file has local modifications:") || !strings.Contains(out, "[exit 1]") {
		t.Fatalf("got %s", out)
	}
	if _, err := os.Stat(filepath.Join(f.directory, "top.txt")); err != nil {
		t.Fatal("a refused git rm removed the file")
	}
	f.write("junk/a.tmp", "j\n")
	if out := f.run("git clean -n -d"); !strings.Contains(out, "Would remove junk/") {
		t.Fatalf("a dry run is git's own: %s", out)
	}
	if out := f.run("git clean -d"); !strings.Contains(out, "refusing to clean") {
		t.Fatalf("without -f git refuses: %s", out)
	}
	expect(t, notes.ReplaceAllString(f.run("git clean -fd"), ""), "Removing d/\nRemoving junk/\n")
	if got := trashed(t); len(got) != 4 {
		t.Fatalf("trash holds %v", got)
	}
	if out := f.run("git clean -fi"); !strings.Contains(out, "cannot ask -i's questions") {
		t.Fatalf("got %s", out)
	}
}

func TestRmRefusesPathsOutsideTheSandbox(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	f.sandbox = true
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := f.run("rm " + outside + "/b.txt"); !strings.Contains(out, "rm: cannot remove") || !strings.Contains(out, "[exit 1]") {
		t.Fatalf("got %s", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "b.txt")); err != nil {
		t.Fatal("rm left the sandbox")
	}
	f.run("rm a.txt")
	if got := trashed(t); len(got) != 1 {
		t.Fatalf("trash holds %v", got)
	}
}
