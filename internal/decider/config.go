package decider

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/11ways/nib/internal/opencode"
)

// Kind names a decider, as NIB_DECIDER spells it.
type Kind string

const (
	Fixed      Kind = "fixed"
	Keywords   Kind = "keywords"
	OpenRouter Kind = "openrouter"
)

// kindFacts are what each kind shows the model, in the words of the nibash tool description; the only list of kinds.
var kindFacts = []struct {
	kind Kind
	cut  string
}{
	{Fixed, "long output cut to its start and end"},
	{Keywords, "long output cut to its start, its end and the lines between that name errors or failures"},
	{OpenRouter, "long output cut to its start, its end and the parts between that a decision model on OpenRouter judges your description needs (keywords when it fails)"},
}

// Kinds lists every kind, in order.
func Kinds() []Kind {
	var kinds []Kind
	for _, f := range kindFacts {
		kinds = append(kinds, f.kind)
	}
	return kinds
}

// Cut is how output of this kind is cut, for the tool description; "" for an unknown kind.
func (k Kind) Cut() string {
	for _, f := range kindFacts {
		if f.kind == k {
			return f.cut
		}
	}
	return ""
}

const (
	DefaultKind    = Keywords
	DefaultModel   = "typesafe/jev-1.14"
	DefaultURL     = "https://openrouter.ai/api/alpha/decisions"
	DefaultTimeout = 30 * time.Second
)

// Config is the decider the environment asks for.
type Config struct {
	Kind Kind
	Key  string
	// Models are the decision models in order of preference; the next one answers when one fails.
	Models  []string
	URL     string
	Timeout time.Duration
	// Cache is a directory for answers on disk; "" keeps them in memory only.
	Cache string
	// Client makes the requests; nil uses a plain one. Tests set it.
	Client *http.Client
	// Summary are the summarizers in order of preference; none turns summaries off.
	Summary        []SummaryEntry
	SummaryURL     string
	SummaryTimeout time.Duration
	// OpenCode is the OpenCode 2 program opencode: summarizers run: NIB_DECIDER_OPENCODE, else opencode on PATH.
	OpenCode string
	// logins are where opencode: summarizers find a provider's login.
	logins logins
}

// Parse reads every NIB_DECIDER_* variable through getenv, NIB_SANDBOX included; an error is a startup error.
func Parse(getenv func(string) string) (Config, error) {
	c := Config{Kind: DefaultKind, Models: []string{DefaultModel}, URL: DefaultURL, Timeout: DefaultTimeout, SummaryURL: DefaultSummaryURL,
		SummaryTimeout: DefaultSummaryTimeout}
	if name := getenv("NIB_DECIDER"); name != "" {
		c.Kind = Kind(name)
		if c.Kind.Cut() == "" {
			var names []string
			for _, k := range Kinds() {
				names = append(names, string(k))
			}
			return Config{}, fmt.Errorf("NIB_DECIDER is %q; use one of %s", name, strings.Join(names, ", "))
		}
	}
	// The key is looked for only when something needs it, so a keywords setup never reads a key file.
	needKey := func(what string) (string, error) {
		key, looked, err := findKey(getenv)
		if err != nil {
			return "", err
		}
		if key == "" {
			return "", fmt.Errorf("%s needs an OpenRouter key; nib looked in %s", what, looked)
		}
		return key, nil
	}
	c.Cache = getenv("NIB_DECIDER_CACHE")
	if list := getenv("NIB_DECIDER_SUMMARY"); list != "" {
		if c.Kind == Fixed {
			return Config{}, errors.New("NIB_DECIDER_SUMMARY reads the passages a decider picks, which NIB_DECIDER=fixed does not; use keywords or openrouter")
		}
		entries, err := parseSummary(list)
		if err != nil {
			return Config{}, err
		}
		c.Summary = entries
		if slices.ContainsFunc(entries, func(e SummaryEntry) bool { return e.Program == "" }) {
			if c.Key, err = needKey("NIB_DECIDER_SUMMARY names an OpenRouter model, which"); err != nil {
				return Config{}, err
			}
		}
		c.OpenCode = getenv("NIB_DECIDER_OPENCODE")
		if c.OpenCode == "" {
			c.OpenCode = "opencode"
		}
		c.logins = logins{databases: opencode.Databases(getenv), auth: opencodeAuth(getenv)}
		if url := getenv("NIB_DECIDER_SUMMARY_URL"); url != "" {
			c.SummaryURL = url
		}
		if c.SummaryTimeout, err = milliseconds(getenv, "NIB_DECIDER_SUMMARY_TIMEOUT", c.SummaryTimeout); err != nil {
			return Config{}, err
		}
	}
	if c.Kind != OpenRouter {
		return c, nil
	}
	var err error
	if c.Key == "" {
		if c.Key, err = needKey("NIB_DECIDER=openrouter"); err != nil {
			return Config{}, err
		}
	}
	if getenv("NIB_SANDBOX") == "1" && getenv("NIB_DECIDER_SANDBOX") != "1" {
		return Config{}, errors.New("NIB_DECIDER=openrouter sends program output to OpenRouter, which NIB_SANDBOX=1 allows only with NIB_DECIDER_SANDBOX=1")
	}
	if models := getenv("NIB_DECIDER_MODEL"); models != "" {
		c.Models = nil
		for _, m := range strings.Split(models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.Models = append(c.Models, m)
			}
		}
		if len(c.Models) == 0 {
			return Config{}, fmt.Errorf("NIB_DECIDER_MODEL is %q; give one or more model ids, separated by commas", models)
		}
	}
	if url := getenv("NIB_DECIDER_URL"); url != "" {
		c.URL = url
	}
	if c.Timeout, err = milliseconds(getenv, "NIB_DECIDER_TIMEOUT", c.Timeout); err != nil {
		return Config{}, err
	}
	return c, nil
}

// milliseconds reads a variable holding a number of milliseconds; unset keeps otherwise.
func milliseconds(getenv func(string) string, name string, otherwise time.Duration) (time.Duration, error) {
	value := getenv(name)
	if value == "" {
		return otherwise, nil
	}
	ms, err := strconv.Atoi(value)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("%s is %q; give a number of milliseconds", name, value)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// New is the decider c describes; nil for the fixed cut.
func (c Config) New() Decider {
	switch c.Kind {
	case Keywords:
		return keywords{}
	case OpenRouter:
		return newOpenRouter(c)
	}
	return nil
}
