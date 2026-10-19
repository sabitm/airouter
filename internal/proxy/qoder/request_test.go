package qoder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
)

func TestEncodeRequestBasic(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model:  "qoder/qmodel_latest",
		System: "sys",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		}},
		MaxTokens: 100,
		Tools: []ir.Tool{{
			Name: "fn", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["system"] != "sys" || m["stream"] != true {
		t.Fatalf("system/stream = %#v", m)
	}
	if ModelKeyFromBody(body) != "qmodel_latest" {
		t.Fatalf("model key = %q", ModelKeyFromBody(body))
	}
	params := m["parameters"].(map[string]any)
	if int(params["max_tokens"].(float64)) != 100 {
		t.Fatalf("max_tokens=%v", params["max_tokens"])
	}
	tools := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%d", len(tools))
	}
}

func TestInjectModelConfig(t *testing.T) {
	plain, err := EncodeRequest(&ir.Request{
		Model:    "auto",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "x"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := json.RawMessage(`{"key":"auto","is_reasoning":true,"max_output_tokens":2048,"source":"system"}`)
	out, err := InjectModelConfig(plain, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	mc := m["model_config"].(map[string]any)
	if mc["key"] != "auto" || mc["is_reasoning"] != true {
		t.Fatalf("model_config=%#v", mc)
	}
	// WAF encode produces non-JSON
	wire := EncodeBody(out)
	if json.Valid(wire) {
		t.Fatal("wire body should not be plain JSON")
	}
}

func TestInjectModelConfigPreservesNumberTokens(t *testing.T) {
	body := []byte(`{
		"tools":[{"function":{"parameters":{"maximum":9223372036854775807}}}],
		"chat_context":{"extra":{"modelConfig":{"key":"auto","is_reasoning":false,"n":9050000000000000001},"huge":1e400}}
	}`)
	cfg := json.RawMessage(`{"key":"auto","is_reasoning":true,"cap":9223372036854775807,"huge":1e400}`)
	out, err := InjectModelConfig(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "9223372036854775807") {
		t.Errorf("client or catalog large integer lost: %s", out)
	}
	if !strings.Contains(s, "9050000000000000001") {
		t.Errorf("nested sibling integer lost: %s", out)
	}
	if !strings.Contains(s, "1e400") {
		t.Errorf("1e400 lost: %s", out)
	}
	if !strings.Contains(s, `"is_reasoning":true`) {
		t.Errorf("is_reasoning not updated: %s", out)
	}
	if _, err := InjectModelConfig([]byte("null"), cfg); err == nil {
		t.Fatal("expected error on top-level null body")
	}
	if _, err := InjectModelConfig([]byte(`{}`), json.RawMessage("null")); err == nil {
		t.Fatal("expected error on null config")
	}
}

func TestStableHash(t *testing.T) {
	a := stableHash("pfx", "a", "b")
	b := stableHash("pfx", "a", "b")
	if a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("len = %d, want 16 hex chars", len(a))
	}
	if stableHash("pfx", "a", "b") == stableHash("PFX", "a", "b") {
		t.Error("different prefix produced same hash")
	}
	if stableHash("pfx", "a", "b") == stableHash("pfx", "a", "c") {
		t.Error("different parts produced same hash")
	}
	if stableHash("pfx") == stableHash("pfx", "a") {
		t.Error("empty parts vs non-empty produced same hash")
	}
}

func TestStableChatRecordID(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "hello"}}
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "f"}}}

	base, err := stableChatRecordID("m", "sys", msgs, tools, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 16 {
		t.Errorf("len = %d, want 16", len(base))
	}
	if got, err := stableChatRecordID("m", "sys", msgs, tools, 100); err != nil || got != base {
		t.Errorf("not deterministic: %q, %v", got, err)
	}
	// Different model -> different id.
	if got, _ := stableChatRecordID("m2", "sys", msgs, tools, 100); got == base {
		t.Error("different model produced same id")
	}
	// Different system -> different id.
	if got, _ := stableChatRecordID("m", "other", msgs, tools, 100); got == base {
		t.Error("different system produced same id")
	}
	// Different message content -> different id.
	msgs2 := []map[string]any{{"role": "user", "content": "bye"}}
	if got, _ := stableChatRecordID("m", "sys", msgs2, tools, 100); got == base {
		t.Error("different message content produced same id")
	}
	// Different maxTokens -> different id.
	if got, _ := stableChatRecordID("m", "sys", msgs, tools, 200); got == base {
		t.Error("different maxTokens produced same id")
	}
	// Different tools -> different id.
	if got, _ := stableChatRecordID("m", "sys", msgs, nil, 100); got == base {
		t.Error("tools present vs absent produced same id")
	}
	// An unparsable raw schema propagates instead of hashing a partial input.
	badTools := []map[string]any{{"function": map[string]any{"parameters": json.RawMessage(`{bad`)}}}
	if got, err := stableChatRecordID("m", "sys", msgs, badTools, 100); err == nil {
		t.Errorf("expected error for invalid raw schema, got %q", got)
	}
}

// qoderRecordIDs extracts both record ids from a plaintext EncodeRequest body.
func qoderRecordIDs(t *testing.T, body []byte) (chat, set string) {
	t.Helper()
	var m struct {
		ChatRecordID string `json:"chat_record_id"`
		RequestSetID string `json:"request_set_id"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode record ids: %v", err)
	}
	return m.ChatRecordID, m.RequestSetID
}

func encodeQoderRecordIDs(t *testing.T, req *ir.Request) (chat, set string) {
	t.Helper()
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return qoderRecordIDs(t, body)
}

func isLowerHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// qoderRecordRequest is a tool round-trip whose pieces each map onto one
// meaningful Qoder wire field so identity regressions can mutate one at a time.
func qoderRecordRequest() *ir.Request {
	return &ir.Request{
		Model:  "qoder/m1",
		System: "be brief",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "weather?"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "calling"},
				{Type: ir.BlockToolUse, ToolID: "call_1", ToolName: "get_weather", ToolInput: json.RawMessage(`{"city":"paris"}`)},
			}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "call_1", ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "sunny"}}},
			}},
		},
		MaxTokens: 1024,
		Tools: []ir.Tool{{
			Name: "get_weather", Description: "look up weather", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
}

func TestEncodeRequestRecordIdentity(t *testing.T) {
	baseChat, baseSet := encodeQoderRecordIDs(t, qoderRecordRequest())

	cases := []struct {
		name   string
		mutate func(*ir.Request)
		change bool
	}{
		{"baseline", nil, false},
		{"tool result text", func(r *ir.Request) { r.Messages[2].Content[0].ToolResult[0].Text = "rainy" }, true},
		{"tool call id", func(r *ir.Request) { r.Messages[1].Content[1].ToolID = "call_2" }, true},
		{"tool call name", func(r *ir.Request) { r.Messages[1].Content[1].ToolName = "get_temp" }, true},
		{"tool call arguments", func(r *ir.Request) { r.Messages[1].Content[1].ToolInput = json.RawMessage(`{"city":"rome"}`) }, true},
		{"tool result reference id", func(r *ir.Request) { r.Messages[2].Content[0].ToolUseID = "call_9" }, true},
		{"system prompt", func(r *ir.Request) { r.System = "be verbose" }, true},
		{"model", func(r *ir.Request) { r.Model = "qoder/m2" }, true},
		{"encoder max tokens", func(r *ir.Request) { r.MaxTokens = 512 }, true},
		{"tool description", func(r *ir.Request) { r.Tools[0].Description = "other" }, true},
		{"tool name", func(r *ir.Request) { r.Tools[0].Name = "get_temp" }, true},
		{"tool schema", func(r *ir.Request) { r.Tools[0].Parameters = json.RawMessage(`{"type":"object","properties":{}}`) }, true},
		{"message role", func(r *ir.Request) { r.Messages[0].Role = ir.RoleAssistant }, true},
		{"message order", func(r *ir.Request) { r.Messages[0], r.Messages[1] = r.Messages[1], r.Messages[0] }, true},
		{"message text", func(r *ir.Request) { r.Messages[0].Content[0].Text = "forecast?" }, true},
		{"assistant text", func(r *ir.Request) { r.Messages[1].Content[0].Text = "invoking" }, true},
		// BlockImage is not sent to Qoder; attachment preflight rejects media before
		// encode, so it must not perturb the record identity.
		{"unsupported image block", func(r *ir.Request) {
			r.Messages[0].Content = append(r.Messages[0].Content, ir.ContentBlock{Type: ir.BlockImage, Image: &ir.Image{Data: "AAAA"}})
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := qoderRecordRequest()
			if tc.mutate != nil {
				tc.mutate(r)
			}
			chat, set := encodeQoderRecordIDs(t, r)
			if chat != set {
				t.Fatalf("chat_record_id %q != request_set_id %q", chat, set)
			}
			if tc.change && chat == baseChat {
				t.Fatalf("record id unchanged after %s: %q", tc.name, chat)
			}
			if !tc.change && chat != baseChat {
				t.Fatalf("record id changed after %s: %q vs %q", tc.name, chat, baseChat)
			}
		})
	}
	_ = baseSet
}

func TestEncodeRequestRecordIDStableAndValid(t *testing.T) {
	var firstChat, firstSet string
	for i := 0; i < 5; i++ {
		body, err := EncodeRequest(qoderRecordRequest())
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			RequestID    string `json:"request_id"`
			ChatRecordID string `json:"chat_record_id"`
			RequestSetID string `json:"request_set_id"`
			Business     struct {
				ID      string `json:"id"`
				BeginAt int64  `json:"begin_at"`
			} `json:"business"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		if m.ChatRecordID != m.RequestSetID {
			t.Fatalf("record fields differ: %q vs %q", m.ChatRecordID, m.RequestSetID)
		}
		if !isLowerHex16(m.ChatRecordID) {
			t.Fatalf("record id %q is not lowercase hex len 16", m.ChatRecordID)
		}
		if i == 0 {
			firstChat, firstSet = m.ChatRecordID, m.RequestSetID
			continue
		}
		if m.ChatRecordID != firstChat || m.RequestSetID != firstSet {
			t.Fatalf("record ids drifted on call %d: %q/%q vs %q/%q", i, m.ChatRecordID, m.RequestSetID, firstChat, firstSet)
		}
	}
}

func TestEncodeRequestEmptyToolInputMatchesExplicitBraces(t *testing.T) {
	empty := qoderRecordRequest()
	empty.Messages[1].Content[1].ToolInput = nil
	braces := qoderRecordRequest()
	braces.Messages[1].Content[1].ToolInput = json.RawMessage(`{}`)

	emptyChat, emptySet := encodeQoderRecordIDs(t, empty)
	bracesChat, bracesSet := encodeQoderRecordIDs(t, braces)
	if emptyChat != bracesChat || emptySet != bracesSet {
		t.Fatalf("empty input %q/%q differs from explicit {} %q/%q", emptyChat, emptySet, bracesChat, bracesSet)
	}
}

func TestEncodeRequestRecordIdentityLargeToolSchemaNumbers(t *testing.T) {
	reqWith := func(schema string) *ir.Request {
		r := qoderRecordRequest()
		r.Tools[0].Parameters = json.RawMessage(schema)
		return r
	}
	big := `{"type":"object","maximum":9050000000000000001}`
	body, err := EncodeRequest(reqWith(big))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("9050000000000000001")) {
		t.Fatalf("large integer lost from wire schema: %s", body)
	}
	huge := `{"type":"object","maximum":1e400}`
	body, err = EncodeRequest(reqWith(huge))
	if err != nil {
		t.Fatalf("1e400 schema rejected: %v", err)
	}
	if !bytes.Contains(body, []byte("1e400")) {
		t.Fatalf("1e400 lost from wire schema: %s", body)
	}
	bigChat, _ := encodeQoderRecordIDs(t, reqWith(big))
	hugeChat, _ := encodeQoderRecordIDs(t, reqWith(huge))
	if bigChat == hugeChat {
		t.Fatal("distinct large schema values produced the same record id")
	}
}

func TestEncodeRequestMalformedToolArgumentsStillEncode(t *testing.T) {
	valid := qoderRecordRequest()
	malformed := qoderRecordRequest()
	// Qoder accepts the raw argument string without JSON validation, so a
	// malformed value must reach the wire and identity unchanged in kind.
	malformed.Messages[1].Content[1].ToolInput = json.RawMessage(`{"city":`)

	validBody, err := EncodeRequest(valid)
	if err != nil {
		t.Fatal(err)
	}
	malformedBody, err := EncodeRequest(malformed)
	if err != nil {
		t.Fatalf("malformed tool args rejected: %v", err)
	}
	if !bytes.Contains(malformedBody, []byte(`{\"city\":`)) {
		t.Fatalf("malformed arguments not relayed as a string: %s", malformedBody)
	}
	validChat, _ := qoderRecordIDs(t, validBody)
	malformedChat, _ := qoderRecordIDs(t, malformedBody)
	if validChat == malformedChat {
		t.Fatal("malformed arguments did not influence the record id")
	}
}

func TestEncodeRequestInvalidToolSchemaErrorsWithNilBody(t *testing.T) {
	r := qoderRecordRequest()
	r.Tools[0].Parameters = json.RawMessage(`{bad`)
	body, err := EncodeRequest(r)
	if err == nil {
		t.Fatal("expected serialization error for invalid raw schema")
	}
	if body != nil {
		t.Fatalf("body = %s, want nil on serialization error", body)
	}
}

func TestToolResultPlain(t *testing.T) {
	t.Run("single text", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "ok"}}}
		if got := toolResultPlain(b); got != "ok" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("multiple joined by newline", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "a"},
			{Type: ir.BlockText, Text: "b"},
		}}
		if got := toolResultPlain(b); got != "a\nb" {
			t.Errorf("got %q, want a\\nb", got)
		}
	})
	t.Run("non-text skipped", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{
			{Type: ir.BlockImage},
			{Type: ir.BlockText, Text: "keep"},
		}}
		if got := toolResultPlain(b); got != "keep" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("empty text skipped", func(t *testing.T) {
		b := ir.ContentBlock{ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: ""}}}
		if got := toolResultPlain(b); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("iserror and empty returns error", func(t *testing.T) {
		b := ir.ContentBlock{IsError: true, ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: ""}}}
		if got := toolResultPlain(b); got != "error" {
			t.Errorf("got %q, want error", got)
		}
	})
	t.Run("iserror with text returns text", func(t *testing.T) {
		b := ir.ContentBlock{IsError: true, ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "boom"}}}
		if got := toolResultPlain(b); got != "boom" {
			t.Errorf("got %q, want boom", got)
		}
	})
}

func TestClampMaxTokens(t *testing.T) {
	cfg := json.RawMessage(`{"max_output_tokens":50}`)

	t.Run("no parameters unchanged", func(t *testing.T) {
		body := []byte(`{"model":"x"}`)
		if got := clampMaxTokens(body, cfg); string(got) != `{"model":"x"}` {
			t.Errorf("got %s", got)
		}
	})
	t.Run("max_tokens absent clamped to cap", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":0}}`)
		got := clampMaxTokens(body, cfg)
		var m map[string]any
		_ = json.Unmarshal(got, &m)
		params, _ := m["parameters"].(map[string]any)
		if params == nil || params["max_tokens"].(float64) != 50 {
			t.Errorf("got %s, want max_tokens=50", got)
		}
	})
	t.Run("max_tokens over cap clamped", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":100}}`)
		got := clampMaxTokens(body, cfg)
		var m map[string]any
		_ = json.Unmarshal(got, &m)
		params, _ := m["parameters"].(map[string]any)
		if params["max_tokens"].(float64) != 50 {
			t.Errorf("got %s, want max_tokens=50", got)
		}
	})
	t.Run("max_tokens in range unchanged", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":30}}`)
		got := clampMaxTokens(body, cfg)
		if string(got) != `{"parameters":{"max_tokens":30}}` {
			t.Errorf("got %s, want unchanged", got)
		}
	})
	t.Run("invalid config unchanged", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":100}}`)
		if got := clampMaxTokens(body, json.RawMessage(`{bad`)); string(got) != `{"parameters":{"max_tokens":100}}` {
			t.Errorf("got %s, want unchanged for bad config", got)
		}
	})
	t.Run("invalid body unchanged", func(t *testing.T) {
		body := []byte(`not json`)
		if got := clampMaxTokens(body, cfg); string(got) != `not json` {
			t.Errorf("got %s, want unchanged for bad body", got)
		}
	})
	t.Run("preserves large sibling while clamping", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":100,"n":9050000000000000001,"huge":1e400}}`)
		got := clampMaxTokens(body, cfg)
		s := string(got)
		if !strings.Contains(s, `"max_tokens":50`) {
			t.Errorf("cap not written: %s", got)
		}
		if !strings.Contains(s, "9050000000000000001") {
			t.Errorf("large sibling lost: %s", got)
		}
		if !strings.Contains(s, "1e400") {
			t.Errorf("1e400 lost: %s", got)
		}
	})
	t.Run("fractional max_tokens unchanged", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":12.5}}`)
		if got := clampMaxTokens(body, cfg); string(got) != string(body) {
			t.Errorf("got %s, want fractional unchanged", got)
		}
	})
	t.Run("unparsable max_tokens unchanged", func(t *testing.T) {
		body := []byte(`{"parameters":{"max_tokens":"high"}}`)
		if got := clampMaxTokens(body, cfg); string(got) != string(body) {
			t.Errorf("got %s, want unparsable unchanged", got)
		}
	})
}

func TestCredsFromProvider(t *testing.T) {
	t.Run("nil returns empty", func(t *testing.T) {
		c := CredsFromProvider(nil)
		if c.UserID != "" || c.AuthToken != "" || c.MachineID != "" || c.Name != "" || c.Email != "" {
			t.Errorf("got %+v, want empty", c)
		}
	})
	t.Run("apikey only sets AuthToken", func(t *testing.T) {
		p := &domain.Provider{APIKey: "static-key"}
		c := CredsFromProvider(p)
		if c.AuthToken != "static-key" {
			t.Errorf("AuthToken = %q", c.AuthToken)
		}
		if c.UserID != "" || c.MachineID != "" {
			t.Errorf("identity fields should be empty: %+v", c)
		}
	})
	t.Run("oauth only uses AccessToken and copies identity", func(t *testing.T) {
		p := &domain.Provider{
			OAuthCreds: &domain.OAuthCreds{
				AccessToken: "tok",
				UserID:      "u1",
				MachineID:   "mid",
				DisplayName: "Alice",
				Email:       "a@e.com",
			},
		}
		c := CredsFromProvider(p)
		if c.AuthToken != "tok" {
			t.Errorf("AuthToken = %q, want tok", c.AuthToken)
		}
		if c.UserID != "u1" || c.MachineID != "mid" || c.Name != "Alice" || c.Email != "a@e.com" {
			t.Errorf("identity = %+v", c)
		}
	})
	t.Run("both apikey and oauth: apikey wins for AuthToken, identity still copied", func(t *testing.T) {
		p := &domain.Provider{
			APIKey: "resolved-token",
			OAuthCreds: &domain.OAuthCreds{
				AccessToken: "tok",
				UserID:      "u1",
				MachineID:   "mid",
				DisplayName: "Alice",
				Email:       "a@e.com",
			},
		}
		c := CredsFromProvider(p)
		if c.AuthToken != "resolved-token" {
			t.Errorf("AuthToken = %q, want resolved-token (apikey wins)", c.AuthToken)
		}
		if c.UserID != "u1" || c.MachineID != "mid" || c.Name != "Alice" || c.Email != "a@e.com" {
			t.Errorf("identity = %+v, want copied from OAuth", c)
		}
	})
}

func TestModelSourceFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want string
	}{
		{"present source", `{"source":"catalog"}`, "catalog"},
		{"empty source", `{"source":""}`, "system"},
		{"missing field", `{"key":"x"}`, "system"},
		{"invalid json", `{bad`, "system"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModelSourceFromConfig(json.RawMessage(tc.cfg)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelKeyAndSource(t *testing.T) {
	// Reuse the EncodeRequest + InjectModelConfig shape to build a body with
	// both the nested modelConfig.key and a top-level model_config.
	plain, err := EncodeRequest(&ir.Request{
		Model:    "qoder/qmodel_latest",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "x"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("both key and source present", func(t *testing.T) {
		cfg := json.RawMessage(`{"key":"qmodel_latest","source":"catalog"}`)
		injected, err := InjectModelConfig(plain, cfg)
		if err != nil {
			t.Fatal(err)
		}
		key, source := ModelKeyAndSource(injected)
		if key != "qmodel_latest" {
			t.Errorf("key = %q", key)
		}
		if source != "catalog" {
			t.Errorf("source = %q", source)
		}
	})
	t.Run("source absent defaults to system", func(t *testing.T) {
		cfg := json.RawMessage(`{"key":"qmodel_latest"}`)
		injected, err := InjectModelConfig(plain, cfg)
		if err != nil {
			t.Fatal(err)
		}
		key, source := ModelKeyAndSource(injected)
		if key != "qmodel_latest" {
			t.Errorf("key = %q", key)
		}
		if source != "system" {
			t.Errorf("source = %q, want system", source)
		}
	})
	t.Run("invalid body returns empty key and system", func(t *testing.T) {
		key, source := ModelKeyAndSource([]byte(`not json`))
		if key != "" {
			t.Errorf("key = %q, want empty", key)
		}
		if source != "system" {
			t.Errorf("source = %q, want system", source)
		}
	})
}

func TestModelKeyFromBodyEdges(t *testing.T) {
	t.Run("malformed json returns empty", func(t *testing.T) {
		if got := ModelKeyFromBody([]byte(`{not valid`)); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("missing nested path returns empty", func(t *testing.T) {
		if got := ModelKeyFromBody([]byte(`{"chat_context":{}}`)); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("valid nested key", func(t *testing.T) {
		body := []byte(`{"chat_context":{"extra":{"modelConfig":{"key":"k1"}}}}`)
		if got := ModelKeyFromBody(body); got != "k1" {
			t.Errorf("got %q, want k1", got)
		}
	})
}

func TestEncodeMessages(t *testing.T) {
	t.Run("user text only", func(t *testing.T) {
		out, lastUser := encodeMessages([]ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
		})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0]["role"] != "user" || out[0]["content"] != "hi" {
			t.Errorf("msg = %+v", out[0])
		}
		if lastUser != "hi" {
			t.Errorf("lastUser = %q, want hi", lastUser)
		}
	})

	t.Run("user text plus image keeps text only", func(t *testing.T) {
		// Images are not representable on Qoder; preflight skips before encode.
		out, lastUser := encodeMessages([]ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "look"},
				{Type: ir.BlockImage, Image: &ir.Image{Data: "B64"}},
			}},
		})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		want := "look"
		if out[0]["content"] != want {
			t.Errorf("content = %q, want %q", out[0]["content"], want)
		}
		if lastUser != want {
			t.Errorf("lastUser = %q, want %q", lastUser, want)
		}
	})

	t.Run("user tool result only emits role tool message", func(t *testing.T) {
		out, lastUser := encodeMessages([]ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "tu1",
					ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r"}}},
			}},
		})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0]["role"] != "tool" {
			t.Errorf("role = %v, want tool", out[0]["role"])
		}
		if out[0]["tool_call_id"] != "tu1" {
			t.Errorf("tool_call_id = %v", out[0]["tool_call_id"])
		}
		if out[0]["content"] != "r" {
			t.Errorf("content = %v, want r", out[0]["content"])
		}
		if lastUser != "" {
			t.Errorf("lastUser = %q, want empty (no user text)", lastUser)
		}
	})

	t.Run("user mixed tool result text and image", func(t *testing.T) {
		out, lastUser := encodeMessages([]ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{
				{Type: ir.BlockToolResult, ToolUseID: "tu1",
					ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "r"}}},
				{Type: ir.BlockText, Text: "after"},
				{Type: ir.BlockImage, Image: &ir.Image{Data: "B"}},
			}},
		})
		// tool result -> role:tool; text+image joined -> role:user
		if len(out) != 2 {
			t.Fatalf("len = %d, want 2", len(out))
		}
		if out[0]["role"] != "tool" || out[0]["tool_call_id"] != "tu1" {
			t.Errorf("msg0 = %+v", out[0])
		}
		if out[1]["role"] != "user" || out[1]["content"] != "after" {
			t.Errorf("msg1 = %+v", out[1])
		}
		if lastUser != "after" {
			t.Errorf("lastUser = %q", lastUser)
		}
	})

	t.Run("assistant text only", func(t *testing.T) {
		out, _ := encodeMessages([]ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "reply"}}},
		})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0]["role"] != "assistant" || out[0]["content"] != "reply" {
			t.Errorf("msg = %+v", out[0])
		}
		if _, has := out[0]["tool_calls"]; has {
			t.Error("tool_calls should be absent")
		}
	})

	t.Run("assistant tool use only defaults empty input to {}", func(t *testing.T) {
		out, _ := encodeMessages([]ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockToolUse, ToolID: "t1", ToolName: "f", ToolInput: nil},
			}},
		})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0]["content"] != "" {
			t.Errorf("content = %q, want empty for tool-only assistant", out[0]["content"])
		}
		tcs, ok := out[0]["tool_calls"].([]map[string]any)
		if !ok || len(tcs) != 1 {
			t.Fatalf("tool_calls = %+v", out[0]["tool_calls"])
		}
		tc := tcs[0]
		if tc["id"] != "t1" || tc["type"] != "function" {
			t.Errorf("tc = %+v", tc)
		}
		fn := tc["function"].(map[string]any)
		if fn["name"] != "f" || fn["arguments"] != "{}" {
			t.Errorf("function = %+v, want name=f arguments={}", fn)
		}
	})

	t.Run("assistant text plus tool use", func(t *testing.T) {
		out, _ := encodeMessages([]ir.Message{
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "calling"},
				{Type: ir.BlockToolUse, ToolID: "t1", ToolName: "f", ToolInput: json.RawMessage(`{"x":1}`)},
			}},
		})
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
		if out[0]["content"] != "calling" {
			t.Errorf("content = %q, want calling", out[0]["content"])
		}
		tcs := out[0]["tool_calls"].([]map[string]any)
		if len(tcs) != 1 {
			t.Fatalf("tool_calls len = %d", len(tcs))
		}
		fn := tcs[0]["function"].(map[string]any)
		if fn["arguments"] != `{"x":1}` {
			t.Errorf("arguments = %q, want raw args", fn["arguments"])
		}
	})

	t.Run("multiple messages preserve order and lastUser", func(t *testing.T) {
		out, lastUser := encodeMessages([]ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "first"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "mid"}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "last"}}},
		})
		if len(out) != 3 {
			t.Fatalf("len = %d, want 3", len(out))
		}
		if out[0]["role"] != "user" || out[1]["role"] != "assistant" || out[2]["role"] != "user" {
			t.Errorf("roles = %v %v %v", out[0]["role"], out[1]["role"], out[2]["role"])
		}
		if lastUser != "last" {
			t.Errorf("lastUser = %q, want last", lastUser)
		}
	})
}

// qoderTestProvider builds an OAuth provider with the identity fields COSY and
// the session hash read.
func qoderTestProvider(userID, token string) *domain.Provider {
	return &domain.Provider{
		APIKey: token,
		OAuthCreds: &domain.OAuthCreds{
			UserID:      userID,
			AccessToken: token,
		},
	}
}

// seedQoderCatalog installs configs for the provider and clears the shared
// catalog after the test.
func seedQoderCatalog(t *testing.T, p *domain.Provider, configs map[string]string) {
	t.Helper()
	t.Cleanup(ClearCatalogForTest)
	raw := make(map[string]json.RawMessage, len(configs))
	keys := make([]string, 0, len(configs))
	for k, v := range configs {
		raw[k] = json.RawMessage(v)
		keys = append(keys, k)
	}
	SeedCatalogForTest(p, raw, keys)
}

// mustEncodeQoderRequest builds the plaintext body EncodeRequest produces.
func mustEncodeQoderRequest(t *testing.T, model string) []byte {
	t.Helper()
	body, err := EncodeRequest(&ir.Request{
		Model:    model,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// decodeWireForTest inverts EncodeBody. It lives in tests because production
// only needs the one-way WAF encoding.
func decodeWireForTest(t *testing.T, wire []byte) map[string]json.RawMessage {
	t.Helper()
	if len(wire) == 0 {
		t.Fatal("decodeWireForTest: empty wire")
	}
	n := len(wire)
	a := n / 3
	rear := make([]byte, n)
	for i := 0; i < n; i++ {
		c := wire[i]
		if c == '$' {
			rear[i] = '='
			continue
		}
		rear[i] = c
		for j := 0; j < len(stdAlphabet); j++ {
			if s2c[stdAlphabet[j]] == c {
				rear[i] = stdAlphabet[j]
				break
			}
		}
	}
	// Encoder emits [tail][mid][head]; rebuild the standard base64 order.
	std := make([]byte, 0, n)
	std = append(std, rear[n-a:]...)
	std = append(std, rear[a:n-a]...)
	std = append(std, rear[:a]...)
	plain, err := base64.StdEncoding.DecodeString(string(std))
	if err != nil {
		t.Fatalf("decodeWireForTest: base64: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(plain, &m); err != nil {
		t.Fatalf("decodeWireForTest: json: %v (plain=%s)", err, plain)
	}
	return m
}

func objectSessionID(t *testing.T, m map[string]json.RawMessage) string {
	t.Helper()
	var sid string
	if err := json.Unmarshal(m["session_id"], &sid); err != nil {
		t.Fatalf("session_id: %v", err)
	}
	return sid
}

func wireSessionID(t *testing.T, wire []byte) string {
	t.Helper()
	return objectSessionID(t, decodeWireForTest(t, wire))
}

func TestWithSessionIDRejectsNonObject(t *testing.T) {
	for _, in := range []string{"not json", "null", "[]", `"str"`, "42", "true"} {
		if _, err := withSessionID([]byte(in), "u", "m"); err == nil {
			t.Errorf("input %q: expected error", in)
		}
	}
}

func TestWithSessionIDAlwaysOverwrites(t *testing.T) {
	body := []byte(`{"session_id":"old","chat_record_id":"rec","request_set_id":"set","n":9050000000000000001,"huge":1e400}`)
	first, err := withSessionID(body, "u1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(first, &m); err != nil {
		t.Fatal(err)
	}
	if got := objectSessionID(t, m); got != stableHash("qoder-session", "u1", "m1") {
		t.Fatalf("session_id = %q", got)
	}
	if !strings.Contains(string(first), "9050000000000000001") || !strings.Contains(string(first), "1e400") {
		t.Fatalf("large numbers lost: %s", first)
	}
	// Re-running the helper on its own output stays stable and keeps the field.
	second, err := withSessionID(first, "u1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatalf("helper not idempotent:\nfirst  %s\nsecond %s", first, second)
	}
}

func TestPrepareWireBodySessionIDReferenceFixture(t *testing.T) {
	p := qoderTestProvider("fixture-user-1", "tok")
	seedQoderCatalog(t, p, map[string]string{"qmodel_latest": `{"key":"qmodel_latest"}`})
	plain := mustEncodeQoderRequest(t, "qoder/qmodel_latest")
	wire, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	// Fixed fixture from ignored/9router/open-sse/executors/qoder.js:268:
	// stableHash("qoder-session", "fixture-user-1", "qmodel_latest").
	const want = "0aab3589cf9782f9"
	if got := wireSessionID(t, wire); got != want {
		t.Fatalf("session_id = %q, want reference fixture %q", got, want)
	}
}

func TestPrepareWireBodySessionIDScopedByAccount(t *testing.T) {
	cfg := map[string]string{"m1": `{"key":"m1"}`}
	pa := qoderTestProvider("user-a", "tok-a")
	pb := qoderTestProvider("user-b", "tok-b")
	seedQoderCatalog(t, pa, cfg)
	seedQoderCatalog(t, pb, cfg)
	plain := mustEncodeQoderRequest(t, "qoder/m1")

	wa, err := PrepareWireBody(context.Background(), pa, plain)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := PrepareWireBody(context.Background(), pb, plain)
	if err != nil {
		t.Fatal(err)
	}
	sa, sb := wireSessionID(t, wa), wireSessionID(t, wb)
	if sa == sb {
		t.Fatalf("different accounts share session_id %q", sa)
	}
	if sa != stableHash("qoder-session", "user-a", "m1") {
		t.Fatalf("account A session_id = %q", sa)
	}
	if sb != stableHash("qoder-session", "user-b", "m1") {
		t.Fatalf("account B session_id = %q", sb)
	}
}

func TestPrepareWireBodySessionIDStableForAccount(t *testing.T) {
	p := qoderTestProvider("acct-1", "tok")
	seedQoderCatalog(t, p, map[string]string{"m1": `{"key":"m1"}`})
	plain := mustEncodeQoderRequest(t, "qoder/m1")

	first, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := wireSessionID(t, first), wireSessionID(t, second); a != b {
		t.Fatalf("session_id unstable: %q vs %q", a, b)
	}
}

func TestPrepareWireBodySessionIDVariesByModel(t *testing.T) {
	p := qoderTestProvider("acct-1", "tok")
	seedQoderCatalog(t, p, map[string]string{"m1": `{"key":"m1"}`, "m2": `{"key":"m2"}`})
	w1, err := PrepareWireBody(context.Background(), p, mustEncodeQoderRequest(t, "qoder/m1"))
	if err != nil {
		t.Fatal(err)
	}
	w2, err := PrepareWireBody(context.Background(), p, mustEncodeQoderRequest(t, "qoder/m2"))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := wireSessionID(t, w1), wireSessionID(t, w2); a == b {
		t.Fatalf("different models share session_id %q", a)
	}
}

func TestPrepareWireBodySessionIDSameAcrossProviderRows(t *testing.T) {
	p1 := qoderTestProvider("acct-1", "tok-1")
	p1.ID, p1.Name = 1, "row-one"
	p2 := qoderTestProvider("acct-1", "tok-2")
	p2.ID, p2.Name = 2, "row-two"
	seedQoderCatalog(t, p1, map[string]string{"m1": `{"key":"m1"}`})
	plain := mustEncodeQoderRequest(t, "qoder/m1")

	w1, err := PrepareWireBody(context.Background(), p1, plain)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := PrepareWireBody(context.Background(), p2, plain)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := wireSessionID(t, w1), wireSessionID(t, w2); a != b {
		t.Fatalf("same account rows differ: %q vs %q", a, b)
	}
}

func TestPrepareWireBodyOverwritesPlaceholderSession(t *testing.T) {
	p := qoderTestProvider("acct-1", "tok")
	seedQoderCatalog(t, p, map[string]string{"m1": `{"key":"m1"}`})
	plain := mustEncodeQoderRequest(t, "qoder/m1")
	// The encoder seeds a model-only placeholder that prepare must replace.
	var before map[string]json.RawMessage
	if err := json.Unmarshal(plain, &before); err != nil {
		t.Fatal(err)
	}
	placeholder := stableHash("qoder-session", "m1")
	if got := objectSessionID(t, before); got != placeholder {
		t.Fatalf("precondition: placeholder = %q", got)
	}

	wire, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	got := wireSessionID(t, wire)
	if want := stableHash("qoder-session", "acct-1", "m1"); got != want {
		t.Fatalf("session_id = %q, want account-scoped %q", got, want)
	}
	if got == placeholder {
		t.Fatalf("placeholder not overwritten: %q", got)
	}
}

func TestPrepareWireBodyDoesNotMutateInputs(t *testing.T) {
	p := qoderTestProvider("acct-1", "tok")
	p.ID, p.Name = 7, "row"
	seedQoderCatalog(t, p, map[string]string{"m1": `{"key":"m1"}`})
	plain := mustEncodeQoderRequest(t, "qoder/m1")
	original := append([]byte(nil), plain...)

	wire, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, original) {
		t.Fatalf("input plaintext mutated:\nbefore %s\nafter  %s", original, plain)
	}
	if p.APIKey != "tok" || p.ID != 7 || p.Name != "row" {
		t.Fatalf("provider mutated: %+v", p)
	}
	if p.OAuthCreds.UserID != "acct-1" || p.OAuthCreds.AccessToken != "tok" {
		t.Fatalf("credentials mutated: %+v", p.OAuthCreds)
	}
	if len(wire) == 0 {
		t.Fatal("empty wire body")
	}
}

func TestPrepareWireBodyEmptyUserIDUsesEmptyComponent(t *testing.T) {
	p := qoderTestProvider("", "tok")
	seedQoderCatalog(t, p, map[string]string{"m1": `{"key":"m1"}`})
	plain := mustEncodeQoderRequest(t, "qoder/m1")
	wire, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	got := wireSessionID(t, wire)
	if want := stableHash("qoder-session", "", "m1"); got != want {
		t.Fatalf("session_id = %q, want empty-component hash %q", got, want)
	}
	if got == stableHash("qoder-session", "m1") {
		t.Fatal("empty user id reused the old model-only hash")
	}
}

func TestPrepareWireBodyPreservesUnrelatedWireFields(t *testing.T) {
	p := qoderTestProvider("acct-1", "tok")
	seedQoderCatalog(t, p, map[string]string{"m1": `{"key":"m1","is_reasoning":true,"max_output_tokens":50}`})
	encoded, err := EncodeRequest(&ir.Request{
		Model:     "qoder/m1",
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		MaxTokens: 32768,
		Tools: []ir.Tool{{
			Name:        "fn",
			Description: "d",
			Parameters:  json.RawMessage(`{"type":"object","maximum":9223372036854775807}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &top); err != nil {
		t.Fatal(err)
	}
	top["n"] = json.RawMessage("9050000000000000001")
	top["huge"] = json.RawMessage("1e400")
	plain, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	origRecordID := string(top["chat_record_id"])
	origRequestSetID := string(top["request_set_id"])

	wire, err := PrepareWireBody(context.Background(), p, plain)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeWireForTest(t, wire)

	var mc map[string]json.RawMessage
	if err := json.Unmarshal(got["model_config"], &mc); err != nil {
		t.Fatalf("model_config: %v", err)
	}
	if string(mc["is_reasoning"]) != "true" {
		t.Errorf("model_config.is_reasoning = %s", mc["is_reasoning"])
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(got["parameters"], &params); err != nil {
		t.Fatalf("parameters: %v", err)
	}
	if string(params["max_tokens"]) != "50" {
		t.Errorf("max_tokens = %s, want clamp to 50", params["max_tokens"])
	}
	if string(got["n"]) != "9050000000000000001" {
		t.Errorf("n = %s, want raw large integer", got["n"])
	}
	if string(got["huge"]) != "1e400" {
		t.Errorf("huge = %s, want raw 1e400", got["huge"])
	}
	if !bytes.Contains(got["tools"], []byte("9223372036854775807")) {
		t.Errorf("tool raw schema lost: %s", got["tools"])
	}
	if string(got["chat_record_id"]) != origRecordID {
		t.Errorf("chat_record_id = %s, want %s", got["chat_record_id"], origRecordID)
	}
	if string(got["request_set_id"]) != origRequestSetID {
		t.Errorf("request_set_id = %s, want %s", got["request_set_id"], origRequestSetID)
	}
	// Nested chat_context.extra.modelConfig.is_reasoning must be updated too.
	var cc, extra, nested map[string]json.RawMessage
	if err := json.Unmarshal(got["chat_context"], &cc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(cc["extra"], &extra); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(extra["modelConfig"], &nested); err != nil {
		t.Fatal(err)
	}
	if string(nested["is_reasoning"]) != "true" {
		t.Errorf("nested is_reasoning = %s, want true", nested["is_reasoning"])
	}
}

func TestPrepareWireBodyMissingModelNoWire(t *testing.T) {
	p := qoderTestProvider("acct-1", "tok")
	wire, err := PrepareWireBody(context.Background(), p, []byte(`{"foo":1}`))
	if err == nil {
		t.Fatal("expected error for missing model key")
	}
	if wire != nil {
		t.Fatalf("wire = %q, want nil on preparation error", wire)
	}
}

func TestPrepareWireBodyCatalogErrorNoWire(t *testing.T) {
	// No credentials: catalog resolution fails locally without a network call.
	ClearCatalogForTest()
	t.Cleanup(ClearCatalogForTest)
	wire, err := PrepareWireBody(context.Background(), &domain.Provider{}, mustEncodeQoderRequest(t, "qoder/m1"))
	if err == nil {
		t.Fatal("expected catalog error")
	}
	if wire != nil {
		t.Fatalf("wire = %q, want nil on catalog error", wire)
	}
}
