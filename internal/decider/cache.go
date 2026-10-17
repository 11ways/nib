package decider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// memoryEntries bounds the in-memory cache.
const memoryEntries = 256

// entry is one request's answers; it holds no output text, only probabilities by question id.
type entry struct {
	Version     int                `json:"version"`
	Answers     map[string]float64 `json:"answers"`
	Model       string             `json:"model"`
	InputTokens int                `json:"input_tokens"`
	Cost        float64            `json:"cost"`
}

// cache keeps answers by request, in memory and, with a directory, on disk, so a repeated output costs nothing and gets the same rows.
type cache struct {
	mu     sync.Mutex
	memory map[string]entry
	order  []string
	dir    string
	// texts are summarizers' replies by request, which are checked against the log again whenever they are used.
	texts map[string]string
}

func newCache(dir string) *cache {
	return &cache{memory: map[string]entry{}, dir: dir, texts: map[string]string{}}
}

// cacheKey covers everything an answer depends on: the endpoint and the whole request body, model, state and question wording included.
func cacheKey(c Config, body []byte) string {
	sum := sha256.New()
	sum.Write([]byte(strconv.Itoa(questionsVersion) + "\x00" + c.URL + "\x00"))
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil))
}

// get is a cached answer for every id; entries on disk are checked like live answers, since a directory under the roots is writable by
// sandboxed programs.
func (c *cache) get(key string, ids []string) (map[string]float64, bool) {
	c.mu.Lock()
	found, ok := c.memory[key]
	c.mu.Unlock()
	if !ok && c.dir != "" {
		data, err := os.ReadFile(filepath.Join(c.dir, key+".json"))
		if err != nil || json.Unmarshal(data, &found) != nil {
			return nil, false
		}
		ok = true
	}
	if !ok || found.Version != questionsVersion || !valid(found.Answers, ids) {
		return nil, false
	}
	c.remember(key, found)
	return found.Answers, true
}

func (c *cache) put(key string, e entry) {
	c.remember(key, e)
	if data, err := json.Marshal(e); err == nil {
		c.store(key, data)
	}
}

// getText is a cached summarizer reply; the caller checks it like a live one.
func (c *cache) getText(key string) (string, bool) {
	c.mu.Lock()
	text, ok := c.texts[key]
	c.mu.Unlock()
	if ok || c.dir == "" {
		return text, ok
	}
	data, err := os.ReadFile(filepath.Join(c.dir, key+".json"))
	if err != nil || json.Unmarshal(data, &text) != nil {
		return "", false
	}
	c.rememberText(key, text)
	return text, true
}

func (c *cache) putText(key, text string) {
	c.rememberText(key, text)
	if data, err := json.Marshal(text); err == nil {
		c.store(key, data)
	}
}

func (c *cache) rememberText(key, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.texts) >= memoryEntries {
		clear(c.texts)
	}
	c.texts[key] = text
}

// store writes data as the entry for key, through a temporary file renamed into place; without a directory it does nothing.
func (c *cache) store(key string, data []byte) {
	if c.dir == "" || os.MkdirAll(c.dir, 0o700) != nil {
		return
	}
	temporary, err := os.CreateTemp(c.dir, ".entry-*")
	if err != nil {
		return
	}
	_, err = temporary.Write(data)
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil || os.Rename(temporary.Name(), filepath.Join(c.dir, key+".json")) != nil {
		os.Remove(temporary.Name())
	}
}

func (c *cache) remember(key string, e entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.memory[key]; !ok {
		c.order = append(c.order, key)
	}
	c.memory[key] = e
	for len(c.order) > memoryEntries {
		delete(c.memory, c.order[0])
		c.order = c.order[1:]
	}
}
