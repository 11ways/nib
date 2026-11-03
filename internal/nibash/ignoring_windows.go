//go:build windows

package nibash

import "syscall"

// ignoringEnv is unused on Windows, where no signal a trap could ignore reaches a program.
const ignoringEnv = "NIBASH_EXEC_IGNORING"

// ignoringArgv is argv as it is: Windows ends a program for each signal it can stand for, which no program can ignore.
func ignoringArgv(argv []string, _ string, _ []syscall.Signal) ([]string, bool) { return argv, false }
