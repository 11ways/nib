// Package names holds named directories: ~name/ paths that stand for long directory prefixes, from nib.json and from JVM source roots.
package names

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Entry is one named directory; Directory is canonical.
type Entry struct {
	Name      string
	Directory string
}

// Table is the set of named directories of one server; the zero value names nothing.
type Table struct {
	Entries []Entry
	// Base is the server's directory, canonical, for relative display.
	Base string
}

const (
	autoMinChars = 24
	autoMax      = 16
	walkDepth    = 8
	walkFolders  = 20000
)

var valid = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// skipped are folders the automatic walk never enters: build output and package caches.
var skipped = map[string]bool{"node_modules": true, "build": true, "target": true, "dist": true, "out": true, "bin": true}

// ConfigFile is the name of the file nib reads a directory's settings from: its named directories here, nibash's command rules too.
const ConfigFile = "nib.json"

// Config is nib.json's named directories, and Base: the integration branch of the repository it sits in, for git-each's {base}.
type Config struct {
	Directories     map[string]string `json:"directories"`
	AutoDirectories *bool             `json:"autoDirectories"`
	Base            string            `json:"base"`
}

// BaseBranch is the integration branch the nib.json in directory names; "" when it names none or cannot be read.
func BaseBranch(directory string) string {
	var config Config
	data, err := os.ReadFile(filepath.Join(directory, ConfigFile))
	if err != nil || json.Unmarshal(data, &config) != nil {
		return ""
	}
	return strings.TrimSpace(config.Base)
}

// isUser reports whether name is a user on this machine, whose ~name a shell would expand to that user's home.
var isUser = func(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}

// Load builds the table for a server running in directory; notes say what was skipped and why.
func Load(directory string) (*Table, []string) {
	base, err := filepath.EvalSymlinks(directory)
	if err != nil {
		base = directory
	}
	table := &Table{Base: base}
	var notes []string
	config := Config{}
	if data, err := os.ReadFile(filepath.Join(directory, ConfigFile)); err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			notes = append(notes, fmt.Sprintf(ConfigFile+" is not valid JSON (%v); its named directories were skipped", err))
			config = Config{}
		}
	}
	taken := map[string]bool{}
	configured := slices.Sorted(func(yield func(string) bool) {
		for name := range config.Directories {
			if !yield(name) {
				return
			}
		}
	})
	for _, name := range configured {
		path := config.Directories[name]
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		canonical, err := filepath.EvalSymlinks(path)
		switch {
		case !valid.MatchString(name):
			notes = append(notes, fmt.Sprintf("~%s skipped: a name is lowercase letters, digits and dashes, starting with a letter", name))
		case isUser(name):
			notes = append(notes, fmt.Sprintf("~%s skipped: it is a user on this machine, whose home ~%s already names", name, name))
		case err != nil:
			notes = append(notes, fmt.Sprintf("~%s skipped: %s does not exist", name, path))
		default:
			table.Entries = append(table.Entries, Entry{name, canonical})
			taken[name] = true
		}
	}
	if config.AutoDirectories == nil || *config.AutoDirectories {
		for _, entry := range automatic(base) {
			if taken[entry.Name] || isUser(entry.Name) {
				continue
			}
			table.Entries = append(table.Entries, entry)
			taken[entry.Name] = true
		}
	}
	return table, notes
}

type found struct {
	module, set, parent string
	directory           string
}

// automatic finds JVM source roots below base and names each one's package prefix, longest prefixes first, at most autoMax.
func automatic(base string) []Entry {
	var roots []found
	folders := 0
	var walk func(directory string, depth int)
	walk = func(directory string, depth int) {
		if depth > walkDepth || folders >= walkFolders {
			return
		}
		folders++
		entries, err := os.ReadDir(directory)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || skipped[name] {
				continue
			}
			child := filepath.Join(directory, name)
			if name == "src" {
				sets, _ := os.ReadDir(child)
				for _, set := range sets {
					if !set.IsDir() {
						continue
					}
					for _, language := range []string{"java", "kotlin"} {
						root := filepath.Join(child, set.Name(), language)
						if info, err := os.Stat(root); err == nil && info.IsDir() {
							roots = append(roots, found{module: filepath.Base(directory), set: set.Name(), parent: filepath.Base(filepath.Dir(directory)), directory: packagePrefix(root)})
						}
					}
				}
				continue
			}
			walk(child, depth+1)
		}
	}
	walk(base, 0)
	slugify := func(s string) string {
		s = strings.ToLower(s)
		var b strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			} else {
				b.WriteRune('-')
			}
		}
		return strings.Trim(b.String(), "-")
	}
	type candidate struct {
		name, alternative string
		directory         string
		length            int
	}
	var candidates []candidate
	for _, root := range roots {
		relative, err := filepath.Rel(base, root.directory)
		if err != nil || len(relative) < autoMinChars {
			continue
		}
		name := slugify(root.module)
		if root.set != "main" {
			name += "-" + slugify(root.set)
		}
		candidates = append(candidates, candidate{name, slugify(root.parent) + "-" + name, root.directory, len(relative)})
	}
	counts := map[string]int{}
	for _, c := range candidates {
		counts[c.name]++
	}
	for i := range candidates {
		if counts[candidates[i].name] > 1 {
			candidates[i].name = candidates[i].alternative
		}
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return b.length - a.length })
	seen := map[string]bool{}
	var entries []Entry
	for _, c := range candidates {
		if len(entries) == autoMax || seen[c.name] || !valid.MatchString(c.name) {
			continue
		}
		seen[c.name] = true
		canonical, err := filepath.EvalSymlinks(c.directory)
		if err != nil {
			continue
		}
		entries = append(entries, Entry{c.name, canonical})
	}
	return entries
}

// packagePrefix follows directories that hold nothing but one directory, from a source root down to where a package holds more.
func packagePrefix(root string) string {
	directory := root
	for {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 1 || !entries[0].IsDir() {
			return directory
		}
		directory = filepath.Join(directory, entries[0].Name())
	}
}

// Expand turns a leading ~name/ (or a bare ~name) into its directory; ok is false when input names none.
func (t *Table) Expand(input string) (string, bool) {
	if t == nil || !strings.HasPrefix(input, "~") {
		return "", false
	}
	// On Windows a path may come with either separator; nib prints names with a slash.
	name, rest, _ := strings.Cut(filepath.ToSlash(input[1:]), "/")
	for _, entry := range t.Entries {
		if entry.Name == name {
			if rest == "" {
				return entry.Directory, true
			}
			return filepath.Join(entry.Directory, rest), true
		}
	}
	return "", false
}

// Display is canonical under the named directory that prints it shortest, the deepest on a tie; ok is false when none does or when no name
// prints it shorter than it prints plain: relative to Base when inside it, else whole.
func (t *Table) Display(canonical string) (string, bool) {
	if t == nil {
		return "", false
	}
	best := ""
	deepest := -1
	for _, entry := range t.Entries {
		if canonical != entry.Directory && !strings.HasPrefix(canonical, entry.Directory+string(filepath.Separator)) {
			continue
		}
		shown := "~" + entry.Name + canonical[len(entry.Directory):]
		if best == "" || len(shown) < len(best) || (len(shown) == len(best) && len(entry.Directory) > deepest) {
			best, deepest = shown, len(entry.Directory)
		}
	}
	if best == "" || len(best) >= len(plain(t.Base, canonical)) {
		return "", false
	}
	return filepath.ToSlash(best), true
}

// plain is how canonical prints without names: relative to base when strictly below it, else whole.
func plain(base, canonical string) string {
	if base != "" {
		if relative, err := filepath.Rel(base, canonical); err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return relative
		}
	}
	return canonical
}

// Home answers the environment's "HOME name" lookup that tilde expansion makes, for a named directory.
func (t *Table) Home(name string) (string, bool) {
	if t == nil {
		return "", false
	}
	for _, entry := range t.Entries {
		if entry.Name == name {
			return entry.Directory, true
		}
	}
	return "", false
}

// Lines are the table as instruction lines, longest prefixes first, as many as fit in room characters.
func (t *Table) Lines(room int) []string {
	if t == nil {
		return nil
	}
	entries := slices.Clone(t.Entries)
	slices.SortStableFunc(entries, func(a, b Entry) int { return len(b.Directory) - len(a.Directory) })
	var lines []string
	for _, entry := range entries {
		shown := entry.Directory
		if relative, err := filepath.Rel(t.Base, entry.Directory); err == nil && !strings.HasPrefix(relative, "..") {
			shown = relative
		}
		shown = filepath.ToSlash(shown)
		line := fmt.Sprintf("~%s = %s", entry.Name, shown)
		if len(line)+1 > room {
			break
		}
		room -= len(line) + 1
		lines = append(lines, line)
	}
	return lines
}

// Keep restricts the table to the named entries, so a name the model was never told about does not resolve.
func (t *Table) Keep(lines []string) {
	shown := map[string]bool{}
	for _, line := range lines {
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "~"), " ")
		shown[name] = true
	}
	t.Entries = slices.DeleteFunc(t.Entries, func(e Entry) bool { return !shown[e.Name] })
}
