package engine

import "time"

// writesKept bounds the files whose last write by nib's own tools the engine remembers; past it, writes older than writesFor go.
const (
	writesKept = 4096
	writesFor  = time.Hour
)

// wrote notes that an edit or write of any session is writing canonical now.
func (e *Engine) wrote(canonical string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if e.writes == nil {
		e.writes = map[string]time.Time{}
	}
	if len(e.writes) >= writesKept {
		for path, at := range e.writes {
			if now.Sub(at) > writesFor {
				delete(e.writes, path)
			}
		}
	}
	e.writes[canonical] = now
}

// WroteSince reports whether nib's edit or write tools, in any session of this server, wrote canonical at or after since.
func (e *Engine) WroteSince(canonical string, since time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	at, ok := e.writes[canonical]
	return ok && !at.Before(since)
}
