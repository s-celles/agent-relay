package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/s-celles/agent-relay/internal/core"
)

func TestDecodeRequestFileSizeLimit(t *testing.T) {
	old := maxFileBytes
	maxFileBytes = 4
	defer func() { maxFileBytes = old }()

	body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAECAwQ="}}
	]}]}` // 5 decoded bytes > limit of 4
	if _, err := DecodeRequest(strings.NewReader(body)); err == nil {
		t.Fatal("oversized file block must be rejected")
	}
}

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
				Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "hello")},
			},
		},
		{
			name: "block content keeps separate text blocks",
			body: `{"model":"sonnet","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			want: core.InferRequest{
				Model:     "sonnet",
				MaxTokens: 1,
				Messages: []core.Message{{Role: core.RoleUser, Blocks: []core.Block{
					{Kind: core.BlockText, Text: "a"},
					{Kind: core.BlockText, Text: "b"},
				}}},
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
				Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "q")},
			},
		},
		{
			name: "system blocks",
			body: `{"model":"m","max_tokens":1,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"messages":[{"role":"user","content":"q"}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				System:    "one\ntwo",
				Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "q")},
			},
		},
		{
			name: "tool_use and tool_result blocks round-trip",
			body: `{"model":"m","max_tokens":1,"messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":[
					{"type":"text","text":"Checking."},
					{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}
				]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny","is_error":false}
				]}
			]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages: []core.Message{
					core.NewTextMessage(core.RoleUser, "weather?"),
					{Role: core.RoleAssistant, Blocks: []core.Block{
						{Kind: core.BlockText, Text: "Checking."},
						{Kind: core.BlockToolUse, ToolID: "toolu_1", ToolName: "get_weather", ToolInput: []byte(`{"city":"Paris"}`)},
					}},
					{Role: core.RoleUser, Blocks: []core.Block{
						{Kind: core.BlockToolResult, ToolID: "toolu_1", Text: "sunny"},
					}},
				},
			},
		},
		{
			name: "tool_result with block content and error flag",
			body: `{"model":"m","max_tokens":1,"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"boom"}],"is_error":true}
				]}
			]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages: []core.Message{
					{Role: core.RoleUser, Blocks: []core.Block{
						{Kind: core.BlockToolResult, ToolID: "toolu_2", Text: "boom", IsError: true},
					}},
				},
			},
		},
		{
			name: "tools and tool_choice",
			body: `{"model":"m","max_tokens":1,"tool_choice":{"type":"auto"},"tools":[
				{"name":"get_weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}
			],"messages":[{"role":"user","content":"q"}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "q")},
				Tools: []core.Tool{{
					Name:        "get_weather",
					Description: "Get weather",
					InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
				}},
				ToolChoice: "auto",
			},
		},
		{
			name: "thinking blocks are dropped silently",
			body: `{"model":"m","max_tokens":1,"messages":[
				{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"text","text":"ok"}]},
				{"role":"user","content":"next"}
			]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages: []core.Message{
					{Role: core.RoleAssistant, Blocks: []core.Block{{Kind: core.BlockText, Text: "ok"}}},
					core.NewTextMessage(core.RoleUser, "next"),
				},
			},
		},
		{
			name: "image block decodes to a file block",
			body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAEC"}},
				{"type":"text","text":"describe this"}
			]}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages: []core.Message{
					{Role: core.RoleUser, Blocks: []core.Block{
						{Kind: core.BlockFile, MediaType: "image/png", Data: []byte{0x00, 0x01, 0x02}},
						{Kind: core.BlockText, Text: "describe this"},
					}},
				},
			},
		},
		{
			name: "pdf document block decodes to a file block",
			body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERg=="}}
			]}]}`,
			want: core.InferRequest{
				Model:     "m",
				MaxTokens: 1,
				Messages: []core.Message{
					{Role: core.RoleUser, Blocks: []core.Block{
						{Kind: core.BlockFile, MediaType: "application/pdf", Data: []byte("%PDF")},
					}},
				},
			},
		},
		{name: "url image source rejected", body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]}]}`, wantErr: true},
		{name: "unsupported media type rejected", body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/tiff","data":"AA=="}}]}]}`, wantErr: true},
		{name: "invalid base64 rejected", body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"not base64!!!"}}]}]}`, wantErr: true},
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
		got.Stream != want.Stream || got.MaxTokens != want.MaxTokens ||
		got.ToolChoice != want.ToolChoice {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if len(got.Tools) != len(want.Tools) {
		t.Fatalf("got %d tools, want %d", len(got.Tools), len(want.Tools))
	}
	for i := range got.Tools {
		if got.Tools[i].Name != want.Tools[i].Name ||
			got.Tools[i].Description != want.Tools[i].Description ||
			!jsonEqual(got.Tools[i].InputSchema, want.Tools[i].InputSchema) {
			t.Fatalf("tool %d: got %+v, want %+v", i, got.Tools[i], want.Tools[i])
		}
	}
	if len(got.Messages) != len(want.Messages) {
		t.Fatalf("got %d messages, want %d", len(got.Messages), len(want.Messages))
	}
	for i := range got.Messages {
		assertMessageEqual(t, i, got.Messages[i], want.Messages[i])
	}
}

func assertMessageEqual(t *testing.T, i int, got, want core.Message) {
	t.Helper()
	if got.Role != want.Role {
		t.Fatalf("message %d: role %q, want %q", i, got.Role, want.Role)
	}
	if len(got.Blocks) != len(want.Blocks) {
		t.Fatalf("message %d: got %d blocks (%+v), want %d", i, len(got.Blocks), got.Blocks, len(want.Blocks))
	}
	for j, b := range got.Blocks {
		w := want.Blocks[j]
		if b.Kind != w.Kind || b.Text != w.Text || b.ToolID != w.ToolID ||
			b.ToolName != w.ToolName || b.IsError != w.IsError || !jsonEqual(b.ToolInput, w.ToolInput) ||
			b.MediaType != w.MediaType || !bytes.Equal(b.Data, w.Data) {
			t.Fatalf("message %d block %d: got %+v, want %+v", i, j, b, w)
		}
	}
}

func jsonEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return string(a) == string(b)
	}
	na, _ := json.Marshal(va)
	nb, _ := json.Marshal(vb)
	return string(na) == string(nb)
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

var toolUsePath = []core.Event{
	{Kind: core.EventMessageStart},
	{Kind: core.EventTextDelta, Text: "Checking."},
	{Kind: core.EventToolUseStart, ToolID: "toolu_1", ToolName: "get_weather"},
	{Kind: core.EventToolUseDelta, Text: `{"city":`},
	{Kind: core.EventToolUseDelta, Text: `"Paris"}`},
	{Kind: core.EventToolUseStop},
	{Kind: core.EventMessageStop, StopReason: "tool_use", Usage: &core.Usage{InputTokens: 3, OutputTokens: 5}},
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

func TestStreamSinkToolUse(t *testing.T) {
	rec := httptest.NewRecorder()
	sink := NewStreamSink(rec, "msg_test", "sonnet")
	emitAll(t, sink, toolUsePath)

	body := rec.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"id":"toolu_1"`,
		`"name":"get_weather"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"city\":"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE body missing %q\nbody:\n%s", want, body)
		}
	}
	// The text block is index 0, the tool_use block index 1.
	if !strings.Contains(body, `"index":1`) {
		t.Errorf("SSE body missing second block index:\n%s", body)
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
		Type       string `json:"type"`
		Role       string `json:"role"`
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

func TestCollectSinkToolUse(t *testing.T) {
	sink := NewCollectSink("msg_test", "sonnet")
	emitAll(t, sink, toolUsePath)

	rec := httptest.NewRecorder()
	if err := sink.WriteResponse(rec); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content = %+v, want text + tool_use", resp.Content)
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text != "Checking." {
		t.Errorf("content[0] = %+v", resp.Content[0])
	}
	tu := resp.Content[1]
	if tu.Type != "tool_use" || tu.ID != "toolu_1" || tu.Name != "get_weather" {
		t.Errorf("content[1] = %+v", tu)
	}
	if !jsonEqual(tu.Input, []byte(`{"city":"Paris"}`)) {
		t.Errorf("tool input = %s", tu.Input)
	}
}
