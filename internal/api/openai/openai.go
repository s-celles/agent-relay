// Package openai translates the OpenAI Chat Completions wire format to and
// from the neutral core model (REQ-API-03).
package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/s-celles/agent-relay/internal/core"
)

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	// MaxTokens is the legacy parameter; current OpenAI SDKs send
	// max_completion_tokens instead, which takes precedence when set.
	MaxTokens           int        `json:"max_tokens"`
	MaxCompletionTokens int        `json:"max_completion_tokens"`
	Stream              bool       `json:"stream"`
	Tools               []wireTool `json:"tools"`
	Temperature         *float64   `json:"temperature"`
	TopP                *float64   `json:"top_p"`
	Stop                []string   `json:"stop"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// DecodeRequest parses a POST /v1/chat/completions body into a
// core.InferRequest. OpenAI system messages map onto the neutral System
// field; `tool` messages become tool_result blocks in user turns; assistant
// `tool_calls` become tool_use blocks.
func DecodeRequest(r io.Reader) (core.InferRequest, error) {
	var wire chatRequest
	if err := json.NewDecoder(r).Decode(&wire); err != nil {
		return core.InferRequest{}, fmt.Errorf("malformed request body: %w", err)
	}
	if wire.Model == "" {
		return core.InferRequest{}, errors.New("model is required")
	}

	req := core.InferRequest{
		Model:         wire.Model,
		MaxTokens:     wire.MaxTokens,
		Stream:        wire.Stream,
		Temperature:   wire.Temperature,
		TopP:          wire.TopP,
		StopSequences: wire.Stop,
	}
	if wire.MaxCompletionTokens > 0 {
		req.MaxTokens = wire.MaxCompletionTokens
	}
	if wire.StreamOptions != nil {
		req.IncludeUsage = wire.StreamOptions.IncludeUsage
	}
	for _, t := range wire.Tools {
		if t.Type != "function" {
			return core.InferRequest{}, fmt.Errorf("unsupported tool type %q", t.Type)
		}
		req.Tools = append(req.Tools, core.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	var system []string
	for i, m := range wire.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content)
		case "user":
			req.Messages = append(req.Messages, core.NewTextMessage(core.RoleUser, m.Content))
		case "assistant":
			var blocks []core.Block
			if m.Content != "" {
				blocks = append(blocks, core.Block{Kind: core.BlockText, Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, core.Block{
					Kind:      core.BlockToolUse,
					ToolID:    tc.ID,
					ToolName:  tc.Function.Name,
					ToolInput: json.RawMessage(tc.Function.Arguments),
				})
			}
			req.Messages = append(req.Messages, core.Message{Role: core.RoleAssistant, Blocks: blocks})
		case "tool":
			req.Messages = append(req.Messages, core.Message{Role: core.RoleUser, Blocks: []core.Block{{
				Kind:   core.BlockToolResult,
				ToolID: m.ToolCallID,
				Text:   m.Content,
			}}})
		default:
			return core.InferRequest{}, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	req.System = strings.Join(system, "\n")
	if len(req.Messages) == 0 {
		return core.InferRequest{}, errors.New("at least one user, assistant, or tool message is required")
	}
	return req, nil
}
