package antigravity

import "testing"

func TestCloakToolsSuffixAndDecoys(t *testing.T) {
	req := &geminiRequest{
		Tools: []geminiToolGroup{{
			FunctionDeclarations: []functionDecl{
				{Name: "Shell", Description: "run"},
				{Name: "view_file", Description: "native"},
			},
		}},
		Contents: []geminiContent{{
			Role: "model",
			Parts: []geminiPart{
				{FunctionCall: &functionCall{Name: "Shell", Args: map[string]any{"c": "ls"}}},
			},
		}, {
			Role: "user",
			Parts: []geminiPart{
				{FunctionResponse: &functionResponse{Name: "Shell", Response: map[string]any{"result": "ok"}}},
			},
		}},
	}
	m := CloakTools(req)
	if m["Shell_ide"] != "Shell" {
		t.Fatalf("map: %+v", m)
	}
	decls := req.Tools[0].FunctionDeclarations
	if decls[0].Name != "Shell_ide" {
		t.Fatalf("first decl %q", decls[0].Name)
	}
	if decls[1].Name != "view_file" {
		t.Fatalf("native should stay: %q", decls[1].Name)
	}
	// Decoys appended
	foundRun := false
	for _, d := range decls {
		if d.Name == "run_command" {
			foundRun = true
			if d.Description != "This tool is currently unavailable." {
				t.Fatalf("decoy desc: %q", d.Description)
			}
		}
	}
	if !foundRun {
		t.Fatal("expected run_command decoy")
	}
	if req.Contents[0].Parts[0].FunctionCall.Name != "Shell_ide" {
		t.Fatalf("history call: %q", req.Contents[0].Parts[0].FunctionCall.Name)
	}
	if req.Contents[1].Parts[0].FunctionResponse.Name != "Shell_ide" {
		t.Fatalf("history resp: %q", req.Contents[1].Parts[0].FunctionResponse.Name)
	}
}

func TestCloakToolsNoToolsNoOp(t *testing.T) {
	req := &geminiRequest{Contents: []geminiContent{{Role: "user", Parts: []geminiPart{{Text: "hi"}}}}}
	if CloakTools(req) != nil {
		t.Fatal("expected nil map")
	}
	if len(req.Tools) != 0 {
		t.Fatal("should not inject decoys alone")
	}
}

func TestDecloakName(t *testing.T) {
	if got := DecloakName("Shell_ide"); got != "Shell" {
		t.Fatalf("got %q", got)
	}
	if got := DecloakName("view_file"); got != "view_file" {
		t.Fatalf("native: %q", got)
	}
	if got := DecloakName("x_ide_ide"); got != "x_ide" {
		t.Fatalf("double suffix strip once: %q", got)
	}
}
