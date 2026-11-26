package nibash

import (
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// printfCases are printf calls as format and operands; printfForms turns each into a command line.
func printfCases() [][]string {
	var cases [][]string
	add := func(format string, operands ...string) { cases = append(cases, append([]string{format}, operands...)) }
	for _, format := range []string{"%d", "%i", "%5d", "%-5d|", "%05d", "%+d", "% d", "%+ d", "%.3d", "%.0d|", "%8.3d", "%-08d|", "%+05d", "%x",
		"%#x", "%#X", "%X", "%o", "%#o", "%#.3o", "%#5o", "%#.0o|", "%.0o|", "%u", "%5.0d|", "%#08x", "%-#8o|", "%ld", "%hhd"} {
		for _, operand := range []string{"0", "1", "-1", "42", "-42", "0x1f", "0X1F", "010", "077", "'a", `"b`, "'", "", " 7", "\t-3",
			"9223372036854775807", "-9223372036854775808", "18446744073709551615"} {
			add(format+"\n", operand)
		}
	}
	for _, format := range []string{"%f", "%.2f", "%.0f", "%#.0f", "%e", "%.3e", "%E", "%.0e", "%#.0e", "%g", "%G", "%.3g", "%#g", "%#.3g",
		"%.0g", "%10.3f|", "%-10.2e|", "%+f", "% f", "%010.2f", "%.10g", "%.20f", "%.30f", "%.18e", "%-+12.4G|", "%F"} {
		for _, operand := range []string{"0", "1", "-0", "0.1", "2.5", "3.5", "1e5", "123456789", "0.0001", "0.00001", "1e-5", "1e100",
			"-2.25", "'a", ".5", "5.", "", "2.675", "100000", "999999.5", "1234567.875", " 3.25", "0.000123456", "9.9999999"} {
			add(format+"\n", operand)
		}
	}
	for _, format := range []string{"%s|", "%5s|", "%-5s|", "%.2s|", "%5.1s|", "%05s|", "%.0s|", "%c|", "%5c|", "%-3c|", "%05c|", "%b|",
		"%5b|", "%.3b|", "%q|", "%5q|", "%.3q|", "%Q|", "%.3Q|", "%-8Q|"} {
		for _, operand := range []string{"abc", "", "a b", "x", `a\tb`, `a\cb`, `\0101`, `\101`, `\x41z`, `\x`, `\q`, `\`, `\'`, `\"`,
			`\?`, `\e`, `A`, "it's", "$HOME"} {
			add(format+"\n", operand)
		}
	}
	// What %q backslashes, at the start, in the middle and after = and :, and the control characters it quotes as $'...'.
	for c := byte(0x20); c < 0x7f; c++ {
		add("%q\n", string(c)+"a", "a"+string(c)+"b", "="+string(c), ":"+string(c))
	}
	for _, operand := range []string{"\x01", "a\x1bb", "t\tn\nr\r", "\x7f", "a'b\x02", `back\slash` + "\x03", "\a\b\f\v", "x\x1f"} {
		add("%q\n", operand)
	}
	for _, format := range []string{`a\101\x41\e\?\q\c\'z\n`, `\0101\n`, `\x4g\n`, `\u00\n`, `\U0001F600\n`, `\u00e9\n`, `trail\`, "%%|%s\n",
		"%s %s\n", "no verbs\n", "%5%|\n", "%*d|\n", "%-*d|\n", "%.*f|\n", "%*.*s|\n", "%(%Y)T\n", "%a\n", "%'d\n", "%\n", "%z\n", "%d\n",
		"%s-%d-%c\n"} {
		add(format, "5", "3.14159", "abc", "x")
	}
	for _, operand := range []string{"abc", "1_000", "0b1", "99999999999999999999", "5x", "0x", "-", "1e2"} {
		add("%d\n", operand)
	}
	add("%.2f\n", "1,5")
	add("%f\n", "inf")
	add("%f\n", "0x1p3")
	add("%c|\n")
	add("%s=%s\n", "a", "1", "b")
	return cases
}

// printfForms are the printf calls the test runs, each a command line.
func printfForms() []string {
	var forms []string
	for _, c := range printfCases() {
		words := make([]string, len(c))
		for i, word := range c {
			words[i] = printfQuote(word)
		}
		forms = append(forms, "printf "+strings.Join(words, " "))
	}
	return append(forms,
		`printf -- '%s\n' -x`, `printf -x`, `printf`, `printf -v v '%s-%s' a b; echo "[$v]"`, `printf -v v '%q' 'a b'; echo "[$v]"`,
		`printf -v v 'x\n\n'; echo "[$v]"`, `printf -v v '%d' abc; echo "[$?][$v]"`, `f() { local v; printf -v v '%s' in; echo "in:$v"; }; f; echo "out:$v"`,
		`f() { local v; printf -v v '%.1f' 2.25; echo "in:$v"; }; f; echo "out:$v"`, `printf -v 'a[2]' '%s' two; echo "${a[2]}"`, `printf -v 1x %s a`,
		`printf -v v '%c' ''; echo "[${#v}]"`, `builtin printf '%q\n' 'a b'`, `command printf '%s\n' ok`, `x=$(printf '%q' '$HOME'); echo "$x"`,
		`printf '%s\n' a b | cat`, `printf '%b|%s\n' 'a\cb' never`)
}

// TestPrintfPrintsWhatBashPrints runs each form through nibash and through bash, in the C locale, a UTF-8 one and one with a decimal comma,
// and compares the bytes printed and the status.
func TestPrintfPrintsWhatBashPrints(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	f := newFixture(t, nil)
	locales := map[string][]string{"C": printfForms(), "C.UTF-8": {`printf '\u00e9|%5s|%.1s|%q|%c|%d\n' é é é é "'é"`, `printf '%b\n' '\u00e9'`},
		"nl_BE.UTF-8": {`printf '%.1f|%f\n' 2.25 '2,25'`, `printf '%g %d\n' 1e5 7`}}
	for locale, forms := range locales {
		var script strings.Builder
		for _, form := range forms {
			script.WriteString("{ " + form + "; echo \" [$?]\"; } 2>/dev/null | od -An -c; echo ===\n")
		}
		run := "export LC_ALL=" + locale + "\n" + script.String()
		want, err := exec.Command(bash, "-c", run).Output()
		if err != nil {
			t.Fatalf("bash: %v", err)
		}
		out := filepath.Join(t.TempDir(), "nibash.out")
		f.run("{\n" + run + "} > " + out)
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.TrimSuffix(string(data), "\n")
		wants, gots := strings.Split(string(want), "===\n"), strings.Split(got+"\n", "===\n")
		if len(wants) != len(gots) {
			t.Fatalf("%s: %d outputs from bash, %d from nibash:\n%s", locale, len(wants), len(gots), got)
		}
		for i, form := range forms {
			if strings.Join(strings.Fields(gots[i]), " ") != strings.Join(strings.Fields(wants[i]), " ") {
				t.Errorf("LC_ALL=%s %s\ngot:  %s\nwant: %s", locale, form, gots[i], wants[i])
			}
		}
	}
}

// TestPrintfFormatsBashsFormsItself pins which forms nibash formats itself and which it leaves to bash.
func TestPrintfFormatsBashsFormsItself(t *testing.T) {
	c := printfLocale{point: true}
	for _, form := range [][]string{{"%s\n", "a"}, {"%q", "a b"}, {"%5.2f", "3.14159"}, {"%.3e", "1e5"}, {"%#x", "255"}, {"%*d", "5", "1"},
		{"%b", `a\tb`}, {"%c", ""}, {"%d", "'a"}, {"%Q", "x"}, {`\u00e9`}} {
		if _, done := bashPrintf(form[0], form[1:], printfLocale{point: true, utf8: true}); !done {
			t.Errorf("printf %q: left to bash", form)
		}
	}
	for _, form := range [][]string{{"%d", "abc"}, {"%(%Y)T", "0"}, {"%a", "1"}, {"%'d", "1"}, {"%5s", "é"}, {"%q", "é"}, {"%"}, {`\u00e9`}} {
		if _, done := bashPrintf(form[0], form[1:], c); done {
			t.Errorf("printf %q: formatted by nibash", form)
		}
	}
	if _, done := bashPrintf("%f", []string{"1"}, printfLocale{}); done {
		t.Error("a float outside the C numeric locale must be left to bash")
	}
}

// TestPrintfNumbersMatchBashsAtRandom formats seeded random numbers with each numeric conversion, one bash call per format reusing it for
// every operand, so rounding at the edges of a long double is checked far beyond the cases written out.
func TestPrintfNumbersMatchBashsAtRandom(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	random := rand.New(rand.NewPCG(1, 2))
	var floats, integers []string
	for range 2000 {
		digits := strconv.FormatUint(random.Uint64()>>random.IntN(64), 10)
		point := random.IntN(len(digits) + 1)
		number := digits[:point] + "." + digits[point:]
		if random.IntN(3) == 0 {
			number += "e" + strconv.Itoa(random.IntN(80)-40)
		}
		if random.IntN(4) == 0 {
			number = "-" + number
		}
		floats = append(floats, number)
		value := int64(random.Uint64()) >> random.IntN(64)
		sign, magnitude := "", uint64(value)
		if value < 0 {
			sign, magnitude = "-", -magnitude
		}
		switch random.IntN(3) {
		case 0:
			integers = append(integers, sign+"0x"+strconv.FormatUint(magnitude, 16))
		case 1:
			integers = append(integers, sign+"0"+strconv.FormatUint(magnitude, 8))
		default:
			integers = append(integers, sign+strconv.FormatUint(magnitude, 10))
		}
	}
	check := func(format string, operands []string) {
		want, err := exec.Command(bash, append([]string{"-c", `LC_ALL=C printf "$@"`, "printf", format + "\n"}, operands...)...).Output()
		if err != nil {
			t.Fatalf("bash: %s: %v", format, err)
		}
		got, done := bashPrintf(format+"\n", operands, printfLocale{point: true})
		if !done {
			t.Errorf("%s: left to bash", format)
			return
		}
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Errorf("printf %s %s: got %q, want %q", format, operands[i], gotLines[i], wantLines[i])
				return
			}
		}
		if len(gotLines) != len(wantLines) {
			t.Errorf("%s: %d lines, want %d", format, len(gotLines), len(wantLines))
		}
	}
	for _, format := range []string{"%f", "%.0f", "%.1f", "%.2f", "%.3f", "%.10f", "%.20f", "%.25f", "%e", "%.0e", "%.3e", "%.15e", "%.19e",
		"%.25e", "%g", "%.1g", "%.3g", "%.10g", "%.17g", "%.19g", "%.21g", "%G", "%E", "%+012.4f", "%- 14.6e"} {
		check(format, floats)
	}
	for _, format := range []string{"%d", "%+.5d", "%x", "%#X", "%#o", "%u", "% 022d", "%-#21x"} {
		check(format, integers)
	}
}
