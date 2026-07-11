// Package anthropic translates the Anthropic Messages wire format to and
// from the neutral core model (REQ-API-01).
package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/s-celles/agent-relay/internal/core"
)

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []wireMessage   `json:"messages"`
	Stream    bool            `json:"stream"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// DecodeRequest parses a POST /v1/messages body into a core.InferRequest.
func DecodeRequest(r io.Reader) (core.InferRequest, error) {
	var wire messagesRequest
	dec := json.NewDecoder(r)
	if err := dec.Decode(&wire); err != nil {
		return core.InferRequest{}, fmt.Errorf("malformed request body: %w", err)
	}
	if wire.Model == "" {
		return core.InferRequest{}, errors.New("model is required")
	}
	if len(wire.Messages) == 0 {
		return core.InferRequest{}, errors.New("messages must not be empty")
	}

	req := core.InferRequest{
		Model:     wire.Model,
		MaxTokens: wire.MaxTokens,
		Stream:    wire.Stream,
	}

	if len(wire.System) > 0 {
		system, err := textFromStringOrBlocks(wire.System)
		if err != nil {
			return core.InferRequest{}, fmt.Errorf("system: %w", err)
		}
		req.System = system
	}

	for i, m := range wire.Messages {
		role := core.Role(m.Role)
		if role != core.RoleUser && role != core.RoleAssistant {
			return core.InferRequest{}, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
		text, err := textFromStringOrBlocks(m.Content)
		if err != nil {
			return core.InferRequest{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, core.Message{Role: role, Content: text})
	}
	return req, nil
}

// textFromStringOrBlocks accepts the two shapes the Messages API allows for
// content: a bare string, or an array of typed blocks. v1 keeps text blocks
// and joins them; other block types are rejected explicitly.
func textFromStringOrBlocks(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("content must be a string or an array of text blocks: %w", err)
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("unsupported content block type %q (v1 is text-only)", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}
