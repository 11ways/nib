package nibash

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/js"
)

// --- head -c and tail -c

// span is the bytes [from, to) of an input of size bytes that the plan prints.
func (p *edgePlan) span(size int64) (from, to int64) {
	n := int64(p.count)
	switch {
	case !p.tail:
		return 0, min(n, size)
	case p.from:
		return min(max(n, 1)-1, size), size
	default:
		return max(size-n, 0), size
	}
}

// bytesOfFile prints the bytes of a file head -c or tail -c prints, as whole lines: a line it prints whole is the file's row where nib holds
// the file, and the one it cuts at an edge prints as a row naming the characters it shows, never an anchor; where a cut splits a character,
// or the file is not held, the line is plain text.
func (p *edgePlan) bytesOfFile(inv *invocation, operand string, source *engine.Source) int {
	file, err := source.Open()
	if err != nil {
		inv.fail("%s: %v", operand, err)
		return 1
	}
	defer file.Close()
	var reader io.Reader = file
	var size int64
	held := source.View != nil
	if held {
		data, err := io.ReadAll(file)
		if err != nil {
			inv.fail("%s: %v", operand, err)
			return 1
		}
		// The rows are the held version's only while the bytes are that version.
		body, bom := bytes.CutPrefix(data, []byte("\xef\xbb\xbf"))
		held = bom == source.BOM && strings.ReplaceAll(string(body), "\r\n", "\n") == source.View.Key
		reader, size = bytes.NewReader(data), int64(len(data))
	} else {
		info, err := file.Stat()
		if err != nil {
			inv.fail("%s: %v", operand, err)
			return 1
		}
		size = info.Size()
	}
	from, to := p.span(size)
	buffered := bufio.NewReaderSize(reader, 1<<16)
	first, last, total := 0, 0, 0
	var at int64
	for {
		line, err := buffered.ReadBytes('\n')
		if len(line) == 0 {
			break
		}
		total++
		start := at
		at += int64(len(line))
		if at > from && start < to {
			if first == 0 {
				first = total
			}
			last = total
			lo, hi := max(from, start)-start, min(to, at)-start
			var r record
			if held {
				r = heldPiece(source, total, line, int(lo), int(hi))
			} else {
				r = plainPiece(line[lo:hi])
			}
			if !inv.out.emit(r) {
				break
			}
		}
		// A head need not read what it does not print, except to count a held file's lines, which nib knows.
		if err != nil || !p.tail && at >= to && !held {
			break
		}
	}
	if held {
		total = len(source.View.Lines)
	}
	if first > 0 && (p.tail || held) {
		inv.spanFact(source.Display, first, last, total)
	}
	return 0
}

// heldPiece is what prints of line n of a held file, whose raw bytes are line, when the bytes lo to hi of it print.
func heldPiece(source *engine.Source, n int, line []byte, lo, hi int) record {
	content := source.View.Lines[n-1]
	start := 0
	if n == 1 && source.BOM {
		start = 3
	}
	end := start + len(content)
	whole := record{text: content, origin: &origin{source: source, line: n}}
	switch {
	case start > 0 && (lo > 0 && lo < start || hi < start):
		// Part of the byte order mark, which no row shows.
		return plainPiece(line[lo:hi])
	case lo <= start && hi >= end:
		return whole
	}
	clamp := func(at int) int { return min(max(at, start), end) - start }
	from, to := clamp(lo), clamp(hi)
	between := func(at int) bool { return at == len(content) || utf8.RuneStart(content[at]) }
	if !between(from) || !between(to) || engine.Visible(content) != content {
		return plainPiece(line[lo:hi])
	}
	kept := []engine.Stretch{}
	if to > from {
		kept = append(kept, engine.Stretch{Start: js.UnitsOf(content, from), End: js.UnitsOf(content, to)})
	}
	whole.text, whole.kept = content[from:to], kept
	return whole
}

// plainPiece is printed bytes as a plain line, without its newline.
func plainPiece(piece []byte) record {
	text := strings.TrimSuffix(string(piece), "\n")
	r := record{text: text}
	if !utf8.ValidString(text) {
		r.text, r.raw = strings.ToValidUTF8(text, "\xef\xbf\xbd"), text
	}
	return r
}

// bytesOfStdin prints the bytes of standard input head -c or tail -c prints, each line plain; a tail of a real program's output at the
// model's end keeps all of it in a log, as tail -n does.
func (p *edgePlan) bytesOfStdin(inv *invocation) int {
	if inv.in.link != nil || inv.in.rows != nil {
		// Classification never joins a byte count to nib's rows; only an option an expansion gave can.
		inv.fail("-c counts the bytes a program writes, which rows of nib's own commands are not; write the option literally")
		return 2
	}
	if inv.in.reader == nil {
		return 0
	}
	var data []byte
	var from int64
	if !p.tail {
		data, _ = io.ReadAll(io.LimitReader(inv.in.reader, int64(p.count)))
	} else {
		data, _ = io.ReadAll(inv.in.reader)
		from, _ = p.span(int64(len(data)))
	}
	all, shown := data, data[from:]
	for len(shown) > 0 {
		at := bytes.IndexByte(shown, '\n') + 1
		if at == 0 {
			at = len(shown)
		}
		if !inv.out.emit(plainPiece(shown[:at])) {
			break
		}
		shown = shown[at:]
	}
	if p.tail && inv.out.sink != nil && from > 0 {
		lines := len(plainLines(all))
		whole := len(plainLines(all[from:]))
		if all[from-1] != '\n' {
			whole--
		}
		if lines > whole {
			raw := string(all)
			inv.out.sink.addTail(&tailed{raw: raw, log: inv.call.logs.save(raw), total: lines, shown: whole})
		}
	}
	return 0
}
