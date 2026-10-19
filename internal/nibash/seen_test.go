package nibash

import (
	"fmt"
	"strings"
	"testing"

	"github.com/11ways/nib/internal/engine"
)

// A program the agent runs, a formatter say, makes a new version of a file it saw: lines that kept their number and text stay anchors, a
// line it moved needs a ~ check, and a line it changed needs a read.
func TestLinesSeenStayAnchorsAfterACommandRewritesTheFile(t *testing.T) {
	var lines []string
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	f := newFixture(t, map[string]string{"a.txt": strings.Join(lines, "\n") + "\n"})
	f.run("cat a.txt")
	tag := tagOf(t, f.run("sed -i 's/^line 10$/LINE 10/' a.txt"))
	result, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT >5:\n+inserted", tag))
	if err != nil {
		t.Fatalf("a line the command left in place must stay an anchor: %v", err)
	}
	tag = tagOf(t, result.Text)
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 11:\n+changed", tag)); err == nil || !strings.Contains(err.Error(), "never displayed") {
		t.Fatalf("a line the command changed must be refused, got %v", err)
	}
	tag = tagOf(t, f.run("sed -i '1i top' a.txt"))
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 4:\n+moved", tag)); err == nil || !strings.Contains(err.Error(), "lines 4 of a.txt, which moved since you saw or wrote them") {
		t.Fatalf("a line the command moved must need a ~ check, got %v", err)
	}
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT 4:\n~line 3\n+LINE 3", tag)); err != nil {
		t.Fatalf("a moved line the agent saw must anchor with a ~ check: %v", err)
	}
}

// A replaced line the agent only saw cut comes whole in the refusal, so sending the patch again applies it; one too long to show stays a
// preview that asks for a read.
func TestARefusalShowsACutLineWholeSoTheResendApplies(t *testing.T) {
	long := strings.Repeat("x", 2500)
	huge := strings.Repeat("y", 9000)
	f := newFixture(t, map[string]string{"a.txt": "one\n" + long + "\nthree\n" + huge + "\n"})
	read, err := f.engine.Read(f.tools(), engine.ReadArgs{Paths: []string{"a.txt:1-"}})
	if err != nil {
		t.Fatal(err)
	}
	tag := tagOf(t, read.Text)
	patch := fmt.Sprintf("[a.txt#%s]\nPUT 2:\n+short", tag)
	_, err = f.engine.Patch(f.tools(), patch)
	if err == nil || !strings.Contains(err.Error(), "*2:"+long+"\n") {
		t.Fatalf("the refusal must show the cut line whole, got %v", err)
	}
	if _, err := f.engine.Patch(f.tools(), patch); err != nil {
		t.Fatalf("the same patch must apply once the refusal showed the line: %v", err)
	}
	read, err = f.engine.Read(f.tools(), engine.ReadArgs{Paths: []string{"a.txt:1"}})
	if err != nil {
		t.Fatal(err)
	}
	patch = fmt.Sprintf("[a.txt#%s]\nPUT 4:\n+short", tagOf(t, read.Text))
	_, err = f.engine.Patch(f.tools(), patch)
	if err == nil || strings.Contains(err.Error(), "4:"+huge) || !strings.Contains(err.Error(), "Read those lines in full") {
		t.Fatalf("a line past the budget must stay a preview, got %v", err)
	}
	if _, err := f.engine.Patch(f.tools(), patch); err == nil {
		t.Fatal("a line never shown whole must still be refused")
	}
}

// An insert next to a cut line keeps it, so a ~ check of its start is all it needs.
func TestAnInsertBesideACutLineNeedsOnlyACheck(t *testing.T) {
	long := strings.Repeat("z", 2500)
	f := newFixture(t, map[string]string{"a.txt": "one\n" + long + "\nthree\n"})
	read, err := f.engine.Read(f.tools(), engine.ReadArgs{Paths: []string{"a.txt:1-"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Patch(f.tools(), fmt.Sprintf("[a.txt#%s]\nPUT >2:\n~zzzz\n+after", tagOf(t, read.Text))); err != nil {
		t.Fatalf("an insert after a cut line with a ~ check must apply: %v", err)
	}
}
