package nibash

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/names"
	"github.com/11ways/nib/internal/server"
)

func TestHashDefinesListsAndRefusesNamedDirectories(t *testing.T) {
	directory := agentDirectory(t)
	for _, folder := range []string{"sub/inner", "static"} {
		if err := os.MkdirAll(filepath.Join(directory, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "sub", "inner", "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	table := &names.Table{Entries: []names.Entry{{Name: "fixed", Directory: filepath.Join(directory, "static")}}, Base: directory}
	run := connectNamed(t, directory, table, func(string) hookstate.Agent { return hookstate.Agent{} })
	sub := filepath.Join(directory, "sub")
	// The path is taken from the shell's directory, and may start with a name already defined.
	expect(t, run("toolu_1", "cd sub && hash -d api=. && cd .."), "[~api now names "+sub+"]")
	expect(t, run("toolu_2", "hash -d deep=~api/inner"), "[~deep now names "+filepath.Join(sub, "inner")+"]")
	expect(t, run("toolu_3", "hash -d"), "api="+sub+"\ndeep="+filepath.Join(sub, "inner")+"\nfixed="+filepath.Join(directory, "static"))
	expect(t, run("toolu_4", "echo ~api/x ~deep '~api/x'"), sub+"/x "+filepath.Join(sub, "inner")+" ~api/x")
	// nib's own commands print paths under the caller's names, the deepest winning.
	if got := run("toolu_5", "cat sub/inner/f.txt"); !strings.HasPrefix(got, "[~deep/f.txt#") {
		t.Fatalf("a path under a defined name must display with it, got %q", got)
	}
	for command, reason := range map[string]string{
		"hash -d fixed=sub":            "one of nib's own named directories",
		"hash -d Bad=sub":              "lowercase letters",
		"hash -d root=sub":             "is a user on this machine",
		"hash -d gone=missing":         "does not exist",
		"hash -d odd=~nope/x":          "~nope names no directory",
		"hash -d file=sub/inner/f.txt": "is not a directory",
	} {
		if got := run("toolu_refused", command); !strings.Contains(got, reason) || !strings.Contains(got, "[exit 1]") {
			t.Errorf("%s: got %q, want a refusal with %q", command, got, reason)
		}
	}
	// Bash's own hash -d name keeps its meaning: it forgets a remembered command, and defines nothing.
	if got := run("toolu_bash", "hash -d ls; hash -d | grep -c ls"); strings.Contains(got, "now names") {
		t.Fatalf("bash's hash -d name must not define a name, got %q", got)
	}
}

func TestADefinitionIsTheDefinersAndLaterSubagentsOnly(t *testing.T) {
	directory := agentDirectory(t)
	for _, folder := range []string{"early", "late", "mine", "sibling"} {
		if err := os.Mkdir(filepath.Join(directory, folder), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	agents := map[string]hookstate.Agent{}
	file := func(toolUse string, agent hookstate.Agent) {
		mu.Lock()
		defer mu.Unlock()
		agents[toolUse] = agent
	}
	run := connectNamed(t, directory, nil, func(toolUse string) hookstate.Agent {
		mu.Lock()
		defer mu.Unlock()
		return agents[toolUse]
	})
	sees := func(toolUse string) string { return run(toolUse, "hash -d") }
	moment := func() time.Time {
		at := time.Now()
		time.Sleep(time.Millisecond)
		return at
	}
	before := moment()
	run("toolu_m1", "hash -d early=early")
	afterEarly := moment()
	run("toolu_m2", "hash -d late=late")
	afterLate := moment()
	// a1 was spawned between the main agent's two definitions, a0 before both; a2 after both, with no moment known, and a fork after both.
	file("toolu_a1", hookstate.Agent{ID: "a1", Spawned: true, At: afterEarly})
	file("toolu_a0", hookstate.Agent{ID: "a0", Spawned: true, At: before})
	file("toolu_f1", hookstate.Agent{ID: "f1", Spawned: true, Forked: true, At: afterLate})
	file("toolu_u1", hookstate.Agent{ID: "u1"})
	expect(t, sees("toolu_a1"), "early="+filepath.Join(directory, "early"))
	expect(t, sees("toolu_a0"), "(no output)")
	expect(t, sees("toolu_f1"), "early="+filepath.Join(directory, "early")+"\nlate="+filepath.Join(directory, "late"))
	// An agent whose parent the hook could not tell inherits nothing.
	expect(t, sees("toolu_u1"), "(no output)")
	file("toolu_a1b", hookstate.Agent{ID: "a1"})
	run("toolu_a1b", "hash -d mine=mine")
	afterMine := moment()
	// The definer's parent and siblings never see its names; its own later subagents do, its earlier ones do not.
	expect(t, sees("toolu_m3"), "early="+filepath.Join(directory, "early")+"\nlate="+filepath.Join(directory, "late"))
	file("toolu_a2", hookstate.Agent{ID: "a2", Spawned: true})
	expect(t, sees("toolu_a2"), "early="+filepath.Join(directory, "early")+"\nlate="+filepath.Join(directory, "late"))
	file("toolu_c1", hookstate.Agent{ID: "c1", Spawned: true, Parent: "a1", At: afterMine})
	file("toolu_c0", hookstate.Agent{ID: "c0", Spawned: true, Parent: "a1", At: afterEarly})
	expect(t, sees("toolu_c1"), "early="+filepath.Join(directory, "early")+"\nmine="+filepath.Join(directory, "mine"))
	expect(t, sees("toolu_c0"), "early="+filepath.Join(directory, "early"))
	// A name defined later by the parent never reaches an agent already started, the main agent's included.
	run("toolu_m4", "hash -d sibling=sibling")
	file("toolu_a2b", hookstate.Agent{ID: "a2"})
	if got := sees("toolu_a2b"); strings.Contains(got, "sibling") {
		t.Fatalf("a name defined after an agent started must not reach it, got %q", got)
	}
}

func TestALongPathIsNamedOnItsSecondUseOrAtACd(t *testing.T) {
	f := newFixture(t, nil)
	skill := filepath.Join(t.TempDir(), "claude-1000", "bundled-skills", "2.1.302", "fc11d6128293e2a1c2935a66fffdd589", "plugin-authoring", "types")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "api.d.ts"), []byte("export {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defined := names.NewDefined()
	defined.Automatic("", f.directory)
	f.names = (&names.Table{Base: f.directory}).For(defined, "s")
	// A real program's argument, which nib never prints, counts all the same, once a later call uses it again.
	f.run("/usr/bin/wc -l " + filepath.Join(skill, "api.d.ts"))
	if notes := defined.Announced("s"); len(notes) != 0 {
		t.Fatalf("named on its first use: %v", notes)
	}
	f.run("/usr/bin/wc -l " + filepath.Join(skill, "api.d.ts"))
	if notes := defined.Announced("s"); len(notes) != 1 || !strings.HasPrefix(notes[0], "[nib named ~bptypes = ") {
		t.Fatalf("notes %v", notes)
	}
	if got := f.run("cat ~bptypes/api.d.ts"); !strings.Contains(got, "1:export {}") {
		t.Fatalf("got %s", got)
	}
	// A directory the agent changes into is named at once, the cd given relative to where the shell is.
	other := filepath.Join(filepath.Dir(skill), "other-types")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := names.NewDefined()
	fresh.Automatic("", f.directory)
	f.names = (&names.Table{Base: f.directory}).For(fresh, "s")
	top := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(skill))))))
	relative, _ := filepath.Rel(top, other)
	f.run("cd " + top + " && cd " + relative + " && pwd >/dev/null")
	if notes := fresh.Announced("s"); len(notes) != 1 || !strings.Contains(notes[0], "/plugin-authoring/other-types, too long to print again") {
		t.Fatalf("notes %v", notes)
	}
}

func TestOnlyCleanPathsACommandSpellsCountTowardNames(t *testing.T) {
	f := newFixture(t, nil)
	skill := filepath.Join(t.TempDir(), "claude-1000", "bundled-skills", "2.1.302", "fc11d6128293e2a1c2935a66fffdd589", "plugin-authoring", "types")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(skill)
	defined := names.NewDefined()
	defined.Automatic("", f.directory)
	f.names = (&names.Table{Base: f.directory}).For(defined, "s")
	// A path after an expansion is a fragment of another path, and one that does not exist names nothing.
	// Two calls, so a path that counted would be named in the second.
	for range 2 {
		f.run("d=x; /usr/bin/true \"$d" + filepath.Join(skill, "api.d.ts") + "\" " + filepath.Join(t.TempDir(), "claude-1000", "bundled-skills", "2.1.302", "fc11d6128293e2a1c2935a66fffdd589", "missing", "x.txt"))
		if notes := defined.Announced("s"); len(notes) != 0 {
			t.Fatalf("notes %v", notes)
		}
	}
	// A path inside code given to another program counts up to where the path ends, and never names the file.
	code := "/usr/bin/true -c \"f='" + filepath.Join(skill, "api.d.ts") + "'\nimport json\nprint(sum(delta)/len(y))\""
	f.run(code)
	defined.Announced("s")
	f.run(code)
	notes := defined.Announced("s")
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "[nib named ~bptypes = ") {
		t.Fatalf("notes %v", notes)
	}
	if got, _ := f.names.Home("bptypes"); got != canonical {
		t.Fatalf("~bptypes is %q, want %q", got, canonical)
	}
}

func TestNamesExpandInBackgroundJobsAndPipelines(t *testing.T) {
	f := newFixture(t, map[string]string{"d/a.txt": "x\n"})
	f.names = (&names.Table{Base: f.directory}).For(names.NewDefined(), "s")
	expect(t, f.run("hash -d nm=d"), "[~nm now names "+filepath.Join(f.directory, "d")+"]")
	want := filepath.Join(f.directory, "d", "a.txt")
	expect(t, f.run("echo ~nm/a.txt | cat"), want)
	expect(t, f.run("echo ~nm/a.txt > amp.txt & wait; read v < amp.txt; echo $v"), want)
	f.call(server.ShellCall{Command: "echo ~nm/a.txt > job.txt", Background: true})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(filepath.Join(f.directory, "job.txt")); len(data) > 0 {
			expect(t, strings.TrimSpace(string(data)), want)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the background job wrote nothing")
}

func TestASandboxedDefinitionStaysInsideTheRoots(t *testing.T) {
	f := newFixture(t, map[string]string{"d/a.txt": "x\n"})
	f.sandbox = true
	f.names = (&names.Table{Base: f.directory}).For(names.NewDefined(), "s")
	outside := t.TempDir()
	if got := f.run("hash -d out=" + outside); !strings.Contains(got, "hash: ~out:") || !strings.Contains(got, "[exit 1]") {
		t.Fatalf("a name for a directory outside the roots must be refused, got %q", got)
	}
	expect(t, f.run("hash -d in=d"), "[~in now names "+filepath.Join(f.directory, "d")+"]")
}
