package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordingConn keeps what it is asked to write.
type recordingConn struct {
	mcp.Connection
	written []jsonrpc.Message
}

func (c *recordingConn) Write(_ context.Context, msg jsonrpc.Message) error {
	c.written = append(c.written, msg)
	return nil
}

// A reply over the limit, which once made Claude Code drop the whole server, becomes an error for its one request.
func TestAnOversizedReplyBecomesAnErrorForItsRequest(t *testing.T) {
	inner := &recordingConn{}
	conn := boundedConn{inner, 1024}
	id, _ := jsonrpc.MakeID(float64(7))
	small, _ := json.Marshal(map[string]string{"text": "ok"})
	if err := conn.Write(context.Background(), &jsonrpc.Response{ID: id, Result: small}); err != nil {
		t.Fatal(err)
	}
	large, _ := json.Marshal(map[string]string{"text": strings.Repeat("a", 4096)})
	if err := conn.Write(context.Background(), &jsonrpc.Response{ID: id, Result: large}); err != nil {
		t.Fatal(err)
	}
	if len(inner.written) != 2 {
		t.Fatalf("written: %d", len(inner.written))
	}
	if r := inner.written[0].(*jsonrpc.Response); string(r.Result) != string(small) {
		t.Fatalf("small reply changed: %s", r.Result)
	}
	r := inner.written[1].(*jsonrpc.Response)
	wire, ok := r.Error.(*jsonrpc.Error)
	if r.ID != id || r.Result != nil || !ok || !strings.Contains(wire.Message, "over the 1024 it sends") {
		t.Fatalf("oversized reply: %+v", r)
	}
	if err := conn.Write(context.Background(), &jsonrpc.Request{Method: channelMethod, Params: large}); err == nil || len(inner.written) != 2 {
		t.Fatalf("an oversized notification was written: %v", err)
	}
}
