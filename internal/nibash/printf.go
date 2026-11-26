package nibash

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// printfName is the bash builtin whose interpreter form covers only part of bash's.
const printfName = "printf"

// printfVariable is a name printf -v may assign: a variable or an array element.
var printfVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?$`)

// printfInteger is an argument the interpreter's numeric conversions read as bash does: decimal, octal with a leading 0, or hex.
var printfInteger = regexp.MustCompile(`^[+-]?(0[xX][0-9a-fA-F]+|0[0-7]*|[1-9][0-9]*)$`)

// printfAt is where printf is in args, also through builtin or command, or -1.
func printfAt(args []string) int {
	at := 0
	if len(args) > 1 && (args[0] == "builtin" || args[0] == "command") {
		at = 1
	}
	if args[at] == printfName {
		return at
	}
	return -1
}

// printfCall runs printf as bash does: the interpreter's builtin for a format it formats exactly as bash, real bash for any other (%q, %f,
// precision, %*d and the rest), and printf -v as an assignment in the caller's scope; args are what follows the printf word.
func printfCall(ctx context.Context, args []string) []string {
	variable, format, operands, ok := printfOperands(args)
	if ok && printfExact(format, operands) {
		if variable == "" {
			return append([]string{printfName, format}, operands...)
		}
		value := printfFormat(format, operands)
		return []string{"eval", variable + "=" + printfQuote(value)}
	}
	hc := interp.HandlerCtx(ctx)
	bash, err := interp.LookPathDir(hc.Dir, hc.Env, "bash")
	if err != nil {
		return append([]string{printfName}, args...)
	}
	if variable == "" {
		return append([]string{bash, "-c", `printf "$@"`, printfName}, args...)
	}
	// Bash's printf -v assigns even when a conversion fails (status 1); its status is 0, 1 or 2, kept past the assignment that strips the x
	// guarding trailing newlines from the command substitution.
	// What follows -v NAME, for bash to print rather than assign.
	args = args[2:]
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = printfQuote(arg)
	}
	script := variable + "=$(" + printfQuote(bash) + ` -c 'printf "$@"; s=$?; printf x; exit $s' printf ` + strings.Join(quoted, " ") + ")\n" +
		"case $? in 0) " + variable + "=${" + variable + "%x} ;; 1) " + variable + "=${" + variable + "%x}; false ;; *) " + variable + "=${" + variable + "%x}; (exit 2) ;; esac"
	return []string{"eval", script}
}

// printfOperands splits bash's printf options from its format and arguments; ok is false for what the interpreter's builtin reads otherwise
// than bash: an option it lacks, -v naming no variable, or no format.
func printfOperands(args []string) (variable, format string, operands []string, ok bool) {
	if len(args) >= 2 && args[0] == "-v" {
		if !printfVariable.MatchString(args[1]) {
			return "", "", nil, false
		}
		variable, args = args[1], args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	} else if len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' {
		return variable, "", nil, false
	}
	if len(args) == 0 {
		return variable, "", nil, false
	}
	return variable, args[0], args[1:], true
}

// printfFormat is what the interpreter's printf prints for format and operands, the format used again while operands are left.
func printfFormat(format string, operands []string) string {
	var out strings.Builder
	for {
		s, n, err := expand.Format(nil, format, operands)
		if err != nil {
			return out.String()
		}
		out.WriteString(s)
		operands = operands[n:]
		if n == 0 || len(operands) == 0 {
			return out.String()
		}
	}
}

// printfExact says whether the interpreter's printf prints exactly what bash's does for format and operands, its passes over the format
// included.
func printfExact(format string, operands []string) bool {
	for {
		n, ok := printfPass(format, operands)
		if !ok {
			return false
		}
		operands = operands[n:]
		if n == 0 || len(operands) == 0 {
			return true
		}
	}
}

// printfPass checks one pass over format against the operands it consumes and returns how many that is.
func printfPass(format string, operands []string) (int, bool) {
	used := 0
	next := func() string {
		if used < len(operands) {
			used++
			return operands[used-1]
		}
		return ""
	}
	for i := 0; i < len(format); i++ {
		switch format[i] {
		case '\\':
			i++
			if i >= len(format) || !strings.ContainsRune(`abeEfnrtv\'"?01234567xuU`, rune(format[i])) {
				return 0, false
			}
		case '%':
			i++
			if i < len(format) && format[i] == '%' {
				continue
			}
			flag := byte(0)
			if i < len(format) && strings.IndexByte("-+ ", format[i]) >= 0 {
				flag = format[i]
				i++
			}
			width := i
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			padded := i > width
			zero := padded && format[width] == '0'
			if i >= len(format) {
				return 0, false
			}
			switch verb := format[i]; verb {
			case 's':
				if arg := next(); flag == '+' || flag == ' ' || zero || (padded && !printfASCII(arg)) {
					return 0, false
				}
			case 'b':
				if flag != 0 || padded || strings.ContainsRune(next(), '\\') {
					return 0, false
				}
			case 'c':
				// The interpreter writes a NUL for a missing operand and one byte of a multibyte letter.
				if arg := next(); flag != 0 || padded || arg == "" || arg[0] >= 0x80 {
					return 0, false
				}
			case 'd', 'i', 'u', 'o', 'x':
				if (flag == '+' || flag == ' ') && verb != 'd' && verb != 'i' {
					return 0, false
				}
				if arg := next(); arg != "" && !printfFits(arg) {
					return 0, false
				}
			default:
				return 0, false
			}
		}
	}
	return used, true
}

// printfFits says whether a numeric operand is one bash and the interpreter both read, as the same 64-bit number.
func printfFits(arg string) bool {
	if !printfInteger.MatchString(arg) {
		return false
	}
	_, err := strconv.ParseInt(arg, 0, 64)
	return err == nil
}

// printfQuote quotes a word for the shell.
func printfQuote(word string) string {
	quoted, err := syntax.Quote(word, syntax.LangBash)
	if err != nil {
		return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return quoted
}

// printfASCII says whether s is ASCII, where a width counts the same letters in bash's bytes as in Go's runes.
func printfASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
