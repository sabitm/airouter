package anthropic

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
)

func encodeAndDecode(t *testing.T, req *ir.Request) messagesRequest {
	t.Helper()
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	var enc messagesRequest
	if err := json.Unmarshal(out, &enc); err != nil {
		t.Fatalf("encoded body invalid: %v (%s)", err, out)
	}
	return enc
}

func decodeMessageBlocks(t *testing.T, raw json.RawMessage) []anthBlock {
	t.Helper()
	var blocks []anthBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("message content invalid: %v (%s)", err, raw)
	}
	return blocks
}

func TestEncodeRequestClaudeCodeMythos5DisableOmitsThinking(t *testing.T) {
	zero := 0
	req := &ir.Request{
		Model:     "claude-mythos-5",
		MaxTokens: 32,
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled},
		Anthropic: &ir.AnthropicRequest{
			OutputFormat: json.RawMessage(`{"type":"json_schema","schema":{"type":"object","title":"Out"}}`),
			TaskBudget:   &ir.AnthropicTaskBudget{Total: 1000, Remaining: &zero},
		},
	}
	out, err := EncodeRequestClaudeCode(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["thinking"]; ok {
		t.Fatalf("claude-code disable wrote thinking: %s", out)
	}
	var oc map[string]json.RawMessage
	if err := json.Unmarshal(doc["output_config"], &oc); err != nil {
		t.Fatalf("decode output_config: %v; body=%s", err, out)
	}
	if _, ok := oc["effort"]; ok {
		t.Fatalf("claude-code disable wrote effort: %s", out)
	}
	if !strings.Contains(string(oc["format"]), "json_schema") || !strings.Contains(string(oc["task_budget"]), `"remaining":0`) {
		t.Fatalf("output_config siblings lost: %s", out)
	}

	stale := messagesRequest{
		Thinking:     &anthThinking{Type: "adaptive"},
		OutputConfig: &anthOutputConfig{Effort: "high", Format: json.RawMessage(`{"type":"json_object"}`)},
	}
	applyThinking(&stale, req, domain.ProtocolClaudeCode)
	if stale.Thinking != nil || stale.OutputConfig == nil || stale.OutputConfig.Effort != "" {
		t.Fatalf("encoder kept stale controls: %+v", stale)
	}
	if !strings.Contains(string(stale.OutputConfig.Format), "json_object") {
		t.Fatalf("encoder dropped output_config sibling: %+v", stale.OutputConfig)
	}

	generic, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var genericDoc map[string]json.RawMessage
	if err := json.Unmarshal(generic, &genericDoc); err != nil {
		t.Fatal(err)
	}
	var genericThinking anthThinking
	if err := json.Unmarshal(genericDoc["thinking"], &genericThinking); err != nil || genericThinking.Type != "disabled" {
		t.Fatalf("generic mythos disable changed: %s", generic)
	}
}

// TestEncodeRequestMalformedToolInputIsValidationError verifies a non-empty
// invalid tool argument fails the encode with a typed client error instead of
// dropping the assistant turn, and that the error message leaks no payload.
func TestEncodeRequestMalformedToolInputIsValidationError(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleAssistant,
			Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "sure"},
				{Type: ir.BlockToolUse, ToolID: "call_secret_id", ToolName: "tool_secret_name", ToolInput: json.RawMessage(`{"secret":"hunter2",}`)},
			},
		}},
	}
	_, err := EncodeRequest(req)
	if err == nil {
		t.Fatal("want validation error")
	}
	if _, ok := ir.AsRequestValidation(err); !ok {
		t.Fatalf("err = %v, want request validation error", err)
	}
	msg := err.Error()
	for _, leak := range []string{"hunter2", "call_secret_id", "tool_secret_name"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("error %q leaked %q", msg, leak)
		}
	}
	if !strings.Contains(msg, "message 0") || !strings.Contains(msg, "block 1") {
		t.Fatalf("error %q does not identify message and block", msg)
	}
}

// TestEncodeRequestMalformedNestedToolResultInput verifies the typed error is
// preserved through nested tool_result encoding and the position stays
// meaningful.
func TestEncodeRequestMalformedNestedToolResultInput(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "ok"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{
				Type:      ir.BlockToolResult,
				ToolUseID: "call_secret_id",
				ToolResult: []ir.ContentBlock{
					{Type: ir.BlockText, Text: "fine"},
					{Type: ir.BlockToolUse, ToolID: "nested_id", ToolName: "nested_name", ToolInput: json.RawMessage(`[bad`)},
				},
			}}},
		},
	}
	_, err := EncodeRequest(req)
	if err == nil {
		t.Fatal("want validation error")
	}
	if _, ok := ir.AsRequestValidation(err); !ok {
		t.Fatalf("err = %v, want request validation error", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "message 1") || !strings.Contains(msg, "tool_result block 0") {
		t.Fatalf("error %q does not identify nested position", msg)
	}
	for _, leak := range []string{"[bad", "call_secret_id", "nested_id", "nested_name"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("error %q leaked %q", msg, leak)
		}
	}
}

// TestEncodeRequestMalformedToolSchemaIsValidationError verifies malformed
// tool schema syntax is a typed client error and is not treated as schema
// validation (a valid non-object schema still passes).
func TestEncodeRequestMalformedToolSchemaIsValidationError(t *testing.T) {
	req := &ir.Request{
		Model:    "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "tool_secret_name", Parameters: json.RawMessage(`{"type":`)}},
	}
	_, err := EncodeRequest(req)
	if err == nil {
		t.Fatal("want validation error")
	}
	if _, ok := ir.AsRequestValidation(err); !ok {
		t.Fatalf("err = %v, want request validation error", err)
	}
	if strings.Contains(err.Error(), "tool_secret_name") {
		t.Fatalf("error %q leaked tool name", err)
	}
	if !strings.Contains(err.Error(), "tool 0") {
		t.Fatalf("error %q does not identify tool position", err)
	}

	// A syntactically valid non-object schema is not rejected.
	req.Tools = []ir.Tool{{Name: "t", Parameters: json.RawMessage(`["not-an-object"]`)}}
	if _, err := EncodeRequest(req); err != nil {
		t.Fatalf("valid non-object schema rejected: %v", err)
	}
}

// TestEncodeRequestValidHistoryPreserved verifies assistant text, reasoning,
// multiple tool calls, and matching tool results all survive one encode, so a
// failure in one block cannot silently discard its siblings.
func TestEncodeRequestValidHistoryPreserved(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockReasoning, Text: "think"},
				{Type: ir.BlockText, Text: "sure"},
				{Type: ir.BlockToolUse, ToolID: "c1", ToolName: "f1", ToolInput: json.RawMessage(`{"a":1}`)},
				{Type: ir.BlockToolUse, ToolID: "c2", ToolName: "f2", ToolInput: json.RawMessage(`{"b":2}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "c1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r1"}}},
				{Type: ir.BlockToolResult, ToolUseID: "c2", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r2"}}},
			}},
		},
	}
	enc := encodeAndDecode(t, req)
	if len(enc.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(enc.Messages))
	}
	assistant := decodeMessageBlocks(t, enc.Messages[0].Content)
	if len(assistant) != 4 {
		t.Fatalf("assistant blocks = %d, want 4 (%+v)", len(assistant), assistant)
	}
	if assistant[0].Type != "thinking" || thinkingText(assistant[0].Thinking) != "think" {
		t.Fatalf("reasoning = %+v", assistant[0])
	}
	if assistant[1].Type != "text" || assistant[1].Text != "sure" {
		t.Fatalf("text = %+v", assistant[1])
	}
	if assistant[2].Type != "tool_use" || assistant[2].ID != "c1" || assistant[2].Name != "f1" || string(assistant[2].Input) != `{"a":1}` {
		t.Fatalf("tool 0 = %+v", assistant[2])
	}
	if assistant[3].Type != "tool_use" || assistant[3].ID != "c2" || assistant[3].Name != "f2" || string(assistant[3].Input) != `{"b":2}` {
		t.Fatalf("tool 1 = %+v", assistant[3])
	}
	user := decodeMessageBlocks(t, enc.Messages[1].Content)
	if len(user) != 2 || user[0].ToolUseID != "c1" || user[1].ToolUseID != "c2" {
		t.Fatalf("tool results = %+v", user)
	}
}

// TestEncodeRequestValidToolInputForms verifies syntax-only acceptance of
// primitives, arrays, and null, exact raw-byte preservation (including large
// numbers), and the empty-arguments default.
func TestEncodeRequestValidToolInputForms(t *testing.T) {
	const huge = `{"n":12345678901234567890123}`
	cases := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{"empty defaults to object", nil, `{}`},
		{"null", json.RawMessage(`null`), `null`},
		{"array", json.RawMessage(`[1,2,3]`), `[1,2,3]`},
		{"string", json.RawMessage(`"plain"`), `"plain"`},
		{"number", json.RawMessage(`42`), `42`},
		{"bool", json.RawMessage(`true`), `true`},
		{"huge number fidelity", json.RawMessage(huge), huge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model: "m",
				Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
					{Type: ir.BlockToolUse, ToolID: "c", ToolName: "f", ToolInput: tc.input},
				}}},
			}
			enc := encodeAndDecode(t, req)
			blocks := decodeMessageBlocks(t, enc.Messages[0].Content)
			if len(blocks) != 1 || blocks[0].Type != "tool_use" {
				t.Fatalf("blocks = %+v", blocks)
			}
			if got := string(blocks[0].Input); got != tc.want {
				t.Fatalf("input = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEncodeRequestMarshalFailureIsNotValidationError verifies a final
// json.Marshal failure caused by non-client data is not labelled as a client
// validation error automatically.
func TestEncodeRequestMarshalFailureIsNotValidationError(t *testing.T) {
	nan := math.NaN()
	req := &ir.Request{
		Model:       "m",
		Temperature: &nan,
		Messages:    []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	}
	_, err := EncodeRequest(req)
	if err == nil {
		t.Fatal("want marshal error")
	}
	if _, ok := ir.AsRequestValidation(err); ok {
		t.Fatalf("marshal failure mislabeled as validation error: %v", err)
	}
}

// TestDecodeRequestSpeedRaw pins the Messages speed field to raw-value
// semantics: absent and null are absence; any non-null JSON shape (string,
// bool, object, array, number) is retained verbatim and alone is enough to
// create the Anthropic-owned request state.
func TestDecodeRequestSpeedRaw(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		wantSet bool
		wantRaw string
	}{
		{"absent", "", false, ""},
		{"null", `,"speed":null`, false, ""},
		{"fast", `,"speed":"fast"`, true, `"fast"`},
		{"empty", `,"speed":""`, true, `""`},
		{"false", `,"speed":false`, true, `false`},
		{"object", `,"speed":{"mode":"fast"}`, true, `{"mode":"fast"}`},
		{"array", `,"speed":["x"]`, true, `["x"]`},
		{"number", `,"speed":1`, true, `1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]` + tc.field + `}`)
			req, err := DecodeRequest(body)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if !tc.wantSet {
				if req.Anthropic != nil {
					t.Fatalf("absent speed built Anthropic state: %+v", req.Anthropic)
				}
				return
			}
			if req.Anthropic == nil || req.Anthropic.Speed == nil {
				t.Fatalf("speed dropped: %+v", req.Anthropic)
			}
			if got := string(req.Anthropic.Speed); got != tc.wantRaw {
				t.Fatalf("speed = %s, want %s", got, tc.wantRaw)
			}
			if req.Anthropic.CachePolicySet {
				t.Fatalf("speed marked cache policy: %+v", req.Anthropic)
			}
		})
	}
}

// TestEncodeRequestRejectsNonNullSpeed is the translated-encoder backstop: a
// non-null speed on the IR is rejected payload-free instead of dropped, and
// null or absent encodes without any speed key.
func TestEncodeRequestRejectsNonNullSpeed(t *testing.T) {
	for _, raw := range []string{`"fast"`, `""`, `false`, `{"mode":"fast"}`, `[1]`, `0`, `"unknown-shape"`} {
		req := &ir.Request{
			Model: "m", MaxTokens: 1,
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
			Anthropic: &ir.AnthropicRequest{Speed: json.RawMessage(raw)},
		}
		for _, encode := range []func(*ir.Request) ([]byte, error){EncodeRequest, EncodeRequestClaudeCode} {
			out, err := encode(req)
			if err == nil {
				t.Fatalf("speed %s accepted: %s", raw, out)
			}
			if _, ok := ir.AsRequestValidation(err); !ok {
				t.Fatalf("speed %s error not a validation error: %v", raw, err)
			}
			if strings.Contains(err.Error(), raw) {
				t.Fatalf("error echoed caller value %s: %v", raw, err)
			}
		}
	}
	valid := &ir.Request{
		Model: "m", MaxTokens: 1,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Anthropic: &ir.AnthropicRequest{Speed: json.RawMessage("null")},
	}
	for _, encode := range []func(*ir.Request) ([]byte, error){EncodeRequest, EncodeRequestClaudeCode} {
		out, err := encode(valid)
		if err != nil {
			t.Fatalf("null speed rejected: %v", err)
		}
		if strings.Contains(string(out), `"speed"`) {
			t.Fatalf("encoded speed key leaked: %s", out)
		}
	}
}
