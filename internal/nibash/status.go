package nibash

import (
	"cmp"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/server"
)

// MaxTail is the most output lines Status gives of one command or job.
const MaxTail = 50

// tailBytes bounds how much of a command's output Status reads to find its last lines.
const tailBytes = 64 * 1024

// endedShown is how long an ended job stays in Status, so a watcher sees how it ended.
const endedShown = 10 * time.Minute

// Status is what runs under the keys owns names an agent for: the calls in flight and the jobs running or ended within endedShown, each with
// the last tail lines of its output, oldest first.
func (s *Shell) Status(owns func(key any) (agent string, ok bool), tail int) server.ShellStatus {
	tail = min(max(tail, 0), MaxTail)
	s.mu.Lock()
	type owned struct {
		agent string
		sess  *session
	}
	var sessions []owned
	for key, sess := range s.sessions {
		if agent, ok := owns(key); ok {
			sessions = append(sessions, owned{agent, sess})
		}
	}
	s.mu.Unlock()
	status := server.ShellStatus{Commands: []server.RunningCommand{}, Jobs: []server.ShellJob{}}
	now := time.Now()
	for _, o := range sessions {
		o.sess.mu.Lock()
		for c, r := range o.sess.active {
			command := server.RunningCommand{Agent: o.agent, Command: r.command, Description: r.description, Started: r.started.UnixMilli()}
			if tail > 0 {
				command.Tail = c.sink.tail(tail)
			}
			status.Commands = append(status.Commands, command)
		}
		o.sess.mu.Unlock()
		for _, j := range o.sess.jobs.all() {
			job := server.ShellJob{ID: j.id, Agent: o.agent, Command: j.command, Started: j.started.UnixMilli(), Running: j.running(), Log: j.log}
			if !job.Running {
				if now.Sub(j.ended) > endedShown {
					continue
				}
				exit := j.exit
				job.Exit, job.Ended = &exit, j.ended.UnixMilli()
			}
			if tail > 0 {
				job.Tail = fileTail(j.log, tail)
			}
			status.Jobs = append(status.Jobs, job)
		}
	}
	status.Models, status.Summaries = s.modelCalls()
	slices.SortStableFunc(status.Commands, func(a, b server.RunningCommand) int { return cmp.Compare(a.Started, b.Started) })
	slices.SortStableFunc(status.Jobs, func(a, b server.ShellJob) int { return cmp.Compare(a.Started, b.Started) })
	return status
}

// tail is the last n lines of what the sink holds, as plain text.
func (s *sink) tail(n int) []string {
	s.mu.Lock()
	var parts []string
	size := 0
	for i := len(s.entries) - 1; i >= 0 && size < tailBytes; i-- {
		text := string(s.entries[i].bytes)
		if r := s.entries[i].record; r != nil {
			text = r.text + "\n"
		}
		parts = append(parts, text)
		size += len(text)
	}
	s.mu.Unlock()
	var text strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		text.WriteString(parts[i])
	}
	return lastLines(text.String(), n)
}

// fileTail is the last n lines of a log, as plain text; none when it cannot be read.
func fileTail(path string, n int) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	from := max(info.Size()-tailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(file, from, info.Size()-from))
	if err != nil {
		return nil
	}
	return lastLines(string(data), n)
}

// lastLines are the last n lines of text, a final line break aside, with control characters and color codes stripped.
func lastLines(text string, n int) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	lines = lines[max(len(lines)-n, 0):]
	for i, line := range lines {
		lines[i] = engine.PlainText(strings.ToValidUTF8(strings.TrimSuffix(line, "\r"), "�"))
	}
	return lines
}

// modelCalls are the decider and summarizer calls running now, oldest first, and whether summaries are on; nil without a summarizer.
func (s *Shell) modelCalls() ([]server.ModelCall, *server.SummaryState) {
	s.modelMu.Lock()
	calls := make([]server.ModelCall, 0, len(s.models))
	for _, c := range s.models {
		calls = append(calls, c)
	}
	s.modelMu.Unlock()
	slices.SortStableFunc(calls, func(a, b server.ModelCall) int { return cmp.Compare(a.Started, b.Started) })
	var state *server.SummaryState
	if s.summarizer != nil {
		state = &server.SummaryState{On: !s.summariesOff.Load()}
	}
	return calls, state
}
