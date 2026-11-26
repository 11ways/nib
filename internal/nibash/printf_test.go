package nibash

import (
	"os/exec"
	"strings"
	"testing"
)

// printfForms are printf calls, each run by nibash and by bash; the interpreter's builtin takes some, real bash the others.
var printfForms = []string{
	`printf 'a b\n'`, `printf '%s\n' one two three`, `printf '%s=%s\n' a 1 b`, `printf '%5s|%-5s|\n' ab cd`, `printf '%d %i %u %o %x\n' 42 -7 9 8 255`,
	`printf '%05d|%-4d|%+d|% d\n' 42 7 3 4`, `printf '%x %d\n' 0x1f 010`, `printf '%d\n' abc`, `printf '%d\n' 1_000`, `printf '%d\n' 0b1`,
	`printf '%d\n' "'a"`, `printf '%d\n' 99999999999999999999`, `printf '%u %x\n' -1 -1`, `printf '%c|\n' hello`, `printf '%c|\n'`,
	`printf '%c|\n' é`, `printf '%5c|\n' a`, `printf '%b\n' 'a\tb'`, `printf '%b|\n' plain`, `printf '%b\n' 'a\cb'`, `printf '%q\n' 'a b' "it's"`,
	`printf '%.2f\n' 3.14159`, `printf '%e %g\n' 1234.5 0.0001`, `printf '%.3s|\n' abcdef`, `printf '%*d|\n' 5 42`, `printf '%X\n' 255`,
	`printf '%#x\n' 255`, `printf '%5s|\n' é`, `printf '%05s|\n' ab`, `printf '%+s|\n' ab`, `printf '\x41é\101\n'`, `printf '\q\n'`,
	`printf '\c' ; printf x`, `printf '%%|%s\n' a`, `printf '%'`, `printf -- '%s\n' -x`, `printf -x`, `printf '%s %s\n' a`, `printf 'no verbs\n' extra`,
	`printf '%(%Y)T\n' 0`, `printf -v v '%s-%s' a b; echo "[$v]"`, `printf -v v '%q' 'a b'; echo "[$v]"`, `printf -v v 'x\n\n'; echo "[$v]"`,
	`printf -v v '%d' abc; echo "[$?][$v]"`, `f() { local v; printf -v v '%s' in; echo "in:$v"; }; f; echo "out:$v"`,
	`f() { local v; printf -v v '%.1f' 2.25; echo "in:$v"; }; f; echo "out:$v"`, `printf -v 'a[2]' '%s' two; echo "${a[2]}"`, `printf -v 1x %s a`,
	`builtin printf '%q\n' 'a b'`, `command printf '%s\n' ok`, `x=$(printf '%q' '$HOME'); echo "$x"`,
}

// TestPrintfPrintsWhatBashPrints runs each form through nibash and through bash and compares the bytes printed and the status.
func TestPrintfPrintsWhatBashPrints(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	f := newFixture(t, nil)
	for _, form := range printfForms {
		script := "{ " + form + "; echo \" [$?]\"; } 2>/dev/null | od -An -c"
		want, err := exec.Command(bash, "-c", script).Output()
		if err != nil {
			t.Fatalf("bash: %s: %v", form, err)
		}
		if got := f.run(script); strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(string(want)), " ") {
			t.Errorf("%s\ngot:  %s\nwant: %s", form, got, want)
		}
	}
}

// TestPrintfLeavesBashWhatTheInterpreterFormatsOtherwise pins which forms the interpreter's builtin keeps.
func TestPrintfLeavesBashWhatTheInterpreterFormatsOtherwise(t *testing.T) {
	for args, want := range map[string]bool{
		"%s\\n a b": true, "%05d 3": true, "%c x": true, "%d 1_000": false, "%q x": false, "%.2f 1": false, "%*d 5 1": false, "%c": false,
		"%5s é": false, "%b a\\tb": false, "\\c": false, "%": false, "-v v %s a": true, "-v 1x %s a": false, "-x": false, "-- %s -x": true,
	} {
		words := strings.Fields(args)
		_, format, operands, ok := printfOperands(words)
		if got := ok && printfExact(format, operands); got != want {
			t.Errorf("printf %s: the interpreter's builtin takes it %v, want %v", args, got, want)
		}
	}
}
