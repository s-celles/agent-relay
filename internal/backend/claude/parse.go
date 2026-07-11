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

// wireContentBlock is one block of an assistant/user message line — the CLI's
// own agent-loop traffic (its tool calls and their results).
type wireContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// streamLine is the (deliberately partial) shape of one stream-json line.
// Unknown types and fields are ignored rather than rejected, so schema drift
// in the CLI degrades gracefully instead of breaking the relay (DQ-1).
type streamLine struct {
	Type      string     `json:"type"`
	Subtype   string     `json:"subtype"`
	IsError   bool       `json:"is_error"`
	Result    string     `json:"result"`
	Usage     *wireUsage `json:"usage"`
	CostUSD   float64    `json:"total_cost_usd"`
	SessionID string     `json:"session_id"`
	Message   *struct {
		Content []wireContentBlock `json:"content"`
	} `json:"message"`
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		Message *struct {
			Usage *wireUsage `json:"usage"`
		} `json:"message"`
	} `json:"event"`
}

// parseStreamJSONLine maps one line of `claude --output-format stream-json
// --include-partial-messages` output to neutral events. An empty result means
// the line carries nothing the relay forwards.
func parseStreamJSONLine(line []byte) []core.Event {
	var l streamLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}

	switch l.Type {
	case "system":
		// The init line names the conversation; forward it so the caller can
		// resume this session later (it arrives before any content).
		if l.Subtype == "init" && l.SessionID != "" {
			return []core.Event{{Kind: core.EventSession, SessionID: l.SessionID}}
		}

	case "stream_event":
		if l.Event == nil {
			return nil
		}
		switch l.Event.Type {
		case "message_start":
			ev := core.Event{Kind: core.EventMessageStart}
			// The CLI reports input tokens up front; forward them so wire
			// adapters can render a faithful message_start.
			if l.Event.Message != nil && l.Event.Message.Usage != nil {
				ev.Usage = &core.Usage{
					InputTokens:  l.Event.Message.Usage.InputTokens,
					OutputTokens: l.Event.Message.Usage.OutputTokens,
				}
			}
			return []core.Event{ev}
		case "content_block_delta":
			if l.Event.Delta != nil && l.Event.Delta.Type == "text_delta" {
				return []core.Event{{Kind: core.EventTextDelta, Text: l.Event.Delta.Text}}
			}
		}

	case "assistant":
		// Trace only: the assistant's *text* already reached the client as
		// content_block_delta events, so only tool calls are forwarded.
		return agentToolUses(l)

	case "user":
		// The CLI feeds its own tool results back as user messages.
		return agentToolResults(l)

	case "result":
		if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
			msg := l.Result
			if msg == "" {
				msg = "backend reported " + l.Subtype
			}
			return []core.Event{{Kind: core.EventError, Err: errors.New(msg)}}
		}
		ev := core.Event{Kind: core.EventMessageStop}
		if l.Usage != nil || l.CostUSD > 0 {
			ev.Usage = &core.Usage{CostUSD: l.CostUSD}
			if l.Usage != nil {
				ev.Usage.InputTokens = l.Usage.InputTokens
				ev.Usage.OutputTokens = l.Usage.OutputTokens
			}
		}
		return []core.Event{ev}
	}
	return nil
}

func agentToolUses(l streamLine) []core.Event {
	if l.Message == nil {
		return nil
	}
	var evs []core.Event
	for _, b := range l.Message.Content {
		if b.Type != "tool_use" {
			continue
		}
		evs = append(evs, core.Event{
			Kind:      core.EventAgentToolUse,
			ToolID:    b.ID,
			ToolName:  b.Name,
			ToolInput: b.Input,
		})
	}
	return evs
}

func agentToolResults(l streamLine) []core.Event {
	if l.Message == nil {
		return nil
	}
	var evs []core.Event
	for _, b := range l.Message.Content {
		if b.Type != "tool_result" {
			continue
		}
		evs = append(evs, core.Event{
			Kind:    core.EventAgentToolResult,
			ToolID:  b.ToolUseID,
			Text:    truncate(flattenContent(b.Content), 4096),
			IsError: b.IsError,
		})
	}
	return evs
}

// flattenContent renders a tool_result's content (string or block array) as
// plain text for the trace.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []wireContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var out string
	for _, b := range blocks {
		if b.Type == "text" {
			if out != "" {
				out += "\n"
			}
			out += b.Text
		}
	}
	return out
}
