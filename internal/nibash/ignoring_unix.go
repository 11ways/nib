//go:build unix

package nibash

import (
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ignoringCommand and ignoringEnv make a run of nib's own binary start a program with the signals it names ignored (execIgnoring), as a
// program bash starts inherits the signals an empty trap makes bash ignore; Go cannot set a child's dispositions, but they survive an exec.
const (
	ignoringCommand = "__nibash-exec-ignoring"
	ignoringEnv     = "NIBASH_EXEC_IGNORING"
)

// Any binary that links nibash, nib's and the tests', runs a program for nibash this way when nibash starts it so.
func init() {
	if len(os.Args) > 1 && os.Args[1] == ignoringCommand && os.Getenv(ignoringEnv) == "1" {
		os.Exit(execIgnoring(os.Args[2:]))
	}
}

// ignoringArgv is argv, a program's path and its arguments, run through nib's own binary so it starts with ignored ignored and sees argv0
// as its name; argv itself when nothing is ignored or nib's binary cannot be found.
func ignoringArgv(argv []string, argv0 string, ignored []syscall.Signal) ([]string, bool) {
	if len(ignored) == 0 {
		return argv, false
	}
	self, err := os.Executable()
	if err != nil {
		return argv, false
	}
	numbers := make([]string, len(ignored))
	for i, sig := range ignored {
		numbers[i] = strconv.Itoa(int(sig))
	}
	return append([]string{self, ignoringCommand, strings.Join(numbers, ","), argv0}, argv...), true
}

// execIgnoring is the run ignoringArgv starts: args are the signal numbers, the program's name, its path and its arguments. It ignores the
// signals and becomes the program, keeping its pid, its process group and its descriptors.
func execIgnoring(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "nibash: a program run with ignored signals lacks its path")
		return 126
	}
	var signals []os.Signal
	for _, field := range strings.Split(args[0], ",") {
		if n, err := strconv.Atoi(field); err == nil && n > 0 {
			signals = append(signals, syscall.Signal(n))
		}
	}
	signal.Ignore(signals...)
	env := slices.DeleteFunc(os.Environ(), func(pair string) bool { return strings.HasPrefix(pair, ignoringEnv+"=") })
	path := args[2]
	err := syscall.Exec(path, append([]string{args[1]}, args[3:]...), env)
	fmt.Fprintf(os.Stderr, "%s: %v\n", args[1], err)
	return 126
}
