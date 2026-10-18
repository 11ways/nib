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
	for i, agent := range []Agent{{ID: "a1", Spawned: true, Forked: true}, {ID: "a2", Spawned: true, Forked: true, Parent: "a83334bbe36e1e7ac"}, {ID: "a3"}, {ID: "a4", Spawned: true, Forked: true, At: spawned}, {ID: "a5", Spawned: true, Forked: true, Parent: "a1", At: spawned}, {ID: "a6", Spawned: true}, {ID: "a7", Spawned: true, Parent: "a1", At: spawned}, {ID: "a8", Spawned: true, Worktree: "/p/.claude/worktrees/agent-a8 x"}, {ID: "a9", Spawned: true, Parent: "a1", At: spawned, Worktree: "/w"}} {
		toolUse := fmt.Sprintf("toolu_%d", i)
		if err := agents.Record(toolUse, agent, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := agents.Take(toolUse); got != agent {
			t.Fatalf("got %+v, want %+v", got, agent)
		}
	}
	if agents.Record("toolu_x", Agent{ID: "a4", Parent: "a1"}, time.Now()) == nil || agents.Record("toolu_x", Agent{ID: "a4", At: spawned}, time.Now()) == nil || agents.Record("toolu_x", Agent{ID: "a4", Forked: true}, time.Now()) == nil || agents.Record("toolu_x", Agent{ID: "a4", Worktree: "/w"}, time.Now()) == nil {
		t.Fatal("a parent, a moment, a fork or a worktree without a known spawn must be refused")
	}
	for _, worktree := range []string{"w", "/w/../x", "/w/", "/w\nat 5"} {
		if agents.Record("toolu_x", Agent{ID: "a4", Spawned: true, Worktree: worktree}, time.Now()) == nil {
			t.Errorf("worktree %q must be refused", worktree)
		}
	}
	// A parent line nib cannot read leaves the agent its own; a moment or worktree it cannot read leaves it with none.
	for text, want := range map[string]Agent{
		"a5\nfork ../x": {ID: "a5"}, "a5\nspoon a1": {ID: "a5"}, "a5\nfork a1 a2": {ID: "a5"},
		"a5\nfork\nat soon": {ID: "a5", Spawned: true, Forked: true}, "a5\nfork\nat -5": {ID: "a5", Spawned: true, Forked: true}, "a5\nspoon\nat 5": {ID: "a5"},
		"a5\nspawn a1 a2": {ID: "a5"}, "a5\nspawn\nat 5": {ID: "a5", Spawned: true, At: time.Unix(0, 5)},
		"a5\nspawn\nin w": {ID: "a5", Spawned: true}, "a5\nspoon\nin /w": {ID: "a5"}, "a5\nspawn\nin /w": {ID: "a5", Spawned: true, Worktree: "/w"},
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

func TestContextsKeepTheNewestMomentsAndDropsAreTakenOnce(t *testing.T) {
	state := private(t)
	contexts := OpenContexts(state)
	if _, filed := contexts.Of(SessionKey("6d0b")); filed {
		t.Fatal("no file before the first drop")
	}
	now := time.Now()
	for i := range contextMoments + 3 {
		if err := contexts.Drop(SessionKey("6d0b"), time.Unix(int64(100+i), 0), now); err != nil {
			t.Fatal(err)
		}
	}
	moments, filed := contexts.Of(SessionKey("6d0b"))
	if !filed || len(moments) != contextMoments || !moments[0].Equal(time.Unix(103, 0)) {
		t.Fatalf("got %d moments from %v, filed %v", len(moments), moments[0], filed)
	}
	if got := Latest(moments, time.Unix(110, 0)); !got.Equal(time.Unix(109, 0)) {
		t.Fatalf("latest before 110: %v", got)
	}
	if got := Latest(moments, time.Time{}); !got.Equal(time.Unix(int64(100+contextMoments+2), 0)) {
		t.Fatalf("latest: %v", got)
	}
	if got := Latest(moments, time.Unix(50, 0)); !got.IsZero() {
		t.Fatalf("none before 50: %v", got)
	}
	drops := OpenDrops(state)
	if err := drops.Note("toolu_a", time.Unix(0, 1234), "6d0b-session", now); err != nil {
		t.Fatal(err)
	}
	if err := drops.Note("toolu_b", time.Time{}, "", now); err != nil {
		t.Fatal(err)
	}
	if got, known, session := drops.Take("toolu_a"); !known || !got.Equal(time.Unix(0, 1234)) || session != "6d0b-session" {
		t.Fatalf("a: %v %v %q", got, known, session)
	}
	if got, known, session := drops.Take("toolu_b"); !known || !got.IsZero() || session != "" {
		t.Fatalf("b: %v %v %q", got, known, session)
	}
	if _, known, _ := drops.Take("toolu_a"); known {
		t.Fatal("a drop record is taken once")
	}
	if err := drops.Note("../x", time.Time{}, "", now); err == nil {
		t.Fatal("only Claude Code ids name records")
	}
	// A session that is no id is left out, never written into the record.
	if err := drops.Note("toolu_c", time.Time{}, "../x\nsession y", now); err != nil {
		t.Fatal(err)
	}
	if _, known, session := drops.Take("toolu_c"); !known || session != "" {
		t.Fatalf("c: %v %q", known, session)
	}
}
