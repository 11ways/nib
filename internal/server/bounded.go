package server

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxMessageBytes is the largest message nib writes: Claude Code drops a stdio server that writes 16 MiB without a line break, failing
// every call in flight, so a larger reply becomes an error for its one call instead.
const maxMessageBytes = 8 * 1024 * 1024

// boundedTransport is a transport whose connection never writes a message over its limit.
type boundedTransport struct {
	inner mcp.Transport
	limit int
}

func (t boundedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return boundedConn{conn, t.limit}, nil
}

// boundedConn answers a response over its limit with an error for that request, and refuses any other message over it.
type boundedConn struct {
	mcp.Connection
	limit int
}

func (c boundedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil || len(data) <= c.limit {
		return c.Connection.Write(ctx, msg)
	}
	note := fmt.Sprintf("nib: a reply of %d bytes is over the %d it sends, so it was not sent", len(data), c.limit)
	fmt.Fprintln(os.Stderr, note)
	if r, ok := msg.(*jsonrpc.Response); ok {
		return c.Connection.Write(ctx, &jsonrpc.Response{ID: r.ID, Error: &jsonrpc.Error{Code: -32603, Message: note}})
	}
	return errors.New(note)
}
