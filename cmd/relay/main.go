// Command relay is the self-hosted agent relay: an authenticating inference
// proxy that fronts agent CLIs (v1: claude) behind Anthropic- and
// OpenAI-compatible HTTP APIs.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s-celles/agent-relay/internal/config"
	"github.com/s-celles/agent-relay/internal/core"
	"github.com/s-celles/agent-relay/internal/server"

	_ "github.com/s-celles/agent-relay/internal/backend/claude" // register the claude backend
	_ "github.com/s-celles/agent-relay/internal/backend/ollama" // register the ollama backend
)

// version is what the relay reports about itself — today only on the A2A Agent
// Card, where a peer expects an agent to state its version.
const version = "0.9.1"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err // REQ-CFG-05: fail fast, never serve on a bad config
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	backend, err := core.New(cfg.Backend, cfg.Backends[cfg.Backend])
	if err != nil {
		return err
	}

	// Model-name routing (DQ-2): the client keeps choosing a model, the relay
	// decides which backend that means. Instantiate each routed backend once.
	routes := map[string]core.Backend{}
	instances := map[string]core.Backend{cfg.Backend: backend}
	for model, name := range cfg.ModelRoutes {
		b, ok := instances[name]
		if !ok {
			if b, err = core.New(name, cfg.Backends[name]); err != nil {
				return fmt.Errorf("model route %q: %w", model, err)
			}
			instances[name] = b
		}
		routes[model] = b
	}

	handler, err := server.NewRouted(cfg, backend, routes,
		server.WithLogger(logger), server.WithVersion(version))
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	logger.Info("relay listening",
		"addr", cfg.BindAddr,
		"backend", backend.Name(),
		"max_concurrent", cfg.MaxConcurrent,
		"auth", len(cfg.Tokens) > 0,
		"agentic", cfg.Agentic.Enabled,
	)
	if cfg.Agentic.Enabled {
		logger.Warn("agentic execution mode is ENABLED (REQ-EXEC-01: this is an explicit opt-in)")
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return nil
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
