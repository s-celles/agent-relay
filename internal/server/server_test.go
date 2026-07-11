package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s-celles/agent-relay/internal/config"
	"github.com/s-celles/agent-relay/internal/core"
)

type fakeBackend struct {
	calls         atomic.Int64
	lastAgentic   atomic.Bool
	lastSessionID atomic.Value // string
	lastOutputDir atomic.Value // string
	block         bool
	clientTools   bool
	maxTokens     bool
	sampling      bool
	costUSD       float64
	emitTrace     bool
	sessionID     string // reported back via EventSession
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Capabilities() core.Capabilities {
	return core.Capabilities{
		Streaming:   true,
		ClientTools: f.clientTools,
		MaxTokens:   f.maxTokens,
		Sampling:    f.sampling,
	}
}
func (f *fakeBackend) Infer(ctx context.Context, req core.InferRequest, sink core.EventSink) error {
	f.calls.Add(1)
	f.lastAgentic.Store(req.Agentic)
	f.lastSessionID.Store(req.SessionID)
	f.lastOutputDir.Store(req.OutputDir)
	if f.sessionID != "" {
		if err := sink.Emit(ctx, core.Event{Kind: core.EventSession, SessionID: f.sessionID}); err != nil {
			return err
		}
	}
	if req.OutputDir != "" {
		// Simulate an agentic run producing artifacts.
		os.MkdirAll(filepath.Join(req.OutputDir, "sub"), 0o700)
		os.WriteFile(filepath.Join(req.OutputDir, "result.txt"), []byte("artifact"), 0o600)
		os.WriteFile(filepath.Join(req.OutputDir, "sub", "data.json"), []byte(`{"ok":true}`), 0o600)
	}
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	events := []core.Event{{Kind: core.EventMessageStart}}
	if f.emitTrace {
		events = append(events,
			core.Event{Kind: core.EventAgentToolUse, ToolID: "t1", ToolName: "Write", ToolInput: []byte(`{"file_path":"/x"}`)},
			core.Event{Kind: core.EventAgentToolResult, ToolID: "t1", Text: "created"},
		)
	}
	events = append(events,
		core.Event{Kind: core.EventTextDelta, Text: "Hello"},
		core.Event{Kind: core.EventMessageStop, Usage: &core.Usage{
			InputTokens: 3, OutputTokens: 5, CostUSD: f.costUSD,
		}},
	)
	for _, ev := range events {
		if err := sink.Emit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBackend) sessionSeen() string {
	v, _ := f.lastSessionID.Load().(string)
	return v
}

func (f *fakeBackend) outputDirSeen() string {
	v, _ := f.lastOutputDir.Load().(string)
	return v
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

// Per-request agentic authorization (REQ-EXEC-06).

func agenticServer(t *testing.T, fb core.Backend, perRequest bool) http.Handler {
	t.Helper()
	return newTestServer(t, fb, func(c *config.Config) {
		c.Agentic.Enabled = true
		c.Agentic.PerRequestAuthz = perRequest
		if perRequest {
			c.AgenticTokens = [][]byte{[]byte("agentic-secret")}
		}
	})
}

func postMessages(t *testing.T, h http.Handler, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
	req.Header.Set("x-api-key", "good-token")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgenticPerRequestAuthz(t *testing.T) {
	fb := &fakeBackend{}
	h := agenticServer(t, fb, true)

	t.Run("no agentic credential falls back to inference", func(t *testing.T) {
		rec := postMessages(t, h, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if fb.lastAgentic.Load() {
			t.Fatal("request without agentic credential must not run agentically")
		}
	})

	t.Run("valid agentic credential enables agentic", func(t *testing.T) {
		rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer agentic-secret"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
		}
		if !fb.lastAgentic.Load() {
			t.Fatal("valid agentic credential must enable agentic execution")
		}
	})

	t.Run("invalid agentic credential is 403 without spawn", func(t *testing.T) {
		before := fb.calls.Load()
		rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer wrong"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if fb.calls.Load() != before {
			t.Fatal("backend must not run on a rejected agentic credential")
		}
	})

	t.Run("caller token is not an agentic token", func(t *testing.T) {
		rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer good-token"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (credentials must be distinct)", rec.Code)
		}
	})
}

func TestAgenticWithoutPerRequestAuthzAppliesToAll(t *testing.T) {
	// Legacy loopback posture: agentic on, no per-request authz -> every
	// request is agentic (Config.Validate keeps this loopback-only).
	fb := &fakeBackend{}
	h := agenticServer(t, fb, false)
	rec := postMessages(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !fb.lastAgentic.Load() {
		t.Fatal("with agentic enabled and no per-request authz, requests run agentically")
	}
}

func TestAgenticHeaderRejectedWhenAgenticDisabled(t *testing.T) {
	fb := &fakeBackend{}
	h := newTestServer(t, fb) // agentic disabled
	rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer whatever"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when agentic mode is disabled", rec.Code)
	}
	if fb.calls.Load() != 0 {
		t.Fatal("backend must not run for a rejected agentic request")
	}
}

// Agentic audit trail: authorized agentic requests are logged at Info,
// rejected agentic credentials at Warn, both correlated by X-Request-Id.

// newLoggedServer is newTestServer with the server's slog output captured as
// JSON lines, so tests can assert on structured log output.
func newLoggedServer(t *testing.T, fb core.Backend, mutate ...func(*config.Config)) (http.Handler, *bytes.Buffer) {
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
	buf := &bytes.Buffer{}
	h, err := New(cfg, fb, WithLogger(slog.New(slog.NewJSONHandler(buf, nil))))
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return h, buf
}

// logLines returns every JSON log record in buf whose msg field matches.
func logLines(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func agenticPerRequest(c *config.Config) {
	c.Agentic.Enabled = true
	c.Agentic.PerRequestAuthz = true
	c.AgenticTokens = [][]byte{[]byte("agentic-secret")}
}

func TestAgenticAuthorizedRequestIsAudited(t *testing.T) {
	t.Run("per-request authz", func(t *testing.T) {
		h, buf := newLoggedServer(t, &fakeBackend{}, agenticPerRequest)
		rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer agentic-secret"})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
		}
		lines := logLines(t, buf, "agentic request authorized")
		if len(lines) != 1 {
			t.Fatalf("audit lines = %d, want exactly 1 (log: %s)", len(lines), buf.String())
		}
		entry := lines[0]
		if entry["level"] != "INFO" {
			t.Errorf("level = %v, want INFO", entry["level"])
		}
		wantID := rec.Header().Get("X-Request-Id")
		if wantID == "" || entry["id"] != wantID {
			t.Errorf("id = %v, want X-Request-Id header %q", entry["id"], wantID)
		}
		if entry["path"] != "/v1/messages" {
			t.Errorf("path = %v, want /v1/messages", entry["path"])
		}
	})

	t.Run("legacy all-requests posture", func(t *testing.T) {
		h, buf := newLoggedServer(t, &fakeBackend{}, func(c *config.Config) {
			c.Agentic.Enabled = true
		})
		rec := postMessages(t, h, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := len(logLines(t, buf, "agentic request authorized")); got != 1 {
			t.Fatalf("audit lines = %d, want exactly 1 (log: %s)", got, buf.String())
		}
	})
}

func TestPlainInferenceRequestIsNotAudited(t *testing.T) {
	t.Run("agentic disabled", func(t *testing.T) {
		h, buf := newLoggedServer(t, &fakeBackend{})
		rec := postMessages(t, h, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := len(logLines(t, buf, "agentic request authorized")); got != 0 {
			t.Fatalf("audit lines = %d, want 0 for plain inference", got)
		}
	})

	t.Run("no credential falls back to inference", func(t *testing.T) {
		h, buf := newLoggedServer(t, &fakeBackend{}, agenticPerRequest)
		rec := postMessages(t, h, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := len(logLines(t, buf, "agentic request authorized")); got != 0 {
			t.Fatalf("audit lines = %d, want 0 for inference fallback", got)
		}
	})
}

func TestRejectedAgenticCredentialIsLoggedAtWarn(t *testing.T) {
	h, buf := newLoggedServer(t, &fakeBackend{}, agenticPerRequest)
	rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer wrong"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	lines := logLines(t, buf, "agentic request denied")
	if len(lines) != 1 {
		t.Fatalf("denial lines = %d, want exactly 1 (log: %s)", len(lines), buf.String())
	}
	entry := lines[0]
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
	wantID := rec.Header().Get("X-Request-Id")
	if wantID == "" || entry["id"] != wantID {
		t.Errorf("id = %v, want X-Request-Id header %q", entry["id"], wantID)
	}
	if entry["path"] != "/v1/messages" {
		t.Errorf("path = %v, want /v1/messages", entry["path"])
	}
	if got := len(logLines(t, buf, "agentic request authorized")); got != 0 {
		t.Fatalf("rejected request must not also be logged as authorized (%d lines)", got)
	}
}

const toolsBody = `{"model":"sonnet","max_tokens":100,
	"tools":[{"name":"get_weather","description":"d","input_schema":{"type":"object"}}],
	"messages":[{"role":"user","content":"hi"}]}`

func TestClientToolsRejectedWhenBackendCannotServeThem(t *testing.T) {
	fb := &fakeBackend{} // ClientTools: false
	h := newTestServer(t, fb)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(toolsBody))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for tools[] on a non-tool backend", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "tool") {
		t.Fatalf("error body should explain the tools limitation: %s", rec.Body.String())
	}
	if fb.calls.Load() != 0 {
		t.Fatal("backend must not run for a rejected tools request")
	}
}

func TestClientToolsAcceptedWhenBackendSupportsThem(t *testing.T) {
	fb := &fakeBackend{clientTools: true}
	h := newTestServer(t, fb)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(toolsBody))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if fb.calls.Load() != 1 {
		t.Fatal("backend should have served the tools request")
	}
}

func TestStructuredHistoryAccepted(t *testing.T) {
	// tool_use/tool_result blocks in history are fine on any backend — only
	// client-defined tools[] require backend support.
	h := newTestServer(t, &fakeBackend{})
	body := `{"model":"sonnet","max_tokens":100,"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"sunny"}]}
	]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
}

func TestMaxTokensWarningLoggedOncePerProcess(t *testing.T) {
	// The fake backend does not enforce max_tokens; the gap must be logged,
	// but only once — the Anthropic wire makes max_tokens mandatory, so a
	// per-request warning would flood the log.
	h, buf := newLoggedServer(t, &fakeBackend{}) // MaxTokens: false
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
		req.Header.Set("x-api-key", "good-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body: %s", i, rec.Code, rec.Body.String())
		}
	}
	if n := strings.Count(buf.String(), "not enforced"); n != 1 {
		t.Fatalf("max_tokens warning logged %d times across two requests, want exactly 1; log:\n%s", n, buf.String())
	}
}

// Session continuity: the CLI keys its sessions by working directory, so
// resuming is only offered where the workdir is stable.

func TestSessionIDReturnedInHeader(t *testing.T) {
	h := newTestServer(t, &fakeBackend{sessionID: "984f3680-403a-4275-9b3f-eeed6b8100bf"})
	rec := postMessages(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Session-Id"); got != "984f3680-403a-4275-9b3f-eeed6b8100bf" {
		t.Errorf("X-Session-Id = %q", got)
	}
}

func TestResumeInInferenceMode(t *testing.T) {
	fb := &fakeBackend{}
	h := newTestServer(t, fb)
	rec := postMessages(t, h, map[string]string{"X-Session-Id": "984f3680-403a-4275-9b3f-eeed6b8100bf"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if fb.sessionSeen() != "984f3680-403a-4275-9b3f-eeed6b8100bf" {
		t.Errorf("backend got session id %q", fb.sessionSeen())
	}
}

func TestResumeInRetainedAgenticWorkspace(t *testing.T) {
	// The caller pins the workspace by echoing back the outputs id; the
	// request runs in that retained directory, so the CLI finds its session.
	fb := &fakeBackend{}
	h := newTestServer(t, fb, agenticPerRequest, withOutputsDir(t))

	first := postMessages(t, h, map[string]string{
		"X-Agentic-Authorization": "Bearer agentic-secret",
		"X-Agentic-Keep-Outputs":  "true",
	})
	id := first.Header().Get("X-Agentic-Outputs")
	if id == "" {
		t.Fatal("no outputs id")
	}
	firstDir := fb.outputDirSeen()

	second := postMessages(t, h, map[string]string{
		"X-Agentic-Authorization": "Bearer agentic-secret",
		"X-Agentic-Outputs":       id,
		"X-Session-Id":            "984f3680-403a-4275-9b3f-eeed6b8100bf",
	})
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", second.Code, second.Body.String())
	}
	if fb.outputDirSeen() != firstDir {
		t.Errorf("resumed request ran in %q, want the retained workspace %q",
			fb.outputDirSeen(), firstDir)
	}
	if second.Header().Get("X-Agentic-Outputs") != id {
		t.Errorf("outputs id should be echoed back, got %q", second.Header().Get("X-Agentic-Outputs"))
	}
}

func TestResumeRefusedWithoutStableWorkspace(t *testing.T) {
	// Agentic without a pinned workspace = ephemeral dir = the CLI would not
	// find the session. Fail loudly instead of confusingly.
	h := newTestServer(t, &fakeBackend{}, agenticPerRequest, withOutputsDir(t))
	rec := postMessages(t, h, map[string]string{
		"X-Agentic-Authorization": "Bearer agentic-secret",
		"X-Session-Id":            "984f3680-403a-4275-9b3f-eeed6b8100bf",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "workspace") {
		t.Errorf("error should explain the workspace requirement: %s", rec.Body.String())
	}
}

func TestUnknownWorkspaceIDRejected(t *testing.T) {
	h := newTestServer(t, &fakeBackend{}, agenticPerRequest, withOutputsDir(t))
	rec := postMessages(t, h, map[string]string{
		"X-Agentic-Authorization": "Bearer agentic-secret",
		"X-Agentic-Outputs":       "00000000000000000000000000000000",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown workspace", rec.Code)
	}
}

func TestTraceFileWrittenToRetainedOutputs(t *testing.T) {
	// Agent tool activity is persisted as trace.jsonl alongside retained
	// outputs, so a harness can review what the agent actually did.
	fb := &fakeBackend{emitTrace: true}
	h := newTestServer(t, fb, agenticPerRequest, withOutputsDir(t))

	rec := postMessages(t, h, map[string]string{
		"X-Agentic-Authorization": "Bearer agentic-secret",
		"X-Agentic-Keep-Outputs":  "true",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get("X-Agentic-Outputs")

	req := httptest.NewRequest("GET", "/v1/outputs/"+id+"/files/trace.jsonl", nil)
	req.Header.Set("x-api-key", "good-token")
	drec := httptest.NewRecorder()
	h.ServeHTTP(drec, req)
	if drec.Code != http.StatusOK {
		t.Fatalf("trace.jsonl download = %d", drec.Code)
	}
	lines := strings.Split(strings.TrimSpace(drec.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("trace lines = %d, want 2:\n%s", len(lines), drec.Body.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("trace line is not JSON: %v", err)
	}
	if first["type"] != "tool_use" || first["name"] != "Write" {
		t.Errorf("first trace line = %v", first)
	}
}

func TestNoTraceFileWithoutRetainedOutputs(t *testing.T) {
	h := newTestServer(t, &fakeBackend{emitTrace: true}, agenticPerRequest, withOutputsDir(t))
	rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer agentic-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Agentic-Outputs") != "" {
		t.Fatal("no outputs id expected without X-Agentic-Keep-Outputs")
	}
}

func TestUsageIsAccountedPerRequest(t *testing.T) {
	// Every served request logs its token usage and dollar cost, correlated
	// by request id, and feeds the cumulative metrics — so a harness that
	// fans out can attribute spend.
	fb := &fakeBackend{costUSD: 0.0228}
	h, buf := newLoggedServer(t, fb)

	rec := postMessages(t, h, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	lines := logLines(t, buf, "request usage")
	if len(lines) != 1 {
		t.Fatalf("usage lines = %d, want 1 (log: %s)", len(lines), buf.String())
	}
	entry := lines[0]
	if entry["id"] != rec.Header().Get("X-Request-Id") {
		t.Errorf("id = %v, want the X-Request-Id header", entry["id"])
	}
	if entry["input_tokens"] != float64(3) || entry["output_tokens"] != float64(5) {
		t.Errorf("tokens = %v/%v, want 3/5", entry["input_tokens"], entry["output_tokens"])
	}
	if cost, _ := entry["cost_usd"].(float64); cost < 0.0227 || cost > 0.0229 {
		t.Errorf("cost_usd = %v, want ~0.0228", entry["cost_usd"])
	}

	// The same numbers must reach /v1/metrics.
	req := httptest.NewRequest("GET", "/v1/metrics", nil)
	req.Header.Set("x-api-key", "good-token")
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, req)
	var m struct {
		InputTokens  int64   `json:"input_tokens_total"`
		OutputTokens int64   `json:"output_tokens_total"`
		CostUSD      float64 `json:"cost_usd_total"`
	}
	if err := json.Unmarshal(mrec.Body.Bytes(), &m); err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.InputTokens != 3 || m.OutputTokens != 5 || m.CostUSD < 0.0227 {
		t.Errorf("metrics = %+v", m)
	}
}

func TestSamplingParamsWarnOnceAndAreListed(t *testing.T) {
	// Dropped sampling parameters must be signaled, not silently ignored —
	// but only once per process, like the max_tokens warning.
	h, buf := newLoggedServer(t, &fakeBackend{}) // Sampling: false
	body := `{"model":"sonnet","max_tokens":100,"temperature":0.7,"top_k":40,
		"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "good-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
		}
	}
	lines := logLines(t, buf, "sampling parameters are not supported by this backend and were ignored")
	if len(lines) != 1 {
		t.Fatalf("warnings = %d, want exactly 1 (log: %s)", len(lines), buf.String())
	}
	params, _ := lines[0]["params"].(string)
	for _, want := range []string{"temperature", "top_k", "stop_sequences"} {
		if !strings.Contains(params, want) {
			t.Errorf("warning should name %q; params = %q", want, params)
		}
	}
	if strings.Contains(params, "top_p") {
		t.Errorf("warning must not name unset params; params = %q", params)
	}
}

func TestNoSamplingWarningWhenUnsetOrSupported(t *testing.T) {
	t.Run("no sampling params set", func(t *testing.T) {
		h, buf := newLoggedServer(t, &fakeBackend{})
		rec := postMessages(t, h, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if strings.Contains(buf.String(), "sampling parameters") {
			t.Errorf("no warning expected:\n%s", buf.String())
		}
	})

	t.Run("backend supports sampling", func(t *testing.T) {
		h, buf := newLoggedServer(t, &fakeBackend{sampling: true})
		body := `{"model":"sonnet","max_tokens":100,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "good-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if strings.Contains(buf.String(), "sampling parameters") {
			t.Errorf("no warning expected when the backend honors them:\n%s", buf.String())
		}
	})
}

func TestDisabledAgenticDenialIsLogged(t *testing.T) {
	// The "agentic execution disabled" branch must leave the same audit trail
	// as an invalid credential.
	h, buf := newLoggedServer(t, &fakeBackend{}) // agentic disabled
	rec := postMessages(t, h, map[string]string{"X-Agentic-Authorization": "Bearer whatever"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	lines := logLines(t, buf, "agentic request denied")
	if len(lines) != 1 {
		t.Fatalf("denial lines = %d, want exactly 1 (log: %s)", len(lines), buf.String())
	}
	if lines[0]["reason"] != "agentic execution disabled" {
		t.Errorf("reason = %v, want %q", lines[0]["reason"], "agentic execution disabled")
	}
}

func TestMaxTokensWarningOnOpenAIEndpoint(t *testing.T) {
	// The warning must also fire for /v1/chat/completions traffic.
	h, buf := newLoggedServer(t, &fakeBackend{}) // MaxTokens: false
	body := `{"model":"sonnet","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if n := strings.Count(buf.String(), "not enforced"); n != 1 {
		t.Fatalf("max_tokens warning logged %d times, want exactly 1; log:\n%s", n, buf.String())
	}
}

func TestNoMaxTokensWarningWhenBackendEnforcesIt(t *testing.T) {
	h, buf := newLoggedServer(t, &fakeBackend{maxTokens: true})
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(messagesBody))
	req.Header.Set("x-api-key", "good-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(buf.String(), "not enforced") {
		t.Fatalf("no warning expected when the backend enforces max_tokens; log:\n%s", buf.String())
	}
}

// Agentic output retrieval (X-Agentic-Keep-Outputs + /v1/outputs).

func withOutputsDir(t *testing.T) func(*config.Config) {
	dir := t.TempDir()
	return func(c *config.Config) {
		c.OutputsDir = dir
		c.OutputsTTL = time.Minute
	}
}

func TestKeepOutputsFlow(t *testing.T) {
	h := newTestServer(t, &fakeBackend{}, agenticPerRequest, withOutputsDir(t))

	rec := postMessages(t, h, map[string]string{
		"X-Agentic-Authorization": "Bearer agentic-secret",
		"X-Agentic-Keep-Outputs":  "true",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get("X-Agentic-Outputs")
	if id == "" {
		t.Fatal("missing X-Agentic-Outputs response header")
	}

	// List the retained artifacts.
	req := httptest.NewRequest("GET", "/v1/outputs/"+id, nil)
	req.Header.Set("x-api-key", "good-token")
	lrec := httptest.NewRecorder()
	h.ServeHTTP(lrec, req)
	if lrec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body: %s", lrec.Code, lrec.Body.String())
	}
	var listing struct {
		Files []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("unmarshal listing: %v", err)
	}
	if len(listing.Files) != 2 {
		t.Fatalf("files = %+v, want 2", listing.Files)
	}

	// Download one artifact.
	req = httptest.NewRequest("GET", "/v1/outputs/"+id+"/files/sub/data.json", nil)
	req.Header.Set("x-api-key", "good-token")
	drec := httptest.NewRecorder()
	h.ServeHTTP(drec, req)
	if drec.Code != http.StatusOK || drec.Body.String() != `{"ok":true}` {
		t.Fatalf("download = %d %q", drec.Code, drec.Body.String())
	}

	// Release, then everything 404s.
	req = httptest.NewRequest("DELETE", "/v1/outputs/"+id, nil)
	req.Header.Set("x-api-key", "good-token")
	rrec := httptest.NewRecorder()
	h.ServeHTTP(rrec, req)
	if rrec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rrec.Code)
	}
	req = httptest.NewRequest("GET", "/v1/outputs/"+id, nil)
	req.Header.Set("x-api-key", "good-token")
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, req)
	if grec.Code != http.StatusNotFound {
		t.Fatalf("list after delete = %d, want 404", grec.Code)
	}
}

func TestKeepOutputsRequiresAgentic(t *testing.T) {
	h := newTestServer(t, &fakeBackend{}, agenticPerRequest, withOutputsDir(t))
	// No agentic credential: the request degrades to inference, so retention
	// must be refused rather than silently ignored.
	rec := postMessages(t, h, map[string]string{"X-Agentic-Keep-Outputs": "true"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestOutputsRequireAuth(t *testing.T) {
	h := newTestServer(t, &fakeBackend{}, withOutputsDir(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/outputs/00000000000000000000000000000000", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestOutputsUnknownOrInvalidID(t *testing.T) {
	h := newTestServer(t, &fakeBackend{}, withOutputsDir(t))
	for _, id := range []string{"00000000000000000000000000000000", "not-an-id", "%2e%2e"} {
		req := httptest.NewRequest("GET", "/v1/outputs/"+id, nil)
		req.Header.Set("x-api-key", "good-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id %q: status = %d, want 404", id, rec.Code)
		}
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
