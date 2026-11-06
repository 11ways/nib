package server

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type findShell struct {
	fakeShell
	lines  []string
	expect string
	limit  int
	calls  int
}

func (s *findShell) ShapeFind(_ context.Context, lines []string, expect string, limit int) string {
	s.lines, s.expect, s.limit = lines, expect, limit
	s.calls++
	return "selected " + lines[len(lines)-1]
}

func TestFindPassesTheCompleteInventoryAndExpectationToTheShell(t *testing.T) {
	directory := t.TempDir()
	for i := range 1000 {
		file(t, filepath.Join(directory, "src", fmt.Sprintf("f%04d.go", i)), "package p\n")
	}
	shell := &findShell{fakeShell: fakeShell{expects: true}}
	c := connect(t, Options{Directory: directory, Shell: shell}, nil, nil, false)
	result := c.call(t, &mcp.CallToolParams{Name: "find", Arguments: map[string]any{"limit": 20, "expect": "the last file"}})
	if result.IsError || !strings.Contains(text(result), "1000 files\n\nselected src/f0999.go") || shell.limit != 20 || shell.expect != "the last file" || len(shell.lines) != 1000 {
		t.Fatalf("%s; inventory %d, limit %d, expect %s", text(result), len(shell.lines), shell.limit, shell.expect)
	}
	result = c.call(t, &mcp.CallToolParams{Name: "find", Arguments: map[string]any{"limit": 20, "expect": "the last file", "skip": 0}})
	if result.IsError || shell.calls != 1 || !strings.Contains(text(result), "continue with skip 20") {
		t.Fatalf("explicit page: %s; %d shape calls", text(result), shell.calls)
	}
	tools, err := c.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "find" && !strings.Contains(fmt.Sprint(tool.InputSchema), "expect") {
			t.Fatal("find does not advertise expect with a model configured")
		}
	}
}
