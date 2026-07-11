package toolbridge

import (
	"encoding/json"
	"net/http"
	"time"
)

// protocolVersion is the MCP revision the relay speaks. The server surface is
// deliberately tiny: initialize, tools/list, tools/call.
const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent on notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      struct {
		ToolUseID string `json:"claudecode/toolUseId"`
	} `json:"_meta"`
}

func (b *Bridge) handleMCP(w http.ResponseWriter, r *http.Request) {
	s, err := b.session(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Notifications carry no id and expect no reply.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": "1"},
		})
	case "tools/list":
		writeResult(w, req.ID, map[string]any{"tools": s.tools})
	case "tools/call":
		b.handleToolCall(w, r, s, req)
	default:
		// Unknown methods are answered empty rather than erroring: the relay
		// only needs the three above, and MCP clients probe for more.
		writeResult(w, req.ID, map[string]any{})
	}
}

// handleToolCall parks: it surfaces the call to the relay, then blocks until
// the caller resolves it (or the deadline passes). The CLI subprocess stays
// alive and idle in the meantime — that is what makes client-side tool
// execution possible across two HTTP requests.
func (b *Bridge) handleToolCall(w http.ResponseWriter, r *http.Request, s *Session, req rpcRequest) {
	var p callParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeToolResult(w, req.ID, "invalid tool call parameters", true)
		return
	}
	id := p.Meta.ToolUseID
	if id == "" {
		id = "toolu_" + randomHex()
	}
	input := p.Arguments
	if len(input) == 0 || !json.Valid(input) {
		input = json.RawMessage(`{}`)
	}

	pend := &pending{result: make(chan callResult, 1)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		writeToolResult(w, req.ID, "the relay closed this tool session", true)
		return
	}
	s.pending[id] = pend
	s.mu.Unlock()

	select {
	case s.calls <- &Call{ID: id, Name: p.Name, Input: input}:
	case <-r.Context().Done():
		s.forget(id)
		return
	}

	select {
	case res := <-pend.result:
		writeToolResult(w, req.ID, res.text, res.isError)
	case <-time.After(b.callWait):
		s.forget(id)
		writeToolResult(w, req.ID, "the caller did not return a tool result in time", true)
	case <-r.Context().Done():
		s.forget(id)
	}
}

func (s *Session) forget(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func writeToolResult(w http.ResponseWriter, id json.RawMessage, text string, isError bool) {
	writeResult(w, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	})
}
