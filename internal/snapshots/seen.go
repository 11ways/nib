package snapshots

import (
	"slices"
	"strings"
)

// SeenFile is what a session holds of one file's latest version: its tag, size and the lines it saw in full.
type SeenFile struct {
	Path  string
	Tag   string
	Lines int
	// Anywhere says edits may anchor on any line: the session has no record of which it saw.
	Anywhere bool
	// Seen are the lines seen in full as 1-based inclusive ranges, ascending.
	Seen [][2]int
	// Known counts the lines whose content the session wrote or saw before they moved, which it does not count as seen.
	Known int
}

// SeenFiles lists what session holds of each of its files, the most recently used first. It only reads: nothing counts as used.
func (s *Store) SeenFiles(session string) []SeenFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := session + "\x00"
	var files []SeenFile
	for element := s.order.Back(); element != nil; element = element.Prev() {
		h := element.Value.(*history)
		if !strings.HasPrefix(h.id, prefix) || len(h.versions) == 0 {
			continue
		}
		v := h.versions[0]
		file := SeenFile{Path: h.id[len(prefix):], Tag: v.Tag, Lines: len(v.Lines), Anywhere: v.Seen == nil}
		for _, line := range sortedKeys(v.Seen) {
			if !v.Seen[line] {
				continue
			}
			if n := len(file.Seen); n > 0 && file.Seen[n-1][1] == line-1 {
				file.Seen[n-1][1] = line
				continue
			}
			file.Seen = append(file.Seen, [2]int{line, line})
		}
		for line := range v.Known {
			if !v.Seen[line] {
				file.Known++
			}
		}
		files = append(files, file)
	}
	return slices.Clip(files)
}
