// Package anthropic translates the Anthropic Messages wire format to and
// from the neutral core model (REQ-API-01).
package anthropic

import (
	"encoding/base64"
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

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesRequest struct {
	Model      string          `json:"model"`
	MaxTokens  int             `json:"max_tokens"`
	System     json.RawMessage `json:"system"`
	Messages   []wireMessage   `json:"messages"`
	Stream     bool            `json:"stream"`
	Tools      []wireTool      `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// wireBlock is the union of the content block shapes the relay understands.
type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *wireSource     `json:"source"`
}

type wireSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// maxFileBytes bounds the decoded size of one image/document block (matches
// the Anthropic API's practical attachment sizes; var for tests).
var maxFileBytes = 20 << 20

var allowedMediaTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
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

	for _, t := range wire.Tools {
		req.Tools = append(req.Tools, core.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	if len(wire.ToolChoice) > 0 {
		var tc struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(wire.ToolChoice, &tc); err == nil {
			req.ToolChoice = tc.Type
		}
	}

	for i, m := range wire.Messages {
		role := core.Role(m.Role)
		if role != core.RoleUser && role != core.RoleAssistant {
			return core.InferRequest{}, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
		blocks, err := decodeBlocks(m.Content)
		if err != nil {
			return core.InferRequest{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, core.Message{Role: role, Blocks: blocks})
	}
	return req, nil
}

// decodeBlocks accepts a bare string or an array of content blocks and maps
// them onto the neutral block model.
func decodeBlocks(raw json.RawMessage) ([]core.Block, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []core.Block{{Kind: core.BlockText, Text: s}}, nil
	}
	var wire []wireBlock
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of blocks: %w", err)
	}
	var blocks []core.Block
	for _, b := range wire {
		switch b.Type {
		case "text":
			blocks = append(blocks, core.Block{Kind: core.BlockText, Text: b.Text})
		case "tool_use":
			blocks = append(blocks, core.Block{
				Kind:      core.BlockToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
		case "tool_result":
			text, err := toolResultText(b.Content)
			if err != nil {
				return nil, fmt.Errorf("tool_result %q: %w", b.ToolUseID, err)
			}
			blocks = append(blocks, core.Block{
				Kind:    core.BlockToolResult,
				ToolID:  b.ToolUseID,
				Text:    text,
				IsError: b.IsError,
			})
		case "image", "document":
			fb, err := decodeFileBlock(b)
			if err != nil {
				return nil, fmt.Errorf("%s block: %w", b.Type, err)
			}
			blocks = append(blocks, fb)
		case "thinking", "redacted_thinking":
			// Clients echo thinking blocks back per the Messages API contract;
			// the relay has nothing to do with them — drop silently.
		default:
			return nil, fmt.Errorf("unsupported content block type %q", b.Type)
		}
	}
	return blocks, nil
}

// decodeFileBlock turns a base64 image/document block into a neutral file
// block. Only base64 sources are supported (no URL fetching).
func decodeFileBlock(b wireBlock) (core.Block, error) {
	if b.Source == nil || b.Source.Type != "base64" {
		return core.Block{}, errors.New("only base64 sources are supported")
	}
	if !allowedMediaTypes[b.Source.MediaType] {
		return core.Block{}, fmt.Errorf("unsupported media type %q", b.Source.MediaType)
	}
	if base64.StdEncoding.DecodedLen(len(b.Source.Data)) > maxFileBytes {
		return core.Block{}, fmt.Errorf("attachment exceeds the %d-byte limit", maxFileBytes)
	}
	data, err := base64.StdEncoding.DecodeString(b.Source.Data)
	if err != nil {
		return core.Block{}, fmt.Errorf("invalid base64 data: %w", err)
	}
	if len(data) > maxFileBytes {
		return core.Block{}, fmt.Errorf("attachment exceeds the %d-byte limit", maxFileBytes)
	}
	return core.Block{Kind: core.BlockFile, MediaType: b.Source.MediaType, Data: data}, nil
}

// toolResultText flattens a tool_result's content (absent, string, or an
// array of text blocks) into plain text.
func toolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	return textFromStringOrBlocks(raw)
}

// textFromStringOrBlocks accepts a bare string or an array of typed text
// blocks and joins them.
func textFromStringOrBlocks(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []wireBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("expected a string or an array of text blocks: %w", err)
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("unsupported block type %q (text only here)", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}
