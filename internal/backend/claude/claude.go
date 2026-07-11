// Package claude adapts the `claude` CLI to the neutral core.Backend
// interface. It is the only package in the module that knows anything about
// this specific CLI: how to spawn it, what flags it takes, and how to parse
// its stream-json output (REQ-BK-02).
package claude

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

func init() {
	core.Register("claude", New)
}

type Backend struct {
	cliPath  string
	workdir  string
	modelMap map[string]string
	envDeny  []string
	agentic  core.AgenticConfig
}

func New(cfg core.BackendConfig) (core.Backend, error) {
	b := &Backend{
		cliPath:  cfg.CLIPath,
		workdir:  cfg.Workdir,
		modelMap: cfg.ModelMap,
		envDeny:  cfg.EnvDeny,
		agentic:  cfg.Agentic,
	}
	if b.cliPath == "" {
		b.cliPath = "claude"
	}
	return b, nil
}

func (b *Backend) Name() string { return "claude" }

func (b *Backend) Capabilities() core.Capabilities {
	models := make([]string, 0, len(b.modelMap))
	for logical := range b.modelMap {
		models = append(models, logical)
	}
	return core.Capabilities{
		Streaming: true,
		Agentic:   b.agentic.Enabled,
		// MaxTokens stays false: the claude CLI has no flag to cap output
		// tokens, so InferRequest.MaxTokens cannot be enforced. Sampling
		// stays false for the same reason: no temperature/top_p/top_k/stop
		// flags exist.
		MaxTokens: false,
		Sampling:  false,
		Models:    models,
	}
}

func (b *Backend) buildArgs(req core.InferRequest) []string {
	args := []string{
		"-p", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages",
	}
	// REQ-EXEC-02: no permission-bypass flag on the default (inference) path.
	// Agentic is a per-request property, granted by the server after
	// per-request authorization (REQ-EXEC-06).
	if b.agentic.Enabled && req.Agentic {
		args = append(args, b.agentic.PermissionArgs()...) // opt-in, explicit, logged
	}
	if m := b.mapModel(req.Model); m != "" {
		args = append(args, "--model", m)
	}
	if req.System != "" {
		args = append(args, "--system-prompt", req.System)
	}
	return args
}

// mapModel resolves a logical model name via the operator-configured table,
// passing unknown names through unchanged (DQ-2).
func (b *Backend) mapModel(logical string) string {
	if logical == "" {
		return ""
	}
	if mapped, ok := b.modelMap[logical]; ok {
		return mapped
	}
	return logical
}

// encodePrompt flattens the conversation into the text prompt the CLI reads
// from stdin. A single user text message passes through verbatim; multi-turn
// history becomes a labeled transcript. Structured blocks degrade
// gracefully: tool_use and tool_result blocks are rendered as bracketed
// text, so mixed histories still produce a coherent prompt.
func (b *Backend) encodePrompt(req core.InferRequest) string {
	if len(req.Messages) == 1 && req.Messages[0].Role == core.RoleUser && textOnly(req.Messages[0]) {
		return req.Messages[0].PlainText()
	}
	// Resolve tool_use IDs to names so tool_result lines read naturally.
	toolNames := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			if b.Kind == core.BlockToolUse {
				toolNames[b.ToolID] = b.ToolName
			}
		}
	}
	var sb strings.Builder
	// Without framing, the model reads the transcript as a fresh question and
	// ignores bracketed tool results; the preamble makes it a continuation.
	sb.WriteString("Below is a conversation transcript. Bracketed [tool ...] lines are real tool calls and their results. Reply with the Assistant's next message only, using those results.\n\n")
	for _, m := range req.Messages {
		label := "Human"
		if m.Role == core.RoleAssistant {
			label = "Assistant"
		}
		fmt.Fprintf(&sb, "%s: %s\n\n", label, renderBlocks(m.Blocks, toolNames))
	}
	return sb.String()
}

var fileExt = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

func hasFileBlocks(req core.InferRequest) bool {
	for _, m := range req.Messages {
		for _, bl := range m.Blocks {
			if bl.Kind == core.BlockFile {
				return true
			}
		}
	}
	return false
}

// materializeFiles writes every BlockFile attachment into dir — which must
// be the subprocess working directory, because the CLI's read-only Read tool
// is auto-allowed only within its cwd — and rewrites each block into a text
// reference to that path. Cleanup is the caller's: dir is the per-request
// ephemeral directory Infer already removes.
func (b *Backend) materializeFiles(req core.InferRequest, dir string) (core.InferRequest, error) {
	n := 0
	msgs := make([]core.Message, len(req.Messages))
	for i, m := range req.Messages {
		blocks := make([]core.Block, len(m.Blocks))
		for j, bl := range m.Blocks {
			if bl.Kind != core.BlockFile {
				blocks[j] = bl
				continue
			}
			n++
			ext := fileExt[bl.MediaType]
			if ext == "" {
				ext = ".bin"
			}
			path := filepath.Join(dir, fmt.Sprintf("file-%d%s", n, ext))
			if err := os.WriteFile(path, bl.Data, 0o600); err != nil {
				return req, fmt.Errorf("write attachment: %w", err)
			}
			blocks[j] = core.Block{Kind: core.BlockText, Text: fmt.Sprintf(
				"[Attached file (%s) at %s — use your Read tool on that exact path to view it.]",
				bl.MediaType, path)}
		}
		msgs[i] = core.Message{Role: m.Role, Blocks: blocks}
	}
	req.Messages = msgs
	return req, nil
}

func textOnly(m core.Message) bool {
	for _, b := range m.Blocks {
		if b.Kind != core.BlockText {
			return false
		}
	}
	return true
}

func renderBlocks(blocks []core.Block, toolNames map[string]string) string {
	var parts []string
	for _, b := range blocks {
		switch b.Kind {
		case core.BlockText:
			parts = append(parts, b.Text)
		case core.BlockToolUse:
			parts = append(parts, fmt.Sprintf("[called tool %s with input %s]", b.ToolName, string(b.ToolInput)))
		case core.BlockToolResult:
			name := toolNames[b.ToolID]
			if name == "" {
				name = b.ToolID
			}
			status := "returned"
			if b.IsError {
				status = "failed with"
			}
			parts = append(parts, fmt.Sprintf("[tool %s %s: %s]", name, status, b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func (b *Backend) Infer(ctx context.Context, req core.InferRequest, sink core.EventSink) error {
	// Defense in depth: never honor an agentic request unless this backend
	// was explicitly configured for agentic execution (REQ-EXEC-01/06).
	if req.Agentic && !b.agentic.Enabled {
		return fmt.Errorf("agentic request refused: backend %q is not configured for agentic execution", b.Name())
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	needFiles := hasFileBlocks(req)
	workdir := b.workdir
	switch {
	case b.agentic.Enabled && req.Agentic && req.OutputDir != "":
		// The server retains this request's artifacts: run in the supplied
		// directory and leave it alone — its lifecycle belongs to the output
		// store (X-Agentic-Keep-Outputs).
		workdir = req.OutputDir
	case (b.agentic.Enabled && req.Agentic) || needFiles:
		// REQ-EXEC-04: agentic requests never share state — each one runs in
		// its own ephemeral directory (under the configured workdir when set,
		// the system temp dir otherwise), removed after the process is reaped.
		// File-carrying requests get the same treatment so the Read tool is
		// auto-allowed on the materialized attachments (cwd containment).
		dir, err := os.MkdirTemp(b.workdir, "agent-relay-req-")
		if err != nil {
			return fmt.Errorf("create ephemeral workdir: %w", err)
		}
		defer os.RemoveAll(dir)
		workdir = dir
	}
	if needFiles {
		var err error
		if req, err = b.materializeFiles(req, workdir); err != nil {
			return err
		}
	}

	cmd := exec.CommandContext(ctx, b.cliPath, b.buildArgs(req)...)
	cmd.Env = b.sanitizedEnv() // REQ-PROC-05 / REQ-PROC-07
	cmd.Dir = workdir
	setProcAttrs(cmd) // kill the whole process group on cancel (REQ-PROC-04)
	cmd.WaitDelay = 5 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr := &bytes.Buffer{} // captured for diagnostics only
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn backend: %w", err)
	}

	// The Backend contract requires the subprocess to be gone before Infer
	// returns, on every exit path.
	var waitOnce sync.Once
	var waitErr error
	wait := func() error {
		waitOnce.Do(func() { waitErr = cmd.Wait() })
		return waitErr
	}
	defer func() {
		cancel()
		_ = wait()
	}()

	// REQ-PROC-01: payload piped via stdin (avoids ARG_MAX).
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, b.encodePrompt(req))
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var errorDelivered bool
	for scanner.Scan() {
		for _, ev := range parseStreamJSONLine(scanner.Bytes()) {
			if ev.Kind == core.EventError {
				errorDelivered = true
			}
			if err := sink.Emit(ctx, ev); err != nil {
				return err // client gone; deferred cancel+wait reaps the process
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read backend output: %w", err)
	}
	if err := wait(); err != nil {
		// The CLI exits non-zero after an error result line; the parsed
		// message already reached the sink and is strictly more useful than
		// the bare exit status.
		if errorDelivered {
			return nil
		}
		return fmt.Errorf("backend exited: %w (stderr: %s)", err, truncate(stderr.String(), 512))
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
