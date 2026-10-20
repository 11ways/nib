package names

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Mentioned are the names text uses as ~name, each once in the order first used: a ~ at the start or after a character no path or word goes
// on through, a valid name, and then anything a name cannot hold.
func Mentioned(text string) []string {
	var found []string
	for i := 0; i < len(text); i++ {
		if text[i] != '~' || i > 0 && continues(text[i-1]) {
			continue
		}
		end := i + 1
		for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' || text[end] >= '0' && text[end] <= '9' || text[end] == '-') {
			end++
		}
		if end < len(text) && (text[end] >= 'A' && text[end] <= 'Z' || text[end] == '_') {
			continue
		}
		if name := text[i+1 : end]; valid.MatchString(name) && !slices.Contains(found, name) {
			found = append(found, name)
		}
		i = end - 1
	}
	return found
}

// continues says whether c, before a ~, makes it part of a word or path rather than the start of a name.
func continues(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_~/.-", c) >= 0
}

// Prompted is what the agent must be told of mentioned, the names a message the user sent it uses, as lines ~name = directory: each name it
// sees but got before its context was last dropped, and each it does not see that another agent of this server or an earlier session of
// its directory had, which it sees from now on. A name the server instructions list, or one the agent holds, is told only when an earlier
// session had it for another directory, which the line then names; a name nothing knows is left out.
func (v *View) Prompted(mentioned []string) []string {
	if v.defined == nil || len(mentioned) == 0 {
		return nil
	}
	var static []Entry
	base := ""
	if v.static != nil {
		static, base = v.static.Entries, v.static.Base
	}
	var lines []string
	for _, entry := range v.defined.prompted(v.agent, static, mentioned) {
		line := fmt.Sprintf("~%s = %s", entry.Name, filepath.ToSlash(plain(base, entry.Directory)))
		if entry.earlier != "" {
			line += fmt.Sprintf(" (in an earlier session ~%s was %s)", entry.Name, filepath.ToSlash(entry.earlier))
		}
		lines = append(lines, line)
	}
	return lines
}

// toldEntry is a name to tell, with the directory an earlier session had for it when that differs.
type toldEntry struct {
	Entry
	earlier string
}

// prompted is Prompted's names for agent, static the server's table, which the agent was told in the instructions.
func (d *Defined) prompted(agent string, static []Entry, mentioned []string) []toldEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		names = &defined{}
		d.agents[agent] = names
	}
	now := d.Now()
	earlier := map[string]string{}
	if d.file != "" {
		for _, e := range current(d.file, d.base, now) {
			earlier[e.Name] = e.Directory
		}
	}
	var told []toldEntry
	for _, name := range mentioned {
		directory, held := "", false
		for _, e := range static {
			if e.Name == name {
				directory, held = e.Directory, true
			}
		}
		for _, e := range names.list {
			if e.Name == name {
				directory, held = e.Directory, !names.untold[name]
			}
		}
		if directory != "" {
			delete(names.untold, name)
			if was := earlier[name]; !held || was != "" && was != directory {
				told = append(told, toldEntry{Entry{name, directory}, otherThan(was, directory)})
			}
			continue
		}
		directory = d.elsewhereLocked(agent, name, earlier[name])
		if info, err := os.Stat(directory); directory == "" || err != nil || !info.IsDir() {
			continue
		}
		entry := Entry{name, directory}
		names.list = append(names.list, definition{entry, now.UnixNano(), "message"})
		d.learnLocked(entry)
		told = append(told, toldEntry{Entry: entry})
	}
	return told
}

// otherThan is was when it differs from directory, else "".
func otherThan(was, directory string) string {
	if was == directory {
		return ""
	}
	return was
}

// elsewhereLocked is the directory name stands for outside agent's view: the one directory other agents of this server have for it, else
// earlier, an earlier session's; "" when agents disagree or none has it.
func (d *Defined) elsewhereLocked(agent, name, earlier string) string {
	found := ""
	for other, names := range d.agents {
		if other == agent {
			continue
		}
		for _, e := range names.list {
			if e.Name != name {
				continue
			}
			if found != "" && found != e.Directory {
				return ""
			}
			found = e.Directory
		}
	}
	if found != "" {
		return found
	}
	return earlier
}
