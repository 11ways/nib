package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/engine"
	"github.com/11ways/nib/internal/hookstate"
	"github.com/11ways/nib/internal/platform"
)

// ShapeRequest is what nib shape asks the session's server: the log, the run's description, its exit status and the command that wrote it.
// nib's hook asks with Mentioned instead of a log: the named directories a message the user sent uses, which the main agent's context last
// dropped at Dropped (Unix nanoseconds, 0 for never).
type ShapeRequest struct {
	Log       string   `json:"log,omitempty"`
	Purpose   string   `json:"purpose,omitempty"`
	Command   string   `json:"command,omitempty"`
	Exit      int      `json:"exit,omitempty"`
	Mentioned []string `json:"mentioned,omitempty"`
	Dropped   int64    `json:"dropped,omitempty"`
}

// ShapeAnswer is the server's answer: the shaped text, or with Status 2 why the log was refused.
type ShapeAnswer struct {
	Text   string `json:"text"`
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

const (
	// shapeRequestLimit bounds a request; a log's path, purpose and command fit many times over.
	shapeRequestLimit = 64 * 1024
	// shapeWait bounds how long nib shape waits for the session's server, which may ask a decider and then a summarizer.
	shapeWait = 3 * time.Minute
	// shapeAgent is how the token report names what nib shape requests returned, as no agent is known to have asked.
	shapeAgent = "nib shape"
)

// ShapeSocketEnv names the socket of the nib server whose programs see it, which nib shape asks after the session's.
const ShapeSocketEnv = "NIB_SHAPE_SOCKET"

// RuntimeDirectory is nib's private runtime folder: nibash's logs and the session sockets nib shape finds a server by.
func RuntimeDirectory() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(base) {
		return filepath.Join(base, "nib")
	}
	return platform.TempFolder()
}

// SocketPath is the socket of the nib server known by key.
func SocketPath(key string) string { return filepath.Join(RuntimeDirectory(), "session-"+key+".sock") }

// ShapeKey is the key this server's socket is known by: Claude Code's session, which the session's other programs (another MCP server,
// a Bash command, a hook) see in their environment too, else this process.
func ShapeKey(getenv func(string) string) string {
	if session := getenv("CLAUDE_CODE_SESSION_ID"); hookstate.IsID(session) {
		return "claude-" + session
	}
	return fmt.Sprintf("nib-%d", os.Getpid())
}

// ParentKey is the key this server's second socket is known by under Claude Code: the process that started it, Claude Code itself, whose
// id its hooks and commands see as CLAUDE_PID; "" outside Claude Code. A /clear gives the session a new id but leaves its MCP servers
// with the old one, so after it only this socket is found.
func ParentKey(getenv func(string) string) string {
	if !hookstate.IsID(getenv("CLAUDE_CODE_SESSION_ID")) {
		return ""
	}
	return fmt.Sprintf("claude-pid-%d", os.Getppid())
}

// ShapeSockets are the sockets nib shape and nib's hook ask, in order: the Claude Code session's they run in, its process's, then the one
// the nib server whose program started them named.
func ShapeSockets(getenv func(string) string) []string {
	var paths []string
	if session := getenv("CLAUDE_CODE_SESSION_ID"); hookstate.IsID(session) {
		paths = append(paths, SocketPath("claude-"+session))
	}
	if pid, err := strconv.Atoi(getenv("CLAUDE_PID")); err == nil && pid > 0 {
		paths = append(paths, SocketPath(fmt.Sprintf("claude-pid-%d", pid)))
	}
	if named := getenv(ShapeSocketEnv); filepath.IsAbs(named) && !slices.Contains(paths, named) {
		paths = append(paths, named)
	}
	return paths
}

// ListenShape listens on the socket known by key; nil when the runtime folder is not private or another live server holds the socket
// already, the first one keeping it. A socket a dead server left is replaced.
func ListenShape(key string) (net.Listener, error) {
	if !hookstate.Private(RuntimeDirectory(), true) {
		return nil, nil
	}
	path := SocketPath(key)
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return nil, nil
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := platform.RestrictSocket(path); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

// ServeShape answers nib shape requests on listener until ctx ends, closing it then, which removes the socket.
func (s *Server) ServeShape(ctx context.Context, listener net.Listener) {
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.answerShape(ctx, conn)
	}
}

// answerShape reads one request from conn and writes its answer.
func (s *Server) answerShape(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(io.LimitReader(conn, shapeRequestLimit)).ReadBytes('\n')
	var request ShapeRequest
	if err == nil {
		err = json.Unmarshal(line, &request)
	}
	if err == nil && request.Mentioned == nil && !filepath.IsAbs(request.Log) {
		err = fmt.Errorf("the log %q is not an absolute path", request.Log)
	}
	answer := ShapeAnswer{Status: 2}
	switch {
	case err != nil:
		answer.Error = err.Error()
	case request.Mentioned != nil:
		answer = s.promptAnswer(request)
	default:
		answer = s.shapeAnswer(ctx, request)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(conn).Encode(answer)
}

// shapeAnswer shapes the log a request names with this server's shell, judging its path as a read is judged; what it returns counts in the
// token report under shapeAgent.
func (s *Server) shapeAnswer(ctx context.Context, request ShapeRequest) ShapeAnswer {
	if s.options.Shell == nil {
		return ShapeAnswer{Status: 2, Error: "this nib server offers no nibash"}
	}
	arguments, _ := json.Marshal(request)
	end := s.shapes.begin(shapeAgent, "shape", false, arguments)
	tools := &engine.Context{
		Ctx:       ctx,
		Directory: s.options.Directory,
		Session:   s.engineSession(shapeAgent),
		Sandbox:   s.options.Sandbox,
		Authorize: func(context.Context, string, string) error { return nil },
		Names:     s.options.Names.For(s.defined, s.engineSession(shapeAgent)),
	}
	if s.options.Sandbox {
		roots, err := s.knownRoots()
		if err != nil {
			end(nil)
			return ShapeAnswer{Status: 2, Error: err.Error()}
		}
		tools.Roots = func(context.Context) ([]string, error) { return roots, nil }
	}
	text, err := s.options.Shell.Shape(ctx, request.Log, request.Command, request.Purpose, request.Exit, tools)
	if err != nil {
		end(nil)
		return ShapeAnswer{Status: 2, Error: err.Error()}
	}
	end(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}})
	return ShapeAnswer{Text: text}
}

// promptAnswer is what the main agent must be told of the named directories a message the user sent uses, one line each, after its context
// was dropped at the moment the request names, which the server applies now as its next call would.
func (s *Server) promptAnswer(request ShapeRequest) ShapeAnswer {
	main := s.engineSession("")
	s.seeding.Lock()
	defer s.seeding.Unlock()
	if request.Dropped > 0 {
		if moment := time.Unix(0, request.Dropped); s.engine.Store.Forget(main, moment) {
			s.defined.Forget(main, moment)
		}
	}
	return ShapeAnswer{Text: strings.Join(s.options.Names.For(s.defined, main).Prompted(request.Mentioned), "\n")}
}

// knownRoots are the roots the sandbox confines a nib shape request to: the client's, as last listed for one of its calls, or the server's
// directory for a client that has none; with neither known yet the sandbox allows nothing, as for a call.
func (s *Server) knownRoots() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, state := range s.sessions {
		params := key.InitializeParams()
		if params == nil || params.Capabilities == nil || params.Capabilities.RootsV2 == nil {
			return []string{s.options.Directory}, nil
		}
		state.mu.Lock()
		roots := state.roots
		state.mu.Unlock()
		if roots != nil {
			return roots, nil
		}
	}
	return nil, errors.New("the client's roots are not known yet, so the sandbox allows nothing")
}

// AskShape asks the server of the first of paths whose socket answers; ok is false when none did, or the answer could not be read, so the
// caller shapes the log itself.
func AskShape(ctx context.Context, paths []string, request ShapeRequest) (answer ShapeAnswer, ok bool) {
	if !hookstate.Private(RuntimeDirectory(), false) {
		return answer, false
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err != nil {
			continue
		}
		answer, ok = exchange(conn, request)
		return answer, ok
	}
	return answer, false
}

// exchange sends request on conn and reads the answer, within shapeWait.
func exchange(conn net.Conn, request ShapeRequest) (answer ShapeAnswer, ok bool) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(shapeWait))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return answer, false
	}
	if err := json.NewDecoder(conn).Decode(&answer); err != nil {
		return answer, false
	}
	return answer, answer.Status == 0 || answer.Status == 2
}
