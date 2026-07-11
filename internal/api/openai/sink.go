package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

type chunkChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// StreamSink renders core events as Chat Completions SSE chunks, flushing
// after every event (REQ-API-02).
type StreamSink struct {
	w http.ResponseWriter
	// IncludeUsage mirrors stream_options.include_usage: when set, a final
	// chunk with empty choices carries token usage before [DONE].
	IncludeUsage bool
	id           string
	model        string
	created      int64
	started      bool
	usage        core.Usage
	// toolIndex numbers the tool calls of this turn: the OpenAI wire streams
	// them as an indexed array, unlike Anthropic's content blocks.
	toolIndex  int
	sawToolUse bool
}

func NewStreamSink(w http.ResponseWriter, id, model string) *StreamSink {
	return &StreamSink{w: w, id: id, model: model, created: time.Now().Unix()}
}

// Started reports whether any bytes have been written.
func (s *StreamSink) Started() bool { return s.started }

func (s *StreamSink) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventMessageStart:
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		return s.start()
	case core.EventTextDelta:
		if err := s.start(); err != nil {
			return err
		}
		return s.chunk(chunkChoice{Delta: map[string]any{"content": ev.Text}})
	case core.EventToolUseStart:
		if err := s.start(); err != nil {
			return err
		}
		s.sawToolUse = true
		defer func() { s.toolIndex++ }()
		return s.chunk(chunkChoice{Delta: map[string]any{
			"tool_calls": []map[string]any{{
				"index": s.toolIndex,
				"id":    ev.ToolID,
				"type":  "function",
				"function": map[string]any{
					"name": ev.ToolName, "arguments": "",
				},
			}},
		}})
	case core.EventToolUseDelta:
		if !s.sawToolUse {
			return nil
		}
		return s.chunk(chunkChoice{Delta: map[string]any{
			"tool_calls": []map[string]any{{
				"index":    s.toolIndex - 1,
				"function": map[string]any{"arguments": ev.Text},
			}},
		}})
	case core.EventToolUseStop:
		return nil
	case core.EventMessageStop:
		if err := s.start(); err != nil {
			return err
		}
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		stop := "stop"
		if s.sawToolUse {
			stop = "tool_calls"
		}
		if err := s.chunk(chunkChoice{Delta: map[string]any{}, FinishReason: &stop}); err != nil {
			return err
		}
		if s.IncludeUsage {
			if err := s.usageChunk(); err != nil {
				return err
			}
		}
		return s.raw("[DONE]")
	case core.EventError:
		if err := s.start(); err != nil {
			return err
		}
		data, err := json.Marshal(map[string]any{
			"error": map[string]any{"message": ev.Err.Error(), "type": "api_error"},
		})
		if err != nil {
			return err
		}
		return s.raw(string(data))
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
	return s.chunk(chunkChoice{Delta: map[string]any{"role": "assistant"}})
}

// usageChunk emits the final usage-carrying chunk (empty choices), per
// stream_options.include_usage.
func (s *StreamSink) usageChunk() error {
	data, err := json.Marshal(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []chunkChoice{},
		"usage": map[string]int{
			"prompt_tokens":     s.usage.InputTokens,
			"completion_tokens": s.usage.OutputTokens,
			"total_tokens":      s.usage.InputTokens + s.usage.OutputTokens,
		},
	})
	if err != nil {
		return err
	}
	return s.raw(string(data))
}

func (s *StreamSink) chunk(choice chunkChoice) error {
	data, err := json.Marshal(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []chunkChoice{choice},
	})
	if err != nil {
		return err
	}
	return s.raw(string(data))
}

func (s *StreamSink) raw(data string) error {
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// CollectSink accumulates the whole event stream for a non-streaming
// chat.completion response.
// collectedToolCall accumulates one tool call for a non-streaming response.
type collectedToolCall struct {
	id   string
	name string
	args strings.Builder
}

type CollectSink struct {
	id        string
	model     string
	text      strings.Builder
	toolCalls []*collectedToolCall
	usage     core.Usage
	err       error
}

func NewCollectSink(id, model string) *CollectSink {
	return &CollectSink{id: id, model: model}
}

func (c *CollectSink) Emit(ctx context.Context, ev core.Event) error {
	switch ev.Kind {
	case core.EventTextDelta:
		c.text.WriteString(ev.Text)
	case core.EventToolUseStart:
		c.toolCalls = append(c.toolCalls, &collectedToolCall{id: ev.ToolID, name: ev.ToolName})
	case core.EventToolUseDelta:
		if n := len(c.toolCalls); n > 0 {
			c.toolCalls[n-1].args.WriteString(ev.Text)
		}
	case core.EventMessageStart, core.EventMessageStop:
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
	message := map[string]any{"role": "assistant", "content": c.text.String()}
	finishReason := "stop"
	if len(c.toolCalls) > 0 {
		calls := make([]map[string]any, 0, len(c.toolCalls))
		for _, tc := range c.toolCalls {
			args := tc.args.String()
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id": tc.id, "type": "function",
				"function": map[string]any{"name": tc.name, "arguments": args},
			})
		}
		message["tool_calls"] = calls
		finishReason = "tool_calls"
		// The wire says content is null when the turn is only tool calls.
		if c.text.Len() == 0 {
			message["content"] = nil
		}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"id":      c.id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   c.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens":     c.usage.InputTokens,
			"completion_tokens": c.usage.OutputTokens,
			"total_tokens":      c.usage.InputTokens + c.usage.OutputTokens,
		},
	})
}

// WriteError writes an OpenAI-shaped error body with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
}
