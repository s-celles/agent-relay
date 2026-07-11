package core

import (
	"context"
	"testing"
)

type stubBackend struct{ name string }

func (s *stubBackend) Name() string               { return s.name }
func (s *stubBackend) Capabilities() Capabilities { return Capabilities{} }
func (s *stubBackend) Infer(ctx context.Context, req InferRequest, sink EventSink) error {
	return nil
}

func TestRegistryNewKnownBackend(t *testing.T) {
	Register("stub-known", func(cfg BackendConfig) (Backend, error) {
		return &stubBackend{name: "stub-known"}, nil
	})

	b, err := New("stub-known", BackendConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Name() != "stub-known" {
		t.Fatalf("got backend %q, want %q", b.Name(), "stub-known")
	}
}

func TestRegistryNewUnknownBackend(t *testing.T) {
	if _, err := New("no-such-backend", BackendConfig{}); err == nil {
		t.Fatal("New with unknown backend should fail")
	}
}
