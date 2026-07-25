package qoder

import (
	"encoding/json"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestEncodeRequestBasic(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model:  "qoder/qmodel_latest",
		System: "sys",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
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
		Model: "auto",
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
