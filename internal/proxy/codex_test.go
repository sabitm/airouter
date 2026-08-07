package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/qoder"
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
	body, err = prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), codexCodec, &domain.Provider{}, body)
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
	applyUpstreamHeaders(req, provider, clientHeaders, ctx, nil)

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

func TestApplyClineHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.cline.bot/api/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{
		Protocol: domain.ProtocolOpenAI,
		APIKey:   "raw-token",
		OAuthCreds: &domain.OAuthCreds{
			ClineAuth: true,
		},
	}
	clientHeaders := http.Header{"User-Agent": []string{"bad-client"}}
	applyUpstreamHeaders(req, provider, clientHeaders, context.Background(), nil)

	if got := req.Header.Get("Authorization"); got != "Bearer workos:raw-token" {
		t.Errorf("authorization = %q", got)
	}
	if got := req.Header.Get("HTTP-Referer"); got != "https://cline.bot" {
		t.Errorf("referer = %q", got)
	}
	if got := req.Header.Get("X-Title"); got != "Cline" {
		t.Errorf("x-title = %q", got)
	}
	if got := req.Header.Get("X-CLIENT-TYPE"); got != "airouter" {
		t.Errorf("x-client-type = %q", got)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "airouter/") {
		t.Errorf("user-agent = %q, want airouter/ prefix", got)
	}
}

func TestApplyQoderHeaders(t *testing.T) {
	body := []byte(`{"stream":true}`)
	req, err := http.NewRequest(http.MethodPost, qoder.ChatURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{
		Protocol: domain.ProtocolQoder,
		APIKey:   "dt-token",
		OAuthCreds: &domain.OAuthCreds{
			QoderAuth:   true,
			UserID:      "u-1",
			MachineID:   "m-1",
			DisplayName: "Ada",
			Email:       "a@b.c",
		},
	}
	ctx := WithTraceInfo(context.Background(), &TraceInfo{
		QoderModelKey:    "auto",
		QoderModelSource: "system",
	})
	applyUpstreamHeaders(req, provider, nil, ctx, body)

	if auth := req.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Fatalf("authorization = %q", auth)
	}
	if got := req.Header.Get("Cosy-User"); got != "u-1" {
		t.Errorf("cosy-user = %q", got)
	}
	if got := req.Header.Get("Cosy-Machineid"); got != "m-1" {
		t.Errorf("cosy-machineid = %q", got)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("accept-encoding = %q", got)
	}
	if got := req.Header.Get("X-Model-Key"); got != "auto" {
		t.Errorf("x-model-key = %q", got)
	}
	if got := req.Header.Get("X-Model-Source"); got != "system" {
		t.Errorf("x-model-source = %q", got)
	}
}

func TestApplyAntigravityHeadersAndProject(t *testing.T) {
	body, err := antigravity.EncodeRequest(&ir.Request{
		Model: "gemini-3-flash",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{
		Protocol: domain.ProtocolAntigravity,
		APIKey:   "tok",
		OAuthCreds: &domain.OAuthCreds{
			AntigravityAuth: true,
			ProjectID:       "proj-1",
		},
	}
	body, err = prepareUpstreamRequest(context.Background(), antigravityCodec, provider, body)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if json.Unmarshal(body, &env) != nil || env["project"] != "proj-1" {
		t.Fatalf("project inject: %+v", env)
	}

	req, err := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "client/1")
	applyUpstreamHeaders(req, provider, http.Header{"User-Agent": {"client/1"}}, context.Background(), body)
	if got := req.Header.Get("User-Agent"); got != antigravity.UserAgent {
		t.Fatalf("user-agent = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("auth = %q", got)
	}

	// Missing project is terminal.
	provider.OAuthCreds.ProjectID = ""
	if _, err := prepareUpstreamRequest(context.Background(), antigravityCodec, provider, body); err == nil {
		t.Fatal("expected missing project error")
	}
}

func TestApplyCursorHeaders(t *testing.T) {
	body, err := cursor.EncodeRequest(&ir.Request{
		Model:    "gpt-5.2",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{
		Protocol: domain.ProtocolCursor,
		APIKey:   "sess::ide-tok",
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true,
			MachineID:  "m-1",
		},
	}
	req, err := http.NewRequest(http.MethodPost, cursor.DefaultBaseURL+cursor.UpstreamPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// A forwarded client User-Agent must be overwritten by the Cursor identity set.
	applyUpstreamHeaders(req, provider, http.Header{"User-Agent": {"bad-client"}}, context.Background(), body)
	if got := req.Header.Get("Authorization"); got != "Bearer ide-tok" {
		t.Errorf("authorization = %q, want stripped Bearer ide-tok", got)
	}
	if got := req.Header.Get("Content-Type"); got != cursor.ConnectContentType {
		t.Errorf("content-type = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != cursor.UserAgent {
		t.Errorf("user-agent = %q, want %s (identity overwrite)", got, cursor.UserAgent)
	}
	if got := req.Header.Get("X-Cursor-Checksum"); !strings.HasSuffix(got, "m-1") {
		t.Errorf("checksum = %q, want suffix m-1", got)
	}
	if got := req.Header.Get("X-Cursor-Client-Version"); got != cursor.ClientVersion {
		t.Errorf("client-version = %q", got)
	}
	if got := req.Header.Get("X-Session-Id"); got == "" {
		t.Error("session-id empty")
	}
	if got := req.Header.Get("Connect-Protocol-Version"); got != "1" {
		t.Errorf("connect-protocol-version = %q", got)
	}
}

func TestCodexFinalizeKeepsNativeHyphenAndSyncsInclude(t *testing.T) {
	req := &ir.Request{Model: "gpt-5.3-codex-high"}
	body, err := responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{Protocol: domain.ProtocolOpenAICodex}
	out, err := finalizeEncodedBody(body, req, codexCodec, provider)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := got["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %v", reasoning)
	}

	req.Model = "gpt-5.6-luna"
	req.Thinking = &ir.Thinking{Mode: ir.ThinkingNone}
	body, err = responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err = finalizeEncodedBody(body, req, codexCodec, provider)
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["include"]; ok {
		t.Fatalf("none retained encrypted include: %s", out)
	}
}

func TestCodexEncodeRequestIRThinkingOverridesHyphen(t *testing.T) {
	body, err := responses.EncodeCodexRequest(&ir.Request{
		Model:    "gpt-5.3-codex-low",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
	})
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
	r, _ := got["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "high" {
		t.Fatalf("reasoning = %v", got["reasoning"])
	}
}

func TestCodexEncodeRequestIRThinkingMaxMinimalPassThrough(t *testing.T) {
	for _, lvl := range []string{"max", "minimal"} {
		body, err := responses.EncodeCodexRequest(&ir.Request{
			Model:    "gpt-5.3-codex",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
			Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: lvl},
		})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		r, _ := got["reasoning"].(map[string]any)
		if r == nil || r["effort"] != lvl {
			t.Fatalf("level %q: reasoning = %v", lvl, got["reasoning"])
		}
	}
}
