package nibash

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

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
	// listed is the path the line names, when the program that printed it read one there.
	listed *listedLine
	// raw is the line as read from bytes when it was not valid UTF-8, which text shows with U+FFFD in its place.
	raw string
	// kept are the stretches of origin's line a command like cut kept, nil when text shows it whole.
	kept []engine.Stretch
	// shape is how the terminal prints a line of git's that nib reshapes for the model, whose text stays git's line; nil prints text.
	shape *shaped
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

// output is where a nib command's lines go: the terminal sink, a link to the next nib stage, a pipe whose nib reader takes rows in band
// (rows), or plain bytes.
type output struct {
	ctx    context.Context
	sink   *sink
	link   *link
	rows   *rowBand
	writer io.Writer
	// model is whether what the command writes reaches the model through nothing but nib's own commands passing whole lines on.
	model    bool
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
	case o.rows != nil:
		if _, err := io.WriteString(o.writer, o.rows.put(r)+"\n"); err != nil {
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

// input is where a nib command reads standard input from: a link's records, or bytes split into plain records, among which rows a nib writer
// sent in band (rows) come back as its records.
type input struct {
	link   *link
	reader *bufio.Reader
	file   *os.File
	rows   *rowBand
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
	text = strings.TrimSuffix(text, "\n")
	if in.rows != nil {
		if r, ok := in.rows.take(text); ok {
			if in.all != nil {
				in.all.WriteString(r.text + "\n")
			}
			return r, true
		}
	}
	if in.all != nil {
		in.all.WriteString(text + "\n")
	}
	r := record{text: text}
	if !utf8.ValidString(text) {
		r.text, r.raw = strings.ToValidUTF8(text, "�"), text
	}
	return r, true
}

// rowBand carries records through a real pipe: a nib writer whose reader is a nib command that reads the pipe as bytes, because a compound
// command or other programs write to it too, puts each record here and writes a line naming it, so the rows keep their place among the other
// lines. The token is the call's own, so no program's output can name a row.
type rowBand struct {
	prefix string
	mu     sync.Mutex
	next   int
	held   map[int]record
}

func newRowBand() *rowBand {
	token := make([]byte, 12)
	_, _ = rand.Read(token)
	return &rowBand{prefix: "\x1bnib-row:" + hex.EncodeToString(token) + ":", held: map[int]record{}}
}

// put keeps a record and returns the line that names it.
func (b *rowBand) put(r record) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	b.held[b.next] = r
	return b.prefix + strconv.Itoa(b.next)
}

// take is the record a line names, which it gives once.
func (b *rowBand) take(line string) (record, bool) {
	rest, ok := strings.CutPrefix(line, b.prefix)
	if !ok {
		return record{}, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil {
		return record{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.held[id]
	delete(b.held, id)
	return r, ok
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
