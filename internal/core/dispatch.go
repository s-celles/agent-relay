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
	Backend Backend
	Limiter *Limiter
	Timeout time.Duration // 0 means no timeout (REQ-PROC-06)
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
	return d.Backend.Infer(ctx, req, sink)
}
