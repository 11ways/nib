package fallback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordAppendsAndReadReadsTheRolledOverLogToo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", File)
	Record(path, []Event{{Time: time.Unix(1, 0).UTC(), Command: "grep", Kind: Missing, Reason: "option -P", Form: "grep -P ... ...", Count: 2}})
	Record(path, []Event{{Time: time.Unix(2, 0).UTC(), Command: "printf", Kind: Error, Reason: "x", Form: "printf %d", Count: 1}})
	events, err := Read(path)
	if err != nil || len(events) != 2 || events[0].Command != "grep" || events[0].Count != 2 || events[1].Command != "printf" {
		t.Fatalf("got %+v, %v", events, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the log must be the user's own: %v %v", info, err)
	}
	// Past the limit the log becomes the .1 file and a new one starts; reading gives both, oldest first, and skips what is not an event.
	if err := os.WriteFile(path, []byte(strings.Repeat("not an event\n", Limit/13+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	Record(path, []Event{{Time: time.Unix(3, 0).UTC(), Command: "sed", Kind: Missing, Reason: "y", Count: 1}})
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rolled-over log: %v", err)
	}
	events, err = Read(path)
	if err != nil || len(events) != 1 || events[0].Command != "sed" {
		t.Fatalf("after the roll-over: %+v, %v", events, err)
	}
}

func TestDefaultIsTheStateFolderUnlessTurnedOffOrNamed(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	if got := Default(env(map[string]string{"XDG_STATE_HOME": "/s"})); got != filepath.Join("/s", "nib", File) {
		t.Errorf("default: %q", got)
	}
	if got := Default(env(map[string]string{"XDG_STATE_HOME": "/s", "NIB_FALLBACK_LOG": "off"})); got != "" {
		t.Errorf("off: %q", got)
	}
	if got := Default(env(map[string]string{"NIB_FALLBACK_LOG": "/x/log.jsonl"})); got != "/x/log.jsonl" {
		t.Errorf("named: %q", got)
	}
}
