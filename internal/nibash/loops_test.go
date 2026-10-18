package nibash

import (
	"strings"
	"testing"
)

// TestLoopsPipedIntoNibCommands runs loops and blocks whose bodies hold pipelines ending in a nib command, piped into another nib command; the
// body's last stage once wrote over the outer pipe's link, which each pass closed, so the second pass panicked.
func TestLoopsPipedIntoNibCommands(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "alpha one\nalpha two\n", "b.txt": "beta one\n"})
	for _, c := range []struct{ command, want string }{
		{"for f in a.txt b.txt; do cat $f | cat; done | cut -c1-5", "[line 2, chars 1-5 of 9]|alpha\n[b.txt#"},
		{"for f in a.txt b.txt; do grep one $f | cat; done | cut -c1-5", "*[line 1, chars 1-5 of 8]|beta "},
		{"for f in a.txt b.txt; do sed -n 1p $f | cat; done | grep a", "*1:alpha one\n[b.txt#"},
		{"{ cat a.txt | cat; cat b.txt | cat; } | cut -c1-3", "[line 1, chars 1-3 of 8]|bet"},
		{"for f in a.txt b.txt; do for g in 1 2; do cat $f | cat; done; done | cut -c1-4", "[line 1, chars 1-4 of 8]|beta"},
		{"for f in a.txt b.txt; do cat $f | head -n 1; done | cut -c1-4", "[line 1, chars 1-4 of 9]|alph\n[b.txt#"},
		{"for f in a.txt b.txt; do cat $f | cat | cat; done 2>&1 | cut -c1-140 | head -n 2", "1:alpha one\n2:alpha two"},
		{"while read f; do cat $f | cat; done < /dev/null | cut -c1-3", "(no output)"},
	} {
		out := f.run(c.command)
		if strings.Contains(out, "internal error") || !strings.Contains(out, c.want) {
			t.Errorf("%s:\n%s", c.command, out)
		}
	}
	// The command the crash was found with: git's output through cat in each pass, the loop's through cut.
	if out := f.run("git init -q -b main && git add -A && git -c user.name=t -c user.email=t@t commit -qm first && echo x >> a.txt && git -c user.name=t -c user.email=t@t commit -qam second"); strings.Contains(out, "internal error") {
		t.Fatal(out)
	}
	for _, command := range []string{
		"for s in alpha beta x; do echo; git log --oneline -S\"$s\" | cat; done 2>&1 | cut -c1-140",
		"for s in alpha beta x; do echo; git log --oneline -S\"$s\" | cat; done | cut -c1-140",
	} {
		if out := f.run(command); strings.Contains(out, "internal error") || strings.Count(out, " first") != 2 || strings.Count(out, " second") != 1 {
			t.Errorf("%s:\n%s", command, out)
		}
	}
}
