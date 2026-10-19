package nibash

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// reachLimit bounds the directories and named paths one call keeps for attribution; past it a change below none of them is not credited.
const reachLimit = 256

// register lists c among the server's writers from now until its run ends, and forgets writers no running call overlaps any more.
func (s *Shell) register(c *call) {
	c.ranFrom = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writers == nil {
		s.writers = map[*call]bool{}
	}
	earliest := c.ranFrom
	for w := range s.writers {
		if w.ranTo.IsZero() && w.ranFrom.Before(earliest) {
			earliest = w.ranFrom
		}
	}
	for w := range s.writers {
		if !w.ranTo.IsZero() && w.ranTo.Before(earliest) {
			delete(s.writers, w)
		}
	}
	s.writers[c] = true
}

// end marks that c's run, a job's included, is over.
func (c *call) end() {
	c.session.shell.mu.Lock()
	defer c.session.shell.mu.Unlock()
	c.ranTo = time.Now()
}

// reachIn notes what a command c ran can reach: the directory it ran in and the paths its arguments name. The bare root is never one, so a
// pattern like tr's / does not make every file the command's.
func (c *call) reachIn(dir string, args []string) {
	add := func(path string) {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		}
		if filepath.Dir(path) == path {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.reach) < reachLimit && !slices.Contains(c.reach, path) {
			c.reach = append(c.reach, path)
		}
	}
	add(dir)
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			_, value, ok := strings.Cut(arg, "=")
			if !ok {
				continue
			}
			arg = value
		}
		if arg != "" {
			add(absolute(dir, arg))
		}
	}
}

// reaches says whether path, canonical, lies in what c's commands reached or is a file one of its redirects wrote.
func (c *call) reaches(path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.reach {
		if r == path || strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	for written := range c.written {
		if real, err := filepath.EvalSymlinks(written); err == nil && real == path {
			return true
		}
	}
	return false
}

// root is the call whose command started c, c itself for a call of its own.
func (c *call) root() *call {
	for c.origin != nil {
		c = c.origin
	}
	return c
}

// own says whether nib can credit the change of path, canonical, to c's command: the command or a job it started reached it, and no edit or
// write of nib's, and no other nibash call that ran meanwhile and reached it, could have made it instead. A writer outside this server, such
// as another server's agent or an editor, is seen only when the path lies beyond what the command reached.
func (c *call) own(path string) bool {
	if c.engine.WroteSince(path, c.ranFrom) {
		return false
	}
	s := c.session.shell
	s.mu.Lock()
	writers := make([]*call, 0, len(s.writers))
	ended := map[*call]time.Time{}
	for w := range s.writers {
		writers = append(writers, w)
		ended[w] = w.ranTo
	}
	s.mu.Unlock()
	mine := false
	for _, w := range writers {
		switch {
		case w.root() == c.root():
			mine = mine || w.reaches(path)
		case (ended[w].IsZero() || ended[w].After(c.ranFrom)) && w.reaches(path):
			return false
		}
	}
	return mine
}
