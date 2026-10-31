package decider

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

// The calls a decider or summarizer makes of a model, as an Observer hears of them and the ledger records them.
const (
	CallDecider = "decider"
	CallRules   = "rules"
	CallSummary = "summary"
)

// LedgerFile is the ledger's name in nib's state folder.
const LedgerFile = "models.jsonl"

// Event is one decision, rule verdict or summary as the ledger keeps it: what was asked, who answered, what it cost and what came of it.
// It holds no program output, only its size and what the model made of it.
type Event struct {
	Time time.Time `json:"time"`
	Call string    `json:"call"`
	// Who is the model or summary entry that answered; empty when none did.
	Who string `json:"who,omitempty"`
	// Asked are the models or entries asked, in order, the failed ones included.
	Asked []string `json:"asked"`
	// Failure is why no model answered, or, beside Who, why the ones asked before it gave no answer.
	Failure     string  `json:"failure,omitempty"`
	Requests    int     `json:"requests"`
	Cached      int     `json:"cached,omitempty"`
	InputTokens int     `json:"input_tokens,omitempty"`
	Cost        float64 `json:"cost,omitempty"`
	Millis      int64   `json:"ms"`
	Command     string  `json:"command"`
	Purpose     string  `json:"purpose,omitempty"`
	Expect      string  `json:"expect,omitempty"`
	Exit        int     `json:"exit,omitempty"`
	TimedOut    bool    `json:"timed_out,omitempty"`
	// Rows, Windows and Picked are a decision's candidate rows, the windows judged and those that reached PickThreshold; Whole is how sure
	// the model was that the purpose needs every line, when it was asked.
	Rows    int      `json:"rows,omitempty"`
	Windows int      `json:"windows,omitempty"`
	Picked  int      `json:"picked,omitempty"`
	Whole   *float64 `json:"whole,omitempty"`
	// Directory, Rules, Scores and Broken are a rule verdict's: where the command runs, the vague rules asked of it, the score of each and
	// the indexes of those that reached RuleThreshold, which refused the command.
	Directory string    `json:"directory,omitempty"`
	Rules     []string  `json:"rules,omitempty"`
	Scores    []float64 `json:"scores,omitempty"`
	Broken    []int     `json:"broken,omitempty"`
	// LogLines, ReadLines, Ranges, Dropped and Chars are a summary's: the log's lines, those the summarizer read, the ranges it pointed at
	// that held, those that did not, and the length of its text.
	LogLines  int `json:"log_lines,omitempty"`
	ReadLines int `json:"read_lines,omitempty"`
	Ranges    int `json:"ranges,omitempty"`
	Dropped   int `json:"dropped,omitempty"`
	Chars     int `json:"chars,omitempty"`
}

// Ledger is a file of events, one JSON line each, appended to by every nib process; the zero value records nothing.
type Ledger struct{ path string }

// DefaultLedger is the ledger NIB_DECIDER_LEDGER names: unset, the file in nib's state folder; "off", none.
func DefaultLedger(getenv func(string) string) string {
	switch name := getenv("NIB_DECIDER_LEDGER"); name {
	case "off":
		return ""
	case "":
		if state := platform.StateHome(getenv); state != "" {
			return filepath.Join(state, "nib", LedgerFile)
		}
		return ""
	default:
		return name
	}
}

// record appends e, stamped with the time now; a ledger that cannot be written is skipped, since it must never fail a call.
func (l Ledger) record(e Event, started time.Time) {
	if l.path == "" {
		return
	}
	e.Time = time.Now().UTC()
	e.Millis = time.Since(started).Milliseconds()
	data, err := json.Marshal(e)
	if err != nil || os.MkdirAll(filepath.Dir(l.path), 0o700) != nil {
		return
	}
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	// One write per line keeps lines whole when several nib processes append at once.
	file.Write(append(data, '\n'))
	file.Close()
}

// usage puts u's counts into e.
func (e *Event) usage(u Usage) {
	e.Requests, e.Cached, e.InputTokens, e.Cost = u.Requests, u.Cached, u.InputTokens, u.Cost
}

// ReadLedger is every event in the ledger at path, oldest first; a missing file has none, and a line that is not an event is skipped.
func ReadLedger(path string) ([]Event, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var events []Event
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for lines.Scan() {
		var e Event
		if json.Unmarshal(lines.Bytes(), &e) == nil && e.Call != "" {
			events = append(events, e)
		}
	}
	return events, lines.Err()
}
