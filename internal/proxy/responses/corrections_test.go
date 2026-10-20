package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestStrictToolEncoded(t *testing.T) {
	strict := true
	req := &ir.Request{
		Model:    "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:    []ir.Tool{{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict}},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"strict":true`) {
		t.Fatalf("strict dropped: %s", out)
	}
	decoded, err := DecodeRequest(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].Strict == nil || !*decoded.Tools[0].Strict {
		t.Fatalf("strict not decoded: %+v", decoded.Tools)
	}
}

func TestOutputFormatAndMetadataMapped(t *testing.T) {
	req := &ir.Request{
		Model:    "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Anthropic: &ir.AnthropicRequest{
			Metadata:     json.RawMessage(`{"user_id":"u1"}`),
			OutputFormat: json.RawMessage(`{"type":"json_schema","schema":{"type":"object","title":"Out"}}`),
		},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	text, ok := doc["text"].(map[string]any)
	if !ok {
		t.Fatalf("text=%v body=%s", doc["text"], out)
	}
	format, ok := text["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || format["name"] != "Out" {
		t.Fatalf("text.format=%v", text["format"])
	}
	if !strings.Contains(string(out), `"user_id":"u1"`) {
		t.Fatalf("metadata dropped: %s", out)
	}
}

func TestUnsupportedOutputFormatFails(t *testing.T) {
	req := &ir.Request{
		Model:    "m",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Anthropic: &ir.AnthropicRequest{
			OutputFormat: json.RawMessage(`{"type":"regex"}`),
		},
	}
	if _, err := EncodeRequest(req); err == nil {
		t.Fatal("unsupported output format was silently dropped")
	}
}

func TestServerOnlyUnaryResponseFails(t *testing.T) {
	resp := &ir.Response{Model: "m", Content: []ir.ContentBlock{{Type: ir.BlockServer, AnthropicServer: &ir.AnthropicServerBlock{WireType: "compaction", Raw: json.RawMessage(`{"type":"compaction","content":"x"}`)}}}}
	if _, err := EncodeResponse(resp); err == nil {
		t.Fatal("server-only response became an empty success")
	}
}

func TestServerStreamEventRejected(t *testing.T) {
	enc := NewStreamEncoder("m")
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventServerUpdate, Data: `{"content":"x"}`}, nil); err == nil {
		t.Fatal("server update was accepted by the responses stream encoder")
	}
}

func TestProviderOwnedToolControlsFailClosed(t *testing.T) {
	eager, deferred := true, true
	maxUses := 2
	for name, at := range map[string]*ir.AnthropicTool{
		"server type":     {Type: "web_search_20250305"},
		"eager":           {EagerInput: &eager},
		"deferred":        {DeferLoading: &deferred},
		"max uses":        {MaxUses: &maxUses},
		"allowed domains": {AllowedDomains: []string{"example.test"}},
		"blocked domains": {BlockedDomains: []string{"example.test"}},
	} {
		t.Run(name, func(t *testing.T) {
			req := &ir.Request{
				Model:    "m",
				Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
				Tools:    []ir.Tool{{Name: "t", AnthropicTool: at}},
			}
			if _, err := EncodeRequest(req); err == nil {
				t.Fatalf("%s was silently dropped", name)
			}
		})
	}
}
