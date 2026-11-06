package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func findFixture(t *testing.T, nested bool) *Context {
	t.Helper()
	directory, _ := filepath.EvalSymlinks(t.TempDir())
	for i := range 1000 {
		name := fmt.Sprintf("f%04d.txt", i)
		if nested {
			name = fmt.Sprintf("folder%02d/%s", i/100, name)
		}
		file := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Context{Ctx: context.Background(), Directory: directory, Session: "s"}
}

func TestFindLimitsPresentationWithoutLimitingMatches(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			ctx := findFixture(t, nested)
			for _, limit := range []int{1, 3, 20, 999} {
				out, err := New().Find(ctx, FindArgs{Limit: &limit})
				if err != nil {
					t.Fatal(err)
				}
				parts := strings.Split(out.Text, "\n\n")
				if !strings.HasPrefix(parts[0], "1000 files") || len(strings.Split(parts[1], "\n")) > min(limit, 200) {
					t.Fatalf("limit %d:\n%s", limit, out.Text)
				}
				if limit == 999 && !strings.Contains(out.Text, "limit 999 is over its limit of 200, so 200 was used") {
					t.Fatalf("missing clamp note: %s", out.Text)
				}
				if nested && limit == 20 && !strings.Contains(out.Text, "100 files)") {
					t.Fatalf("expected folder counts: %s", out.Text)
				}
			}
		})
	}
}

func TestFindContinuationCountsFilesNotFolderHeaders(t *testing.T) {
	ctx := findFixture(t, true)
	limit, skip := 20, 95
	out, err := New().Find(ctx, FindArgs{Limit: &limit, Skip: &skip})
	if err != nil {
		t.Fatal(err)
	}
	files := regexp.MustCompile(`f([0-9]{4})\.txt \(`).FindAllStringSubmatch(out.Text, -1)
	next := regexp.MustCompile(`continue with skip ([0-9]+)`).FindStringSubmatch(out.Text)
	if len(files) == 0 || len(next) != 2 {
		t.Fatal(out.Text)
	}
	offset, _ := strconv.Atoi(next[1])
	if offset != skip+len(files) || len(files) >= limit {
		t.Fatalf("headers must use the budget, not the offset: %s", out.Text)
	}
	for i, file := range files {
		if file[1] != fmt.Sprintf("%04d", skip+i) {
			t.Fatalf("missing or repeated file: %s", out.Text)
		}
	}
	page, err := New().Find(ctx, FindArgs{Limit: &limit, Skip: &offset})
	if err != nil || !strings.Contains(page.Text, fmt.Sprintf("f%04d.txt", offset)) || strings.Contains(page.Text, files[len(files)-1][0]) {
		t.Fatalf("continuation: %v\n%s", err, page.Text)
	}
}

func TestFindShapingGetsTheWholeFilteredInventoryAndFallsBack(t *testing.T) {
	ctx := findFixture(t, false)
	limit, calls := 3, 0
	args := FindArgs{Limit: &limit, Glob: []string{"f09*.txt"}}
	plain, err := New().Find(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	args.Shape = func(lines []string, budget int) string {
		calls++
		if len(lines) != 100 || lines[0] != "f0900.txt" || lines[99] != "f0999.txt" || budget != limit {
			t.Fatalf("inventory: %v, budget %d", lines, budget)
		}
		return ""
	}
	out, err := New().Find(ctx, args)
	if err != nil || out.Text != plain.Text || calls != 1 {
		t.Fatalf("fallback: %v\n%s", err, out.Text)
	}
	zero := 0
	args.Skip = &zero
	if _, err := New().Find(ctx, args); err != nil || calls != 1 {
		t.Fatalf("explicit pages must bypass shaping: %v, %d calls", err, calls)
	}
}
