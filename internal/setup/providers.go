package setup

import (
	"fmt"
	"slices"
	"strings"

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
	out := slices.Clone(command)
	for _, spec := range specs {
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
