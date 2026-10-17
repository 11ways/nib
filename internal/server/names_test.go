package server

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/names"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestADirectoryAgentsKeepReadingInGetsANameTheyAreToldOf(t *testing.T) {
	directory := resolvedPath(t, t.TempDir())
	deep := filepath.Join(directory, "zenit", "src", "server", "java", "be", "elevenways", "widgets")
	for i := range 8 {
		file(t, filepath.Join(deep, fmt.Sprintf("W%d.java", i)), "class W {}\n")
	}
	learned := filepath.Join(t.TempDir(), "learned.json")
	c := connect(t, Options{Directory: directory, Names: &names.Table{Base: directory}, NamesFile: learned}, nil, nil, false)
	var last string
	for i := range 6 {
		last = text(c.call(t, readCall(filepath.Join(deep, fmt.Sprintf("W%d.java", i)))))
		if i < 5 && strings.Contains(last, "nib named") {
			t.Fatalf("named after %d reads: %s", i+1, last)
		}
	}
	// Six shown paths save more than the note that tells of the name costs; the name takes effect from the next call.
	if !strings.HasPrefix(last, "[zenit/src/server/java/be/elevenways/widgets/W5.java#") || !strings.HasSuffix(last, "\n\n[nib named ~bewidgets = zenit/src/server/java/be/elevenways/widgets, which 6 of the paths shown to you ran through; ~bewidgets/ works in every tool and in nibash, for you and the subagents you start]") {
		t.Fatalf("got:\n%s", last)
	}
	// The name works as input and in output, and the note is told once.
	again := text(c.call(t, readCall("~bewidgets/W0.java")))
	if !strings.HasPrefix(again, "[~bewidgets/W0.java#") || strings.Contains(again, "nib named") {
		t.Fatalf("got:\n%s", again)
	}
	// The next server of this directory has the name from the start, in its instructions.
	table := &names.Table{Base: directory}
	table.AddLearned(learned)
	if got := Instructions(Options{Names: table}); !strings.Contains(got, "\n~bewidgets = zenit/src/server/java/be/elevenways/widgets") {
		t.Fatalf("instructions: %s", got)
	}
}

func TestNamesMadeWhileAListingPrintsApplyFromTheNextCall(t *testing.T) {
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
	listing, notes, _ := strings.Cut(first, "\n\n[nib named ")
	// Every row of the listing prints the same way, so each folder is named once, its whole count with it.
	if strings.Contains(listing, "~") || strings.Count(listing, "sub/") != 1 || notes == "" {
		t.Fatalf("got:\n%s", first)
	}
	again := text(c.call(t, find))
	if !strings.Contains(again, "~bewidgets/") || strings.Contains(again, "zenit/src") {
		t.Fatalf("got:\n%s", again)
	}
}
