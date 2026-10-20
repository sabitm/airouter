package cursor

import (
	"encoding/json"
	"testing"

	"airouter/internal/proxy/ir"
)

func cursorRequest(tools []ir.Tool, anthropic *ir.AnthropicRequest) *ir.Request {
	return &ir.Request{
		Model:     "m",
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Tools:     tools,
		Anthropic: anthropic,
	}
}

func TestStrictToolPolicy(t *testing.T) {
	strict := true
	if _, err := EncodeAgentRequest(cursorRequest([]ir.Tool{{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict}}, nil)); err != nil {
		t.Fatalf("ordinary strict custom tool rejected: %v", err)
	}
	loose := false
	if _, err := EncodeAgentRequest(cursorRequest([]ir.Tool{{Name: "t", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &loose}}, nil)); err != nil {
		t.Fatalf("strict=false rejected: %v", err)
	}
	if _, err := EncodeAgentRequest(cursorRequest([]ir.Tool{{Name: "t", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}, nil)); err != nil {
		t.Fatalf("ordinary schema tool rejected: %v", err)
	}
}

func TestDeferAndEagerPolicy(t *testing.T) {
	eager, deferred := true, true
	for name, at := range map[string]*ir.AnthropicTool{
		"eager":    {EagerInput: &eager},
		"deferred": {DeferLoading: &deferred},
	} {
		if _, err := EncodeAgentRequest(cursorRequest([]ir.Tool{{Name: "t", AnthropicTool: at}}, nil)); err == nil {
			t.Fatalf("%s=true was silently accepted", name)
		}
	}
}

func TestOutputFormatRejected(t *testing.T) {
	if _, err := EncodeAgentRequest(cursorRequest(nil, &ir.AnthropicRequest{OutputFormat: json.RawMessage(`{"type":"json_schema","schema":{"type":"object"}}`)})); err == nil {
		t.Fatal("output format was silently accepted")
	}
}
