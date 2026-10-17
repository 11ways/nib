// Package providers runs the programs nib hosts, whose MCP tools nib offers as its own, and reads their answers; docs/design-providers.md is
// the protocol.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/11ways/nib/internal/engine"
)

// Subcommand is the argument a provider program is run with.
const Subcommand = "nib-provider"

// Version is the version of the answer format nib reads; any other is shown as the provider's plain text.
const Version = 1

// StartTimeout bounds starting a provider and listing its tools.
const StartTimeout = 30 * time.Second

// DescriptionLimit is where clients cut a tool description; a provider tool with a longer one is left out.
const DescriptionLimit = 2048

var (
	nameShape = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	toolShape = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// Spec is one --provider NAME=PROGRAM.
type Spec struct {
	Name, Program string
}

// String is the spec as the flag takes it.
func (s Spec) String() string { return s.Name + "=" + s.Program }

// ParseSpec reads NAME=PROGRAM.
func ParseSpec(text string) (Spec, error) {
	name, program, ok := strings.Cut(text, "=")
	if !ok || !nameShape.MatchString(name) || program == "" {
		return Spec{}, fmt.Errorf("--provider takes NAME=PROGRAM, with NAME like zemble (lowercase letters and digits), got %q", text)
	}
	return Spec{name, program}, nil
}

// Provider is one hosted program, with the tools it listed when it started.
type Provider struct {
	Spec
	// Tools are the tools nib offers, by the provider's own names; Skipped says why any other was left out.
	Tools   []*mcp.Tool
	Skipped []string
	dir     string
	mu      sync.Mutex
	session *mcp.ClientSession
	// ended is closed when the session's connection ends.
	ended chan struct{}
}

// Start runs a provider in dir and lists its tools.
func Start(ctx context.Context, spec Spec, dir string) (*Provider, error) {
	p := &Provider{Spec: spec, dir: dir}
	ctx, cancel := context.WithTimeout(ctx, StartTimeout)
	defer cancel()
	if err := p.connect(ctx); err != nil {
		return nil, err
	}
	listed, err := p.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("%s: listing its tools: %w", spec.Name, err)
	}
	for _, tool := range listed.Tools {
		if why := unfit(tool); why != "" {
			p.Skipped = append(p.Skipped, fmt.Sprintf("%s: %s", tool.Name, why))
			continue
		}
		p.Tools = append(p.Tools, tool)
	}
	return p, nil
}

// unfit says why nib cannot offer a tool, or "".
func unfit(tool *mcp.Tool) string {
	if !toolShape.MatchString(tool.Name) {
		return "its name is not lowercase letters, digits and underscores"
	}
	if len(tool.Description) > DescriptionLimit {
		return fmt.Sprintf("its description has %d characters, over the %d clients keep", len(tool.Description), DescriptionLimit)
	}
	schema, _ := tool.InputSchema.(map[string]any)
	if schema == nil || schema["type"] != "object" {
		return "its input schema is not an object schema"
	}
	return ""
}

func (p *Provider) connect(ctx context.Context) error {
	program := p.Program
	if !strings.Contains(program, "/") {
		found, err := exec.LookPath(program)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		program = found
	} else if !filepath.IsAbs(program) {
		program = filepath.Join(p.dir, program)
	}
	cmd := exec.Command(program, Subcommand)
	cmd.Dir = p.dir
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "nib", Version: "provider-host"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("%s: starting %s %s: %w", p.Name, program, Subcommand, err)
	}
	ended := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(ended)
	}()
	p.session, p.ended = session, ended
	return nil
}

// Call calls one of the provider's tools, starting it again first when its process ended; a call it was answering when it ended is not sent
// again, since nib cannot know what it did.
func (p *Provider) Call(ctx context.Context, tool string, arguments json.RawMessage) (*mcp.CallToolResult, error) {
	p.mu.Lock()
	select {
	case <-p.ended:
		p.session = nil
	default:
	}
	if p.session == nil {
		start, cancel := context.WithTimeout(ctx, StartTimeout)
		err := p.connect(start)
		cancel()
		if err != nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("%s had ended and could not be started again: %w", p.Name, err)
		}
	}
	session, ended := p.session, p.ended
	p.mu.Unlock()
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		select {
		case <-ended:
		default:
			if !errors.Is(err, io.EOF) && !errors.Is(err, mcp.ErrConnectionClosed) {
				return nil, fmt.Errorf("%s: %w", p.Name, err)
			}
		}
		p.mu.Lock()
		if p.session == session {
			_ = session.Close()
			p.session = nil
		}
		p.mu.Unlock()
		return nil, fmt.Errorf("%s ended during the call; it starts again on the next one, and this call was not sent again", p.Name)
	}
	return result, nil
}

// Close ends the provider's process.
func (p *Provider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session != nil {
		_ = p.session.Close()
		p.session = nil
	}
}

// Text is a result's text content, joined; what nib shows when it cannot read the structured answer.
func Text(result *mcp.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// part is one entry of an answer's parts; a part with any other key is unknown, and so is the whole answer.
type part struct {
	Text    *string `json:"text"`
	Path    *string `json:"path"`
	Start   *int    `json:"start"`
	End     *int    `json:"end"`
	Indexed *string `json:"indexed"`
}

// Pieces reads a result's structured answer; ok is false when it has none nib knows, a version, key or value it does not expect included,
// so the caller shows the text instead and never guesses rows.
func Pieces(result *mcp.CallToolResult) (pieces []engine.Piece, ok bool) {
	if result.StructuredContent == nil {
		return nil, false
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, false
	}
	var answer struct {
		Nib   *int              `json:"nib"`
		Parts []json.RawMessage `json:"parts"`
	}
	if strict(data, &answer) != nil || answer.Nib == nil || *answer.Nib != Version || answer.Parts == nil {
		return nil, false
	}
	for _, raw := range answer.Parts {
		var p part
		if strict(raw, &p) != nil {
			return nil, false
		}
		piece, ok := p.piece()
		if !ok {
			return nil, false
		}
		pieces = append(pieces, piece)
	}
	return pieces, true
}

// piece checks a part is either prose alone or a whole location: an absolute path, 1 <= start <= end, and indexed text of that many lines.
func (p part) piece() (engine.Piece, bool) {
	if p.Text != nil {
		return engine.Piece{Text: *p.Text}, p.Path == nil && p.Start == nil && p.End == nil && p.Indexed == nil
	}
	if p.Path == nil || p.Start == nil || p.End == nil || !filepath.IsAbs(*p.Path) || *p.Start < 1 || *p.End < *p.Start {
		return engine.Piece{}, false
	}
	if p.Indexed != nil && strings.Count(*p.Indexed, "\n")+1 != *p.End-*p.Start+1 {
		return engine.Piece{}, false
	}
	return engine.Piece{Path: *p.Path, Start: *p.Start, End: *p.End, Indexed: p.Indexed}, true
}

// strict decodes JSON refusing unknown keys and trailing data.
func strict(data []byte, into any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data")
	}
	return nil
}
