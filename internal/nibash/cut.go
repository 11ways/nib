package nibash

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/js"
)

// --- cut -c

// cutPlan is cut -c: each line's characters at the positions its list names. A file's line it shortens prints as a row naming the stretches
// it kept, which is no anchor; one it keeps whole stays the file's row.
type cutPlan struct {
	ranges   [][2]int // positions from 1, both ends included; an end of 0 runs to the line's end
	operands []string
	// bytes is set where the system cut counts bytes, as in the C locale, rather than characters.
	bytes bool
}

var cutSpec = optionSpec{values: "c", long: map[string]string{"characters=": "c"}}

func prepareCut(name string, args []string) (plan, error) {
	options, operands, err := parseOptions(cutSpec, args)
	if err != nil {
		return nil, err
	}
	// Without a list, or with two, cut only complains; the system program says how.
	if len(options) != 1 {
		return nil, errUnsupported
	}
	ranges, ok := cutList(options[0].value)
	if !ok {
		return nil, errUnsupported
	}
	return &cutPlan{ranges: ranges, operands: operands}, nil
}

// cutList parses a list like 1-5,8,10- as GNU cut takes one; ok is false for one it refuses, which the system program then reports.
func cutList(list string) ([][2]int, bool) {
	var ranges [][2]int
	for _, part := range strings.Split(list, ",") {
		from, to, isRange := strings.Cut(part, "-")
		if !isRange {
			to = from
		}
		start, end := 1, 0
		var ok bool
		if from != "" {
			if start, ok = count(from); !ok || start == 0 {
				return nil, false
			}
		}
		if to != "" {
			if end, ok = count(to); !ok || end == 0 {
				return nil, false
			}
		}
		if from == "" && to == "" || end != 0 && end < start {
			return nil, false
		}
		ranges = append(ranges, [2]int{start, end})
	}
	return ranges, true
}

// errUncounted means nib could not tell how the system cut counts, so it cannot print what that cut would.
var errUncounted = errors.New("the system cut counts in a way nib's own does not know")

// standIn learns whether the system cut counts bytes or characters, which the locale and the cut on PATH decide.
func (p *cutPlan) standIn(c *call, hc interp.HandlerContext) error {
	counted, err := c.cutCounts(hc)
	if err != nil {
		return err
	}
	p.bytes = counted == countsBytes
	return nil
}

type cutCounting int

const (
	countsBytes cutCounting = iota + 1
	countsCharacters
)

// cutCountings caches what each cut, in each locale, was seen to count.
var cutCountings sync.Map

// cutProbe is a line with a character of two bytes, an invalid byte, a truncated sequence and one of four bytes, and what characters 2-6 of it
// are to a cut counting bytes, and to one counting characters with every invalid byte as one.
const (
	cutProbe           = "\xc3\xa9\xff\xe2\x82a\xf0\x9f\x98\x80b\n"
	cutProbeBytes      = "\xa9\xff\xe2\x82a\n"
	cutProbeCharacters = "\xff\xe2\x82a\xf0\x9f\x98\x80\n"
)

// cutCounts runs the cut the command would run, in its environment, once on cutProbe, and says how it counts.
func (c *call) cutCounts(hc interp.HandlerContext) (cutCounting, error) {
	path, err := interp.LookPathDir(hc.Dir, hc.Env, "cut")
	if err != nil {
		return 0, errUncounted
	}
	env := environ(hc.Env)
	key := path
	for _, pair := range env {
		if strings.HasPrefix(pair, "LC_ALL=") || strings.HasPrefix(pair, "LC_CTYPE=") || strings.HasPrefix(pair, "LANG=") {
			key += "\x00" + pair
		}
	}
	if known, ok := cutCountings.Load(key); ok {
		return known.(cutCounting), nil
	}
	argv := []string{path, "-c2-6"}
	if c.tools.Sandbox {
		if argv, err = c.sandboxed(hc.Dir, argv); err != nil {
			return 0, errUncounted
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = hc.Dir
	cmd.Stdin = strings.NewReader(cutProbe)
	out, err := cmd.Output()
	if err != nil {
		return 0, errUncounted
	}
	var counted cutCounting
	switch {
	case bytes.Equal(out, []byte(cutProbeBytes)):
		counted = countsBytes
	case bytes.Equal(out, []byte(cutProbeCharacters)):
		counted = countsCharacters
	default:
		return 0, errUncounted
	}
	cutCountings.Store(key, counted)
	return counted, nil
}

func (p *cutPlan) run(inv *invocation) int {
	operands := p.operands
	if len(operands) == 0 {
		operands = []string{"-"}
	}
	status := 0
	each := func(r record) bool { return inv.out.emit(p.cut(r)) }
	for _, operand := range operands {
		if inv.out.stopped {
			break
		}
		if operand == "-" {
			inv.stdin(each)
			continue
		}
		source := inv.open(operand, false)
		if source == nil {
			status = 1
			continue
		}
		_ = source.Raw(func(line int, text string) bool {
			r := record{text: text}
			if source.View != nil {
				r.origin = &origin{source: source, line: line}
			} else if !utf8.ValidString(text) {
				r.text, r.raw = strings.ToValidUTF8(text, "�"), text
			}
			return each(r)
		})
	}
	return status
}

// cut is what cut prints for r: r itself when it keeps all of it; the file's line with the stretches it keeps when r is that line, without
// color codes, and the stretches fall between its characters; else plain text.
func (p *cutPlan) cut(r record) record {
	text := r.text
	if r.raw != "" {
		text = r.raw
	}
	o := r.origin
	held := o != nil && !r.display && r.kept == nil && o.source.View != nil && o.line >= 1 && o.line <= len(o.source.View.Lines) &&
		o.source.View.Lines[o.line-1] == r.text && engine.Visible(text) == text
	if held && o.line == 1 && o.source.BOM {
		// The program reads the byte order mark the row leaves out.
		text, held = "\xef\xbb\xbf"+text, false
	}
	pieces, whole := p.pieces(text)
	if whole {
		return r
	}
	var b strings.Builder
	for _, piece := range pieces {
		b.WriteString(text[piece[0]:piece[1]])
	}
	cut := b.String()
	plain := record{text: strings.ToValidUTF8(cut, "�")}
	if plain.text != cut {
		plain.raw = cut
	}
	if !held {
		return plain
	}
	between := func(at int) bool { return at == len(text) || utf8.RuneStart(text[at]) }
	kept := make([]engine.Stretch, 0, len(pieces))
	for _, piece := range pieces {
		if !between(piece[0]) || !between(piece[1]) {
			return plain
		}
		kept = append(kept, engine.Stretch{Start: js.UnitsOf(text, piece[0]), End: js.UnitsOf(text, piece[1])})
	}
	r.text, r.kept = cut, kept
	return r
}

// pieces are the byte ranges of text the list keeps, in order and merged, and whether they are all of it.
func (p *cutPlan) pieces(text string) ([][2]int, bool) {
	bounds := p.units(text)
	n := len(bounds) - 1
	var spans [][2]int
	for _, r := range p.ranges {
		start, end := r[0]-1, r[1]
		if end == 0 || end > n {
			end = n
		}
		if start < end {
			spans = append(spans, [2]int{start, end})
		}
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var merged [][2]int
	for _, s := range spans {
		if k := len(merged); k > 0 && s[0] <= merged[k-1][1] {
			merged[k-1][1] = max(merged[k-1][1], s[1])
		} else {
			merged = append(merged, s)
		}
	}
	whole := n == 0 || len(merged) == 1 && merged[0] == [2]int{0, n}
	pieces := make([][2]int, len(merged))
	for i, m := range merged {
		pieces[i] = [2]int{bounds[m[0]], bounds[m[1]]}
	}
	return pieces, whole
}

// units are the byte offsets where each of text's units starts, and its length: bytes, or characters with every invalid byte as one.
func (p *cutPlan) units(text string) []int {
	bounds := make([]int, 0, len(text)+1)
	for at := 0; at < len(text); {
		bounds = append(bounds, at)
		width := 1
		if !p.bytes && text[at] >= utf8.RuneSelf {
			_, width = utf8.DecodeRuneInString(text[at:])
		}
		at += width
	}
	return append(bounds, len(text))
}
