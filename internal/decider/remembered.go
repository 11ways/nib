package decider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/11ways/nib/internal/opencode"
	"github.com/11ways/nib/internal/platform"
)

// Remembered are the variables nib setup may keep for every nib it registers; the key is never one of them.
var Remembered = []string{"NIB_DECIDER", "NIB_DECIDER_MODEL", "NIB_DECIDER_SUMMARY", "NIB_DECIDER_OPENCODE"}

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

// OpenCodeVars are the variables setup's --opencode stands for: the OpenCode 2 program opencode: summarizers run; "" forgets it.
func OpenCodeVars(program string) map[string]string {
	return map[string]string{"NIB_DECIDER_OPENCODE": program}
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
		if login, ok := readOpencodeLogin(file, "openrouter"); ok && login.Kind == "key" {
			return login.Key, "", nil
		}
	}
	return "", strings.Join(places, ", "), nil
}

// keyFile is nib's own key file, in $XDG_CONFIG_HOME/nib or ~/.config/nib; "" without a home.
func keyFile(getenv func(string) string) string {
	config := platform.ConfigHome(getenv)
	if config == "" {
		return ""
	}
	return filepath.Join(config, "nib", "openrouter-key")
}

// readKeyFile is the key file's first line; a missing file is no key.
func readKeyFile(file string) (string, error) {
	if _, err := os.Stat(file); err != nil {
		return "", nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %v", file, err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(first), nil
}

// opencodeAuth is where OpenCode keeps its keys: $XDG_DATA_HOME/opencode/auth.json or ~/.local/share/opencode/auth.json, on every OS, as
// OpenCode finds its data folder the XDG way everywhere.
func opencodeAuth(getenv func(string) string) string {
	data := platform.DataHome(getenv)
	if data == "" {
		return ""
	}
	return filepath.Join(data, "opencode", "auth.json")
}

// readOpencodeLogin is provider's entry of an auth.json, in OpenCode 2's words: an api entry's key and a wellknown entry's token are "key"
// logins, an oauth entry an "oauth" one; anything unexpected in the file is no login.
func readOpencodeLogin(file, provider string) (opencode.Login, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return opencode.Login{}, false
	}
	var auth map[string]struct {
		Type  string `json:"type"`
		Key   string `json:"key"`
		Token string `json:"token"`
	}
	if json.Unmarshal(data, &auth) != nil {
		return opencode.Login{}, false
	}
	entry, ok := auth[provider]
	switch {
	case !ok:
		return opencode.Login{}, false
	case entry.Type == "api" && strings.TrimSpace(entry.Key) != "":
		return opencode.Login{Kind: "key", Key: strings.TrimSpace(entry.Key)}, true
	case entry.Type == "wellknown" && strings.TrimSpace(entry.Token) != "":
		return opencode.Login{Kind: "key", Key: strings.TrimSpace(entry.Token)}, true
	case entry.Type == "oauth":
		return opencode.Login{Kind: "oauth"}, true
	}
	return opencode.Login{}, false
}
