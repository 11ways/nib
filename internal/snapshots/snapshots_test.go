package snapshots

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/11ways/nib/internal/tag"
)

func view(key string) View { return NewView(key, []string{key}) }

func seen(v *Version) []int {
	lines := make([]int, 0, len(v.Seen))
	for line := range v.Seen {
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}

func TestStoreDeduplicatesUnionsAndEvictsLeastRecentlyUsed(t *testing.T) {
	store := NewStoreWith(Limits{Paths: 2, Versions: 2, Units: 1 << 30})
	store.Record("s", "a", view("one"), []int{1}, nil)
	store.Record("s", "a", view("two"), []int{2}, nil)
	store.Record("s", "a", view("one"), []int{3}, nil)
	versions := store.Versions("s", "a")
	if versions[0].Key != "one" || !reflect.DeepEqual(seen(versions[0]), []int{1, 3}) || versions[1].Key != "two" || !reflect.DeepEqual(seen(versions[1]), []int{2}) {
		t.Fatalf("versions = %+v", versions)
	}
	store.Record("s", "a", view("three"), nil, nil)
	if keys := []string{store.Versions("s", "a")[0].Key, store.Versions("s", "a")[1].Key}; !reflect.DeepEqual(keys, []string{"three", "one"}) {
		t.Fatalf("keys = %v", keys)
	}
	store.Record("s", "b", view("b"), nil, nil)
	store.Versions("s", "a")
	store.Record("other", "a", view("c"), nil, nil)
	if got := store.Versions("s", "b"); len(got) != 0 {
		t.Fatalf("b should be evicted, got %d versions", len(got))
	}
	if got := store.Versions("s", "a"); len(got) != 2 {
		t.Fatalf("a should stay, got %d versions", len(got))
	}
	if got := store.Origins("s", tag.Of("three"), "b"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("origins = %v", got)
	}
}

func TestSeenAsUnionsTheLinesSeenOfTheSameTextUnderOtherPaths(t *testing.T) {
	store := NewStore()
	text := NewView("a\nb\nc\nd", []string{"a", "b", "c", "d"})
	store.Record("s", "main", text, []int{1}, nil)
	store.Record("s", "lane", text, []int{2}, nil)
	store.Record("s", "other", NewView("x\ny\nz\nw", []string{"x", "y", "z", "w"}), []int{3}, nil)
	store.Record("fork", "copy", text, []int{4}, nil)
	if got := store.SeenAs("s", text.Key, "lane"); !reflect.DeepEqual(got, map[int]bool{1: true}) {
		t.Fatalf("seen = %v", got)
	}
	// A version that lets edits anchor anywhere counts all its lines.
	store.Record("s", "open", text, nil, nil)
	if got := store.SeenAs("s", text.Key, "lane"); len(got) != 4 {
		t.Fatalf("seen = %v", got)
	}
}

func TestATextManyAgentsHoldCountsOnce(t *testing.T) {
	store := NewStoreWith(Limits{Paths: 8, Versions: 2, Units: 1 << 30})
	text := "the same file\n"
	for _, session := range []string{"main", "builder", "reviewer"} {
		store.Record(session, "a", view(text), []int{1}, nil)
	}
	// Three histories cost one unit each, and their one text once.
	if want := 3 + 2*len(text); store.units != want || len(store.texts) != 1 || store.texts[text].refs != 3 {
		t.Fatalf("units %d, want %d; texts %d", store.units, want, len(store.texts))
	}
	store.Record("main", "a", view("changed\n"), []int{1}, nil)
	store.Forget("builder", time.Now())
	if store.texts[text].refs != 3 {
		t.Fatalf("refs %d", store.texts[text].refs)
	}
}

func TestAFileTheAgentWroteOutlastsOneRoundOfEviction(t *testing.T) {
	store := NewStoreWith(Limits{Paths: 2, Versions: 1, Units: 1 << 30})
	store.Record("main", "AGENTS.md", view("rules"), []int{1}, []int{1})
	store.Record("builder", "x.go", view("x"), []int{1}, nil)
	store.Record("builder", "y.go", view("y"), []int{1}, nil)
	if len(store.Versions("main", "AGENTS.md")) != 1 || len(store.Versions("builder", "x.go")) != 0 {
		t.Fatal("the written file went before one only read")
	}
	// The second round takes it like any other.
	store.Record("builder", "z.go", view("z"), []int{1}, nil)
	store.Record("builder", "w.go", view("w"), []int{1}, nil)
	if len(store.Versions("main", "AGENTS.md")) != 0 {
		t.Fatal("the written file never goes")
	}
}

func TestAnEvictedHistoryIsRememberedAsForgotten(t *testing.T) {
	store := NewStoreWith(Limits{Paths: 2, Versions: 2, Units: 1 << 30})
	store.Record("a", "/f", view("x"), []int{1}, nil)
	if store.Forgotten("a", "/f") {
		t.Fatal("a history never evicted is not forgotten")
	}
	store.Record("b", "/f", view("x"), []int{1}, nil)
	store.Record("b", "/g", view("y"), []int{1}, nil)
	if !store.Forgotten("a", "/f") || store.Forgotten("b", "/f") {
		t.Fatal("only the evicted history is forgotten")
	}
	// A new history for it stays marked, as the lines seen before it are lost; it only restricts by what was seen since.
	version := store.Record("a", "/f", view("x"), []int{2}, nil)
	if !store.Forgotten("a", "/f") || !reflect.DeepEqual(seen(version), []int{2}) {
		t.Fatalf("got %v %v", store.Forgotten("a", "/f"), seen(version))
	}
}

func TestEmptySeenRestrictsAndNilLeavesUnrestricted(t *testing.T) {
	store := NewStore()
	if v := store.Record("s", "a", view("x"), nil, nil); v.Seen != nil {
		t.Fatal("a nil seen list must leave the version unrestricted")
	}
	if v := store.Record("s", "a", view("x"), []int{}, nil); v.Seen == nil || len(v.Seen) != 0 {
		t.Fatal("an empty seen list must restrict the version to no lines")
	}
}

func TestCopyGivesAnotherSessionItsOwnCopy(t *testing.T) {
	store := NewStoreWith(Limits{Paths: 8, Versions: 2, Units: 1 << 30})
	store.Record("parent", "a", view("one"), []int{1}, []int{9})
	store.Record("parent", "a", view("two"), []int{2}, nil)
	store.Record("other", "c", view("c"), []int{1}, nil)
	store.Record("parent", "b", view("b"), nil, nil)
	store.Record("fork", "stale", view("stale"), []int{1}, nil)
	store.Copy("parent", "fork", time.Time{})
	if got := store.Paths("fork"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("the copy must replace what the fork had and keep the parent's order, got %v", got)
	}
	versions := store.Versions("fork", "a")
	if len(versions) != 2 || versions[0].Key != "two" || !reflect.DeepEqual(seen(versions[0]), []int{2}) || !reflect.DeepEqual(seen(versions[1]), []int{1}) || !versions[1].Known[9] {
		t.Fatalf("versions = %+v", versions)
	}
	if b := store.Versions("fork", "b"); len(b) != 1 || b[0].Seen != nil {
		t.Fatalf("an unrestricted version must stay unrestricted, got %+v", b)
	}
	store.Record("fork", "a", view("two"), []int{3}, nil)
	store.Record("parent", "a", view("two"), []int{4}, nil)
	if got := seen(store.Versions("parent", "a")[0]); !reflect.DeepEqual(got, []int{2, 4}) {
		t.Fatalf("the fork's record must not reach the parent, got %v", got)
	}
	if got := seen(store.Versions("fork", "a")[0]); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("the parent's record must not reach the fork, got %v", got)
	}
	if got := store.Paths("other"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("other sessions must stay, got %v", got)
	}
}

func TestCopyEvictsByRecency(t *testing.T) {
	store := NewStoreWith(Limits{Paths: 3, Versions: 1, Units: 1 << 30})
	store.Record("parent", "old", view("old"), []int{1}, nil)
	store.Record("other", "x", view("x"), []int{1}, nil)
	store.Copy("parent", "fork", time.Time{})
	// The copy of old is as old as old itself, so it goes right after old and before other's x.
	store.Record("other", "y", view("y"), []int{1}, nil)
	if store.Paths("parent") != nil || store.Paths("fork") == nil {
		t.Fatalf("parent %v fork %v other %v", store.Paths("parent"), store.Paths("fork"), store.Paths("other"))
	}
	store.Record("other", "z", view("z"), []int{1}, nil)
	if got := store.Paths("other"); store.Paths("fork") != nil || !reflect.DeepEqual(got, []string{"x", "y", "z"}) {
		t.Fatalf("fork %v other %v", store.Paths("fork"), got)
	}
}

func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	store := NewStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, path := fmt.Sprintf("s%d", i%2), fmt.Sprintf("p%d", i)
			for j := range 50 {
				store.Record(session, path, view(fmt.Sprint(j%3)), []int{j}, nil)
				store.Versions(session, path)
				store.Origins(session, "0000", path)
				store.Copy(session, fmt.Sprintf("fork%d", i), time.Unix(0, int64(j)))
			}
		}()
	}
	wg.Wait()
}

func TestAnchorLinesOfAReversedRangeIsEmpty(t *testing.T) {
	if got := AnchorLines(LineEdit{Start: 3, End: 1}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// clock is a store clock that reads the moment each test sets.
type clock struct{ now time.Time }

func (c *clock) read() time.Time { return c.now }

func TestCopyAsOfTakesOnlyWhatWasSeenBefore(t *testing.T) {
	at := &clock{time.Unix(100, 0)}
	store := NewStoreWith(Limits{Paths: 8, Versions: 4, Units: 1 << 30})
	store.Now = at.read
	store.Record("parent", "a", view("one"), []int{1, 2}, []int{9})
	store.Record("parent", "unrestricted", view("u"), nil, nil)
	at.now = time.Unix(200, 0)
	// After the moment: more of the same version, a line made known, a newer version and a new path, each with a moment of its own.
	store.Record("parent", "a", view("one"), []int{2, 5}, []int{8})
	store.Record("parent", "a", view("two"), []int{1}, nil)
	store.Record("parent", "late", view("late"), []int{1}, nil)
	store.Copy("parent", "fork", time.Unix(150, 0))
	if got := store.Paths("fork"); !reflect.DeepEqual(got, []string{"unrestricted", "a"}) {
		t.Fatalf("a path first seen after the moment must be left out, got %v", got)
	}
	versions := store.Versions("fork", "a")
	if len(versions) != 1 || versions[0].Key != "one" || !reflect.DeepEqual(seen(versions[0]), []int{1, 2}) || !versions[0].Known[9] || versions[0].Known[8] {
		t.Fatalf("the fork must hold version one as it was before the moment, got %+v", versions)
	}
	if u := store.Versions("fork", "unrestricted"); len(u) != 1 || u[0].Seen != nil {
		t.Fatalf("an unrestricted version from before the moment stays unrestricted, got %+v", u)
	}
	// Lines keep their moments in the copy, so a fork of the fork is filtered alike; the zero moment copies everything.
	at.now = time.Unix(300, 0)
	store.Record("fork", "a", view("one"), []int{3}, nil)
	store.Copy("fork", "nested", time.Unix(250, 0))
	if got := seen(store.Versions("nested", "a")[0]); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("the nested fork must take only what its parent had seen before its moment, got %v", got)
	}
	store.Copy("parent", "all", time.Time{})
	if got := store.Versions("all", "a"); len(got) != 2 || !reflect.DeepEqual(seen(got[1]), []int{1, 2, 5}) {
		t.Fatalf("the zero moment must copy everything, got %+v", got)
	}
}

func lines(text ...string) View {
	key := ""
	for i, line := range text {
		if i > 0 {
			key += "\n"
		}
		key += line
	}
	return NewView(key, text)
}

func TestHeldFollowsSeenLinesAcrossVersionsUnchanged(t *testing.T) {
	store := NewStore()
	old := lines("a", "b", "c", "d")
	store.Record("s", "/f", old, []int{1, 2, 3}, nil)
	if got := store.Held("s", "/f", old); !reflect.DeepEqual(got, map[int]int{1: 1, 2: 2, 3: 3}) {
		t.Fatalf("same version: %v", got)
	}
	// A line put above moves the rest down; the changed line c was never held there, and d was never seen.
	current := lines("new", "a", "b", "C", "d")
	store.Record("s", "/f", current, []int{1}, nil)
	if got := store.Held("s", "/f", current); !reflect.DeepEqual(got, map[int]int{1: 1, 2: 1, 3: 2}) {
		t.Fatalf("after the edit: %v", got)
	}
	if got := store.Held("other", "/f", current); len(got) != 0 {
		t.Fatalf("another agent holds %v", got)
	}
}

func TestHeldMapsLinesKnownByContentOnlyToZeroAfterSeenOnes(t *testing.T) {
	store := NewStore()
	old := lines("a", "b", "c", "d")
	// A plain read showed b, c and d whole; a numbered one showed a and b.
	store.Record("s", "/f", old, nil, []int{2, 3, 4})
	store.Record("s", "/f", old, []int{1, 2}, nil)
	if got := store.Held("s", "/f", old); !reflect.DeepEqual(got, map[int]int{1: 1, 2: 2, 3: 0, 4: 0}) {
		t.Fatalf("same version: %v", got)
	}
	// After a line is put above, what was seen with numbers still maps to those, and what was known by content stays known.
	current := lines("new", "a", "b", "c", "D")
	if got := store.Held("s", "/f", current); !reflect.DeepEqual(got, map[int]int{2: 1, 3: 2, 4: 0}) {
		t.Fatalf("after the edit: %v", got)
	}
}

func TestSeenMomentsAreThoseOfTheVersionHeld(t *testing.T) {
	store := NewStore()
	now := int64(0)
	store.Now = func() time.Time { now++; return time.Unix(0, now) }
	v := lines("a", "b", "c")
	store.Record("s", "/f", v, []int{1, 2}, nil)
	store.Record("s", "/f", v, []int{3}, nil)
	if got, ok := store.SeenMoments("s", "/f", v, []int{1, 2, 3}); !ok || !reflect.DeepEqual(got, map[int]int64{1: 1, 2: 1, 3: 2}) {
		t.Fatalf("lines 1-3: %v %v", got, ok)
	}
	if _, ok := store.SeenMoments("s", "/f", v, []int{1, 4}); ok {
		t.Fatal("a line never seen has a moment")
	}
	if _, ok := store.SeenMoments("s", "/f", lines("a", "b", "x"), []int{1}); ok {
		t.Fatal("a version not held has moments")
	}
	if _, ok := store.SeenMoments("s", "/f", v, nil); ok {
		t.Fatal("no lines have a moment")
	}
}

func TestForgetDropsWhatWasSeenBeforeItsMomentOnce(t *testing.T) {
	clock := time.Unix(100, 0)
	store := NewStore()
	store.Now = func() time.Time { return clock }
	file := lines("a", "b", "c")
	store.Record("s", "/f", file, []int{1, 2}, []int{3})
	store.Teach("s", "held")
	clock = time.Unix(200, 0)
	store.Record("s", "/f", file, []int{3}, nil)
	store.Record("t", "/f", file, []int{1}, nil)
	if !store.Forget("s", time.Unix(150, 0)) {
		t.Fatal("a first drop applies")
	}
	version := store.Versions("s", "/f")[0]
	if !reflect.DeepEqual(seen(version), []int{3}) || len(version.Known) != 0 {
		t.Fatalf("seen %v known %v", seen(version), version.Known)
	}
	if !store.Teach("s", "held") {
		t.Fatal("lessons are taught again after a drop")
	}
	if got := seen(store.Versions("t", "/f")[0]); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("another agent lost %v", got)
	}
	if store.Forget("s", time.Unix(150, 0)) || store.Forget("s", time.Unix(120, 0)) {
		t.Fatal("a drop told again, or an older one, changes nothing")
	}
	// Nothing seen is a restriction, not a version that lets edits anchor anywhere.
	store.Forget("s", time.Unix(300, 0))
	if version := store.Versions("s", "/f")[0]; version.Seen == nil || len(version.Seen) != 0 {
		t.Fatalf("seen after a later drop: %v", version.Seen)
	}
}
