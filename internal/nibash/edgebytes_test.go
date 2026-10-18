package nibash

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/engine"
)

// fact matches nib's own notes after a command's output, such as which lines a head or tail showed.
var fact = regexp.MustCompile(`^\[[^\]|]*\]$`)

// shownBytes reads nibash output back into the lines it shows, without nib's notes, a carriage return or a byte order mark.
func shownBytes(out string) []string {
	var lines []string
	for _, line := range shownLines(out) {
		if line == "(no output)" || fact.MatchString(line) {
			continue
		}
		lines = append(lines, strings.TrimSuffix(line, "\r"))
	}
	return lines
}

// TestHeadAndTailCountingBytesPrintWhatTheSystemProgramsPrint runs head -c and tail -c under bash and under nibash on files nib holds (UTF-8,
// with a byte order mark, without a final newline, with CRLF) and on a log it streams, and compares the lines each shows.
func TestHeadAndTailCountingBytesPrintWhatTheSystemProgramsPrint(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	files := map[string]string{
		"utf8.txt":  "plain ascii line\nh\xc3\xa9llo w\xc3\xb6rld \xe2\x82\xac\nemoji \xf0\x9f\x98\x80 here\n\nlast\n",
		"bom.txt":   "\xef\xbb\xbfhead line\nsecond\n",
		"open.txt":  "one\ntwo\nno newline",
		"crlf.txt":  "first\r\nsecond line\r\nthird\r\n",
		"build.log": "compiling\nh\xc3\xa9llo\nerror: x\nok\n",
	}
	f := newFixture(t, files)
	counts := []string{"0", "1", "2", "3", "4", "5", "7", "11", "13", "20", "1000", "+1", "+3", "+9", "+40"}
	for name := range files {
		for _, n := range counts {
			for _, program := range []string{"head", "tail"} {
				if program == "head" && strings.HasPrefix(n, "+") {
					continue
				}
				command := fmt.Sprintf("%s -c %s %s", program, n, name)
				cmd := exec.Command("bash", "-c", command)
				cmd.Dir = f.directory
				real, err := cmd.Output()
				if err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				var want []string
				if len(real) > 0 {
					for _, line := range strings.Split(strings.TrimSuffix(string(real), "\n"), "\n") {
						want = append(want, strings.TrimSuffix(strings.TrimPrefix(engine.PlainText(strings.ToValidUTF8(line, "\ufffd")), "\xef\xbb\xbf"), "\r"))
					}
				}
				out := f.run(command)
				if strings.Contains(out, "system program") {
					t.Errorf("%s ran as the system program: %s", command, out)
				}
				if got := shownBytes(out); strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("%s:\ngot:  %q\nwant: %q\nout: %s", command, got, want, out)
				}
			}
		}
	}
}

func TestHeadAndTailCountingBytesKeepTheRowsTheyShowWholeAndNameWhatTheyCut(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "alpha\nbeta gamma\n", "u.txt": "\xc3\xa9t\xc3\xa9\n"})
	out := f.run("tail -c 8 a.txt")
	tag := tagOf(t, out)
	expect(t, out, fmt.Sprintf("[a.txt#%s]\n[line 2, chars 4-10 of 10]|a gamma\n[a.txt: line 2 of 2]", tag))
	expect(t, f.run("head -c 8 a.txt"), fmt.Sprintf("[a.txt#%s]\n1:alpha\n[line 2, chars 1-2 of 10]|be", tag))
	// Only the newline of a line prints as none of its characters.
	expect(t, f.run("tail -c 1 a.txt"), fmt.Sprintf("[a.txt#%s]\n[line 2, no chars of 10]|\n[a.txt: line 2 of 2]", tag))
	// A row it kept whole anchors an edit; a line it cut was never shown whole.
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 2:\n+beta", tag)); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("a line -c cut must not anchor, got %v", err)
	}
	expect(t, f.run("tail -c +7 a.txt"), fmt.Sprintf("[a.txt#%s]\n2:beta gamma\n[a.txt: line 2 of 2]", tag))
	result, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 1:\n+ALPHA", tag))
	if err != nil {
		t.Fatalf("edit from a row -c kept whole: %v", err)
	}
	tagOf(t, result.Text)
	// A cut inside a character prints the bytes as they are, as plain text.
	if out := f.run("tail -c 5 u.txt"); !strings.Contains(out, "\xef\xbf\xbdt\xc3\xa9") || strings.Contains(out, "[line") {
		t.Fatalf("a cut inside a character, got %s", out)
	}
	if out := f.run("tail -c 3 u.txt"); !strings.Contains(out, "[line 1, chars 3-3 of 3]|\u00e9") {
		t.Fatalf("a cut between characters, got %s", out)
	}
	// Program output and a nib command's output both reach -c as the bytes a program writes.
	for command, want := range map[string]string{
		"printf 'abc\\ndef\\nghi' | tail -c 6": "ef\nghi\n[2 earlier lines in ",
		"printf 'abc\\ndef\\n' | head -c 5":    "abc\nd",
		"cat a.txt | tail -c 8":                "a gamma\n[2 earlier lines in ",
		"{ echo x; cat a.txt; } | head -c 4":   "x\nAL",
	} {
		if out := f.run(command); !strings.HasPrefix(out, want) || strings.Contains(out, "#") {
			t.Errorf("%s: got %s", command, out)
		}
	}
}

func TestATailCountingBytesOfProgramOutputKeepsAllOfItInALog(t *testing.T) {
	f := newFixture(t, nil)
	out := f.run("seq 1 1000 | tail -c 11")
	if !strings.HasPrefix(out, "8\n999\n1000\n[998 earlier lines in ") {
		t.Fatalf("got %s", out)
	}
	expect(t, f.run("seq 1 3 | tail -c 100"), "1\n2\n3")
}

func TestAHeadOfAFileNibDoesNotHoldClaimsNoLineCount(t *testing.T) {
	f := newFixture(t, nil)
	for _, command := range []string{"head -3 a.log", "head -c 6 a.log"} {
		if out := f.run("seq 1 100 > a.log; " + command); out != "1\n2\n3" && !strings.HasPrefix(out, "1\n2\n3\n[changed") || strings.Contains(out, " of ") {
			t.Errorf("%s: got %s", command, out)
		}
	}
	if out := f.run("tail -c 6 a.log"); out != "9\n100\n[a.log: lines 99-100 of 100]" {
		t.Errorf("got %s", out)
	}
}
