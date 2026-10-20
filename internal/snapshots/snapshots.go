// Package snapshots keeps per-session file versions with the lines the agent saw or wrote, and rebases edits across versions.
package snapshots

import (
	"container/list"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/11ways/nib/internal/diff"
	"github.com/11ways/nib/internal/js"
	"github.com/11ways/nib/internal/tag"
)

// View is the text identity of one file version: LF-normalized BOM-free body, its line contents, and its tag.
type View struct {
	Key   string
	Lines []string
	Tag   string
}

// Version is a retained View; Seen is nil when edits may anchor anywhere, Known holds lines whose content the agent wrote or saw before they moved.
type Version struct {
	View
	Seen  map[int]bool
	Known map[int]bool
	// at is when the store first held this version, and seenAt and knownAt when each line joined Seen or Known, in Unix nanoseconds; Copy
	// takes only what is older than its moment.
	at              int64
	seenAt, knownAt map[int]int64
}

// At is when the store first held the version.
func (v *Version) At() time.Time { return time.Unix(0, v.at) }

// Operation is how a LineEdit changes its lines.
type Operation int

const (
	Replace Operation = iota
	Before
	After
)

// Tail is the start and end of an edit that appends after whatever the last line is when applied.
const Tail = int(^uint(0) >> 2)

// LineEdit is one edit in 1-based line coordinates; see the TypeScript LineEdit for what each field means. Import is an IMPORT's name,
// which nib places among the file's imports itself, so the edit depends on no line the agent saw.
type LineEdit struct {
	Start, End int
	Operation  Operation
	Content    []string
	Edge       bool
	Block      bool
	Checks     []string
	HasChecks  bool
	Confirmed  []int
	Sub        *string
	Quoted     bool
	Opaque     bool
	Old        []string
	Written    string
	Import     string
}

const (
	// maxPaths bounds the agent and file pairs held; text is bounded by maxUnits, so it can be generous. Subagents each hold their own, and
	// nibash records every file its own commands show, so a long session reaches a few hundred quickly.
	maxPaths    = 4096
	maxVersions = 4
	maxUnits    = 64 * 1024 * 1024
	// maxForgotten bounds the evicted pairs remembered, so a refusal can say its earlier views were dropped.
	maxForgotten = 16384
	diffTimeout  = time.Second
)

// EvidenceMinRunes is the shortest trimmed text that, unique in a file, tells which line an anchor meant.
const EvidenceMinRunes = 8

// Limits bound a Store.
type Limits struct{ Paths, Versions, Units int }

type history struct {
	id       string
	versions []*Version
	units    int
	// forgotten is set when an earlier history of the same agent and file was evicted: lines seen before then no longer count.
	forgotten bool
}

// Store holds per-session file versions with LRU eviction across all sessions and paths. It is safe for concurrent use; a Version's line
// sets change only in Record, so a caller reads one it holds only while it keeps others from recording the same session and path.
type Store struct {
	mu        sync.Mutex
	limits    Limits
	histories map[string]*list.Element
	order     *list.List
	units     int
	forgotten map[string]bool
	evicted   *list.List
	taught    map[string]bool
	// cleared is the latest moment each session's context was dropped, in Unix nanoseconds, so a drop told again is applied once.
	cleared map[string]int64
	// Now is the wall clock lines are stamped with when they become seen or known; it must be comparable with other processes' clocks, as a
	// fork's moment comes from its harness. Set it before first use.
	Now func() time.Time
	// durable keeps what sessions with a lasting key saw across restarts; nil keeps nothing.
	durable *durable
}

// NewStore makes a store with nib's default limits.
func NewStore() *Store { return NewStoreWith(Limits{maxPaths, maxVersions, maxUnits}) }

// NewStoreWith makes a store with the given limits.
func NewStoreWith(limits Limits) *Store {
	return &Store{limits: limits, histories: map[string]*list.Element{}, order: list.New(), forgotten: map[string]bool{}, evicted: list.New(), taught: map[string]bool{}, cleared: map[string]int64{}, Now: time.Now}
}

func id(session, path string) string { return session + "\x00" + path }

// Record makes view the most recent version and unions seen and known into its line sets; a nil seen leaves Seen as it is. A line keeps the
// moment it first joined a set. A view new to the history, when seen is given, starts with what the agent held of the newest version before
// it, however the file changed: a line that kept its number and text stays seen, one that moved is known content a ~ check anchors, each
// with its first moment, so dropping a context later drops them alike.
func (s *Store) Record(session, path string, view View, seen, known []int) *Version {
	var inherited *carried
	if seen != nil {
		inherited = s.carry(session, path, view)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now().UnixNano()
	key := id(session, path)
	h := &history{id: key, forgotten: s.forgotten[key]}
	delete(s.forgotten, key)
	if element, ok := s.histories[key]; ok {
		h = element.Value.(*history)
		s.order.Remove(element)
		delete(s.histories, key)
		s.units -= h.units
	}
	var version *Version
	for i, existing := range h.versions {
		if existing.Key == view.Key {
			version = existing
			h.versions = slices.Delete(h.versions, i, i+1)
			break
		}
	}
	if version == nil {
		version = &Version{View: view, at: now}
		// A version recorded since the diff is the newer basis, and its lines were not carried.
		if inherited != nil && len(h.versions) > 0 && h.versions[0].Key == inherited.from {
			version.Seen, version.seenAt = inherited.seen, inherited.seenAt
			version.Known, version.knownAt = inherited.known, inherited.knownAt
		}
		// The first version of a file since the server started takes up what the agent saw of it before a restart.
		if seen != nil && len(h.versions) == 0 {
			s.restore(session, path, version)
		}
	}
	if seen != nil {
		version.Seen, version.seenAt = stamp(version.Seen, version.seenAt, seen, now)
	}
	if known != nil {
		version.Known, version.knownAt = stamp(version.Known, version.knownAt, known, now)
	}
	h.versions = append([]*Version{version}, h.versions...)
	if len(h.versions) > s.limits.Versions {
		h.versions = h.versions[:s.limits.Versions]
	}
	h.units = 1
	for _, v := range h.versions {
		h.units += js.Len(v.Key) * 2
	}
	s.histories[key] = s.order.PushBack(h)
	s.units += h.units
	s.evict()
	s.changed(session)
	return version
}

// carried are the lines of a newest version that hold for a new view, with the moments they joined their sets.
type carried struct {
	from            string
	seen, known     map[int]bool
	seenAt, knownAt map[int]int64
}

// carry is what the agent held of the newest version of path that still holds for view, or nil when view is not new or nothing restricts the
// newest version; it copies under the lock and diffs outside it, as Held does.
func (s *Store) carry(session, path string, view View) *carried {
	s.mu.Lock()
	element, ok := s.histories[id(session, path)]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	versions := element.Value.(*history).versions
	if len(versions) == 0 || versions[0].Seen == nil || slices.ContainsFunc(versions, func(v *Version) bool { return v.Key == view.Key }) {
		s.mu.Unlock()
		return nil
	}
	newest := versions[0]
	from, lines := newest.Key, newest.Lines
	seen, seenAt := before(newest.Seen, newest.seenAt, math.MaxInt64)
	known, knownAt := before(newest.Known, newest.knownAt, math.MaxInt64)
	s.mu.Unlock()
	mapping, ok := LineMap(lines, view.Lines)
	if !ok {
		return nil
	}
	c := &carried{from: from, seen: map[int]bool{}, seenAt: map[int]int64{}, known: map[int]bool{}, knownAt: map[int]int64{}}
	add := func(set map[int]bool, at map[int]int64, line int, moment int64) {
		if !set[line] || moment < at[line] {
			set[line], at[line] = true, moment
		}
	}
	for old := range seen {
		if now, ok := mapping[old]; ok {
			if now == old {
				add(c.seen, c.seenAt, now, seenAt[old])
			}
			add(c.known, c.knownAt, now, seenAt[old])
		}
	}
	for old := range known {
		if now, ok := mapping[old]; ok {
			add(c.known, c.knownAt, now, knownAt[old])
		}
	}
	return c
}

// evict drops least recently used histories until the store is within its limits, remembering which.
func (s *Store) evict() {
	for len(s.histories) > s.limits.Paths || s.units > s.limits.Units {
		oldest := s.order.Front()
		if oldest == nil {
			break
		}
		old := oldest.Value.(*history)
		s.units -= old.units
		s.order.Remove(oldest)
		delete(s.histories, old.id)
		s.forgotten[old.id] = true
		s.evicted.PushBack(old.id)
	}
	for s.evicted.Len() > maxForgotten {
		delete(s.forgotten, s.evicted.Remove(s.evicted.Front()).(string))
	}
}

// Forgotten reports whether the store dropped an earlier history of session's views of path, so lines seen before then no longer count.
func (s *Store) Forgotten(session, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.histories[id(session, path)]; ok {
		return element.Value.(*history).forgotten
	}
	return s.forgotten[id(session, path)]
}

// Teach reports whether session has not yet been taught lesson, and marks it taught: a hint a response gives each agent once.
func (s *Store) Teach(session, lesson string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := id(session, lesson)
	if s.taught[key] {
		return false
	}
	s.taught[key] = true
	return true
}

// Taught reports whether session was taught lesson, without teaching it.
func (s *Store) Taught(session, lesson string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taught[id(session, lesson)]
}

// Forget drops every line session saw or knew before moment, and every lesson it was taught, as its harness compacted or cleared the agent's
// context then; a version left with no line seen still restricts edits, so nothing seen before the moment anchors one without being shown
// again. It reports whether moment is later than the last one given for session; an earlier or equal one changes nothing.
func (s *Store) Forget(session string, moment time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := moment.UnixNano()
	if at <= s.cleared[session] {
		return false
	}
	s.cleared[session] = at
	prefix := session + "\x00"
	for element := s.order.Front(); element != nil; element = element.Next() {
		h := element.Value.(*history)
		if !strings.HasPrefix(h.id, prefix) {
			continue
		}
		// New versions rather than changed ones, so a call already holding the old ones reads them unchanged.
		for i, v := range h.versions {
			kept := &Version{View: v.View, at: v.at}
			kept.Seen, kept.seenAt = after(v.Seen, v.seenAt, at)
			kept.Known, kept.knownAt = after(v.Known, v.knownAt, at)
			if kept.Seen == nil {
				kept.Seen, kept.seenAt = map[int]bool{}, map[int]int64{}
			}
			h.versions[i] = kept
		}
	}
	for key := range s.taught {
		if strings.HasPrefix(key, prefix) {
			delete(s.taught, key)
		}
	}
	s.changed(session)
	return true
}

// Held maps each line of current that session saw in full in a version it still holds, and that has not changed since, to its number in
// that version, the newest such version first; a version whose diff to current times out adds none. The seen lines are copied under the
// store's lock and diffed outside it, so it is safe beside concurrent records.
func (s *Store) Held(session, path string, current View) map[int]int {
	type seenVersion struct {
		key   string
		lines []string
		seen  []int
	}
	var versions []seenVersion
	s.mu.Lock()
	if element, ok := s.histories[id(session, path)]; ok {
		for _, v := range element.Value.(*history).versions {
			if len(v.Seen) > 0 {
				versions = append(versions, seenVersion{v.Key, v.Lines, sortedKeys(v.Seen)})
			}
		}
	}
	s.mu.Unlock()
	held := map[int]int{}
	for _, v := range versions {
		if v.key == current.Key {
			for _, line := range v.seen {
				if _, ok := held[line]; !ok && line >= 1 && line <= len(current.Lines) {
					held[line] = line
				}
			}
			continue
		}
		mapping, ok := LineMap(v.lines, current.Lines)
		if !ok {
			continue
		}
		for _, old := range v.seen {
			if now, ok := mapping[old]; ok {
				if _, taken := held[now]; !taken {
					held[now] = old
				}
			}
		}
	}
	return held
}

// stamp adds lines to set, noting now for each line new to it.
func stamp(set map[int]bool, at map[int]int64, lines []int, now int64) (map[int]bool, map[int]int64) {
	if set == nil {
		set, at = map[int]bool{}, map[int]int64{}
	}
	for _, line := range lines {
		if !set[line] {
			set[line], at[line] = true, now
		}
	}
	return set, at
}

// before is the part of set, with its moments, that joined it before moment.
func before(set map[int]bool, at map[int]int64, moment int64) (map[int]bool, map[int]int64) {
	if set == nil {
		return nil, nil
	}
	kept, keptAt := map[int]bool{}, map[int]int64{}
	for line := range set {
		if at[line] < moment {
			kept[line], keptAt[line] = true, at[line]
		}
	}
	return kept, keptAt
}

// after is the part of set, with its moments, that joined it at or after moment.
func after(set map[int]bool, at map[int]int64, moment int64) (map[int]bool, map[int]int64) {
	if set == nil {
		return nil, nil
	}
	kept, keptAt := map[int]bool{}, map[int]int64{}
	for line := range set {
		if at[line] >= moment {
			kept[line], keptAt[line] = true, at[line]
		}
	}
	return kept, keptAt
}

// Copy makes session to's versions copies of session from's as they were just before asOf, each as recently used as its original, replacing
// any to had; the zero asOf copies them as they are. A version the store first held at or after asOf is left out, and so is a path left with
// none. Lines keep the moments they became seen or known, so a later Copy from to filters them alike. Later records to either session never
// reach the other.
func (s *Store) Copy(from, to string, asOf time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if from == to {
		return
	}
	moment := int64(math.MaxInt64)
	if !asOf.IsZero() {
		moment = asOf.UnixNano()
	}
	source, target := from+"\x00", to+"\x00"
	var originals []*list.Element
	for element := s.order.Front(); element != nil; {
		next := element.Next()
		h := element.Value.(*history)
		switch {
		case strings.HasPrefix(h.id, target):
			s.units -= h.units
			s.order.Remove(element)
			delete(s.histories, h.id)
		case strings.HasPrefix(h.id, source):
			originals = append(originals, element)
		}
		element = next
	}
	for _, element := range originals {
		h := element.Value.(*history)
		copied := &history{id: target + h.id[len(source):], units: 1}
		for _, v := range h.versions {
			if v.at >= moment {
				continue
			}
			version := &Version{View: v.View, at: v.at}
			version.Seen, version.seenAt = before(v.Seen, v.seenAt, moment)
			version.Known, version.knownAt = before(v.Known, v.knownAt, moment)
			copied.versions = append(copied.versions, version)
			copied.units += js.Len(v.Key) * 2
		}
		if len(copied.versions) == 0 {
			continue
		}
		s.histories[copied.id] = s.order.InsertAfter(copied, element)
		s.units += copied.units
	}
	s.evict()
}

// Versions are the retained versions, newest first; asking refreshes recency.
func (s *Store) Versions(session, path string) []*Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.histories[id(session, path)]
	if !ok {
		return nil
	}
	s.order.MoveToBack(element)
	return slices.Clone(element.Value.(*history).versions)
}

// Origins are the other paths this session issued tag for, oldest first.
func (s *Store) Origins(session, tagValue, except string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := session + "\x00"
	var found []string
	for element := s.order.Front(); element != nil; element = element.Next() {
		h := element.Value.(*history)
		if !strings.HasPrefix(h.id, prefix) || h.id == prefix+except {
			continue
		}
		for _, v := range h.versions {
			if v.Tag == tagValue {
				found = append(found, h.id[len(prefix):])
				break
			}
		}
	}
	return found
}

// Cleared is the moment session's harness last compacted or cleared its context, as Forget was told it; zero when never.
func (s *Store) Cleared(session string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at := s.cleared[session]; at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}

// Paths lists the paths this session has versions of.
func (s *Store) Paths(session string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := session + "\x00"
	var found []string
	for element := s.order.Front(); element != nil; element = element.Next() {
		if h := element.Value.(*history); strings.HasPrefix(h.id, prefix) {
			found = append(found, h.id[len(prefix):])
		}
	}
	return found
}

// NewView builds the view of an LF-normalized, BOM-free body split into lines.
func NewView(body string, lines []string) View {
	return View{Key: strings.ReplaceAll(body, "\r\n", "\n"), Lines: lines, Tag: tag.Of(body)}
}

// LineChanges is the line diff from previous to current; ok is false when it times out.
func LineChanges(previous, current []string) ([]diff.Change, bool) {
	return diff.Arrays(previous, current, diffTimeout)
}

// LineMap maps 1-based previous line numbers to current ones across unchanged runs; ok is false when the diff times out.
func LineMap(previous, current []string) (map[int]int, bool) {
	changes, ok := LineChanges(previous, current)
	if !ok {
		return nil, false
	}
	mapping := map[int]int{}
	before, after := 1, 1
	for _, change := range changes {
		switch {
		case change.Added:
			after += change.Count
		case change.Removed:
			before += change.Count
		default:
			for offset := 0; offset < change.Count; offset++ {
				mapping[before+offset] = after + offset
			}
			before += change.Count
			after += change.Count
		}
	}
	return mapping, true
}

// AnchorLines are the lines an edit depends on: the whole replaced range, or the insertion neighbour.
func AnchorLines(edit LineEdit) []int {
	if edit.Edge {
		return nil
	}
	if edit.Operation != Replace {
		return []int{edit.Start}
	}
	lines := make([]int, 0, max(0, edit.End-edit.Start+1))
	for line := edit.Start; line <= edit.End; line++ {
		lines = append(lines, line)
	}
	return lines
}

// Unverified are the anchor lines version does not let these edits use, sorted.
func Unverified(version *Version, edits []LineEdit) []int {
	if version.Seen == nil {
		return nil
	}
	lines := map[int]bool{}
	for _, edit := range edits {
		if edit.Edge || edit.Quoted {
			continue
		}
		confirmed := map[int]bool{}
		for _, line := range edit.Confirmed {
			confirmed[line] = true
		}
		if edit.Operation != Replace {
			if !version.Seen[edit.Start] && !confirmed[edit.Start] {
				lines[edit.Start] = true
			}
			continue
		}
		for line := edit.Start; line <= edit.End; line++ {
			if version.Seen[line] {
				continue
			}
			end := line == edit.Start || line == edit.End
			if !version.Known[line] || (end && !confirmed[line]) {
				lines[line] = true
			}
		}
	}
	return sortedKeys(lines)
}

func sortedKeys(set map[int]bool) []int {
	keys := make([]int, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func duplicates(lines []string) map[string]bool {
	seen := map[string]bool{}
	repeated := map[string]bool{}
	for _, line := range lines {
		if seen[line] {
			repeated[line] = true
		}
		seen[line] = true
	}
	return repeated
}

// Port of oh-my-pi validate_context: every anchor needs a neighbour that moved by the same offset.
func contextHolds(previous, current []string, mapping map[int]int, anchors []int) bool {
	previousRepeated := duplicates(previous)
	currentRepeated := duplicates(current)
	set := map[int]bool{}
	for _, a := range anchors {
		set[a] = true
	}
	sorted := sortedKeys(set)
	for index := 0; index < len(sorted); {
		last := index
		for last+1 < len(sorted) && sorted[last+1] == sorted[last]+1 {
			last++
		}
		before, hasBefore := sorted[index]-1, sorted[index] > 1
		after, hasAfter := sorted[last]+1, sorted[last] < len(previous)
		for _, line := range sorted[index : last+1] {
			mapped, ok := mapping[line]
			if !ok {
				return false
			}
			offset := mapped - line
			moved := func(neighbour int, has bool) bool {
				if !has {
					return false
				}
				target, ok := mapping[neighbour]
				return ok && target == neighbour+offset
			}
			if !previousRepeated[previous[line-1]] && !currentRepeated[current[mapped-1]] {
				if !moved(before, hasBefore) && !moved(after, hasAfter) {
					return false
				}
			} else {
				if !hasBefore && !hasAfter {
					return false
				}
				if hasBefore && !moved(before, true) {
					return false
				}
				if hasAfter && !moved(after, true) {
					return false
				}
			}
		}
		index = last + 1
	}
	return true
}

// ResolveEdges pins tail edges to the last line of a file with length lines.
func ResolveEdges(edits []LineEdit, length int) []LineEdit {
	resolved := make([]LineEdit, len(edits))
	for i, edit := range edits {
		if edit.Edge {
			edit.Start, edit.End = length, length
		}
		resolved[i] = edit
	}
	return resolved
}

// RemapEdits rebases edits authored against previous onto current when each edit's anchors moved by one offset of their own, as lines an
// edit in between left unchanged do; moved says whether any did, and ok is false when an anchor changed or its context does not hold.
func RemapEdits(previous *Version, current []string, edits []LineEdit) (moved bool, remapped []LineEdit, ok bool) {
	var anchors []int
	for _, edit := range edits {
		anchors = append(anchors, AnchorLines(edit)...)
	}
	if len(anchors) == 0 {
		return false, ResolveEdges(edits, len(current)), true
	}
	mapping, ok := LineMap(previous.Lines, current)
	if !ok || !contextHolds(previous.Lines, current, mapping, anchors) {
		return false, nil, false
	}
	shifted := make([]LineEdit, len(edits))
	for i, edit := range edits {
		offsets := map[int]bool{}
		for _, line := range AnchorLines(edit) {
			target, ok := mapping[line]
			if !ok {
				return false, nil, false
			}
			offsets[target-line] = true
		}
		// A replaced range must still be one stretch: lines inserted inside it would be replaced too.
		if len(offsets) > 1 {
			return false, nil, false
		}
		for offset := range offsets {
			edit.Start += offset
			edit.End += offset
			moved = moved || offset != 0
		}
		shifted[i] = edit
	}
	return moved, ResolveEdges(shifted, len(current)), true
}

func uniqueEvidence(lines []string, line int) (string, bool) {
	if line < 1 || line > len(lines) {
		return "", false
	}
	content := js.Trim(lines[line-1])
	if content == "" || utf8.RuneCountInString(content) < EvidenceMinRunes {
		return "", false
	}
	count := 0
	for _, other := range lines {
		if js.Trim(other) == content {
			count++
		}
	}
	return content, count == 1
}

func identifier(r rune, ok bool) bool {
	return ok && (unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_')
}

// carries is token-bounded containment: "foo(bar)" is not carried by "foo(bar_baz)".
func carries(line, evidence string) bool {
	first, _ := utf8.DecodeRuneInString(evidence)
	lastRune, _ := utf8.DecodeLastRuneInString(evidence)
	startsWord := identifier(first, evidence != "")
	endsWord := identifier(lastRune, evidence != "")
	for from := 0; ; {
		at := strings.Index(line[from:], evidence)
		if at < 0 {
			return false
		}
		at += from
		before, _ := utf8.DecodeLastRuneInString(line[:at])
		after, _ := utf8.DecodeRuneInString(line[at+len(evidence):])
		if !(startsWord && identifier(before, at > 0)) && !(endsWord && identifier(after, at+len(evidence) < len(line))) {
			return true
		}
		from = at + 1
	}
}

// RescueShifted are the unseen replace anchors that a shift-aware reading accepts (port of oh-my-pi rescue_shifted_anchors).
func RescueShifted(versions []*Version, current View, edits []LineEdit, unseen map[int]bool) map[int]bool {
	var images []func(int) bool
	for _, version := range versions {
		if len(version.Seen) == 0 {
			continue
		}
		v := version
		if v.Key == current.Key {
			images = append(images, func(line int) bool { return v.Seen[line] })
			continue
		}
		forward, _ := LineMap(v.Lines, current.Lines)
		backward := map[int]int{}
		for before, after := range forward {
			backward[after] = before
		}
		images = append(images, func(line int) bool { return v.Seen[backward[line]] })
	}
	rescued := map[int]bool{}
	for _, edit := range edits {
		if edit.Operation != Replace || len(edit.Content) == 0 {
			continue
		}
		var pending []int
		for _, line := range AnchorLines(edit) {
			if unseen[line] {
				pending = append(pending, line)
			}
		}
		if len(pending) == 0 {
			continue
		}
		head, ok := uniqueEvidence(current.Lines, pending[0])
		headAt := -1
		if ok {
			headAt = slices.IndexFunc(edit.Content, func(line string) bool { return carries(line, head) })
		}
		if headAt < 0 {
			continue
		}
		if len(pending) > 1 {
			tail, ok := uniqueEvidence(current.Lines, pending[len(pending)-1])
			if !ok {
				continue
			}
			lastAt := -1
			for i := len(edit.Content) - 1; i >= 0; i-- {
				if carries(edit.Content[i], tail) {
					lastAt = i
					break
				}
			}
			if lastAt < headAt {
				continue
			}
		}
		all := true
		for _, line := range pending {
			if !slices.ContainsFunc(images, func(seen func(int) bool) bool { return seen(line) }) {
				all = false
				break
			}
		}
		if all {
			for _, line := range pending {
				rescued[line] = true
			}
		}
	}
	return rescued
}
