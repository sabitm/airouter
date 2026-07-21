package antigravity

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func TestEncodeRequestBasic(t *testing.T) {
	temp := 0.2
	body, err := EncodeRequest(&ir.Request{
		Model:  "gemini-3-flash",
		System: "be brief",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		}},
		Temperature: &temp,
		Tools: []ir.Tool{{
			Name:        "Shell",
			Description: "run",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Project != "" {
		t.Fatalf("project should be empty before inject: %q", env.Project)
	}
	if env.UserAgent != "antigravity" || env.RequestType != "agent" {
		t.Fatalf("envelope meta: %+v", env)
	}
	if env.Model != "gemini-3-flash" {
		t.Fatalf("model %q", env.Model)
	}
	if env.Request.SystemInstruction == nil || env.Request.SystemInstruction.Parts[0].Text != "be brief" {
		t.Fatalf("system: %+v", env.Request.SystemInstruction)
	}
	if env.Request.GenerationConfig == nil || env.Request.GenerationConfig.MaxOutputTokens != DefaultMaxTokens {
		t.Fatalf("max tokens: %+v", env.Request.GenerationConfig)
	}
	decls := env.Request.Tools[0].FunctionDeclarations
	// Shell should be cloaked
	if decls[0].Name != "Shell_ide" {
		t.Fatalf("tool name %q", decls[0].Name)
	}
	if env.Request.ToolConfig == nil || env.Request.ToolConfig.FunctionCallingConfig.Mode != "VALIDATED" {
		t.Fatal("expected VALIDATED mode")
	}
	// decoys present
	found := false
	for _, d := range decls {
		if d.Name == "run_command" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing decoy")
	}
}

func TestEncodeRequestToolRoundTrip(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{
		Model: "m",
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "do"}}},
			{Role: ir.RoleAssistant, Content: []ir.ContentBlock{{
				Type: ir.BlockToolUse, ToolID: "t1", ToolName: "Shell",
				ToolInput: json.RawMessage(`{"cmd":"ls"}`),
			}}},
			{Role: ir.RoleUser, Content: []ir.ContentBlock{{
				Type: ir.BlockToolResult, ToolUseID: "t1",
				ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "ok"}},
			}}},
		},
		Tools: []ir.Tool{{Name: "Shell", Parameters: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	// Find model functionCall with thoughtSignature and cloaked name
	var sawCall, sawResp bool
	for _, c := range env.Request.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				sawCall = true
				if p.FunctionCall.Name != "Shell_ide" {
					t.Fatalf("call name %q", p.FunctionCall.Name)
				}
				if p.ThoughtSignature == "" {
					t.Fatal("missing thoughtSignature")
				}
				if c.Role != "model" {
					t.Fatalf("call role %q", c.Role)
				}
			}
			if p.FunctionResponse != nil {
				sawResp = true
				if p.FunctionResponse.Name != "Shell_ide" {
					t.Fatalf("resp name %q", p.FunctionResponse.Name)
				}
				if c.Role != "user" {
					t.Fatalf("resp role %q", c.Role)
				}
			}
		}
	}
	if !sawCall || !sawResp {
		t.Fatalf("call=%v resp=%v contents=%s", sawCall, sawResp, string(body))
	}
}

func TestEncodeRequestMaxTokensClamp(t *testing.T) {
	body, err := EncodeRequest(&ir.Request{Model: "m", MaxTokens: 999999, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "x"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(body, &env)
	if env.Request.GenerationConfig.MaxOutputTokens != MaxOutputTokens {
		t.Fatalf("got %d", env.Request.GenerationConfig.MaxOutputTokens)
	}
}

func TestSanitizeFunctionName(t *testing.T) {
	if g := sanitizeFunctionName("foo bar"); g != "foo_bar" {
		t.Fatal(g)
	}
	if g := sanitizeFunctionName("9bad"); !strings.HasPrefix(g, "_") {
		t.Fatal(g)
	}
}

func TestInjectProjectID(t *testing.T) {
	body, _ := EncodeRequest(&ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "x"}}},
	}})
	out, err := InjectProjectID(body, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(out, &env)
	if env.Project != "proj-1" {
		t.Fatalf("%q", env.Project)
	}
	if _, err := InjectProjectID(body, ""); err == nil {
		t.Fatal("expected error on empty project")
	}
}
