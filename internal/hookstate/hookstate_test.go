package hookstate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func private(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestARecordIsTakenOnce(t *testing.T) {
	agents := OpenAgents(private(t))
	if err := agents.Record("toolu_016yQmgm", Agent{ID: "a1d576263ade979e4"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := agents.Take("toolu_016yQmgm"); got != (Agent{ID: "a1d576263ade979e4"}) {
		t.Fatalf("got %+v", got)
	}
	if got := agents.Take("toolu_016yQmgm"); got.ID != "" {
		t.Fatalf("a record must be taken only once, got %+v", got)
	}
	if got := agents.Take("toolu_other"); got.ID != "" {
		t.Fatalf("a call without a record is the main agent's, got %+v", got)
	}
	info, err := os.Stat(agents.directory)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the records folder must be private, got %v, %v", info, err)
	}
}

func TestARecordNamesTheParentAndMoment(t *testing.T) {
	agents := OpenAgents(private(t))
	spawned := time.Unix(0, 1792097515642000000)
	for i, agent := range []Agent{{ID: "a1", Spawned: true, Forked: true}, {ID: "a2", Spawned: true, Forked: true, Parent: "a83334bbe36e1e7ac"}, {ID: "a3"}, {ID: "a4", Spawned: true, Forked: true, At: spawned}, {ID: "a5", Spawned: true, Forked: true, Parent: "a1", At: spawned}, {ID: "a6", Spawned: true}, {ID: "a7", Spawned: true, Parent: "a1", At: spawned}} {
		toolUse := fmt.Sprintf("toolu_%d", i)
		if err := agents.Record(toolUse, agent, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := agents.Take(toolUse); got != agent {
			t.Fatalf("got %+v, want %+v", got, agent)
		}
	}
	if agents.Record("toolu_x", Agent{ID: "a4", Parent: "a1"}, time.Now()) == nil || agents.Record("toolu_x", Agent{ID: "a4", At: spawned}, time.Now()) == nil || agents.Record("toolu_x", Agent{ID: "a4", Forked: true}, time.Now()) == nil {
		t.Fatal("a parent, a moment or a fork without a known spawn must be refused")
	}
	// A parent line nib cannot read leaves the agent its own; a moment it cannot read leaves it with no moment.
	for text, want := range map[string]Agent{
		"a5\nfork ../x": {ID: "a5"}, "a5\nspoon a1": {ID: "a5"}, "a5\nfork a1 a2": {ID: "a5"},
		"a5\nfork\nat soon": {ID: "a5", Spawned: true, Forked: true}, "a5\nfork\nat -5": {ID: "a5", Spawned: true, Forked: true}, "a5\nspoon\nat 5": {ID: "a5"},
		"a5\nspawn a1 a2": {ID: "a5"}, "a5\nspawn\nat 5": {ID: "a5", Spawned: true, At: time.Unix(0, 5)},
	} {
		if err := os.WriteFile(filepath.Join(agents.directory, "toolu_bad"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := agents.Take("toolu_bad"); got != want {
			t.Fatalf("%q: got %+v", text, got)
		}
	}
}

func TestSpawnsKeepAForksMomentAndWhenItIsUnknown(t *testing.T) {
	spawns := OpenSpawns(private(t))
	if _, filed := spawns.Of("a1"); filed {
		t.Fatal("nothing is filed yet")
	}
	spawned := time.Unix(0, 1792097515642000000)
	if err := spawns.Note("a1", spawned, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := spawns.Note("a2", time.Time{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if at, filed := spawns.Of("a1"); !filed || !at.Equal(spawned) {
		t.Fatalf("got %v %v", at, filed)
	}
	if at, filed := spawns.Of("a2"); !filed || !at.IsZero() {
		t.Fatalf("an unknown moment must be filed as unknown, got %v %v", at, filed)
	}
	if spawns.Note("../a3", spawned, time.Now()) == nil {
		t.Fatal("an id that could name a path must be refused")
	}
	if _, filed := spawns.Of("a1"); !filed {
		t.Fatal("reading a moment must not take it")
	}
}

func TestIdsThatCouldNamePathsAreRefused(t *testing.T) {
	state := private(t)
	agents := OpenAgents(state)
	for _, id := range []string{"", "../escape", "a/b", ".", "x y"} {
		if agents.Record(id, Agent{ID: "agent"}, time.Now()) == nil {
			t.Errorf("tool_use id %q must be refused", id)
		}
		if agents.Record("toolu_1", Agent{ID: id}, time.Now()) == nil {
			t.Errorf("agent id %q must be refused", id)
		}
		if id != "" && agents.Record("toolu_1", Agent{ID: "a1", Spawned: true, Parent: id}, time.Now()) == nil {
			t.Errorf("parent id %q must be refused", id)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state), "escape")); err == nil {
		t.Fatal("a record escaped its folder")
	}
	if got := agents.Take("../state"); got.ID != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestStaleRecordsArePruned(t *testing.T) {
	agents := OpenAgents(private(t))
	now := time.Now()
	if err := agents.Record("toolu_old", Agent{ID: "a1"}, now); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-RecordAge - time.Minute)
	if err := os.Chtimes(filepath.Join(agents.directory, "toolu_old"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := agents.Record("toolu_young", Agent{ID: "a2"}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := agents.Record("toolu_new", Agent{ID: "a3"}, now); err != nil {
		t.Fatal(err)
	}
	if got := agents.Take("toolu_old"); got.ID != "" {
		t.Fatalf("a record past RecordAge must be pruned, got %+v", got)
	}
	if got := agents.Take("toolu_young"); got.ID != "a2" {
		t.Fatalf("a record within RecordAge must stay, got %+v", got)
	}
}

func TestFoldersOthersCanEnterAndLinksAreNotTrusted(t *testing.T) {
	open := filepath.Join(t.TempDir(), "open")
	if err := os.MkdirAll(filepath.Join(open, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(open, "agents", "toolu_1"), []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	agents := OpenAgents(open)
	if agents.Record("toolu_2", Agent{ID: "a1"}, time.Now()) == nil {
		t.Fatal("a folder others can enter must not be written")
	}
	if got := agents.Take("toolu_1"); got.ID != "" {
		t.Fatalf("a record in a folder others can enter must not be trusted, got %+v", got)
	}
	state := private(t)
	linked := OpenAgents(state)
	if err := linked.Record("toolu_3", Agent{ID: "a1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("a9"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked.directory, "toolu_4")); err != nil {
		t.Fatal(err)
	}
	if got := linked.Take("toolu_4"); got.ID != "" {
		t.Fatalf("a record that is a link must not be followed, got %+v", got)
	}
}
