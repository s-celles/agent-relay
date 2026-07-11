package core

import "testing"

func TestNewTextMessage(t *testing.T) {
	m := NewTextMessage(RoleUser, "hello")
	if m.Role != RoleUser {
		t.Errorf("Role = %q", m.Role)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].Kind != BlockText || m.Blocks[0].Text != "hello" {
		t.Errorf("Blocks = %+v", m.Blocks)
	}
}

func TestPlainTextJoinsTextBlocks(t *testing.T) {
	m := Message{Role: RoleAssistant, Blocks: []Block{
		{Kind: BlockText, Text: "a"},
		{Kind: BlockToolUse, ToolID: "t1", ToolName: "f", ToolInput: []byte(`{}`)},
		{Kind: BlockText, Text: "b"},
	}}
	if got := m.PlainText(); got != "a\nb" {
		t.Errorf("PlainText = %q, want %q", got, "a\nb")
	}
}
