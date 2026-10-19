package names

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNamesAnAgentGotBeforeADropAreToldAgainOnceEach(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets, gadgets, later := deep(t, root, "widgets"), deep(t, root, "gadgets"), deep(t, root, "later")
	defined := NewDefined()
	view := (&Table{Base: root}).For(defined, "main")
	for name, directory := range map[string]string{"w": widgets, "g": gadgets} {
		if err := view.Define(name, directory); err != nil {
			t.Fatal(err)
		}
	}
	dropped := time.Now().Add(time.Millisecond)
	defined.Now = func() time.Time { return dropped.Add(time.Second) }
	if err := view.Define("l", later); err != nil {
		t.Fatal(err)
	}
	defined.Forget("main", dropped)
	view.Display(filepath.Join(widgets, "A.java"))
	view.Display(filepath.Join(widgets, "B.java"))
	view.Display(filepath.Join(later, "C.java"))
	notes := defined.Announced("main")
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "[~w = "+filepath.ToSlash(widgets)+", a name you got before your context was compacted or cleared;") {
		t.Fatalf("a name defined before the drop is told again once, in full; one defined since is not: %v", notes)
	}
	view.Display(filepath.Join(gadgets, "D.java"))
	if notes := defined.Announced("main"); len(notes) != 1 || notes[0] != "[~g = "+filepath.ToSlash(gadgets)+"]" {
		t.Fatalf("the next is brief: %v", notes)
	}
	view.Display(filepath.Join(widgets, "E.java"))
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("told again only once: %v", notes)
	}
}

// deep makes root/zenit/src/server/java/be/elevenways/<leaf> and returns it.
func deep(t *testing.T, root, leaf string) string {
	t.Helper()
	directory := filepath.Join(root, "zenit", "src", "server", "java", "be", "elevenways", leaf)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestADirectoryShownPathsKeepRunningThroughGetsANameOnce(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := deep(t, root, "widgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	defined := NewDefined()
	defined.Automatic(file, root)
	view := (&Table{Base: root}).For(defined, "main")
	for i := range 3 {
		view.Display(filepath.Join(widgets, fmt.Sprintf("V%d.java", i)))
	}
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("named too soon: %v", notes)
	}
	// The path that earns the name prints under it already, and the note names the directory whole.
	untilNamed(t, view, widgets)
	notes := defined.Announced("main")
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "[nib named ~bewidgets = "+filepath.ToSlash(widgets)+", which ") {
		t.Fatalf("notes %v", notes)
	}
	if again := defined.Announced("main"); len(again) != 0 {
		t.Fatalf("a note is told once, got %v", again)
	}
	if shown, _ := view.Display(filepath.Join(widgets, "Last.java")); shown != "~bewidgets/Last.java" {
		t.Fatalf("got %q", shown)
	}
	// Another agent counts for itself.
	if shown, ok := (&Table{Base: root}).For(defined, "other").Display(filepath.Join(widgets, "A.java")); ok {
		t.Fatalf("another agent got the name: %q", shown)
	}
	// The next server of the same directory does not start with a name only one session used.
	next := &Table{Base: root}
	next.AddLearned(file)
	if len(next.Entries) != 0 {
		t.Fatalf("one session's name was carried: %v", next.Entries)
	}
	// Once a second session made it too, the next one has it from the start; one of another directory does not.
	second := NewDefined()
	second.Automatic(file, root)
	again := (&Table{Base: root}).For(second, "main")
	untilNamed(t, again, widgets)
	second.Announced("main")
	next.AddLearned(file)
	if len(next.Entries) != 1 || next.Entries[0] != (Entry{"bewidgets", widgets}) {
		t.Fatalf("learned %v", next.Entries)
	}
	elsewhere := &Table{Base: filepath.Join(root, "zenit")}
	elsewhere.AddLearned(file)
	if len(elsewhere.Entries) != 0 {
		t.Fatalf("another directory learned %v", elsewhere.Entries)
	}
}

// sessions runs define once in each of count servers of root keeping names in file, as successive sessions would.
func sessions(t *testing.T, count int, file, root string, define func(*Defined) error) {
	t.Helper()
	for range count {
		defined := NewDefined()
		defined.Automatic(file, root)
		if err := define(defined); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAPrefixTooLongToRepeatIsNamedInTheSecondCallThatShowsIt(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	skill := filepath.Join(root, "claude-1000", "bundled-skills", "2.1.302", "fc11d6128293e2a1c2935a66fffdd589", "plugin-authoring", "types")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	defined := NewDefined()
	defined.Automatic("", "/elsewhere")
	view := (&Table{Base: "/elsewhere"}).For(defined, "main")
	// A build copy shown once costs no note.
	if shown, ok := view.Display(filepath.Join(skill, "claude-code.d.ts")); ok {
		t.Fatalf("got %q", shown)
	}
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("named at first sight: %v", notes)
	}
	if shown, _ := view.Display(filepath.Join(skill, "index.d.ts")); shown != "~bptypes/index.d.ts" {
		t.Fatalf("the second call that shows it names it and prints under it, got %q", shown)
	}
	if notes := defined.Announced("main"); len(notes) != 1 || !strings.Contains(notes[0], "/plugin-authoring/types, which 2 of the paths shown to you ran through; ~bptypes/ works") {
		t.Fatalf("notes %v", notes)
	}
	if shown, _ := view.Display(filepath.Join(skill, "claude-code.d.ts")); shown != "~bptypes/claude-code.d.ts" {
		t.Fatalf("got %q", shown)
	}
}

func TestALongDirectoryTheAgentWorksInIsNamedTheFirstTimeItIsShownUnlessGitIgnoresIt(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	long := func(leaf string) string {
		directory := filepath.Join(root, "zenit", "build", "zenit-dev", "workspace-pins-"+leaf, "zenit", "src", "common", "java", "be", "elevenways", "zenit", "common", "orm", "watch")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	worked, ignored, learnedDir := long("worked"), long("ignored"), long("learned")
	file := filepath.Join(t.TempDir(), "learned.json")
	data, _ := json.Marshal(learnedNames{root, []learned{{Entry{"lwatch", learnedDir}, 1, "earlier", time.Now().Unix()}}})
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	defined := NewDefined()
	defined.Automatic(file, root)
	defined.Ignored = func(directory string) bool { return strings.Contains(directory, "pins-ignored/") }
	view := (&Table{Base: root}).For(defined, "main")
	view.Worked(filepath.Join(worked, "sub"))
	view.Worked(worked)
	view.Worked(ignored)
	view.Display(filepath.Join(worked, "A.java"))
	if notes := defined.Announced("main"); len(notes) != 1 || !strings.Contains(notes[0], "workspace-pins-worked/zenit/src/common/java/be/elevenways/zenit/common/orm/watch, too long to print again") {
		t.Fatalf("a directory worked in: %v", notes)
	}
	view.Display(filepath.Join(ignored, "A.java"))
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("an ignored directory was named at first sight: %v", notes)
	}
	view.Display(filepath.Join(learnedDir, "A.java"))
	if notes := defined.Announced("main"); len(notes) != 1 || !strings.HasPrefix(notes[0], "[~lwatch = ") {
		t.Fatalf("a directory an earlier session named: %v", notes)
	}
	// Another agent did not work there.
	other := (&Table{Base: root}).For(defined, "other")
	other.Display(filepath.Join(worked, "B.java"))
	if notes := defined.Announced("other"); len(notes) != 0 {
		t.Fatalf("another agent: %v", notes)
	}
}

func TestLaterNotesAreBriefUntilManyCallsHavePassed(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	defined := NewDefined()
	defined.Automatic("", root)
	view := (&Table{Base: root}).For(defined, "main")
	// name shows the paths of one call that earn leaf's directory a name, and returns the note on it.
	name := func(leaf string) string {
		directory := deep(t, filepath.Join(root, leaf), leaf)
		untilNamed(t, view, directory)
		notes := defined.Announced("main")
		if len(notes) != 1 {
			t.Fatalf("notes %v", notes)
		}
		return notes[0]
	}
	if got := name("widgets"); !strings.HasPrefix(got, "[nib named ~bewidgets = ") {
		t.Fatalf("the first note is told in full, got %q", got)
	}
	if got := name("gadgets"); !strings.HasPrefix(got, "[~begadgets = "+filepath.ToSlash(filepath.Join(root, "gadgets"))+"/zenit/src/server/java/be/elevenways/gadgets, ") || !strings.HasSuffix(got, " paths shown]") {
		t.Fatalf("got %q", got)
	}
	// Calls that show paths count toward telling a note in full again.
	for range fullAgain - 3 {
		view.Display(filepath.Join(root, "x"))
		defined.Announced("main")
	}
	if got := name("doodads"); !strings.HasPrefix(got, "[~bedoodads = ") {
		t.Fatalf("got %q", got)
	}
	if got := name("gizmos"); !strings.HasPrefix(got, "[nib named ~begizmos = ") {
		t.Fatalf("a note is told in full again after %d calls, got %q", fullAgain, got)
	}
}

func TestShortPrefixesAndTakenNamesAndTheLimit(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	defined := NewDefined()
	defined.Automatic("", root)
	// A prefix under 30 characters as printed is never worth a name.
	short := filepath.Join(root, "src", "a")
	if err := os.MkdirAll(short, 0o755); err != nil {
		t.Fatal(err)
	}
	view := (&Table{Base: root, Entries: []Entry{{"bewidgets", short}}}).For(defined, "main")
	for i := range 3 * autoUses {
		view.Display(filepath.Join(root, "src", fmt.Sprintf("f%d", i)))
	}
	if notes := defined.Announced("main"); len(notes) != 0 {
		t.Fatalf("a short prefix got a name: %v", notes)
	}
	// A taken name gets a number after it.
	widgets := deep(t, root, "widgets")
	for i := range 8 {
		view.Display(filepath.Join(widgets, fmt.Sprintf("W%d.java", i)))
	}
	defined.Announced("main")
	if shown, _ := view.Display(filepath.Join(widgets, "X.java")); shown != "~bewidgets2/X.java" {
		t.Fatalf("got %q", shown)
	}
	defined.Announced("main")
	// At most autoLimit names per agent.
	for leaf := range autoLimit + 2 {
		directory := filepath.Join(root, fmt.Sprintf("project%d", leaf), "src", "main", "java", "be", "elevenways", "core")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range 8 {
			view.Display(filepath.Join(directory, fmt.Sprintf("f%d", i)))
		}
	}
	if named := len(defined.Announced("main")); named != autoLimit-1 {
		t.Fatalf("made %d more names, want %d", named, autoLimit-1)
	}
}

func TestNamesNibMakesStayShort(t *testing.T) {
	table := &Table{Base: "/w"}
	for _, c := range []struct{ directory, want string }{
		{"/h/claude-configs/nib/claude/projects/-home-skerit-projects-nib", "cpnib"},
		{"/t/bundled-skills/2.1.302/fc11d6128293e2a1c2935a66fffdd589/plugin-authoring/types", "bptypes"},
		{"/z/java/be/elevenways/widgets", "bewidgets"},
		{"/m/github.com/modelcontextprotocol/go-sdk@v1.8.0", "gmsdk"},
		{"/a/src/main/PluginAuthoringReference", "smreference"},
		{"/a/src/main/release.notes.archive", "smarchive"},
		{"/a/src/main/supercalifragilistic", "smsupercalif"},
		{"/a/src/main/__pycache__", "smpycache"},
	} {
		if got := autoName(c.directory, table); got != c.want {
			t.Errorf("%s: got %q, want %q", c.directory, got, c.want)
		}
	}
	// A number for a taken name keeps it within nameMax, cutting the stem rather than growing past it.
	table.Entries = []Entry{{"smsupercalif", "/other"}}
	if got := autoName("/a/src/main/supercalifragilistic", table); got != "smsupercali2" {
		t.Errorf("got %q", got)
	}
}

func TestNamesAnAgentDefinesAreLearnedToo(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := deep(t, root, "widgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	sessions(t, 2, file, root, func(defined *Defined) error {
		return (&Table{Base: root}).For(defined, "main").Define("w", widgets)
	})
	next := &Table{Base: root}
	next.AddLearned(file)
	if len(next.Entries) != 1 || next.Entries[0].Name != "w" {
		t.Fatalf("learned %v", next.Entries)
	}
}

func TestANameNeverPrintsAPathLongerThanItsPlainForm(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	internal := filepath.Join(root, "internal")
	if err := os.MkdirAll(filepath.Join(internal, "tag"), 0o755); err != nil {
		t.Fatal(err)
	}
	widgets := deep(t, root, "widgets")
	table := &Table{Base: root, Entries: []Entry{{"pninternal", internal}, {"bewidgets", widgets}}}
	if shown, ok := table.Display(filepath.Join(internal, "tag", "tag.go")); ok {
		t.Fatalf("a path inside the base printed as %q, longer than internal/tag/tag.go", shown)
	}
	if shown, ok := table.Display(filepath.Join(widgets, "a.go")); !ok || shown != "~bewidgets/a.go" {
		t.Fatalf("a deep path printed as %q, %v", shown, ok)
	}
	outside := &Table{Entries: []Entry{{"pninternal", internal}}}
	if shown, ok := outside.Display(filepath.Join(internal, "tag", "tag.go")); !ok || shown != "~pninternal/tag/tag.go" {
		t.Fatalf("outside the base the name should still shorten the absolute path: %q, %v", shown, ok)
	}
}

func TestLearnedNamesThatSaveNothingInTheBaseAreNeitherKeptNorLoaded(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	worktree, _ := filepath.EvalSymlinks(t.TempDir())
	internal := filepath.Join(root, "internal")
	if err := os.MkdirAll(internal, 0o755); err != nil {
		t.Fatal(err)
	}
	widgets := deep(t, root, "widgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	// An agent in a worktree sees the server's internal folder as an absolute path, where a name saves a lot; in root it saves nothing.
	sessions(t, 2, file, root, func(defined *Defined) error {
		if err := (&Table{Base: worktree}).For(defined, "main").Define("pninternal", internal); err != nil {
			return err
		}
		return (&Table{Base: root}).For(defined, "main").Define("bewidgets", widgets)
	})
	next := &Table{Base: root}
	next.AddLearned(file)
	if len(next.Entries) != 1 || next.Entries[0].Name != "bewidgets" {
		t.Fatalf("learned %v", next.Entries)
	}
	// A file written before this rule holds such a name; loading leaves it out.
	used := fmt.Sprint(time.Now().Unix())
	data := `{"base": ` + quoted(root) + `, "names": [{"Name": "pninternal", "Directory": ` + quoted(internal) + `, "runs": 2, "used": ` + used + `}, {"Name": "bewidgets", "Directory": ` + quoted(widgets) + `, "runs": 2, "used": ` + used + `}]}`
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := &Table{Base: root}
	loaded.AddLearned(file)
	if len(loaded.Entries) != 1 || loaded.Entries[0].Name != "bewidgets" {
		t.Fatalf("loaded %v", loaded.Entries)
	}
}

// showUntilNamed prints paths in directory to a fresh server of root keeping names in file until nib names it, and returns the notes.
func showUntilNamed(t *testing.T, file, root, directory string) []string {
	t.Helper()
	defined := NewDefined()
	defined.Automatic(file, root)
	untilNamed(t, (&Table{Base: root}).For(defined, "main"), directory)
	return defined.Announced("main")
}

// untilNamed shows paths in directory to view until one prints under a name nib made, and returns how many it showed.
func untilNamed(t *testing.T, view *View, directory string) int {
	t.Helper()
	for i := range 40 {
		if _, ok := view.Display(filepath.Join(directory, fmt.Sprintf("W%d.java", i))); ok {
			return i + 1
		}
	}
	t.Fatalf("%s was never named", directory)
	return 0
}

func TestAKeptNameStaysWhileSessionsPrintUnderItAndGoesOnceUnused(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := deep(t, root, "widgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	showUntilNamed(t, file, root, widgets)
	showUntilNamed(t, file, root, widgets)
	// A third session starts with the name; a path it prints under the name counts it once more, however many it prints.
	table := &Table{Base: root}
	table.AddLearned(file)
	third := NewDefined()
	third.Automatic(file, root)
	view := table.For(third, "main")
	for range 3 {
		if shown, _ := view.Display(filepath.Join(widgets, "A.java")); shown != "~bewidgets/A.java" {
			t.Fatalf("got %q", shown)
		}
	}
	if kept := readLearned(file, root); len(kept) != 1 || kept[0].Runs != 3 {
		t.Fatalf("kept %+v", kept)
	}
	// Unused for longer than learnedDays, it is neither carried nor kept.
	old := time.Now().Add(-(learnedDays + 1) * 24 * time.Hour).Unix()
	data := fmt.Sprintf(`{"base": %q, "names": [{"Name": "bewidgets", "Directory": %q, "runs": 5, "used": %d}]}`, root, widgets, old)
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := &Table{Base: root}
	stale.AddLearned(file)
	if len(stale.Entries) != 0 {
		t.Fatalf("a stale name was carried: %v", stale.Entries)
	}
	gadgets := deep(t, root, "gadgets")
	showUntilNamed(t, file, root, gadgets)
	if kept := readLearned(file, root); len(kept) != 1 || kept[0].Name != "begadgets" || kept[0].Runs != 1 {
		t.Fatalf("kept %+v", kept)
	}
}

func TestNamesInTheTemporaryFolderOrAnotherWorktreeAreNotKept(t *testing.T) {
	scratch, _ := filepath.EvalSymlinks(t.TempDir())
	skill := deep(t, scratch, "types")
	file := filepath.Join(t.TempDir(), "learned.json")
	// A server outside the temporary folder keeps no name for a folder inside it; it names it for the session alone.
	outside := "/nib-test-base-outside-the-temporary-folder"
	sessions(t, 2, file, outside, func(defined *Defined) error {
		return (&Table{Base: outside}).For(defined, "main").Define("skill", skill)
	})
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a temporary folder's name was written: %v", err)
	}
	// In a repository, a folder of another worktree of it is not kept; one of the server's own worktree or of a nested repository is.
	root, _ := filepath.EvalSymlinks(t.TempDir())
	worktree := filepath.Join(root, ".claude", "worktrees", "agent-a1")
	gitdir := filepath.Join(root, ".git", "worktrees", "agent-a1")
	nested := filepath.Join(root, "vendor", "lib")
	for _, directory := range []string{worktree, gitdir, filepath.Join(nested, ".git")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{filepath.Join(worktree, ".git"): "gitdir: " + gitdir + "\n", filepath.Join(gitdir, "commondir"): "../..\n"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	directories := map[string]string{"theirs": deep(t, worktree, "models"), "ours": deep(t, root, "widgets"), "vendored": deep(t, nested, "parts")}
	sessions(t, 2, file, root, func(defined *Defined) error {
		view := (&Table{Base: root}).For(defined, "main")
		for name, directory := range directories {
			if err := view.Define(name, directory); err != nil {
				return err
			}
		}
		return nil
	})
	next := &Table{Base: root}
	next.AddLearned(file)
	var got []string
	for _, e := range next.Entries {
		got = append(got, e.Name)
	}
	if slices.Sort(got); !slices.Equal(got, []string{"ours", "vendored"}) {
		t.Fatalf("carried %v", got)
	}
}

func TestAFileFromBeforeSessionsWereCountedKeepsOnlyNamesNibMakesAsOneSession(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	projects := filepath.Join(root, "claude", "projects", "-home-skerit-projects-nib")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	widgets, gadgets := deep(t, root, "widgets"), deep(t, root, "gadgets")
	file := filepath.Join(t.TempDir(), "learned.json")
	// An older scheme's name, an agent's, a gone folder's and one nib makes now, numbered as a taken name is.
	data := fmt.Sprintf(`{"base": %q, "names": [{"Name": "cphome-skerit-projects-nib", "Directory": %q}, {"Name": "w", "Directory": %q}, {"Name": "begone", "Directory": %q}, {"Name": "begadgets2", "Directory": %q}]}`,
		root, projects, widgets, filepath.Join(root, "gone"), gadgets)
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	next := &Table{Base: root}
	next.AddLearned(file)
	if len(next.Entries) != 0 {
		t.Fatalf("carried %v", next.Entries)
	}
	// The name nib makes counts as one session, so the next one that names the folder gets that name again, and the one after starts with it.
	if notes := showUntilNamed(t, file, root, gadgets); len(notes) != 1 || !strings.Contains(notes[0], "~begadgets2 = ") {
		t.Fatalf("notes %v", notes)
	}
	if kept := readLearned(file, root); len(kept) != 1 || kept[0].Name != "begadgets2" || kept[0].Runs != 2 {
		t.Fatalf("kept %+v", kept)
	}
	next.AddLearned(file)
	if len(next.Entries) != 1 || next.Entries[0] != (Entry{"begadgets2", gadgets}) {
		t.Fatalf("carried %v", next.Entries)
	}
}

// quoted is text as a JSON string, so a Windows path's backslashes stay what they are.
func quoted(text string) string {
	data, _ := json.Marshal(text)
	return string(data)
}

func TestPreviewedPathsAllPrintUnderTheNameTheyEarnAndCountOnce(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	widgets := deep(t, root, "widgets")
	defined := NewDefined()
	defined.Automatic("", root)
	view := (&Table{Base: root}).For(defined, "main")
	var paths []string
	for i := range 40 {
		paths = append(paths, filepath.Join(widgets, fmt.Sprintf("W%d.java", i)))
	}
	view.Preview(paths)
	for _, path := range paths {
		if shown, _ := view.Display(path); shown != "~bewidgets/"+filepath.Base(path) {
			t.Fatalf("got %q", shown)
		}
	}
	// Previewed paths count once: the name comes at the same count as when each path is only displayed.
	alone := NewDefined()
	alone.Automatic("", root)
	count := untilNamed(t, (&Table{Base: root}).For(alone, "main"), widgets)
	if notes := defined.Announced("main"); len(notes) != 1 || !strings.Contains(notes[0], fmt.Sprintf("which %d of the paths shown to you ran through", count)) {
		t.Fatalf("notes %v, want the count %d", notes, count)
	}
	// The next call counts its paths again.
	view.Display(paths[0])
	if got := defined.uses["main"]["bewidgets"]; got != 41 {
		t.Fatalf("uses %d", got)
	}
}
