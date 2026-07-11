package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/s-celles/agent-relay/internal/core"
)

// These tests drive the adapter through the SDK's own JSON-RPC transport, so
// what they assert is the wire an A2A peer actually sees — not our reading of
// the specification.

// fakeRelay scripts a backend and records what the adapter asked it for.
type fakeRelay struct {
	mu      sync.Mutex
	seen    []core.InferRequest
	emit    func(context.Context, core.EventSink) error
	agentic func(cred string) (bool, error)
	files   []FileRef
	dirs    map[string]string // workspace id -> dir
}

func (f *fakeRelay) Dispatch(ctx context.Context, req core.InferRequest, sink core.EventSink) error {
	f.mu.Lock()
	f.seen = append(f.seen, req)
	f.mu.Unlock()
	if f.emit == nil {
		return emitText(ctx, sink, "ok")
	}
	return f.emit(ctx, sink)
}

func (f *fakeRelay) AuthorizeAgentic(cred string) (bool, error) {
	if f.agentic == nil {
		return false, nil
	}
	return f.agentic(cred)
}

func (f *fakeRelay) NewWorkspace() (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs == nil {
		f.dirs = map[string]string{}
	}
	id := "ws" + strings.Repeat("0", 30) + string(rune('a'+len(f.dirs)))
	f.dirs[id] = "/tmp/" + id
	return id, f.dirs[id], nil
}

func (f *fakeRelay) WorkspaceDir(id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir, ok := f.dirs[id]
	if !ok {
		return "", errors.New("unknown workspace")
	}
	return dir, nil
}

func (f *fakeRelay) WorkspaceFiles(string) ([]FileRef, error) { return f.files, nil }

func (f *fakeRelay) lastRequest(t *testing.T) core.InferRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("the backend was never dispatched to")
	}
	return f.seen[len(f.seen)-1]
}

func emitText(ctx context.Context, sink core.EventSink, chunks ...string) error {
	if err := sink.Emit(ctx, core.Event{Kind: core.EventMessageStart}); err != nil {
		return err
	}
	for _, c := range chunks {
		if err := sink.Emit(ctx, core.Event{Kind: core.EventTextDelta, Text: c}); err != nil {
			return err
		}
	}
	return sink.Emit(ctx, core.Event{Kind: core.EventMessageStop})
}

func newTestServer(t *testing.T, relay Relay, agenticCard bool) *httptest.Server {
	t.Helper()
	exec := NewExecutor(Config{
		Relay:        relay,
		DefaultModel: "sonnet",
		PublicURL:    "http://relay.test",
	})
	mux := http.NewServeMux()
	mux.Handle("/a2a", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)))
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(
		NewAgentCard(CardConfig{
			BaseURL: "http://relay.test",
			Version: "0.8.0",
			Models:  []string{"sonnet", "llama3"},
			Agentic: agenticCard,
		})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// rpc issues a blocking JSON-RPC call and returns the decoded envelope.
func rpc(t *testing.T, srv *httptest.Server, method string, params any, headers map[string]string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("%s: decode: %v", method, err)
	}
	return env
}

// textMessage builds the params of SendMessage for a plain question.
func textMessage(text string) map[string]any {
	return map[string]any{"message": map[string]any{
		"messageId": "m1",
		"role":      "ROLE_USER",
		"parts":     []any{map[string]any{"text": text}},
	}}
}

// result digs the task out of a successful envelope, failing on a JSON-RPC error.
func result(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	if e, ok := env["error"]; ok {
		t.Fatalf("unexpected JSON-RPC error: %v", e)
	}
	res, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", env)
	}
	return res
}

func taskOf(t *testing.T, env map[string]any) map[string]any {
	t.Helper()
	res := result(t, env)
	task, ok := res["task"].(map[string]any)
	if !ok {
		t.Fatalf("result is not a task: %v", res)
	}
	return task
}

func stateOf(t *testing.T, task map[string]any) string {
	t.Helper()
	status, _ := task["status"].(map[string]any)
	state, _ := status["state"].(string)
	return state
}

// artifactText concatenates the text parts of the task's artifacts.
func artifactText(task map[string]any) string {
	var sb strings.Builder
	arts, _ := task["artifacts"].([]any)
	for _, a := range arts {
		art, _ := a.(map[string]any)
		parts, _ := art["parts"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if txt, ok := part["text"].(string); ok {
				sb.WriteString(txt)
			}
		}
	}
	return sb.String()
}

func TestAgentCardIsServedAndDescribesTheRelay(t *testing.T) {
	srv := newTestServer(t, &fakeRelay{}, true)
	resp, err := srv.Client().Get(srv.URL + a2asrv.WellKnownAgentCardPath)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	defer resp.Body.Close()
	var card map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}

	if card["name"] != "agent-relay" {
		t.Errorf("name = %v", card["name"])
	}
	ifaces, _ := card["supportedInterfaces"].([]any)
	if len(ifaces) != 1 {
		t.Fatalf("supportedInterfaces = %v", card["supportedInterfaces"])
	}
	first, _ := ifaces[0].(map[string]any)
	if first["url"] != "http://relay.test/a2a" || first["protocolBinding"] != "JSONRPC" {
		t.Errorf("interface = %v", first)
	}
	caps, _ := card["capabilities"].(map[string]any)
	if caps["streaming"] != true {
		t.Errorf("streaming must be advertised: %v", caps)
	}
	if _, ok := card["securitySchemes"]; !ok {
		t.Error("bearer auth must be advertised on the card")
	}
	// The model names a peer may pass in message.metadata.model.
	if !strings.Contains(card["description"].(string), "sonnet") {
		t.Errorf("description should name the servable models: %v", card["description"])
	}
	if !hasSkill(card, SkillAgenticTask) {
		t.Error("the agentic skill must be advertised when agentic mode is on")
	}
}

func TestAgentCardHidesAgenticSkillWhenDisabled(t *testing.T) {
	srv := newTestServer(t, &fakeRelay{}, false)
	resp, err := srv.Client().Get(srv.URL + a2asrv.WellKnownAgentCardPath)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	defer resp.Body.Close()
	var card map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if hasSkill(card, SkillAgenticTask) {
		t.Error("a relay with agentic execution off must not advertise the agentic skill")
	}
	if !hasSkill(card, SkillChat) {
		t.Error("chat is always available")
	}
}

func hasSkill(card map[string]any, id string) bool {
	skills, _ := card["skills"].([]any)
	for _, s := range skills {
		if sk, ok := s.(map[string]any); ok && sk["id"] == id {
			return true
		}
	}
	return false
}

func TestSendMessageBlocksUntilTerminalAndReturnsTheAnswerAsAnArtifact(t *testing.T) {
	relay := &fakeRelay{emit: func(ctx context.Context, sink core.EventSink) error {
		return emitText(ctx, sink, "Hel", "lo ", "world")
	}}
	srv := newTestServer(t, relay, false)

	task := taskOf(t, rpc(t, srv, "SendMessage", textMessage("hi"), nil))

	if got := stateOf(t, task); got != "TASK_STATE_COMPLETED" {
		t.Errorf("state = %q, want COMPLETED", got)
	}
	if got := artifactText(task); got != "Hello world" {
		t.Errorf("artifact text = %q, want the streamed chunks joined", got)
	}
	if task["id"] == "" || task["contextId"] == "" {
		t.Errorf("the server must generate both ids: %v", task)
	}
	if got := relay.lastRequest(t).Model; got != "sonnet" {
		t.Errorf("model = %q, want the configured default", got)
	}
}

func TestMessageMetadataSelectsTheModel(t *testing.T) {
	relay := &fakeRelay{}
	srv := newTestServer(t, relay, false)

	params := textMessage("hi")
	params["message"].(map[string]any)["metadata"] = map[string]any{"model": "llama3"}
	rpc(t, srv, "SendMessage", params, nil)

	if got := relay.lastRequest(t).Model; got != "llama3" {
		t.Errorf("model = %q; A2A has no model field, so metadata.model must route", got)
	}
}

func TestRawPartBecomesAnAttachment(t *testing.T) {
	relay := &fakeRelay{}
	srv := newTestServer(t, relay, false)

	params := map[string]any{"message": map[string]any{
		"messageId": "m1", "role": "ROLE_USER",
		"parts": []any{
			map[string]any{"text": "what colour?"},
			map[string]any{
				"raw":       base64.StdEncoding.EncodeToString([]byte{0, 1, 2}),
				"mediaType": "image/png",
				"filename":  "x.png",
			},
		},
	}}
	rpc(t, srv, "SendMessage", params, nil)

	blocks := relay.lastRequest(t).Messages[0].Blocks
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want text + file", blocks)
	}
	if blocks[1].Kind != core.BlockFile || blocks[1].MediaType != "image/png" || len(blocks[1].Data) != 3 {
		t.Errorf("file block = %+v", blocks[1])
	}
}

func TestURLPartIsRefused(t *testing.T) {
	srv := newTestServer(t, &fakeRelay{}, false)
	params := map[string]any{"message": map[string]any{
		"messageId": "m1", "role": "ROLE_USER",
		"parts": []any{map[string]any{"url": "http://169.254.169.254/latest/meta-data/"}},
	}}
	env := rpc(t, srv, "SendMessage", params, nil)
	// Fetching a peer-supplied URL would make the relay an SSRF primitive.
	if _, ok := env["error"]; !ok {
		t.Fatalf("a url part must be refused, got %v", env)
	}
}

func TestBackendFailureFailsTheTaskRatherThanTheCall(t *testing.T) {
	relay := &fakeRelay{emit: func(ctx context.Context, sink core.EventSink) error {
		return errors.New("boom")
	}}
	srv := newTestServer(t, relay, false)

	// A2A carries application outcomes inside a successful result: a broken
	// backend is a FAILED task, not a JSON-RPC error.
	task := taskOf(t, rpc(t, srv, "SendMessage", textMessage("hi"), nil))
	if got := stateOf(t, task); got != "TASK_STATE_FAILED" {
		t.Fatalf("state = %q, want FAILED", got)
	}
	status, _ := task["status"].(map[string]any)
	msg, _ := status["message"].(map[string]any)
	if msg == nil {
		t.Fatal("a failed task must explain itself in status.message")
	}
	if !strings.Contains(mustJSON(msg), "boom") {
		t.Errorf("status message = %s, want the backend error", mustJSON(msg))
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestStreamingEmitsStatusAndArtifactUpdates(t *testing.T) {
	relay := &fakeRelay{emit: func(ctx context.Context, sink core.EventSink) error {
		return emitText(ctx, sink, "Hel", "lo")
	}}
	srv := newTestServer(t, relay, false)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage",
		"params": textMessage("hi"),
	})
	req, _ := http.NewRequest("POST", srv.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var kinds []string
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var env struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(data), &env); err != nil {
			t.Fatalf("SSE datum is not a JSON-RPC envelope: %v (%s)", err, data)
		}
		for k, v := range env.Result {
			kinds = append(kinds, k)
			if k == "artifactUpdate" {
				var au struct {
					Artifact struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"artifact"`
				}
				json.Unmarshal(v, &au)
				for _, p := range au.Artifact.Parts {
					text.WriteString(p.Text)
				}
			}
		}
	}

	if len(kinds) == 0 {
		t.Fatal("no SSE events")
	}
	if kinds[0] != "task" {
		t.Errorf("first stream event = %q, want the initial task", kinds[0])
	}
	if !contains(kinds, "statusUpdate") || !contains(kinds, "artifactUpdate") {
		t.Errorf("stream = %v, want status and artifact updates", kinds)
	}
	if text.String() != "Hello" {
		t.Errorf("streamed artifact text = %q, want the chunks in order", text.String())
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestAgenticTaskReturnsItsFilesAsURLArtifacts(t *testing.T) {
	relay := &fakeRelay{
		agentic: func(cred string) (bool, error) { return cred == "secret", nil },
		files:   []FileRef{{Path: "report.md", Size: 12}},
	}
	srv := newTestServer(t, relay, true)

	task := taskOf(t, rpc(t, srv, "SendMessage", textMessage("write a report"),
		map[string]string{"X-Agentic-Authorization": "Bearer secret"}))

	if !relay.lastRequest(t).Agentic {
		t.Fatal("the request should have been dispatched agentically")
	}
	if relay.lastRequest(t).OutputDir == "" {
		t.Error("an agentic A2A task needs a retained workspace, or its files vanish")
	}
	if got := stateOf(t, task); got != "TASK_STATE_COMPLETED" {
		t.Fatalf("state = %q", got)
	}

	// A2A has no download endpoint: files are url parts fetched out of band.
	var found bool
	for _, a := range task["artifacts"].([]any) {
		art := a.(map[string]any)
		for _, p := range art["parts"].([]any) {
			part := p.(map[string]any)
			if u, ok := part["url"].(string); ok {
				found = true
				if !strings.HasPrefix(u, "http://relay.test/v1/outputs/") || !strings.HasSuffix(u, "/files/report.md") {
					t.Errorf("artifact url = %q", u)
				}
				if part["mediaType"] != "text/markdown" {
					t.Errorf("mediaType = %v", part["mediaType"])
				}
			}
		}
	}
	if !found {
		t.Errorf("no url artifact in %v", task["artifacts"])
	}
}

func TestAgenticIsRefusedWithoutCredential(t *testing.T) {
	relay := &fakeRelay{agentic: func(cred string) (bool, error) {
		if cred == "" {
			return false, nil // plain inference
		}
		return false, errors.New("invalid agentic authorization")
	}}
	srv := newTestServer(t, relay, true)

	env := rpc(t, srv, "SendMessage", textMessage("rm -rf"),
		map[string]string{"X-Agentic-Authorization": "Bearer wrong"})
	if _, ok := env["error"]; !ok {
		t.Fatalf("a bad agentic credential must fail the call, got %v", env)
	}

	// No credential at all is not an error: it is an ordinary inference task.
	task := taskOf(t, rpc(t, srv, "SendMessage", textMessage("hi"), nil))
	if stateOf(t, task) != "TASK_STATE_COMPLETED" {
		t.Errorf("state = %q", stateOf(t, task))
	}
	if relay.lastRequest(t).Agentic {
		t.Error("no credential must mean no agentic execution")
	}
}

func TestContextIDResumesTheBackendSession(t *testing.T) {
	relay := &fakeRelay{emit: func(ctx context.Context, sink core.EventSink) error {
		if err := sink.Emit(ctx, core.Event{Kind: core.EventSession, SessionID: "sess-1"}); err != nil {
			return err
		}
		return emitText(ctx, sink, "ok")
	}}
	srv := newTestServer(t, relay, false)

	first := taskOf(t, rpc(t, srv, "SendMessage", textMessage("remember 42"), nil))
	ctxID, _ := first["contextId"].(string)
	if ctxID == "" {
		t.Fatal("no contextId on the first task")
	}
	if relay.lastRequest(t).SessionID != "" {
		t.Error("a fresh context must not resume anything")
	}

	// A peer continues the conversation by echoing the contextId.
	params := textMessage("what did I say?")
	params["message"].(map[string]any)["contextId"] = ctxID
	rpc(t, srv, "SendMessage", params, nil)

	if got := relay.lastRequest(t).SessionID; got != "sess-1" {
		t.Errorf("SessionID = %q; the A2A context must map onto the backend session", got)
	}
}

func TestCancelTaskStopsTheBackend(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	relay := &fakeRelay{emit: func(ctx context.Context, sink core.EventSink) error {
		if err := sink.Emit(ctx, core.Event{Kind: core.EventMessageStart}); err != nil {
			return err
		}
		close(started)
		select {
		case <-ctx.Done(): // the cancel must reach the backend, not just the client
			return ctx.Err()
		case <-release:
			return nil
		}
	}}
	defer close(release)
	srv := newTestServer(t, relay, false)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "SendStreamingMessage", "params": textMessage("long job"),
	})
	req, _ := http.NewRequest("POST", srv.URL+"/a2a", bytes.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	taskID := firstTaskID(t, resp.Body)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the backend never started")
	}

	env := rpc(t, srv, "CancelTask", map[string]any{"id": taskID}, nil)
	if e, ok := env["error"]; ok {
		t.Fatalf("CancelTask: %v", e)
	}

	// GetTask returns the task itself, not the SendMessage task-or-message union.
	got := result(t, rpc(t, srv, "GetTask", map[string]any{"id": taskID}, nil))
	if state := stateOf(t, got); state != "TASK_STATE_CANCELED" {
		t.Errorf("state after cancel = %q", state)
	}
}

// firstTaskID reads SSE until the initial task event and returns its id.
func firstTaskID(t *testing.T, body io.Reader) string {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var env struct {
			Result struct {
				Task struct {
					ID string `json:"id"`
				} `json:"task"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(data), &env) == nil && env.Result.Task.ID != "" {
			return env.Result.Task.ID
		}
	}
	t.Fatal("no task event in the stream")
	return ""
}
