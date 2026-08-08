package qoder

import (
	"encoding/json"
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
	msgs := []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hello"}}},
		{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}},
	}
	tools := []map[string]any{{"name": "f"}}

	base := stableChatRecordID("m", msgs, tools, 100)
	if len(base) != 16 {
		t.Errorf("len = %d, want 16", len(base))
	}
	if stableChatRecordID("m", msgs, tools, 100) != base {
		t.Error("not deterministic for identical inputs")
	}
	// Different model -> different id.
	if stableChatRecordID("m2", msgs, tools, 100) == base {
		t.Error("different model produced same id")
	}
	// Different message text -> different id.
	msgs2 := []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "bye"}}}}
	if stableChatRecordID("m", msgs2, tools, 100) == base {
		t.Error("different message text produced same id")
	}
	// Different maxTokens -> different id.
	if stableChatRecordID("m", msgs, tools, 200) == base {
		t.Error("different maxTokens produced same id")
	}
	// Tools present vs absent -> different id.
	if stableChatRecordID("m", msgs, nil, 100) == base {
		t.Error("tools present vs absent produced same id")
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
