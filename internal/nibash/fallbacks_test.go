package nibash

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/fallback"
)

// TestAFallbackIsLoggedByItsFormAndReasonWithoutItsData runs commands that fall back and reads the log their calls wrote: one line per form
// and reason, counted, and none of the text they worked on.
func TestAFallbackIsLoggedByItsFormAndReasonWithoutItsData(t *testing.T) {
	f := newFixture(t, map[string]string{"notes.txt": "secretword here\n"})
	log := filepath.Join(t.TempDir(), "fallbacks.jsonl")
	f.shell.FallingBack(log)
	f.run("for i in 1 2 3; do grep -P secretword notes.txt; done")
	f.run("sed 's/secretword/other/' notes.txt; grep --colour=always secretword notes.txt; grep -o 'x\\1' notes.txt")
	f.run("printf '%(%Y)T|%s\\n' 0 secretword; printf '%d\\n' secretword; printf 'plain %s\\n' fine")
	f.run("x=1; echo $x | wc -l")
	events, err := fallback.Read(log)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fallback.Event{}
	for _, e := range events {
		if strings.Contains(e.Form+e.Reason, "secretword") {
			t.Errorf("the log keeps data: %+v", e)
		}
		if e.Directory != f.directory {
			t.Errorf("directory %q", e.Directory)
		}
		got[e.Command+" | "+e.Reason] = e
	}
	want := map[string]struct {
		kind, form string
		count      int
	}{
		"grep | option -P":             {fallback.Missing, "grep -P ... ...", 3},
		"sed | sed without -n":         {fallback.Missing, "sed s/.../.../ ...", 1},
		"grep | option --color=always": {fallback.Missing, "grep --colour=... ... ...", 1},
		`grep | a backreference \1`:    {fallback.Missing, "grep -o ... ...", 1},
		"printf | %(...)T":             {fallback.Missing, "printf %(...)T %s", 1},
		"printf | a %d operand bash reports as invalid or out of range": {fallback.Error, "printf %d", 1},
	}
	for key, w := range want {
		e, ok := got[key]
		if !ok {
			t.Errorf("no event %q; got %v", key, slices.Sorted(func(yield func(string) bool) {
				for k := range got {
					if !yield(k) {
						return
					}
				}
			}))
			continue
		}
		if e.Kind != w.kind || e.Form != w.form || e.Count != w.count {
			t.Errorf("%s: got %s %q x%d, want %s %q x%d", key, e.Kind, e.Form, e.Count, w.kind, w.form, w.count)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d events, want %d: what nib formats or runs itself is no fallback", len(got), len(want))
	}
}

func TestAShellWithoutALogRecordsNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n"})
	f.run("grep -P a a.txt")
	c := &call{session: &session{shell: f.shell}}
	c.fellBack("grep", fallback.Missing, "r", "grep")
	if c.fallbacks != nil {
		t.Error("a shell with no log counts nothing")
	}
}

func TestFallbackFormsKeepTheShapeAndLeaveOutTheText(t *testing.T) {
	for args, want := range map[string]string{
		"sed -n -e /^##Fix/,$p report.md":       "sed -n -e /^##.../,$p ...",
		"sed -i.bak s/foo/bar/g a.txt":          "sed -i... s/.../.../g ...",
		"grep -epattern --include=*.go -rn x .": "grep -e... --include=... -rn ... ...",
		"grep -- -x -":                          "grep -- ... -",
		"cut -c 1-5 file":                       "cut -c ... ...",
	} {
		words := strings.Fields(args)
		if got := fallbackForm(words[0], words[1:]); got != want {
			t.Errorf("%s: got %q, want %q", args, got, want)
		}
	}
	for format, want := range map[string]string{
		"%5.2f and %q\n":    "printf %5.2f %q",
		"%%|%(%Y-%m)T %*d":  "printf %(...)T %*d",
		`text \u00e9 %-08x`: `printf %-08x \u`,
	} {
		if got := printfForm("", format); got != want {
			t.Errorf("%q: got %q, want %q", format, got, want)
		}
	}
	if got := printfForm("v", "%s"); got != "printf -v ... %s" {
		t.Errorf("-v: %q", got)
	}
}
