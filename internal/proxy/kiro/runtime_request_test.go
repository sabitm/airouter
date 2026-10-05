package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func runtimeRequest(system string, messages []ir.Message, tools []ir.Tool) *ir.Request {
	temp := 0.2
	return &ir.Request{
		Model:       "runtime-model",
		System:      system,
		Temperature: &temp,
		Messages:    messages,
		Tools:       tools,
	}
}

func decodeRuntime(t *testing.T, body []byte) (map[string]any, rtRequest) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal object: %v\n%s", err, body)
	}
	var got rtRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal runtime: %v\n%s", err, body)
	}
	return raw, got
}

func assertRuntimeEnvelope(t *testing.T, raw map[string]any) {
	t.Helper()
	allowed := map[string]bool{
		"conversationState": true, "profileArn": true, "agentMode": true,
		"additionalModelRequestFields": true, "systemPrompt": true,
	}
	for key := range raw {
		if !allowed[key] {
			t.Errorf("unexpected Runtime key %q", key)
		}
	}
	if _, ok := raw["inferenceConfig"]; ok {
		t.Error("inferenceConfig must stay absent")
	}
	if _, ok := raw["systemPrompt"]; ok {
		t.Error("systemPrompt is not emitted while system_field_injection stays off")
	}
	if _, ok := raw["additionalModelRequestFields"]; ok {
		t.Error("unsupported parameters must not be invented")
	}
	state, ok := raw["conversationState"].(map[string]any)
	if !ok {
		t.Fatalf("conversationState = %#v", raw["conversationState"])
	}
	for _, key := range []string{"agentContinuationId", "agentTaskType"} {
		if _, ok := state[key]; ok {
			t.Errorf("%s fabricated", key)
		}
	}
}

func assertUserIdentity(t *testing.T, msg *cwUserInputMessage, model string) {
	t.Helper()
	if msg == nil || msg.ModelID != model || msg.Origin != runtimeOrigin {
		t.Fatalf("user identity = %+v", msg)
	}
}

func TestEncodeRuntimeRequestEnvelopeAndHistoryIdentity(t *testing.T) {
	req := runtimeRequest("be brief", []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hello"}}},
		{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "hi"},
			{Type: ir.BlockReasoning, Text: "unsigned thought"},
		}},
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "again"}}},
	}, nil)
	before, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeRuntimeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("encode mutated IR")
	}
	raw, got := decodeRuntime(t, body)
	assertRuntimeEnvelope(t, raw)
	if got.ProfileArn != "" || got.AgentMode != "" {
		t.Fatalf("optional profile/mode present: %+v", got)
	}
	state := got.ConversationState
	if state.ChatTriggerType != "MANUAL" || state.ConversationID == "" || state.RootConversationID != state.ConversationID {
		t.Fatalf("state identity = %+v", state)
	}
	if len(state.History) != 2 {
		t.Fatalf("history = %+v", state.History)
	}
	first := state.History[0].UserInputMessage
	assertUserIdentity(t, first, req.Model)
	if first.Content != "be brief\n\nhello" {
		t.Fatalf("system fold = %q", first.Content)
	}
	assistant := state.History[1].AssistantResponseMessage
	if assistant == nil || assistant.Content != "hi" || strings.Contains(string(body), "unsigned thought") || strings.Contains(string(body), "reasoningContent") {
		t.Fatalf("assistant leaked reasoning: %s", body)
	}
	cur := state.CurrentMessage.UserInputMessage
	assertUserIdentity(t, cur, req.Model)
	if cur.Content != "again" {
		t.Fatalf("current = %+v", cur)
	}
}

func TestEncodeRuntimeRequestSystemContexts(t *testing.T) {
	tool := ir.Tool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}
	cases := []struct {
		name     string
		system   string
		messages []ir.Message
		want     string
	}{
		{name: "system only", system: "rules", want: "rules"},
		{name: "assistant only", system: "", messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "old"}}}}},
		{name: "empty", system: "   ", messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: ""}}}}},
		{
			name:   "tool context",
			system: "rules",
			messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "find"}}},
				{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockToolUse, ToolID: "c1", ToolName: "lookup", ToolInput: json.RawMessage(`{"q":"a"}`)}}},
				{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockToolResult, ToolUseID: "c1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "found"}}}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := []ir.Tool(nil)
			if tc.name == "tool context" {
				tools = []ir.Tool{tool}
			}
			body, err := EncodeRuntimeRequest(runtimeRequest(tc.system, tc.messages, tools))
			if err != nil {
				t.Fatal(err)
			}
			raw, got := decodeRuntime(t, body)
			assertRuntimeEnvelope(t, raw)
			if strings.Count(string(body), "rules") > 1 {
				t.Fatalf("system repeated: %s", body)
			}
			cur := got.ConversationState.CurrentMessage.UserInputMessage
			assertUserIdentity(t, cur, "runtime-model")
			for _, item := range got.ConversationState.History {
				if item.UserInputMessage != nil {
					assertUserIdentity(t, item.UserInputMessage, "runtime-model")
				}
				if item.AssistantResponseMessage != nil {
					rawAssistant, err := json.Marshal(item.AssistantResponseMessage)
					if err != nil {
						t.Fatal(err)
					}
					for _, forbidden := range []string{`"modelId"`, `"origin"`, `"reasoningContent"`} {
						if strings.Contains(string(rawAssistant), forbidden) {
							t.Fatalf("assistant contains %s: %s", forbidden, rawAssistant)
						}
					}
				}
			}
			if tc.want != "" && !strings.Contains(cur.Content, tc.want) && !historyContains(got, tc.want) {
				t.Fatalf("system missing: %s", body)
			}
			if tc.name == "tool context" {
				if len(got.ConversationState.History) == 0 || got.ConversationState.History[1].AssistantResponseMessage == nil {
					t.Fatalf("tool history missing: %s", body)
				}
				call := got.ConversationState.History[1].AssistantResponseMessage.ToolUses[0]
				result := cur.UserInputMessageContext.ToolResults[0]
				if call.Name != "lookup" || call.ToolUseID != result.ToolUseID || result.Content[0].Text != "found" {
					t.Fatalf("tool symmetry call=%+v result=%+v", call, result)
				}
				if len(cur.UserInputMessageContext.Tools) != 1 || cur.UserInputMessageContext.Tools[0].ToolSpecification.Name != "lookup" {
					t.Fatalf("catalog = %+v", cur.UserInputMessageContext)
				}
			}
		})
	}
}

func historyContains(got rtRequest, text string) bool {
	for _, item := range got.ConversationState.History {
		if item.UserInputMessage != nil && strings.Contains(item.UserInputMessage.Content, text) {
			return true
		}
	}
	return false
}

func TestEncodeRuntimeRequestDoesNotMutateAndRejectsUnknownTool(t *testing.T) {
	req := runtimeRequest("", []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "go"}}},
		{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockToolUse, ToolID: "c1", ToolName: "missing", ToolInput: json.RawMessage(`{}`)}}},
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockToolResult, ToolUseID: "c1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "no"}}}}},
	}, []ir.Tool{{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}})
	before := req.Messages[1].Content[0].ToolName
	body, err := EncodeRuntimeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if req.Messages[1].Content[0].ToolName != before {
		t.Fatal("encode mutated tool name")
	}
	if strings.Contains(string(body), `"toolUseId":"c1"`) {
		t.Fatalf("unknown tool forwarded: %s", body)
	}
	raw, _ := decodeRuntime(t, body)
	assertRuntimeEnvelope(t, raw)
}

func TestInjectRuntimeAgentMode(t *testing.T) {
	body, err := EncodeRuntimeRequest(runtimeRequest("", []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(InjectRuntimeAgentMode(body, "")), "agentMode") || strings.Contains(string(InjectRuntimeAgentMode(body, "custom")), "agentMode") {
		t.Fatal("unset or unknown mode injected")
	}
	out := InjectRuntimeAgentMode(body, "spec")
	raw, got := decodeRuntime(t, out)
	assertRuntimeEnvelope(t, raw)
	if got.AgentMode != "spec" {
		t.Fatalf("mode = %q", got.AgentMode)
	}
}

func TestLegacyEncodeRetainsInferenceConfig(t *testing.T) {
	temp := 0.4
	req := &ir.Request{
		Model: "legacy-model", System: "rules", Temperature: &temp,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeReq(t, body)
	if got.InferenceConfig.MaxTokens != DefaultMaxTokens || got.InferenceConfig.Temperature == nil || *got.InferenceConfig.Temperature != temp {
		t.Fatalf("legacy inference = %+v", got.InferenceConfig)
	}
	if !strings.Contains(got.ConversationState.CurrentMessage.UserInputMessage.Content, "rules") {
		t.Fatalf("legacy system fold lost: %+v", got.ConversationState.CurrentMessage)
	}
	if strings.Contains(string(body), "rootConversationId") || strings.Contains(string(body), "agentMode") {
		t.Fatalf("legacy gained Runtime fields: %s", body)
	}
}
