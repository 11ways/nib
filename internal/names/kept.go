package names

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/11ways/nib/internal/platform"
)

// keptFor is how long an agent's saved names outlive their last change; a session resumed later gets them again as any agent would.
const keptFor = 7 * 24 * time.Hour

// kept saves the names of agents with a lasting key across server restarts, so a restarted server's paths print under the names the agent
// already holds, and never tells it of them again: per key, a private file with each name the agent got, when and how, and when it was
// last told of it.
type kept struct {
	dir  string
	keys map[string]string
}

// keptFile is one agent's saved names and the latest moment its context was dropped, which the harness reports only once.
type keptFile struct {
	Names   []keptName `json:"names"`
	Dropped int64      `json:"dropped,omitempty"`
}

type keptName struct {
	Name      string `json:"name"`
	Directory string `json:"directory"`
	At        int64  `json:"at"`
	Told      int64  `json:"told"`
	Kind      string `json:"kind"`
}

// Keep makes d save the names agents given a lasting key get, in dir, and drops files there older than keptFor; a dir that cannot be made
// private keeps nothing.
func (d *Defined) Keep(dir string) {
	if err := os.MkdirAll(dir, 0o700); err != nil || !platform.PrivateDirectory(dir) {
		return
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > keptFor {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.kept = &kept{dir: dir, keys: map[string]string{}}
}

// Persist gives agent a lasting key, a harness's own id for it, under which its names are saved from now on; an agent with no names yet in
// this server gets those saved under key, as told when it was last told of each, and those a saved drop took since as untold. Call it before
// the agent inherits its parent's names.
func (d *Defined) Persist(agent, key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := d.kept
	if k == nil || k.keys[agent] == key {
		return
	}
	k.keys[agent] = key
	if names := d.agents[agent]; names != nil && len(names.list) > 0 {
		return
	}
	restored := &defined{}
	file := loadKept(k.file(key))
	for _, saved := range file.Names {
		if !valid.MatchString(saved.Name) || !filepath.IsAbs(saved.Directory) {
			continue
		}
		restored.list = append(restored.list, definition{Entry{saved.Name, saved.Directory}, saved.At, saved.Kind, max(saved.Told, saved.At)})
		switch saved.Kind {
		case "defined":
			restored.own++
		case "automatic":
			restored.auto++
		}
	}
	if len(restored.list) > 0 {
		if file.Dropped > 0 {
			restored.forget(file.Dropped)
		}
		d.agents[agent] = restored
	}
}

// saveLocked writes agent's names when it has a lasting key. The caller holds d.mu.
func (d *Defined) saveLocked(agent string) {
	k := d.kept
	if k == nil {
		return
	}
	key, ok := k.keys[agent]
	names := d.agents[agent]
	if !ok || names == nil {
		return
	}
	file := keptFile{Dropped: names.dropped}
	for _, entry := range names.list {
		file.Names = append(file.Names, keptName{entry.Name, entry.Directory, entry.at, entry.told, entry.kind})
	}
	data, err := json.Marshal(file)
	if err != nil {
		return
	}
	temp, err := os.CreateTemp(k.dir, ".names-*")
	if err != nil {
		return
	}
	_, err = temp.Write(data)
	if closed := temp.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(temp.Name(), k.file(key))
	}
	if err != nil {
		_ = os.Remove(temp.Name())
	}
}

func (k *kept) file(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(k.dir, hex.EncodeToString(sum[:12])+".json")
}

// loadKept reads a saved file, an empty one when there is none or it cannot be read; a link is never followed.
func loadKept(name string) keptFile {
	var saved keptFile
	if info, err := os.Lstat(name); err != nil || !info.Mode().IsRegular() {
		return saved
	}
	file, err := os.OpenFile(name, os.O_RDONLY|platform.NoFollow, 0)
	if err != nil {
		return saved
	}
	defer file.Close()
	if json.NewDecoder(file).Decode(&saved) != nil {
		return keptFile{}
	}
	return saved
}
