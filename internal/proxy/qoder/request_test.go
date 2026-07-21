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
