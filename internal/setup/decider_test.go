package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
)

func TestSetupRemembersADeciderChoiceEveryNibReads(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "project")
	env := map[string]string{"NIB_DECIDER_KEY": "k"}
	run := func(choice map[string]string) []string {
		return Setup(Options{Directory: directory, Command: nib, Home: home, Env: func(name string) string { return env[name] }, Decider: choice,
			Claude: func(string, []string) (int, string) { return 0, "" }})
	}
	vars, err := decider.DeciderVars("openrouter:typesafe/jev-1.14,perplexity/pplx-decider-v1.2")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range decider.SummaryVars("deepseek/deepseek-v4.1-flash-1017") {
		vars[name] = value
	}
	report := run(vars)
	equal(t, report[0], "~/.config/nib/setup.json: remembers NIB_DECIDER=openrouter, NIB_DECIDER_MODEL=typesafe/jev-1.14,perplexity/pplx-decider-v1.2, "+
		"NIB_DECIDER_SUMMARY=deepseek/deepseek-v4.1-flash-1017 for every nib started")
	data, err := os.ReadFile(filepath.Join(home, ".config", "nib", "setup.json"))
	if err != nil || strings.Contains(string(data), `"k"`) {
		t.Fatalf("the key must never be remembered: %s %v", data, err)
	}
	saved, err := RememberedDecider(home, func(string) string { return "" })
	equal(t, err, nil)
	equal(t, saved, map[string]string{"NIB_DECIDER": "openrouter", "NIB_DECIDER_MODEL": "typesafe/jev-1.14,perplexity/pplx-decider-v1.2",
		"NIB_DECIDER_SUMMARY": "deepseek/deepseek-v4.1-flash-1017"})
	// A later setup without the flags keeps the choice and says nothing of it.
	if strings.Contains(strings.Join(run(nil), "\n"), "setup.json") {
		t.Fatal("an unchanged choice must not be reported")
	}
	// --decider=keywords drops the models, and --summary= forgets the summarizers.
	vars, _ = decider.DeciderVars("keywords")
	for name, value := range decider.SummaryVars("") {
		vars[name] = value
	}
	run(vars)
	saved, _ = RememberedDecider(home, func(string) string { return "" })
	equal(t, saved, map[string]string{"NIB_DECIDER": "keywords"})
	// A choice nib would refuse to start with is not remembered.
	delete(env, "NIB_DECIDER_KEY")
	vars, _ = decider.DeciderVars("openrouter")
	report = run(vars)
	if !strings.Contains(report[0], "not remembered") || !strings.Contains(report[0], "needs an OpenRouter key") {
		t.Fatalf("%q", report[0])
	}
	saved, _ = RememberedDecider(home, func(string) string { return "" })
	equal(t, saved, map[string]string{"NIB_DECIDER": "keywords"})
}

func TestDeciderFlagsAreChecked(t *testing.T) {
	for _, bad := range []string{"smart", "keywords:x", "Keywords"} {
		if _, err := decider.DeciderVars(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	vars, err := decider.DeciderVars("")
	equal(t, err, nil)
	equal(t, vars, map[string]string{"NIB_DECIDER": "", "NIB_DECIDER_MODEL": ""})
}
