// Package a2a exposes the relay as an Agent2Agent (A2A) agent: a third wire
// adapter alongside the Anthropic and OpenAI ones.
//
// It is an adapter, not a backend and not a router — the relay does not call
// other A2A agents, it *is* one. Peers discover it through an Agent Card and
// drive it with tasks; the adapter translates a task into the neutral
// core.InferRequest and the backend's event stream back into A2A task status
// and artifacts.
//
// The protocol itself (JSON-RPC binding, SSE, task store, agent card, state
// machine) is the official SDK's, github.com/a2aproject/a2a-go. A2A v1.0 was a
// breaking redesign of 0.3 and keeps moving; hand-rolling the wire would mean
// our tests validated our *reading* of the spec rather than the spec. This is
// the one place the relay takes a dependency — the core, the backends and the
// security path stay standard-library only (NFR-INSPECT-01).
package a2a

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"mime"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/s-celles/agent-relay/internal/core"
)

// agenticHeader carries the agentic credential, exactly as on the Anthropic
// and OpenAI endpoints: one authorization model for the whole relay.
const agenticHeader = "X-Agentic-Authorization"

// FileRef is one file in a retained agentic workspace.
type FileRef struct {
	Path string
	Size int64
}

// Relay is what the adapter needs from the rest of the relay. Keeping it an
// interface is what lets this package stay a wire adapter: it knows nothing
// about backends, limiters, or the outputs store.
type Relay interface {
	// Dispatch runs one inference, routed and rate-limited like any other.
	Dispatch(ctx context.Context, req core.InferRequest, sink core.EventSink) error
	// AuthorizeAgentic reports whether this credential may run agentically.
	// An empty credential is not an error: it means plain inference.
	AuthorizeAgentic(cred string) (bool, error)
	// NewWorkspace allocates a retained working directory for an agentic task.
	NewWorkspace() (id, dir string, err error)
	// WorkspaceDir resolves an existing one, so a follow-up task in the same
	// A2A context reuses it.
	WorkspaceDir(id string) (string, error)
	// WorkspaceFiles lists what the agent produced there.
	WorkspaceFiles(id string) ([]FileRef, error)
}

type Config struct {
	Relay Relay
	// DefaultModel serves messages that name none: A2A is an agent protocol,
	// so it has no model field.
	DefaultModel string
	// PublicURL is the origin peers reach this relay on. It is what the agent
	// card advertises and what artifact URLs are built from.
	PublicURL string
	Logger    *slog.Logger
}

// conversation is the state an A2A context carries between tasks: the backend
// session to resume, and the workspace whose files become artifacts.
type conversation struct {
	sessionID   string
	workspaceID string
}

// Executor implements a2asrv.AgentExecutor: it turns an inbound A2A message
// into a dispatched inference, and the resulting core events into A2A events.
type Executor struct {
	relay        Relay
	defaultModel string
	publicURL    string
	logger       *slog.Logger

	mu       sync.Mutex
	convos   map[string]*conversation          // A2A contextId -> conversation
	inflight map[a2a.TaskID]context.CancelFunc // running tasks, for CancelTask
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

func NewExecutor(cfg Config) *Executor {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	model := cfg.DefaultModel
	if model == "" {
		model = "sonnet"
	}
	return &Executor{
		relay:        cfg.Relay,
		defaultModel: model,
		publicURL:    strings.TrimSuffix(cfg.PublicURL, "/"),
		logger:       logger,
		convos:       map[string]*conversation{},
		inflight:     map[a2a.TaskID]context.CancelFunc{},
	}
}

// Execute runs one A2A task. Errors raised before any event is yielded become
// JSON-RPC errors (the task never existed); everything after is reported as a
// task state, which is what A2A expects — a broken backend is a FAILED task,
// not a transport fault.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.Message == nil {
			yield(nil, errors.New("no message to execute"))
			return
		}
		req, err := e.buildRequest(ec)
		if err != nil {
			yield(nil, err)
			return
		}

		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		e.run(ctx, ec, req, yield)
	}
}

// buildRequest resolves everything the task needs before the backend is
// touched: content, model, agentic authorization, workspace, session.
func (e *Executor) buildRequest(ec *a2asrv.ExecutorContext) (core.InferRequest, error) {
	req, err := ToInferRequest(ec.Message, e.defaultModel)
	if err != nil {
		return core.InferRequest{}, err
	}
	req.Stream = true // the executor always consumes events incrementally

	agentic, err := e.relay.AuthorizeAgentic(credential(ec))
	if err != nil {
		return core.InferRequest{}, err
	}
	req.Agentic = agentic

	cv := e.conversation(ec.ContextID)
	e.mu.Lock()
	req.SessionID = cv.sessionID
	workspaceID := cv.workspaceID
	e.mu.Unlock()

	if agentic {
		// An agentic task with an ephemeral workspace would produce artifacts
		// and then delete them. Retaining it also keeps the workspace stable
		// across the context, which is what lets the backend resume its
		// session (it keys sessions by working directory).
		if workspaceID == "" {
			id, dir, err := e.relay.NewWorkspace()
			if err != nil {
				return core.InferRequest{}, fmt.Errorf("allocate workspace: %w", err)
			}
			e.mu.Lock()
			cv.workspaceID = id
			e.mu.Unlock()
			req.OutputDir = dir
		} else {
			dir, err := e.relay.WorkspaceDir(workspaceID)
			if err != nil {
				return core.InferRequest{}, fmt.Errorf("resolve workspace: %w", err)
			}
			req.OutputDir = dir
		}
	}
	return req, nil
}

// run dispatches the backend and pumps its events out as A2A events. The
// backend pushes (EventSink) while A2A pulls (iter.Seq2), so a goroutine and a
// channel bridge the two.
func (e *Executor) run(ctx context.Context, ec *a2asrv.ExecutorContext, req core.InferRequest, yield func(a2a.Event, error) bool) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.track(ec.TaskID, cancel)
	defer e.untrack(ec.TaskID)

	events := make(chan core.Event, 32)
	done := make(chan error, 1)
	go func() {
		defer close(events)
		done <- e.relay.Dispatch(runCtx, req, &chanSink{ch: events, ctx: runCtx})
	}()

	var (
		artifactID a2a.ArtifactID
		pending    string // one chunk held back, so the last one can be marked
		havePend   bool
		backendErr error
	)

	// emit sends the held-back chunk, learning the artifact id from the first.
	emit := func(text string, last bool) bool {
		var ev *a2a.TaskArtifactUpdateEvent
		if artifactID == "" {
			ev = a2a.NewArtifactEvent(ec, a2a.NewTextPart(text))
			ev.Artifact.Name = "response"
			artifactID = ev.Artifact.ID
		} else {
			ev = a2a.NewArtifactUpdateEvent(ec, artifactID, a2a.NewTextPart(text))
			ev.Append = true
		}
		ev.LastChunk = last
		return yield(ev, nil)
	}

	for ev := range events {
		switch ev.Kind {
		case core.EventTextDelta:
			if ev.Text == "" {
				continue
			}
			if havePend && !emit(pending, false) {
				cancel()
				drain(events)
				return
			}
			pending, havePend = ev.Text, true
		case core.EventSession:
			// The backend's own conversation id: remembered against the A2A
			// context so the next task in it resumes rather than restarts.
			e.setSession(ec.ContextID, ev.SessionID)
		case core.EventError:
			backendErr = ev.Err
		}
	}
	if err := <-done; err != nil && backendErr == nil {
		backendErr = err
	}

	if havePend && !emit(pending, backendErr == nil) {
		return
	}
	if errors.Is(backendErr, context.Canceled) {
		// CancelTask stopped it; that path owns the terminal state. Reporting
		// FAILED here too would race it with a wrong answer.
		e.logger.Info("a2a task canceled", "task_id", ec.TaskID)
		return
	}
	if backendErr != nil {
		e.logger.Error("a2a task failed", "task_id", ec.TaskID, "err", backendErr)
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed,
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(backendErr.Error()))), nil)
		return
	}
	if !e.yieldFiles(ec, yield) {
		return
	}
	yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
}

// yieldFiles turns the files an agentic task produced into artifacts. A2A
// defines no download endpoint, so each is a url part pointing back at the
// relay's outputs endpoint, fetched out of band with the same bearer token.
func (e *Executor) yieldFiles(ec *a2asrv.ExecutorContext, yield func(a2a.Event, error) bool) bool {
	e.mu.Lock()
	cv, ok := e.convos[ec.ContextID]
	var workspaceID string
	if ok {
		workspaceID = cv.workspaceID
	}
	e.mu.Unlock()
	if workspaceID == "" {
		return true
	}

	files, err := e.relay.WorkspaceFiles(workspaceID)
	if err != nil {
		e.logger.Error("list workspace files", "err", err, "task_id", ec.TaskID)
		return true // the answer still stands; only the artifacts are missing
	}
	base := fmt.Sprintf("%s/v1/outputs/%s/files/", e.publicURL, workspaceID)
	for _, f := range files {
		part := a2a.NewFileURLPart(a2a.URL(base+escapePath(f.Path)), MediaTypeOf(f.Path))
		part.Filename = f.Path
		part.SetMeta("size", f.Size)

		ev := a2a.NewArtifactEvent(ec, part)
		ev.Artifact.Name = path.Base(f.Path)
		ev.LastChunk = true
		if !yield(ev, nil) {
			return false
		}
	}
	return true
}

// Cancel stops a running task. Cancelling the dispatch context is what reaches
// the backend — for the claude CLI that is a process-group kill.
func (e *Executor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.mu.Lock()
		cancel, running := e.inflight[ec.TaskID]
		e.mu.Unlock()
		if running {
			cancel()
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

func (e *Executor) track(id a2a.TaskID, cancel context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inflight[id] = cancel
}

func (e *Executor) untrack(id a2a.TaskID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.inflight, id)
}

func (e *Executor) conversation(contextID string) *conversation {
	e.mu.Lock()
	defer e.mu.Unlock()
	cv, ok := e.convos[contextID]
	if !ok {
		cv = &conversation{}
		e.convos[contextID] = cv
	}
	return cv
}

func (e *Executor) setSession(contextID, sessionID string) {
	if sessionID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cv, ok := e.convos[contextID]; ok {
		cv.sessionID = sessionID
	}
}

// credential reads the agentic credential from the request headers, which the
// JSON-RPC transport exposes as service params.
func credential(ec *a2asrv.ExecutorContext) string {
	if ec.ServiceParams == nil {
		return ""
	}
	values, ok := ec.ServiceParams.Get(agenticHeader)
	if !ok || len(values) == 0 {
		return ""
	}
	cred := values[0]
	if tok, found := strings.CutPrefix(cred, "Bearer "); found {
		return tok
	}
	return cred
}

// chanSink is the push-to-pull bridge: the backend emits into a channel that
// Execute ranges over.
type chanSink struct {
	ch  chan<- core.Event
	ctx context.Context
}

func (c *chanSink) Emit(ctx context.Context, ev core.Event) error {
	select {
	case c.ch <- ev:
		return nil
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

// drain lets the backend goroutine finish writing after the consumer gave up,
// so it never blocks on a channel nobody reads.
func drain(ch <-chan core.Event) {
	for range ch {
	}
}

// ToInferRequest maps an inbound A2A message onto the neutral request. A2A is
// an agent protocol and has no model field, so the model is named — when the
// peer cares — in message.metadata.model.
func ToInferRequest(m *a2a.Message, defaultModel string) (core.InferRequest, error) {
	if m.Role != "" && m.Role != a2a.MessageRoleUser {
		return core.InferRequest{}, fmt.Errorf("message role must be %s, got %q", a2a.MessageRoleUser, m.Role)
	}
	req := core.InferRequest{Model: defaultModel}
	if v, ok := m.Metadata["model"].(string); ok && v != "" {
		req.Model = v
	}

	var blocks []core.Block
	for i, p := range m.Parts {
		if p == nil {
			continue
		}
		switch content := p.Content.(type) {
		case a2a.Text:
			if content == "" {
				continue
			}
			blocks = append(blocks, core.Block{Kind: core.BlockText, Text: string(content)})
		case a2a.Raw:
			if p.MediaType == "" {
				return core.InferRequest{}, fmt.Errorf("part %d: a raw part requires mediaType", i)
			}
			blocks = append(blocks, core.Block{
				Kind: core.BlockFile, MediaType: p.MediaType, Data: []byte(content),
			})
		case a2a.URL:
			// The relay never fetches on a peer's behalf: an outbound GET
			// driven by a caller-supplied URL is an SSRF primitive, and the
			// whole security model rests on the relay only ever talking to its
			// own backends.
			return core.InferRequest{}, fmt.Errorf(
				"part %d: url parts are not accepted; send the bytes as a raw part", i)
		case a2a.Data:
			// Structured data is context for the *peer's* pipeline, not prompt
			// content. Splicing it into the transcript would let a peer smuggle
			// instructions past the text parts.
			continue
		}
	}
	if len(blocks) == 0 {
		return core.InferRequest{}, errors.New("message carries no content")
	}
	req.Messages = []core.Message{{Role: core.RoleUser, Blocks: blocks}}
	return req, nil
}

func escapePath(p string) string {
	segs := strings.Split(strings.ReplaceAll(p, `\`, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// mediaTypes pins the types we care about rather than trusting the host's mime
// database, which varies by platform (and has no entry for .md on some).
var mediaTypes = map[string]string{
	".md":    "text/markdown",
	".txt":   "text/plain",
	".log":   "text/plain",
	".json":  "application/json",
	".jsonl": "application/x-ndjson",
	".csv":   "text/csv",
	".html":  "text/html",
	".xml":   "application/xml",
	".yaml":  "application/yaml",
	".yml":   "application/yaml",
	".toml":  "application/toml",
	".pdf":   "application/pdf",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".svg":   "image/svg+xml",
	".zip":   "application/zip",
	".py":    "text/x-python",
	".go":    "text/x-go",
	".sh":    "text/x-shellscript",
}

// MediaTypeOf guesses an artifact's media type from its extension.
func MediaTypeOf(p string) string {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/")))
	if t, ok := mediaTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if base, _, err := mime.ParseMediaType(t); err == nil {
			return base
		}
	}
	return "application/octet-stream"
}
