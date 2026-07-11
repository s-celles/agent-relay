package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s-celles/agent-relay/internal/config"
	"github.com/s-celles/agent-relay/internal/core"
)

type fakeBackend struct {
	calls atomic.Int64
	block bool
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Capabilities() core.Capabilities {
	return core.Capabilities{Streaming: true}
}
func (f *fakeBackend) Infer(ctx context.Context, req core.InferRequest, sink core.EventSink) error {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, ev := range []core.Event{
		{Kind: core.EventMessageStart},
		{Kind: core.EventTextDelta, Text: "Hello"},
		{Kind: core.EventMessageStop, Usage: &core.Usage{InputTokens: 3, OutputTokens: 5}},
	} {
		if err := sink.Emit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func newTestServer(t *testing.T, fb core.Backend, mutate ...func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Config{
		BindAddr:       "127.0.0.1:0",
		Tokens:         [][]byte{[]byte("good-token")},
		Backend:        "fake",
		MaxConcurrent:  2,
		RequestTimeout: 5 * time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	h, err := New(cfg, fb)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return h
}

const messagesBody = `{"model":"sonnet","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

func TestHealthNoAuth(t *testing.T) {
	h := newTestServer(t, &fakeBackend{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200 (REQ-API-04)", rec.Code)
	}
}

func TestMessagesRequiresAuth(t *testing.T) {
	fb := &fakeBackend{}
	h := newTestServer(t, fb)

	for name, decorate := range map[string]func(*http.Request){
		"no credentials":   func(r *http.Request) {},
		"wrong bearer":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer bad") },
		"wrong x-api-key":  func(r *http.Request) { r.Header.Set("x-api-key", "bad") },
		"malformed bearer": func(r *http.Request) { r.Header.Set("Authorization", "good-token") },
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
			decorate(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (REQ-AUTH-01)", rec.Code)
			}
		})
	}
	if fb.calls.Load() != 0 {
		t.Fatal("backend must never run for unauthenticated requests (REQ-AUTH-02)")
	}
}

func TestMessagesNonStreaming(t *testing.T) {
	h := newTestServer(t, &fakeBackend{})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello" {
		t.Fatalf("content = %+v", resp.Content)
	}
}

func TestMessagesStreaming(t *testing.T) {
	h := newTestServer(t, &fakeBackend{})
	body := `{"model":"sonnet","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q (REQ-API-02)", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{"event: message_start", `"text":"Hello"`, "event: message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream missing %q:\n%s", want, out)
		}
	}
}

func TestChatCompletions(t *testing.T) {
	h := newTestServer(t, &fakeBackend{})
	body := `{"model":"sonnet","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"Hello"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestBusyReturns503(t *testing.T) {
	fb := &fakeBackend{block: true}
	h := newTestServer(t, fb, func(c *config.Config) { c.MaxConcurrent = 1 })

	// Occupy the single slot with a blocked streaming request.
	started := make(chan struct{})
	firstDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody)).WithContext(ctx)
		req.Header.Set("x-api-key", "good-token")
		close(started)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started
	// Wait for the slot to actually be held.
	deadline := time.Now().Add(2 * time.Second)
	for fb.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first request never reached the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (REQ-PROC-03)", rec.Code)
	}
	if fb.calls.Load() != 1 {
		t.Fatal("rejected request must not reach the backend")
	}

	cancel()
	<-firstDone
}

func TestMalformedBodyReturns400(t *testing.T) {
	h := newTestServer(t, &fakeBackend{})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{not json"))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMetricsRequiresAuthAndReportsCounts(t *testing.T) {
	h := newTestServer(t, &fakeBackend{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1/metrics = %d, want 401 (REQ-API-06)", rec.Code)
	}

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
	req.Header.Set("x-api-key", "good-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest("GET", "/v1/metrics", nil)
	req.Header.Set("x-api-key", "good-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/metrics = %d", rec.Code)
	}
	var m struct {
		RequestsTotal int64 `json:"requests_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("metrics unmarshal: %v (body: %s)", err, rec.Body.String())
	}
	if m.RequestsTotal < 1 {
		t.Errorf("requests_total = %d, want >= 1", m.RequestsTotal)
	}
}

func TestNoTokensConfiguredAllowsLoopbackCallers(t *testing.T) {
	// Config.Validate guarantees this only happens on loopback binds.
	h := newTestServer(t, &fakeBackend{}, func(c *config.Config) { c.Tokens = nil })
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
