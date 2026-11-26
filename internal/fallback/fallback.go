// Package fallback keeps the log of what nibash's own commands hand to a real program, by the form and the reason, never the data, so the
// forms worth implementing next can be counted.
package fallback

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/11ways/nib/internal/platform"
)

// File is the log's name in nib's state folder; past Limit bytes it becomes File+".1", replacing the one before.
const (
	File  = "fallbacks.jsonl"
	Limit = 4 << 20
)

// The kinds of fallback: a form nib's own lacks, one bash would report an error for, and one whose output depends on the locale or the
// machine in a way nib does not follow.
const (
	Missing = "missing"
	Error   = "error"
	Machine = "machine"
)

// Event is one form a call handed to a real program, Count times.
type Event struct {
	Time    time.Time `json:"time"`
	Command string    `json:"command"`
	Kind    string    `json:"kind"`
	Reason  string    `json:"reason"`
	// Form is the call's shape: its options, and its operands with their text left out.
	Form      string `json:"form"`
	Count     int    `json:"count"`
	Directory string `json:"directory,omitempty"`
}

// Default is the log NIB_FALLBACK_LOG names: unset, the file in nib's state folder; "off", none.
func Default(getenv func(string) string) string {
	switch name := getenv("NIB_FALLBACK_LOG"); name {
	case "off":
		return ""
	case "":
		if state := platform.StateHome(getenv); state != "" {
			return filepath.Join(state, "nib", File)
		}
		return ""
	default:
		return name
	}
}

// Record appends events to the log at path, rolling it over past Limit; a log that cannot be written is skipped, as it must never fail a
// call.
func Record(path string, events []Event) {
	if path == "" || len(events) == 0 || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() > Limit {
		_ = os.Rename(path, path+".1")
	}
	var data []byte
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		data = append(append(data, line...), '\n')
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	// One write keeps lines whole when several nib processes append at once.
	file.Write(data)
	file.Close()
}

// Read is every event in the log at path and the one it rolled over from, oldest first; missing files have none, and a line that is not an
// event is skipped.
func Read(path string) ([]Event, error) {
	var events []Event
	for _, name := range []string{path + ".1", path} {
		file, err := os.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		lines := bufio.NewScanner(file)
		lines.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for lines.Scan() {
			var e Event
			if json.Unmarshal(lines.Bytes(), &e) == nil && e.Command != "" {
				events = append(events, e)
			}
		}
		err = lines.Err()
		file.Close()
		if err != nil {
			return nil, err
		}
	}
	return events, nil
}
