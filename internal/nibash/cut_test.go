package nibash

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/server"
)

func TestCutKeepsTheRowsItLeavesWholeAndNamesWhatItKeptOfTheRest(t *testing.T) {
	long := strings.Repeat("x", 30)
	f := newFixture(t, map[string]string{"a.txt": "short\n" + long + "\nmid line here\n"})
	out := f.run("sed -n 1,3p a.txt | cut -c1-10")
	tag := tagOf(t, out)
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n1:short\n[line 2, chars 1-10 of 30]|xxxxxxxxxx\n[line 3, chars 1-10 of 13]|mid line h", tag))
	// The row cut left whole anchors an edit; a line it shortened was never shown whole.
	result, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 1:\n+SHORT", tag))
	if err != nil {
		t.Fatalf("edit from a row cut kept: %v", err)
	}
	tag = tagOf(t, result.Text)
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 3:\n+mid", tag)); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("a line cut shortened must not anchor, got %v", err)
	}
	// A file operand, a list with a gap and positions past a line's end.
	out = f.run("cut -c1-2,5- a.txt")
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n[line 1, chars 1-2, 5-5 of 5]|SH ... T\n[line 2, chars 1-2, 5-30 of 30]|xx ... %s\n[line 3, chars 1-2, 5-13 of 13]|mi ... line here", tag, long[:26]))
	expect(t, f.run("cut -c 40- a.txt"), fmt.Sprintf("[a.txt#%s]\n[line 1, no chars of 5]|\n[line 2, no chars of 30]|\n[line 3, no chars of 13]|", tag))
	// Program output is cut as plain text, and so is grep's, whose prefixes are part of what cut reads.
	expect(t, f.run("printf 'abcdef\\n' | cut -c2-3,5"), "bce")
	expect(t, f.run("grep -n mid a.txt | cut -c1-6"), "3:mid ")
	if out := f.run("grep -n mid a.txt | cut -c1-60"); !strings.Contains(out, "*3:mid line here") {
		t.Fatalf("a grep row cut keeps whole stays a row: %s", out)
	}
}

func TestCutFormsNibsOwnLacksRunTheSystemProgram(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a:b\n"})
	expect(t, f.run("cut -d: -f2 a.txt"), "b\n[cut ran as the system program: nib's own does not implement this form of it, so its lines are plain output, not edit anchors]")
	if out := f.run("cut -c0 a.txt"); !strings.Contains(out, "numbered from 1") || !strings.HasSuffix(out, "[exit 1]") {
		t.Fatalf("got %s", out)
	}
}

// shownLines reads nibash output back into the lines it shows: a row's text, the stretches a cut row shows joined, a plain line as it is;
// headers and nib's own notes are left out.
func shownLines(out string) []string {
	header := regexp.MustCompile(`^\[[^\]]+#[0-9A-F]{4}\]$`)
	anchor := regexp.MustCompile(`^\d+:`)
	var lines []string
	for _, row := range strings.Split(out, "\n") {
		switch {
		case header.MatchString(row), row == plainFollows, strings.HasPrefix(row, "[exit "):
		case strings.HasPrefix(row, "[line "):
			_, text, _ := strings.Cut(row, "]|")
			lines = append(lines, strings.TrimPrefix(engine.PlainText(strings.ReplaceAll(text, " ... ", "")), "\xef\xbb\xbf"))
		case anchor.MatchString(row):
			_, text, _ := strings.Cut(row, ":")
			lines = append(lines, engine.PlainText(text))
		default:
			lines = append(lines, strings.TrimPrefix(row, "\xef\xbb\xbf"))
		}
	}
	return lines
}

// TestCutPrintsWhatTheSystemCutPrints runs cut -c under bash with the system cut and under nibash on files held whole, a file that is not
// UTF-8 and one with a byte order mark, in a UTF-8 locale and in the C locale, and compares the lines each shows.
func TestCutPrintsWhatTheSystemCutPrints(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	if out, err := exec.Command("cut", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU coreutils") {
		t.Skip("cut is not coreutils' cut")
	}
	files := map[string]string{
		"held.txt":  "plain ascii line\nh\xc3\xa9llo w\xc3\xb6rld \xe2\x82\xac\nemoji \xf0\x9f\x98\x80 here\n\x1b[31mred\x1b[0m text\ntab\tseparated\n\nab\n" + strings.Repeat("0123456789", 30) + "\n",
		"bytes.txt": "bad \xff byte\ntruncated \xe2\x82 seq\nfine line\n",
		"bom.txt":   "\xef\xbb\xbfhead line\nsecond line\n",
	}
	f := newFixture(t, files)
	lists := []string{"1-5", "3", "2-4,8-", "-3", "5-", "1,3,5", "10-20,1-2", "100-", "1-1000", "7-7,7-9"}
	for _, locale := range []string{"LC_ALL=C.UTF-8 ", "LC_ALL=C "} {
		for _, name := range []string{"held.txt", "bytes.txt", "bom.txt"} {
			for _, list := range lists {
				command := fmt.Sprintf("%scut -c %s %s", locale, list, name)
				cmd := exec.Command("bash", "-c", command)
				cmd.Dir = f.directory
				real, err := cmd.Output()
				if err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				var want []string
				for _, line := range strings.Split(strings.TrimSuffix(string(real), "\n"), "\n") {
					// A row leaves a byte order mark out, as read does; what cut kept after it still has to match.
					want = append(want, strings.TrimPrefix(engine.PlainText(strings.ToValidUTF8(line, "�")), "\xef\xbb\xbf"))
				}
				got := shownLines(f.run(command))
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("%s:\ngot:  %q\nwant: %q", command, got, want)
				}
			}
		}
	}
}

func TestNibCommandsInACompoundCommandSendTheirRowsThroughThePipeToANibReader(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers, "b.txt": "alpha\nbeta gamma delta\ngamma\n"})
	// The ranges come from expansions, so only running sed tells it is nib's own; echo's lines keep their place among the rows.
	out := f.run(`for f in a.txt:1-2 b.txt:2-2,3-3; do echo "== $f"; p=${f%%:*}; r=${f#*:}; for rr in ${r//,/ }; do sed -n "${rr/-/,}p" $p; done; done 2>&1 | cut -c1-8`)
	tags := regexp.MustCompile(`#([0-9A-F]{4})\]`).FindAllStringSubmatch(out, -1)
	if len(tags) != 3 {
		t.Fatalf("got %s", out)
	}
	expect(t, out, fmt.Sprintf("== a.txt\n[a.txt#%s]\n1:one\n[line 2, chars 1-8 of 10]|two need\n== b.txt\n[b.txt#%s]\n[line 2, chars 1-8 of 16]|beta gam\n[b.txt#%s]\n3:gamma", tags[0][1], tags[1][1], tags[1][1]))
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[b.txt#%s]\nPUT 3:\n+GAMMA", tags[1][1])); err != nil {
		t.Fatalf("edit from a row that came through the pipe: %v", err)
	}
	// A form only running it rules out runs the system program into the same reader.
	expect(t, f.run(`s='s/one/ONE/p'; { sed -n "$s" a.txt; sed -n 5p a.txt; } | cut -c1-3`), fmt.Sprintf("ONE\n[a.txt#%s]\n[line 5, chars 1-3 of 4]|fiv", tagOf(t, f.run("sed -n 5p a.txt"))))
}

func TestARangeOnlyAnExpansionGivesIsDecidedWhenTheCommandRuns(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	out := f.run(`r=2,3; sed -n "${r}p" a.txt`)
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n2:two needle\n3:three", tagOf(t, out)))
	expect(t, f.run(`r=s/one/ONE/p; sed -n "$r" a.txt`), "ONE\n[sed ran as the system program: nib's own does not implement this form of it, so its lines are plain output, not edit anchors]")
}

func TestRowsPipedIntoARealProgramAreNotedOncePerAgent(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": numbers})
	as := func(session, command string) string {
		t.Helper()
		tools := f.tools()
		tools.Session = session
		result, err := f.shell.Run(context.Background(), session, server.ShellCall{Command: command}, tools)
		if err != nil {
			t.Fatal(err)
		}
		return result.Text
	}
	t.Cleanup(func() { f.shell.Close("t"); f.shell.Close("u") })
	note := pipedText(pipe{"sed", "tr"})
	expect(t, f.run("sed -n 1,2p a.txt | tr a A"), "one\ntwo needle\n"+note)
	expect(t, f.run("sed -n 1,2p a.txt | tr a A"), "one\ntwo needle")
	expect(t, as("t", "for f in a.txt; do sed -n 1p $f; done | tr o O"), "One\n"+pipedText(pipe{"sed", "tr"}))
	// Into nib's own cut the rows stay rows, and output into a file never reaches the agent: neither is noted.
	if out := as("u", "sed -n 1p a.txt | cut -c1-3; sed -n 1p a.txt | tr o O > out.txt"); strings.Contains(out, "plain text") {
		t.Fatalf("got %s", out)
	}
	if out := as("u", "grep needle a.txt | tr e E"); !strings.HasSuffix(out, pipedText(pipe{"grep", "tr"})) {
		t.Fatalf("got %s", out)
	}
}
