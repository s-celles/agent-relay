// Package server wires the HTTP mux: routing, auth, and the handlers that
// bridge wire decoding to core dispatch. Standard library only — no
// framework, small audit surface (NFR-INSPECT-01).
package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/s-celles/agent-relay/internal/api/anthropic"
	"github.com/s-celles/agent-relay/internal/api/openai"
	"github.com/s-celles/agent-relay/internal/config"
	"github.com/s-celles/agent-relay/internal/core"
	"github.com/s-celles/agent-relay/internal/obs"
)

type startedSink interface {
	core.EventSink
	Started() bool
}

type server struct {
	dispatcher *core.Dispatcher
	metrics    *obs.Metrics
	logger     *slog.Logger
}

// Option customizes the server (currently: logger injection).
type Option func(*server)

func WithLogger(l *slog.Logger) Option {
	return func(s *server) { s.logger = l }
}

// New builds the full HTTP handler for the given validated config and
// backend.
func New(cfg config.Config, backend core.Backend, opts ...Option) (http.Handler, error) {
	if backend == nil {
		return nil, errors.New("nil backend")
	}
	s := &server{
		dispatcher: &core.Dispatcher{
			Backend: backend,
			Limiter: core.NewLimiter(cfg.MaxConcurrent),
			Timeout: cfg.RequestTimeout,
		},
		metrics: obs.NewMetrics(),
		logger:  slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}

	auth := func(next http.Handler) http.Handler {
		return RequireBearer(cfg.Tokens, s.metrics.Unauthorized, anthropic.WriteError, next)
	}

	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", auth(http.HandlerFunc(s.handleMessages)))     // REQ-API-01
	mux.Handle("POST /v1/chat/completions", auth(http.HandlerFunc(s.handleChat))) // REQ-API-03
	mux.Handle("GET /health", http.HandlerFunc(s.handleHealth))                   // REQ-API-04
	mux.Handle("GET /v1/metrics", auth(s.metrics.Handler()))                      // REQ-API-06

	return obs.WithRequestID(s.logger, mux), nil
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	req, err := anthropic.DecodeRequest(r.Body)
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := "msg_" + obs.NewRequestID()
	if req.Stream {
		s.run(w, r, req, anthropic.NewStreamSink(w, id, req.Model), anthropic.WriteError)
		return
	}
	sink := anthropic.NewCollectSink(id, req.Model)
	if !s.runCollected(w, r, req, sink, sink.Err, anthropic.WriteError) {
		return
	}
	_ = sink.WriteResponse(w)
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	req, err := openai.DecodeRequest(r.Body)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := "chatcmpl-" + obs.NewRequestID()
	if req.Stream {
		s.run(w, r, req, openai.NewStreamSink(w, id, req.Model), openai.WriteError)
		return
	}
	sink := openai.NewCollectSink(id, req.Model)
	if !s.runCollected(w, r, req, sink, sink.Err, openai.WriteError) {
		return
	}
	_ = sink.WriteResponse(w)
}

// run dispatches a streaming request. Errors before the first byte become an
// HTTP status; errors after it are delivered in-stream via the sink.
func (s *server) run(w http.ResponseWriter, r *http.Request, req core.InferRequest, sink startedSink, writeErr func(http.ResponseWriter, int, string)) {
	s.metrics.RequestStarted()
	defer s.metrics.RequestFinished()

	err := s.dispatcher.Do(r.Context(), req, sink)
	if err == nil {
		return
	}
	s.reportDispatchError(w, r, err, sink.Started(), sink, writeErr)
}

// runCollected dispatches a non-streaming request; returns true when the
// caller should write the collected response.
func (s *server) runCollected(w http.ResponseWriter, r *http.Request, req core.InferRequest, sink core.EventSink, sinkErr func() error, writeErr func(http.ResponseWriter, int, string)) bool {
	s.metrics.RequestStarted()
	defer s.metrics.RequestFinished()

	if err := s.dispatcher.Do(r.Context(), req, sink); err != nil {
		s.reportDispatchError(w, r, err, false, nil, writeErr)
		return false
	}
	if err := sinkErr(); err != nil {
		s.metrics.BackendError()
		writeErr(w, http.StatusBadGateway, err.Error())
		return false
	}
	return true
}

func (s *server) reportDispatchError(w http.ResponseWriter, r *http.Request, err error, streamStarted bool, sink core.EventSink, writeErr func(http.ResponseWriter, int, string)) {
	switch {
	case errors.Is(err, core.ErrBusy):
		s.metrics.RejectedBusy()
		writeErr(w, http.StatusServiceUnavailable, "all backend slots busy, retry later")
	case streamStarted:
		s.metrics.BackendError()
		s.logger.Error("backend error mid-stream", "err", err, "path", r.URL.Path)
		_ = sink.Emit(r.Context(), core.Event{Kind: core.EventError, Err: err})
	default:
		s.metrics.BackendError()
		s.logger.Error("backend error", "err", err, "path", r.URL.Path)
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}
