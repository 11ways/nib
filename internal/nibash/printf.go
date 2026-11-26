package nibash

import (
	"context"
	"math/big"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/11ways/nib/internal/fallback"
)

// printfName is the bash builtin nibash formats itself, as the interpreter's covers only part of bash's.
const printfName = "printf"

// printfVariable is a name printf -v may assign: a variable or an array element.
var printfVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?$`)

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

// printfCall runs printf as bash does: formatted by nibash (bashPrintf), printed through the interpreter's %s, or for what bashPrintf leaves
// to bash, by real bash's printf; printf -v assigns through eval in the caller's scope. args are what follows the printf word.
func printfCall(ctx context.Context, args []string) []string {
	hc := interp.HandlerCtx(ctx)
	variable, format, operands, ok := printfOperands(args)
	kind, reason := fallback.Error, "an option printf lacks, or no format"
	if len(args) >= 2 && args[0] == "-v" && !printfVariable.MatchString(args[1]) {
		reason = "-v with a name bash reports as invalid"
	}
	if ok {
		p := runPrintf(format, operands, localeOf(hc.Env))
		out := p.out.String()
		switch {
		case p.left == "" && variable == "":
			return []string{printfName, "%s", out}
		case p.left == "" && !strings.ContainsRune(out, 0):
			return []string{"eval", variable + "=" + printfQuote(out)}
		case p.left == "":
			kind, reason = fallback.Machine, "-v of output holding a NUL byte"
		default:
			kind, reason = p.kind, p.left
		}
	}
	if c := callOf(ctx); c != nil {
		c.fellBack(printfName, kind, reason, printfForm(variable, format))
	}
	bash, err := interp.LookPathDir(hc.Dir, hc.Env, "bash")
	if err != nil {
		return append([]string{printfName}, args...)
	}
	if variable == "" {
		return append([]string{bash, "-c", `printf "$@"`, printfName}, args...)
	}
	// Bash's printf -v assigns even when a conversion fails (status 1); its status is 0, 1 or 2, kept past the assignment that strips the x
	// guarding trailing newlines from the command substitution. What follows -v NAME is for bash to print rather than assign.
	args = args[2:]
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = printfQuote(arg)
	}
	script := variable + "=$(" + printfQuote(bash) + ` -c 'printf "$@"; s=$?; printf x; exit $s' printf ` + strings.Join(quoted, " ") + ")\n" +
		"case $? in 0) " + variable + "=${" + variable + "%x} ;; 1) " + variable + "=${" + variable + "%x}; false ;; *) " + variable + "=${" + variable + "%x}; (exit 2) ;; esac"
	return []string{"eval", script}
}

// printfOperands splits bash's printf options from its format and arguments; ok is false for what bash reads otherwise or refuses: an
// option it lacks, -v naming no variable, or no format.
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

// printfLocale is what of the locale bash's printf output depends on.
type printfLocale struct {
	// point is whether numbers are read and written with a decimal point, as in the C locale; any other numeric locale is left to bash.
	point bool
	// utf8 is whether \u and \U escapes print UTF-8.
	utf8 bool
}

// localeOf reads the locale categories printf uses from env, as the C library does: LC_ALL, then the category's own, then LANG.
func localeOf(env expand.Environ) printfLocale {
	category := func(name string) string {
		for _, variable := range []string{"LC_ALL", name, "LANG"} {
			if value := env.Get(variable).String(); value != "" {
				return value
			}
		}
		return ""
	}
	numeric := category("LC_NUMERIC")
	ctype := strings.ToLower(category("LC_CTYPE"))
	return printfLocale{
		point: numeric == "" || numeric == "C" || numeric == "POSIX" || strings.HasPrefix(numeric, "C."),
		utf8:  strings.Contains(ctype, "utf-8") || strings.Contains(ctype, "utf8"),
	}
}

// longDoubleBits is the mantissa of the C long double bash's printf reads and formats floats in, on the platforms its value was checked
// against bash on; 0 elsewhere, where floats are left to bash.
var longDoubleBits = map[string]uint{"linux/amd64": 64}[runtime.GOOS+"/"+runtime.GOARCH]

// bashPrintf is what bash's printf prints for format and operands, the format used again while operands are left; done is false for what
// it leaves to bash: an error bash would report, a conversion it lacks (%a, %(...)T), the ' flag, float input outside the C numeric locale,
// and text past ASCII where bash's widths, precisions, quoting or character codes follow the locale.
func bashPrintf(format string, operands []string, locale printfLocale) (out string, done bool) {
	p := runPrintf(format, operands, locale)
	return p.out.String(), p.left == ""
}

// runPrintf formats as bashPrintf does; a printfer left set is a call left to bash, which says why.
func runPrintf(format string, operands []string, locale printfLocale) *printfer {
	p := &printfer{format: format, operands: operands, locale: locale}
	for {
		used := p.at
		if !p.pass() {
			if p.left == "" {
				p.leave(fallback.Missing, "a form nib's printf lacks")
			}
			return p
		}
		if p.stopped || p.at == used || p.at >= len(p.operands) {
			return p
		}
	}
}

type printfer struct {
	format   string
	operands []string
	at       int
	locale   printfLocale
	out      strings.Builder
	// stopped is set by \c in a %b operand, which ends all output.
	stopped bool
	// left and kind are why and how the call was left to bash, for the fallback log; "" while nibash formats it.
	left, kind string
}

// leave notes why the call goes to bash, the first reason found, and returns false for the caller to give up with.
func (p *printfer) leave(kind, reason string) bool {
	if p.left == "" {
		p.left, p.kind = reason, kind
	}
	return false
}

// leaveAt is leave for a conversion's caller, which takes how many bytes the conversion used too.
func (p *printfer) leaveAt(kind, reason string) (int, bool) { return 0, p.leave(kind, reason) }

// utf8Escape is the reason \u past ASCII is left to bash.
const utf8Escape = `\u or \U past ASCII outside a UTF-8 locale`

// next is the next operand, "" past the last.
func (p *printfer) next() (string, bool) {
	if p.at < len(p.operands) {
		p.at++
		return p.operands[p.at-1], true
	}
	return "", false
}

// number is the next numeric operand, "0" past the last, as bash reads a missing one; an empty one bash reports.
func (p *printfer) number() string {
	if arg, ok := p.next(); ok {
		return arg
	}
	return "0"
}

// pass prints format once; false leaves the whole call to bash.
func (p *printfer) pass() bool {
	f := p.format
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case '\\':
			text, n, ok := printfEscape(f[i+1:], false, p.locale)
			if !ok {
				return p.leave(fallback.Machine, utf8Escape)
			}
			p.out.WriteString(text)
			i += n
		case '%':
			n, ok := p.conversion(f[i+1:])
			if !ok {
				return false
			}
			if p.stopped {
				return true
			}
			i += n
		default:
			p.out.WriteByte(f[i])
		}
	}
	return true
}

// printfSpec is one conversion's flags, width and precision.
type printfSpec struct {
	minus, plus, space, alt, zero bool
	width, precision              int
	// precise is whether a precision was given.
	precise bool
}

// conversion prints the conversion spec starts, after its %, and returns how many bytes of spec it took.
func (p *printfer) conversion(spec string) (int, bool) {
	i := 0
	if i < len(spec) && spec[i] == '%' {
		p.out.WriteByte('%')
		return 1, true
	}
	var s printfSpec
flags:
	for ; i < len(spec); i++ {
		switch spec[i] {
		case '-':
			s.minus = true
		case '+':
			s.plus = true
		case ' ':
			s.space = true
		case '#':
			s.alt = true
		case '0':
			s.zero = true
		case '\'':
			return p.leaveAt(fallback.Missing, "the ' flag")
		default:
			break flags
		}
	}
	number := func() (int, bool) {
		if i < len(spec) && spec[i] == '*' {
			i++
			arg := p.number()
			value, ok := printfSigned(arg)
			if !ok || value < -(1<<31) || value >= 1<<31 {
				return 0, p.leave(fallback.Error, "a * width or precision bash reports as invalid")
			}
			return int(value), true
		}
		start := i
		for i < len(spec) && spec[i] >= '0' && spec[i] <= '9' {
			i++
		}
		if i-start > 9 {
			return 0, p.leave(fallback.Missing, "a width or precision past 9 digits")
		}
		value, _ := strconv.Atoi(spec[start:i])
		return value, true
	}
	width, ok := number()
	if !ok {
		return 0, false
	}
	if width < 0 {
		s.minus, width = true, -width
	}
	s.width = width
	if i < len(spec) && spec[i] == '.' {
		i++
		precision, ok := number()
		if !ok {
			return 0, false
		}
		// A negative precision from * is none, as C reads it.
		s.precision, s.precise = precision, precision >= 0
	}
	for i < len(spec) && strings.IndexByte("hjlLtz", spec[i]) >= 0 {
		i++
	}
	if i >= len(spec) {
		return p.leaveAt(fallback.Error, "a % without its conversion")
	}
	verb := spec[i]
	i++
	switch verb {
	case 's':
		arg, _ := p.next()
		return i, p.text(arg, s, s.precise)
	case 'b':
		arg, _ := p.next()
		var expanded strings.Builder
		for j := 0; j < len(arg); j++ {
			if arg[j] != '\\' {
				expanded.WriteByte(arg[j])
				continue
			}
			if j+1 < len(arg) && arg[j+1] == 'c' {
				p.stopped = true
				break
			}
			text, n, ok := printfEscape(arg[j+1:], true, p.locale)
			if !ok {
				return p.leaveAt(fallback.Machine, utf8Escape)
			}
			expanded.WriteString(text)
			j += n
		}
		return i, p.text(expanded.String(), s, s.precise)
	case 'q', 'Q':
		arg, _ := p.next()
		if verb == 'Q' && s.precise && s.precision < len(arg) {
			if !printfASCII(arg) {
				return p.leaveAt(fallback.Machine, "%Q with a precision, of text past ASCII")
			}
			arg = arg[:s.precision]
		}
		quoted, ok := bashQuote(arg, s.alt)
		if !ok {
			if s.alt {
				return p.leaveAt(fallback.Missing, "%#q")
			}
			return p.leaveAt(fallback.Machine, "%q of text past ASCII")
		}
		return i, p.text(quoted, s, verb == 'q' && s.precise)
	case 'c':
		arg, _ := p.next()
		if arg != "" && arg[0] >= 0x80 {
			return p.leaveAt(fallback.Machine, "%c of a letter past ASCII")
		}
		var c string
		if arg != "" {
			c = arg[:1]
		} else {
			c = "\x00"
		}
		p.out.WriteString(printfPad(c, s.width, s.minus, false, ""))
		return i, true
	case 'd', 'i':
		arg := p.number()
		value, ok := printfSigned(arg)
		if !ok {
			return p.leaveAt(fallback.Error, "a %d operand bash reports as invalid or out of range")
		}
		sign := ""
		magnitude := uint64(value)
		if value < 0 {
			sign, magnitude = "-", -magnitude
		} else if s.plus {
			sign = "+"
		} else if s.space {
			sign = " "
		}
		p.out.WriteString(printfInteger(strconv.FormatUint(magnitude, 10), sign, "", s, false))
		return i, true
	case 'u', 'o', 'x', 'X':
		arg := p.number()
		value, ok := printfUnsigned(arg)
		if !ok {
			return p.leaveAt(fallback.Error, "a %u, %o or %x operand bash reports as invalid or out of range")
		}
		base := map[byte]int{'u': 10, 'o': 8, 'x': 16, 'X': 16}[verb]
		digits := strconv.FormatUint(value, base)
		prefix := ""
		switch {
		case verb == 'X':
			digits = strings.ToUpper(digits)
			if s.alt && value != 0 {
				prefix = "0X"
			}
		case verb == 'x' && s.alt && value != 0:
			prefix = "0x"
		}
		p.out.WriteString(printfInteger(digits, "", prefix, s, verb == 'o' && s.alt))
		return i, true
	case 'e', 'E', 'f', 'F', 'g', 'G':
		arg := p.number()
		text, ok := p.float(arg, verb, s)
		if !ok {
			return 0, false
		}
		p.out.WriteString(text)
		return i, true
	}
	switch verb {
	case 'a', 'A':
		return p.leaveAt(fallback.Missing, "%a")
	case '(':
		return p.leaveAt(fallback.Missing, "%(...)T")
	}
	return p.leaveAt(fallback.Error, "a conversion bash does not know")
}

// text prints a string conversion: cut to the precision when cut, then padded to the width, as bash counts both in bytes.
func (p *printfer) text(text string, s printfSpec, cut bool) bool {
	if (cut || s.width > 0) && !printfASCII(text) {
		return p.leave(fallback.Machine, "a width or precision of text past ASCII")
	}
	if cut && s.precision < len(text) {
		text = text[:s.precision]
	}
	p.out.WriteString(printfPad(text, s.width, s.minus, false, ""))
	return true
}

// printfInteger lays out an integer's digits as C does, with its sign or 0x prefix: the precision as the least digits, a precision of 0
// printing no digit for 0, zeros in place of spaces only without a precision, and with leadingZero (%#o) a first digit of 0.
func printfInteger(digits, sign, prefix string, s printfSpec, leadingZero bool) string {
	if s.precise {
		if s.precision == 0 && digits == "0" {
			digits = ""
		}
		if len(digits) < s.precision {
			digits = strings.Repeat("0", s.precision-len(digits)) + digits
		}
	}
	if leadingZero && !strings.HasPrefix(digits, "0") {
		digits = "0" + digits
	}
	return printfPad(digits, s.width, s.minus, s.zero && !s.precise, sign+prefix)
}

// printfPad pads lead+body to width: spaces after it with minus, else zeros between lead and body with zero, else spaces before.
func printfPad(body string, width int, minus, zero bool, lead string) string {
	room := width - len(lead) - len(body)
	switch {
	case room <= 0:
		return lead + body
	case minus:
		return lead + body + strings.Repeat(" ", room)
	case zero:
		return lead + strings.Repeat("0", room) + body
	}
	return strings.Repeat(" ", room) + lead + body
}

// printfCharacter is the code of a numeric operand that starts with a quote, as bash's printf reads 'a; ok is false past ASCII.
func printfCharacter(arg string) (code int64, quoted, ok bool) {
	if arg == "" || (arg[0] != '\'' && arg[0] != '"') {
		return 0, false, false
	}
	if len(arg) == 1 {
		return 0, true, true
	}
	if arg[1] >= 0x80 {
		return 0, true, false
	}
	return int64(arg[1]), true, true
}

// printfMagnitude reads an integer as C's strtoimax and strtoumax do with base 0, leading white space and a sign included; ok is false for
// anything bash would report, trailing text and overflow among it.
func printfMagnitude(arg string) (negative bool, magnitude uint64, ok bool) {
	s := strings.TrimLeft(arg, " \t\n\v\f\r")
	if s == "" {
		return false, 0, false
	}
	if s[0] == '+' || s[0] == '-' {
		negative, s = s[0] == '-', s[1:]
	}
	base := 10
	switch {
	case len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X"):
		base, s = 16, s[2:]
	case len(s) > 1 && s[0] == '0':
		base, s = 8, s[1:]
	}
	if s == "" || strings.ContainsAny(s, "_+-") {
		return false, 0, false
	}
	magnitude, err := strconv.ParseUint(s, base, 64)
	return negative, magnitude, err == nil
}

// printfSigned is a %d operand as bash reads it.
func printfSigned(arg string) (int64, bool) {
	if code, quoted, ok := printfCharacter(arg); quoted {
		return code, ok
	}
	negative, magnitude, ok := printfMagnitude(arg)
	switch {
	case !ok:
		return 0, false
	case negative && magnitude <= 1<<63:
		return -int64(magnitude-1) - 1, true
	case !negative && magnitude < 1<<63:
		return int64(magnitude), true
	}
	return 0, false
}

// printfUnsigned is a %u, %o or %x operand as bash reads it: a negative one wraps, as strtoumax's does.
func printfUnsigned(arg string) (uint64, bool) {
	if code, quoted, ok := printfCharacter(arg); quoted {
		return uint64(code), ok
	}
	negative, magnitude, ok := printfMagnitude(arg)
	if !ok {
		return 0, false
	}
	if negative {
		return -magnitude, true
	}
	return magnitude, true
}

// printfDecimal is a float operand bash's strtold reads whole: decimal digits with a point and an exponent, leading white space allowed.
var printfDecimal = regexp.MustCompile(`^[ \t\n\v\f\r]*([+-]?)((?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]{1,4})?)$`)

// printfSpecial is a float operand strtold reads that nib's printf does not: infinity, not a number, or a hex float.
var printfSpecial = regexp.MustCompile(`(?i)^[ \t\n\v\f\r]*[+-]?(inf|nan|0x)`)

// float formats a float conversion as glibc does from the long double bash reads: the operand rounded to the long double nearest it, then
// its exact decimal value rounded to the precision, half to even.
func (p *printfer) float(arg string, verb byte, s printfSpec) (string, bool) {
	if longDoubleBits == 0 {
		return "", p.leave(fallback.Machine, "a float on a platform not checked against bash")
	}
	if !p.locale.point {
		return "", p.leave(fallback.Machine, "a float outside the C numeric locale")
	}
	value := new(big.Float).SetPrec(longDoubleBits).SetMode(big.ToNearestEven)
	negative := false
	if code, quoted, ok := printfCharacter(arg); quoted {
		if !ok {
			return "", p.leave(fallback.Machine, "a float of a character code past ASCII")
		}
		value.SetInt64(code)
	} else {
		m := printfDecimal.FindStringSubmatch(arg)
		if m == nil {
			if printfSpecial.MatchString(arg) {
				return "", p.leave(fallback.Missing, "a float operand inf, nan or in hex")
			}
			return "", p.leave(fallback.Error, "a float operand bash reports as invalid")
		}
		exact, ok := new(big.Rat).SetString(m[2])
		if !ok {
			return "", p.leave(fallback.Error, "a float operand bash reports as invalid")
		}
		value.SetRat(exact)
		negative = m[1] == "-"
		// Far from 1 a long double's range and subnormals, which big.Float lacks, would matter.
		if exponent := value.MantExp(nil); value.Sign() != 0 && (exponent < -16000 || exponent > 16000) {
			return "", p.leave(fallback.Missing, "a float past the range nib's printf covers")
		}
	}
	precision := 6
	if s.precise {
		precision = s.precision
	}
	var body string
	switch verb {
	case 'f', 'F':
		body = value.Text('f', precision)
		if s.alt && precision == 0 {
			body += "."
		}
	case 'e', 'E':
		body = printfExponent(value, precision, s.alt)
	case 'g', 'G':
		if precision == 0 {
			precision = 1
		}
		exponent := 0
		if value.Sign() != 0 {
			e := value.Text('e', precision-1)
			exponent, _ = strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
			// glibc's %#g drops the zeros of a long double that rounding carried to the next power of ten (999999.5 is 1.e+06).
			unrounded := value.Text('e', 40)
			if before, _ := strconv.Atoi(unrounded[strings.IndexByte(unrounded, 'e')+1:]); s.alt && before != exponent {
				return "", p.leave(fallback.Missing, "glibc's %#g after a carry to the next power of ten")
			}
		}
		if exponent < precision && exponent >= -4 {
			body = value.Text('f', precision-1-exponent)
			if s.alt && precision-1-exponent == 0 {
				body += "."
			}
		} else {
			body = printfExponent(value, precision-1, s.alt)
		}
		if !s.alt {
			body = printfTrimZeros(body)
		}
	}
	if verb == 'E' || verb == 'G' {
		body = strings.ToUpper(body)
	}
	sign := ""
	switch {
	case negative:
		sign = "-"
	case s.plus:
		sign = "+"
	case s.space:
		sign = " "
	}
	return printfPad(body, s.width, s.minus, s.zero, sign), true
}

// printfExponent is value in C's %e form, with at least two exponent digits.
func printfExponent(value *big.Float, precision int, alt bool) string {
	text := value.Text('e', precision)
	if alt && precision == 0 {
		at := strings.IndexByte(text, 'e')
		text = text[:at] + "." + text[at:]
	}
	return text
}

// printfTrimZeros drops the zeros after a point at the end of %g's digits, and the point when nothing follows it.
func printfTrimZeros(body string) string {
	mantissa, exponent := body, ""
	if at := strings.IndexByte(body, 'e'); at >= 0 {
		mantissa, exponent = body[:at], body[at:]
	}
	if strings.IndexByte(mantissa, '.') >= 0 {
		mantissa = strings.TrimRight(mantissa, "0")
		mantissa = strings.TrimSuffix(mantissa, ".")
	}
	return mantissa + exponent
}

// printfEscape decodes the backslash escape esc starts, after its backslash, as bash's printf does in a format, or in a %b operand with
// operand set, where \0 takes three more octal digits; it returns the text and how many bytes of esc it took. An escape bash does not
// know prints its backslash and takes nothing. ok is false for \u past ASCII outside a UTF-8 locale.
func printfEscape(esc string, operand bool, locale printfLocale) (text string, n int, ok bool) {
	if esc == "" {
		return `\`, 0, true
	}
	switch c := esc[0]; c {
	case 'a':
		return "\a", 1, true
	case 'b':
		return "\b", 1, true
	case 'e', 'E':
		return "\x1b", 1, true
	case 'f':
		return "\f", 1, true
	case 'n':
		return "\n", 1, true
	case 'r':
		return "\r", 1, true
	case 't':
		return "\t", 1, true
	case 'v':
		return "\v", 1, true
	case '\\':
		return `\`, 1, true
	case '\'', '"', '?':
		// A %b operand keeps these as written.
		if operand {
			return `\`, 0, true
		}
		return string(c), 1, true
	case '0', '1', '2', '3', '4', '5', '6', '7':
		most := 3
		if operand && c == '0' {
			most = 4
		}
		end := 1
		for end < most && end < len(esc) && esc[end] >= '0' && esc[end] <= '7' {
			end++
		}
		value, _ := strconv.ParseUint(esc[:end], 8, 32)
		return string([]byte{byte(value)}), end, true
	case 'x', 'u', 'U':
		most := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
		end := 1
		for end <= most && end < len(esc) && strings.IndexByte("0123456789abcdefABCDEF", esc[end]) >= 0 {
			end++
		}
		if end == 1 {
			return `\`, 0, true
		}
		value, _ := strconv.ParseUint(esc[1:end], 16, 32)
		if c == 'x' {
			return string([]byte{byte(value)}), end, true
		}
		if value < 0x80 {
			return string([]byte{byte(value)}), end, true
		}
		if !locale.utf8 || !utf8.ValidRune(rune(value)) {
			return "", 0, false
		}
		return string(rune(value)), end, true
	}
	return `\`, 0, true
}

// bashQuote is arg quoted as bash's printf %q quotes it: two single quotes for nothing, $'...' for text with control characters, else a
// backslash before each character the shell would read; ok is false past ASCII, whose quoting follows the locale, and for %#q.
func bashQuote(arg string, alt bool) (string, bool) {
	if alt || !printfASCII(arg) {
		return "", false
	}
	if arg == "" {
		return "''", true
	}
	for i := 0; i < len(arg); i++ {
		if arg[i] < 0x20 || arg[i] == 0x7f {
			return ansiQuote(arg), true
		}
	}
	var out strings.Builder
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch {
		case strings.IndexByte(" \t\n!\"$&'()*,;<>?[\\]^`{|}", c) >= 0:
			out.WriteByte('\\')
		case c == '#' && i == 0:
			out.WriteByte('\\')
		case c == '~' && (i == 0 || arg[i-1] == ':' || arg[i-1] == '='):
			out.WriteByte('\\')
		}
		out.WriteByte(c)
	}
	return out.String(), true
}

// ansiQuote is arg in bash's $'...' quoting.
func ansiQuote(arg string) string {
	var out strings.Builder
	out.WriteString("$'")
	for i := 0; i < len(arg); i++ {
		c := arg[i]
		switch c {
		case '\a':
			out.WriteString(`\a`)
		case '\b':
			out.WriteString(`\b`)
		case 0x1b:
			out.WriteString(`\E`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\v':
			out.WriteString(`\v`)
		case '\\', '\'':
			out.WriteByte('\\')
			out.WriteByte(c)
		default:
			if c < 0x20 || c == 0x7f {
				out.WriteString(`\` + strconv.FormatUint(uint64(c)>>6, 8) + strconv.FormatUint(uint64(c)>>3&7, 8) + strconv.FormatUint(uint64(c)&7, 8))
			} else {
				out.WriteByte(c)
			}
		}
	}
	out.WriteByte('\'')
	return out.String()
}

// printfASCII says whether s is ASCII, where bash's byte counts and the letters a reader sees agree.
func printfASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// printfQuote quotes a word for the shell.
func printfQuote(word string) string {
	quoted, err := syntax.Quote(word, syntax.LangBash)
	if err != nil {
		return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return quoted
}
