package nibash

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sedText = "a\n## One\nx;y\n(b)\n## Two\nc+\nTAB\there\n## Three\nz/z\n"

// TestSedPrintsTheLinesTheSystemSedPrints runs each script nib's sed takes as its own against the system sed.
func TestSedPrintsTheLinesTheSystemSedPrints(t *testing.T) {
	system, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("sed is not installed")
	}
	file := filepath.Join(t.TempDir(), "r.md")
	if err := os.WriteFile(file, []byte(sedText), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(sedText, "\n"), "\n")
	scripts := [][]string{
		{"/^## One/,$p"}, {"/^## One/,/^## T/p"}, {"/^## T/p"}, {"/^## T/,+1p"}, {"2,/Two/p"}, {"/;/p;1p"}, {"1p;1p"}, {"/(b)/p"}, {"/^## t/Ip"},
		{"/c/,2p"}, {"/^## One/,/^## Two/p;/z/p"}, {"3,+2p"}, {"2, 4p"}, {"4,2p"}, {"/a\\|c/p"}, {"/x.y/,/c\\+/p"}, {"/z\\/z/p"}, {"/^#/,+0p"},
		{"/^## /,/^## /p"}, {"2,4p\n6p"}, {" 2p ; 9 , $ p"}, {"/[#]\\{2\\} T/p"}, {"/c+/p"}, {"/^a$/,/^nothing$/p"}, {"3,3p"}, {"2,0p"}, {"1 ,2p"},
		{"-E", "/^(a|c\\+)$/p"}, {"-E", "/c\\+/,/^## T+/p"}, {"-r", "/x;y|z/p"},
	}
	for _, args := range scripts {
		args = append([]string{"-n"}, args...)
		plan, err := prepareSed("sed", append(args, file))
		if err != nil {
			t.Errorf("sed %q is not nib's own: %v", args, err)
			continue
		}
		var got strings.Builder
		prints := plan.(*sedPlan).prints
		for i, line := range lines {
			for p := range prints {
				if prints[p].prints(i+1, line) {
					got.WriteString(line + "\n")
				}
			}
		}
		want, err := exec.Command(system, append(args, file)...).Output()
		if err != nil {
			t.Errorf("the system sed fails on %q, which nib's takes: %v", args, err)
			continue
		}
		if got.String() != string(want) {
			t.Errorf("sed %q\ngot:\n%swant:\n%s", args, got.String(), want)
		}
	}
}

func TestSedLeavesTheSystemSedWhatItDoesNotRunExactly(t *testing.T) {
	for _, script := range []string{"$p", "0,/a/p", "/a/!p", "//p", "/a\\nb/p", "/a\\tb/p", "/\\(a\\)\\1/p", "1~2p", "/a/,~2p", "/a/Mp", "\\,a,p", "/a/{p}",
		"/a/d", "2q", "/a/p x", "/a\\</p", "/[/]/p", "/a", "s/a/b/p", "2,$ p;="} {
		if _, err := prepareSed("sed", []string{"-n", script, "x"}); err == nil {
			t.Errorf("sed -n %q ran as nib's own", script)
		}
	}
}

func TestSedByPatternPrintsAnchoredRowsAndStopsEarlyOnlyByLineNumbers(t *testing.T) {
	f := newFixture(t, map[string]string{"r.md": sedText})
	out := f.run("sed -n '/^## Two/,/^## Three/p' r.md")
	expect(t, out, fmt.Sprintf("[r.md#%s]\n5:## Two\n6:c+\n7:TAB\there\n8:## Three", tagOf(t, out)))
	out = f.run("sed -n '1p;1p;/^z/p' r.md")
	expect(t, out, fmt.Sprintf("[r.md#%s]\n1:a\n1:a\n9:z/z", tagOf(t, out)))
	for script, last := range map[string]int{"2,4p;7p": 7, "3,+2p": 5, "/a/p": 0, "2,$p": 0, "1,/a/p": 0} {
		plan, err := prepareSed("sed", []string{"-n", script})
		if err != nil || plan.(*sedPlan).last() != last {
			t.Errorf("%q: last %v, err %v, want %d", script, plan, err, last)
		}
	}
}
