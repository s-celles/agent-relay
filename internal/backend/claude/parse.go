package claude

import (
	"encoding/json"
	"errors"

	"github.com/s-celles/agent-relay/internal/core"
)

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// streamLine is the (deliberately partial) shape of one stream-json line.
// Unknown types and fields are ignored rather than rejected, so schema drift
// in the CLI degrades gracefully instead of breaking the relay (DQ-1).
type streamLine struct {
	Type    string     `json:"type"`
	Subtype string     `json:"subtype"`
	IsError bool       `json:"is_error"`
	Result  string     `json:"result"`
	Usage   *wireUsage `json:"usage"`
	Event   *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
}

// parseStreamJSONLine maps one line of `claude --output-format stream-json
// --include-partial-messages` output to a neutral event. ok is false for
// lines that carry nothing the relay forwards.
func parseStreamJSONLine(line []byte) (core.Event, bool) {
	var l streamLine
	if err := json.Unmarshal(line, &l); err != nil {
		return core.Event{}, false
	}

	switch l.Type {
	case "stream_event":
		if l.Event == nil {
			return core.Event{}, false
		}
		switch l.Event.Type {
		case "message_start":
			return core.Event{Kind: core.EventMessageStart}, true
		case "content_block_delta":
			if l.Event.Delta != nil && l.Event.Delta.Type == "text_delta" {
				return core.Event{Kind: core.EventTextDelta, Text: l.Event.Delta.Text}, true
			}
		}
	case "result":
		if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
			msg := l.Result
			if msg == "" {
				msg = "backend reported " + l.Subtype
			}
			return core.Event{Kind: core.EventError, Err: errors.New(msg)}, true
		}
		ev := core.Event{Kind: core.EventMessageStop}
		if l.Usage != nil {
			ev.Usage = &core.Usage{
				InputTokens:  l.Usage.InputTokens,
				OutputTokens: l.Usage.OutputTokens,
			}
		}
		return ev, true
	}
	return core.Event{}, false
}
