package nibash

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// historyFixture is a repository whose user.email is t@t, with commits at fixed moments: "first" and "second" by t@t, "third" by o@o, and
// a.go changed since, while headers print in a zone four hours behind UTC.
func historyFixture(t *testing.T) (*fixture, []string) {
	t.Helper()
	headerZone = time.FixedZone("", -4*3600)
	t.Cleanup(func() { headerZone = nil })
	at := func(moment string) {
		t.Setenv("GIT_AUTHOR_DATE", moment)
		t.Setenv("GIT_COMMITTER_DATE", moment)
	}
	at("2026-10-15T09:00:00+02:00")
	f := gitFixture(t, map[string]string{"a.go": "package a\n\nfunc A() {}\n\nfunc B() {}\n", "d.txt": "one\n"})
	f.git("config", "user.email", "t@t")
	at("2026-10-16T10:38:01+02:00")
	f.write("d.txt", "two\n")
	f.git("commit", "-qam", "second\n\nbody line")
	// 01:30 at +02:00 is the evening before at -04:00.
	at("2026-10-17T01:30:00+02:00")
	f.write("d.txt", "three\n")
	f.git("commit", "-qam", "third", "--author", "O <o@o>")
	f.write("a.go", "package a\n\nfunc A() { return }\n\nfunc B() {}\n")
	return f, strings.Fields(f.git("log", "--format=%h"))
}

func TestGitLogPrintsEachCommitHeaderAsOneLine(t *testing.T) {
	f, hashes := historyFixture(t)
	want := fmt.Sprintf("commit %s o@o Fri 2026-10-16 19:30\nthird\n\ncommit %s Fri 2026-10-16 04:38\nsecond\n\nbody line\n\ncommit %s Thu 2026-10-15 03:00\nfirst",
		hashes[0], hashes[1], hashes[2])
	// The first header left without its author's email says once what that means.
	expect(t, f.run("git log"), want+"\n"+ownAuthorLesson)
	expect(t, f.run("git log"), want)
	// The repository's user.email is the one git runs in, as -C and -c give it.
	expect(t, f.run("cd /tmp && git -C "+f.directory+" log -1 HEAD~1"), fmt.Sprintf("commit %s Fri 2026-10-16 04:38\nsecond\n\nbody line", hashes[1]))
	expect(t, f.run("git -c user.email=o@o log -2"), fmt.Sprintf("commit %s Fri 2026-10-16 19:30\nthird\n\ncommit %s t@t Fri 2026-10-16 04:38\nsecond\n\nbody line", hashes[0], hashes[1]))
	// Decorations stay on the commit line, and a merge's parents on their own.
	expect(t, f.run("git log -1 --decorate"), fmt.Sprintf("commit %s (HEAD -> main) o@o Fri 2026-10-16 19:30\nthird", hashes[0]))
	f.git("checkout", "-q", "-b", "side", "HEAD~1")
	f.write("side.txt", "s\n")
	f.git("add", "side.txt")
	f.git("commit", "-qm", "side")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--no-ff", "side", "-m", "merge side")
	parents := strings.Fields(f.git("log", "-1", "--format=%p"))
	merge := strings.TrimSpace(f.git("log", "-1", "--format=%h"))
	expect(t, f.run("git log -1"), fmt.Sprintf("commit %s Fri 2026-10-16 19:30\nMerge: %s %s\nmerge side", merge, parents[0], parents[1]))
}

func TestGitHeadersInOtherFormatsPrintAsGitPrintsThem(t *testing.T) {
	f, _ := historyFixture(t)
	raw := func(args ...string) string { return strings.TrimSuffix(f.git(args...), "\n") }
	for command, want := range map[string]string{
		"git log --oneline":                     raw("log", "--oneline"),
		"git log --format=%s%n%an":              raw("log", "--format=%s%n%an"),
		"git log --graph":                       raw("log", "--graph"),
		"git log --date=iso":                    raw("log", "--date=iso"),
		"git log --pretty=fuller -1 --stat":     strings.Replace(raw("log", "--pretty=fuller", "-1", "--stat"), " d.txt | 2 +-", " d.txt +1 -1", 1),
		"git -c log.date=iso log -1":            raw("-c", "log.date=iso", "log", "-1", "--abbrev-commit"),
		"git -c log.date=relative log -1 | cat": raw("-c", "log.date=relative", "log", "-1", "--abbrev-commit"),
	} {
		expect(t, f.run(command), want)
	}
	// A tag's own header stays; the commit's under it is one line.
	f.git("tag", "-a", "v1", "-m", "the tag", "HEAD~1")
	out := f.run("git show --stat v1")
	if !strings.HasPrefix(out, "tag v1\nTagger: t <t@t>\nDate:   ") || !strings.Contains(out, "\nthe tag\n\ncommit ") || !strings.Contains(out, " Fri 2026-10-16 04:38\nsecond\n") {
		t.Errorf("show of a tag:\n%s", out)
	}
}

func TestGitThroughNibsHeadAndTailPrintsAsAtTheTerminal(t *testing.T) {
	headerZone = time.FixedZone("", -4*3600)
	t.Cleanup(func() { headerZone = nil })
	t.Setenv("GIT_AUTHOR_DATE", "2026-10-16T10:38:01+02:00")
	t.Setenv("GIT_COMMITTER_DATE", "2026-10-16T10:38:01+02:00")
	f := operationFixture(t)
	f.git("config", "user.email", "t@t")
	f.git("commit", "-qam", "change")
	hash := strings.TrimSpace(f.git("log", "-1", "--format=%h"))
	header := "commit " + hash + " Fri 2026-10-16 04:38"
	// head counts git's lines, those a header's one line stands for included: five are the newest commit's header and message.
	expect(t, f.run("git log | head -5"), header+"\nchange\n"+ownAuthorLesson)
	expect(t, f.run("git log -1 | head -n -1 | cat"), header)
	// A header head or tail cut prints as git printed it, its message with git's indent.
	expect(t, f.run("git log | head -2"), "commit "+hash+"\nAuthor: t <t@t>")
	expect(t, f.run("git log -1 | tail -n 1"), "    change")
	// The command the user's agent ran: git show --stat of several commits through head, in a list. What head keeps prints as git's own
	// output at the terminal would: one line a header, every path whole and each folder once.
	whole := f.run("git show --stat HEAD~1 HEAD")
	piped := f.run("git show --stat HEAD~1 HEAD | head -80; echo after")
	expect(t, piped, whole+"\nafter")
	if !strings.Contains(piped, header+"\nchange\n\n a.go +1 -1\n["+operation+"]\n   Operation.java +1 -1\n") {
		t.Errorf("show --stat through head:\n%s", piped)
	}
	// Through a loop's pipe into head too.
	expect(t, f.run("for c in HEAD; do git show --stat $c; done | head -80"), f.run("git show --stat HEAD"))
}

func TestGitDiffThroughHeadAndTailPrintsTheRowsTheyKept(t *testing.T) {
	f, _ := historyFixture(t)
	head := f.run("git diff | head -8")
	tag := tagOf(t, head)
	expect(t, head, fmt.Sprintf("[a.go#%s]\n@@ -1,5 +1,5 @@\n 1:package a\n 2:\n-|func A() {}", tag))
	expect(t, f.run("git diff | tail -3"), fmt.Sprintf("[a.go#%s]\n+3:func A() { return }\n 4:\n 5:func B() {}", tag))
	// The rows are the file's, so an edit anchors on them with no read.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.go#%s]\nPUT 3:\n+func A() { return 1 }", tag)); err != nil {
		t.Fatalf("edit from a row through tail: %v", err)
	}
	// Only a file part's own header lines kept print as git printed them.
	if out := f.run("git diff | head -2"); !strings.HasPrefix(out, "diff --git a/a.go b/a.go\nindex ") || strings.Count(out, "\n") != 1 {
		t.Errorf("diff header through head: %q", out)
	}
	// A log with patches through tail: rows of the file where it still holds the line.
	if out := f.run("git log -p -1 HEAD -- d.txt | tail -2"); !strings.HasPrefix(out, "[d.txt#") || !strings.HasSuffix(out, "\n-|two\n+1:three") {
		t.Errorf("log -p through tail: %q", out)
	}
}

func TestGitIntoAnythingElseGetsGitsOwnLines(t *testing.T) {
	f, hashes := historyFixture(t)
	full := strings.TrimSpace(f.git("log", "-1", "--format=%H"))
	expect(t, f.run("git log -1 | grep Author"), "Author: O <o@o>")
	expect(t, f.run("git log -1 | head -c 7"), "commit ")
	expect(t, f.run("git log -1 | wc -l"), "5")
	expect(t, f.run("git log -1 | sed -n 1p"), "commit "+full)
	expect(t, f.run("git log -1 | cut -c1-14"), "commit "+full[:7]+"\nAuthor: O <o@o\nDate:   Sat Oc\n\n    third")
	expect(t, f.run("git log -1 | cat -n | head -1"), "     1\tcommit "+full)
	expect(t, f.run("git log -1 | /usr/bin/head -1"), "commit "+full)
	expect(t, f.run("echo $(git log -1 | head -1)"), "commit "+full)
	// A loop's pipe into grep reads git's diff lines, as the system git prints them.
	expect(t, f.run("{ git diff; } | grep '^+'"), "+++ b/a.go\n+func A() { return }")
	expect(t, f.run("for r in HEAD; do git show $r -- d.txt; done | grep -c '^[-+]'"), "4")
	if hashes[0] != full[:len(hashes[0])] {
		t.Fatalf("abbreviated %s, full %s", hashes[0], full)
	}
}

func TestGitStatRowsGiveTheExactCountsTheirBarsDraw(t *testing.T) {
	var long, short strings.Builder
	for i := range 300 {
		fmt.Fprintf(&long, "line %d\n", i)
	}
	for i := range 40 {
		fmt.Fprintf(&short, "kept %d\n", i)
	}
	f := gitFixture(t, map[string]string{"big.txt": long.String(), "gone.txt": short.String(), "old name.txt": "same\n"})
	var grown strings.Builder
	for i := range 300 {
		if i%3 == 0 {
			fmt.Fprintf(&grown, "line %d\n", i)
		}
		fmt.Fprintf(&grown, "new %d\n", i)
	}
	f.write("big.txt", grown.String())
	f.write("gone.txt", "kept 0\n")
	f.write("added.txt", strings.Repeat("a\n", 25))
	f.git("mv", "old name.txt", "new name.txt")
	f.git("add", "-A")
	// More changed lines than git's bars are wide, a file only grown or only shrunk, and a rename whose path is no plain path: each row's
	// exact counts, from what git drew.
	out := f.run("git diff --cached --stat")
	for _, row := range []string{" added.txt +25\n", " big.txt +300 -200\n", " gone.txt -39\n", " old name.txt => new name.txt 0\n"} {
		if !strings.Contains(out, row) {
			t.Errorf("no %q in:\n%s", row, out)
		}
	}
	if strings.Contains(out, "|") || strings.Contains(out, "++") {
		t.Errorf("a bar is left:\n%s", out)
	}
	// Through head too, which keeps as many of git's lines.
	expect(t, f.run("git diff --cached --stat | head -2"), " added.txt +25\n big.txt +300 -200")
	// Into anything that reads text go git's own lines, bars and all.
	expect(t, f.run("git diff --cached --stat | grep -c '|'"), "4")
	// A width the command line asks for prints as git prints it.
	if sized := f.run("git diff --cached --stat=60"); !strings.Contains(sized, " big.txt") || !strings.Contains(sized, "| 500 +") {
		t.Errorf("sized stat:\n%s", sized)
	}
}

func TestHeadPrintsAllButTheLastLines(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\n"})
	out := f.run("head -n -2 a.txt")
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n1:one\n2:two\n[a.txt: lines 1-2 of 4]", tagOf(t, out)))
	expect(t, f.run("printf 'a\\nb\\nc\\n' | head --lines=-1"), "a\nb")
	expect(t, f.run("printf 'a\\nb\\n' | head -n -5"), "(no output)")
	// All but the last bytes is the system head's.
	expect(t, f.run("printf 'abc' | head -c -1"), "ab")
}
