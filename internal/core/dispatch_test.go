package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend implements Backend for lifecycle tests without any subprocess.
type fakeBackend struct {
	calls  atomic.Int64
	events []Event
	block  bool // when true, Infer blocks until ctx is done
}

func (f *fakeBackend) Name() string               { return "fake" }
func (f *fakeBackend) Capabilities() Capabilities { return Capabilities{Streaming: true} }

func (f *fakeBackend) Infer(ctx context.Context, req InferRequest, sink EventSink) error {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, ev := range f.events {
		if err := sink.Emit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

type collectSink struct{ events []Event }

func (c *collectSink) Emit(ctx context.Context, ev Event) error {
	c.events = append(c.events, ev)
	return nil
}

func TestDispatcherRunsBackend(t *testing.T) {
	fb := &fakeBackend{events: []Event{
		{Kind: EventMessageStart},
		{Kind: EventTextDelta, Text: "hi"},
		{Kind: EventMessageStop, Usage: &Usage{InputTokens: 1, OutputTokens: 2}},
	}}
	d := &Dispatcher{Backend: fb, Limiter: NewLimiter(1)}
	sink := &collectSink{}

	if err := d.Do(context.Background(), InferRequest{}, sink); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(sink.events) != 3 {
		t.Fatalf("got %d events, want 3", len(sink.events))
	}
	if sink.events[1].Text != "hi" {
		t.Fatalf("delta text = %q, want %q", sink.events[1].Text, "hi")
	}
}

func TestDispatcherRejectsWhenFull(t *testing.T) {
	fb := &fakeBackend{}
	lim := NewLimiter(1)
	if !lim.TryAcquire() { // occupy the only slot
		t.Fatal("setup: acquire failed")
	}
	d := &Dispatcher{Backend: fb, Limiter: lim}

	err := d.Do(context.Background(), InferRequest{}, &collectSink{})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if fb.calls.Load() != 0 {
		t.Fatal("backend must not be invoked when the pool is full (REQ-PROC-03)")
	}
}

func TestDispatcherReleasesSlotAfterRun(t *testing.T) {
	fb := &fakeBackend{}
	d := &Dispatcher{Backend: fb, Limiter: NewLimiter(1)}

	for i := 0; i < 3; i++ {
		if err := d.Do(context.Background(), InferRequest{}, &collectSink{}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

func TestDispatcherCancellation(t *testing.T) {
	fb := &fakeBackend{block: true}
	d := &Dispatcher{Backend: fb, Limiter: NewLimiter(1)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := d.Do(ctx, InferRequest{}, &collectSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Do did not return promptly on cancellation")
	}
}

type namedBackend struct {
	fakeBackend
	name string
	caps Capabilities
}

func (n *namedBackend) Name() string               { return n.name }
func (n *namedBackend) Capabilities() Capabilities { return n.caps }

func TestDispatcherRoutesByModel(t *testing.T) {
	def := &namedBackend{name: "claude"}
	local := &namedBackend{name: "ollama", caps: Capabilities{MaxTokens: true}}
	d := &Dispatcher{
		Backend: def,
		Limiter: NewLimiter(2),
		Routes:  map[string]Backend{"llama3": local},
	}

	if err := d.Do(context.Background(), InferRequest{Model: "sonnet"}, &collectSink{}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if def.calls.Load() != 1 || local.calls.Load() != 0 {
		t.Fatalf("unrouted model must go to the default backend (def=%d local=%d)",
			def.calls.Load(), local.calls.Load())
	}

	if err := d.Do(context.Background(), InferRequest{Model: "llama3"}, &collectSink{}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if local.calls.Load() != 1 {
		t.Fatal("a routed model must reach its backend")
	}

	// Capabilities follow the routed backend, not the default one.
	if !d.For("llama3").Capabilities().MaxTokens {
		t.Error("For() must resolve the backend serving this model")
	}
	if d.For("sonnet").Name() != "claude" {
		t.Error("For() must fall back to the default backend")
	}
}

func TestDispatcherPerRequestTimeout(t *testing.T) {
	// A request may ask for a shorter deadline than the dispatcher default.
	fb := &fakeBackend{block: true}
	d := &Dispatcher{Backend: fb, Limiter: NewLimiter(1), Timeout: time.Hour}

	start := time.Now()
	err := d.Do(context.Background(), InferRequest{Timeout: 30 * time.Millisecond}, &collectSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v: the per-request timeout was ignored", elapsed)
	}
}

func TestDispatcherTimeout(t *testing.T) {
	fb := &fakeBackend{block: true}
	d := &Dispatcher{Backend: fb, Limiter: NewLimiter(1), Timeout: 30 * time.Millisecond}

	err := d.Do(context.Background(), InferRequest{}, &collectSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (REQ-PROC-06)", err)
	}
}
