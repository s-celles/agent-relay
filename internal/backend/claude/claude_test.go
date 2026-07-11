package claude

import (
	"context"
	"os"
	"path/filepath"
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
		Messages: []core.Message{{Role: core.RoleUser, Content: "hi"}},
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
		Messages: []core.Message{{Role: core.RoleUser, Content: "ping-marker-42"}},
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
		Messages: []core.Message{{Role: core.RoleUser, Content: "x"}},
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
		Messages: []core.Message{{Role: core.RoleUser, Content: "x"}},
	}, &collectSink{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Infer should report an error on cancellation")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Infer took %v to return after cancel; subprocess not killed", elapsed)
	}
}

func TestInferReportsSpawnFailure(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "/nonexistent/claude-cli"})
	err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{{Role: core.RoleUser, Content: "x"}},
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
		Messages: []core.Message{{Role: core.RoleUser, Content: "x"}},
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

func reportedCwd(t *testing.T, b core.Backend) string {
	t.Helper()
	sink := &collectSink{}
	if err := b.Infer(context.Background(), core.InferRequest{
		Messages: []core.Message{{Role: core.RoleUser, Content: "x"}},
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

	first := reportedCwd(t, b)
	second := reportedCwd(t, b)

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
}

func TestAgenticEphemeralWorkdirDefaultBase(t *testing.T) {
	// With no configured workdir, the ephemeral dir lives under the system
	// temp dir — never the relay's own working directory.
	b := newTestBackend(t, core.BackendConfig{
		CLIPath: stubCLI(t, cwdScript),
		Agentic: core.AgenticConfig{Enabled: true},
	})
	dir := reportedCwd(t, b)
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
	got := reportedCwd(t, b)
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

func TestBuildArgsModelPassthrough(t *testing.T) {
	b := newTestBackend(t, core.BackendConfig{CLIPath: "claude"}).(*Backend)
	args := b.buildArgs(core.InferRequest{Model: "some-raw-model"})
	if !strings.Contains(strings.Join(args, " "), "--model some-raw-model") {
		t.Errorf("unmapped model should pass through; args = %q", args)
	}
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
		{"error result", `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"limit"}`, true, core.EventError},
		{"init line ignored", `{"type":"system","subtype":"init"}`, false, 0},
		{"assistant line ignored", `{"type":"assistant","message":{"content":[{"type":"text","text":"dup"}]}}`, false, 0},
		{"non-text delta ignored", `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}}`, false, 0},
		{"garbage ignored", `not json at all`, false, 0},
		{"unknown type ignored", `{"type":"telemetry_v9"}`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := parseStreamJSONLine([]byte(tc.line))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (DQ-1: parse defensively)", ok, tc.wantOK)
			}
			if ok && ev.Kind != tc.want {
				t.Fatalf("kind = %v, want %v", ev.Kind, tc.want)
			}
		})
	}
}
