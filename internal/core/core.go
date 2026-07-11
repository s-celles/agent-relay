// Package core defines the neutral request/event model that sits between the
// wire-format API layer and the agent backends. It depends on nothing else in
// the module; api and backend packages depend on it, never the reverse.
package core

import (
	"context"
	"encoding/json"
	"strings"
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
	// Agentic marks a request authorized for host-side agentic execution
	// (REQ-EXEC-06). Only the server sets it, after per-request
	// authorization; backends refuse it unless configured for agentic mode.
	Agentic bool
}

type EventKind int

const (
	EventMessageStart EventKind = iota
	EventTextDelta
	EventUsage
	EventMessageStop
	EventError
	EventToolUseStart // the model starts calling a client-defined tool
	EventToolUseDelta // partial JSON of the tool input
	EventToolUseStop
)

type Usage struct{ InputTokens, OutputTokens int }

type Event struct {
	Kind       EventKind
	Text       string // EventTextDelta; EventToolUseDelta: partial input JSON
	Usage      *Usage // EventUsage / EventMessageStop
	Err        error  // EventError
	ToolID     string // EventToolUseStart
	ToolName   string // EventToolUseStart
	StopReason string // EventMessageStop; "" means default ("end_turn"/"tool_use")
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
	Models    []string
}

// Backend is implemented once per agent CLI/SDK.
// Infer MUST return promptly on ctx cancellation and MUST guarantee any
// spawned subprocess is terminated before it returns.
type Backend interface {
	Name() string
	Capabilities() Capabilities
	Infer(ctx context.Context, req InferRequest, sink EventSink) error
}
