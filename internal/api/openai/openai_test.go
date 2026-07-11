package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/s-celles/agent-relay/internal/core"
)

func TestDecodeRequest(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    core.InferRequest
		wantErr bool
	}{
		{
			name: "basic",
			body: `{"model":"sonnet","messages":[{"role":"user","content":"hello"}]}`,
			want: core.InferRequest{
				Model:    "sonnet",
				Messages: []core.Message{core.NewTextMessage(core.RoleUser, "hello")},
			},
		},
		{
			name: "system message maps to System",
			body: `{"model":"m","stream":true,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"q"}]}`,
			want: core.InferRequest{
				Model:    "m",
				Stream:   true,
				System:   "be brief",
				Messages: []core.Message{core.NewTextMessage(core.RoleUser, "q")},
			},
		},
		{
			name: "assistant tool_calls and tool message",
			body: `{"model":"m","messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":"","tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
				]},
				{"role":"tool","tool_call_id":"call_1","content":"sunny"}
			]}`,
			want: core.InferRequest{
				Model: "m",
				Messages: []core.Message{
					core.NewTextMessage(core.RoleUser, "weather?"),
					{Role: core.RoleAssistant, Blocks: []core.Block{
						{Kind: core.BlockToolUse, ToolID: "call_1", ToolName: "get_weather", ToolInput: []byte(`{"city":"Paris"}`)},
					}},
					{Role: core.RoleUser, Blocks: []core.Block{
						{Kind: core.BlockToolResult, ToolID: "call_1", Text: "sunny"},
					}},
				},
			},
		},
		{
			name: "function tools map to core tools",
			body: `{"model":"m","messages":[{"role":"user","content":"q"}],"tools":[
				{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}
			]}`,
			want: core.InferRequest{
				Model:    "m",
				Messages: []core.Message{core.NewTextMessage(core.RoleUser, "q")},
				Tools: []core.Tool{{
					Name:        "get_weather",
					Description: "d",
					InputSchema: []byte(`{"type":"object"}`),
				}},
			},
		},
		{
			name: "max_completion_tokens preferred over legacy max_tokens",
			body: `{"model":"m","max_tokens":10,"max_completion_tokens":42,"messages":[{"role":"user","content":"q"}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 42,
				Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "q")},
			},
		},
		{name: "invalid role", body: `{"model":"m","messages":[{"role":"function","content":"x"}]}`, wantErr: true},
		{name: "no conversation messages", body: `{"model":"m","messages":[{"role":"system","content":"x"}]}`, wantErr: true},
		{name: "malformed json", body: `[`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeRequest(strings.NewReader(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if got.Model != tc.want.Model || got.System != tc.want.System ||
				got.Stream != tc.want.Stream || got.MaxTokens != tc.want.MaxTokens {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if len(got.Tools) != len(tc.want.Tools) {
				t.Fatalf("got %d tools, want %d", len(got.Tools), len(tc.want.Tools))
			}
			for i := range got.Tools {
				if got.Tools[i].Name != tc.want.Tools[i].Name {
					t.Fatalf("tool %d = %+v", i, got.Tools[i])
				}
			}
			if len(got.Messages) != len(tc.want.Messages) {
				t.Fatalf("got %d messages, want %d", len(got.Messages), len(tc.want.Messages))
			}
			for i := range got.Messages {
				g, w := got.Messages[i], tc.want.Messages[i]
				if g.Role != w.Role || len(g.Blocks) != len(w.Blocks) {
					t.Fatalf("message %d: got %+v, want %+v", i, g, w)
				}
				for j := range g.Blocks {
					gb, wb := g.Blocks[j], w.Blocks[j]
					if gb.Kind != wb.Kind || gb.Text != wb.Text || gb.ToolID != wb.ToolID || gb.ToolName != wb.ToolName {
						t.Fatalf("message %d block %d: got %+v, want %+v", i, j, gb, wb)
					}
				}
			}
		})
	}
}

func emitAll(t *testing.T, sink core.EventSink, events []core.Event) {
	t.Helper()
	for _, ev := range events {
		if err := sink.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
}

var happyPath = []core.Event{
	{Kind: core.EventMessageStart},
	{Kind: core.EventTextDelta, Text: "Hel"},
	{Kind: core.EventTextDelta, Text: "lo"},
	{Kind: core.EventMessageStop, Usage: &core.Usage{InputTokens: 3, OutputTokens: 5}},
}

func TestStreamSinkSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := NewStreamSink(rec, "chatcmpl-test", "sonnet")
	emitAll(t, sink, happyPath)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"object":"chat.completion.chunk"`,
		`"content":"Hel"`,
		`"content":"lo"`,
		`"finish_reason":"stop"`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestStreamSinkError(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := NewStreamSink(rec, "chatcmpl-test", "sonnet")
	emitAll(t, sink, []core.Event{
		{Kind: core.EventMessageStart},
		{Kind: core.EventError, Err: errors.New("backend exploded")},
	})
	if body := rec.Body.String(); !strings.Contains(body, "backend exploded") {
		t.Errorf("SSE body missing error payload:\n%s", body)
	}
}

func TestCollectSinkResponse(t *testing.T) {
	sink := NewCollectSink("chatcmpl-test", "sonnet")
	emitAll(t, sink, happyPath)

	rec := httptest.NewRecorder()
	if err := sink.WriteResponse(rec); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello" ||
		resp.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v", resp.Choices)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}
