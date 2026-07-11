package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
	"github.com/s-celles/agent-relay/internal/toolbridge"
)

// toolSession drives a client-tool conversation across several HTTP requests.
//
// The backend runs in its own goroutine for the whole tool loop; its events
// flow into a channel. When the model calls one of the caller's tools, the
// bridge parks the subprocess and the current HTTP request ends with a
// tool_use block and stop_reason "tool_use" — standard Messages API
// semantics. The caller's next request carries the tool_result, which
// resolves the parked call and the same backend goroutine continues.
type toolSession struct {
	bridge  *toolbridge.Session
	events  chan core.Event // backend → HTTP handler
	done    chan error      // the backend goroutine finished
	cancel  context.CancelFunc
	expires time.Time

	mu       sync.Mutex
	finished bool
}

// chanSink hands the backend's events to the session's channel.
type chanSink struct {
	ch  chan core.Event
	ctx context.Context
}

func (c *chanSink) Emit(ctx context.Context, ev core.Event) error {
	select {
	case c.ch <- ev:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// toolSessions tracks live client-tool conversations, keyed by the id of
// every tool call they have parked: the caller's follow-up request names the
// tool_use_id, which is all we need to find the conversation again.
type toolSessions struct {
	mu       sync.Mutex
	byCallID map[string]*toolSession
}

func newToolSessions() *toolSessions {
	return &toolSessions{byCallID: map[string]*toolSession{}}
}

func (t *toolSessions) put(callID string, s *toolSession) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byCallID[callID] = s
}

func (t *toolSessions) take(callID string) *toolSession {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.byCallID[callID]
	delete(t.byCallID, callID)
	return s
}

// sweep drops sessions whose caller never came back, killing their
// subprocess.
func (t *toolSessions) sweep(now time.Time) {
	t.mu.Lock()
	var stale []*toolSession
	for id, s := range t.byCallID {
		if now.After(s.expires) {
			stale = append(stale, s)
			delete(t.byCallID, id)
		}
	}
	t.mu.Unlock()
	for _, s := range stale {
		s.finish()
	}
}

// finish tears the session down: the parked call is released, the backend
// goroutine's context is cancelled, and its subprocess dies with it.
func (s *toolSession) finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.mu.Unlock()

	s.cancel()
}

// pumpUntilPause forwards backend events to the client's sink until either a
// tool call parks the backend (returns the call) or the turn ends.
//
// Closed channels are disabled by nilling the local handle: a receive on a
// closed channel succeeds forever, so leaving it in the select would spin.
func (s *toolSession) pumpUntilPause(ctx context.Context, sink core.EventSink) (*toolbridge.Call, error) {
	calls := s.bridge.Calls()
	events := s.events
	done := s.done

	var finalErr error
	backendDone := false

	for {
		// The turn is over once the backend has finished *and* everything it
		// emitted has reached the client.
		if backendDone && events == nil {
			return nil, finalErr
		}
		select {
		case call, ok := <-calls:
			if !ok {
				calls = nil
				continue
			}
			return call, nil

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := sink.Emit(ctx, ev); err != nil {
				return nil, err
			}

		case err := <-done:
			finalErr, backendDone, done = err, true, nil

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// emitToolUse renders a parked call as the Messages API would: a tool_use
// content block, then a message_stop with stop_reason "tool_use".
func emitToolUse(ctx context.Context, sink core.EventSink, call *toolbridge.Call) error {
	if err := sink.Emit(ctx, core.Event{
		Kind: core.EventToolUseStart, ToolID: call.ID, ToolName: call.Name,
	}); err != nil {
		return err
	}
	if err := sink.Emit(ctx, core.Event{
		Kind: core.EventToolUseDelta, Text: string(call.Input),
	}); err != nil {
		return err
	}
	if err := sink.Emit(ctx, core.Event{Kind: core.EventToolUseStop}); err != nil {
		return err
	}
	return sink.Emit(ctx, core.Event{Kind: core.EventMessageStop, StopReason: "tool_use"})
}

// toolResults extracts the tool_result blocks of the caller's last message.
func toolResults(req core.InferRequest) []core.Block {
	if len(req.Messages) == 0 {
		return nil
	}
	var out []core.Block
	for _, b := range req.Messages[len(req.Messages)-1].Blocks {
		if b.Kind == core.BlockToolResult {
			out = append(out, b)
		}
	}
	return out
}

func inputOrEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage(`{}`)
	}
	return raw
}
