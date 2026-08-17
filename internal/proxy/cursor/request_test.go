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

func TestEncodeAgentRequestHistoryFoldedAsTranscript(t *testing.T) {
	body := mustEncodeAgent(t, &ir.Request{
		Model: "default",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "first"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "second"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "third"}}},
		},
	})
	um := decodePath(t, agentFramePayload(t, body), acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	for _, want := range []string{"[Conversation History]", "User: first", "Assistant: second", "[Current Message]", "third"} {
		if !strings.Contains(text, want) {
			t.Errorf("user text %q missing %q", text, want)
		}
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
	um := decodePath(t, agentFramePayload(t, body), acmRunRequest, runAction, convUserMessageAction, umaUserMessage)
	umMsg, _ := decodeMessage(um)
	text, _ := stringField(umMsg, umText)
	for _, want := range []string{"Assistant (tool call): get_weather", "Tool result (get_weather): 18C cloudy"} {
		if !strings.Contains(text, want) {
			t.Errorf("user text %q missing %q", text, want)
		}
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
