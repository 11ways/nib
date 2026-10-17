package nibash

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

const deep = "src/main/java/be/elevenways/app/"

func deepFixture(t *testing.T) *fixture {
	return newFixture(t, map[string]string{deep + "Alpha.java": "a\n", deep + "Beta.java": "b\n", deep + "Gamma.java": "c\n", "top.txt": "t\n"})
}

func TestFindPrintsEachFolderOnce(t *testing.T) {
	f := deepFixture(t)
	lines := strings.Split(f.run("find src -name '*.java'"), "\n")
	// find prints a directory's entries in the order the file system gives them, so only the rows under the folder are sorted here.
	if lines[0] != "[src/main/java/be/elevenways/app/]" || len(lines) != 4 {
		t.Fatalf("find: %q", lines)
	}
	rows := slices.Clone(lines[1:])
	slices.Sort(rows)
	if !slices.Equal(rows, []string{"  Alpha.java", "  Beta.java", "  Gamma.java"}) {
		t.Errorf("rows under the folder: %q", rows)
	}
	// Into another command, or with an action nib's find does not run, find's own lines go; the action says so.
	if out := f.run("find src -name '*.java' | sort"); out != "src/main/java/be/elevenways/app/Alpha.java\nsrc/main/java/be/elevenways/app/Beta.java\nsrc/main/java/be/elevenways/app/Gamma.java" {
		t.Errorf("find into sort: %q", out)
	}
	if out := f.run("find src -name 'Alpha.java' -exec echo found {} ';'"); out != "found src/main/java/be/elevenways/app/Alpha.java\n[find ran as the system program: nib's own does not implement this form of it, so its output is as find printed it]" {
		t.Errorf("find -exec: %q", out)
	}
	// Its exit status and standard error are find's.
	// GNU find quotes the name in its locale's quotes, which differ from one machine to another.
	if out := f.run("find missing -name x"); !regexp.MustCompile(`^find: .+missing.+: No such file or directory\n\[exit 1\]$`).MatchString(out) {
		t.Errorf("find of a missing directory: %q", out)
	}
}

func TestLsPrintsEachFolderOfTheNamesItIsGivenOnce(t *testing.T) {
	f := deepFixture(t)
	expect(t, f.run("ls src/main/java/be/elevenways/app/*.java top.txt"), "[src/main/java/be/elevenways/app/]\n  Alpha.java\n  Beta.java\n  Gamma.java\ntop.txt")
	long := f.run("ls -l src/main/java/be/elevenways/app/*.java")
	if !regexp.MustCompile(`^\[src/main/java/be/elevenways/app/\]\n(  -rw-r--r-- 1 \S+ \S+ 2 .+ (Alpha|Beta|Gamma)\.java\n?){3}$`).MatchString(long) {
		t.Errorf("ls -l:\n%s", long)
	}
	// A directory's own listing names its entries without a folder, so it prints as ls printed it.
	expect(t, f.run("ls src/main/java/be/elevenways/app"), "Alpha.java\nBeta.java\nGamma.java")
	if out := f.run("ls --full-time top.txt"); !strings.HasSuffix(out, "[ls ran as the system program: nib's own does not implement this form of it, so its output is as ls printed it]") {
		t.Errorf("ls with an option nib's lacks: %q", out)
	}
}

func TestPathListingsLeaveLinesThatNameNoFileAlone(t *testing.T) {
	f := deepFixture(t)
	parse := pathLines(f.directory)
	lines := []string{"src/main/java/be/elevenways/app/Alpha.java", "src/main/java/be/elevenways/app/Missing.java", "src/main/java/be/elevenways/app/Beta.java",
		"src/main/java/be/elevenways/app/Gamma.java"}
	// The line that names no file breaks the run, and two rows are too few to group.
	expect(t, strings.Join(compactListing(lines, parse), "\n"), strings.Join(lines, "\n"))
}
