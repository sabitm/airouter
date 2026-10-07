package cursor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func mustEncodeAgent(t *testing.T, req *ir.Request) []byte {
	t.Helper()
	body, err := EncodeAgentRequest(req)
	if err != nil {
		t.Fatalf("EncodeAgentRequest: %v", err)
	}
	return body
}

func agentFramePayload(t *testing.T, frame []byte) []byte {
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

// decodePath walks tagged length-delimited fields by number, returning the
// nested bytes of the last field in the path.
func decodePath(t *testing.T, b []byte, path ...int) []byte {
	t.Helper()
	cur := b
	for _, want := range path {
		m, err := decodeMessage(cur)
		if err != nil {
			t.Fatalf("decodeMessage: %v", err)
		}
		f, ok := m[want]
		if !ok || len(f) == 0 {
			t.Fatalf("field %d not found in % x", want, cur)
		}
		cur = f[0].value
	}
	return cur
}

func TestEncodeAgentRequestEmptyModelErrors(t *testing.T) {
	if _, err := EncodeAgentRequest(&ir.Request{Messages: []ir.Message{{Role: ir.RoleUser}}}); err == nil {
		t.Fatal("expected empty model error")
	}
}

func TestEncodeAgentRequestEmptyMessagesErrors(t *testing.T) {
	if _, err := EncodeAgentRequest(&ir.Request{Model: "default"}); err == nil {
		t.Fatal("expected empty messages error")
	}
}

func TestEncodeAgentRequestEnvelope(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
	})
	payload := agentFramePayload(t, body)
	// AgentClientMessage{1: AgentRunRequest{...}}
	run := decodePath(t, payload, acmRunRequest)

	// conversation_state (1) present and empty (fresh session per request).
	m, err := decodeMessage(run)
	if err != nil {
		t.Fatal(err)
	}
	if cs, ok := m[runConversationState]; !ok || len(cs[0].value) != 0 {
		t.Errorf("conversation_state = %+v, want present and empty", cs)
	}

	// action (2) -> user_message_action (1) -> user_message (1)
	um := decodePath(t, run, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	if text != "hi" {
		t.Errorf("user text = %q, want hi", text)
	}
	if id, _ := stringField(umMsg, umMessageID); id == "" {
		t.Error("message id empty")
	}
	if sel, ok := umMsg[umSelectedContext]; !ok || len(sel) == 0 || len(sel[0].value) != 0 {
		t.Errorf("selected_context = %+v, want present and empty", sel)
	}
	if v, ok := varintField(umMsg, umMode); !ok || v != umModeAgent {
		t.Errorf("mode = %d ok=%v, want 1", v, ok)
	}

	// model_details (3): model id repeated on fields 1, 3, and 4.
	md := decodePath(t, run, runModelDetails)
	mdMsg, _ := decodeMessage(md)
	for _, num := range []int{mdModelID, mdModelIDAlt, mdDisplay} {
		if got, ok := stringField(mdMsg, num); !ok || got != "default" {
			t.Errorf("model_details field %d = %q ok=%v, want default", num, got, ok)
		}
	}

	// requested_model (9): id + built_in_model=true.
	rm := decodePath(t, run, runRequestedModel)
	rmMsg, _ := decodeMessage(rm)
	if got, _ := stringField(rmMsg, rmModelID); got != "default" {
		t.Errorf("model = %q, want default", got)
	}
	if v, ok := varintField(rmMsg, rmBuiltInModel); !ok || v != 1 {
		t.Errorf("built_in_model = %d ok=%v, want 1 true", v, ok)
	}

	// custom_system_prompt (8) must NOT appear: the server rejects it.
	if _, ok := m[runCustomSystem]; ok {
		t.Error("custom_system_prompt present; server rejects it")
	}
}

func TestEncodeAgentRequestSystemPromptFoldedIntoUserText(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model:  "default",
		System: "be brief",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
	})
	um := decodePath(t, agentFramePayload(t, body), acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	if !strings.HasPrefix(text, "[System Instructions]\nbe brief") {
		t.Errorf("user text = %q, want system-prompt prefix", text)
	}
	if !strings.HasSuffix(text, "hi") {
		t.Errorf("user text = %q, want trailing user text", text)
	}
}

func TestEncodeAgentRequestFoldsMCPAvailabilityNote(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
		Tools: []ir.Tool{{Name: "bash"}, {Name: "websearch"}},
	})
	um := decodePath(t, agentFramePayload(t, body), acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	if !strings.Contains(text, "bash") || !strings.Contains(text, "websearch") {
		t.Errorf("user text = %q, want declared tool names", text)
	}
	if !strings.Contains(text, "MCP tools") {
		t.Errorf("user text = %q, want MCP availability note", text)
	}
}

func TestMCPAvailabilityNoteUsesRequestTools(t *testing.T) {
	note := MCPAvailabilityNote([]ir.Tool{{Name: "read"}, {Name: "bash"}})
	if !strings.Contains(note, "read, bash") || !strings.Contains(note, "MCP tools") {
		t.Errorf("note = %q", note)
	}
	if MCPAvailabilityNote(nil) != "" {
		t.Error("empty tools should produce no note")
	}
}

func TestEncodeAgentRequestStructuredHistory(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "first"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "second"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "third"}}},
		},
	})
	payload := agentFramePayload(t, body)
	um := decodePath(t, payload, acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	if text != "third" {
		t.Errorf("current user text = %q, want third (history must not be folded in)", text)
	}
	for _, marker := range []string{"[Conversation History]", "User: first", "Assistant: second", "[Current Message]"} {
		if strings.Contains(text, marker) {
			t.Errorf("user text %q still contains transcript marker %q", text, marker)
		}
	}

	hist := decodePath(t, payload, acmRunRequest, runAction, convUserMessageAction, umaConversationHistory)
	hm, err := decodeMessage(hist)
	if err != nil {
		t.Fatal(err)
	}
	msgs := hm[chMessages]
	if len(msgs) != 2 {
		t.Fatalf("history messages = %d, want 2", len(msgs))
	}
	user, _ := decodeMessage(msgs[0].value)
	userBody, _ := decodeMessage(user[chmUser][0].value)
	userPart, _ := decodeMessage(userBody[chuContent][0].value)
	userText, _ := decodeMessage(userPart[hcText][0].value)
	if got, _ := stringField(userText, tpText); got != "first" {
		t.Errorf("history user text = %q, want first", got)
	}
	asst, _ := decodeMessage(msgs[1].value)
	asstBody, _ := decodeMessage(asst[chmAssistant][0].value)
	asstPart, _ := decodeMessage(asstBody[chaContent][0].value)
	asstText, _ := decodeMessage(asstPart[hcText][0].value)
	if got, _ := stringField(asstText, tpText); got != "second" {
		t.Errorf("history assistant text = %q, want second", got)
	}
}

func TestEncodeAgentRequestToolResultInCurrentMessage(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{
				Type: ir.BlockToolUse, ToolID: "c1", ToolName: "get_weather",
				ToolInput: json.RawMessage(`{"city":"Tokyo"}`),
			}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{
				Type: ir.BlockToolResult, ToolUseID: "c1",
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "18C cloudy"}},
			}}},
		},
	})
	payload := agentFramePayload(t, body)
	um := decodePath(t, payload, acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	if !strings.Contains(text, "Tool result (get_weather): 18C cloudy") {
		t.Errorf("current user text %q missing tool result label", text)
	}
	if strings.Contains(text, "Assistant (tool call)") || strings.Contains(text, "[Conversation History]") {
		t.Errorf("current user text %q still folds prior turns", text)
	}

	hist := decodePath(t, payload, acmRunRequest, runAction, convUserMessageAction, umaConversationHistory)
	hm, err := decodeMessage(hist)
	if err != nil {
		t.Fatal(err)
	}
	msgs := hm[chMessages]
	if len(msgs) != 2 {
		t.Fatalf("history messages = %d, want user text + assistant tool call", len(msgs))
	}
	asst, _ := decodeMessage(msgs[1].value)
	asstBody, _ := decodeMessage(asst[chmAssistant][0].value)
	part, _ := decodeMessage(asstBody[chaContent][0].value)
	call, _ := decodeMessage(part[hcToolCall][0].value)
	if got, _ := stringField(call, chtcID); got != "c1" {
		t.Errorf("tool_call id = %q, want c1", got)
	}
	if got, _ := stringField(call, chtcName); got != "get_weather" {
		t.Errorf("tool_call name = %q", got)
	}
	if got, _ := stringField(call, chtcArgsJSON); got != `{"city":"Tokyo"}` {
		t.Errorf("tool_call args = %q", got)
	}
}

func TestEncodeAgentRequestMCPTools(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
		Tools: []ir.Tool{{
			Name:        "get_weather",
			Description: "weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	})
	run := decodePath(t, agentFramePayload(t, body), acmRunRequest)
	m, err := decodeMessage(run)
	if err != nil {
		t.Fatal(err)
	}
	toolsRaw, ok := m[runMCPTools]
	if !ok || len(toolsRaw) == 0 {
		t.Fatal("mcp_tools missing")
	}
	tools, _ := decodeMessage(toolsRaw[0].value)
	defs := tools[mcpDefsName]
	if len(defs) != 1 {
		t.Fatalf("tool defs = %d, want 1", len(defs))
	}
	def, _ := decodeMessage(defs[0].value)
	if got, _ := stringField(def, mcpDefName); got != "get_weather" {
		t.Errorf("tool name = %q", got)
	}
	// provider_identifier and tool_name must be set on every definition:
	// the deployed provider rejects requests with 2+ MCP tools when the
	// definitions carry no provider identity (verified live; single tool
	// slips through).
	if got, _ := stringField(def, mcpDefProviderID); got == "" {
		t.Error("provider_identifier empty; multi-tool requests fail provider-side")
	}
	if got, _ := stringField(def, mcpDefToolName); got != "get_weather" {
		t.Errorf("tool_name = %q, want get_weather", got)
	}
	if got, _ := stringField(def, mcpDefDescription); got != "weather" {
		t.Errorf("tool description = %q", got)
	}
	schema, _ := stringField(def, mcpDefInputSchemaJSON)
	if !strings.Contains(schema, `"city"`) {
		t.Errorf("input schema json = %q, want city property", schema)
	}
	valueRaw, ok := def[mcpDefInputSchemaValue]
	if !ok || len(valueRaw) == 0 {
		t.Fatal("input schema Value (field 3) missing")
	}
	got := protoValueToGo(valueRaw[0].value)
	var want any
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("schema Value = %s, want %s", gb, wb)
	}
}

func TestEncodeAgentRequestHistoryToolCallAndResult(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "checking"},
				{Type: ir.BlockToolUse, ToolID: "c1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"Tokyo"}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{
				Type: ir.BlockToolResult, ToolUseID: "c1", IsError: true,
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "timeout"}},
			}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "try again"}}},
		},
	})
	payload := agentFramePayload(t, body)
	um := decodePath(t, payload, acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	if text, _ := stringField(umMsg, umText); text != "try again" {
		t.Errorf("current text = %q, want try again", text)
	}
	hist := decodePath(t, payload, acmRunRequest, runAction, convUserMessageAction, umaConversationHistory)
	hm, err := decodeMessage(hist)
	if err != nil {
		t.Fatal(err)
	}
	msgs := hm[chMessages]
	if len(msgs) != 3 {
		t.Fatalf("history messages = %d, want user + assistant + tool", len(msgs))
	}

	asst, _ := decodeMessage(msgs[1].value)
	asstBody, _ := decodeMessage(asst[chmAssistant][0].value)
	parts := asstBody[chaContent]
	if len(parts) != 2 {
		t.Fatalf("assistant content parts = %d, want text + tool_call", len(parts))
	}
	textPart, _ := decodeMessage(parts[0].value)
	if _, ok := textPart[hcText]; !ok {
		t.Fatal("assistant part 0 is not text")
	}
	callPart, _ := decodeMessage(parts[1].value)
	call, _ := decodeMessage(callPart[hcToolCall][0].value)
	if got, _ := stringField(call, chtcName); got != "get_weather" {
		t.Errorf("tool_call name = %q", got)
	}
	if got, _ := stringField(call, chtcArgsJSON); got != `{"city":"Tokyo"}` {
		t.Errorf("args_json = %q", got)
	}

	toolMsg, _ := decodeMessage(msgs[2].value)
	if _, ok := toolMsg[chmTool]; !ok {
		t.Fatalf("history[2] fields = %v, want tool", fieldNums(toolMsg))
	}
	tool, _ := decodeMessage(toolMsg[chmTool][0].value)
	if got, _ := stringField(tool, chtCallID); got != "c1" {
		t.Errorf("tool_call_id = %q", got)
	}
	if got, _ := stringField(tool, chtName); got != "get_weather" {
		t.Errorf("tool_name = %q, want name recovered from prior tool_use", got)
	}
	content, _ := decodeMessage(tool[chtContent][0].value)
	if got, _ := stringField(content, chtcText); got != "timeout" {
		t.Errorf("tool content = %q", got)
	}
	if v, ok := varintField(tool, chtIsError); !ok || v != 1 {
		t.Errorf("is_error = %d ok=%v, want 1", v, ok)
	}
}

func TestEncodeAgentRequestEmptyToolInputBecomesEmptyObject(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{
				Type: ir.BlockToolUse, ToolID: "c0", ToolName: "noop",
			}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{
				Type: ir.BlockToolResult, ToolUseID: "missing",
			}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "next"}}},
		},
	})
	hist := decodePath(t, agentFramePayload(t, body), acmRunRequest, runAction, convUserMessageAction, umaConversationHistory)
	hm, _ := decodeMessage(hist)
	msgs := hm[chMessages]
	if len(msgs) != 2 {
		t.Fatalf("history messages = %d, want assistant + tool", len(msgs))
	}
	asst, _ := decodeMessage(msgs[0].value)
	asstBody, _ := decodeMessage(asst[chmAssistant][0].value)
	part, _ := decodeMessage(asstBody[chaContent][0].value)
	call, _ := decodeMessage(part[hcToolCall][0].value)
	if got, _ := stringField(call, chtcArgsJSON); got != "{}" {
		t.Errorf("empty ToolInput args_json = %q, want {}", got)
	}
	toolMsg, _ := decodeMessage(msgs[1].value)
	tool, _ := decodeMessage(toolMsg[chmTool][0].value)
	if got, _ := stringField(tool, chtName); got != "tool" {
		t.Errorf("unknown tool name = %q, want tool", got)
	}
	content, _ := decodeMessage(tool[chtContent][0].value)
	if got, _ := stringField(content, chtcText); got != "[empty tool result]" {
		t.Errorf("empty tool result = %q", got)
	}
}

func TestEncodeAgentRequestOmitsEmptyHistory(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
	})
	action := decodePath(t, agentFramePayload(t, body), acmRunRequest, runAction, convUserMessageAction)
	m, _ := decodeMessage(action)
	if _, ok := m[umaConversationHistory]; ok {
		t.Error("conversation_history present on a single-turn request")
	}
}

func TestEncodeAgentRequestEmptySchemaValue(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
		Tools: []ir.Tool{{Name: "noop"}},
	})
	run := decodePath(t, agentFramePayload(t, body), acmRunRequest)
	m, _ := decodeMessage(run)
	tools, _ := decodeMessage(m[runMCPTools][0].value)
	def, _ := decodeMessage(tools[mcpDefsName][0].value)
	schema, _ := stringField(def, mcpDefInputSchemaJSON)
	if schema != `{"type":"object","properties":{}}` {
		t.Errorf("default schema json = %q", schema)
	}
	if _, ok := def[mcpDefInputSchemaValue]; !ok {
		t.Fatal("default schema Value missing")
	}
}

func fieldNums(m map[int][]field) []int {
	var nums []int
	for n := range m {
		nums = append(nums, n)
	}
	return nums
}

func TestEncodeAgentRequestNoToolsOmitsMCPTools(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
	})
	run := decodePath(t, agentFramePayload(t, body), acmRunRequest)
	m, _ := decodeMessage(run)
	if _, ok := m[runMCPTools]; ok {
		t.Error("mcp_tools present without tools")
	}
}

func TestEncodeAgentRequestStripsCursorPrefix(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "cursor/default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		},
	})
	rm := decodePath(t, agentFramePayload(t, body), acmRunRequest, runRequestedModel)
	rmMsg, _ := decodeMessage(rm)
	if got, _ := stringField(rmMsg, rmModelID); got != "default" {
		t.Errorf("model = %q, want cursor/ prefix stripped", got)
	}
}
