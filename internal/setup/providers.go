package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/11ways/nib/internal/decider"
	"github.com/11ways/nib/internal/providers"
)

// providerFlag is the server flag that hosts a provider.
const providerFlag = "--provider"

// split separates a nib server command line into the rest of it and the providers it hosts, in order; a malformed --provider stays in the
// rest, so a command setup does not understand reads as stale rather than as a provider.
func split(words []string) (rest []string, hosted []providers.Spec) {
	for i := 0; i < len(words); i++ {
		if value, ok := strings.CutPrefix(words[i], providerFlag+"="); ok {
			if spec, err := providers.ParseSpec(value); err == nil {
				hosted = append(hosted, spec)
				continue
			}
		} else if words[i] == providerFlag && i+1 < len(words) {
			if spec, err := providers.ParseSpec(words[i+1]); err == nil {
				hosted = append(hosted, spec)
				i++
				continue
			}
		}
		rest = append(rest, words[i])
	}
	return rest, hosted
}

// wanted is the command line a nib entry running words should run: command, hosting what words host with changes applied, where a change
// replaces the provider of its name, or with no program stops hosting it, and a new one goes last.
func wanted(command, words []string, changes []providers.Spec) []string {
	_, specs := split(words)
	out := slices.Clone(command)
	for _, spec := range applied(specs, changes) {
		out = append(out, providerFlag, spec.String())
	}
	return out
}

// hostingText names what a command line hosts, for a report.
func hostingText(words []string) string {
	_, hosted := split(words)
	if len(hosted) == 0 {
		return "no provider"
	}
	var names []string
	for _, spec := range hosted {
		names = append(names, spec.String())
	}
	return strings.Join(names, ", ")
}

// ParseChange reads a setup --provider: NAME=PROGRAM to host a provider, or NAME= to stop hosting it.
func ParseChange(text string) (providers.Spec, error) {
	if name, ok := strings.CutSuffix(text, "="); ok && !strings.Contains(name, "=") {
		if _, err := providers.ParseSpec(name + "=x"); err != nil {
			return providers.Spec{}, fmt.Errorf("--provider takes NAME=PROGRAM, or NAME= to stop hosting one, got %q", text)
		}
		return providers.Spec{Name: name}, nil
	}
	return providers.ParseSpec(text)
}

// removeHosted takes out of servers the entries of the providers nib now hosts, so the client does not run them twice.
func removeHosted(servers object, changes []providers.Spec) []change {
	var removed []change
	for _, c := range changes {
		if c.Program == "" || c.Name == "nib" {
			continue
		}
		if _, ok := servers.get(c.Name); ok {
			servers.remove(c.Name)
			removed = append(removed, change{what: "the " + c.Name + " MCP server, now hosted by nib", removed: true})
		}
	}
	return removed
}

// rememberedFile is where setup keeps what a --provider, --decider or --summary asked for, so a later setup without them, as a script runs
// it for a new Claude Code config folder, keeps them too; every nib reads the decider choice from it when it starts.
func rememberedFile(home string, env func(string) string) string {
	config := env("XDG_CONFIG_HOME")
	if config == "" {
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "nib", "setup.json")
}

// saved is what setup.json holds: the providers to host and the decider variables (decider.Remembered) every nib starts with.
type saved struct {
	Providers []providers.Spec
	Decider   map[string]string
}

// remembered is what file holds; nothing when it is missing, and an error when it is not one setup wrote.
func remembered(file string) (saved, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return saved{}, nil
	}
	if err != nil {
		return saved{}, err
	}
	var raw struct {
		Providers []string          `json:"providers"`
		Decider   map[string]string `json:"decider"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return saved{}, fmt.Errorf("%s is not what nib setup writes: %v", file, err)
	}
	out := saved{Decider: map[string]string{}}
	for _, text := range raw.Providers {
		spec, err := providers.ParseSpec(text)
		if err != nil {
			return saved{}, fmt.Errorf("%s: %v", file, err)
		}
		out.Providers = append(out.Providers, spec)
	}
	for name, value := range raw.Decider {
		if !slices.Contains(decider.Remembered, name) {
			return saved{}, fmt.Errorf("%s: %s is not a variable setup remembers", file, name)
		}
		if value != "" {
			out.Decider[name] = value
		}
	}
	return out, nil
}

// RememberedDecider are the decider variables setup.json holds, for a nib starting; a file setup did not write gives none and a note.
func RememberedDecider(home string, env func(string) string) (map[string]string, error) {
	s, err := remembered(rememberedFile(home, env))
	return s.Decider, err
}

// remember writes s to file, through a temporary file and a rename.
func remember(file string, s saved) error {
	texts := []string{}
	for _, spec := range s.Providers {
		texts = append(texts, spec.String())
	}
	out := map[string]any{"providers": texts}
	if len(s.Decider) > 0 {
		out["decider"] = s.Decider
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	staged := file + ".tmp"
	if err := os.WriteFile(staged, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(staged, file)
}

// deciderText names a remembered decider choice, for a report.
func deciderText(vars map[string]string) string {
	if len(vars) == 0 {
		return "no decider choice"
	}
	var parts []string
	for _, name := range decider.Remembered {
		if value := vars[name]; value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, ", ")
}

// applied are the providers kept after changes: each change replaces the one of its name, or with no program drops it; a new one goes last.
func applied(specs, changes []providers.Spec) []providers.Spec {
	specs = slices.Clone(specs)
	for _, c := range changes {
		at := slices.IndexFunc(specs, func(s providers.Spec) bool { return s.Name == c.Name })
		switch {
		case c.Program == "" && at >= 0:
			specs = slices.Delete(specs, at, at+1)
		case c.Program != "" && at >= 0:
			specs[at] = c
		case c.Program != "":
			specs = append(specs, c)
		}
	}
	return specs
}
