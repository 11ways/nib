package nibash

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"

	"github.com/11ways/nib/internal/names"
)

func TestScriptsAreFoundInShellsWrappersFindAndXargs(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []int
	}{
		{[]string{"bash", "-c", "s"}, []int{2}},
		{[]string{"bash", "-lc", "s", "name", "arg"}, []int{2}},
		{[]string{"bash", "-o", "pipefail", "-c", "s"}, []int{4}},
		{[]string{"bash", "-e", "--norc", "-c", "s"}, []int{4}},
		{[]string{"/bin/sh", "-c", "s"}, []int{2}},
		{[]string{"dash", "-c", "--", "s"}, []int{3}},
		{[]string{"bash", "script.sh"}, nil},
		{[]string{"bash", "+c", "s"}, nil},
		{[]string{"timeout", "590", "bash", "-c", "s"}, []int{4}},
		{[]string{"timeout", "-k", "5", "-sKILL", "10", "bash", "-c", "s"}, []int{7}},
		{[]string{"timeout", "--kill-after=5", "--signal", "TERM", "10", "sh", "-c", "s"}, []int{7}},
		{[]string{"env", "-i", "X=1", "Y=2", "bash", "-c", "s"}, []int{6}},
		{[]string{"env", "-", "-u", "Z", "zsh", "-c", "s"}, []int{6}},
		{[]string{"env", "-S", "bash -c s"}, nil},
		{[]string{"nice", "-n", "5", "nohup", "stdbuf", "-oL", "setsid", "-w", "bash", "-c", "s"}, []int{10}},
		{[]string{"nice", "-5", "sh", "-c", "s"}, []int{4}},
		{[]string{"xargs", "-0", "-I{}", "-P", "4", "sh", "-c", "s"}, []int{7}},
		{[]string{"xargs", "-i", "--max-args", "1", "sh", "-c", "s"}, []int{6}},
		{[]string{"find", ".", "-name", "*.go", "-exec", "sh", "-c", "s", "{}", ";", "-o", "-execdir", "bash", "-c", "t", "{}", "+"}, []int{7, 14}},
		{[]string{"find", "-L", ".", "-newermt", "2026-01-01", "-print0", "-exec", "timeout", "5", "sh", "-c", "s", ";"}, []int{11}},
		{[]string{"find", ".", "-weird", "-exec", "sh", "-c", "s", ";"}, nil},
		{[]string{"find", ".", "-exec", "sh", "-c", "s"}, nil},
		{[]string{"sudo", "bash", "-c", "s"}, nil},
		{[]string{"timeout", "--preserve", "5", "bash", "-c", "s"}, nil},
	} {
		if got := scriptsIn(c.args, 0); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.args, got, c.want)
		}
	}
}

func TestNamesExpandInAScriptWhereNibashWouldExpandThem(t *testing.T) {
	view := (&names.Table{Entries: []names.Entry{{Name: "nm", Directory: "/d"}, {Name: "sp", Directory: "/a b"}}}).For(names.NewDefined(), "s")
	env := named{expand.ListEnviron(), view}
	for script, want := range map[string]string{
		"cat ~nm/a":                            "cat /d/a",
		"cd ~nm && x=~nm/b ls ~nm/c* > ~nm/o":  "cd /d && x=/d/b ls /d/c* > /d/o",
		"cat ~sp/a":                            "cat '/a b'/a",
		"cat ~nm/\"a b\" \"${u:-~nm/v}\"":      "cat /d/\"a b\" \"${u:-/d/v}\"",
		"f() {\n\tcat ~nm/x\n}\nf":             "f() {\n\tcat /d/x\n}\nf",
		"echo \"$(cat ~nm/a)\"":                "echo \"$(cat /d/a)\"",
		"cat <<~nm/\n~nm/a\n~nm/":              "cat <<~nm/\n~nm/a\n~nm/",
		"cat '~nm/a' \"~nm/a\" x~nm/a ~nm'/a'": "cat '~nm/a' \"~nm/a\" x~nm/a ~nm'/a'",
		"cat ~other/a ~/a ~root/a":             "cat ~other/a ~/a ~root/a",
		"cat ~nm/a; if":                        "cat ~nm/a; if",
		"cd ~/x  &&  echo   'a'  # c ~nm/":     "cd ~/x  &&  echo   'a'  # c ~nm/",
	} {
		if got := expandNames(env, script); got != want {
			t.Errorf("%q: got %q, want %q", script, got, want)
		}
	}
}

func TestShellsTheCommandStartsGetTheNamesExpanded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t, map[string]string{"d/a.txt": "x\n"})
	f.names = (&names.Table{Base: f.directory}).For(names.NewDefined(), "s")
	expect(t, f.run("hash -d nm=d"), "[~nm now names "+filepath.Join(f.directory, "d")+"]")
	// The sketerm session's command: a function made in nibash, sent as text into a real bash behind timeout.
	expect(t, f.run(`wait_for() { cat ~nm/a.txt; }; timeout 5 bash -c "$(declare -f wait_for); wait_for"`), "x")
	expect(t, f.run("bash -lc 'cat ~nm/a.txt'"), "x")
	expect(t, f.run("bash -o pipefail -c 'cat ~nm/a.txt | cat'"), "x")
	expect(t, f.run("sh -c 'cat ~nm/a.txt'"), "x")
	expect(t, f.run("env X=1 bash -c 'echo $X; cat ~nm/a.txt'"), "1\nx")
	expect(t, f.run(`find d -name a.txt -exec sh -c 'cat ~nm/a.txt' \;`), "x\n[find ran as the system program: nib's own does not implement this form of it, so its output is as find printed it]")
	expect(t, f.run("echo a.txt | xargs -I{} sh -c 'cat ~nm/{}'"), "x")
	// A quoted name stays as written, as nibash leaves it.
	f.run(`bash -c "printf '%s %s' '~nm/a.txt' ~nm/a.txt > out.txt"`)
	data, err := os.ReadFile(filepath.Join(f.directory, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, string(data), "~nm/a.txt "+filepath.Join(f.directory, "d", "a.txt"))
	// A script with no names reaches the shell byte for byte.
	if got := f.run(`bash -c 'printf "%s|" "$0"  ~/x' 'a  b'`); !strings.HasPrefix(got, "a  b|") {
		t.Errorf("got %q", got)
	}
}
