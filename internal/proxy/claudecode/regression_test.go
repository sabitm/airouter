package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPreparePreservesFieldsAndDoesNotLeakBilling(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.303.abc; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"be brief","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hello world, this is a test"}],"context_management":{"edits":[]},"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}},"thinking":{"type":"adaptive","display":"summarized"},"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"tools":[{"name":"t","input_schema":{"type":"object"},"strict":true,"defer_loading":true,"eager_input_streaming":true}]}`)
	p := PrepareInput{SessionID: "11111111-1111-4111-8111-111111111111", DeviceID: strings.Repeat("ab", 32), AccountUUID: "acct", Profile: DefaultProfile(), BaseURL: "https://api.anthropic.com", CacheDefault: true}
	one, err := Prepare(in, p)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Prepare(one, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"context_management", "json_schema", "summarized", "disable_parallel_tool_use", "strict", "defer_loading", "eager_input_streaming", "cch=00000;", `"ttl":"1h"`} {
		if !strings.Contains(string(two), want) {
			t.Fatalf("lost %s in %s", want, two)
		}
	}
	if strings.Count(string(two), "cc_entrypoint") != 1 {
		t.Fatalf("billing leaked or duplicated: %s", two)
	}
	var doc map[string]any
	if json.Unmarshal(two, &doc) != nil {
		t.Fatal("prepared body is not JSON")
	}
	sys := doc["system"].([]any)
	if len(sys) < 3 {
		t.Fatalf("system = %#v", sys)
	}
	if sys[2].(map[string]any)["text"] != "be brief" {
		t.Fatalf("caller block = %#v", sys[2])
	}
}

func TestProfileUserAgentKeepsSuffixesInsideParentheses(t *testing.T) {
	p := Profile{Entrypoint: "sdk-cli", SDK: true, AgentSDKVersion: "1.2.3", ClientApp: "app", Workload: "w"}
	ua := p.UserAgent()
	if !strings.HasPrefix(ua, "claude-cli/"+CLIVersion+" (external, sdk-cli, agent-sdk/1.2.3, client-app/app, workload/w)") || !strings.HasSuffix(ua, ")") {
		t.Fatalf("ua = %s", ua)
	}
	if p.IdentityLine() != IdentityCLI {
		t.Fatal("SDK suffix alone selected a non-CLI identity")
	}
	p.NonInteractive = true
	if p.IdentityLine() != IdentityAgent {
		t.Fatalf("noninteractive identity = %s", p.IdentityLine())
	}
	p.HasAppendSystemPrompt = true
	if p.IdentityLine() != IdentitySDK {
		t.Fatalf("append identity = %s", p.IdentityLine())
	}
}

func TestCatalogDoesNotInventMythosOrHaiku(t *testing.T) {
	mythos, ok := LookupModel("claude-mythos-5")
	if !ok || mythos.Adaptive || mythos.RejectsDisabled || mythos.Effort {
		t.Fatalf("mythos = %+v ok=%v", mythos, ok)
	}
	haiku, ok := LookupModel("claude-haiku-4-5")
	if !ok || haiku.Adaptive {
		t.Fatalf("haiku = %+v", haiku)
	}
	haiku55, ok := LookupModel("claude-haiku-5-5")
	if !ok || !haiku55.Adaptive || !haiku55.RejectsDisabled || !haiku55.XHigh {
		t.Fatalf("haiku 5.5 = %+v", haiku55)
	}
}
