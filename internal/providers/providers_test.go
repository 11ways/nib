package providers

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/providers/providertest"
)

func TestMain(m *testing.M) {
	providertest.Serve()
	os.Exit(m.Run())
}

func start(t *testing.T) *Provider {
	t.Helper()
	t.Setenv(providertest.Env, "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Start(context.Background(), Spec{"fake", self}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestParseSpec(t *testing.T) {
	if spec, err := ParseSpec("zemble=/usr/bin/zemble"); err != nil || spec != (Spec{"zemble", "/usr/bin/zemble"}) || spec.String() != "zemble=/usr/bin/zemble" {
		t.Fatalf("%v %v", spec, err)
	}
	for _, bad := range []string{"zemble", "=x", "Zemble=x", "ze-mble=x", "zemble="} {
		if _, err := ParseSpec(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestStartListsTheToolsNibCanOffer(t *testing.T) {
	p := start(t)
	var names []string
	for _, tool := range p.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "crash,fails,future,locate,pid,plain,slow" {
		t.Fatalf("tools %v", names)
	}
	if len(p.Skipped) != 2 || !strings.Contains(strings.Join(p.Skipped, "\n"), "Shouting: its name") || !strings.Contains(strings.Join(p.Skipped, "\n"), "wordy: its description has 2500 characters") {
		t.Fatalf("skipped %v", p.Skipped)
	}
}

func TestAProviderThatEndedStartsAgainOnTheNextCall(t *testing.T) {
	p := start(t)
	ctx := context.Background()
	first, err := p.Call(ctx, "pid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Call(ctx, "crash", nil); err == nil || !strings.Contains(err.Error(), "fake ended during the call; it starts again on the next one, and this call was not sent again") {
		t.Fatalf("got %v", err)
	}
	second, err := p.Call(ctx, "pid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if Text(first) == Text(second) || Text(second) == "" {
		t.Fatalf("expected a new process, got %s then %s", Text(first), Text(second))
	}
}

func TestPiecesReadOnlyTheVersionAndShapesNibKnows(t *testing.T) {
	p := start(t)
	call := func(parts string) ([]engine.Piece, bool) {
		t.Helper()
		result, err := p.Call(context.Background(), "locate", json.RawMessage(`{"parts":`+parts+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return Pieces(result)
	}
	pieces, ok := call(`[{"text":"hit"},{"path":"/a.py","start":2,"end":3,"indexed":"x\ny"}]`)
	if !ok || len(pieces) != 2 || pieces[0].Text != "hit" || pieces[1].Path != "/a.py" || pieces[1].Start != 2 || pieces[1].End != 3 || *pieces[1].Indexed != "x\ny" {
		t.Fatalf("%+v %v", pieces, ok)
	}
	for _, bad := range []string{
		`[{"text":"hit","path":"/a.py"}]`,
		`[{"path":"a.py","start":1,"end":1}]`,
		`[{"path":"/a.py","start":0,"end":1}]`,
		`[{"path":"/a.py","start":3,"end":2}]`,
		`[{"path":"/a.py","start":1}]`,
		`[{"path":"/a.py","start":1,"end":2,"indexed":"one line"}]`,
		`[{"tag":"ABCD"}]`,
		`null`,
	} {
		if _, ok := call(bad); ok {
			t.Errorf("%s must not be read", bad)
		}
	}
	for _, tool := range []string{"future", "plain"} {
		result, err := p.Call(context.Background(), tool, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := Pieces(result); ok {
			t.Errorf("%s's answer must not be read", tool)
		}
	}
	if _, ok := Pieces(&mcp.CallToolResult{StructuredContent: map[string]any{"nib": 1, "parts": []any{}, "more": true}}); ok {
		t.Error("an unknown key must not be read")
	}
}
