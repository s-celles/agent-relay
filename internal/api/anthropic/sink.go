package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/s-celles/agent-relay/internal/core"
)

type usageJSON struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// StreamSink renders core events as Anthropic Messages SSE, flushing after
// every event (REQ-API-02). Content blocks (text and tool_use) are opened
// lazily and indexed in order.
type StreamSink struct {
	w          http.ResponseWriter
	id         string
	model      string
	started    bool
	usage      core.Usage
	blockIndex int    // index of the currently open block; -1 when none
	blockKind  string // "text" or "tool_use"; "" when no block is open
	nextIndex  int
	sawToolUse bool
}

func NewStreamSink(w http.ResponseWriter, id, model string) *StreamSink {
	return &StreamSink{w: w, id: id, model: model, blockIndex: -1}
}

// Started reports whether any bytes have been written; once true, errors can
// only be delivered in-stream, not as an HTTP status.
func (s *StreamSink) Started() bool { return s.started }

func (s *StreamSink) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventMessageStart:
		return s.start()
	case core.EventTextDelta:
		if err := s.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
			return err
		}
		return s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case core.EventToolUseStart:
		s.sawToolUse = true
		if err := s.closeBlock(); err != nil {
			return err
		}
		return s.openBlock("tool_use", map[string]any{
			"type": "tool_use", "id": ev.ToolID, "name": ev.ToolName, "input": map[string]any{},
		})
	case core.EventToolUseDelta:
		return s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Text},
		})
	case core.EventToolUseStop:
		return s.closeBlock()
	case core.EventUsage, core.EventMessageStop:
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		if ev.Kind == core.EventUsage {
			return nil
		}
		return s.stop(ev.StopReason)
	case core.EventError:
		if err := s.start(); err != nil {
			return err
		}
		return s.event("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": ev.Err.Error()},
		})
	}
	return nil
}

func (s *StreamSink) start() error {
	if s.started {
		return nil
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.WriteHeader(http.StatusOK)
	return s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": usageJSON{},
		},
	})
}

// openBlock starts a content block of the given kind unless one of that kind
// is already open; a block of a different kind is closed first.
func (s *StreamSink) openBlock(kind string, contentBlock map[string]any) error {
	if err := s.start(); err != nil {
		return err
	}
	if s.blockKind == kind {
		return nil
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	s.blockIndex = s.nextIndex
	s.nextIndex++
	s.blockKind = kind
	return s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.blockIndex,
		"content_block": contentBlock,
	})
}

func (s *StreamSink) closeBlock() error {
	if s.blockKind == "" {
		return nil
	}
	index := s.blockIndex
	s.blockKind = ""
	s.blockIndex = -1
	return s.event("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

func (s *StreamSink) stop(stopReason string) error {
	if err := s.start(); err != nil {
		return err
	}
	// A response with no content at all still carries one empty text block.
	if s.nextIndex == 0 {
		if err := s.openBlock("text", map[string]any{"type": "text", "text": ""}); err != nil {
			return err
		}
	}
	if err := s.closeBlock(); err != nil {
		return err
	}
	if stopReason == "" {
		stopReason = "end_turn"
		if s.sawToolUse {
			stopReason = "tool_use"
		}
	}
	if err := s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": usageJSON{InputTokens: s.usage.InputTokens, OutputTokens: s.usage.OutputTokens},
	}); err != nil {
		return err
	}
	return s.event("message_stop", map[string]any{"type": "message_stop"})
}

func (s *StreamSink) event(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// collectedBlock accumulates one content block for a non-streaming response.
type collectedBlock struct {
	kind     string // "text" or "tool_use"
	text     strings.Builder
	toolID   string
	toolName string
}

// CollectSink accumulates the whole event stream for a non-streaming
// response body.
type CollectSink struct {
	id         string
	model      string
	blocks     []*collectedBlock
	usage      core.Usage
	stopReason string
	sawToolUse bool
	err        error
}

func NewCollectSink(id, model string) *CollectSink {
	return &CollectSink{id: id, model: model}
}

func (c *CollectSink) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventTextDelta:
		c.appendTo("text").text.WriteString(ev.Text)
	case core.EventToolUseStart:
		c.sawToolUse = true
		c.blocks = append(c.blocks, &collectedBlock{
			kind: "tool_use", toolID: ev.ToolID, toolName: ev.ToolName,
		})
	case core.EventToolUseDelta:
		if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == "tool_use" {
			c.blocks[n-1].text.WriteString(ev.Text)
		}
	case core.EventUsage, core.EventMessageStop:
		if ev.Usage != nil {
			c.usage = *ev.Usage
		}
		if ev.Kind == core.EventMessageStop {
			c.stopReason = ev.StopReason
		}
	case core.EventError:
		c.err = ev.Err
	}
	return nil
}

// appendTo returns the trailing block of the given kind, creating one if the
// last block is of a different kind.
func (c *CollectSink) appendTo(kind string) *collectedBlock {
	if n := len(c.blocks); n > 0 && c.blocks[n-1].kind == kind {
		return c.blocks[n-1]
	}
	b := &collectedBlock{kind: kind}
	c.blocks = append(c.blocks, b)
	return b
}

// Err reports a backend-signaled error captured during collection.
func (c *CollectSink) Err() error { return c.err }

func (c *CollectSink) WriteResponse(w http.ResponseWriter) error {
	content := make([]map[string]any, 0, len(c.blocks))
	for _, b := range c.blocks {
		switch b.kind {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.text.String()})
		case "tool_use":
			input := json.RawMessage(b.text.String())
			if !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": b.toolID, "name": b.toolName, "input": input,
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stopReason := c.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
		if c.sawToolUse {
			stopReason = "tool_use"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id":   c.id,
		"type": "message", "role": "assistant", "model": c.model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usageJSON{InputTokens: c.usage.InputTokens, OutputTokens: c.usage.OutputTokens},
	})
}

// WriteError writes an Anthropic-shaped error body with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errorType(status), "message": msg},
	})
}

func errorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
