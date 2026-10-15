package nibash

import (
	"errors"
	"strings"
)

// errUnsupported means an option nibash does not implement; the command runs as the real program instead.
var errUnsupported = errors.New("option not implemented by nibash")

// optionSpec describes a command's options the GNU way: short letters that take no value, short letters that take one, and long names.
type optionSpec struct {
	flags  string
	values string
	// long maps a long option to its short letter, or to "" for one handled by name; a trailing "=" means it takes a value.
	long map[string]string
	// numeric accepts -NUM as a count, as head and tail do.
	numeric bool
}

// option is one parsed option: its short letter or long name, and its value.
type option struct {
	name  string
	value string
}

// parseOptions splits args into options and operands, GNU style: options may follow operands, "--" ends them, and short ones bundle.
func parseOptions(spec optionSpec, args []string) ([]option, []string, error) {
	var options []option
	var operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			operands = append(operands, args[i+1:]...)
			return options, operands, nil
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(arg[2:], "=")
			short, known := spec.long[name]
			takes := false
			if !known {
				short, known = spec.long[name+"="]
				takes = known
			}
			if !known {
				return nil, nil, errUnsupported
			}
			if takes && !hasValue {
				if i+1 >= len(args) {
					return nil, nil, errUnsupported
				}
				i++
				value = args[i]
			} else if !takes && hasValue {
				return nil, nil, errUnsupported
			}
			if short == "" {
				short = name
			}
			options = append(options, option{short, value})
		case len(arg) > 1 && arg[0] == '-':
			if spec.numeric && isDigits(arg[1:]) {
				options = append(options, option{"#", arg[1:]})
				continue
			}
			for j := 1; j < len(arg); j++ {
				letter := string(arg[j])
				switch {
				case strings.Contains(spec.flags, letter):
					options = append(options, option{letter, ""})
				case strings.Contains(spec.values, letter):
					value := arg[j+1:]
					if value == "" {
						if i+1 >= len(args) {
							return nil, nil, errUnsupported
						}
						i++
						value = args[i]
					}
					options = append(options, option{letter, value})
					j = len(arg)
				default:
					return nil, nil, errUnsupported
				}
			}
		default:
			operands = append(operands, arg)
		}
	}
	return options, operands, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// count parses a non-negative count; ok is false for anything else.
func count(value string) (int, bool) {
	if !isDigits(value) || len(value) > 9 {
		return 0, false
	}
	n := 0
	for _, r := range value {
		n = n*10 + int(r-'0')
	}
	return n, true
}
