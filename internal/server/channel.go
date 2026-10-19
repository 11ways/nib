package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Claude Code's channels: a server that declares channelCapability may put a message in the session's conversation with channelMethod, which
// wakes an idle session and joins a running turn at its next request. Claude Code takes them only from a server its command line names in a
// channel option and tells the server nothing of that, but passes its environment on, so the launcher that names nib's channel sets
// ChannelEnv; and only on a connection of a protocol revision before channelRevision, the first with no way to send a notification unasked.
const (
	channelCapability = "claude/channel"
	channelMethod     = "notifications/claude/channel"
	// channelClient is Claude Code's clientInfo name.
	channelClient = "claude-code"
	// channelRevision is the first MCP revision a channel cannot use.
	channelRevision = "2026-07-28"
	// ChannelEnv is set by the launcher that starts Claude Code naming nib's channel (server:nib).
	ChannelEnv = "NIB_CHANNEL"
)

// channelVersions are the protocol versions a server with a channel offers: those before channelRevision.
func channelVersions() []string {
	return slices.DeleteFunc(mcp.SupportedProtocolVersions(), func(v string) bool { return v >= channelRevision })
}

// ChannelOn says whether the launcher said the session names nib's channel; nibash keeps ChannelEnv from the programs it starts, so a Claude
// Code started inside the session does not take it for its own.
func ChannelOn() bool {
	value := os.Getenv(ChannelEnv)
	return value != "" && value != "0"
}

// channel is the connection channel messages go out on, which the transport from Transport keeps; nib serves one client over stdio.
type channel struct {
	mu   sync.Mutex
	conn mcp.Connection
}

// channelTransport keeps the connection its transport makes for the channel.
type channelTransport struct {
	inner   mcp.Transport
	channel *channel
}

func (t channelTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err == nil {
		t.channel.mu.Lock()
		t.channel.conn = conn
		t.channel.mu.Unlock()
	}
	return conn, err
}

// push sends one channel message; Claude Code shows content in the conversation in a channel tag carrying meta as its attributes.
func (ch *channel) push(ctx context.Context, content string, meta map[string]string) error {
	ch.mu.Lock()
	conn := ch.conn
	ch.mu.Unlock()
	if conn == nil {
		return errors.New("no client connected")
	}
	params, err := json.Marshal(struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta,omitempty"`
	}{content, meta})
	if err != nil {
		return err
	}
	return conn.Write(ctx, &jsonrpc.Request{Method: channelMethod, Params: params})
}

// Transport is t, keeping its connection for channel messages when the server has a channel.
func (s *Server) Transport(t mcp.Transport) mcp.Transport {
	if s.channel == nil {
		return t
	}
	return channelTransport{t, s.channel}
}

// pushes is how a message reaches the agent making c on its own, nil when none does: only Claude Code's main agent has channel messages, as
// one pushed while a subagent runs lands in the main conversation, so the hook must have told this call apart from a subagent's.
func (s *Server) pushes(c *call) func(content string, meta map[string]string) error {
	if s.channel == nil || c.agent != "" || !c.hooked {
		return nil
	}
	if params := c.key.InitializeParams(); params == nil || params.ClientInfo == nil || params.ClientInfo.Name != channelClient {
		return nil
	}
	return func(content string, meta map[string]string) error {
		return s.channel.push(context.Background(), content, meta)
	}
}
