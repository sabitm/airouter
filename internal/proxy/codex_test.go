package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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
	body, err = prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), nil, codexCodec, &domain.Provider{}, nil, body)
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
	body, err = prepareUpstreamRequest(context.Background(), nil, antigravityCodec, provider, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if json.Unmarshal(body, &env) != nil || env["project"] != "proj-1" {
		t.Fatalf("project inject: %+v", env)
	}

	req, err := http.NewRequest(http.MethodPost, antigravity.DefaultBaseURL+antigravity.UpstreamPathStream, bytes.NewReader(body))
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
	if _, err := prepareUpstreamRequest(context.Background(), nil, antigravityCodec, provider, nil, body); err == nil {
		t.Fatal("expected missing project error")
	}
}

func TestApplyCursorHeaders(t *testing.T) {
	body, err := cursor.EncodeAgentRequest(&ir.Request{
		Model:    "default",
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
	req, err := http.NewRequest(http.MethodPost, cursor.AgentRunURL, bytes.NewReader(body))
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
	if got := req.Header.Get("X-Cursor-Client-Type"); got != cursor.ClientType {
		t.Errorf("client-type = %q", got)
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

func TestCodexFinalizePreservesToolSchemaNumbers(t *testing.T) {
	req := &ir.Request{
		Model: "gpt-5.3-codex",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "hi"},
		}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
		Tools: []ir.Tool{{
			Name:       "t",
			Parameters: json.RawMessage(`{"type":"object","properties":{"n":{"maximum":9007199254740993,"huge":1e400}}}`),
		}},
	}
	body, err := responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := finalizeEncodedBody(body, req, codexCodec, &domain.Provider{Protocol: domain.ProtocolOpenAICodex})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("9007199254740993")) {
		t.Fatalf("lost integer token: %s", out)
	}
	if !bytes.Contains(out, []byte("1e400")) {
		t.Fatalf("lost 1e400 token: %s", out)
	}
	var got struct {
		Include   []string `json:"include"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %+v", got.Reasoning)
	}
	found := false
	for _, item := range got.Include {
		if item == "reasoning.encrypted_content" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing encrypted include: %s", out)
	}
}

func TestCodexFinalizeThenPreparePreservesNumbersAndPairsTrace(t *testing.T) {
	req := &ir.Request{
		Model: "gpt-5.3-codex",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockText, Text: "hi"},
		}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
		Tools: []ir.Tool{{
			Name:       "t",
			Parameters: json.RawMessage(`{"maximum":9007199254740993,"huge":1e400}`),
		}},
	}
	body, err := responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	provider := &domain.Provider{Protocol: domain.ProtocolOpenAICodex}
	body, err = finalizeEncodedBody(body, req, codexCodec, provider)
	if err != nil {
		t.Fatal(err)
	}
	trace := &TraceInfo{}
	out, err := prepareUpstreamRequest(WithTraceInfo(context.Background(), trace), nil, codexCodec, provider, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("9007199254740993")) {
		t.Fatalf("lost integer token: %s", out)
	}
	if !bytes.Contains(out, []byte("1e400")) {
		t.Fatalf("lost 1e400 token: %s", out)
	}
	var got struct {
		Key       string   `json:"prompt_cache_key"`
		Include   []string `json:"include"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Reasoning.Effort != "high" {
		t.Fatalf("effort = %q", got.Reasoning.Effort)
	}
	found := false
	for _, item := range got.Include {
		if item == "reasoning.encrypted_content" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing encrypted include: %s", out)
	}
	if got.Key == "" || got.Key != trace.CodexSessionID {
		t.Fatalf("prompt_cache_key=%q trace=%q", got.Key, trace.CodexSessionID)
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

func TestCodexFinalizeNormalizesUnsupportedMax(t *testing.T) {
	req := &ir.Request{
		Model:    "gpt-5.3-codex",
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "max"},
	}
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
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("reasoning = %v, want xhigh; body=%s", reasoning, out)
	}
}

// TestCodexIgnoresResponsesStoreInclude decodes a raw Responses request that
// sets store and include, then runs the actual Codex encode/finalize/prepare
// path. The Codex envelope must keep store=false, forward only its required
// encrypted include when effort is not none, and keep the generated scoped
// prompt_cache_key instead of the explicit raw key.
func TestCodexIgnoresResponsesStoreInclude(t *testing.T) {
	const (
		explicitKey   = "client-raw-key"
		customInclude = "custom.include.sentinel"
	)
	cases := []struct {
		name        string
		upstream    string
		reasoning   string // raw JSON member, empty means absent
		wantInclude bool
	}{
		{name: "high", upstream: "gpt-5.3-codex", reasoning: `,"reasoning":{"effort":"high"}`, wantInclude: true},
		{name: "none native suffix", upstream: "gpt-5.3-codex-none", wantInclude: false},
		{name: "none supported model", upstream: "gpt-5.6-luna", reasoning: `,"reasoning":{"effort":"none"}`, wantInclude: false},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"model":"combo","stream":%t,"input":"hi","store":true,"include":["%s","reasoning.encrypted_content"],"prompt_cache_key":"%s"%s}`, stream, customInclude, explicitKey, tc.reasoning))
				req, err := responsesCodec.decodeRequest(raw)
				if err != nil {
					t.Fatal(err)
				}
				if req.Store == nil || !*req.Store {
					t.Fatalf("decode lost store intent: %v", req.Store)
				}
				if req.Include == nil || len(*req.Include) != 2 || (*req.Include)[0] != customInclude {
					t.Fatalf("decode lost include intent: %#v", req.Include)
				}
				applyUpstreamModel(req, tc.upstream)
				backend := codexCodec
				encoded, err := backend.encodeRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(encoded, []byte(customInclude)) {
					t.Fatalf("encoder forwarded the caller include: %s", encoded)
				}
				var encodedFields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &encodedFields); err != nil {
					t.Fatal(err)
				}
				if string(encodedFields["store"]) != "false" {
					t.Fatalf("encoded store = %s, want false", encodedFields["store"])
				}
				if tc.wantInclude {
					if string(encodedFields["include"]) != `["reasoning.encrypted_content"]` {
						t.Fatalf("encoded include = %s", encodedFields["include"])
					}
				} else if _, ok := encodedFields["include"]; ok {
					t.Fatalf("none encoded include: %s", encodedFields["include"])
				}

				p := &domain.Provider{Name: "codex", Protocol: domain.ProtocolOpenAICodex}
				finalized, err := finalizeEncodedBody(encoded, req, backend, p)
				if err != nil {
					t.Fatal(err)
				}
				trace := &TraceInfo{}
				ctx := WithTraceInfo(context.Background(), trace)
				ctx = withCodexRequest(ctx, "tenant-x", http.Header{}, raw)
				out, err := New(nil, nil).prepareUpstreamRequestFor(ctx, backend, p, req, finalized)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(out, []byte(customInclude)) {
					t.Fatalf("prepare forwarded the caller include: %s", out)
				}
				var got struct {
					Stream  bool     `json:"stream"`
					Store   *bool    `json:"store"`
					Include []string `json:"include"`
					Key     string   `json:"prompt_cache_key"`
				}
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatal(err)
				}
				if got.Store == nil || *got.Store {
					t.Fatalf("prepared store = %v, want false", got.Store)
				}
				if got.Stream != stream {
					t.Fatalf("prepared stream = %v, want %v", got.Stream, stream)
				}
				if tc.wantInclude {
					if !reflect.DeepEqual(got.Include, []string{"reasoning.encrypted_content"}) {
						t.Fatalf("prepared include = %#v", got.Include)
					}
				} else if len(got.Include) != 0 {
					t.Fatalf("prepared include = %#v, want omitted", got.Include)
				}
				if got.Key == "" || got.Key == explicitKey {
					t.Fatalf("prepared key = %q, want a generated scoped key", got.Key)
				}
				if trace.CodexSessionID != got.Key {
					t.Fatalf("trace session %q != body key %q", trace.CodexSessionID, got.Key)
				}
				httpReq, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", bytes.NewReader(out))
				if err != nil {
					t.Fatal(err)
				}
				applyCodexHeaders(httpReq, p, ctx)
				if httpReq.Header.Get("session_id") != got.Key {
					t.Fatalf("session_id = %q, want %q", httpReq.Header.Get("session_id"), got.Key)
				}
			})
		}
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

func TestCodexEncodeFinalizeAutoAndUltra(t *testing.T) {
	provider := &domain.Provider{Protocol: domain.ProtocolOpenAICodex}
	cases := []struct {
		name       string
		upstream   string
		thinking   *ir.Thinking
		wantModel  string
		wantEffort string
	}{
		{
			name:       "explicit auto becomes medium",
			upstream:   "gpt-5.3-codex",
			thinking:   &ir.Thinking{Mode: ir.ThinkingAuto},
			wantModel:  "gpt-5.3-codex",
			wantEffort: "medium",
		},
		{
			name:       "sol ultra suffix",
			upstream:   "gpt-5.6-sol(ultra)",
			wantModel:  "gpt-5.6-sol",
			wantEffort: "ultra",
		},
		{
			name:       "terra ultra suffix",
			upstream:   "gpt-5.6-terra(ultra)",
			wantModel:  "gpt-5.6-terra",
			wantEffort: "ultra",
		},
		{
			name:       "luna ultra suffix",
			upstream:   "gpt-5.6-luna(ultra)",
			wantModel:  "gpt-5.6-luna",
			wantEffort: "max",
		},
		{
			name:       "classic ultra suffix",
			upstream:   "gpt-5.3-codex(ultra)",
			wantModel:  "gpt-5.3-codex",
			wantEffort: "xhigh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model:    "combo",
				Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
				Thinking: tc.thinking,
			}
			applyUpstreamModel(req, tc.upstream)
			body, err := responses.EncodeCodexRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			out, err := finalizeEncodedBody(body, req, codexCodec, provider)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got["model"] != tc.wantModel {
				t.Fatalf("model = %v, want %q; body=%s", got["model"], tc.wantModel, out)
			}
			reasoning, _ := got["reasoning"].(map[string]any)
			if reasoning["effort"] != tc.wantEffort {
				t.Fatalf("reasoning = %v, want effort %q; body=%s", reasoning, tc.wantEffort, out)
			}
			include, _ := got["include"].([]any)
			found := false
			for _, item := range include {
				if item == "reasoning.encrypted_content" {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("missing reasoning.encrypted_content: %s", out)
			}
		})
	}
}

// TestCodexEnvelopeHistoryScopedKey verifies the Codex wire envelope at once:
// store is forced false, include keeps only the effective encrypted-reasoning
// entry, the history input bytes survive preparation exactly, and the generated
// cache key is the same in the body, the session_id header, and the trace even
// when the caller sent a conflicting envelope.
func TestCodexEnvelopeHistoryScopedKey(t *testing.T) {
	raw := []byte(`{"model":"combo","stream":true,"input":[` +
		`{"type":"reasoning","id":"rs_hist","status":"completed","encrypted_content":"cipher-hist","summary":[{"type":"summary_text","text":"prior chain"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"prior answer"}]}` +
		`],"store":true,"include":["custom.include.sentinel"],"prompt_cache_key":"caller-key","reasoning":{"effort":"high"}}`)
	req, err := responsesCodec.decodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.Include == nil || len(*req.Include) != 1 || (*req.Include)[0] != "custom.include.sentinel" {
		t.Fatalf("decode lost include intent: %#v", req.Include)
	}
	applyUpstreamModel(req, "gpt-5.3-codex")
	before := snapshotIR(t, req)
	p := &domain.Provider{Name: "codex", Protocol: domain.ProtocolOpenAICodex}
	encoded, err := codexCodec.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("custom.include.sentinel")) {
		t.Fatalf("encoder forwarded the caller include: %s", encoded)
	}
	finalized, err := finalizeEncodedBody(encoded, req, codexCodec, p)
	if err != nil {
		t.Fatal(err)
	}
	trace := &TraceInfo{}
	ctx := WithTraceInfo(context.Background(), trace)
	h := http.Header{}
	h.Set("session_id", "conflicting-header-session")
	h.Set("prompt_cache_key", "conflicting-header-key")
	ctx = withCodexRequest(ctx, "tenant-x", h, raw)
	out, err := New(nil, nil).prepareUpstreamRequestFor(ctx, codexCodec, p, req, finalized)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshotIR(t, req), before) {
		t.Fatal("codex preparation mutated the decoded request")
	}
	var encFields, outFields struct {
		Input   json.RawMessage `json:"input"`
		Store   *bool           `json:"store"`
		Include []string        `json:"include"`
		Key     string          `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(encoded, &encFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &outFields); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encFields.Input, outFields.Input) {
		t.Fatalf("history changed during preparation:\n%s\n%s", encFields.Input, outFields.Input)
	}
	if outFields.Store == nil || *outFields.Store {
		t.Fatalf("prepared store = %v, want false", outFields.Store)
	}
	if !reflect.DeepEqual(outFields.Include, []string{"reasoning.encrypted_content"}) {
		t.Fatalf("prepared include = %#v", outFields.Include)
	}
	if outFields.Key == "" || outFields.Key == "caller-key" || outFields.Key == "conflicting-header-key" {
		t.Fatalf("prepared key = %q, want a generated scoped key", outFields.Key)
	}
	if trace.CodexSessionID != outFields.Key {
		t.Fatalf("trace key %q != body key %q", trace.CodexSessionID, outFields.Key)
	}
	httpReq, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	applyCodexHeaders(httpReq, p, ctx)
	if got := httpReq.Header.Get("session_id"); got != outFields.Key {
		t.Fatalf("session_id = %q, want %q", got, outFields.Key)
	}
	var input []map[string]any
	if err := json.Unmarshal(outFields.Input, &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 3 {
		t.Fatalf("input items = %d", len(input))
	}
	if input[0]["id"] != "rs_hist" || input[0]["encrypted_content"] != "cipher-hist" {
		t.Fatalf("reasoning history lost state: %#v", input[0])
	}
}
