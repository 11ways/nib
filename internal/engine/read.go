package engine

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/snapshots"
)

type lineRange struct {
	start int
	end   int // 0 means open
}

// selection is a line selector: ranges, or tail > 0 for the last tail lines, or symbols, names whose declarations give the ranges; none of
// them means the whole file.
type selection struct {
	ranges  []lineRange
	tail    int
	symbols []string
}

var selectorPattern = regexp.MustCompile(`^(.*?):((?:[1-9]\d*(?:-(?:[1-9]\d*)?|\+[1-9]\d*)?|-[1-9]\d*)(?:,[1-9]\d*(?:-(?:[1-9]\d*)?|\+[1-9]\d*)?)*)$`)
var rangePart = regexp.MustCompile(`^(\d+)(?:([-+])(\d*))?$`)

var symbolList = regexp.MustCompile(`^(.*):(` + symbolPart + `(?:\.` + symbolPart + `)*(?:,` + symbolPart + `(?:\.` + symbolPart + `)*)*)$`)

// splitSelector splits path:selector; an existing literal path wins over selector parsing.
func splitSelector(entry string, ctx *Context) (string, selection, error) {
	match := selectorPattern.FindStringSubmatch(entry)
	if match == nil {
		if symbols := symbolList.FindStringSubmatch(entry); symbols != nil && symbols[1] != "" && !exists(ctx.resolve(entry)) {
			return symbols[1], selection{symbols: strings.Split(symbols[2], ",")}, nil
		}
		return entry, selection{}, nil
	}
	if exists(ctx.resolve(entry)) {
		return entry, selection{}, nil
	}
	spec := match[2]
	if strings.HasPrefix(spec, "-") {
		n, _ := strconv.Atoi(spec[1:])
		return match[1], selection{tail: n}, nil
	}
	var parsed []lineRange
	for _, part := range strings.Split(spec, ",") {
		m := rangePart.FindStringSubmatch(part)
		start, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "+":
			count, _ := strconv.Atoi(m[3])
			parsed = append(parsed, lineRange{start, start + count - 1})
		case "-":
			end := 0
			if m[3] != "" {
				end, _ = strconv.Atoi(m[3])
			}
			parsed = append(parsed, lineRange{start, end})
		default:
			// A bare number is that one line: models write path:N the way grep prints a hit, so reading onward needs N-.
			parsed = append(parsed, lineRange{start, start})
		}
	}
	for _, r := range parsed {
		if r.end != 0 && r.end < r.start {
			return "", selection{}, fmt.Errorf("Range %d-%d in %s is reversed", r.start, r.end, entry)
		}
	}
	return match[1], selection{ranges: parsed}, nil
}

var sincePattern = regexp.MustCompile(`^(.+)#([0-9A-Fa-f]{4})$`)

// splitSince splits path#TAG, a read of what changed since that tag; an existing literal path wins.
func splitSince(entry string, ctx *Context) (string, string) {
	match := sincePattern.FindStringSubmatch(entry)
	if match == nil || exists(ctx.resolve(entry)) {
		return entry, ""
	}
	return match[1], strings.ToUpper(match[2])
}

type keptLine struct {
	bytes []byte
	cut   bool
}

type scanned struct {
	total int
	kept  map[int]keptLine
}

// scanLines streams the first size bytes of a file line by line, keeping up to ReadLines of the lines wanted accepts, or with ring > 0 only the
// last ring lines, each up to limit bytes; total counts every line.
func scanLines(ctx *Context, file *os.File, size int64, wanted func(int) bool, limit int, ring int) (scanned, error) {
	kept := map[int]keptLine{}
	chunk := make([]byte, max(1, min(size, 1<<20)))
	line := 1
	var parts [][]byte
	held, length := 0, 0
	keep := wanted(line)
	take := func(piece []byte) {
		length += len(piece)
		if !keep || held >= limit {
			return
		}
		part := bytes.Clone(piece[:min(len(piece), limit-held)])
		parts = append(parts, part)
		held += len(part)
	}
	closeLine := func() {
		if keep {
			kept[line] = keptLine{bytes.Join(parts, nil), length > held}
			if ring > 0 {
				delete(kept, line-ring)
			}
		}
		line++
		parts = nil
		held, length = 0, 0
		keep = ring > 0 || (len(kept) < ReadLines && wanted(line))
	}
	for position := int64(0); position < size; {
		if err := ctx.checkAbort(); err != nil {
			return scanned{}, err
		}
		n, err := file.ReadAt(chunk[:min(int64(len(chunk)), size-position)], position)
		if n == 0 {
			if err != nil && !errors.Is(err, os.ErrClosed) {
				break
			}
			break
		}
		position += int64(n)
		data := chunk[:n]
		start := 0
		for {
			at := bytes.IndexByte(data[start:], '\n')
			if at < 0 {
				break
			}
			take(data[start : start+at])
			closeLine()
			start += at + 1
		}
		take(data[start:])
	}
	if length > 0 {
		closeLine()
	}
	return scanned{line - 1, kept}, nil
}

type lineFactsResult struct {
	lines, longest, longestAt, crlf, lf int
	final, bom, escapes, binary         bool
}

// lineFactsCounter counts line facts over bytes fed in order; widths are UTF-16 units like a line's length, without its CR or a leading BOM.
type lineFactsCounter struct {
	facts    lineFactsResult
	width    int
	previous int
	fed      int
}

func newLineFacts() *lineFactsCounter { return &lineFactsCounter{previous: -1} }

func (c *lineFactsCounter) end() {
	c.facts.lines++
	if c.width > c.facts.longest {
		c.facts.longest = c.width
		c.facts.longestAt = c.facts.lines
	}
	c.width = 0
}

func (c *lineFactsCounter) feed(data []byte) {
	if c.fed == 0 && len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf {
		c.facts.bom = true
		c.width = -1
	}
	c.fed += len(data)
	for _, b := range data {
		if b == 10 {
			if c.previous == 13 {
				c.facts.crlf++
				c.width--
			} else {
				c.facts.lf++
			}
			c.end()
		} else {
			// Lead bytes count once, and four-byte sequences twice, as surrogate pairs.
			if b&0xc0 != 0x80 {
				if b >= 0xf0 {
					c.width += 2
				} else {
					c.width++
				}
			}
			if b == 27 {
				c.facts.escapes = true
			} else if b < 9 || (b > 13 && b < 32) || b == 127 {
				c.facts.binary = true
			}
		}
		c.previous = int(b)
	}
}

func (c *lineFactsCounter) done() lineFactsResult {
	if c.fed > 0 && c.previous != 10 {
		c.end()
	}
	c.facts.final = c.previous == 10
	return c.facts
}

// measure is the line facts of the first size bytes, streamed so a file of any size works.
func measure(ctx *Context, file *os.File, size int64) (lineFactsResult, error) {
	counter := newLineFacts()
	chunk := make([]byte, max(1, min(size, 1<<20)))
	for position := int64(0); position < size; {
		if err := ctx.checkAbort(); err != nil {
			return lineFactsResult{}, err
		}
		n, _ := file.ReadAt(chunk[:min(int64(len(chunk)), size-position)], position)
		if n == 0 {
			break
		}
		counter.feed(chunk[:n])
		position += int64(n)
	}
	return counter.done(), nil
}

// PlainLine is one line of output for looking only, with the line number it came from.
type PlainLine struct {
	At   int
	Text string
}

var digits = regexp.MustCompile(`\d+`)

// EchoLesson explains the markers of a recurring block, once per agent.
const EchoLesson = "[a row starting [#N, K lines] begins a K-line block that recurs below; each [#N again, K lines] row stands for that block printed again there]"

// EchoNote is EchoLesson the first time an agent sees those markers, else nothing.
func (e *Engine) EchoNote(ctx *Context) []string {
	if e.Store.Teach(ctx.Session, "echo") {
		return []string{EchoLesson}
	}
	return nil
}

// RepeatKind says what a Repeat stands for.
type RepeatKind int

const (
	// RepeatLine is a line shown as it is.
	RepeatLine RepeatKind = iota
	// RepeatRun is Period lines repeated back to back Copies times; with Exact false the copies differ in their digits.
	RepeatRun
	// RepeatEcho is Length lines that recur from an earlier place, Of, where they were shown.
	RepeatEcho
)

// Repeat is one step of the repetition Repeats finds; indexes are into its lines.
type Repeat struct {
	Kind           RepeatKind
	Index          int
	Period, Copies int
	Exact          bool
	Of, Length     int
}

// Pricing is what a caller prints, in characters: Line for one line on its own, and Collapsed for a repeat as the caller collapses it, the
// copies it still shows included. Repeats collapses a repeat only where that is shorter than its lines one by one.
type Pricing struct {
	Line      func(index int) int
	Collapsed func(r Repeat) int
}

// pays says whether r collapsed costs less than the count lines from its start printed one by one.
func (p Pricing) pays(r Repeat, count int) bool {
	whole := 0
	for i := r.Index; i < r.Index+count; i++ {
		whole += p.Line(i)
	}
	return p.Collapsed(r) < whole
}

// Repeats finds the repetition among plain rows that Compress collapses: back-to-back runs and blocks that recur from earlier, each only
// where price says its marker costs less than the lines it stands for; with masked, lines compare with their digits masked. It is the one
// home of what counts as repeated output.
func Repeats(lines []PlainLine, masked bool, price Pricing) []Repeat {
	return RepeatsBy(lines, repeatKeys(lines, masked), price)
}

// RepeatsBy is Repeats with lines compared by keys, one per line; a copy counts as exact only where the texts themselves match.
func RepeatsBy(lines []PlainLine, keys []string, price Pricing) []Repeat {
	same := func(a, b, length int) bool {
		for offset := 0; offset < length; offset++ {
			if keys[a+offset] != keys[b+offset] {
				return false
			}
		}
		return true
	}
	var steps []Repeat
	printed := make([]bool, len(lines))
	starts := map[string][]int{}
	print := func(index int) {
		printed[index] = true
		starts[keys[index]] = append(starts[keys[index]], index)
	}
	cadences := map[int]*Repeat{}
	cadenceKnown := map[int]bool{}
	cadence := func(index int) *Repeat {
		if cadenceKnown[index] {
			return cadences[index]
		}
		var best *Repeat
		for period := 1; period <= cadenceLines && index+period*3 <= len(lines); period++ {
			blank := true
			for _, key := range keys[index : index+period] {
				if js.Trim(key) != "" {
					blank = false
					break
				}
			}
			if blank {
				continue
			}
			copies := 1
			for index+(copies+1)*period <= len(lines) && same(index, index+copies*period, period) {
				copies++
			}
			exact := true
			for copy := 1; copy < copies && exact; copy++ {
				exact = identicalLines(lines, index, index+copy*period, period)
			}
			rowCount := period*2 + 1
			if exact {
				rowCount = period + 1
			}
			candidate := Repeat{Kind: RepeatRun, Index: index, Period: period, Copies: copies, Exact: exact}
			if copies >= 3 && rowCount < copies*period && (best == nil || copies*period > best.Copies*best.Period) && price.pays(candidate, copies*period) {
				best = &candidate
			}
		}
		cadenceKnown[index] = true
		cadences[index] = best
		return best
	}
	for index := 0; index < len(lines); {
		if best := cadence(index); best != nil {
			for offset := 0; offset < best.Period; offset++ {
				print(index + offset)
			}
			if !best.Exact {
				for offset := 0; offset < best.Period; offset++ {
					print(index + (best.Copies-1)*best.Period + offset)
				}
			}
			steps = append(steps, *best)
			index += best.Copies * best.Period
			continue
		}
		// A recurring block ends where a back-to-back repeat begins on either side, so each repeat still collapses on its own.
		echoOf, echoLength := -1, 0
		candidates := starts[keys[index]]
		if len(candidates) > echoCandidates {
			candidates = candidates[len(candidates)-echoCandidates:]
		}
		for _, earlier := range candidates {
			length := 0
			for length < echoMaxLines && earlier+length < index && index+length < len(lines) && printed[earlier+length] &&
				keys[earlier+length] == keys[index+length] && (length == 0 || (cadence(index+length) == nil && cadence(earlier+length) == nil)) {
				length++
			}
			if length >= echoMinLines && (echoOf < 0 || length > echoLength) {
				echoOf, echoLength = earlier, length
			}
		}
		if echo := (Repeat{Kind: RepeatEcho, Index: index, Of: echoOf, Length: echoLength}); echoOf >= 0 && price.pays(echo, echoLength) {
			steps = append(steps, echo)
			index += echoLength
			continue
		}
		print(index)
		steps = append(steps, Repeat{Kind: RepeatLine, Index: index})
		index++
	}
	return steps
}

// repeatKeys are the texts lines compare by: as they are, or with their digits masked.
func repeatKeys(lines []PlainLine, masked bool) []string {
	keys := make([]string, len(lines))
	for i, l := range lines {
		keys[i] = l.Text
		if masked {
			keys[i] = MaskDigits(l.Text)
		}
	}
	return keys
}

// MaskDigits is a line's text with every run of digits masked, as Repeats compares masked lines.
func MaskDigits(text string) string { return digits.ReplaceAllString(text, "0") }

// Numbers are a line's runs of digits, in order: what MaskDigits masks.
func Numbers(text string) []string { return digits.FindAllString(text, -1) }

// tellingValues is how many values a number in one place of otherwise equal lines may take and still tell them apart, as a status code
// does; it also needs tellingShare lines per value on average, so ids, durations and timestamps stay masked.
const (
	tellingValues = 8
	tellingShare  = 4
)

// MaskedKeys are the texts lines compare by with their digits masked, except a number in a place where otherwise equal lines take few
// values, each often: POST /x 201 and POST /x 503 stay apart while their timestamps, ids and durations are masked.
func MaskedKeys(texts []string) []string {
	keys := make([]string, len(texts))
	groups := map[string][]int{}
	for i, text := range texts {
		keys[i] = MaskDigits(text)
		groups[keys[i]] = append(groups[keys[i]], i)
	}
	for _, members := range groups {
		if len(members) < 2*tellingShare {
			continue
		}
		numbers := make([][]string, len(members))
		for j, i := range members {
			numbers[j] = Numbers(texts[i])
		}
		telling := make([]bool, len(numbers[0]))
		tells := false
		for place := range telling {
			values := map[string]bool{}
			for _, n := range numbers {
				values[n[place]] = true
			}
			telling[place] = len(values) > 1 && len(values) <= tellingValues && len(values)*tellingShare <= len(members)
			tells = tells || telling[place]
		}
		if !tells {
			continue
		}
		for j, i := range members {
			place := -1
			keys[i] = digits.ReplaceAllStringFunc(texts[i], func(string) string {
				place++
				if telling[place] {
					return numbers[j][place]
				}
				return "0"
			})
		}
	}
	return keys
}

func identicalLines(lines []PlainLine, a, b, length int) bool {
	for offset := 0; offset < length; offset++ {
		if lines[a+offset].Text != lines[b+offset].Text {
			return false
		}
	}
	return true
}

// Compress collapses repetition among plain rows; with masked, lines compare with their digits masked, similar reports whether anything
// collapsed had other numbers than what is shown, and echoed whether a recurring block got [#N] markers.
func Compress(lines []PlainLine, masked bool) (rows []PlainLine, similar, echoed bool) {
	steps := Repeats(lines, masked, compressPricing(lines))
	// Ids follow the order the originals appear in; an original shows the most lines any of its echoes covered.
	type original struct{ id, length int }
	var ofs []int
	for _, s := range steps {
		if s.Kind == RepeatEcho && !slices.Contains(ofs, s.Of) {
			ofs = append(ofs, s.Of)
		}
	}
	slices.Sort(ofs)
	originals := map[int]*original{}
	for _, of := range ofs {
		originals[of] = &original{id: len(originals) + 1}
	}
	for _, s := range steps {
		if s.Kind == RepeatEcho {
			originals[s.Of].length = max(originals[s.Of].length, s.Length)
		}
	}
	line := func(index int) {
		l := lines[index]
		if o := originals[index]; o != nil {
			l.Text = echoTag(o.id, o.length) + l.Text
		}
		rows = append(rows, l)
	}
	for _, s := range steps {
		switch s.Kind {
		case RepeatLine:
			line(s.Index)
		case RepeatRun:
			for offset := 0; offset < s.Period; offset++ {
				line(s.Index + offset)
			}
			rows = append(rows, PlainLine{At: lines[s.Index+s.Period].At, Text: runNote(s)})
			if !s.Exact {
				similar = true
				for offset := 0; offset < s.Period; offset++ {
					line(s.Index + (s.Copies-1)*s.Period + offset)
				}
			}
		case RepeatEcho:
			l := lines[s.Index]
			o := originals[s.Of]
			count := itoa(s.Length)
			if s.Length < o.length {
				count = fmt.Sprintf("its first %d", s.Length)
			}
			if !identicalLines(lines, s.Of, s.Index, s.Length) {
				similar = true
			}
			rows = append(rows, PlainLine{At: l.At, Text: echoAgain(o.id, count) + l.Text})
		}
	}
	return rows, similar, len(originals) > 0
}

// compressPricing prices the rows Compress prints, each with its newline; an echo is counted with its original's tag, and ids as one digit.
func compressPricing(lines []PlainLine) Pricing {
	line := func(index int) int { return js.Len(lines[index].Text) + 1 }
	return Pricing{Line: line, Collapsed: func(r Repeat) int {
		if r.Kind == RepeatEcho {
			return js.Len(echoTag(1, r.Length)) + js.Len(echoAgain(1, itoa(r.Length))) + line(r.Index)
		}
		n := js.Len(runNote(r)) + 1
		for offset := 0; offset < r.Period; offset++ {
			n += line(r.Index + offset)
			if !r.Exact {
				n += line(r.Index + (r.Copies-1)*r.Period + offset)
			}
		}
		return n
	}}
}

// runNote is the row that stands for a run's copies after its first, or before its last when they differ in their digits.
func runNote(r Repeat) string {
	what := "the line above"
	if r.Period != 1 {
		what = fmt.Sprintf("the %d lines above", r.Period)
	}
	if !r.Exact {
		one := "ones"
		if r.Period == 1 {
			one = "one"
		}
		return fmt.Sprintf("[%d more like %s; the last %s:]", r.Copies-1, what, one)
	}
	plural, times := "", "time"
	if r.Period == 1 {
		plural = "s"
	}
	if r.Copies > 2 {
		times = "times"
	}
	return fmt.Sprintf("[%s repeat%s %d more %s]", what, plural, r.Copies-1, times)
}

// echoTag starts the first line of a block that recurs below; echoAgain starts the row that stands for it printed again.
func echoTag(id, length int) string { return fmt.Sprintf("[#%d, %d lines] ", id, length) }

func echoAgain(id int, count string) string { return fmt.Sprintf("[#%d again, %s lines] ", id, count) }

// selectedLines says whether a line falls inside any of a file's line selections; unlike a read, an open range runs to the end of the file.
func selectedLines(ctx *Context, file string, selections []selection) (func(int) bool, error) {
	length := 0
	if slices.ContainsFunc(selections, func(s selection) bool { return s.tail > 0 }) {
		handle, err := os.OpenFile(file, os.O_RDONLY|nonblock, 0)
		if err != nil {
			return nil, err
		}
		defer handle.Close()
		if err := confined(ctx, handle, file); err != nil {
			return nil, err
		}
		info, err := handle.Stat()
		if err != nil {
			return nil, err
		}
		s, err := scanLines(ctx, handle, info.Size(), func(int) bool { return false }, 0, 0)
		if err != nil {
			return nil, err
		}
		length = s.total
	}
	return func(line int) bool {
		for _, s := range selections {
			if s.tail > 0 {
				if line > length-s.tail {
					return true
				}
				continue
			}
			for _, r := range s.ranges {
				if line >= r.start && (r.end == 0 || line <= r.end) {
					return true
				}
			}
		}
		return false
	}, nil
}

type span2 struct{ start, end int }

// selectLines resolves a selection against a file length into sorted, merged, inclusive ranges.
func selectLines(s selection, length int) ([]span2, error) {
	if s.tail > 0 {
		return []span2{{max(1, length-s.tail+1), length}}, nil
	}
	requested := s.ranges
	if requested == nil {
		requested = []lineRange{{start: 1}}
	}
	sorted := make([]span2, len(requested))
	for i, r := range requested {
		end := r.end
		if end == 0 {
			end = r.start + ReadLines - 1
		}
		sorted[i] = span2{r.start, min(length, end)}
	}
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].start < sorted[b].start })
	if sorted[0].start > length {
		return nil, fmt.Errorf("Line %d is beyond the end of the file (%d lines)", sorted[0].start, length)
	}
	var merged []span2
	for _, r := range sorted {
		if r.start > length {
			continue
		}
		if n := len(merged); n > 0 && r.start <= merged[n-1].end+1 {
			merged[n-1].end = max(merged[n-1].end, r.end)
		} else {
			merged = append(merged, r)
		}
	}
	return merged, nil
}

// blockRow is one row of a read block; line > 0 marks a fully shown anchor row.
type blockRow struct {
	text string
	line int
	at   int
	// covers are the lines a held marker stands for.
	covers []int
	marks
}

// block is one displayable read entry; rows compete for the shared budget, while head and the footer always print.
type block struct {
	head   string
	rows   []blockRow
	finish func(shown int) []string
}

func noteBlock(head string) *block {
	return &block{head: head, finish: func(int) []string { return nil }}
}

func approximateTokens(size int) string {
	tokens := int(math.Round(float64(size) / 4))
	if tokens >= 1000 {
		return fmt.Sprintf("about %dk tokens", int(math.Round(float64(tokens)/1000)))
	}
	if tokens == 1 {
		return "about 1 token"
	}
	return fmt.Sprintf("about %d tokens", tokens)
}

func formatSize(size int64) string {
	if size < 1024 {
		if size == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", size)
	}
	if size < 1048576 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/1048576)
}

// formatAge is how long before now t was, in its largest whole unit up to days.
func formatAge(t time.Time) string {
	seconds := max(0, int(math.Floor(time.Since(t).Seconds())))
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds ago", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm ago", seconds/60)
	case seconds < 172800:
		return fmt.Sprintf("%dh ago", seconds/3600)
	}
	return fmt.Sprintf("%dd ago", seconds/86400)
}

var sizePattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)` + jsSpace + `*([kmg]?)(?:i?b)?$`)
var agePattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)` + jsSpace + `*([smhdw])$`)

// parseSize is the bytes in a size like 512, 20k, 1.5M or 2GiB; units count in 1024s.
func parseSize(input string) (int64, error) {
	match := sizePattern.FindStringSubmatch(js.Trim(input))
	if match == nil {
		return 0, fmt.Errorf("Size %s is not like 512, 20k, 1.5M or 2G", js.Quote(input))
	}
	value, _ := strconv.ParseFloat(match[1], 64)
	unit := map[string]float64{"": 1, "k": 1024, "m": 1024 * 1024, "g": 1024 * 1024 * 1024}[strings.ToLower(match[2])]
	return int64(math.Round(value * unit)), nil
}

// parseAge is the duration in an age like 30s, 10m, 2h, 3d or 1w.
func parseAge(input string) (time.Duration, error) {
	match := agePattern.FindStringSubmatch(js.Trim(input))
	if match == nil {
		return 0, fmt.Errorf("Age %s is not like 30s, 10m, 2h, 3d or 1w", js.Quote(input))
	}
	value, _ := strconv.ParseFloat(match[1], 64)
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[strings.ToLower(match[2])]
	return time.Duration(value * float64(unit)), nil
}

func listDirectory(file *os.File, display string) (*block, error) {
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		} else if e.Type()&os.ModeSymlink != 0 {
			suffix = "@"
		}
		names[i] = e.Name() + suffix
	}
	slices.Sort(names)
	rows := make([]blockRow, len(names))
	for i, n := range names {
		rows[i] = blockRow{text: n}
	}
	return &block{head: display + "/", rows: rows, finish: func(shown int) []string {
		if shown < len(names) {
			return []string{fmt.Sprintf("[%d more entries not shown]", len(names)-shown)}
		}
		return nil
	}}, nil
}

func directoryFacts(file *os.File, display string) (*block, error) {
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	directories := 0
	for _, e := range entries {
		if e.IsDir() {
			directories++
		}
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	entryWord, dirWord := "entries", "directories"
	if len(entries) == 1 {
		entryWord = "entry"
	}
	if directories == 1 {
		dirWord = "directory"
	}
	return noteBlock(fmt.Sprintf("%s/ directory, %d %s (%d %s); modified %s", display, len(entries), entryWord, directories, dirWord, formatAge(info.ModTime()))), nil
}

// ReadArgs are read's arguments.
type ReadArgs struct {
	Paths         []string
	MaxLineLength *int
	Plain         bool
	Collapse      *bool
	Info          bool
	// Full prints rows the agent already holds rather than their held markers.
	Full bool
}

// Read reads files, lists directories and attaches images.
func (e *Engine) Read(ctx *Context, args ReadArgs) (Result, error) {
	if args.Info && (args.Plain || args.Collapse != nil || args.MaxLineLength != nil) {
		return Result{}, errors.New("info: true gives facts instead of content, so it takes no plain, collapse or maxLineLength")
	}
	if args.Full && ctx.Markers {
		whole := *ctx
		whole.Markers = false
		ctx = &whole
	}
	var caps []string
	maxLineLength := valueOr(capped("maxLineLength", args.MaxLineLength, TextLimit, &caps), LineLimit)
	var blocks []*block
	var absent []string
	var images []Image
	single := false
	for _, entry := range args.Paths {
		selected, sel, err := splitSelector(entry, ctx)
		if err != nil {
			return Result{}, err
		}
		input, since := splitSince(selected, ctx)
		narrowed := sel.ranges != nil || sel.tail > 0 || sel.symbols != nil
		if since != "" && (narrowed || args.Info || args.Plain) {
			return Result{}, fmt.Errorf("%s: path#TAG shows what changed since that tag, so it takes no line selector, info or plain", entry)
		}
		if args.Info && narrowed {
			return Result{}, fmt.Errorf("%s: info describes whole files, so it takes no line selector", entry)
		}
		if args.Plain && sel.symbols != nil {
			return Result{}, fmt.Errorf("%s: a symbol is read as numbered rows, so it takes no plain", entry)
		}
		single = len(args.Paths) == 1 && len(sel.ranges) == 1 && sel.ranges[0].end == sel.ranges[0].start
		t, err := resolveTarget(input, ctx, false)
		var nf *NotFound
		if errors.As(err, &nf) && since == "" && sel.symbols == nil {
			// A path that names nothing may be DATABASE:TABLE, its row selector already split off.
			if found, table := databaseTable(ctx, input); found != nil {
				t, err, sel.symbols = found, nil, []string{table}
			}
		}
		if errors.As(err, &nf) {
			// A path that names nothing may be PATH@REV, which only then reads as one.
			rev, revErr := e.revisionOf(ctx, entry)
			var none *noCommit
			switch {
			case errors.As(revErr, &none):
				absent = append(absent, none.message)
				blocks = append(blocks, noteBlock("["+none.message+"]"))
				continue
			case revErr != nil:
				return Result{}, revErr
			case rev != nil:
				b, err := e.revisionBlock(ctx, rev, args, maxLineLength)
				if err != nil {
					return Result{}, err
				}
				blocks = append(blocks, b)
				continue
			}
		}
		if err != nil {
			var out *Outside
			switch {
			case errors.As(err, &out):
				absent = append(absent, out.message)
			case errors.As(err, &nf):
				absent = append(absent, missingPath(entry, ctx))
			default:
				return Result{}, err
			}
			// A path that names nothing, or that the sandbox refuses, is noted in its place, so the other files of a read still come back.
			blocks = append(blocks, noteBlock("["+absent[len(absent)-1]+"]"))
			continue
		}
		b, err := e.entryBlock(ctx, t, sel, since, args, maxLineLength, &images)
		if err != nil {
			return Result{}, err
		}
		blocks = append(blocks, b)
	}
	budget := OutputLimit
	if single && maxLineLength > LineLimit {
		budget = TextLimit + 1024
	}
	parts := fill(blocks, budget)
	if len(absent) == len(args.Paths) {
		return Result{}, errors.New(strings.Join(absent, "; "))
	}
	return Result{Text: strings.Join(append(caps, parts...), "\n\n"), Images: images}, nil
}

// entryBlock is read's block for one resolved path: a listing, facts, plain lines, an image note, changes since a tag, or rows.
func (e *Engine) entryBlock(ctx *Context, t *target, sel selection, since string, args ReadArgs, maxLineLength int, images *[]Image) (*block, error) {
	return serialized(t.canonical, ctx, func() (*block, error) {
		return inParent(t, func(p *parent) (*block, error) {
			file, err := p.open()
			if err != nil {
				return nil, err
			}
			defer file.Close()
			if err := checkDescriptor(file, p, p.name, t.canonical); err != nil {
				return nil, err
			}
			info, err := file.Stat()
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				if args.Info {
					return directoryFacts(file, t.display)
				}
				return listDirectory(file, t.display)
			}
			if args.Info {
				return e.facts(ctx, t, file)
			}
			if isDatabase(file) {
				return e.databaseBlock(ctx, t, sel)
			}
			// Images and PDFs fall through to the usual handling, since plain only changes how text lines print.
			if args.Plain {
				collapse := args.Collapse == nil || *args.Collapse
				plain, err := e.plainBlock(ctx, t, file, sel, maxLineLength, collapse)
				if err != nil || plain != nil {
					return plain, err
				}
			}
			// A file too big to hold as text is noted rather than failing the call, so the other files of a read still come back.
			tooBig := func(size int64) *block {
				return noteBlock(fmt.Sprintf("[%s] skipped: %.1f MiB, over the 2 MiB text limit. Read it with plain: true (a range like %s:-200 works too), or grep it.", t.display, float64(size)/1048576, t.display))
			}
			if info.Size() > MediaLimit {
				return tooBig(info.Size()), nil
			}
			current, err := snapshot(file, MediaLimit)
			if err != nil {
				return nil, err
			}
			if err := p.check(); err != nil {
				return nil, err
			}
			if err := checkDescriptor(file, p, p.name, t.canonical); err != nil {
				return nil, err
			}
			if err := ctx.checkAbort(); err != nil {
				return nil, err
			}
			attachment := mime(current.bytes)
			if attachment == "application/pdf" {
				return noteBlock(t.display + ": PDF is not supported here; use the built-in Read tool"), nil
			}
			if attachment != "" {
				*images = append(*images, Image{Data: current.bytes, MimeType: attachment})
				return noteBlock(fmt.Sprintf("%s: %s image attached", t.display, attachment)), nil
			}
			if len(current.bytes) > TextLimit {
				return tooBig(int64(len(current.bytes))), nil
			}
			source, err := decodeText(current.bytes)
			if err != nil {
				return nil, err
			}
			if since != "" {
				return e.sinceBlock(ctx, t, source, since, maxLineLength), nil
			}
			return e.textBlock(ctx, t, source, sel, maxLineLength, args.MaxLineLength != nil)
		})
	})
}

// fill prints blocks within budget, shared so small blocks show whole and large ones split the rest; a block's head and footer always print,
// and an empty head none.
func fill(blocks []*block, budget int) []string {
	size := func(text string) int { return len(text) + 1 }
	demands := make([]int, len(blocks))
	for i, b := range blocks {
		for _, r := range b.rows {
			demands[i] += size(r.text)
		}
	}
	shares := allocate(demands, budget)
	parts := make([]string, len(blocks))
	for index, b := range blocks {
		left := shares[index]
		shown := 0
		for shown < len(b.rows) && size(b.rows[shown].text) <= left {
			left -= size(b.rows[shown].text)
			shown++
		}
		var lines []string
		if b.head != "" {
			lines = append(lines, b.head)
		}
		for _, r := range b.rows[:shown] {
			lines = append(lines, r.text)
		}
		lines = append(lines, b.finish(shown)...)
		parts[index] = strings.Join(lines, "\n")
	}
	return parts
}

// facts is one line of facts about a file instead of its content; a file nib can hold as text also gets its tag, recorded with no line seen.
func (e *Engine) facts(ctx *Context, t *target, file *os.File) (*block, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	line := func(tagValue string, facts []string) *block {
		head := "[" + t.display
		if tagValue != "" {
			head += "#" + tagValue
		}
		return noteBlock(head + "] " + strings.Join(append(facts, "modified "+formatAge(info.ModTime())), "; "))
	}
	start := make([]byte, min(size, 16))
	n, _ := file.ReadAt(start, 0)
	if attachment := mime(start[:n]); attachment != "" {
		kind := attachment
		if attachment == "application/pdf" {
			kind = "PDF"
		}
		return line("", []string{fmt.Sprintf("%s, %s", kind, formatSize(size))}), nil
	}
	// A log still being written cannot be held as one version, so like a file over the text limit it is only streamed.
	var current *held
	if size <= TextLimit {
		current, err = snapshot(file, TextLimit)
		if err != nil {
			var changed changedWhileReading
			if !errors.As(err, &changed) {
				return nil, err
			}
			current = nil
		}
	}
	var facts lineFactsResult
	tagValue, kind := "", ""
	if current != nil {
		counter := newLineFacts()
		counter.feed(current.bytes)
		facts = counter.done()
		if !facts.binary {
			source, err := decodeText(current.bytes)
			if err != nil {
				return line("", []string{"not UTF-8 text, " + formatSize(size)}), nil
			}
			anchored := viewOf(source)
			tagValue = anchored.Tag
			kind = generatedKind(t.canonical, len(source.body), anchored.Lines)
			e.Store.Record(ctx.Session, t.canonical, anchored, []int{}, nil)
		}
	} else {
		facts, err = measure(ctx, file, size)
		if err != nil {
			return nil, err
		}
		kind = generatedKind(t.canonical, int(size), nil)
	}
	if facts.binary {
		return line("", []string{"binary, " + formatSize(size)}), nil
	}
	if facts.lines == 0 {
		return line(tagValue, []string{"empty"}), nil
	}
	lineWord, charWord := "lines", "chars"
	if facts.lines == 1 {
		lineWord = "line"
	}
	if facts.longest == 1 {
		charWord = "char"
	}
	longest := fmt.Sprintf("longest line %d %s", facts.longest, charWord)
	if facts.lines > 1 {
		longest += fmt.Sprintf(" (line %d)", facts.longestAt)
	}
	parts := []string{fmt.Sprintf("%d %s, %s, %s", facts.lines, lineWord, formatSize(size), approximateTokens(int(size))), longest}
	switch {
	case facts.crlf > 0 && facts.lf > 0:
		parts = append(parts, fmt.Sprintf("mixed line endings (%d CRLF, %d LF)", facts.crlf, facts.lf))
	case facts.crlf > 0:
		parts = append(parts, "CRLF")
	case facts.lf > 0:
		parts = append(parts, "LF")
	}
	if !facts.final {
		parts = append(parts, "no final newline")
	}
	if facts.bom {
		parts = append(parts, "BOM")
	}
	if facts.escapes {
		parts = append(parts, "color codes")
	}
	if kind != "" {
		parts = append(parts, kind)
	}
	if current == nil {
		reason := "changing while read"
		if size > TextLimit {
			reason = "over 2 MiB"
		}
		parts = append(parts, reason+", so no tag: read it with plain: true, or grep it")
	}
	return line(tagValue, parts), nil
}

// sinceBlock shows what changed since the version tagged since: changed lines with context, numbered as the file is now, and removed lines as -| rows.
func (e *Engine) sinceBlock(ctx *Context, t *target, source *text, since string, maxLineLength int) *block {
	live := viewOf(source)
	var old *snapshots.Version
	for _, v := range e.Store.Versions(ctx.Session, t.canonical) {
		if v.Tag == since {
			old = v
			break
		}
	}
	if old == nil {
		elsewhere := e.Store.Origins(ctx.Session, since, t.canonical)
		extra := ""
		if len(elsewhere) > 0 {
			extra = fmt.Sprintf(" (it was issued for %s)", strings.Join(elsewhere, ", "))
		}
		return noteBlock(fmt.Sprintf("[%s] #%s is not a version of this file that nib kept in this session%s; read the file instead.", t.display, since, extra))
	}
	if old.Key == live.Key {
		return noteBlock(fmt.Sprintf("[%s#%s] unchanged since #%s", t.display, live.Tag, since))
	}
	changes, ok := snapshots.LineChanges(old.Lines, live.Lines)
	if !ok {
		e.Store.Record(ctx.Session, t.canonical, live, []int{}, nil)
		return noteBlock(fmt.Sprintf("[%s#%s] changed too much since #%s to compare in time; read it again.", t.display, live.Tag, since))
	}
	// A current line with old kept its content from that old line; one without was added.
	type entry struct {
		removed *string
		line    int
		old     int
	}
	var entries []entry
	before, after := 1, 1
	for _, change := range changes {
		if change.Removed {
			for i := range change.Value {
				entries = append(entries, entry{removed: &change.Value[i]})
				before++
			}
			continue
		}
		for range change.Value {
			if change.Added {
				entries = append(entries, entry{line: after})
			} else {
				entries = append(entries, entry{line: after, old: before})
				before++
			}
			after++
		}
	}
	listed := map[int]bool{}
	for index, en := range entries {
		if en.removed == nil && en.old != 0 {
			continue
		}
		for near := max(0, index-previewContext); near <= min(len(entries)-1, index+previewContext); near++ {
			listed[near] = true
		}
	}
	var rows []blockRow
	for _, index := range sortedSet(listed) {
		en := entries[index]
		if en.removed != nil {
			shown := visible(*en.removed)
			if js.Len(shown) > maxLineLength {
				shown = js.Slice(shown, 0, maxLineLength) + "..."
			}
			rows = append(rows, blockRow{text: "-|" + shown})
		} else {
			r := lineRow(en.line, live.Lines[en.line-1], maxLineLength)
			marker := " "
			if en.old == 0 {
				marker = "+"
			}
			if r.anchor {
				rows = append(rows, blockRow{text: marker + r.text, line: en.line})
			} else {
				rows = append(rows, blockRow{text: marker + r.text, marks: r.marks})
			}
		}
	}
	knew := func(line int) bool { return old.Seen != nil && knownLine(old, line) }
	// Unchanged lines that moved by one offset, as stretches; only those holding a line the agent knew get a note.
	type stretch struct {
		from, to, offset int
		known            bool
	}
	var moved []*stretch
	for _, en := range entries {
		if en.removed != nil || en.old == 0 || en.old == en.line {
			continue
		}
		offset := en.line - en.old
		if n := len(moved); n > 0 && moved[n-1].to == en.line-1 && moved[n-1].offset == offset {
			moved[n-1].to = en.line
		} else {
			moved = append(moved, &stretch{en.line, en.line, offset, false})
		}
		last := moved[len(moved)-1]
		last.known = last.known || knew(en.old)
	}
	spanText := func(low, high int) string {
		if low == high {
			return itoa(low)
		}
		return fmt.Sprintf("%d-%d", low, high)
	}
	var stretches []*stretch
	for _, s := range moved {
		if s.known {
			stretches = append(stretches, s)
		}
	}
	var notes []string
	for _, s := range stretches[:min(len(stretches), sinceNotes)] {
		plural, verb := "s", "are"
		if s.from == s.to {
			plural, verb = "", "is"
		}
		direction := "down"
		if s.offset < 0 {
			direction = "up"
		}
		notes = append(notes, fmt.Sprintf("[line%s %s %s old line%s %s, moved %s by %d]", plural, spanText(s.from, s.to), verb, plural, spanText(s.from-s.offset, s.to-s.offset), direction, abs(s.offset)))
	}
	if len(stretches) > sinceNotes {
		notes = append(notes, fmt.Sprintf("[%d more stretches of lines you knew moved]", len(stretches)-sinceNotes))
	}
	var carried, known []int
	added := 0
	for _, en := range entries {
		if en.removed != nil {
			continue
		}
		if en.old == en.line && en.old != 0 && (old.Seen == nil || old.Seen[en.old]) {
			carried = append(carried, en.line)
		}
		if en.old != 0 && knew(en.old) {
			known = append(known, en.line)
		}
		if en.old == 0 {
			added++
		}
	}
	addedWord := "lines"
	if added == 1 {
		addedWord = "line"
	}
	return &block{
		head: fmt.Sprintf("[%s#%s] changed since #%s: %d %s added, %d removed", t.display, live.Tag, since, added, addedWord, len(entries)-len(live.Lines)),
		rows: rows,
		finish: func(shown int) []string {
			seen := slices.Clone(carried)
			var m []marks
			for _, r := range rows[:shown] {
				if r.line > 0 {
					seen = append(seen, r.line)
				}
				m = append(m, r.marks)
			}
			e.Store.Record(ctx.Session, t.canonical, live, nonNil(seen), nonNil(known))
			var out []string
			if shown < len(rows) {
				out = append(out, "[changes cut by the output limit; read the ranges you need]")
			}
			out = append(out, notes...)
			return append(out, rowNotes(t.display, m)...)
		},
	}
}

var controlChars = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)

// PlainText is a line for looking only: escape codes and other control characters removed.
func PlainText(content string) string {
	return controlChars.ReplaceAllString(visible(content), "")
}

// plainBlock is lines for looking only: no numbers, tag or snapshot, escape codes stripped, long lines shortened and, unless collapse is false,
// repetition collapsed. It streams the file, so huge logs and logs still being written read fine; nil for images and PDFs.
func (e *Engine) plainBlock(ctx *Context, t *target, file *os.File, sel selection, maxLineLength int, collapse bool) (*block, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	start := make([]byte, min(size, 8192))
	n, _ := file.ReadAt(start, 0)
	if mime(start[:n]) != "" {
		return nil, nil
	}
	if bytes.IndexByte(start[:n], 0) >= 0 {
		return nil, errors.New("Binary files are not supported")
	}
	// A line can carry many escape bytes, so more is kept than the visible width.
	limit := maxLineLength*4 + 4096
	scan := func(wanted func(int) bool, ring int) (scanned, error) {
		return scanLines(ctx, file, size, wanted, limit, ring)
	}
	rangesWanted := sel.ranges
	if rangesWanted == nil {
		rangesWanted = []lineRange{{start: 1}}
	}
	inRange := func(line int) bool {
		for _, r := range rangesWanted {
			end := r.end
			if end == 0 {
				end = r.start + ReadLines - 1
			}
			if line >= r.start && line <= end {
				return true
			}
		}
		return false
	}
	var result scanned
	if sel.tail > 0 {
		// A tail that fits the line cap is kept in one pass as a ring of the last lines; a longer one needs the line count first.
		if sel.tail <= ReadLines {
			result, err = scan(func(int) bool { return true }, sel.tail)
		} else {
			var counted scanned
			counted, err = scan(func(int) bool { return false }, 0)
			if err == nil {
				before := counted.total - sel.tail
				result, err = scan(func(line int) bool { return line > before }, 0)
			}
		}
	} else {
		result, err = scan(inRange, 0)
	}
	if err != nil {
		return nil, err
	}
	if result.total == 0 {
		return noteBlock(fmt.Sprintf("[%s] empty, plain", t.display)), nil
	}
	selected, err := selectLines(sel, result.total)
	if err != nil {
		return nil, err
	}
	var rows []PlainLine
	similar, echoed := false, false
	// Repetition is collapsed per selected range, never across a gap between ranges.
	var segment []PlainLine
	flush := func() {
		if collapse {
			compressed, s, echo := Compress(segment, true)
			rows = append(rows, compressed...)
			similar = similar || s
			echoed = echoed || echo
		} else {
			rows = append(rows, segment...)
		}
		segment = nil
	}
	missingLine := 0
	last := 0
	for _, r := range selected {
		if last > 0 && r.start > last+1 {
			flush()
			rows = append(rows, PlainLine{At: r.start, Text: fmt.Sprintf("[lines %d-%d skipped]", last+1, r.start-1)})
		}
		for line := r.start; line <= r.end && missingLine == 0; line++ {
			entry, ok := result.kept[line]
			if !ok {
				missingLine = line
				break
			}
			content := PlainText(strings.ToValidUTF8(string(entry.bytes), "\ufffd"))
			if line == 1 {
				content = strings.TrimPrefix(content, bom)
			}
			if entry.cut || js.Len(content) > maxLineLength {
				content = js.Slice(content, 0, maxLineLength) + "..."
			}
			segment = append(segment, PlainLine{At: line, Text: content})
			last = line
		}
		if missingLine != 0 {
			break
		}
	}
	flush()
	whole := len(selected) == 1 && selected[0].start == 1 && selected[0].end == result.total
	head := fmt.Sprintf("[%s] %d lines, plain", t.display, result.total)
	if !whole {
		var parts []string
		for _, r := range selected {
			if r.start == r.end {
				parts = append(parts, itoa(r.start))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", r.start, r.end))
			}
		}
		head = fmt.Sprintf("[%s] lines %s of %d, plain", t.display, strings.Join(parts, ", "), result.total)
	}
	blockRows := make([]blockRow, len(rows))
	for i, r := range rows {
		blockRows[i] = blockRow{text: r.Text, at: r.At}
	}
	return &block{head: head, rows: blockRows, finish: func(shown int) []string {
		var notes []string
		if similar {
			notes = []string{"[repeats were matched with their numbers ignored; read with collapse: false to see every line]"}
		}
		if echoed {
			notes = append(notes, e.EchoNote(ctx)...)
		}
		if shown < len(blockRows) {
			next := blockRows[shown]
			if len(next.text)+1 > OutputLimit {
				return append([]string{fmt.Sprintf("[line %d is longer than one call's output; read %s:%d alone]", next.at, t.display, next.at)}, notes...)
			}
			return append([]string{fmt.Sprintf("[output limit reached; continue with %s:%d-]", t.display, next.at)}, notes...)
		}
		if missingLine != 0 {
			return append([]string{fmt.Sprintf("[continue with %s:%d-]", t.display, missingLine)}, notes...)
		}
		open := sel.tail == 0 && !slices.ContainsFunc(rangesWanted, func(r lineRange) bool { return r.end != 0 })
		if open && last < result.total {
			return append([]string{fmt.Sprintf("[continue with %s:%d-]", t.display, last+1)}, notes...)
		}
		return notes
	}}, nil
}

// textBlock reads a file as numbered rows; explicit is whether the agent chose maxLineLength, since without it a minified file is cut at minifiedColumns.
func (e *Engine) textBlock(ctx *Context, t *target, source *text, sel selection, maxLineLength int, explicit bool) (*block, error) {
	anchored := viewOf(source)
	for _, symbol := range sel.symbols {
		found, err := symbolRanges(t.display, anchored.Lines, symbol)
		if err != nil {
			return noteBlock("[" + err.Error() + "]"), nil
		}
		sel.ranges = append(sel.ranges, found...)
	}
	total := len(anchored.Lines)
	size := len(source.body)
	squeezed := !explicit && maxLineLength > minifiedColumns && slices.ContainsFunc(anchored.Lines, func(l string) bool { return js.Len(l) > minifiedColumns }) &&
		minifiedReason(t.canonical, size, anchored.Lines) != ""
	limit := maxLineLength
	// name is how the path prints in what the block says: with @REV for a revision, so a range from it reads the same revision.
	name := t.display + t.rev
	head := fmt.Sprintf("[%s#%s]", t.display, anchored.Tag)
	if t.rev != "" {
		head = "[" + name + "]"
	}
	if squeezed {
		limit = minifiedColumns
		head += fmt.Sprintf(" minified, so lines are cut at %d chars; pass maxLineLength for more", minifiedColumns)
	}
	rowFor := func(line int) blockRow {
		r := lineRow(line, anchored.Lines[line-1], limit)
		if r.anchor && t.rev != "" {
			// A revision's line is no line of the file as it is now.
			return blockRow{text: itoa(line) + "|" + anchored.Lines[line-1], at: line}
		}
		if r.anchor {
			return blockRow{text: r.text, line: line, at: line}
		}
		return blockRow{text: r.text, at: line, marks: r.marks}
	}
	held := func(rows []blockRow) []blockRow {
		if t.rev != "" {
			return rows
		}
		return e.heldRows(ctx, t.canonical, anchored, rows)
	}
	record := func(rows []blockRow) {
		if t.rev != "" {
			return
		}
		seen := []int{}
		for _, r := range rows {
			if r.line > 0 {
				seen = append(seen, r.line)
			}
			seen = append(seen, r.covers...)
		}
		e.Store.Record(ctx.Session, t.canonical, anchored, seen, nil)
	}
	cuts := func(rows []blockRow) []string {
		m := make([]marks, len(rows))
		markers := false
		for i, r := range rows {
			m[i] = r.marks
			markers = markers || r.covers != nil
		}
		if t.rev != "" {
			// No row of a revision is an anchor, so there is nothing to change in place.
			if slices.ContainsFunc(m, func(m marks) bool { return m.cut }) {
				return []string{fmt.Sprintf("[cut rows: read %s:N with maxLineLength to see one whole]", name)}
			}
			return nil
		}
		notes := rowNotes(name, m)
		if markers {
			notes = append(notes, e.HeldNote(ctx)...)
		}
		return notes
	}
	// A row bigger than a whole call's budget never fits, so continuing from it would repeat the same cut.
	stop := func(line int, text string) string {
		if len(text)+1 > OutputLimit {
			return fmt.Sprintf("[line %d has %d chars, more than one call's output; read %s:%d alone]", line, js.Len(anchored.Lines[line-1]), name, line)
		}
		return fmt.Sprintf("[output limit reached; continue with %s:%d-]", name, line)
	}
	sizeText := fmt.Sprintf("%d lines, %s", total, approximateTokens(size))
	if sel.ranges == nil && sel.tail == 0 {
		if reason := junk(t.canonical, size, anchored.Lines); reason != "" {
			return noteBlock(fmt.Sprintf("[%s] skipped: %s, %s. Read %s:1- to read it anyway, a range like %s:1-100, or grep it.", name, reason, sizeText, name, name)), nil
		}
		large := total > outlineLines || size > outlineBytes
		if t.commit {
			if diffAt := slices.IndexFunc(anchored.Lines, isDiffStart); large && diffAt > 0 {
				rows := make([]blockRow, diffAt)
				for i := range rows {
					rows[i] = rowFor(i + 1)
				}
				return &block{
					head: fmt.Sprintf("%s %s; header and files only. Read the diff with %s:%d-, or one file's with %s:PATH.", head, sizeText, name, diffAt+1, name),
					rows: rows,
					finish: func(shown int) []string {
						if shown < len(rows) {
							return []string{stop(rows[shown].at, rows[shown].text)}
						}
						return nil
					},
				}, nil
			}
		} else if large || total > codeOutlineLines || size > codeOutlineBytes {
			s := outline(t.display, anchored.Lines)
			anchors := 0
			for _, r := range s.rows {
				if r.line > 0 {
					anchors++
				}
			}
			nonBlank := 0
			for _, l := range anchored.Lines {
				if js.Trim(l) != "" {
					nonBlank++
				}
			}
			// Flat data (logs, CSV, lists) puts nearly every line at the top level; its head says more than such an "outline".
			if anchors >= outlineMinRows && s.candidates*2 <= nonBlank && (large || len(s.rows)*3 <= total) {
				rows := make([]blockRow, len(s.rows))
				for i, r := range s.rows {
					if r.line > 0 {
						rows[i] = rowFor(r.line)
					} else {
						rows[i] = blockRow{text: r.text}
					}
				}
				rows = held(rows)
				return &block{
					head: fmt.Sprintf("%s %s; outline only. Read the ranges you need, like %s:120-180, or %s:1- for everything.", head, sizeText, name, name),
					rows: rows,
					finish: func(shown int) []string {
						record(rows[:shown])
						var out []string
						if shown < len(rows) {
							out = append(out, "[outline cut by the output limit]")
						}
						return append(out, cuts(rows[:shown])...)
					},
				}, nil
			}
			if large {
				count := min(headLines, total)
				rows := make([]blockRow, count)
				for i := range rows {
					rows[i] = rowFor(i + 1)
				}
				rows = held(rows)
				return &block{
					head: fmt.Sprintf("%s %s; first %d lines only. Continue with %s:%d-, or grep it.", head, sizeText, count, name, count+1),
					rows: rows,
					finish: func(shown int) []string {
						record(rows[:shown])
						var out []string
						if shown < len(rows) {
							out = append(out, stop(rows[shown].at, rows[shown].text))
						}
						return append(out, cuts(rows[:shown])...)
					},
				}, nil
			}
		}
	}
	selected, err := selectLines(sel, total)
	if err != nil {
		return nil, err
	}
	var rows []blockRow
	last := 0
	for _, r := range selected {
		for line := r.start; line <= r.end; line++ {
			rows = append(rows, rowFor(line))
		}
		last = r.end
	}
	rows = held(rows)
	return &block{head: head, rows: rows, finish: func(shown int) []string {
		record(rows[:shown])
		notes := cuts(rows[:shown])
		if shown < len(rows) {
			return append([]string{stop(rows[shown].at, rows[shown].text)}, notes...)
		}
		whole := len(selected) == 1 && selected[0].start == 1 && selected[0].end == total
		// The missing final newline shows only with the last line, where a task to add one would look for it.
		var ending []string
		if last == total && t.rev == "" && source.body != "" && !strings.HasSuffix(source.body, "\n") {
			ending = []string{"[no final newline; " + finalNewlineHint + "]"}
		}
		if whole {
			return append(ending, notes...)
		}
		rangesWanted := sel.ranges
		if rangesWanted == nil {
			rangesWanted = []lineRange{{start: 1}}
		}
		open := sel.tail == 0 && !slices.ContainsFunc(rangesWanted, func(r lineRange) bool { return r.end != 0 })
		footer := fmt.Sprintf("[%d lines total]", total)
		if open && last < total {
			footer = fmt.Sprintf("[%d lines total; continue with %s:%d-]", total, name, last+1)
		}
		return append(append(ending, footer), notes...)
	}}, nil
}

// heldRows puts each run of rows the agent already holds as its marker, which covers the run's lines.
func (e *Engine) heldRows(ctx *Context, canonical string, view snapshots.View, rows []blockRow) []blockRow {
	return HeldGroups(rows, e.HeldLines(ctx, canonical, view), func(r blockRow) int { return r.line }, func(run []blockRow, r HeldRun) blockRow {
		return blockRow{text: r.Marker(), at: run[0].at, covers: r.Lines()}
	})
}
