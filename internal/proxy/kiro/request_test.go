package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func decodeReq(t *testing.T, body []byte) cwRequest {
	t.Helper()
	var got cwRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	return got
}

func TestEncodeRequestSystemFoldAndHistorySplit(t *testing.T) {
	req := &ir.Request{
		Model:  "claude-sonnet-4.5",
		System: "be brief",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hello"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi there"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "again"}}},
		},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)

	// System folds into the first user turn (in history since it is not last).
	if len(got.ConversationState.History) != 2 {
		t.Fatalf("history len = %d\n%s", len(got.ConversationState.History), body)
	}
	first := got.ConversationState.History[0].UserInputMessage
	if first == nil || first.Content != "be brief\n\nhello" {
		t.Errorf("first user content = %q", contentOf(first))
	}
	if a := got.ConversationState.History[1].AssistantResponseMessage; a == nil || a.Content != "hi there" {
		t.Errorf("assistant history = %+v", got.ConversationState.History[1])
	}
	// Last user turn is current, carries modelId + origin.
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur == nil || cur.Content != "again" {
		t.Fatalf("current = %+v", cur)
	}
	if cur.ModelID != "claude-sonnet-4.5" || cur.Origin != "AI_EDITOR" {
		t.Errorf("current modelId/origin = %q/%q", cur.ModelID, cur.Origin)
	}
	if got.InferenceConfig.MaxTokens != DefaultMaxTokens {
		t.Errorf("maxTokens = %d, want %d", got.InferenceConfig.MaxTokens, DefaultMaxTokens)
	}
}

func TestEncodeRequestToolsOnCurrentMessage(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4.5",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
		},
		Tools: []ir.Tool{{Name: "get_weather", Description: "w", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) != 1 {
		t.Fatalf("tools not attached to current message: %s", body)
	}
	if cur.UserInputMessageContext.Tools[0].ToolSpecification.Name != "get_weather" {
		t.Errorf("tool name = %q", cur.UserInputMessageContext.Tools[0].ToolSpecification.Name)
	}
}

func TestEncodeRequestToolResultAndToolUse(t *testing.T) {
	req := &ir.Request{
		Model: "claude-sonnet-4.5",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"NYC"}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "call_1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "sunny"}}},
				{Type: ir.BlockText, Text: "thanks"},
			}},
		},
		// Tools present so the guards do not flatten the structured tool blocks.
		Tools: []ir.Tool{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)

	// Assistant tool_use lands in history.
	var sawToolUse bool
	for _, h := range got.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			for _, tu := range h.AssistantResponseMessage.ToolUses {
				if tu.ToolUseID == "call_1" && tu.Name == "get_weather" {
					sawToolUse = true
				}
			}
		}
	}
	if !sawToolUse {
		t.Errorf("tool_use not in history: %s", body)
	}
	// Tool result lands on the current (last user) message context.
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("toolResults missing: %s", body)
	}
	tr := cur.UserInputMessageContext.ToolResults[0]
	if tr.ToolUseID != "call_1" || tr.Status != "success" || len(tr.Content) != 1 || tr.Content[0].Text != "sunny" {
		t.Errorf("toolResult = %+v", tr)
	}
}

func TestInjectProfileArn(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	// Empty arn leaves the field absent.
	if decodeReq(t, InjectProfileArn(body, "")).ProfileArn != "" {
		t.Errorf("empty arn should stay absent")
	}
	arn := "arn:aws:codewhisperer:us-east-1:123:profile/ABC"
	if got := decodeReq(t, InjectProfileArn(body, arn)).ProfileArn; got != arn {
		t.Errorf("profileArn = %q", got)
	}
}

func TestFlattenToolInteractionsWhenNoTools(t *testing.T) {
	// A follow-up with tool blocks but no tools declaration must be flattened to
	// text to avoid Kiro's HTTP 400.
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"NYC"}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "call_1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "sunny"}}},
				{Type: ir.BlockText, Text: "and tomorrow?"},
			}},
		},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)
	// No structured tool state should survive anywhere.
	for _, h := range got.ConversationState.History {
		if h.AssistantResponseMessage != nil && len(h.AssistantResponseMessage.ToolUses) != 0 {
			t.Errorf("tool_use survived flattening: %s", body)
		}
		if h.UserInputMessage != nil && h.UserInputMessage.UserInputMessageContext != nil &&
			len(h.UserInputMessage.UserInputMessageContext.ToolResults) != 0 {
			t.Errorf("toolResult survived flattening: %s", body)
		}
	}
	cur := got.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Errorf("current toolResults survived flattening: %s", body)
	}
	// The result content should be salvaged into text.
	if !strings.Contains(cur.Content, "Tool result") || !strings.Contains(cur.Content, "sunny") {
		t.Errorf("flattened result text missing: %q", cur.Content)
	}
}

func TestReconcileOrphanedToolResults(t *testing.T) {
	// A tool_result whose tool_use was dropped is salvaged to text even when tools
	// are declared (structured orphan would 400).
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "gone", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "orphan"}}},
				{Type: ir.BlockText, Text: "continue"},
			}},
		},
		Tools: []ir.Tool{{Name: "x", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	cur := decodeReq(t, body).ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
		t.Errorf("orphan toolResult should have been salvaged: %s", body)
	}
	if !strings.Contains(cur.Content, "orphan") {
		t.Errorf("salvaged orphan text missing: %q", cur.Content)
	}
}

func contentOf(m *cwUserInputMessage) string {
	if m == nil {
		return ""
	}
	return m.Content
}
