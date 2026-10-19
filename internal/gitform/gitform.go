// Package gitform reads the forms of git's output nib prints shorter for the model, a commit header in git's default format and a --stat
// file row, and gives what each prints as; nibash's git and read's PATH@REV share them.
package gitform

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// StatWidth is the --stat-width and --stat-graph-width that print every path whole and draw each changed line as one bar character, so a
// row's bar holds its exact counts; a file with more changed lines gets a scaled bar, which StatCounts leaves as git printed it.
const StatWidth = "1000000"

// StatArgs are the options that make git's --stat print rows StatCounts reads.
var StatArgs = []string{"--stat-width=" + StatWidth, "--stat-graph-width=" + StatWidth}

// statBar matches a --stat file row with its count and bar: the path, the count and the bar, which a file with no changed lines lacks.
var statBar = regexp.MustCompile(`^ (\S.*?) +\| +(\d+)(?: ([+-]+))?$`)

// StatCounts is a --stat file row as it prints, " PATH +A -D", its bar given as the counts it draws (+A alone for a file only grown, -D
// for one only shrunk, 0 for one with no changed lines); ok is false for a line that is no such row or whose bar git scaled.
func StatCounts(line string) (shown, path string, ok bool) {
	m := statBar.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	added, removed := strings.Count(m[3], "+"), strings.Count(m[3], "-")
	if total, _ := strconv.Atoi(m[2]); total != added+removed {
		return "", "", false
	}
	var counts []string
	if added > 0 {
		counts = append(counts, "+"+strconv.Itoa(added))
	}
	if removed > 0 {
		counts = append(counts, "-"+strconv.Itoa(removed))
	}
	if len(counts) == 0 {
		counts = []string{"0"}
	}
	return " " + m[1] + " " + strings.Join(counts, " "), m[1], true
}

// OwnAuthorLesson explains a commit header's one line without an email, once per agent.
const OwnAuthorLesson = "[a commit header names its author's email only when it is not the repository's own user.email]"

// The lines of a commit's header in git's default format.
var (
	Commit = regexp.MustCompile(`^commit [0-9a-f]{4,64}(?: \(.+\))?$`)
	merge  = regexp.MustCompile(`^Merge:(?: [0-9a-f]{4,64}){2,}$`)
	author = regexp.MustCompile(`^Author: (?:.* )?<([^<>]+)>$`)
)

// datePrefix starts a header's "Date:" line, and dateLayout is git's default date after it.
const (
	datePrefix = "Date:   "
	dateLayout = "Mon Jan 2 15:04:05 2006 -0700"
)

// Zone is the zone a header's one line gives its date in, nil for the machine's; a variable so a test can set it.
var Zone *time.Location

// HeaderTakes says whether a header whose lines so far are held, its Commit line first, takes line next: a merge's parents, the author, the
// date and the blank line after them, in that order; complete is whether line is that blank line, which ends the header.
func HeaderTakes(held []string, line string) (takes, complete bool) {
	last := held[len(held)-1]
	switch {
	case len(held) == 1 && merge.MatchString(line),
		author.MatchString(line) && (len(held) == 1 || merge.MatchString(last)),
		strings.HasPrefix(line, datePrefix) && author.MatchString(last):
		return true, false
	case line == "" && strings.HasPrefix(last, datePrefix):
		return true, true
	}
	return false, false
}

// Header is the one line a complete header's lines print as: git's commit line, the author's email unless it is own (the repository's
// user.email), and the author date in Zone to the minute; ok is false when its date does not read as git's default, so it prints as git
// printed it.
func Header(held []string, own string) (line string, ownAuthor, ok bool) {
	email, date := "", ""
	for _, l := range held[1:] {
		if m := author.FindStringSubmatch(l); m != nil {
			email = m[1]
		}
		if value, found := strings.CutPrefix(l, datePrefix); found {
			t, err := time.Parse(dateLayout, value)
			if err != nil || t.Format(dateLayout) != value {
				return "", false, false
			}
			zone := Zone
			if zone == nil {
				zone = time.Local
			}
			date = t.In(zone).Format("Mon 2006-01-02 15:04")
		}
	}
	line = held[0]
	if ownAuthor = own != "" && strings.EqualFold(email, own); !ownAuthor {
		line += " " + email
	}
	return line + " " + date, ownAuthor, true
}

// Message is a message line under a header's one line, without git's indent.
func Message(line string) string { return strings.TrimPrefix(line, "    ") }

// IsMerge is whether a header's line is a merge's "Merge:" line, which prints under the one line as git printed it.
func IsMerge(line string) bool { return strings.HasPrefix(line, "Merge:") }
