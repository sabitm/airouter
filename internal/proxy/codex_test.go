package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
)

func TestCodexEncodeRequestEnvelope(t *testing.T) {
	req := &ir.Request{
		Model:  "gpt-5.3-codex-high",
		System: "",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "hi"},
		}}},
		Stream: true,
	}
	body, err := responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	body = prepareCodexRequest(WithTraceInfo(context.Background(), &TraceInfo{}), codexCodec, body)

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "gpt-5.3-codex" {
		t.Errorf("model = %v", got["model"])
	}
	if got["store"] != false || got["instructions"] == "" || got["prompt_cache_key"] == "" {
		t.Errorf("codex envelope missing required fields: %s", body)
	}
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Errorf("reasoning = %+v", reasoning)
	}
	if _, ok := got["temperature"]; ok {
		t.Errorf("temperature should be stripped: %s", body)
	}
}

func TestCodexEncodeRequestEffortNoneOmitsReasoningInclude(t *testing.T) {
	body, err := responses.EncodeCodexRequest(&ir.Request{Model: "gpt-5.3-codex-none"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "gpt-5.3-codex" {
		t.Errorf("model = %v", got["model"])
	}
	if _, ok := got["reasoning"]; ok {
		t.Errorf("reasoning should be omitted for effort none: %s", body)
	}
	if _, ok := got["include"]; ok {
		t.Errorf("include should be omitted for effort none: %s", body)
	}
}

func TestApplyCodexHeaders(t *testing.T) {
	trace := &TraceInfo{CodexSessionID: "sess-1"}
	ctx := WithTraceInfo(context.Background(), trace)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{
		Protocol: domain.ProtocolOpenAICodex,
		APIKey:   "tok",
		OAuthCreds: &domain.OAuthCreds{
			AccountID: "acct-1",
		},
	}
	clientHeaders := http.Header{"User-Agent": []string{"bad-client"}}
	applyUpstreamHeaders(req, provider, clientHeaders, ctx)

	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("authorization = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "codex_cli_rs/"+responses.CodexCLIVersion {
		t.Errorf("user-agent = %q", got)
	}
	if got := req.Header.Get("originator"); got != "codex_cli_rs" {
		t.Errorf("originator = %q", got)
	}
	if got := req.Header.Get("session_id"); got != "sess-1" {
		t.Errorf("session_id = %q", got)
	}
	if got := req.Header.Get("chatgpt-account-id"); got != "acct-1" {
		t.Errorf("chatgpt-account-id = %q", got)
	}
}
