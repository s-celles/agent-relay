// Package toolbridge lets a backend agent call the *caller's* tools.
//
// The claude CLI has no raw tool-calling mode, but it speaks MCP. So the
// relay hosts a tiny MCP server exposing the tools the client declared, and
// points the CLI at it. When the model calls one, the MCP handler *parks* —
// the relay reports a tool_use to the client and ends the HTTP response,
// leaving the subprocess alive and blocked. The client answers with a
// tool_result on its next request (standard Messages API semantics), which
// resolves the parked call and the CLI resumes.
//
// The MCP endpoint listens on its own loopback socket, never on the relay's
// public bind, and each session carries an unguessable id and bearer token.
package toolbridge

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

// ServerName is how the MCP server appears to the CLI; tools are then named
// mcp__<ServerName>__<tool>.
const ServerName = "relay"

// Call is one pending tool call: the model asked for it, and the caller must
// resolve it before the backend can continue.
type Call struct {
	ID    string // the CLI's own tool_use id, reused on the wire
	Name  string
	Input json.RawMessage
}

type pending struct {
	result chan callResult
}

type callResult struct {
	text    string
	isError bool
}

// Session is one client-tool conversation: the tools it may call, and the
// calls currently parked.
type Session struct {
	id      string
	token   string
	baseURL string
	tools   []toolSpec

	calls chan *Call

	mu      sync.Mutex
	pending map[string]*pending
	closed  bool
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (s *Session) ID() string    { return s.id }
func (s *Session) Token() string { return s.token }
func (s *Session) URL() string   { return s.baseURL + "/mcp/" + s.id }

// Calls yields tool calls as the model makes them.
func (s *Session) Calls() <-chan *Call { return s.calls }

// AllowedTools names the tools the CLI may call without a permission prompt.
func (s *Session) AllowedTools() []string {
	out := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, "mcp__"+ServerName+"__"+t.Name)
	}
	return out
}

// MCPConfig is the --mcp-config payload pointing the CLI at this session.
func (s *Session) MCPConfig() string {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			ServerName: map[string]any{
				"type":    "http",
				"url":     s.URL(),
				"headers": map[string]string{"Authorization": "Bearer " + s.token},
			},
		},
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// Resolve delivers a tool result to the parked call, letting the backend
// continue.
func (s *Session) Resolve(callID, text string, isError bool) error {
	s.mu.Lock()
	p := s.pending[callID]
	delete(s.pending, callID)
	s.mu.Unlock()
	if p == nil {
		return fmt.Errorf("toolbridge: no pending call %q", callID)
	}
	p.result <- callResult{text: text, isError: isError}
	return nil
}

// Pending reports whether the session is waiting on any tool result.
func (s *Session) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

func (s *Session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	pendings := s.pending
	s.pending = map[string]*pending{}
	s.mu.Unlock()

	// Release every parked call so the CLI cannot hang forever on a caller
	// that walked away.
	for _, p := range pendings {
		p.result <- callResult{text: "the relay closed this tool session", isError: true}
	}
	close(s.calls)
}

// Bridge hosts the MCP endpoint and owns the live sessions.
type Bridge struct {
	baseURL  string
	ln       net.Listener
	srv      *http.Server
	callWait time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
}

// New starts the MCP listener on a loopback port. callWait bounds how long a
// tool call may stay parked.
func New(callWait time.Duration) (*Bridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("toolbridge listen: %w", err)
	}
	b := &Bridge{
		baseURL:  "http://" + ln.Addr().String(),
		ln:       ln,
		callWait: callWait,
		sessions: map[string]*Session{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp/{session}", b.handleMCP)
	b.srv = &http.Server{Handler: mux}
	go b.srv.Serve(ln)
	return b, nil
}

func (b *Bridge) Close() {
	b.mu.Lock()
	sessions := b.sessions
	b.sessions = map[string]*Session{}
	b.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	b.srv.Close()
}

// NewSession registers the caller's tools and returns the session the CLI
// will connect back to.
func (b *Bridge) NewSession(tools []core.Tool) *Session {
	specs := make([]toolSpec, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		specs = append(specs, toolSpec{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	s := &Session{
		id:      randomHex(),
		token:   randomHex(),
		baseURL: b.baseURL,
		tools:   specs,
		calls:   make(chan *Call, 8),
		pending: map[string]*pending{},
	}
	b.mu.Lock()
	b.sessions[s.id] = s
	b.mu.Unlock()
	return s
}

func (b *Bridge) Session(id string) *Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

func (b *Bridge) CloseSession(id string) {
	b.mu.Lock()
	s := b.sessions[id]
	delete(b.sessions, id)
	b.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func randomHex() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf[:])
}

var errUnauthorized = errors.New("unauthorized")

// session authenticates the CLI's request against the session token.
func (b *Bridge) session(r *http.Request) (*Session, error) {
	s := b.Session(r.PathValue("session"))
	if s == nil {
		return nil, errUnauthorized
	}
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		return nil, errUnauthorized
	}
	return s, nil
}
