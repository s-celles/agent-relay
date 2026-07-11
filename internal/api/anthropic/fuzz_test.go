package anthropic

import (
	"strings"
	"testing"
)

// FuzzDecodeRequest asserts the decoder never panics on arbitrary bytes and
// that every accepted request satisfies the invariants the rest of the relay
// relies on: a non-empty model and at least one message with a valid role.
func FuzzDecodeRequest(f *testing.F) {
	seeds := []string{
		``,
		`{}`,
		`not json at all`,
		`{"model":"haiku","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"haiku","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
		`{"model":"haiku","system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"haiku","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}]}]}`,
		`{"model":"haiku","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]}]}`,
		`{"model":"haiku","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}`,
		`{"model":"haiku","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"text/evil","data":"aGk="}}]}]}`,
		`{"model":"haiku","tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"haiku","messages":[{"role":"robot","content":"hi"}]}`,
		`{"model":"haiku","messages":[{"role":"user","content":[{"type":"text"}],"extra":1}],"stream":true,"temperature":0.5,"top_k":3}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := DecodeRequest(strings.NewReader(string(data)))
		if err != nil {
			return
		}
		if req.Model == "" {
			t.Errorf("accepted request with empty model: %q", data)
		}
		if len(req.Messages) == 0 {
			t.Errorf("accepted request with no messages: %q", data)
		}
		for i, m := range req.Messages {
			if m.Role != "user" && m.Role != "assistant" {
				t.Errorf("messages[%d] has invalid role %q: %q", i, m.Role, data)
			}
		}
	})
}
