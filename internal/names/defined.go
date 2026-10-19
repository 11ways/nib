package names

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefinedLimit bounds the definitions one agent makes; a redefinition counts too.
const DefinedLimit = 64

// Defined are the named directories agents define at runtime, kept per agent in memory for the life of the process. An agent sees the names
// it defined and those it inherited when it started: the names its parent saw before the agent was spawned.
type Defined struct {
	mu     sync.Mutex
	agents map[string]*defined
	// Now stamps definitions; it must be comparable with other processes' clocks, as a spawn moment comes from the harness. Set it before
	// first use.
	Now func() time.Time
	// Ignored reports whether git's ignore rules cover a directory, which is then never named the first time it is shown; nil covers none.
	// Set it before first use.
	Ignored func(directory string) bool
	// automatic is set by Automatic: nib names the directories shown paths keep running through, and keeps names in file for base. run tells
	// this server apart from others in the file, and touched are the kept names it counted a use of.
	automatic  bool
	file, base string
	run        string
	touched    map[string]bool
	// uses counts, per agent, the paths printed under each name, the server's own included.
	uses map[string]map[string]int
	// ignoredAt keeps what Ignored said of each directory asked.
	ignoredAt map[string]bool
}

// defined is one agent's names, oldest first: the inherited ones, then its own; the latest of a name wins. counts are the shown paths per
// directory that automatic naming weighs, since the call (plus one) each was first shown in, worked the directories the agent changed into
// or wrote a file in, auto the names it made, and made those not yet told, which take effect once told. calls counts the calls that showed
// the agent paths, full the call of the last note told in full, and observed that the current call showed one. untold are the names the
// agent got before its context was last dropped, and retell those of them a path in the current call ran through.
type defined struct {
	list     []definition
	own      int
	counts   map[string]int
	since    map[string]int
	worked   map[string]bool
	auto     int
	made     []made
	calls    int
	full     int
	observed bool
	untold   map[string]bool
	retell   []made
}

// made is a name nib made and has not told of yet: the directory as it printed, and the shown paths that ran through it.
type made struct {
	Entry
	prefix string
	count  int
}

// definition is a name with the moment it became visible to the agent's line: when it was defined, kept when it is inherited.
type definition struct {
	Entry
	at int64
	// kind is where the agent got the name: "defined" by itself, "automatic" made by nib, "inherited" from the agent that started it.
	kind string
}

// NewDefined makes an empty set.
func NewDefined() *Defined { return &Defined{agents: map[string]*defined{}, Now: time.Now} }

// Inherit gives agent the names parent saw before asOf, the zero asOf meaning now; it does nothing for an agent that has names already.
func (d *Defined) Inherit(agent, parent string, asOf time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	from := d.agents[parent]
	if agent == parent || d.agents[agent] != nil || from == nil {
		return
	}
	moment := int64(math.MaxInt64)
	if !asOf.IsZero() {
		moment = asOf.UnixNano()
	}
	to := &defined{}
	for _, entry := range from.list {
		if entry.at < moment {
			entry.kind = "inherited"
			to.list = append(to.list, entry)
		}
	}
	d.agents[agent] = to
}

// define adds a name for agent.
func (d *Defined) define(agent string, entry Entry) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		names = &defined{}
		d.agents[agent] = names
	}
	if names.own >= DefinedLimit {
		return fmt.Errorf("this agent has made %d definitions, the most nib keeps", DefinedLimit)
	}
	names.own++
	names.list = append(names.list, definition{entry, d.Now().UnixNano(), "defined"})
	d.learnLocked(entry)
	return nil
}

// visible are the names agent sees, the latest of each name, in the order they were first defined.
func (d *Defined) visible(agent string) []Entry {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		return nil
	}
	var entries []Entry
	at := map[string]int{}
	for _, entry := range names.list {
		if i, ok := at[entry.Name]; ok {
			entries[i] = entry.Entry
			continue
		}
		at[entry.Name] = len(entries)
		entries = append(entries, entry.Entry)
	}
	return entries
}

// View is what one agent sees: the server's table with the names visible to the agent laid over it. Each lookup reads the names as they are
// then, so a name works from the moment it is defined.
type View struct {
	static  *Table
	defined *Defined
	agent   string
	// server is the server's table as loaded, whose names no agent may define, also those a worktree lacks.
	server *Table
}

// For is the view of agent, whose definitions go to defined.
func (t *Table) For(defined *Defined, agent string) *View {
	return &View{static: t, defined: defined, agent: agent, server: t}
}

// In is the view of an agent working in directory, a git worktree of the server's directory: each of the server's names inside that
// directory names the same place in the worktree, and one the worktree lacks is left out rather than naming the server's copy. Names agents
// defined keep the directory they were given. directory "" or the server's own leaves the view as it is.
func (v *View) In(directory string) *View {
	if v.static == nil || directory == "" {
		return v
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || canonical == v.static.Base {
		return v
	}
	moved := &Table{Base: canonical}
	for _, entry := range v.static.Entries {
		relative, err := filepath.Rel(v.static.Base, entry.Directory)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			moved.Entries = append(moved.Entries, entry)
			continue
		}
		if there, err := filepath.EvalSymlinks(filepath.Join(canonical, relative)); err == nil {
			moved.Entries = append(moved.Entries, Entry{entry.Name, there})
		}
	}
	return &View{static: moved, defined: v.defined, agent: v.agent, server: v.server}
}

// table is the static table with the agent's names added.
func (v *View) table() *Table {
	visible := v.defined.visible(v.agent)
	if len(visible) == 0 && v.static != nil {
		return v.static
	}
	table := &Table{}
	if v.static != nil {
		table.Base = v.static.Base
		table.Entries = slices.Clone(v.static.Entries)
	}
	table.Entries = append(table.Entries, visible...)
	return table
}

// Expand turns a leading ~name/ (or a bare ~name) into its directory; ok is false when input names none.
func (v *View) Expand(input string) (string, bool) { return v.table().Expand(input) }

// Display is canonical under the deepest named directory holding it; ok is false when none does. With automatic naming on, the path counts
// toward naming a directory it runs through; a name made then takes effect once Announced tells of it, so one response prints every path the
// same way. A name the agent got before its context was dropped is told again once a path printed under it.
func (v *View) Display(canonical string) (string, bool) {
	table := v.table()
	v.defined.observe(v.agent, table, filepath.Dir(canonical))
	shown, ok := table.Display(canonical)
	if ok {
		v.defined.mention(v.agent, table, shown)
		v.defined.use(v.agent, shown)
	}
	return shown, ok
}

// Observe counts path, an absolute path the agent used, toward naming a directory it runs through, as a path shown to it counts: from the
// deepest directory of it that exists, canonical, so text after a path or a file's own name never ends up in a name.
func (v *View) Observe(path string) {
	directory := filepath.Clean(path)
	for info, err := os.Stat(directory); err != nil || !info.IsDir(); info, err = os.Stat(directory) {
		if directory == filepath.Dir(directory) {
			return
		}
		directory = filepath.Dir(directory)
	}
	if canonical, err := filepath.EvalSymlinks(directory); err == nil {
		v.defined.observe(v.agent, v.table(), canonical)
	}
}

// Worked notes that the agent works in directory, an absolute path it changed into or wrote a file in: a long directory holding it is then
// named the first time it is shown, as the agent will be back.
func (v *View) Worked(directory string) {
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return
	}
	if info, err := os.Stat(canonical); err == nil && info.IsDir() {
		v.defined.work(v.agent, canonical)
	}
}

// Home is the directory of name; ok is false when it names none.
func (v *View) Home(name string) (string, bool) { return v.table().Home(name) }

// Entries are every name the agent sees: the server's, then its own and inherited ones.
func (v *View) Entries() []Entry { return slices.Clone(v.table().Entries) }

// Define makes name stand for directory, an absolute path, for this agent and the agents it spawns from now on. A name is lowercase letters,
// digits and dashes starting with a letter; a user's name on this machine, whose home ~name already means, and a name of the server's own
// table are refused. directory must be an existing directory; it is kept canonical.
func (v *View) Define(name, directory string) error {
	switch {
	case !valid.MatchString(name):
		return errors.New("a name is lowercase letters, digits and dashes, starting with a letter")
	case v.server != nil && slices.ContainsFunc(v.server.Entries, func(e Entry) bool { return e.Name == name }):
		return fmt.Errorf("~%s is one of nib's own named directories, which cannot be redefined", name)
	case isUser(name):
		return fmt.Errorf("%s is a user on this machine, whose home ~%s already names", name, name)
	case !filepath.IsAbs(directory):
		return errors.New("the directory must be an absolute path")
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("%s does not exist", directory)
	}
	if info, err := os.Stat(canonical); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", directory)
	}
	return v.defined.define(v.agent, Entry{name, canonical})
}
