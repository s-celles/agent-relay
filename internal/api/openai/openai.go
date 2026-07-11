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

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Stream    bool          `json:"stream"`
}

// DecodeRequest parses a POST /v1/chat/completions body into a
// core.InferRequest. OpenAI system messages map onto the neutral System
// field; multiple system messages are concatenated.
func DecodeRequest(r io.Reader) (core.InferRequest, error) {
	var wire chatRequest
	if err := json.NewDecoder(r).Decode(&wire); err != nil {
		return core.InferRequest{}, fmt.Errorf("malformed request body: %w", err)
	}
	if wire.Model == "" {
		return core.InferRequest{}, errors.New("model is required")
	}

	req := core.InferRequest{
		Model:     wire.Model,
		MaxTokens: wire.MaxTokens,
		Stream:    wire.Stream,
	}
	var system []string
	for i, m := range wire.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content)
		case "user":
			req.Messages = append(req.Messages, core.Message{Role: core.RoleUser, Content: m.Content})
		case "assistant":
			req.Messages = append(req.Messages, core.Message{Role: core.RoleAssistant, Content: m.Content})
		default:
			return core.InferRequest{}, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	req.System = strings.Join(system, "\n")
	if len(req.Messages) == 0 {
		return core.InferRequest{}, errors.New("at least one user or assistant message is required")
	}
	return req, nil
}
