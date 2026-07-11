// Package obs provides request IDs, structured request logging, and a
// minimal JSON metrics snapshot (REQ-API-06, DQ-4: minimal JSON over
// Prometheus for v1).
package obs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// NewRequestID returns a random 16-hex-char identifier.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// Metrics holds process-lifetime counters. All methods are safe for
// concurrent use.
type Metrics struct {
	start         time.Time
	requestsTotal atomic.Int64
	inFlight      atomic.Int64
	rejectedBusy  atomic.Int64
	unauthorized  atomic.Int64
	agenticDenied atomic.Int64
	backendErrors atomic.Int64
	inputTokens   atomic.Int64
	outputTokens  atomic.Int64
	// costMicroUSD accumulates the backend-reported cost in millionths of a
	// dollar: an integer counter keeps the atomic add lock-free.
	costMicroUSD atomic.Int64
}

func NewMetrics() *Metrics { return &Metrics{start: time.Now()} }

func (m *Metrics) RequestStarted()  { m.requestsTotal.Add(1); m.inFlight.Add(1) }
func (m *Metrics) RequestFinished() { m.inFlight.Add(-1) }
func (m *Metrics) RejectedBusy()    { m.rejectedBusy.Add(1) }
func (m *Metrics) Unauthorized()    { m.unauthorized.Add(1) }
func (m *Metrics) AgenticDenied()   { m.agenticDenied.Add(1) }
func (m *Metrics) BackendError()    { m.backendErrors.Add(1) }

// RecordUsage accumulates the token counts and dollar cost of one served
// request, so operators (and fanning-out harnesses) can attribute spend.
func (m *Metrics) RecordUsage(inputTokens, outputTokens int, costUSD float64) {
	m.inputTokens.Add(int64(inputTokens))
	m.outputTokens.Add(int64(outputTokens))
	m.costMicroUSD.Add(int64(costUSD * 1e6))
}

// Handler serves the metrics snapshot as JSON.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uptime_seconds": int64(time.Since(m.start).Seconds()),
			"requests_total": m.requestsTotal.Load(),
			"in_flight":      m.inFlight.Load(),
			"rejected_busy":  m.rejectedBusy.Load(),
			"unauthorized":   m.unauthorized.Load(),
			"agentic_denied": m.agenticDenied.Load(),
			"backend_errors": m.backendErrors.Load(),

			"input_tokens_total":  m.inputTokens.Load(),
			"output_tokens_total": m.outputTokens.Load(),
			"cost_usd_total":      float64(m.costMicroUSD.Load()) / 1e6,
		})
	})
}

// WithRequestID stamps every request with an X-Request-Id header and logs
// method, path, and duration on completion.
func WithRequestID(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request",
			"id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
