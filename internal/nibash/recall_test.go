package nibash

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/11ways/nib/internal/server"
)

// failure is a go test failure block of n lines, its line changed at changed (1-based) when changed > 0.
func failure(n, changed int) string {
	var lines []string
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("    names_test.go:%d: got notes for ~bewidgets, want %d", 60+i, i)
		if i == changed {
			text = "    names_test.go: this line changed since"
		}
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n") + "\n"
}

var foldMarkerRow = regexp.MustCompile(`^\[nibash: the same (\d+) rows from ` + "`" + `([^` + "`" + `]*)` + "`" + ` your (previous call|call \d+ calls ago) showed; log lines (\d+)-(\d+) in (\S+)\]$`)

func TestABlockAnEarlierCallShowedFoldsIntoOneMarker(t *testing.T) {
	f := newFixture(t, map[string]string{"fail.txt": failure(15, 0), "changed.txt": failure(15, 8), "short.txt": failure(4, 0)})
	first := f.run("awk 1 fail.txt; echo first")
	if !strings.HasPrefix(first, "    names_test.go:61:") || strings.Contains(first, "the same") {
		t.Fatalf("the first call shows it all: %s", first)
	}
	second := f.run("echo second; awk 1 fail.txt")
	rows := strings.Split(second, "\n")
	if len(rows) != 2 || rows[0] != "second" {
		t.Fatalf("got %s", second)
	}
	m := foldMarkerRow.FindStringSubmatch(rows[1])
	if m == nil || m[1] != "15" || !strings.HasPrefix(m[2], "    names_test.go:61:") || m[3] != "previous call" || m[4] != "2" || m[5] != "16" {
		t.Fatalf("got %s", second)
	}
	// The log holds the lines the marker names.
	data, err := os.ReadFile(m[6])
	if log := string(data); err != nil || strings.Join(strings.Split(log, "\n")[1:16], "\n")+"\n" != failure(15, 0) {
		t.Fatalf("log %q", log)
	}
	// A changed line prints; only identical runs fold.
	changed := f.run("awk 1 changed.txt")
	if !strings.Contains(changed, "\n    names_test.go: this line changed since\n") || strings.Count(changed, "[nibash: the same 7 rows") != 2 {
		t.Fatalf("got %s", changed)
	}
	// A short block is cheaper to print again than to name.
	f.run("awk 1 short.txt")
	if again := f.run("awk 1 short.txt"); again != strings.TrimSuffix(failure(4, 0), "\n") {
		t.Fatalf("got %s", again)
	}
}

func TestAFoldNeedsTheSameAgentAndItsContextSinceTheEarlierCall(t *testing.T) {
	f := newFixture(t, map[string]string{"fail.txt": failure(15, 0)})
	f.run("awk 1 fail.txt")
	// Another agent never saw it.
	other := f.tools()
	other.Session = "other"
	result, err := f.shell.Run(t.Context(), "other", server.ShellCall{Command: "awk 1 fail.txt"}, other)
	if err != nil || strings.Contains(result.Text, "the same") {
		t.Fatalf("another agent: %v %s", err, result.Text)
	}
	// After the agent's context was dropped, the earlier call is gone from it.
	f.engine.Store.Forget("s", time.Now())
	if out := f.run("awk 1 fail.txt"); strings.Contains(out, "the same") {
		t.Fatalf("after a drop: %s", out)
	}
	// The call after that one folds again, against it.
	if out := f.run("awk 1 fail.txt"); !strings.HasPrefix(out, "[nibash: the same 15 rows from `    names_test.go:61:") || !strings.Contains(out, "` your previous call showed; ") {
		t.Fatalf("got %s", out)
	}
}

func TestAFailedCallFoldsOnlyALongBlock(t *testing.T) {
	long := strings.ReplaceAll(failure(25, 0), "names_test", "store_test")
	f := newFixture(t, map[string]string{"fail.txt": failure(15, 0), "long.txt": long})
	f.run("awk 1 fail.txt long.txt")
	// An error of 15 rows shows again whole when the call fails; 25 rows fold.
	if out := f.run("awk 1 fail.txt; false"); strings.Contains(out, "the same") || !strings.HasSuffix(out, "want 15\n[exit 1]") {
		t.Fatalf("short block: %s", out)
	}
	if out := f.run("awk 1 long.txt; false"); !strings.HasPrefix(out, "[nibash: the same 25 rows from ") {
		t.Fatalf("long block: %s", out)
	}
}

// A pipeline whose first stage failed is a failed call for the fold too, though its last stage set its status.
func TestAPipelineWhoseStageFailedFoldsOnlyALongBlock(t *testing.T) {
	f := newFixture(t, map[string]string{"fail.txt": failure(15, 0)})
	f.run("awk 1 fail.txt")
	if out := f.run("sh -c 'cat fail.txt; exit 1' | cat"); strings.Contains(out, "the same") || !strings.Contains(out, "want 15\n[nibash: sh -c 'cat fail.txt; exit 1' exited 1 before | cat]") {
		t.Fatalf("got %s", out)
	}
}
