package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/s-celles/agent-relay/internal/core"
)

type collectSink struct{ events []core.Event }

func (c *collectSink) Emit(ctx context.Context, ev core.Event) error {
	c.events = append(c.events, ev)
	return nil
}

// fakeOllama serves canned NDJSON and captures the request it received.
func fakeOllama(t *testing.T, lines ...string) (*httptest.Server, *map[string]any) {
	t.Helper()
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, l := range lines {
			io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newBackend(t *testing.T, url string) core.Backend {
	t.Helper()
	b, err := New(core.BackendConfig{BaseURL: url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestInferStreamsTextAndUsage(t *testing.T) {
	srv, got := fakeOllama(t,
		`{"message":{"role":"assistant","content":"Bon"},"done":false}`,
		`{"message":{"role":"assistant","content":"jour"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":14,"eval_count":10}`,
	)
	b := newBackend(t, srv.URL)
	sink := &collectSink{}

	err := b.Infer(context.Background(), core.InferRequest{
		Model:    "llama3",
		System:   "be brief",
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "salut")},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	var text strings.Builder
	var kinds []core.EventKind
	for _, ev := range sink.events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == core.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "Bonjour" {
		t.Errorf("text = %q", text.String())
	}
	if kinds[0] != core.EventMessageStart || kinds[len(kinds)-1] != core.EventMessageStop {
		t.Errorf("kinds = %v", kinds)
	}
	last := sink.events[len(sink.events)-1]
	if last.Usage == nil || last.Usage.InputTokens != 14 || last.Usage.OutputTokens != 10 {
		t.Errorf("usage = %+v", last.Usage)
	}

	// The system prompt becomes a system message; the request streams.
	msgs := (*got)["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("messages[0] = %v", first)
	}
	if (*got)["stream"] != true {
		t.Errorf("stream = %v, want true", (*got)["stream"])
	}
}

func TestInferHonorsMaxTokensAndSampling(t *testing.T) {
	srv, got := fakeOllama(t, `{"message":{"content":"x"},"done":true,"prompt_eval_count":1,"eval_count":1}`)
	b := newBackend(t, srv.URL)

	temp, topP := 0.3, 0.9
	topK := 40
	err := b.Infer(context.Background(), core.InferRequest{
		Model:         "llama3",
		MaxTokens:     128,
		Temperature:   &temp,
		TopP:          &topP,
		TopK:          &topK,
		StopSequences: []string{"END"},
		Messages:      []core.Message{core.NewTextMessage(core.RoleUser, "hi")},
	}, &collectSink{})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	opts, ok := (*got)["options"].(map[string]any)
	if !ok {
		t.Fatalf("no options in request: %v", *got)
	}
	if opts["num_predict"] != float64(128) {
		t.Errorf("num_predict = %v, want 128", opts["num_predict"])
	}
	if opts["temperature"] != 0.3 || opts["top_p"] != 0.9 || opts["top_k"] != float64(40) {
		t.Errorf("sampling options = %v", opts)
	}
	stop := opts["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", stop)
	}
}

func TestInferDisablesThinking(t *testing.T) {
	// Thinking models (qwen3.x) put their reasoning in Ollama's `thinking`
	// field and the answer in `content` only once thinking ends. The relay
	// surfaces `content` alone, so with thinking on the caller sees nothing:
	// a non-streaming request returns empty text (the token budget goes to
	// reasoning it never sees), and a streaming one gets a long silent gap
	// that makes agent clients give up and cancel. Ask Ollama to answer
	// directly. Models without thinking ignore the flag.
	srv, got := fakeOllama(t, `{"message":{"content":"hi"},"done":true}`)
	b := newBackend(t, srv.URL)

	if err := b.Infer(context.Background(), core.InferRequest{
		Model:    "qwen3.5",
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "hi")},
	}, &collectSink{}); err != nil {
		t.Fatalf("Infer: %v", err)
	}

	think, present := (*got)["think"]
	if !present {
		t.Fatalf("request carries no `think` field: %v", *got)
	}
	if think != false {
		t.Errorf("think = %v, want false (thinking output is never surfaced)", think)
	}
}

func TestInferToolCalls(t *testing.T) {
	srv, got := fakeOllama(t,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"get_weather","arguments":{"city":"Paris"}}}]},"done":false}`,
		`{"message":{"content":""},"done":true,"done_reason":"stop","prompt_eval_count":20,"eval_count":5}`,
	)
	b := newBackend(t, srv.URL)
	sink := &collectSink{}

	err := b.Infer(context.Background(), core.InferRequest{
		Model:    "qwen3.5",
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "weather?")},
		Tools: []core.Tool{{
			Name: "get_weather", Description: "Weather",
			InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	var start, delta, stop bool
	var toolInput string
	for _, ev := range sink.events {
		switch ev.Kind {
		case core.EventToolUseStart:
			start = true
			if ev.ToolName != "get_weather" || ev.ToolID != "call_1" {
				t.Errorf("tool start = %+v", ev)
			}
		case core.EventToolUseDelta:
			delta = true
			toolInput += ev.Text
		case core.EventToolUseStop:
			stop = true
		}
	}
	if !start || !delta || !stop {
		t.Fatalf("tool events missing: start=%v delta=%v stop=%v", start, delta, stop)
	}
	if !strings.Contains(toolInput, "Paris") {
		t.Errorf("tool input = %q", toolInput)
	}
	last := sink.events[len(sink.events)-1]
	if last.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", last.StopReason)
	}

	// The client's tools reach Ollama in its function-calling shape.
	tools := (*got)["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tools = %v", tools)
	}
}

func TestInferSendsToolResultsAndImages(t *testing.T) {
	srv, got := fakeOllama(t, `{"message":{"content":"ok"},"done":true,"prompt_eval_count":1,"eval_count":1}`)
	b := newBackend(t, srv.URL)

	err := b.Infer(context.Background(), core.InferRequest{
		Model: "qwen3.5",
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.Block{
				{Kind: core.BlockFile, MediaType: "image/png", Data: []byte{1, 2, 3}},
				{Kind: core.BlockText, Text: "describe"},
			}},
			{Role: core.RoleAssistant, Blocks: []core.Block{
				{Kind: core.BlockToolUse, ToolID: "call_1", ToolName: "f", ToolInput: []byte(`{"a":1}`)},
			}},
			{Role: core.RoleUser, Blocks: []core.Block{
				{Kind: core.BlockToolResult, ToolID: "call_1", Text: "42"},
			}},
		},
	}, &collectSink{})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	msgs := (*got)["messages"].([]any)
	user := msgs[0].(map[string]any)
	if user["content"] != "describe" {
		t.Errorf("user content = %v", user["content"])
	}
	if imgs, ok := user["images"].([]any); !ok || len(imgs) != 1 {
		t.Errorf("images = %v (native vision expected)", user["images"])
	}
	assistant := msgs[1].(map[string]any)
	if _, ok := assistant["tool_calls"].([]any); !ok {
		t.Errorf("assistant tool_calls missing: %v", assistant)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["content"] != "42" {
		t.Errorf("tool message = %v", tool)
	}
}

func TestInferSurfacesOllamaError(t *testing.T) {
	// Tool support is per *model*: llama3 refuses. The error must reach the
	// caller intact, not as an opaque failure.
	srv, _ := fakeOllama(t, `{"error":"registry.ollama.ai/library/llama3:latest does not support tools"}`)
	b := newBackend(t, srv.URL)
	sink := &collectSink{}

	if err := b.Infer(context.Background(), core.InferRequest{
		Model:    "llama3",
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
		Tools:    []core.Tool{{Name: "f", InputSchema: []byte(`{}`)}},
	}, sink); err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var sawError bool
	for _, ev := range sink.events {
		if ev.Kind == core.EventError {
			sawError = true
			if !strings.Contains(ev.Err.Error(), "does not support tools") {
				t.Errorf("error = %v", ev.Err)
			}
		}
	}
	if !sawError {
		t.Fatal("the ollama error must reach the caller")
	}
}

func TestCapabilities(t *testing.T) {
	b := newBackend(t, "http://127.0.0.1:1")
	caps := b.Capabilities()
	if !caps.Streaming || !caps.MaxTokens || !caps.Sampling || !caps.ClientTools {
		t.Errorf("caps = %+v: ollama honors max_tokens, sampling and tools", caps)
	}
	if caps.Agentic {
		t.Error("ollama has no agentic mode")
	}
	if b.Name() != "ollama" {
		t.Errorf("Name = %q", b.Name())
	}
}

func TestRegisteredInCore(t *testing.T) {
	b, err := core.New("ollama", core.BackendConfig{})
	if err != nil {
		t.Fatalf("core.New(ollama): %v", err)
	}
	if b.Name() != "ollama" {
		t.Errorf("Name = %q", b.Name())
	}
}
