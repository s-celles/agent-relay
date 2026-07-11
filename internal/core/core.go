// Package core defines the neutral request/event model that sits between the
// wire-format API layer and the agent backends. It depends on nothing else in
// the module; api and backend packages depend on it, never the reverse.
package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type BlockKind int

const (
	BlockText BlockKind = iota
	BlockToolUse
	BlockToolResult
	BlockFile
)

// Block is one unit of structured message content. Text blocks carry Text;
// tool_use blocks carry ToolID/ToolName/ToolInput; tool_result blocks carry
// ToolID plus the flattened result in Text (and IsError); file blocks
// (decoded image/document attachments) carry MediaType and Data.
type Block struct {
	Kind      BlockKind
	Text      string
	ToolID    string
	ToolName  string
	ToolInput json.RawMessage
	IsError   bool
	MediaType string // BlockFile
	Data      []byte // BlockFile: decoded bytes
}

type Message struct {
	Role   Role
	Blocks []Block
}

// NewTextMessage builds the common single-text-block message.
func NewTextMessage(role Role, text string) Message {
	return Message{Role: role, Blocks: []Block{{Kind: BlockText, Text: text}}}
}

// PlainText joins the message's text blocks; non-text blocks are skipped.
func (m Message) PlainText() string {
	var parts []string
	for _, b := range m.Blocks {
		if b.Kind == BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Tool is a client-defined tool the model may call. Serving it requires a
// backend with Capabilities.ClientTools.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// InferRequest is the normalized request. Wire adapters build it; backends
// consume it.
type InferRequest struct {
	Model     string // logical model name, e.g. "sonnet"
	System    string
	Messages  []Message
	Stream    bool
	MaxTokens int
	// Tools are client-defined tools the caller expects the model to call
	// (with results returned by the caller). Only backends reporting
	// Capabilities.ClientTools can serve them.
	Tools      []Tool
	ToolChoice string // "auto", "any", "tool", "none"; "" when unset
	// Sampling parameters. Nil/empty means unset. Backends that do not
	// report Capabilities.Sampling ignore them; the server signals that.
	Temperature   *float64
	TopP          *float64
	TopK          *int
	StopSequences []string
	// IncludeUsage asks a streaming response to carry token usage
	// (OpenAI's stream_options.include_usage; always on in the Anthropic wire).
	IncludeUsage bool
	// Traces asks for the backend agent's own tool activity to be surfaced
	// (X-Agent-Traces). Off by default: unknown SSE event types can trip
	// strict clients.
	Traces bool
	// Agentic marks a request authorized for host-side agentic execution
	// (REQ-EXEC-06). Only the server sets it, after per-request
	// authorization; backends refuse it unless configured for agentic mode.
	Agentic bool
	// OutputDir, when set on an agentic request, is the working directory
	// the backend must use and must NOT delete — its lifecycle belongs to
	// the server's output store (X-Agentic-Keep-Outputs).
	OutputDir string
	// SessionID resumes a previous backend conversation (X-Session-Id).
	// Backends key sessions by working directory, so the server only allows
	// it where the workdir is stable.
	SessionID string
	// Timeout overrides the dispatcher's default deadline for this request
	// (X-Request-Timeout, clamped by the operator's ceiling). Zero means
	// "use the default".
	Timeout time.Duration
}

type EventKind int

const (
	EventMessageStart EventKind = iota
	EventTextDelta
	EventMessageStop
	EventError
	EventToolUseStart // the model starts calling a client-defined tool
	EventToolUseDelta // partial JSON of the tool input
	EventToolUseStop
	// Trace events: the backend's *own* agent loop used one of its tools
	// (agentic mode). Informational — they are not part of the model's reply
	// to the caller, and are only surfaced when the client opts in.
	EventAgentToolUse
	EventAgentToolResult
	// EventSession reports the backend conversation id, so the caller can
	// resume it on a later request.
	EventSession
)

type Usage struct {
	InputTokens, OutputTokens int
	// CostUSD is the backend-reported dollar cost of the turn, when it
	// reports one (the claude CLI does). Zero means "not reported".
	CostUSD float64
}

type Event struct {
	Kind EventKind
	Text string // EventTextDelta; EventToolUseDelta: partial input JSON
	// Usage is set on EventMessageStart (input tokens, as the wire formats
	// report them up front) and on EventMessageStop (final counts).
	Usage     *Usage
	Err       error           // EventError
	ToolID    string          // EventToolUseStart / EventAgentTool*
	ToolName  string          // EventToolUseStart / EventAgentToolUse
	ToolInput json.RawMessage // EventAgentToolUse
	IsError   bool            // EventAgentToolResult
	SessionID string          // EventSession
	// StopReason is set on EventMessageStop; "" means the default
	// ("end_turn", or "tool_use" when the turn produced tool calls).
	StopReason string
}

// EventSink renders neutral events to the client wire format and flushes.
type EventSink interface {
	Emit(ctx context.Context, ev Event) error
}

type Capabilities struct {
	Streaming bool
	Agentic   bool
	// ClientTools is true when the backend can accept client-defined tool
	// definitions and stop the turn on a tool_use for the caller to execute.
	// The claude CLI backend cannot (the CLI runs its own agent loop).
	ClientTools bool
	// MaxTokens is true when the backend enforces InferRequest.MaxTokens.
	// The claude CLI backend cannot (the CLI has no such flag), so the wire
	// value is accepted for compatibility but responses may exceed it.
	MaxTokens bool
	// Sampling is true when the backend honors Temperature/TopP/TopK/
	// StopSequences. The claude CLI exposes no such flags.
	Sampling bool
	Models   []string
}

// UnsupportedSampling lists the sampling parameters set on the request, for
// signaling when the backend cannot honor them.
func (r InferRequest) UnsupportedSampling() []string {
	var params []string
	if r.Temperature != nil {
		params = append(params, "temperature")
	}
	if r.TopP != nil {
		params = append(params, "top_p")
	}
	if r.TopK != nil {
		params = append(params, "top_k")
	}
	if len(r.StopSequences) > 0 {
		params = append(params, "stop_sequences")
	}
	return params
}

// Backend is implemented once per agent CLI/SDK.
// Infer MUST return promptly on ctx cancellation and MUST guarantee any
// spawned subprocess is terminated before it returns.
type Backend interface {
	Name() string
	Capabilities() Capabilities
	Infer(ctx context.Context, req InferRequest, sink EventSink) error
}
