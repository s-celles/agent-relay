package anthropic

import (
	"context"
	"encoding/json"
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
			name: "string content",
			body: `{"model":"sonnet","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`,
			want: core.InferRequest{
				Model:     "sonnet",
				MaxTokens: 100,
				Messages:  []core.Message{{Role: core.RoleUser, Content: "hello"}},
			},
		},
		{
			name: "block content",
			body: `{"model":"sonnet","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			want: core.InferRequest{
				Model:     "sonnet",
				MaxTokens: 1,
				Messages:  []core.Message{{Role: core.RoleUser, Content: "a\nb"}},
			},
		},
		{
			name: "system string and stream flag",
			body: `{"model":"m","max_tokens":1,"stream":true,"system":"be brief","messages":[{"role":"user","content":"q"}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Stream:    true,
				System:    "be brief",
				Messages:  []core.Message{{Role: core.RoleUser, Content: "q"}},
			},
		},
		{
			name: "system blocks",
			body: `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"messages":[{"role":"user","content":"q"}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				System:    "one\ntwo",
				Messages:  []core.Message{{Role: core.RoleUser, Content: "q"}},
			},
		},
		{
			name: "multi turn",
			body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages: []core.Message{
					{Role: core.RoleUser, Content: "a"},
					{Role: core.RoleAssistant, Content: "b"},
					{Role: core.RoleUser, Content: "c"},
				},
			},
		},
		{name: "invalid role", body: `{"model":"m","max_tokens":1,"messages":[{"role":"tool","content":"x"}]}`, wantErr: true},
		{name: "empty messages", body: `{"model":"m","max_tokens":1,"messages":[]}`, wantErr: true},
		{name: "missing model", body: `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`, wantErr: true},
		{name: "malformed json", body: `{`, wantErr: true},
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
			assertInferEqual(t, got, tc.want)
		})
	}
}

func assertInferEqual(t *testing.T, got, want core.InferRequest) {
	t.Helper()
	if got.Model != want.Model || got.System != want.System ||
		got.Stream != want.Stream || got.MaxTokens != want.MaxTokens {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(want.Messages))
	}
	for i := range got.Messages {
		if got.Messages[i] != want.Messages[i] {
			t.Fatalf("message %d: got %+v, want %+v", i, got.Messages[i], want.Messages[i])
		}
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
	sink := NewStreamSink(rec, "msg_test", "sonnet")
	emitAll(t, sink, happyPath)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream (REQ-API-02)", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"content_block_delta"`,
		`"text":"Hel"`,
		`"text":"lo"`,
		"event: content_block_stop",
		`"stop_reason":"end_turn"`,
		`"output_tokens":5`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestStreamSinkError(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := NewStreamSink(rec, "msg_test", "sonnet")
	emitAll(t, sink, []core.Event{
		{Kind: core.EventMessageStart},
		{Kind: core.EventError, Err: errBackend},
	})
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "backend exploded") {
		t.Errorf("SSE body missing error event:\n%s", body)
	}
}

func TestCollectSinkResponse(t *testing.T) {
	sink := NewCollectSink("msg_test", "sonnet")
	emitAll(t, sink, happyPath)

	rec := httptest.NewRecorder()
	if err := sink.WriteResponse(rec); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	var resp struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.Type != "message" || resp.Role != "assistant" || resp.StopReason != "end_turn" {
		t.Errorf("envelope = %+v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello" {
		t.Errorf("content = %+v, want single text block %q", resp.Content, "Hello")
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}
