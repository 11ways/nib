package decider

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	DefaultTimeout = 2 * time.Second
)

// Config is the decider the environment asks for.
type Config struct {
	Kind    Kind
	Key     string
	Model   string
	URL     string
	Timeout time.Duration
	// Cache is a directory for answers on disk; "" keeps them in memory only.
	Cache string
	// Client makes the requests; nil uses a plain one. Tests set it.
	Client *http.Client
}

// Parse reads every NIB_DECIDER_* variable through getenv, NIB_SANDBOX included; an error is a startup error.
func Parse(getenv func(string) string) (Config, error) {
	c := Config{Kind: DefaultKind, Model: DefaultModel, URL: DefaultURL, Timeout: DefaultTimeout}
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
	if c.Kind != OpenRouter {
		return c, nil
	}
	c.Key = getenv("NIB_DECIDER_KEY")
	if c.Key == "" {
		c.Key = getenv("OPENROUTER_API_KEY")
	}
	if c.Key == "" {
		return Config{}, errors.New("NIB_DECIDER=openrouter needs a key in NIB_DECIDER_KEY or OPENROUTER_API_KEY")
	}
	if getenv("NIB_SANDBOX") == "1" && getenv("NIB_DECIDER_SANDBOX") != "1" {
		return Config{}, errors.New("NIB_DECIDER=openrouter sends program output to OpenRouter, which NIB_SANDBOX=1 allows only with NIB_DECIDER_SANDBOX=1")
	}
	if model := getenv("NIB_DECIDER_MODEL"); model != "" {
		c.Model = model
	}
	if url := getenv("NIB_DECIDER_URL"); url != "" {
		c.URL = url
	}
	if timeout := getenv("NIB_DECIDER_TIMEOUT"); timeout != "" {
		ms, err := strconv.Atoi(timeout)
		if err != nil || ms <= 0 {
			return Config{}, fmt.Errorf("NIB_DECIDER_TIMEOUT is %q; give a number of milliseconds", timeout)
		}
		c.Timeout = time.Duration(ms) * time.Millisecond
	}
	c.Cache = getenv("NIB_DECIDER_CACHE")
	return c, nil
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
