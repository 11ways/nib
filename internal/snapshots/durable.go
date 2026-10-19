package snapshots

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/11ways/nib/internal/platform"
)

const (
	// keptFor is how long a session's saved lines outlive its last change; a session resumed later reads its files again.
	keptFor = 7 * 24 * time.Hour
	// flushAfter batches the writes of a busy session into one.
	flushAfter = 2 * time.Second
)

// durable keeps what sessions with a lasting key saw across server restarts: per key, a private file with each path's newest version as a
// hash of its text, never the text, and the lines seen and known in it with the moments they joined. A restored line counts only while the
// file still hashes the same, so a file changed meanwhile needs reading again.
type durable struct {
	dir   string
	keys  map[string]string
	files map[string]*savedFile
	dirty map[string]bool
	timer *time.Timer
	// writing keeps two flushes from writing the same file at once.
	writing sync.Mutex
}

type savedFile struct {
	Paths map[string]*savedPath `json:"paths"`
}

type savedPath struct {
	Hash  string        `json:"hash"`
	At    int64         `json:"at"`
	Seen  map[int]int64 `json:"seen,omitempty"`
	Known map[int]int64 `json:"known,omitempty"`
}

// Keep makes the store save the lines sessions given a lasting key saw in dir, and drops files there older than keptFor; a dir that cannot be
// made private keeps nothing.
func (s *Store) Keep(dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil || !private(dir) {
		return
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > keptFor {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.durable = &durable{dir: dir, keys: map[string]string{}, files: map[string]*savedFile{}, dirty: map[string]bool{}}
}

// Persist gives session a lasting key, a harness's own id for the agent, under which what it sees is saved and from which a new history of a
// file it saw before a restart starts; a store that keeps nothing ignores it.
func (s *Store) Persist(session, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.durable
	if d == nil || d.keys[session] == key {
		return
	}
	d.keys[session] = key
	if _, ok := d.files[key]; !ok {
		d.files[key] = loadSaved(d.file(key))
	}
}

// Flush writes what changed since the last write at once, as a server that ends does.
func (s *Store) Flush() {
	s.mu.Lock()
	d := s.durable
	if d == nil {
		s.mu.Unlock()
		return
	}
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	writes := s.collect()
	s.mu.Unlock()
	d.write(writes)
}

func (d *durable) file(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(d.dir, hex.EncodeToString(sum[:12])+".json")
}

// restore seeds a version new to session's history of path with the lines saved for it, when its text hashes as saved, leaving out what a
// drop of the session's context since then took. The caller holds the store's lock.
func (s *Store) restore(session, path string, version *Version) {
	d := s.durable
	if d == nil {
		return
	}
	key, ok := d.keys[session]
	if !ok {
		return
	}
	saved := d.files[key].Paths[path]
	if saved == nil || saved.Hash != hashOf(version.Key) {
		return
	}
	cleared := s.cleared[session]
	for line, at := range saved.Seen {
		if at >= cleared && line >= 1 && line <= len(version.Lines) {
			version.Seen, version.seenAt = stampAt(version.Seen, version.seenAt, line, at)
		}
	}
	for line, at := range saved.Known {
		if at >= cleared && line >= 1 && line <= len(version.Lines) {
			version.Known, version.knownAt = stampAt(version.Known, version.knownAt, line, at)
		}
	}
}

// changed marks session's saved lines to be written soon. The caller holds the store's lock.
func (s *Store) changed(session string) {
	d := s.durable
	if d == nil {
		return
	}
	key, ok := d.keys[session]
	if !ok {
		return
	}
	d.dirty[key] = true
	if d.timer == nil {
		d.timer = time.AfterFunc(flushAfter, func() {
			s.mu.Lock()
			d.timer = nil
			writes := s.collect()
			s.mu.Unlock()
			d.write(writes)
		})
	}
}

// collect is each dirty key's file as of now: its saved paths, updated from the newest version of every file a session under the key holds,
// with what a drop of their contexts took left out, the paths kept within maxPaths, the most recent. The caller holds the store's lock.
func (s *Store) collect() map[string][]byte {
	d := s.durable
	writes := map[string][]byte{}
	now := time.Now().UnixNano()
	for key := range d.dirty {
		file := d.files[key]
		for session, sessionKey := range d.keys {
			if sessionKey != key {
				continue
			}
			prefix := session + "\x00"
			for element := s.order.Front(); element != nil; element = element.Next() {
				h := element.Value.(*history)
				if !strings.HasPrefix(h.id, prefix) || len(h.versions) == 0 || h.versions[0].Seen == nil {
					continue
				}
				v := h.versions[0]
				file.Paths[h.id[len(prefix):]] = &savedPath{Hash: hashOf(v.Key), At: now, Seen: momentsOf(v.Seen, v.seenAt), Known: momentsOf(v.Known, v.knownAt)}
			}
			cleared := s.cleared[session]
			for _, saved := range file.Paths {
				for _, set := range []map[int]int64{saved.Seen, saved.Known} {
					for line, at := range set {
						if at < cleared {
							delete(set, line)
						}
					}
				}
			}
		}
		for path, saved := range file.Paths {
			if now-saved.At > int64(keptFor) {
				delete(file.Paths, path)
			}
		}
		if len(file.Paths) > maxPaths {
			paths := make([]string, 0, len(file.Paths))
			for path := range file.Paths {
				paths = append(paths, path)
			}
			slices.SortFunc(paths, func(a, b string) int { return cmp.Compare(file.Paths[b].At, file.Paths[a].At) })
			for _, path := range paths[maxPaths:] {
				delete(file.Paths, path)
			}
		}
		if data, err := json.Marshal(file); err == nil {
			writes[d.file(key)] = data
		}
		delete(d.dirty, key)
	}
	return writes
}

// write publishes each file whole: a private temporary file renamed over the old one.
func (d *durable) write(writes map[string][]byte) {
	d.writing.Lock()
	defer d.writing.Unlock()
	for name, data := range writes {
		temp, err := os.CreateTemp(d.dir, ".seen-*")
		if err != nil {
			continue
		}
		_, err = temp.Write(data)
		if closed := temp.Close(); err == nil {
			err = closed
		}
		if err == nil {
			err = os.Rename(temp.Name(), name)
		}
		if err != nil {
			_ = os.Remove(temp.Name())
		}
	}
}

// loadSaved reads a saved file, an empty one when there is none or it cannot be read; a link is never followed.
func loadSaved(name string) *savedFile {
	empty := &savedFile{Paths: map[string]*savedPath{}}
	if info, err := os.Lstat(name); err != nil || !info.Mode().IsRegular() {
		return empty
	}
	file, err := os.OpenFile(name, os.O_RDONLY|platform.NoFollow, 0)
	if err != nil {
		return empty
	}
	defer file.Close()
	var saved savedFile
	if json.NewDecoder(file).Decode(&saved) != nil || saved.Paths == nil {
		return empty
	}
	for path, entry := range saved.Paths {
		if entry == nil {
			delete(saved.Paths, path)
		}
	}
	return &saved
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

func momentsOf(set map[int]bool, at map[int]int64) map[int]int64 {
	if len(set) == 0 {
		return nil
	}
	out := make(map[int]int64, len(set))
	for line := range set {
		out[line] = at[line]
	}
	return out
}

// stampAt adds line to set at moment, keeping the earlier moment of a line already there.
func stampAt(set map[int]bool, at map[int]int64, line int, moment int64) (map[int]bool, map[int]int64) {
	if set == nil {
		set, at = map[int]bool{}, map[int]int64{}
	}
	if !set[line] || moment < at[line] {
		set[line], at[line] = true, moment
	}
	return set, at
}

// private is true for a real folder of this user's own that no one else may enter.
func private(dir string) bool { return platform.PrivateDirectory(dir) }
