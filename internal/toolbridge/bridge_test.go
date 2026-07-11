package toolbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/s-celles/agent-relay/internal/core"
)

func newBridge(t *testing.T) *Bridge {
	t.Helper()
	b, err := New(time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

var weather = []core.Tool{{
	Name:        "get_weather",
	Description: "Get the weather",
	InputSchema: []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
}}

// rpc posts one JSON-RPC message to the session's MCP endpoint, as the CLI
// would.
func rpc(t *testing.T, s *Session, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, _ := http.NewRequest("POST", s.URL(), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d", method, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s: decode: %v", method, err)
	}
	return out
}

func TestToolsListExposesClientTools(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)
	defer b.Close()

	out := rpc(t, s, "tools/list", nil)
	result := out["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" || tool["description"] != "Get the weather" {
		t.Errorf("tool = %v", tool)
	}
	if _, ok := tool["inputSchema"].(map[string]any); !ok {
		t.Errorf("inputSchema missing or not an object: %v", tool["inputSchema"])
	}
}

func TestToolCallParksUntilResult(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)

	// The CLI calls the tool; the handler must park (not answer) until the
	// caller supplies a result.
	done := make(chan map[string]any, 1)
	go func() {
		done <- rpc(t, s, "tools/call", map[string]any{
			"name":      "get_weather",
			"arguments": map[string]any{"city": "Paris"},
			"_meta":     map[string]any{"claudecode/toolUseId": "toolu_1"},
		})
	}()

	// The bridge surfaces the pending call.
	var call *Call
	select {
	case call = <-s.Calls():
	case <-time.After(2 * time.Second):
		t.Fatal("no pending call surfaced")
	}
	if call.Name != "get_weather" || call.ID != "toolu_1" {
		t.Fatalf("call = %+v", call)
	}
	if string(call.Input) == "" {
		t.Fatal("call input lost")
	}

	select {
	case <-done:
		t.Fatal("the MCP handler answered before a result was supplied")
	case <-time.After(100 * time.Millisecond):
	}

	// Supplying the result unblocks the CLI.
	if err := s.Resolve("toolu_1", "sunny, 24C", false); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	select {
	case out := <-done:
		res := out["result"].(map[string]any)
		content := res["content"].([]any)
		first := content[0].(map[string]any)
		if first["text"] != "sunny, 24C" {
			t.Errorf("content = %v", content)
		}
		if res["isError"] == true {
			t.Error("isError should be false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the MCP handler did not resume after Resolve")
	}
}

func TestResolveErrorResult(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)

	done := make(chan map[string]any, 1)
	go func() {
		done <- rpc(t, s, "tools/call", map[string]any{
			"name": "get_weather", "arguments": map[string]any{},
			"_meta": map[string]any{"claudecode/toolUseId": "toolu_2"},
		})
	}()
	<-s.Calls()
	s.Resolve("toolu_2", "boom", true)

	out := <-done
	res := out["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("isError = %v, want true", res["isError"])
	}
}

func TestResolveUnknownCall(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)
	if err := s.Resolve("nope", "x", false); err == nil {
		t.Fatal("resolving an unknown call must fail")
	}
}

func TestSessionLookupAndClose(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)

	if got := b.Session(s.ID()); got != s {
		t.Fatal("session lookup failed")
	}
	b.CloseSession(s.ID())
	if got := b.Session(s.ID()); got != nil {
		t.Fatal("closed session must be gone")
	}
}

func TestMCPRequiresSessionToken(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)

	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	req, _ := http.NewRequest("POST", s.URL(), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestParkedCallFailsWhenSessionCloses(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)

	done := make(chan map[string]any, 1)
	go func() {
		done <- rpc(t, s, "tools/call", map[string]any{
			"name": "get_weather", "arguments": map[string]any{},
			"_meta": map[string]any{"claudecode/toolUseId": "toolu_3"},
		})
	}()
	<-s.Calls()

	// A caller that never returns must not park the CLI forever.
	b.CloseSession(s.ID())
	select {
	case out := <-done:
		res := out["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("a closed session should fail the pending call: %v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the session did not release the parked call")
	}
}

func TestServerNameAndAllowedTools(t *testing.T) {
	b := newBridge(t)
	s := b.NewSession(weather)
	got := s.AllowedTools()
	if len(got) != 1 || got[0] != "mcp__"+ServerName+"__get_weather" {
		t.Errorf("allowed tools = %v", got)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(s.MCPConfig()), &cfg); err != nil {
		t.Fatalf("mcp config is not JSON: %v", err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers[ServerName]; !ok {
		t.Errorf("mcp config missing the %q server: %v", ServerName, cfg)
	}
}

var _ = context.Background
