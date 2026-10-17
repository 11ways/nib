package nibash

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	// logShrink is the dedup ratio, lines to deduplicated entries, past which a file reads as a log by its content alone: no tracked file of
	// nib's own crosses 1.5, while its recorded test, build and service logs reach 2 to 14.
	logShrink = 2.0
	// logMinLines is how long a file must be before its content alone can make it a log.
	logMinLines = 100
)

// logName matches a log's file name: .log or .out, with a rotation number after it or not.
var logName = regexp.MustCompile(`\.(?:log|out)(?:\.\d+)?$`)

// logDirectories are directory names whose untracked files are logs.
var logDirectories = []string{"log", "logs", "test-logs"}

// looksLikeLog says whether nib's own commands show a file as program output rather than as anchored rows: a file named as a log, or kept in
// a log directory, that git does not track, or one whose lines repeat so much (dedupe) that it can only be a log.
func looksLikeLog(path string, lines []string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Dir(path)), "/")
	if (logName.MatchString(filepath.Base(path)) || slices.ContainsFunc(parts, func(p string) bool { return slices.Contains(logDirectories, p) })) &&
		!tracked(path) {
		return true
	}
	if len(lines) < logMinLines {
		return false
	}
	entries, _ := dedupe(lines)
	return float64(len(lines)) >= logShrink*float64(len(entries))
}

// tracked says whether git tracks the file; without git, or outside a repository, nothing is.
func tracked(path string) bool {
	cmd := exec.Command("git", "-c", "core.fsmonitor=false", "-C", filepath.Dir(path), "ls-files", "--error-unmatch", "--", filepath.Base(path))
	return cmd.Run() == nil
}
