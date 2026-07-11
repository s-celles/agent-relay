// Package core defines the neutral request/event model that sits between the
// wire-format API layer and the agent backends. It depends on nothing else in
// the module; api and backend packages depend on it, never the reverse.
package core

import "context"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role
	Content string // v1: text only; structured content is a later concern
}

// InferRequest is the normalized request. Wire adapters build it; backends
// consume it.
type InferRequest struct {
	Model     string // logical model name, e.g. "sonnet"
	System    string
	Messages  []Message
	Stream    bool
	MaxTokens int
}

type EventKind int

const (
	EventMessageStart EventKind = iota
	EventTextDelta
	EventUsage
	EventMessageStop
	EventError
)

type Usage struct{ InputTokens, OutputTokens int }

type Event struct {
	Kind  EventKind
	Text  string // EventTextDelta
	Usage *Usage // EventUsage / EventMessageStop
	Err   error  // EventError
}

// EventSink renders neutral events to the client wire format and flushes.
type EventSink interface {
	Emit(ctx context.Context, ev Event) error
}

type Capabilities struct {
	Streaming bool
	Agentic   bool
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
