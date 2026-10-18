package decider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestARememberedChoiceAppliesUnlessTheEnvironmentMakesOne(t *testing.T) {
	saved := map[string]string{"NIB_DECIDER": "openrouter", "NIB_DECIDER_SUMMARY": "claude-p:haiku"}
	env := map[string]string{"HOME": "/h"}
	getenv := Layered(func(name string) string { return env[name] }, saved)
	if getenv("NIB_DECIDER") != "openrouter" || getenv("NIB_DECIDER_SUMMARY") != "claude-p:haiku" || getenv("HOME") != "/h" {
		t.Fatal("a remembered choice must fill what the environment leaves unset")
	}
	if getenv("NIB_DECIDER_KEY") != "" {
		t.Fatal("the key is never remembered")
	}
	env["NIB_DECIDER"] = "keywords"
	getenv = Layered(func(name string) string { return env[name] }, saved)
	if getenv("NIB_DECIDER") != "keywords" || getenv("NIB_DECIDER_SUMMARY") != "" {
		t.Fatal("a choice in the environment must replace the remembered one whole")
	}
}

func TestTheKeyIsFoundInNibsFileThenOpencodes(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home, "NIB_DECIDER": "openrouter"}
	getenv := func(name string) string { return env[name] }
	_, err := Parse(getenv)
	want := "NIB_DECIDER=openrouter needs an OpenRouter key; nib looked in NIB_DECIDER_KEY, OPENROUTER_API_KEY, " +
		filepath.Join(home, ".config", "nib", "openrouter-key") + ", the openrouter entry of " + filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	auth := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	write(t, auth, `{"openai": {"type": "oauth"}, "openrouter": {"type": "api", "key": "from-opencode"}}`, 0o600)
	if c, err := Parse(getenv); err != nil || c.Key != "from-opencode" {
		t.Fatalf("%v %v", c.Key, err)
	}
	file := filepath.Join(home, ".config", "nib", "openrouter-key")
	write(t, file, "from-file\n", 0o600)
	if c, err := Parse(getenv); err != nil || c.Key != "from-file" {
		t.Fatalf("%v %v", c.Key, err)
	}
	env["NIB_DECIDER_KEY"] = "from-env"
	if c, err := Parse(getenv); err != nil || c.Key != "from-env" {
		t.Fatalf("%v %v", c.Key, err)
	}
	// XDG_DATA_HOME moves OpenCode's file, and a keywords decider never needs a key.
	delete(env, "NIB_DECIDER_KEY")
	os.Remove(file)
	env["XDG_DATA_HOME"] = filepath.Join(home, "data")
	if _, err := Parse(getenv); err == nil || !strings.Contains(err.Error(), filepath.Join(home, "data", "opencode", "auth.json")) {
		t.Fatalf("%v", err)
	}
	env["NIB_DECIDER"] = "keywords"
	if _, err := Parse(getenv); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, file, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}
