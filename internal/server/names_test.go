package server

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestADirectoryAgentsKeepReadingInGetsANameTheyAreToldOf(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	deep := filepath.Join(directory, "zenit", "src", "server", "java", "be", "elevenways", "widgets")
	for i := range 12 {
		file(t, filepath.Join(deep, fmt.Sprintf("W%d.java", i)), "class W {}\n")
	}
	learned := filepath.Join(t.TempDir(), "learned.json")
	c := connect(t, Options{Directory: directory, Names: &names.Table{Base: directory}, NamesFile: learned}, nil, nil, false)
	last, named := "", 0
	for named < 12 && !strings.Contains(last, "nib named") {
		last = text(c.call(t, readCall(filepath.Join(deep, fmt.Sprintf("W%d.java", named)))))
		named++
	}
	if named < 3 {
		t.Fatalf("named after %d reads: %s", named, last)
	}
	// The note comes first and names the directory whole; the read that earned the name prints under it already.
	want := fmt.Sprintf("[nib named ~bewidgets = %s, which %d of the paths shown to you ran through; ~bewidgets/ works in every tool and in nibash, for you and the subagents you start]\n\n[~bewidgets/W%d.java#", filepath.ToSlash(deep), named, named-1)
	if !strings.HasPrefix(last, want) {
		t.Fatalf("got:\n%s", last)
	}
	// The name works as input and in output, and the note is told once.
	again := text(c.call(t, readCall("~bewidgets/W0.java")))
	if !strings.HasPrefix(again, "[~bewidgets/W0.java#") || strings.Contains(again, "nib named") {
		t.Fatalf("got:\n%s", again)
	}
	// Once a second server of this directory made it too, the next one has the name from the start, in its instructions.
	second := connect(t, Options{Directory: directory, Names: &names.Table{Base: directory}, NamesFile: learned}, nil, nil, false)
	for i := range 12 {
		second.call(t, readCall(filepath.Join(deep, fmt.Sprintf("W%d.java", i))))
	}
	table := &names.Table{Base: directory}
	table.AddLearned(learned)
	if got := Instructions(Options{Names: table}); !strings.Contains(got, "\n~bewidgets = zenit/src/server/java/be/elevenways/widgets") {
		t.Fatalf("instructions: %s", got)
	}
}

func TestANameMadeWhileAListingPrintsHoldsForTheWholeListing(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	deep := filepath.Join(directory, "zenit", "src", "server", "java", "be", "elevenways", "widgets")
	for _, name := range []string{"a.txt", "b.txt", "z.txt"} {
		file(t, filepath.Join(deep, name), "x\n")
	}
	for i := range 8 {
		file(t, filepath.Join(deep, "sub", fmt.Sprintf("s%d.txt", i)), "x\n")
	}
	c := connect(t, Options{Directory: directory, Names: &names.Table{Base: directory}, NamesFile: filepath.Join(t.TempDir(), "learned.json")}, nil, nil, false)
	find := &mcp.CallToolParams{Name: "find", Arguments: map[string]any{"paths": []string{"zenit"}}}
	first := text(c.call(t, find))
	notes, listing, _ := strings.Cut(first, "\n\n")
	// The note comes first, and every row of the listing prints under the name.
	if !strings.HasPrefix(notes, "[nib named ~bewidgets = "+filepath.ToSlash(deep)+", ") || !strings.HasPrefix(listing, "11 files under ~bewidgets/") || strings.Contains(listing, "zenit/src") || strings.Count(listing, "sub/") != 1 {
		t.Fatalf("got:\n%s", first)
	}
	again := text(c.call(t, find))
	if !strings.Contains(again, "~bewidgets/") || strings.Contains(again, "zenit/src") || strings.Contains(again, "nib named") {
		t.Fatalf("got:\n%s", again)
	}
}

func TestANameMadeWhileAGrepPrintsHoldsForEveryFileItShows(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	deep := filepath.Join(directory, "zenit", "src", "server", "java", "be", "elevenways", "widgets")
	for i := range 12 {
		file(t, filepath.Join(deep, fmt.Sprintf("W%02d.java", i)), "class W {}\n")
	}
	c := connect(t, Options{Directory: directory, Names: &names.Table{Base: directory}, NamesFile: filepath.Join(t.TempDir(), "learned.json")}, nil, nil, false)
	got := text(c.call(t, &mcp.CallToolParams{Name: "grep", Arguments: map[string]any{"pattern": "class", "paths": []string{"zenit"}}}))
	notes, matches, _ := strings.Cut(got, "\n\n")
	if !strings.HasPrefix(notes, "[nib named ~bewidgets = "+filepath.ToSlash(deep)+", ") || strings.Count(matches, "[~bewidgets/W") != 12 || strings.Contains(matches, "zenit/src") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestNamesAreToldAgainAfterAnAgentsContextDrops(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	deep := filepath.Join(directory, "zenit", "src", "server", "java", "be", "elevenways", "widgets")
	for i := range 12 {
		file(t, filepath.Join(deep, fmt.Sprintf("W%d.java", i)), "class W {}\n")
	}
	r := &records{byUse: map[string]hookstate.Agent{}}
	d := &drops{known: true}
	c := connect(t, Options{Directory: directory, Agent: r.take, Dropped: d.take, Names: &names.Table{Base: directory}, NamesFile: filepath.Join(t.TempDir(), "learned.json")}, []string{directory}, nil, false)
	read := func(agent *hookstate.Agent, path string) string {
		if agent == nil {
			return text(c.call(t, r.as("", readCall(path))))
		}
		return text(c.call(t, r.by(*agent, readCall(path))))
	}
	for i := range 12 {
		read(nil, filepath.Join(deep, fmt.Sprintf("W%d.java", i)))
	}
	if got := read(nil, "~bewidgets/W6.java"); strings.Contains(got, "~bewidgets =") {
		t.Fatalf("a name told once is not told again before a drop: %s", got)
	}
	d.mu.Lock()
	d.dropped = time.Now()
	d.mu.Unlock()
	// The note went with the rest of the context, so the next path printed under the name tells of it again, in full; then not again.
	if got := read(nil, "~bewidgets/W7.java"); !strings.HasPrefix(got, "[~bewidgets = "+filepath.ToSlash(deep)+", a name you got before your context was compacted or cleared; ~bewidgets/ works in every tool and in nibash, for you and the subagents you start]\n\n") {
		t.Fatalf("after the drop: %s", got)
	}
	if got := read(nil, "~bewidgets/W0.java"); strings.Contains(got, "~bewidgets =") {
		t.Fatalf("told again once: %s", got)
	}
	// A subagent is greeted with its names on its first call, and again on its first call after its context drops, which tells them all.
	sub := hookstate.Agent{ID: "s1", Spawned: true, At: time.Now()}
	if got := read(&sub, "~bewidgets/W1.java"); !strings.Contains(got, "~bewidgets = zenit/src/server/java/be/elevenways/widgets\n") {
		t.Fatalf("a subagent's greeting: %s", got)
	}
	if got := read(&sub, "~bewidgets/W2.java"); strings.Contains(got, "~bewidgets =") {
		t.Fatalf("greeted once: %s", got)
	}
	d.mu.Lock()
	d.dropped = time.Now()
	d.mu.Unlock()
	got := read(&sub, "~bewidgets/W3.java")
	if strings.Count(got, "~bewidgets = ") != 1 || !strings.Contains(got, "~bewidgets = zenit/src/server/java/be/elevenways/widgets\n") {
		t.Fatalf("a subagent after its drop: %s", got)
	}
}
