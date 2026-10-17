package decider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Remembered are the variables nib setup may keep for every nib it registers; the key is never one of them.
var Remembered = []string{"NIB_DECIDER", "NIB_DECIDER_MODEL", "NIB_DECIDER_SUMMARY"}

// Layered reads the environment, falling back to saved for Remembered; when the environment sets any of them, saved is ignored whole, so a
// choice made in a server's env never mixes with a remembered one.
func Layered(getenv func(string) string, saved map[string]string) func(string) string {
	for _, name := range Remembered {
		if getenv(name) != "" {
			return getenv
		}
	}
	return func(name string) string {
		if value := getenv(name); value != "" || !slices.Contains(Remembered, name) {
			return value
		}
		return saved[name]
	}
}

// DeciderVars are the variables setup's --decider stands for: KIND, or openrouter:MODEL,... for decision models in order of preference; ""
// forgets the choice, which gives every variable an empty value.
func DeciderVars(text string) (map[string]string, error) {
	kind, models, _ := strings.Cut(text, ":")
	vars := map[string]string{"NIB_DECIDER": kind, "NIB_DECIDER_MODEL": models}
	if text == "" {
		return vars, nil
	}
	if Kind(kind).Cut() == "" {
		var names []string
		for _, k := range Kinds() {
			names = append(names, string(k))
		}
		return nil, fmt.Errorf("--decider takes one of %s, or openrouter:MODEL,... for its models; got %q", strings.Join(names, ", "), text)
	}
	if models != "" && Kind(kind) != OpenRouter {
		return nil, fmt.Errorf("--decider %s takes no models; only openrouter:MODEL,... does", kind)
	}
	return vars, nil
}

// SummaryVars are the variables setup's --summary stands for; "" forgets the summarizers.
func SummaryVars(text string) map[string]string {
	return map[string]string{"NIB_DECIDER_SUMMARY": text}
}

// findKey is the OpenRouter key: from NIB_DECIDER_KEY or OPENROUTER_API_KEY, else the key file in nib's config folder, else OpenCode's
// openrouter entry in its auth.json, read only; looked names where it searched, for an error.
func findKey(getenv func(string) string) (key, looked string, err error) {
	places := []string{"NIB_DECIDER_KEY", "OPENROUTER_API_KEY"}
	for _, name := range places {
		if key = getenv(name); key != "" {
			return key, "", nil
		}
	}
	if file := keyFile(getenv); file != "" {
		places = append(places, file)
		key, err = readKeyFile(file)
		if key != "" || err != nil {
			return key, "", err
		}
	}
	if file := opencodeAuth(getenv); file != "" {
		places = append(places, "the openrouter entry of "+file)
		if key = readOpencodeKey(file); key != "" {
			return key, "", nil
		}
	}
	return "", strings.Join(places, ", "), nil
}

// keyFile is nib's own key file, in $XDG_CONFIG_HOME/nib or ~/.config/nib; "" without a home.
func keyFile(getenv func(string) string) string {
	config := getenv("XDG_CONFIG_HOME")
	if config == "" {
		if home := getenv("HOME"); home != "" {
			config = filepath.Join(home, ".config")
		}
	}
	if config == "" {
		return ""
	}
	return filepath.Join(config, "nib", "openrouter-key")
}

// readKeyFile is the key file's first line; a missing file is no key, and one others may read is refused, since the key is a secret.
func readKeyFile(file string) (string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return "", nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s holds the OpenRouter key but others may read it; chmod 600 it", file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %v", file, err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(first), nil
}

// opencodeAuth is where OpenCode keeps its keys: $XDG_DATA_HOME/opencode/auth.json or ~/.local/share/opencode/auth.json.
func opencodeAuth(getenv func(string) string) string {
	data := getenv("XDG_DATA_HOME")
	if data == "" {
		if home := getenv("HOME"); home != "" {
			data = filepath.Join(home, ".local", "share")
		}
	}
	if data == "" {
		return ""
	}
	return filepath.Join(data, "opencode", "auth.json")
}

// readOpencodeKey is the key of auth.json's openrouter entry; anything unexpected in the file is no key.
func readOpencodeKey(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	var auth map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return ""
	}
	entry := auth["openrouter"]
	if entry.Type != "api" {
		return ""
	}
	return strings.TrimSpace(entry.Key)
}
