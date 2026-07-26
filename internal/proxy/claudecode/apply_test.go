package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
}

func TestCloakToolsRenamesAndAppendsDecoys(t *testing.T) {
	body := &messagesBody{
		Tools: []wireTool{
			{Name: "search", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "Bash", Description: "mine"}, // native name but client tool: still suffixed
		},
	}
	CloakTools(body)
	names := toolNames(body.Tools)
	if names[0] != "search_ide" {
		t.Errorf("client tool not suffixed: %v", names)
	}
	if names[1] != "Bash_ide" {
		t.Errorf("native-named client tool not suffixed: %v", names)
	}
	// Decoys appended after, unsuffixed native names.
	if len(body.Tools) != 2+len(DecoyTools) {
		t.Fatalf("decoys not appended: %d tools", len(body.Tools))
	}
	if body.Tools[2].Name != "Task" || body.Tools[2].Description != "This tool is currently unavailable." {
		t.Errorf("first decoy wrong: %+v", body.Tools[2])
	}
}

func TestCloakToolsSkipsServerTools(t *testing.T) {
	body := &messagesBody{
		Tools: []wireTool{
			{Type: "web_search_20250305", Name: "web_search"},
			{Name: "client_tool"},
		},
	}
	CloakTools(body)
	if body.Tools[0].Name != "web_search" {
		t.Errorf("server tool must keep exact name, got %q", body.Tools[0].Name)
	}
	if body.Tools[1].Name != "client_tool_ide" {
		t.Errorf("client tool must be suffixed, got %q", body.Tools[1].Name)
	}
}

func TestCloakToolsRewritesHistoryAndChoice(t *testing.T) {
	blocks := []wireBlock{
		{Type: "text", Text: "hi"},
		{Type: "tool_use", ID: "t1", Name: "search", Input: json.RawMessage(`{}`)},
	}
	content, _ := json.Marshal(blocks)
	body := &messagesBody{
		Tools:      []wireTool{{Name: "search"}},
		ToolChoice: &wireToolChoice{Type: "tool", Name: "search"},
		Messages:   []wireMessage{{Role: "assistant", Content: content}},
	}
	CloakTools(body)

	var got []wireBlock
	mustJSON(t, body.Messages[0].Content, &got)
	if got[1].Name != "search_ide" {
		t.Errorf("history tool_use not suffixed: %+v", got[1])
	}
	if body.ToolChoice.Name != "search_ide" {
		t.Errorf("forced tool_choice not rewritten: %q", body.ToolChoice.Name)
	}
}

func TestCloakToolsNoOpOnEmpty(t *testing.T) {
	body := &messagesBody{}
	CloakTools(body)
	if len(body.Tools) != 0 {
		t.Errorf("empty tools must not inject decoys, got %d", len(body.Tools))
	}
}

func TestCloakToolsChoiceNotRewrittenForDecoyName(t *testing.T) {
	// A tool_choice pointing at a native/decoy name that is NOT a client tool
	// must not be rewritten (it was not renamed).
	body := &messagesBody{
		Tools:      []wireTool{{Name: "client"}},
		ToolChoice: &wireToolChoice{Type: "tool", Name: "Bash"}, // Bash is a decoy, not a client tool here
	}
	CloakTools(body)
	if body.ToolChoice.Name != "Bash" {
		t.Errorf("tool_choice on decoy name must not be rewritten, got %q", body.ToolChoice.Name)
	}
}

func TestApplyOAuthCloakingNoOpForAPIKey(t *testing.T) {
	in := []byte(`{"model":"x","messages":[],"max_tokens":10,"tools":[{"name":"t"}]}`)
	out, err := ApplyOAuthCloaking(in, "sk-ant-apikey-xyz", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(in) {
		t.Errorf("apikey token must leave body unchanged")
	}
}

func TestApplyOAuthCloakingInjectsBillingAndMetadata(t *testing.T) {
	in := []byte(`{"model":"x","system":"hello","messages":[],"max_tokens":10}`)
	out, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess-123", "seed-1")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("cloaked body invalid: %v", err)
	}
	// system promoted to [billing, {text: "hello"}]
	var blocks []wireBlock
	mustJSON(t, m.System, &blocks)
	if len(blocks) != 2 || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") {
		t.Fatalf("billing not injected as system[0]: %s", m.System)
	}
	if blocks[1].Text != "hello" {
		t.Errorf("existing system text not preserved: %+v", blocks[1])
	}
	// metadata.user_id set with the session id.
	if m.Metadata == nil || m.Metadata.UserID == "" {
		t.Fatalf("metadata.user_id not set")
	}
	if !strings.Contains(m.Metadata.UserID, "sess-123") {
		t.Errorf("user_id session_id mismatch: %s", m.Metadata.UserID)
	}
}

func TestApplyOAuthCloakingBillingIdempotent(t *testing.T) {
	in := []byte(`{"model":"x","messages":[],"max_tokens":10}`)
	out1, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	// Second pass on the already-cloaked body must not add a second billing block.
	out2, err := ApplyOAuthCloaking(out1, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	_ = json.Unmarshal(out2, &m)
	var blocks []wireBlock
	mustJSON(t, m.System, &blocks)
	if len(blocks) != 1 || !strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") {
		t.Fatalf("idempotent re-cloak should keep a single billing block, got %d blocks", len(blocks))
	}
}

func TestApplyOAuthCloakingCloaksTools(t *testing.T) {
	in := []byte(`{"model":"x","messages":[],"max_tokens":10,"tools":[{"name":"search"}]}`)
	out, err := ApplyOAuthCloaking(in, "sk-ant-oat-token", "sess", "seed")
	if err != nil {
		t.Fatal(err)
	}
	var m messagesBody
	_ = json.Unmarshal(out, &m)
	if len(m.Tools) == 0 || m.Tools[0].Name != "search_ide" {
		t.Fatalf("tools not cloaked in apply: %+v", m.Tools)
	}
	// Decoys present.
	found := false
	for _, tt := range m.Tools {
		if tt.Name == "Bash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("decoys not appended in apply")
	}
}

func TestApplyOAuthCloakingInvalidJSON(t *testing.T) {
	_, err := ApplyOAuthCloaking([]byte(`{not json`), "sk-ant-oat-token", "sess", "seed")
	if err == nil {
		t.Fatal("invalid JSON should error")
	}
}

func toolNames(tools []wireTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
