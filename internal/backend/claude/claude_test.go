package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

// stubCLI writes an executable shell script standing in for the `claude`
// binary, so tests exercise spawn/parse/stdin/env/kill without spending a
// single token (spec §9).
func stubCLI(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type collectSink struct{ events []core.Event }

func (c *collectSink) Emit(ctx context.Context, ev core.Event) error {
	c.events = append(c.events, ev)
	return nil
}

func newTestBackend(t *testing.T, cfg core.BackendConfig) core.Backend {
	t.Helper()
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

const happyScript = `
cat > /dev/null
echo '{"type":"system","subtype":"init","session_id":"abc"}'
echo '{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":3}}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}'
echo 'this line is not json and must be ignored'
echo '{"type":"unknown_future_type","payload":42}'
echo '{"type":"result","subtype":"success","result":"Hello","usage":{"input_tokens":3,"output_tokens":5}}'
`

func TestInferHappyPath(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, happyScript)})
	sink := &collectSink{}

	err := b.Infer(context.Background(), core.InferRequest{
		Model:    "sonnet",
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "hi")},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}

	var kinds []core.EventKind
	var text strings.Builder
	for _, ev := range sink.events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == core.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	want := []core.EventKind{
		core.EventSession, // the init line names the conversation
		core.EventMessageStart,
		core.EventTextDelta, core.EventTextDelta,
		core.EventMessageStop,
	}
	if len(kinds) != len(want) {
		t.Fatalf("got kinds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d: got kind %v, want %v", i, kinds[i], want[i])
		}
	}
	if text.String() != "Hello" {
		t.Errorf("assembled text = %q, want %q", text.String(), "Hello")
	}
	last := sink.events[len(sink.events)-1]
	if last.Usage == nil || last.Usage.InputTokens != 3 || last.Usage.OutputTokens != 5 {
		t.Errorf("final usage = %+v", last.Usage)
	}
	// message_start reports input tokens; forward them so wire adapters can
	// render a faithful message_start instead of zeros.
	var start *core.Event
	for i, ev := range sink.events {
		if ev.Kind == core.EventMessageStart {
			start = &sink.events[i]
		}
	}
	if start == nil || start.Usage == nil || start.Usage.InputTokens != 3 {
		t.Errorf("message_start usage = %+v, want input_tokens 3", start)
	}
}

func TestInferPipesPromptViaStdin(t *testing.T) {
	// The stub echoes back what it received on stdin as a delta (REQ-PROC-01).
	script := `
input=$(cat)
printf '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"%s"}}}\n' "$input"
echo '{"type":"result","subtype":"success","result":"","usage":{"input_tokens":1,"output_tokens":1}}'
`
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, script)})
	sink := &collectSink{}
	err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "ping-marker-42")},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var got string
	for _, ev := range sink.events {
		if ev.Kind == core.EventTextDelta {
			got += ev.Text
		}
	}
	if !strings.Contains(got, "ping-marker-42") {
		t.Errorf("prompt was not piped via stdin; echoed %q", got)
	}
}

func TestInferSanitizesEnv(t *testing.T) {
	// REQ-PROC-05/07: base-url and CLAUDECODE vars must not reach the child.
	t.Setenv("ANTHROPIC_BASE_URL", "http://relay.local")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("MY_OPERATOR_SECRET", "s3cret")

	script := `
cat > /dev/null
if [ -n "$ANTHROPIC_BASE_URL" ] || [ -n "$CLAUDECODE" ] || [ -n "$MY_OPERATOR_SECRET" ]; then
  echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"leaked"}}}'
else
  echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"clean"}}}'
fi
echo '{"type":"result","subtype":"success","result":"","usage":{"input_tokens":1,"output_tokens":1}}'
`
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: stubCLI(t, script),
		EnvDeny: []string{"MY_OPERATOR_SECRET"},
	})
	sink := &collectSink{}
	if err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, sink); err != nil {
		t.Fatalf("Infer: %v", err)
	}
	for _, ev := range sink.events {
		if ev.Kind == core.EventTextDelta && ev.Text != "clean" {
			t.Fatalf("env leak detected: child saw denied variables (got %q)", ev.Text)
		}
	}
}

func TestInferKillsProcessOnCancel(t *testing.T) {
	// REQ-PROC-04: cancellation must terminate the subprocess promptly.
	script := `
cat > /dev/null
sleep 60
`
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, script)})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := b.Infer(ctx, core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, &collectSink{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Infer should report an error on cancellation")
	}
	// The cause must survive: callers distinguish a deadline or a disconnect
	// from a backend failure (the server maps it to 504).
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Infer took %v to return after cancel; subprocess not killed", elapsed)
	}
}

func TestInferReportsDeadlineExceeded(t *testing.T) {
	script := `
cat > /dev/null
sleep 60
`
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, script)})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := b.Infer(ctx, core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, &collectSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestInferReportsSpawnFailure(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "/nonexistent/claude-cli"})
	err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, &collectSink{})
	if err == nil {
		t.Fatal("Infer should fail when the CLI cannot be spawned")
	}
}

func TestInferReportsBackendError(t *testing.T) {
	// The real CLI exits 1 after an error result line; the parsed error event
	// must win over the bare exit status, so Infer returns nil here and the
	// sink carries the useful message.
	script := `
cat > /dev/null
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}'
exit 1
`
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, script)})
	sink := &collectSink{}
	if err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, sink); err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var sawError bool
	for _, ev := range sink.events {
		if ev.Kind == core.EventError {
			sawError = true
			if ev.Err == nil || !strings.Contains(ev.Err.Error(), "boom") {
				t.Errorf("error event = %v, want message containing %q", ev.Err, "boom")
			}
		}
	}
	if !sawError {
		t.Fatal("expected an EventError from an error result line")
	}
}

// cwdScript reports the subprocess working directory back as a text delta.
const cwdScript = `
cat > /dev/null
printf '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"%s"}}}\n' "$PWD"
echo '{"type":"result","subtype":"success","result":"","usage":{"input_tokens":1,"output_tokens":1}}'
`

func reportedCwd(t *testing.T, b core.Backend, agentic bool) string {
	t.Helper()
	sink := &collectSink{}
	if err := b.Infer(context.Background(), core.InferRequest{
		Agentic:  agentic,
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, sink); err != nil {
		t.Fatalf("Infer: %v", err)
	}
	for _, ev := range sink.events {
		if ev.Kind == core.EventTextDelta {
			return ev.Text
		}
	}
	t.Fatal("no cwd delta received")
	return ""
}

func TestAgenticEphemeralWorkdir(t *testing.T) {
	// REQ-EXEC-04: each agentic request runs in its own ephemeral directory
	// under the configured workdir, removed once the request finishes.
	base := t.TempDir()
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: stubCLI(t, cwdScript),
		Workdir: base,
		Agentic: core.AgenticConfig{Enabled: true},
	})

	first := reportedCwd(t, b, true)
	second := reportedCwd(t, b, true)

	// The child reports its cwd with symlinks resolved (macOS: /var ->
	// /private/var), so compare against the resolved base.
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{first, second} {
		if dir == resolvedBase {
			t.Fatal("agentic request ran directly in the base workdir, not an ephemeral subdirectory")
		}
		if filepath.Dir(dir) != resolvedBase {
			t.Fatalf("ephemeral dir %q is not under configured base %q", dir, resolvedBase)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("ephemeral dir %q still exists after Infer returned", dir)
		}
	}
	if first == second {
		t.Fatalf("two requests shared the same workdir %q", first)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("base workdir not clean after requests: %v", entries)
	}

	// A non-agentic request on the same backend stays in the static base dir
	// (REQ-EXEC-06: agentic is a per-request property).
	inferenceCwd := reportedCwd(t, b, false)
	if inferenceCwd != resolvedBase {
		t.Fatalf("non-agentic request cwd = %q, want static base %q", inferenceCwd, resolvedBase)
	}
}

func TestAgenticEphemeralWorkdirDefaultBase(t *testing.T) {
	// With no configured workdir, the ephemeral dir lives under the system
	// temp dir — never the relay's own working directory.
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: stubCLI(t, cwdScript),
		Agentic: core.AgenticConfig{Enabled: true},
	})
	dir := reportedCwd(t, b, true)
	relayCwd, _ := os.Getwd()
	if dir == relayCwd {
		t.Fatal("agentic request inherited the relay's working directory")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral dir %q still exists after Infer returned", dir)
	}
}

func TestInferenceWorkdirIsConfiguredDir(t *testing.T) {
	// Inference mode keeps the static configured workdir (spec §3).
	base := t.TempDir()
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: stubCLI(t, cwdScript),
		Workdir: base,
	})
	got := reportedCwd(t, b, false)
	// Resolve symlinks: on macOS t.TempDir() lives under /var -> /private/var.
	want, _ := filepath.EvalSymlinks(base)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Fatalf("cwd = %q, want configured workdir %q", got, base)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("configured workdir must survive the request: %v", err)
	}
}

func TestBuildArgs(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{
		CLIPath:  "claude",
		ModelMap: map[string]string{"sonnet": "claude-sonnet-5"},
	}).(*Backend)

	args := b.buildArgs(core.InferRequest{Model: "sonnet", System: "be brief"})
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-p", "--output-format stream-json", "--verbose", "--include-partial-messages",
		"--model claude-sonnet-5", "--system-prompt be brief",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	// REQ-EXEC-02: no permission-bypass flags on the default inference path.
	for _, forbidden := range []string{"--dangerously-skip-permissions", "--permission-mode"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("args %q must not contain %q in inference mode", joined, forbidden)
		}
	}
}

func TestBuildArgsAgenticPerRequest(t *testing.T) {
	// Permission args are applied per request, not per backend (REQ-EXEC-06).
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: "claude",
		Agentic: core.AgenticConfig{Enabled: true, ExtraArgs: []string{"--permission-mode", "acceptEdits"}},
	}).(*Backend)

	plain := strings.Join(b.buildArgs(core.InferRequest{Model: "m"}), " ")
	if strings.Contains(plain, "--permission-mode") {
		t.Errorf("non-agentic request got permission args: %q", plain)
	}
	agentic := strings.Join(b.buildArgs(core.InferRequest{Model: "m", Agentic: true}), " ")
	if !strings.Contains(agentic, "--permission-mode acceptEdits") {
		t.Errorf("agentic request missing permission args: %q", agentic)
	}
}

func TestInferRejectsAgenticWhenDisabled(t *testing.T) {
	// Defense in depth: even if the server mislabels a request, a backend not
	// configured for agentic execution must refuse it without spawning.
	b := newTestBackend(t, core.BackendConfig{CLIPath: "/nonexistent/claude-cli"})
	sink := &collectSink{}
	err := b.Infer(context.Background(), core.InferRequest{
		Agentic:  true,
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, sink)
	if err == nil {
		t.Fatal("agentic request on a non-agentic backend must fail")
	}
	if strings.Contains(err.Error(), "spawn") {
		t.Fatalf("request must be refused before spawning, got: %v", err)
	}
}

func TestBuildArgsToolBridge(t *testing.T) {
	// Client tools reach the CLI as an MCP server, allowlisted by name — and
	// the allowlist must not grant the CLI's own Write/Bash tools.
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	args := b.buildArgs(core.InferRequest{
		Model: "m",
		ToolBridge: &core.ToolBridge{
			Config:       `{"mcpServers":{"relay":{"type":"http","url":"http://127.0.0.1:1/mcp/x"}}}`,
			AllowedTools: []string{"mcp__relay__get_weather", "mcp__relay__lookup"},
		},
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--mcp-config") || !strings.Contains(joined, "mcpServers") {
		t.Errorf("args missing --mcp-config: %q", joined)
	}
	if !strings.Contains(joined, "--allowedTools mcp__relay__get_weather,mcp__relay__lookup") {
		t.Errorf("args missing the tool allowlist: %q", joined)
	}
	for _, forbidden := range []string{"Write", "Bash", "--permission-mode", "--dangerously"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("tool requests must stay inference-mode; args contain %q: %q", forbidden, joined)
		}
	}
	// No bridge, no flags.
	if strings.Contains(strings.Join(b.buildArgs(core.InferRequest{Model: "m"}), " "), "--mcp-config") {
		t.Error("--mcp-config must not appear without a tool bridge")
	}
}

func TestBuildArgsResume(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	args := b.buildArgs(core.InferRequest{
		Model:     "m",
		SessionID: "984f3680-403a-4275-9b3f-eeed6b8100bf",
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume 984f3680-403a-4275-9b3f-eeed6b8100bf") {
		t.Errorf("args missing --resume: %q", joined)
	}
	// No session: no flag.
	if strings.Contains(strings.Join(b.buildArgs(core.InferRequest{Model: "m"}), " "), "--resume") {
		t.Error("--resume must not appear without a session id")
	}
}

func TestInferRejectsMalformedSessionID(t *testing.T) {
	// A session id becomes an argv element: anything but a UUID (notably a
	// leading dash) must be refused before spawning.
	b := newTestBackend(t, core.BackendConfig{CLIPath: "/nonexistent/claude"})
	for _, bad := range []string{"--dangerously-skip-permissions", "../../etc", "not a uuid", "x;y"} {
		err := b.Infer(context.Background(), core.InferRequest{
			SessionID: bad,
			Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "x")},
		}, &collectSink{})
		if err == nil || strings.Contains(err.Error(), "spawn") {
			t.Errorf("session id %q must be refused before spawn, got %v", bad, err)
		}
	}
}

func TestInferReportsSessionID(t *testing.T) {
	// The CLI's init line carries the session id; forward it so the caller
	// can resume later.
	script := `
cat > /dev/null
echo '{"type":"system","subtype":"init","session_id":"984f3680-403a-4275-9b3f-eeed6b8100bf"}'
echo '{"type":"result","subtype":"success","usage":{"input_tokens":1,"output_tokens":1}}'
`
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, script)})
	sink := &collectSink{}
	if err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, sink); err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var got string
	for _, ev := range sink.events {
		if ev.Kind == core.EventSession {
			got = ev.SessionID
		}
	}
	if got != "984f3680-403a-4275-9b3f-eeed6b8100bf" {
		t.Errorf("session id = %q", got)
	}
}

func TestBuildArgsModelPassthrough(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	args := b.buildArgs(core.InferRequest{Model: "some-raw-model"})
	if !strings.Contains(strings.Join(args, " "), "--model some-raw-model") {
		t.Errorf("unmapped model should pass through; args = %q", args)
	}
}

func TestEncodePromptFlattensToolBlocks(t *testing.T) {
	// Structured history degrades gracefully: tool_use/tool_result blocks are
	// rendered as text in the transcript, so mixed conversations still work.
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	prompt := b.encodePrompt(core.InferRequest{
		Messages: []core.Message{
			core.NewTextMessage(core.RoleUser, "weather?"),
			{Role: core.RoleAssistant, Blocks: []core.Block{
				{Kind: core.BlockText, Text: "Checking."},
				{Kind: core.BlockToolUse, ToolID: "toolu_1", ToolName: "get_weather", ToolInput: []byte(`{"city":"Paris"}`)},
			}},
			{Role: core.RoleUser, Blocks: []core.Block{
				{Kind: core.BlockToolResult, ToolID: "toolu_1", Text: "sunny"},
			}},
			core.NewTextMessage(core.RoleUser, "and tomorrow?"),
		},
	})
	for _, want := range []string{"weather?", "Checking.", "get_weather", `{"city":"Paris"}`, "sunny", "and tomorrow?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestSingleTextMessagePassesThrough(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	prompt := b.encodePrompt(core.InferRequest{
		Messages: []core.Message{core.NewTextMessage(core.RoleUser, "just this")},
	})
	if prompt != "just this" {
		t.Errorf("single user text message must pass through verbatim, got %q", prompt)
	}
}

func TestMaterializeFiles(t *testing.T) {
	// Files land inside the given directory — the subprocess workdir — so the
	// CLI's Read tool can view them without a permission grant (reads within
	// the working directory are auto-allowed; outside they are not).
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	dir := t.TempDir()
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}
	req := core.InferRequest{Messages: []core.Message{
		{Role: core.RoleUser, Blocks: []core.Block{
			{Kind: core.BlockFile, MediaType: "image/png", Data: pngBytes},
			{Kind: core.BlockText, Text: "describe this"},
		}},
	}}

	got, err := b.materializeFiles(req, dir)
	if err != nil {
		t.Fatalf("materializeFiles: %v", err)
	}

	blocks := got.Messages[0].Blocks
	if blocks[0].Kind != core.BlockText {
		t.Fatalf("file block was not rewritten to text: %+v", blocks[0])
	}
	ref := blocks[0].Text
	if !strings.Contains(ref, "file-1.png") || !strings.Contains(ref, "Read") {
		t.Errorf("reference text should name the file and the Read tool: %q", ref)
	}
	path := extractPath(t, ref, `/\S+file-1\.png`)
	if filepath.Dir(path) != dir {
		t.Errorf("file %q not inside the workdir %q", path, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("materialized file unreadable: %v", err)
	}
	if string(data) != string(pngBytes) {
		t.Errorf("file content mismatch: %v", data)
	}
	if blocks[1].Text != "describe this" {
		t.Errorf("sibling text block altered: %+v", blocks[1])
	}
}

func TestMaterializeFilesNoopWithoutFiles(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	req := core.InferRequest{Messages: []core.Message{core.NewTextMessage(core.RoleUser, "hi")}}
	got, err := b.materializeFiles(req, t.TempDir())
	if err != nil {
		t.Fatalf("materializeFiles: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Blocks[0].Text != "hi" {
		t.Fatalf("noop request altered: %+v", got.Messages)
	}
}

func TestAgenticOutputDirIsUsedAndRetained(t *testing.T) {
	// When the server supplies OutputDir (X-Agentic-Keep-Outputs), the
	// agentic request runs there and the directory survives Infer — its
	// lifecycle belongs to the server's output store, not the backend.
	outDir := t.TempDir()
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: stubCLI(t, cwdScript),
		Agentic: core.AgenticConfig{Enabled: true},
	})
	sink := &collectSink{}
	err := b.Infer(context.Background(), core.InferRequest{
		Agentic:   true,
		OutputDir: outDir,
		Messages:  []core.Message{core.NewTextMessage(core.RoleUser, "x")},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var cwd string
	for _, ev := range sink.events {
		if ev.Kind == core.EventTextDelta {
			cwd = ev.Text
		}
	}
	want, _ := filepath.EvalSymlinks(outDir)
	if cwd != want {
		t.Fatalf("cwd = %q, want the supplied OutputDir %q", cwd, want)
	}
	if _, err := os.Stat(outDir); err != nil {
		t.Fatalf("OutputDir must survive Infer: %v", err)
	}
}

func TestInferWithFilesRunsInEphemeralWorkdir(t *testing.T) {
	// A file-carrying inference request must run with the ephemeral dir as
	// cwd, so Read is auto-allowed on the materialized files.
	base := t.TempDir()
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, cwdScript), Workdir: base})
	sink := &collectSink{}
	err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.Block{
				{Kind: core.BlockFile, MediaType: "image/png", Data: []byte{1}},
				{Kind: core.BlockText, Text: "x"},
			}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var cwd string
	for _, ev := range sink.events {
		if ev.Kind == core.EventTextDelta {
			cwd = ev.Text
		}
	}
	resolvedBase, _ := filepath.EvalSymlinks(base)
	if cwd == resolvedBase {
		t.Fatal("file-carrying request ran in the static workdir; Read would need a permission grant")
	}
	if filepath.Dir(cwd) != resolvedBase {
		t.Fatalf("ephemeral cwd %q not under base %q", cwd, resolvedBase)
	}
}

func TestInferCleansUpMaterializedFiles(t *testing.T) {
	// The echoed prompt reveals the materialized path; after Infer returns,
	// the file must be gone regardless of outcome.
	script := `
input=$(cat)
printf '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"%s"}}}\n' "$input" | tr '\n' ' '
echo
echo '{"type":"result","subtype":"success","result":"","usage":{"input_tokens":1,"output_tokens":1}}'
`
	b := newTestBackend(t, core.BackendConfig{CLIPath: stubCLI(t, script)})
	sink := &collectSink{}
	err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{
			{Role: core.RoleUser, Blocks: []core.Block{
				{Kind: core.BlockFile, MediaType: "application/pdf", Data: []byte("%PDF-1.4")},
				{Kind: core.BlockText, Text: "summarize"},
			}},
		},
	}, sink)
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	var echoed string
	for _, ev := range sink.events {
		if ev.Kind == core.EventTextDelta {
			echoed += ev.Text
		}
	}
	path := extractPath(t, echoed, `/\S+file-1\.pdf`)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("materialized file %q still exists after Infer", path)
	}
}

func extractPath(t *testing.T, s, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindString(s)
	if m == "" {
		t.Fatalf("no path matching %q in %q", pattern, s)
	}
	return m
}

func TestRegisteredInCore(t *testing.T) {
	b, err := core.New("claude", core.BackendConfig{CLIPath: "claude"})
	if err != nil {
		t.Fatalf("core.New(claude): %v", err)
	}
	if b.Name() != "claude" {
		t.Errorf("Name = %q", b.Name())
	}
	caps := b.Capabilities()
	if !caps.Streaming {
		t.Error("claude backend must report streaming capability")
	}
	if caps.Agentic {
		t.Error("agentic must be off by default (REQ-EXEC-01)")
	}
	if !caps.ClientTools {
		t.Error("the CLI serves client-defined tools through the relay's MCP bridge")
	}
	if caps.MaxTokens {
		t.Error("the claude CLI has no max-tokens flag; MaxTokens must be false")
	}
	if caps.Sampling {
		t.Error("the claude CLI has no sampling flags; Sampling must be false")
	}
}

func TestParseAgentToolActivity(t *testing.T) {
	// The CLI's own agent loop reports its tool calls on `assistant` lines
	// and their outcomes on `user` lines; forward them as trace events.
	assistant := `{"type":"assistant","message":{"content":[
		{"type":"text","text":"I'll write the file."},
		{"type":"tool_use","id":"toolu_1","name":"Write","input":{"file_path":"/tmp/x","content":"hi"}}
	]}}`
	evs := parseStreamJSONLine([]byte(assistant))
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 (text blocks must not be re-emitted)", len(evs))
	}
	ev := evs[0]
	if ev.Kind != core.EventAgentToolUse || ev.ToolName != "Write" || ev.ToolID != "toolu_1" {
		t.Fatalf("ev = %+v", ev)
	}
	if !strings.Contains(string(ev.ToolInput), "/tmp/x") {
		t.Errorf("tool input lost: %s", ev.ToolInput)
	}

	user := `{"type":"user","message":{"content":[
		{"type":"tool_result","tool_use_id":"toolu_1","content":"File created","is_error":false}
	]}}`
	evs = parseStreamJSONLine([]byte(user))
	if len(evs) != 1 || evs[0].Kind != core.EventAgentToolResult {
		t.Fatalf("evs = %+v", evs)
	}
	if evs[0].ToolID != "toolu_1" || !strings.Contains(evs[0].Text, "File created") {
		t.Errorf("ev = %+v", evs[0])
	}

	// A text-only assistant line stays ignored: its text already reached the
	// client as content_block_delta events.
	if evs := parseStreamJSONLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"dup"}]}}`)); len(evs) != 0 {
		t.Fatalf("text-only assistant line must be ignored, got %+v", evs)
	}
}

func TestParseResultCarriesCost(t *testing.T) {
	// The CLI reports the real dollar cost of the turn; forward it so the
	// relay can attribute spend per request.
	line := `{"type":"result","subtype":"success","total_cost_usd":0.0228547,"usage":{"input_tokens":10,"output_tokens":20}}`
	evs := parseStreamJSONLine([]byte(line))
	if len(evs) != 1 || evs[0].Kind != core.EventMessageStop {
		t.Fatalf("evs = %+v", evs)
	}
	if evs[0].Usage == nil {
		t.Fatal("usage missing")
	}
	if evs[0].Usage.CostUSD != 0.0228547 {
		t.Errorf("CostUSD = %v, want 0.0228547", evs[0].Usage.CostUSD)
	}
}

func TestParseStreamJSONLine(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		wantOK bool
		want   core.EventKind
	}{
		{"message start", `{"type":"stream_event","event":{"type":"message_start"}}`, true, core.EventMessageStart},
		{"text delta", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}}`, true, core.EventTextDelta},
		{"success result", `{"type":"result","subtype":"success","usage":{"input_tokens":1,"output_tokens":2}}`, true, core.EventMessageStop},
		{"result with cost", `{"type":"result","subtype":"success","total_cost_usd":0.0228,"usage":{"input_tokens":1,"output_tokens":2}}`, true, core.EventMessageStop},
		{"error result", `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"limit"}`, true, core.EventError},
		{"init line ignored", `{"type":"system","subtype":"init"}`, false, 0},
		{"text-only assistant line ignored", `{"type":"assistant","message":{"content":[{"type":"text","text":"dup"}]}}`, false, 0},
		{"non-text delta ignored", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}}`, false, 0},
		{"garbage ignored", `not json at all`, false, 0},
		{"unknown type ignored", `{"type":"telemetry_v9"}`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := parseStreamJSONLine([]byte(tc.line))
			if (len(evs) > 0) != tc.wantOK {
				t.Fatalf("got %d events, wantOK = %v (DQ-1: parse defensively)", len(evs), tc.wantOK)
			}
			if tc.wantOK && evs[0].Kind != tc.want {
				t.Fatalf("kind = %v, want %v", evs[0].Kind, tc.want)
			}
		})
	}
}
