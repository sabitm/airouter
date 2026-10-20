package anthropic

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestServerToolRoundTrip(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"tool_reference","tool_name":"search"}]}],"tools":[{"type":"web_search_20250305","name":"web_search","defer_loading":true,"cache_control":{"type":"ephemeral","scope":"global"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"context_management":{"edits":[{"type":"compact_20250101"}]}}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Tools[0].AnthropicTool == nil || req.Tools[0].AnthropicTool.Type == "" || req.Tools[0].AnthropicTool.DeferLoading == nil {
		t.Fatalf("typed tool dropped: %+v", req.Tools[0])
	}
	if req.ToolChoice == nil || req.ToolChoice.DisableParallel == nil || !*req.ToolChoice.DisableParallel {
		t.Fatalf("tool choice = %+v", req.ToolChoice)
	}
	if req.Messages[0].Content[1].Type != ir.BlockServer {
		t.Fatalf("tool_reference dropped: %+v", req.Messages[0].Content[1])
	}
	if req.Anthropic == nil || len(req.Anthropic.ContextManagement) == 0 {
		t.Fatal("context_management dropped")
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"web_search_20250305"`, `"defer_loading":true`, `"scope":"global"`, `"disable_parallel_tool_use":true`, `"tool_reference"`, `"context_management"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestRefusalStopReasonRoundTrip(t *testing.T) {
	body := []byte(`{"id":"msg","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"no"}],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":1}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopRefusal {
		t.Fatalf("stop = %s", resp.StopReason)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stop_reason":"refusal"`) {
		t.Fatalf("encoded = %s", out)
	}
}
