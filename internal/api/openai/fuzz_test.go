package openai

import (
	"strings"
	"testing"
)

// FuzzDecodeRequest asserts the decoder never panics on arbitrary bytes and
// that every accepted request satisfies the invariants the rest of the relay
// relies on: a non-empty model and at least one decoded message.
func FuzzDecodeRequest(f *testing.F) {
	seeds := []string{
		``,
		`{}`,
		`not json at all`,
		`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gpt","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
		`{"model":"gpt","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}]}`,
		`{"model":"gpt","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		`{"model":"gpt","messages":[{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`,
		`{"model":"gpt","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gpt","tools":[{"type":"retrieval"}],"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gpt","messages":[{"role":"robot","content":"hi"}]}`,
		`{"model":"gpt","messages":[{"role":"system","content":"only system"}]}`,
		`{"model":"gpt","max_completion_tokens":10,"stream":true,"stream_options":{"include_usage":true},"stop":["x"],"messages":[{"role":"user","content":"hi"}]}`,
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
	})
}
