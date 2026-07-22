package cursor

import (
	"bytes"
	"encoding/json"
	"testing"

	"airouter/internal/proxy/ir"
)

func mustEncode(t *testing.T, req *ir.Request) []byte {
	t.Helper()
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	return body
}

func framePayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	flags, payload, err := readFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if flags != flagNone {
		t.Fatalf("request frame flags = %d, want 0 (no compress)", flags)
	}
	return payload
}

func TestEncodeRequestEmptyModelErrors(t *testing.T) {
	_, err := EncodeRequest(&ir.Request{
		Model:    "",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("empty model should error")
	}
}

func TestEncodeRequestEmptyMessagesErrors(t *testing.T) {
	_, err := EncodeRequest(&ir.Request{Model: "default", Messages: nil})
	if err == nil {
		t.Fatal("empty messages should error")
	}
}

func TestEncodeRequestModelNameInBody(t *testing.T) {
	body := mustEncode(t, &ir.Request{
		Model:    "gpt-5.2",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	payload := framePayload(t, body)
	top, err := decodeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	reqBytes, ok := stringField(top, topRequest)
	if !ok {
		t.Fatal("missing top.request field 1")
	}
	req, err := decodeMessage([]byte(reqBytes))
	if err != nil {
		t.Fatal(err)
	}
	modelBytes, ok := stringField(req, reqModel)
	if !ok {
		t.Fatal("missing model field 5")
	}
	modelMsg, err := decodeMessage([]byte(modelBytes))
	if err != nil {
		t.Fatal(err)
	}
	name, ok := stringField(modelMsg, modelName)
	if !ok || name != "gpt-5.2" {
		t.Errorf("model name = %q ok=%v", name, ok)
	}
}

func TestEncodeRequestStripsCursorPrefix(t *testing.T) {
	body := mustEncode(t, &ir.Request{
		Model:    "cursor/gpt-5.2",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	payload := framePayload(t, body)
	top, _ := decodeMessage(payload)
	reqBytes, _ := stringField(top, topRequest)
	req, _ := decodeMessage([]byte(reqBytes))
	modelBytes, _ := stringField(req, reqModel)
	modelMsg, _ := decodeMessage([]byte(modelBytes))
	if name, _ := stringField(modelMsg, modelName); name != "gpt-5.2" {
		t.Errorf("model name = %q, want gpt-5.2 (prefix stripped)", name)
	}
}

func TestEncodeRequestToolsUseMcpCustom(t *testing.T) {
	body := mustEncode(t, &ir.Request{
		Model:    "default",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "do it"}}}},
		Tools:    []ir.Tool{{Name: "Write", Description: "write a file", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	payload := framePayload(t, body)
	top, _ := decodeMessage(payload)
	reqBytes, _ := stringField(top, topRequest)
	req, _ := decodeMessage([]byte(reqBytes))
	tools, ok := req[reqMCPTools]
	if !ok || len(tools) != 1 {
		t.Fatalf("mcp_tools = %d entries, want 1", len(tools))
	}
	toolMsg, _ := decodeMessage(tools[0].value)
	name, ok := stringField(toolMsg, mcpToolName)
	if !ok || name != "mcp_custom_Write" {
		t.Errorf("tool name = %q, want mcp_custom_Write", name)
	}
	server, _ := stringField(toolMsg, mcpToolServer)
	if server != "custom" {
		t.Errorf("tool server = %q, want custom", server)
	}
	// is_agentic + unified_mode should reflect agent mode.
	if v, _ := varintField(req, reqIsAgentic); v != 1 {
		t.Errorf("is_agentic = %d, want 1", v)
	}
	if v, _ := varintField(req, reqUnifiedMode); v != unifiedModeAgent {
		t.Errorf("unified_mode = %d, want %d", v, unifiedModeAgent)
	}
	if v, _ := varintField(req, reqShouldDisableTools); v != 0 {
		t.Errorf("should_disable_tools = %d, want 0", v)
	}
}

func TestEncodeRequestNoToolsIsChatMode(t *testing.T) {
	body := mustEncode(t, &ir.Request{
		Model:    "default",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	payload := framePayload(t, body)
	top, _ := decodeMessage(payload)
	reqBytes, _ := stringField(top, topRequest)
	req, _ := decodeMessage([]byte(reqBytes))
	if v, _ := varintField(req, reqIsAgentic); v != 0 {
		t.Errorf("is_agentic = %d, want 0", v)
	}
	if v, _ := varintField(req, reqUnifiedMode); v != unifiedModeChat {
		t.Errorf("unified_mode = %d, want chat", v)
	}
	if v, _ := varintField(req, reqShouldDisableTools); v != 1 {
		t.Errorf("should_disable_tools = %d, want 1", v)
	}
}

func TestEncodeRequestSystemPromptPrefixedToFirstUser(t *testing.T) {
	body := mustEncode(t, &ir.Request{
		Model:    "default",
		System:   "be concise",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	payload := framePayload(t, body)
	top, _ := decodeMessage(payload)
	reqBytes, _ := stringField(top, topRequest)
	req, _ := decodeMessage([]byte(reqBytes))
	msgs, ok := req[reqMessages]
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	msg, _ := decodeMessage(msgs[0].value)
	content, _ := stringField(msg, msgContent)
	if !startsWith(content, "[System Instructions]\nbe concise") {
		t.Errorf("content = %q, want system prefix", content)
	}
}

func TestEncodeRequestToolResultAsXMLNotProtobuf(t *testing.T) {
	body := mustEncode(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockToolUse, ToolID: "tc1", ToolName: "Write"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockToolResult, ToolUseID: "tc1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "ok"}}}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "now what"}}},
		},
	})
	payload := framePayload(t, body)
	top, _ := decodeMessage(payload)
	reqBytes, _ := stringField(top, topRequest)
	req, _ := decodeMessage([]byte(reqBytes))
	msgs := req[reqMessages]
	// The tool_result becomes XML text inside the second user message's content.
	// It must contain <tool_result> and NOT a protobuf field-18 occurrence we can
	// detect; we assert the XML marker is present in the content bytes.
	found := false
	for _, mf := range msgs {
		msg, _ := decodeMessage(mf.value)
		content, _ := stringField(msg, msgContent)
		if bytes.Contains([]byte(content), []byte("<tool_result>")) {
			found = true
			if !bytes.Contains([]byte(content), []byte("<tool_name>Write</tool_name>")) {
				t.Errorf("tool result XML missing tool name: %q", content)
			}
		}
	}
	if !found {
		t.Error("no <tool_result> XML found in any message content")
	}
}

func TestFormatToolName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Write", "mcp_custom_Write"},
		{"", "mcp_custom_tool"},
		{"mcp__server__tool", "mcp_server_tool"},
		{"mcp__s__t", "mcp_s_t"},
		{"mcp__only", "mcp_custom_only"},
		{"mcp_foo", "mcp_foo"},
		{"mcp_custom_bar", "mcp_custom_bar"},
	}
	for _, c := range cases {
		if got := formatToolName(c.in); got != c.want {
			t.Errorf("formatToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
