package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Claude Code's channels: a server that declares channelCapability may put a message in the session's conversation with channelMethod, which
// wakes an idle session and joins a running turn at its next request. Claude Code takes them only from a server its command line names in a
// channel option, and tells the server nothing of that, so the server reads its ancestors' command lines; and only on a connection of a
// protocol revision before channelRevision, the first with no way for a server to send a notification unasked.
const (
	channelCapability = "claude/channel"
	channelMethod     = "notifications/claude/channel"
	// channelClient is Claude Code's clientInfo name.
	channelClient = "claude-code"
	// channelDepth bounds the ancestors searched for a channel option: a wrapper or two may sit between Claude Code and the server.
	channelDepth = 8
	// channelRevision is the first MCP revision a channel cannot use.
	channelRevision = "2026-07-28"
	// ChannelName is the name nib setup registers the server under, which a channel option names it by.
	ChannelName = "nib"
)

// channelFlags are Claude Code's options that list a session's channels.
var channelFlags = []string{"--channels", "--dangerously-load-development-channels"}

// channelVersions are the protocol versions a server with a channel offers: those before channelRevision.
func channelVersions() []string {
	return slices.DeleteFunc(mcp.SupportedProtocolVersions(), func(v string) bool { return v >= channelRevision })
}

// ArgvNamesChannel says whether argv lists name's channel, server:NAME or plugin:NAME@MARKETPLACE, after a channel option, which takes each
// following word up to the next option, or in its =value; known is false when argv has no channel option at all.
func ArgvNamesChannel(argv []string, name string) (named, known bool) {
	listing := false
	for _, word := range argv {
		if strings.HasPrefix(word, "-") {
			listing = false
			for _, flag := range channelFlags {
				if word == flag {
					listing, known = true, true
				}
				if value, ok := strings.CutPrefix(word, flag+"="); ok {
					known = true
					if entryNames(value, name) {
						return true, true
					}
				}
			}
			continue
		}
		if listing && entryNames(word, name) {
			return true, true
		}
	}
	return false, known
}

func entryNames(entry, name string) bool {
	if rest, ok := strings.CutPrefix(entry, "server:"); ok {
		return rest == name
	}
	if rest, ok := strings.CutPrefix(entry, "plugin:"); ok {
		plugin, _, found := strings.Cut(rest, "@")
		return found && plugin == name
	}
	return false
}

// ChannelNamed says whether the nearest ancestor of this process started with a channel option names name's channel; one farther up is
// another session, whose channels never reach this one. It is false where /proc cannot be read.
func ChannelNamed(name string) bool { return channelNamed(os.Getppid(), name, procArgv, procParent) }

func channelNamed(pid int, name string, argv func(int) ([]string, bool), parent func(int) (int, bool)) bool {
	for depth := 0; depth < channelDepth && pid > 1; depth++ {
		if args, ok := argv(pid); ok {
			if named, known := ArgvNamesChannel(args, name); known {
				return named
			}
		}
		next, ok := parent(pid)
		if !ok {
			return false
		}
		pid = next
	}
	return false
}

func procArgv(pid int) ([]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), true
}

// procParent reads a process's parent from its stat line, after the command name, which may hold spaces and parentheses.
func procParent(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	at := strings.LastIndexByte(string(data), ')')
	if at < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[at+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	return parent, err == nil
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
