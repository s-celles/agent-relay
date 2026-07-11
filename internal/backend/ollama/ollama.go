// Package ollama adapts a local Ollama server to the neutral core.Backend
// interface. It is the module's second backend, and deliberately a different
// *kind* of one: an HTTP client rather than a supervised subprocess. If the
// neutral model holds for both, it holds generally (REQ-BK-03).
//
// Unlike the claude CLI, Ollama honors max_tokens and the sampling
// parameters, and it calls client-defined tools natively — but only on models
// that support them (llama3 does not; qwen3.5 does), so tool errors are
// surfaced to the caller as the server reports them.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

func init() {
	core.Register("ollama", New)
}

const defaultBaseURL = "http://127.0.0.1:11434"

type Backend struct {
	baseURL  string
	modelMap map[string]string
	client   *http.Client
}

func New(cfg core.BackendConfig) (core.Backend, error) {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return &Backend{
		baseURL:  strings.TrimSuffix(base, "/"),
		modelMap: cfg.ModelMap,
		// Local inference on a big model can be slow; the per-request
		// deadline in ctx is the real bound.
		client: &http.Client{Timeout: 0},
	}, nil
}

func (b *Backend) Name() string { return "ollama" }

func (b *Backend) Capabilities() core.Capabilities {
	models := make([]string, 0, len(b.modelMap))
	for logical := range b.modelMap {
		models = append(models, logical)
	}
	return core.Capabilities{
		Streaming: true,
		Agentic:   false, // no host-side tools: Ollama only generates
		// Ollama honors all three, unlike the CLI backend. Tool support is
		// per model, and the server says so when a model cannot.
		ClientTools: true,
		MaxTokens:   true,
		Sampling:    true,
		Models:      models,
	}
}

func (b *Backend) mapModel(logical string) string {
	if mapped, ok := b.modelMap[logical]; ok {
		return mapped
	}
	return logical
}

// wire shapes ------------------------------------------------------------

type wireToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Images    []string       `json:"images,omitempty"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

type chatRequest struct {
	Model    string         `json:"model"`
	Messages []wireMessage  `json:"messages"`
	Stream   bool           `json:"stream"`
	Tools    []any          `json:"tools,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type chatChunk struct {
	Message    wireMessage `json:"message"`
	Done       bool        `json:"done"`
	DoneReason string      `json:"done_reason"`
	PromptEval int         `json:"prompt_eval_count"`
	Eval       int         `json:"eval_count"`
	Error      string      `json:"error"`
}

// buildRequest maps the neutral request onto Ollama's chat API.
func (b *Backend) buildRequest(req core.InferRequest) chatRequest {
	out := chatRequest{Model: b.mapModel(req.Model), Stream: true}

	if req.System != "" {
		out.Messages = append(out.Messages, wireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, encodeMessages(m)...)
	}

	for _, t := range req.Tools {
		schema := t.InputSchema
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}

	opts := map[string]any{}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		opts["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		opts["stop"] = req.StopSequences
	}
	if len(opts) > 0 {
		out.Options = opts
	}
	return out
}

// encodeMessages renders one neutral message as Ollama messages: tool
// results become their own `tool` message, images ride along as base64.
func encodeMessages(m core.Message) []wireMessage {
	var out []wireMessage
	msg := wireMessage{Role: string(m.Role)}
	var texts []string

	for _, bl := range m.Blocks {
		switch bl.Kind {
		case core.BlockText:
			texts = append(texts, bl.Text)
		case core.BlockFile:
			if strings.HasPrefix(bl.MediaType, "image/") {
				msg.Images = append(msg.Images, base64.StdEncoding.EncodeToString(bl.Data))
			}
		case core.BlockToolUse:
			tc := wireToolCall{ID: bl.ToolID}
			tc.Function.Name = bl.ToolName
			tc.Function.Arguments = bl.ToolInput
			msg.ToolCalls = append(msg.ToolCalls, tc)
		case core.BlockToolResult:
			out = append(out, wireMessage{Role: "tool", Content: bl.Text})
		}
	}

	msg.Content = strings.Join(texts, "\n")
	if msg.Content != "" || len(msg.Images) > 0 || len(msg.ToolCalls) > 0 {
		out = append([]wireMessage{msg}, out...)
	}
	return out
}

func (b *Backend) Infer(ctx context.Context, req core.InferRequest, sink core.EventSink) error {
	if req.Agentic {
		return errors.New("agentic request refused: the ollama backend has no host-side execution")
	}

	body, err := json.Marshal(b.buildRequest(req))
	if err != nil {
		return fmt.Errorf("encode ollama request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := sink.Emit(ctx, core.Event{Kind: core.EventMessageStart}); err != nil {
		return err
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)
	sawTool := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue // parse defensively, as with the CLI backend (DQ-1)
		}

		// Ollama reports refusals in-band (e.g. "does not support tools").
		if chunk.Error != "" {
			return sink.Emit(ctx, core.Event{Kind: core.EventError, Err: errors.New(chunk.Error)})
		}

		if chunk.Message.Content != "" {
			if err := sink.Emit(ctx, core.Event{
				Kind: core.EventTextDelta, Text: chunk.Message.Content,
			}); err != nil {
				return err
			}
		}
		for _, tc := range chunk.Message.ToolCalls {
			sawTool = true
			if err := emitToolCall(ctx, sink, tc); err != nil {
				return err
			}
		}

		if chunk.Done {
			stop := ""
			if sawTool {
				stop = "tool_use"
			}
			return sink.Emit(ctx, core.Event{
				Kind:       core.EventMessageStop,
				StopReason: stop,
				Usage: &core.Usage{
					InputTokens:  chunk.PromptEval,
					OutputTokens: chunk.Eval,
				},
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read ollama stream: %w", err)
	}
	// The stream ended without a done chunk.
	return sink.Emit(ctx, core.Event{Kind: core.EventMessageStop})
}

func emitToolCall(ctx context.Context, sink core.EventSink, tc wireToolCall) error {
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	if err := sink.Emit(ctx, core.Event{
		Kind: core.EventToolUseStart, ToolID: id, ToolName: tc.Function.Name,
	}); err != nil {
		return err
	}
	input := tc.Function.Arguments
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}
	if err := sink.Emit(ctx, core.Event{
		Kind: core.EventToolUseDelta, Text: string(input),
	}); err != nil {
		return err
	}
	return sink.Emit(ctx, core.Event{Kind: core.EventToolUseStop})
}
