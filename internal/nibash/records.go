package nibash

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/11ways/nib/internal/engine"
)

// record is one line as a stage passes it on: text is what the stage would print (with grep's own prefixes), origin the file line it shows,
// when it shows one whole and unchanged.
type record struct {
	text   string
	origin *origin
	// match marks a grep match, as opposed to a context line; context records render with a space where matches get a *.
	match   bool
	context bool
	// added marks a line a diff adds, which renders with a +.
	added bool
	// separator is grep's "--" between groups of context.
	separator bool
	// display is a line of nib's own or a program's that belongs under origin's file header but is not that file's line, as text shows it.
	display bool
}

type origin struct {
	source *engine.Source
	line   int
}

// linkCapacity bounds a link, so a writer waits for its reader instead of filling memory.
const linkCapacity = 1024

// link joins two nib stages through one pipe: records go over a channel, and the pipe itself carries nothing.
type link struct {
	records chan record
	done    chan struct{}
	once    sync.Once
	// Written by the writer before it closes records; read by the renderer after the call.
	stats linkStats
}

// linkStats is what a writer saw, also after its reader stopped, for facts like "20 of 312 matches".
type linkStats struct {
	kind    string // "grep" or "lines"
	total   int
	files   int
	partial bool
	single  *engine.Source
}

// finish is called by the reader when it stops, so the writer stops sending.
func (l *link) finish() { l.once.Do(func() { close(l.done) }) }

// links is a call's registry of links, keyed by the pipe both ends share.
type links struct {
	mu    sync.Mutex
	byKey map[[2]uint64]*link
}

func pipeKey(file *os.File) ([2]uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return [2]uint64{}, false
	}
	return [2]uint64{st.Dev, st.Ino}, true
}

func (l *links) get(file *os.File) *link {
	key, ok := pipeKey(file)
	if !ok {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey == nil {
		l.byKey = map[[2]uint64]*link{}
	}
	found := l.byKey[key]
	if found == nil {
		found = &link{records: make(chan record, linkCapacity), done: make(chan struct{})}
		l.byKey[key] = found
	}
	return found
}

// output is where a nib command's lines go: the terminal sink, a link to the next nib stage, or plain bytes.
type output struct {
	ctx      context.Context
	sink     *sink
	link     *link
	writer   io.Writer
	stopped  bool
	sent     int
	reporter func(record)
}

// emit sends one record; false means nothing reads any more, so the command may stop (or keep counting for a fact).
func (o *output) emit(r record) bool {
	if o.stopped {
		return false
	}
	switch {
	case o.sink != nil:
		o.sink.add(r)
	case o.link != nil:
		select {
		case o.link.records <- r:
		case <-o.link.done:
			o.stopped = true
			return false
		case <-o.ctx.Done():
			o.stopped = true
			return false
		}
	default:
		if _, err := io.WriteString(o.writer, r.text+"\n"); err != nil {
			o.stopped = true
			return false
		}
	}
	o.sent++
	return true
}

// close ends the output; a link's reader sees the end of its records.
func (o *output) close() {
	if o.link != nil {
		close(o.link.records)
	}
}

// input is where a nib command reads standard input from: a link's records, or bytes split into plain records.
type input struct {
	link   *link
	reader *bufio.Reader
	file   *os.File
	// all keeps every line read from bytes, for tail's log of program output it dropped.
	all *strings.Builder
}

// next is the next record; ok is false at the end.
func (in *input) next() (record, bool) {
	if in.link != nil {
		r, ok := <-in.link.records
		return r, ok
	}
	if in.reader == nil {
		return record{}, false
	}
	text, err := in.reader.ReadString('\n')
	if text == "" && err != nil {
		return record{}, false
	}
	if in.all != nil {
		in.all.WriteString(text)
	}
	text = strings.TrimSuffix(text, "\n")
	return record{text: strings.ToValidUTF8(text, "\ufffd")}, true
}

// stop says the command reads no more: a link's writer stops sending, and a pipe's writer gets SIGPIPE as under bash once the shell closes it.
func (in *input) stop() {
	if in.link != nil {
		in.link.finish()
		// Drain what is already queued so a writer blocked mid-send sees done promptly.
		go func() {
			for range in.link.records {
			}
		}()
	}
}
