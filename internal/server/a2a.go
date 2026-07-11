package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	a2aapi "github.com/s-celles/agent-relay/internal/api/a2a"
	"github.com/s-celles/agent-relay/internal/config"
	"github.com/s-celles/agent-relay/internal/core"
	"github.com/s-celles/agent-relay/internal/outputs"
)

// a2aRelay adapts the server to what the A2A executor needs. It is the seam
// that keeps the protocol adapter free of the relay's internals — and keeps
// the relay free of the SDK's types.
type a2aRelay struct{ s *server }

// Dispatch runs an A2A task through the same path as every other request:
// routed by model, capped by the limiter, bounded by the request timeout,
// counted in the metrics.
func (a *a2aRelay) Dispatch(ctx context.Context, req core.InferRequest, sink core.EventSink) error {
	a.s.metrics.RequestStarted()
	defer a.s.metrics.RequestFinished()

	trace := a.s.traceFor(req)
	defer trace.Close()

	// Unlike the HTTP sinks, this one forwards EventSession: the executor maps
	// the backend's conversation id onto the A2A contextId, and there is no
	// response header here to carry it.
	observed := &a2aSink{EventSink: sink, trace: trace}
	err := a.s.dispatcher.Do(ctx, req, observed)
	a.s.accountUsageOn("/a2a", observed.usage)
	if err != nil {
		a.s.metrics.BackendError()
	}
	return err
}

func (a *a2aRelay) AuthorizeAgentic(cred string) (bool, error) {
	agentic, err := a.s.authorizeAgenticCred(cred)
	switch {
	case err != nil:
		a.s.metrics.AgenticDenied()
		a.s.logger.Warn("agentic request denied", "path", "/a2a", "reason", err.Error())
		return false, errors.New(agenticMessage(err))
	case agentic:
		a.s.logger.Info("agentic request authorized", "path", "/a2a")
	}
	return agentic, nil
}

func (a *a2aRelay) NewWorkspace() (string, string, error) {
	id := outputs.NewID()
	dir, err := a.s.outputs.Create(id)
	if err != nil {
		return "", "", err
	}
	a.s.logger.Info("agentic outputs retained", "outputs_id", id, "path", "/a2a")
	return id, dir, nil
}

func (a *a2aRelay) WorkspaceDir(id string) (string, error) { return a.s.outputs.Dir(id) }

func (a *a2aRelay) WorkspaceFiles(id string) ([]a2aapi.FileRef, error) {
	files, err := a.s.outputs.List(id)
	if err != nil {
		return nil, err
	}
	refs := make([]a2aapi.FileRef, 0, len(files))
	for _, f := range files {
		refs = append(refs, a2aapi.FileRef{Path: f.Path, Size: f.Size})
	}
	return refs, nil
}

// a2aSink observes usage and writes traces on their way through, and — unlike
// usageSink — lets EventSession reach the adapter.
type a2aSink struct {
	core.EventSink
	usage core.Usage
	trace *traceWriter
}

func (a *a2aSink) Emit(ctx context.Context, ev core.Event) error {
	if ev.Kind == core.EventMessageStop && ev.Usage != nil {
		a.usage = *ev.Usage
	}
	a.trace.record(ev)
	return a.EventSink.Emit(ctx, ev)
}

// Denial reasons, as they appear in the audit log. The message sent to the
// caller is deliberately the wordier one (see agenticMessage): the log wants a
// stable, greppable reason, the caller wants to know what to do.
var (
	errAgenticDisabled   = errors.New("agentic execution disabled")
	errAgenticCredential = errors.New("invalid credential")
)

// authorizeAgenticCred is the credential half of authorizeAgentic, shared with
// the A2A adapter so the two surfaces cannot drift apart on the one decision
// that matters most.
func (s *server) authorizeAgenticCred(cred string) (bool, error) {
	switch {
	case cred == "":
		// No credential: agentic only in the legacy all-requests posture
		// (enabled without per-request authz, loopback-only by Config.Validate);
		// otherwise plain inference.
		return s.agentic.Enabled && !s.agentic.PerRequestAuthz, nil
	case !s.agentic.Enabled:
		return false, errAgenticDisabled
	case !s.agentic.PerRequestAuthz:
		return true, nil
	default:
		for _, t := range s.agenticTokens {
			if subtle.ConstantTimeCompare([]byte(cred), t) == 1 {
				return true, nil
			}
		}
		return false, errAgenticCredential
	}
}

// agenticMessage is what a denied caller is told.
func agenticMessage(err error) string {
	if errors.Is(err, errAgenticDisabled) {
		return "agentic execution is disabled on this relay"
	}
	return "invalid agentic authorization"
}

// servedModels is what the Agent Card advertises: A2A carries no model field,
// so naming them is the only way a peer learns what it may ask for in
// message.metadata.model. The default model is always named — it is the one a
// peer gets by asking for nothing.
func servedModels(cfg config.Config, backend core.Backend, routes map[string]core.Backend) []string {
	seen := map[string]bool{}
	var models []string
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	add(cfg.A2AModel)
	for _, m := range backend.Capabilities().Models {
		add(m)
	}
	for m := range routes {
		add(m)
	}
	sort.Strings(models)
	return models
}

// mountA2A adds the Agent2Agent surface: a JSON-RPC endpoint behind the usual
// auth and quota, and the Agent Card — which is public, because discovery is
// the point of a card and a peer must read it before it holds any credential.
func (s *server) mountA2A(mux *http.ServeMux, cfg config.Config, models []string,
	throttledAuth func(http.Handler, func(http.ResponseWriter, int, string)) http.Handler) {

	executor := a2aapi.NewExecutor(a2aapi.Config{
		Relay:        &a2aRelay{s: s},
		DefaultModel: cfg.A2AModel,
		PublicURL:    cfg.PublicURL,
		Logger:       s.logger,
	})
	handler := a2asrv.NewHandler(executor, a2asrv.WithLogger(s.logger))

	mux.Handle("POST /a2a", throttledAuth(a2asrv.NewJSONRPCHandler(handler), writeA2AError))
	mux.Handle("GET "+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(
		a2aapi.NewAgentCard(a2aapi.CardConfig{
			BaseURL: cfg.PublicURL,
			Version: s.version,
			Models:  models,
			Agentic: cfg.Agentic.Enabled,
		})))
	s.logger.Info("a2a enabled", "endpoint", cfg.PublicURL+"/a2a",
		"card", cfg.PublicURL+a2asrv.WellKnownAgentCardPath, "agentic", cfg.Agentic.Enabled)
}

// writeA2AError renders the relay's transport-level failures (auth, quota,
// busy) as a JSON-RPC error body, so an A2A peer parses one shape whatever
// went wrong. The HTTP status is preserved: that is what a proxy and a
// backpressure-aware client read.
func writeA2AError(w http.ResponseWriter, status int, msg string) {
	code := -32603 // internal error
	if status == http.StatusBadRequest {
		code = -32600 // invalid request
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": code, "message": msg},
	})
}
