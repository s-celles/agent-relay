package obs

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestMetricsRecordUsage(t *testing.T) {
	m := NewMetrics()
	m.RecordUsage(10, 20, 0.02)
	m.RecordUsage(5, 5, 0.01)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/metrics", nil))

	var got struct {
		InputTokens  int64   `json:"input_tokens_total"`
		OutputTokens int64   `json:"output_tokens_total"`
		CostUSD      float64 `json:"cost_usd_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if got.InputTokens != 15 || got.OutputTokens != 25 {
		t.Errorf("tokens = %+v, want 15/25", got)
	}
	if got.CostUSD < 0.0299 || got.CostUSD > 0.0301 {
		t.Errorf("cost_usd_total = %v, want ~0.03", got.CostUSD)
	}
}

func TestMetricsCostAbsentWhenUnreported(t *testing.T) {
	m := NewMetrics()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/metrics", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	if got["cost_usd_total"] != float64(0) {
		t.Errorf("cost_usd_total = %v, want 0", got["cost_usd_total"])
	}
}

func TestNewRequestID(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a) {
		t.Errorf("id %q is not 16 hex chars", a)
	}
	if a == b {
		t.Errorf("two ids collided: %q", a)
	}
}

func TestMetricsCounters(t *testing.T) {
	m := NewMetrics()
	m.RequestStarted()
	m.RequestStarted()
	m.RequestFinished()
	m.RejectedBusy()
	m.Unauthorized()
	m.AgenticDenied()
	m.RateLimited()
	m.BackendError()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rec.Body.String())
	}
	want := map[string]float64{
		"requests_total": 2,
		"in_flight":      1,
		"rejected_busy":  1,
		"unauthorized":   1,
		"agentic_denied": 1,
		"rate_limited":   1,
		"backend_errors": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["uptime_seconds"]; !ok {
		t.Error("uptime_seconds missing from snapshot")
	}
}

func TestWithRequestIDGeneratesAndLogs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	h := WithRequestID(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/messages", nil))

	id := rec.Header().Get("X-Request-Id")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(id) {
		t.Errorf("generated X-Request-Id %q is not 16 hex chars", id)
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log line: %v (log %s)", err, logs.String())
	}
	if entry["id"] != id {
		t.Errorf("logged id = %v, want %q", entry["id"], id)
	}
	if entry["method"] != "GET" || entry["path"] != "/v1/messages" {
		t.Errorf("logged method/path = %v/%v, want GET//v1/messages", entry["method"], entry["path"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Error("duration_ms missing from log entry")
	}
}

func TestWithRequestIDPreservesCallerID(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	h := WithRequestID(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("X-Request-Id", "caller-supplied-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "caller-supplied-id" {
		t.Errorf("X-Request-Id = %q, want caller-supplied-id", got)
	}
}
