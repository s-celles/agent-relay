package core

import (
	"context"
	"errors"
	"time"
)

// ErrBusy is returned when every concurrency slot is taken; the HTTP layer
// maps it to 503 without any subprocess having been spawned (REQ-PROC-03).
var ErrBusy = errors.New("all backend slots busy")

// Dispatcher owns the per-request lifecycle: slot acquisition, timeout, and
// handoff to the backend. It knows nothing about wire formats or any
// specific CLI.
type Dispatcher struct {
	// Backend serves any model without a route: the default.
	Backend Backend
	// Routes maps a logical model name to the backend that serves it. The
	// client keeps choosing a model, as it would against the real API; the
	// relay decides which backend that means (DQ-2).
	Routes  map[string]Backend
	Limiter *Limiter
	Timeout time.Duration // 0 means no timeout (REQ-PROC-06)
}

// For resolves the backend serving a model. Capabilities differ between
// backends — the CLI cannot enforce max_tokens, a local model can — so
// callers must ask about the backend that will actually run the request.
func (d *Dispatcher) For(model string) Backend {
	if b, ok := d.Routes[model]; ok {
		return b
	}
	return d.Backend
}

// Do runs one inference request. The slot is released on every exit path.
func (d *Dispatcher) Do(ctx context.Context, req InferRequest, sink EventSink) error {
	if !d.Limiter.TryAcquire() {
		return ErrBusy
	}
	defer d.Limiter.Release()

	// A request may carry its own deadline (the server clamps it to the
	// operator's ceiling); otherwise the dispatcher default applies.
	timeout := d.Timeout
	if req.Timeout > 0 {
		timeout = req.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return d.For(req.Model).Infer(ctx, req, sink)
}
