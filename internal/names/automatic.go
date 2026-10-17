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
)

// Automatic turns on naming the directories an agent's shown paths keep running through, and keeps every name agents get, theirs and
// nib's, in file for the next server of base. Call it before serving.
func (d *Defined) Automatic(file, base string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.automatic, d.file, d.base = true, file, base
}

// Announced are the notes on names nib made for agent since the last call, once each.
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
	notes := names.pending
	names.pending = nil
	return notes
}

// observe counts the directories a path shown to the agent runs through, below the deepest named one holding it, and names the one that
// saves the most among those with a printed prefix of autoChars or more that autoAtOnce or the characters saved so far make worth a note.
func (d *Defined) observe(agent string, table *Table, canonical string) {
	if d == nil || !d.automatic {
		return
	}
	shown := func(directory string) string {
		if s, ok := table.Display(directory); ok {
			return s
		}
		if relative, err := filepath.Rel(table.Base, directory); err == nil && relative != "." && !strings.HasPrefix(relative, "..") {
			return relative
		}
		return directory
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		names = &defined{}
		d.agents[agent] = names
	}
	if names.auto >= autoLimit {
		return
	}
	if names.counts == nil {
		names.counts = map[string]int{}
	}
	best, saving := "", 0
	for directory := filepath.Dir(canonical); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
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
		saved := count * (len(prefix) - len(slug(filepath.Base(directory))) - 3)
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
	names.list = append(names.list, definition{entry, d.Now().UnixNano()})
	names.auto++
	names.pending = append(names.pending, note(name, shown(best), names.counts[best]))
	d.learnLocked(entry)
}

// note announces a name nib made for the directory printed as prefix, which count shown paths ran through.
func note(name, prefix string, count int) string {
	why := fmt.Sprintf("which %d of the paths shown to you ran through", count)
	if count == 1 {
		why = "too long to print again"
	}
	return fmt.Sprintf("[nib named ~%s = %s, %s; ~%s/ works in every tool and in nibash, for you and the subagents you start]", name, prefix, why, name)
}

var unslugged = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a folder name as a name's lowercase letters, digits and dashes.
func slug(folder string) string {
	return strings.Trim(unslugged.ReplaceAllString(strings.ToLower(folder), "-"), "-")
}

// meaningless matches a folder whose name says nothing of what it holds: a hash, or a version.
var meaningless = regexp.MustCompile(`(?i)^(?:[0-9a-f]{8,}|v?[0-9]+(?:\.[0-9]+)*)$`)

// autoName is a free name for directory: the first letters of the two folders above it that say something (a hash or a version does not),
// outer first, then its own folder, as ~bptypes for bundled-skills/2.1.302/<hash>/plugin-authoring/types; with a number after where that
// is taken; "" when none can be made.
func autoName(directory string, table *Table) string {
	taken := func(name string) bool {
		return !valid.MatchString(name) || isUser(name) || slices.ContainsFunc(table.Entries, func(e Entry) bool { return e.Name == name })
	}
	last := slug(filepath.Base(directory))
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
		if candidate := base + strconv.Itoa(n); !taken(candidate) {
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
		if !valid.MatchString(entry.Name) || isUser(entry.Name) || slices.ContainsFunc(t.Entries, func(e Entry) bool { return e.Name == entry.Name || e.Directory == entry.Directory }) {
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

// learnLocked adds entry to the learned file, replacing a name of the same name or directory and keeping the newest learnedLimit.
func (d *Defined) learnLocked(entry Entry) {
	if d.file == "" {
		return
	}
	kept := slices.DeleteFunc(readLearned(d.file, d.base), func(e Entry) bool { return e.Name == entry.Name || e.Directory == entry.Directory })
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
