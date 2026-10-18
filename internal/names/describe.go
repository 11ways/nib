package names

import (
	"maps"
	"strings"
)

// Described is a name an agent sees: where it came from and how many paths were printed under it.
type Described struct {
	Name      string
	Directory string
	// Source is "server" for nib's own table, else what the agent's list says: "defined", "automatic" or "inherited".
	Source string
	Uses   int
}

// Describe is every name the agent sees, the server's first, each with the paths printed under it. It only reads.
func (v *View) Describe() []Described {
	var out []Described
	used := v.defined.usedBy(v.agent)
	if v.static != nil {
		for _, e := range v.static.Entries {
			out = append(out, Described{e.Name, e.Directory, "server", used[e.Name]})
		}
	}
	for _, d := range v.defined.described(v.agent) {
		d.Uses = used[d.Name]
		out = append(out, d)
	}
	return out
}

// described are the names agent defined or got, the latest of each name, in the order they were first defined.
func (d *Defined) described(agent string) []Described {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := d.agents[agent]
	if names == nil {
		return nil
	}
	var out []Described
	at := map[string]int{}
	for _, def := range names.list {
		row := Described{Name: def.Name, Directory: def.Directory, Source: def.kind}
		if i, ok := at[def.Name]; ok {
			out[i] = row
			continue
		}
		at[def.Name] = len(out)
		out = append(out, row)
	}
	return out
}

// usedBy are the paths printed under each of agent's names.
func (d *Defined) usedBy(agent string) map[string]int {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return maps.Clone(d.uses[agent])
}

// use counts a path printed as shown, which starts with ~name/ when a name stands for part of it, and toward the name's kept entry.
func (d *Defined) use(agent, shown string) {
	if d == nil || !strings.HasPrefix(shown, "~") {
		return
	}
	name, _, _ := strings.Cut(shown[1:], "/")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.uses == nil {
		d.uses = map[string]map[string]int{}
	}
	if d.uses[agent] == nil {
		d.uses[agent] = map[string]int{}
	}
	d.uses[agent][name]++
	d.touchLocked(name)
}
