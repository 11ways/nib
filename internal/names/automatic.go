package names

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	// autoUses is the fewest shown paths that earn a directory a name, unless its prefix is autoAtOnce long; past it, a name is made once
	// the characters it would have saved so far pay for the note announcing it.
	autoUses = 3
	// autoChars is the shortest prefix, as nib would print it, worth a name.
	autoChars = 30
	// autoAtOnce is a prefix too long to print even twice: such a directory is named the first time it is shown.
	autoAtOnce = 100
	// autoLimit bounds the names nib makes for one agent.
	autoLimit = 8
	// learnedLimit bounds the names kept for the next session of one server directory.
	learnedLimit = 16
	// fullAgain is how many calls showing paths pass before a note is told in full again, as the first one may have left the agent's context.
	fullAgain = 25
	// nameMax caps a name nib makes, its number included: a name is typed and printed often, and two initials plus a folder's word of up
	// to stemMax letters say enough.
	nameMax = 12
	// stemMax caps the part of a name taken from the named folder itself.
	stemMax = 10
)

// Automatic turns on naming the directories an agent's shown paths keep running through, and keeps every name agents get, theirs and
// nib's, in file for the next server of base. Call it before serving.
func (d *Defined) Automatic(file, base string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.automatic, d.file, d.base = true, file, base
}

// Announced are the notes on names nib made for agent since the last call, once each, and makes those names take effect, then the notes
// again on names its paths ran through that it got before its context was dropped; call it once at the end of each call. The first note is
// told in full, as is the first after fullAgain more calls that showed paths, or after a drop, the rest briefly.
func (d *Defined) Announced(agent string) []string {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		return nil
	}
	if names.observed {
		names.calls++
		names.observed = false
	}
	inFull := func() bool {
		if names.full == 0 || names.calls-names.full >= fullAgain {
			names.full = names.calls
			return true
		}
		return false
	}
	var notes []string
	for _, m := range names.made {
		names.list = append(names.list, definition{m.Entry, d.Now().UnixNano(), "automatic"})
		if inFull() {
			notes = append(notes, note(m.Name, m.prefix, m.count))
		} else {
			notes = append(notes, brief(m.Name, m.prefix, m.count))
		}
	}
	for _, m := range names.retell {
		if inFull() {
			notes = append(notes, fmt.Sprintf("[~%s = %s, a name you got before your context was compacted or cleared; ~%s/ works in every tool and in nibash, for you and the subagents you start]", m.Name, m.prefix, m.Name))
		} else {
			notes = append(notes, fmt.Sprintf("[~%s = %s]", m.Name, m.prefix))
		}
	}
	names.made, names.retell = nil, nil
	return notes
}

// Forget marks every name agent got before moment as no longer told, as its harness compacted or cleared its context then: a path printed
// under one tells of it again, and the next note is told in full. Call it once per moment, as Store.Forget reports.
func (d *Defined) Forget(agent string, moment time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		return
	}
	names.full = 0
	// The latest definition of a name is the one the agent works with; one redefined since the moment is still told.
	latest := map[string]int64{}
	for _, entry := range names.list {
		latest[entry.Name] = entry.at
	}
	at := moment.UnixNano()
	for name, defined := range latest {
		if defined < at {
			if names.untold == nil {
				names.untold = map[string]bool{}
			}
			names.untold[name] = true
		}
	}
}

// Told marks the names of lines, as Table.Lines gives them, as told to agent again: a greeting listed them.
func (d *Defined) Told(agent string, lines []string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		return
	}
	for _, line := range lines {
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "~"), " = ")
		delete(names.untold, name)
	}
}

// mention notes that a response printed shown, a path under a name of table, so a name agent got before its context was dropped is told again.
func (d *Defined) mention(agent string, table *Table, shown string) {
	if d == nil {
		return
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(shown, "~"), "/")
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil || !names.untold[name] {
		return
	}
	home, ok := table.Home(name)
	if !ok {
		return
	}
	delete(names.untold, name)
	names.observed = true
	rest := &Table{Base: table.Base, Entries: slices.DeleteFunc(slices.Clone(table.Entries), func(e Entry) bool { return e.Name == name })}
	names.retell = append(names.retell, made{Entry{name, home}, printed(rest, home), 0})
}

// printed is directory as a path shows it with table: under a name, relative to the table's base inside it, else absolute.
func printed(table *Table, directory string) string {
	if s, ok := table.Display(directory); ok {
		return s
	}
	return plain(table.Base, directory)
}

// useful reports whether entry prints its own directory shorter than it prints plain from base; a name that saves nothing there, such as
// one made while an agent worked in a worktree for a folder of the server's directory, is never kept for a session in base.
func useful(base string, entry Entry) bool {
	_, ok := (&Table{Base: base, Entries: []Entry{entry}}).Display(entry.Directory)
	return ok
}

// observe counts the directories from directory up, below the deepest named one holding it, and names the one that saves the most among
// those with a printed prefix of autoChars or more that autoAtOnce or the characters saved so far make worth a note. A directory whose name
// holds a control character is never named.
func (d *Defined) observe(agent string, table *Table, directory string) {
	if d == nil || !d.automatic || strings.ContainsFunc(directory, unicode.IsControl) {
		return
	}
	shown := func(directory string) string { return printed(table, directory) }
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		names = &defined{}
		d.agents[agent] = names
	}
	names.observed = true
	if names.auto >= autoLimit {
		return
	}
	if names.counts == nil {
		names.counts = map[string]int{}
	}
	// Names made in this call but not yet told count as taken and as named, so a path below one adds nothing until it takes effect.
	if len(names.made) > 0 {
		table = &Table{Base: table.Base, Entries: slices.Clone(table.Entries)}
		for _, m := range names.made {
			table.Entries = append(table.Entries, m.Entry)
		}
	}
	best, saving := "", 0
	for ; directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
		// A named directory, or the server's own for a path inside it, prints no further up: nothing above it is ever shown.
		if directory == table.Base || slices.ContainsFunc(table.Entries, func(e Entry) bool { return e.Directory == directory }) {
			break
		}
		prefix := shown(directory)
		if len(prefix) < autoChars {
			continue
		}
		names.counts[directory]++
		count := names.counts[directory]
		saved := count * (len(prefix) - len(stem(filepath.Base(directory))) - 3)
		worth := len(prefix) >= autoAtOnce || (count >= autoUses && saved >= len(note("x", prefix, count)))
		if worth && saved > saving {
			best, saving = directory, saved
		}
	}
	if best == "" {
		return
	}
	name := autoName(best, table)
	if name == "" {
		return
	}
	entry := Entry{name, best}
	names.made = append(names.made, made{entry, shown(best), names.counts[best]})
	names.auto++
	d.learnLocked(entry)
}

// note announces in full a name nib made for the directory printed as prefix, which count shown paths ran through.
func note(name, prefix string, count int) string {
	return fmt.Sprintf("[nib named ~%s = %s, %s; ~%s/ works in every tool and in nibash, for you and the subagents you start]", name, prefix, why(count, "which %d of the paths shown to you ran through"), name)
}

// brief is note for an agent already told in full what such a name is.
func brief(name, prefix string, count int) string {
	return fmt.Sprintf("[~%s = %s, %s]", name, prefix, why(count, "%d paths shown"))
}

// why is the reason a name was made: count shown paths in the words of format, or a prefix named at first sight.
func why(count int, format string) string {
	if count == 1 {
		return "too long to print again"
	}
	return fmt.Sprintf(format, count)
}

var unslugged = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a folder name as a name's lowercase letters, digits and dashes.
func slug(folder string) string {
	return strings.Trim(unslugged.ReplaceAllString(strings.ToLower(folder), "-"), "-")
}

// camel marks where a lowercase letter or digit meets an uppercase one, so camelCase folders split into their words.
var camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// stem is the part of a name a folder gives: its slug when that is at most stemMax long, else its last word that is not a hash or a
// version, as nib for -home-skerit-projects-nib; a word is cut to stemMax.
func stem(folder string) string {
	if whole := slug(folder); len(whole) <= stemMax {
		return whole
	}
	words := strings.Split(slug(camel.ReplaceAllString(folder, "$1-$2")), "-")
	word := words[len(words)-1]
	for i := len(words) - 1; i >= 0; i-- {
		if !meaningless.MatchString(words[i]) {
			word = words[i]
			break
		}
	}
	return word[:min(len(word), stemMax)]
}

// meaningless matches a folder whose name says nothing of what it holds: a hash, or a version.
var meaningless = regexp.MustCompile(`(?i)^(?:[0-9a-f]{8,}|v?[0-9]+(?:\.[0-9]+)*)$`)

// autoName is a free name for directory: the first letters of the two folders above it that say something (a hash or a version does not),
// outer first, then its own folder's stem, as ~bptypes for bundled-skills/2.1.302/<hash>/plugin-authoring/types and ~cpnib for
// claude/projects/-home-skerit-projects-nib; with a number after where that is taken, never past nameMax; "" when none can be made.
func autoName(directory string, table *Table) string {
	taken := func(name string) bool {
		return !valid.MatchString(name) || isUser(name) || slices.ContainsFunc(table.Entries, func(e Entry) bool { return e.Name == name })
	}
	last := stem(filepath.Base(directory))
	if last == "" {
		return ""
	}
	initials := ""
	for above := filepath.Dir(directory); above != filepath.Dir(above) && len(initials) < 2; above = filepath.Dir(above) {
		folder := slug(filepath.Base(above))
		if folder != "" && !meaningless.MatchString(filepath.Base(above)) {
			initials = folder[:1] + initials
		}
	}
	base := initials + last
	if !taken(base) {
		return base
	}
	for n := 2; n < 100; n++ {
		number := strconv.Itoa(n)
		if candidate := strings.TrimRight(base[:min(len(base), nameMax-len(number))], "-") + number; !taken(candidate) {
			return candidate
		}
	}
	return ""
}

// LearnedFile is where the names agents got while serving base are kept for the next session: nib's folder in the user's state home, one
// file per server directory; "" without a home to put it in.
func LearnedFile(base string) string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		state = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(base))
	return filepath.Join(state, "nib", "names", hex.EncodeToString(sum[:8])+".json")
}

// learnedNames is a learned file: the server directory it is for, and its names, oldest first.
type learnedNames struct {
	Base  string  `json:"base"`
	Names []Entry `json:"names"`
}

// AddLearned adds the names file keeps for this table's directory after the table's own: each with a free name and an existing directory
// no name holds yet.
func (t *Table) AddLearned(file string) {
	for _, entry := range readLearned(file, t.Base) {
		if !valid.MatchString(entry.Name) || isUser(entry.Name) || !useful(t.Base, entry) || slices.ContainsFunc(t.Entries, func(e Entry) bool { return e.Name == entry.Name || e.Directory == entry.Directory }) {
			continue
		}
		if canonical, err := filepath.EvalSymlinks(entry.Directory); err != nil || canonical != entry.Directory {
			continue
		}
		t.Entries = append(t.Entries, entry)
	}
}

// readLearned are the names file keeps for base; none when it is missing, unreadable or for another directory.
func readLearned(file, base string) []Entry {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var saved learnedNames
	if json.Unmarshal(data, &saved) != nil || saved.Base != base {
		return nil
	}
	return saved.Names
}

// learnLocked adds entry to the learned file, replacing a name of the same name or directory, dropping names that no longer print shorter
// from the file's directory, and keeping the newest learnedLimit.
func (d *Defined) learnLocked(entry Entry) {
	if d.file == "" || !useful(d.base, entry) {
		return
	}
	kept := slices.DeleteFunc(readLearned(d.file, d.base), func(e Entry) bool {
		return e.Name == entry.Name || e.Directory == entry.Directory || !valid.MatchString(e.Name) || !useful(d.base, e)
	})
	kept = append(kept, entry)
	if len(kept) > learnedLimit {
		kept = kept[len(kept)-learnedLimit:]
	}
	data, err := json.MarshalIndent(learnedNames{d.base, kept}, "", "  ")
	if err != nil || os.MkdirAll(filepath.Dir(d.file), 0o700) != nil {
		return
	}
	staged := d.file + ".tmp"
	if os.WriteFile(staged, append(data, '\n'), 0o600) == nil {
		_ = os.Rename(staged, d.file)
	}
}
