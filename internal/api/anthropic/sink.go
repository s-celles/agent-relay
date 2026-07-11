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
// every event (REQ-API-02).
type StreamSink struct {
	w       http.ResponseWriter
	id      string
	model   string
	started bool
	usage   core.Usage
}

func NewStreamSink(w http.ResponseWriter, id, model string) *StreamSink {
	return &StreamSink{w: w, id: id, model: model}
}

// Started reports whether any bytes have been written; once true, errors can
// only be delivered in-stream, not as an HTTP status.
func (s *StreamSink) Started() bool { return s.started }

func (s *StreamSink) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventMessageStart:
		return s.start()
	case core.EventTextDelta:
		// Tolerate backends that never emit an explicit start (DQ-1).
		if err := s.start(); err != nil {
			return err
		}
		return s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case core.EventUsage, core.EventMessageStop:
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		if ev.Kind == core.EventUsage {
			return nil
		}
		return s.stop()
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
	if err := s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.id, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": usageJSON{},
		},
	}); err != nil {
		return err
	}
	return s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *StreamSink) stop() error {
	if err := s.start(); err != nil {
		return err
	}
	if err := s.event("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}); err != nil {
		return err
	}
	if err := s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
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

// CollectSink accumulates the whole event stream for a non-streaming
// response body.
type CollectSink struct {
	id    string
	model string
	text  strings.Builder
	usage core.Usage
	err   error
}

func NewCollectSink(id, model string) *CollectSink {
	return &CollectSink{id: id, model: model}
}

func (c *CollectSink) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventTextDelta:
		c.text.WriteString(ev.Text)
	case core.EventUsage, core.EventMessageStop:
		if ev.Usage != nil {
			c.usage = *ev.Usage
		}
	case core.EventError:
		c.err = ev.Err
	}
	return nil
}

// Err reports a backend-signaled error captured during collection.
func (c *CollectSink) Err() error { return c.err }

func (c *CollectSink) WriteResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id":   c.id,
		"type": "message", "role": "assistant", "model": c.model,
		"content":       []map[string]any{{"type": "text", "text": c.text.String()}},
		"stop_reason":   "end_turn",
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
