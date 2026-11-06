package nibash

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/decider"
)

func findInventory() []string {
	lines := make([]string, 1000)
	for i := range lines {
		lines[i] = fmt.Sprintf("src/file-%04d.go", i)
	}
	return lines
}

func TestFindSelectsBeyondTheFirstPageAndRetainsAllPaths(t *testing.T) {
	f := newFixture(t, nil)
	f.shell.logs.path = t.TempDir()
	f.shell.Deciding(pickText("file-0900"))
	lines := findInventory()
	out := f.shell.ShapeFind(context.Background(), lines, "find file 900", 20)
	if !strings.Contains(out, "901|src/file-0900.go") || !strings.Contains(out, "1 rows shown of 1,000 log lines") || !strings.Contains(out, "[find:") {
		t.Fatal(out)
	}
	files, _ := filepath.Glob(filepath.Join(f.shell.logs.path, "*.log"))
	if len(files) != 1 {
		t.Fatalf("logs %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || string(raw) != strings.Join(lines, "\n") || !strings.Contains(out, files[0]) {
		t.Fatalf("inventory retrieval: %v\n%s", err, out)
	}
}

func TestFindSummaryUsesItsBudgetAndExactInventory(t *testing.T) {
	f := newFixture(t, nil)
	f.shell.logs.path = t.TempDir()
	model := pickText("file-0900")
	summary := &fakeSummarizer{summarize: func(in decider.SummaryInput) decider.Summary {
		if len(in.Log) != 1000 || in.Expect != "locate the relevant files" || in.Log[900] != "src/file-0900.go" {
			t.Fatalf("summary input: %+v", in)
		}
		return decider.Summary{Text: "The relevant files start at file-0900.go.", Ranges: [][2]int{{901, 950}}, Who: "fake"}
	}}
	f.shell.Deciding(model).Summarizing(summary)
	for _, limit := range []int{1, 20} {
		out := f.shell.ShapeFind(context.Background(), findInventory(), "locate the relevant files", limit)
		if !strings.HasPrefix(out, "<summary>") || !strings.Contains(out, "901|src/file-0900.go") || strings.Count(out, "\n") > limit+4 {
			t.Fatalf("limit %d:\n%s", limit, out)
		}
	}
	if len(summary.asked) != 2 {
		t.Fatalf("summary calls %d", len(summary.asked))
	}
	// A failed summary may still use the decision model's real paths.
	summary.summarize = func(decider.SummaryInput) decider.Summary { return decider.Summary{Failure: "offline"} }
	if out := f.shell.ShapeFind(context.Background(), findInventory(), "locate the relevant files", 20); strings.Contains(out, "<summary>") || !strings.Contains(out, "901|src/file-0900.go") {
		t.Fatal(out)
	}
	// When both models fail, the caller keeps its deterministic overview.
	model.choose = func(decider.Input) decider.Choice { return decider.Choice{Failure: "offline"} }
	if out := f.shell.ShapeFind(context.Background(), findInventory(), "locate the relevant files", 20); out != "" {
		t.Fatalf("expected fallback, got %s", out)
	}
	// Turning summaries off prevents further summary calls.
	f.shell.SetSummaries(false)
	asked := len(summary.asked)
	f.shell.ShapeFind(context.Background(), findInventory(), "locate the relevant files", 20)
	if len(summary.asked) != asked {
		t.Fatal("summary called while disabled")
	}
}

func TestFindKeepsSmallInventoriesAndAllRequiredFilesDeterministic(t *testing.T) {
	f := newFixture(t, nil)
	f.shell.logs.path = t.TempDir()
	model := pickText("file")
	f.shell.Deciding(model)
	if got := f.shell.ShapeFind(context.Background(), []string{"a.go"}, "locate it", 20); got != "" || len(model.asked) != 0 {
		t.Fatalf("small listing: %q", got)
	}
	model.choose = func(decider.Input) decider.Choice { return decider.Choice{Model: true, Judged: true, Whole: 1} }
	if got := f.shell.ShapeFind(context.Background(), findInventory(), "list every file", 20); got != "" {
		t.Fatalf("selection would omit required files: %s", got)
	}
}
