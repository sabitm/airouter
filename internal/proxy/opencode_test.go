package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/thinking"
)

// thinkingCapsFor resolves caps through the shared thinking package.
func thinkingCapsFor(t *testing.T, model string) thinking.Caps {
	t.Helper()
	return thinking.CapsFor(model, domain.ProtocolOpencode, domain.ReasoningOpencode)
}

func opencodeTestProvider() *domain.Provider {
	return &domain.Provider{
		Name:     "opencode zen",
		BaseURL:  opencode.ZenBaseURL,
		APIKey:   opencode.PublicKey,
		Protocol: domain.ProtocolOpencode,
	}
}

func setupOpencodeTranslated(t *testing.T, handler http.HandlerFunc) (base, token string, mux *http.ServeMux) {
	t.Helper()
	return setupOpencodeTranslatedModel(t, "big-pickle", handler)
}

func setupOpencodeTranslatedModel(t *testing.T, upstreamModel string, handler http.HandlerFunc) (base, token string, mux *http.ServeMux) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: upstreamModel, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux = http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, mux
}

func TestOpencodeBackendCodecPerModel(t *testing.T) {
	chat := backendCodecFor(domain.ProtocolOpencode, opencode.ZenBaseURL, "big-pickle")
	if chat.id != "opencode-chat" {
		t.Fatalf("big-pickle codec = %q", chat.id)
	}
	if chat.upstreamPath != opencode.ChatPath {
		t.Fatalf("chat path = %q", chat.upstreamPath)
	}
	resp := backendCodecFor(domain.ProtocolOpencode, opencode.ZenBaseURL, "muse-spark-1.2-contributor-free")
	if resp.id != "opencode-responses" {
		t.Fatalf("muse-spark codec = %q", resp.id)
	}
	if resp.upstreamPath != opencode.ResponsesPath {
		t.Fatalf("responses path = %q", resp.upstreamPath)
	}
	msg := backendCodecFor(domain.ProtocolOpencode, opencode.ZenBaseURL, "qwen3.6-plus")
	if msg.id != "opencode-messages" || msg.upstreamPath != opencode.MessagesPath {
		t.Fatalf("qwen codec = %q path %q", msg.id, msg.upstreamPath)
	}
	goMini := backendCodecFor(domain.ProtocolOpencode, opencode.GoBaseURL, "minimax-m3")
	if goMini.id != "opencode-messages" {
		t.Fatalf("go minimax codec = %q, want messages", goMini.id)
	}
}

// The distinct codec ids must prevent passthrough for both ingress formats so
// the fingerprint headers and model patches are always applied.
func TestOpencodeNeverPassthrough(t *testing.T) {
	for _, ing := range []codec{openaiCodec, responsesCodec} {
		for _, model := range []string{"big-pickle", "muse-spark-1.2-contributor-free"} {
			if ing.id == backendCodec(domain.ProtocolOpencode, model).id {
				t.Fatalf("ingress %q passes through to opencode model %q", ing.id, model)
			}
		}
	}
}

func TestOpencodePrepareUpstreamRequestChat(t *testing.T) {
	px := New(nil, nil)
	p := opencodeTestProvider()
	p.ID = 7
	body := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`)
	trace := &TraceInfo{}
	out, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), trace), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), p, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"reasoning_content":" "`) {
		t.Fatalf("deepseek chat body missing reasoning echo: %s", out)
	}
	if !opencode.IsNativeSessionID(trace.OpencodeSessionID) {
		t.Fatalf("trace session id = %q", trace.OpencodeSessionID)
	}
	// Session id is conversation-stable: same Proxy+provider+body derives the same value.
	trace2 := &TraceInfo{}
	out2, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), trace2), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), p, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("same conversation produced different bodies: %s vs %s", out, out2)
	}
	if trace.OpencodeSessionID != trace2.OpencodeSessionID {
		t.Fatalf("same conversation produced different sessions: %q vs %q", trace.OpencodeSessionID, trace2.OpencodeSessionID)
	}
}

func TestOpencodePrepareUpstreamRequestMuseSpark(t *testing.T) {
	p := opencodeTestProvider()
	body := []byte(`{"model":"muse-spark-1.2-contributor-free","reasoning":{"effort":"max"},"max_output_tokens":400}`)
	out, err := New(nil, nil).prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backendCodec(domain.ProtocolOpencode, "muse-spark-1.2-contributor-free"), p, body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	r, _ := got["reasoning"].(map[string]any)
	if r == nil || r["effort"] != "xhigh" || r["summary"] != "auto" {
		t.Fatalf("muse-spark reasoning = %+v", r)
	}
}

// The end-to-end translated unary path through the deepseek body: decode chat,
// encode chat, finalize with the opencode dialect, then prepare.
func TestOpencodeTranslatedDeepseekFinalize(t *testing.T) {
	p := opencodeTestProvider()
	ing := openaiCodec
	body := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	req, err := ing.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	applyUpstreamModel(req, "deepseek-v4-pro")
	backend := backendCodec(p.Protocol, "deepseek-v4-pro")
	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = New(nil, nil).prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(upstreamBody, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "deepseek-v4-pro" {
		t.Fatalf("model = %v", got["model"])
	}
	// Catalog effort is high|max. high stays high. Toggle is not also written.
	if got["reasoning_effort"] != "high" {
		t.Fatalf("deepseek effort = %v, want high", got["reasoning_effort"])
	}
	if _, ok := got["thinking"]; ok {
		t.Fatalf("thinking leaked: %v", got["thinking"])
	}
}

func TestOpencodeTranslatedReasoningEchoPreservesRealContent(t *testing.T) {
	p := opencodeTestProvider()
	body := []byte(`{"model":"combo","messages":[{"role":"assistant","content":"answer","reasoning_content":"real chain"},{"role":"user","content":"next"}]}`)
	req, err := openaiCodec.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	applyUpstreamModel(req, "deepseek-v4-pro")
	backend := backendCodec(p.Protocol, "deepseek-v4-pro")
	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = New(nil, nil).prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(upstreamBody, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].ReasoningContent != "real chain" {
		t.Fatalf("real reasoning was not preserved: %s", upstreamBody)
	}
}

func TestOpencodeUnaryResponseReasoningPreserved(t *testing.T) {
	backend := backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro")
	resp, err := backend.decodeResponse([]byte(`{"id":"chatcmpl-1","model":"deepseek-v4-pro","choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning_content":"chain"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := openaiCodec.encodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.ReasoningContent != "chain" {
		t.Fatalf("reasoning response lost: %s", out)
	}
}

func TestOpencodeStreamResponseReasoningPreserved(t *testing.T) {
	backend := backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro")
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"chain\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var reasoning string
	if err := backend.decodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventReasoningDelta {
			reasoning += ev.Text
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reasoning != "chain" {
		t.Fatalf("reasoning = %q", reasoning)
	}

	resp, err := collectStreamResponse(strings.NewReader(body), backend, nil, "deepseek-v4-pro", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockReasoning || resp.Content[0].Text != "chain" {
		t.Fatalf("collected content = %+v", resp.Content)
	}
	out, err := openai.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"reasoning_content":"chain"`) {
		t.Fatalf("collected unary response lost reasoning: %s", out)
	}
}

func TestApplyOpencodeHeaders(t *testing.T) {
	trace := &TraceInfo{OpencodeSessionID: "ses_abc"}
	req, _ := http.NewRequestWithContext(WithTraceInfo(context.Background(), trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4-pro"}`))
	// A forwarded client UA that is not opencode must be replaced. Paid Zen keeps
	// the desktop fingerprint and does not synthesize a session-id mirror.
	req.Header.Set("User-Agent", "some-agent/1.0")
	applyUpstreamHeaders(req, opencodeTestProvider(), http.Header{}, req.Context(), []byte(`{"model":"deepseek-v4-pro"}`))
	if got := req.Header.Get("User-Agent"); got != opencode.UserAgent {
		t.Fatalf("User-Agent = %q", got)
	}
	sid := req.Header.Get("x-opencode-session")
	if !opencode.IsNativeSessionID(sid) || sid == "ses_abc" {
		t.Fatalf("x-opencode-session = %q", sid)
	}
	if got := req.Header.Get("x-opencode-client"); got != "desktop" {
		t.Fatalf("x-opencode-client = %q", got)
	}
	if got := req.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project = %q", got)
	}
	if got := req.Header.Get("x-opencode-request"); !opencode.IsNativeRequestID(got) {
		t.Fatalf("x-opencode-request = %q", got)
	}
	if got := req.Header.Get(opencode.SessionIDHeader); got != "" {
		t.Fatalf("paid request synthesized %s: %q", opencode.SessionIDHeader, got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestApplyOpencodeFreeHeaders(t *testing.T) {
	native := "ses_0123456789abABCDEFGHIJKLMN"
	body := []byte(`{"model":"big-pickle"}`)
	trace := &TraceInfo{OpencodeSessionID: native}
	req, _ := http.NewRequestWithContext(WithTraceInfo(context.Background(), trace), http.MethodPost, opencode.ZenBaseURL+"/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Add(opencode.SessionIDHeader, "ses_0123456789abOTHERSESSIONID")
	req.Header.Add(opencode.SessionIDHeader, native+"\r\nX-Injected: 1")
	applyUpstreamHeaders(req, opencodeTestProvider(), nil, req.Context(), body)
	if got := req.Header.Get("User-Agent"); got != opencode.FreeUserAgent {
		t.Fatalf("free UA = %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != opencode.FreeClient {
		t.Fatalf("free client = %q", got)
	}
	if got := req.Header.Get("x-opencode-session"); got != native {
		t.Fatalf("session = %q", got)
	}
	if got := req.Header.Values(opencode.SessionIDHeader); len(got) != 1 || got[0] != native {
		t.Fatalf("session id mirror = %v", got)
	}

	goProvider := opencodeTestProvider()
	goProvider.BaseURL = opencode.GoBaseURL
	goReq, _ := http.NewRequest(http.MethodPost, opencode.GoBaseURL+"/chat/completions", nil)
	applyUpstreamHeaders(goReq, goProvider, nil, context.Background(), []byte(`{"model":"ox-alpha-free"}`))
	if goReq.Header.Get("User-Agent") != opencode.UserAgent || goReq.Header.Get("x-opencode-client") != "desktop" || goReq.Header.Get(opencode.SessionIDHeader) != "" {
		t.Fatalf("Go free-id identity changed: %v", goReq.Header)
	}

	other := &domain.Provider{Name: "openai", BaseURL: opencode.ZenBaseURL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	otherReq, _ := http.NewRequest(http.MethodPost, opencode.ZenBaseURL+"/chat/completions", nil)
	otherReq.Header.Set("User-Agent", "caller/1")
	applyUpstreamHeaders(otherReq, other, nil, context.Background(), body)
	if otherReq.Header.Get("User-Agent") != "caller/1" || otherReq.Header.Get("x-opencode-client") != "" || otherReq.Header.Get(opencode.SessionIDHeader) != "" {
		t.Fatalf("non-opencode protocol received OpenCode identity: %v", otherReq.Header)
	}
}

// A forwarded opencode client UA survives the header application.
func TestApplyOpencodeHeadersPreservesClientUA(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	clientHeaders := http.Header{}
	clientHeaders.Set("User-Agent", "opencode/0.16.7")
	clientHeaders.Set("x-opencode-client", "my-cli")
	applyUpstreamHeaders(req, opencodeTestProvider(), clientHeaders, context.Background(), nil)
	if got := req.Header.Get("User-Agent"); got != "opencode/0.16.7" {
		t.Fatalf("client UA clobbered: %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "my-cli" {
		t.Fatalf("client x-opencode-client clobbered: %q", got)
	}
}

func TestOpencodeChatAndResponsesShareSessionPolicy(t *testing.T) {
	px := New(nil, nil)
	p := opencodeTestProvider()
	p.ID = 3
	chatBody := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	respBody := []byte(`{"model":"muse-spark-1.3-contributor-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	chatTrace := &TraceInfo{}
	respTrace := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), chatTrace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, chatBody); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), respTrace), backendCodec(domain.ProtocolOpencode, "muse-spark-1.3-contributor-free"), p, respBody); err != nil {
		t.Fatal(err)
	}
	if chatTrace.OpencodeSessionID == "" || chatTrace.OpencodeSessionID != respTrace.OpencodeSessionID {
		t.Fatalf("free chat/responses first-turn sessions differ: %q vs %q", chatTrace.OpencodeSessionID, respTrace.OpencodeSessionID)
	}
	if !opencode.IsNativeSessionID(chatTrace.OpencodeSessionID) {
		t.Fatalf("free shared session = %q", chatTrace.OpencodeSessionID)
	}

	h := http.Header{}
	nativeShared := "ses_0123456789abABCDEFGHIJKLMN"
	h.Set("x-opencode-session", nativeShared)
	ctx := withOpencodeRequest(context.Background(), px.opencodeNonce, h, nil)
	chatTrace = &TraceInfo{}
	respTrace = &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, chatTrace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, chatBody); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, respTrace), backendCodec(domain.ProtocolOpencode, "muse-spark-1.3-contributor-free"), p, respBody); err != nil {
		t.Fatal(err)
	}
	if chatTrace.OpencodeSessionID != nativeShared || respTrace.OpencodeSessionID != nativeShared {
		t.Fatalf("explicit session not shared: %q vs %q", chatTrace.OpencodeSessionID, respTrace.OpencodeSessionID)
	}

	paidBody := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	goBody := []byte(`{"model":"minimax-m3","messages":[{"role":"user","content":"hi"}]}`)
	paid := opencodeTestProvider()
	paid.ID = 3
	goProvider := opencodeTestProvider()
	goProvider.ID = 3
	goProvider.BaseURL = opencode.GoBaseURL
	wantPaid := opencode.ResolveSession(opencode.Identity{}, px.opencodeNonce, paid.ID, paid.BaseURL, paid.APIKey, "")
	wantGo := opencode.ResolveSession(opencode.Identity{}, px.opencodeNonce, goProvider.ID, goProvider.BaseURL, goProvider.APIKey, "")
	paidTrace := &TraceInfo{}
	goTrace := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), paidTrace), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), paid, paidBody); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), goTrace), backendCodec(domain.ProtocolOpencode, "minimax-m3"), goProvider, goBody); err != nil {
		t.Fatal(err)
	}
	if paidTrace.OpencodeSessionID != wantPaid || goTrace.OpencodeSessionID != wantGo {
		t.Fatalf("paid/go legacy sessions changed: paid %q want %q go %q want %q", paidTrace.OpencodeSessionID, wantPaid, goTrace.OpencodeSessionID, wantGo)
	}
}

func TestOpencodeFallbackNamespacedByProxyAndProvider(t *testing.T) {
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	zenA := opencodeTestProvider()
	zenA.ID = 1
	zenB := opencodeTestProvider()
	zenB.ID = 2
	px := New(nil, nil)
	traceA := &TraceInfo{}
	traceB := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), traceA), backendCodec(domain.ProtocolOpencode, "big-pickle"), zenA, body); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), traceB), backendCodec(domain.ProtocolOpencode, "big-pickle"), zenB, body); err != nil {
		t.Fatal(err)
	}
	if traceA.OpencodeSessionID == "" || traceA.OpencodeSessionID == traceB.OpencodeSessionID {
		t.Fatalf("zen providers collided: %q vs %q", traceA.OpencodeSessionID, traceB.OpencodeSessionID)
	}
	px2 := New(nil, nil)
	traceC := &TraceInfo{}
	if _, err := px2.prepareUpstreamRequest(WithTraceInfo(context.Background(), traceC), backendCodec(domain.ProtocolOpencode, "big-pickle"), zenA, body); err != nil {
		t.Fatal(err)
	}
	if traceC.OpencodeSessionID == "" || traceC.OpencodeSessionID == traceA.OpencodeSessionID {
		t.Fatalf("proxy instances collided: %q vs %q", traceA.OpencodeSessionID, traceC.OpencodeSessionID)
	}
}

func TestOpencodeRequestIDFreshPerSendUnlessClientSupplied(t *testing.T) {
	px := New(nil, nil)
	p := opencodeTestProvider()
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	ctx := withOpencodeRequest(context.Background(), px.opencodeNonce, nil, body)
	trace := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	req1, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req1, p, nil, req1.Context(), body)
	req2, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req2, p, nil, req2.Context(), body)
	if !opencode.IsNativeSessionID(req1.Header.Get("x-opencode-session")) || req1.Header.Get("x-opencode-session") != req2.Header.Get("x-opencode-session") {
		t.Fatalf("session not stable across sends: %q vs %q", req1.Header.Get("x-opencode-session"), req2.Header.Get("x-opencode-session"))
	}
	if req1.Header.Get(opencode.SessionIDHeader) != req1.Header.Get("x-opencode-session") || req2.Header.Get(opencode.SessionIDHeader) != req2.Header.Get("x-opencode-session") {
		t.Fatalf("free session mirror drifted: %q %q", req1.Header.Get(opencode.SessionIDHeader), req2.Header.Get(opencode.SessionIDHeader))
	}
	if !opencode.IsNativeRequestID(req1.Header.Get("x-opencode-request")) || !opencode.IsNativeRequestID(req2.Header.Get("x-opencode-request")) || req1.Header.Get("x-opencode-request") == req2.Header.Get("x-opencode-request") {
		t.Fatalf("request id not fresh: %q vs %q", req1.Header.Get("x-opencode-request"), req2.Header.Get("x-opencode-request"))
	}

	nativeReq := "msg_0123456789abABCDEFGHIJKLMN"
	h := http.Header{}
	h.Set("x-opencode-request", nativeReq)
	ctx = withOpencodeRequest(context.Background(), px.opencodeNonce, h, body)
	trace = &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	req3, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req3, p, nil, req3.Context(), body)
	if req3.Header.Get("x-opencode-request") != nativeReq {
		t.Fatalf("client request id lost: %q", req3.Header.Get("x-opencode-request"))
	}
}

func TestApplyOpencodeHeadersFromCapturedIdentity(t *testing.T) {
	nativeReq := "msg_0123456789abABCDEFGHIJKLMN"
	nativeSes := "ses_0123456789abABCDEFGHIJKLMN"
	h := http.Header{}
	h.Set("User-Agent", "opencode/0.16.7")
	h.Set("x-opencode-client", "my-cli")
	h.Set("x-opencode-project", "proj-a")
	h.Set("x-opencode-request", nativeReq)
	h.Set("x-opencode-session", nativeSes)
	ctx := withOpencodeRequest(context.Background(), "nonce", h, nil)
	trace := &TraceInfo{}
	px := New(nil, nil)
	p := opencodeTestProvider()
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req, p, nil, req.Context(), body)
	if got := req.Header.Get("User-Agent"); got != "opencode/0.16.7" {
		t.Fatalf("UA = %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "my-cli" {
		t.Fatalf("client = %q", got)
	}
	if got := req.Header.Get("x-opencode-project"); got != "proj-a" {
		t.Fatalf("project = %q", got)
	}
	if got := req.Header.Get("x-opencode-request"); got != nativeReq {
		t.Fatalf("request = %q", got)
	}
	if got := req.Header.Get("x-opencode-session"); got != nativeSes {
		t.Fatalf("session = %q", got)
	}
	if got := req.Header.Get(opencode.SessionIDHeader); got != nativeSes {
		t.Fatalf("session id mirror = %q", got)
	}
	if trace.OpencodeSessionID != nativeSes {
		t.Fatalf("trace session = %q", trace.OpencodeSessionID)
	}

	generic := http.Header{}
	generic.Set("x-session-id", "client-session")
	genericCtx := withOpencodeRequest(context.Background(), px.opencodeNonce, generic, nil)
	genericTrace := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(genericCtx, genericTrace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	genericReq, _ := http.NewRequestWithContext(WithTraceInfo(genericCtx, genericTrace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(genericReq, p, generic, genericReq.Context(), body)
	if genericReq.Header.Get("x-opencode-session") != genericTrace.OpencodeSessionID || genericReq.Header.Get(opencode.SessionIDHeader) != genericTrace.OpencodeSessionID {
		t.Fatalf("free mapped session not preferred: header %q mirror %q trace %q", genericReq.Header.Get("x-opencode-session"), genericReq.Header.Get(opencode.SessionIDHeader), genericTrace.OpencodeSessionID)
	}
	if genericTrace.OpencodeSessionID == opencode.NativeSessionID("client-session") {
		t.Fatal("free header path kept the legacy generic hash")
	}
}

func TestOpencodeFreeMappedSessionStableAndScoped(t *testing.T) {
	px := New(nil, nil)
	p := opencodeTestProvider()
	p.ID = 4
	body := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	h := http.Header{}
	h.Set("x-session-id", "client-session")
	ctx := withOpencodeRequest(context.Background(), px.opencodeNonce, h, nil)
	trace1 := &TraceInfo{}
	trace2 := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace1), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace2), backendCodec(domain.ProtocolOpencode, "muse-spark-1.3-contributor-free"), p, []byte(`{"model":"muse-spark-1.3-contributor-free","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if !opencode.IsNativeSessionID(trace1.OpencodeSessionID) || trace1.OpencodeSessionID != trace2.OpencodeSessionID {
		t.Fatalf("generic free session not stable across families: %q %q", trace1.OpencodeSessionID, trace2.OpencodeSessionID)
	}
	if trace1.OpencodeSessionID == opencode.NativeSessionID("client-session") {
		t.Fatal("free generic session used the legacy hash")
	}

	other := New(nil, nil)
	otherTrace := &TraceInfo{}
	if _, err := other.prepareUpstreamRequest(WithTraceInfo(context.Background(), otherTrace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	if otherTrace.OpencodeSessionID == trace1.OpencodeSessionID {
		t.Fatal("free fallback leaked across Proxy instances")
	}

	paid := &TraceInfo{}
	paidBody := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, paid), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), p, paidBody); err != nil {
		t.Fatal(err)
	}
	wantPaid := opencode.ResolveSession(opencode.Identity{Session: "client-session"}, px.opencodeNonce, p.ID, p.BaseURL, p.APIKey, "")
	if paid.OpencodeSessionID != wantPaid {
		t.Fatalf("paid attempt inherited free mapping: %q want %q", paid.OpencodeSessionID, wantPaid)
	}

	p2 := opencodeTestProvider()
	p2.ID = 9
	fallback1 := &TraceInfo{}
	fallback2 := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), fallback1), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), fallback2), backendCodec(domain.ProtocolOpencode, "big-pickle"), p2, body); err != nil {
		t.Fatal(err)
	}
	if fallback1.OpencodeSessionID == "" || fallback1.OpencodeSessionID == fallback2.OpencodeSessionID || fallback1.OpencodeSessionID == trace1.OpencodeSessionID {
		t.Fatalf("free fallback not provider-scoped: %q %q client %q", fallback1.OpencodeSessionID, fallback2.OpencodeSessionID, trace1.OpencodeSessionID)
	}
}

func TestApplyOpencodeHeadersIgnoresInvalidPassthroughIdentity(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	clientHeaders := http.Header{}
	clientHeaders.Set("User-Agent", "curl/8.0")
	clientHeaders.Set("x-opencode-session", "ses_ok\r\nX-Injected: 1")
	clientHeaders.Set("x-opencode-client", strings.Repeat("c", 300))
	trace := &TraceInfo{OpencodeSessionID: "ses_fallback"}
	applyUpstreamHeaders(req, opencodeTestProvider(), clientHeaders, WithTraceInfo(context.Background(), trace), []byte(`{"model":"deepseek-v4-pro"}`))
	if got := req.Header.Get("User-Agent"); got != opencode.UserAgent {
		t.Fatalf("UA = %q", got)
	}
	sid := req.Header.Get("x-opencode-session")
	if !opencode.IsNativeSessionID(sid) || sid == "ses_fallback" {
		t.Fatalf("session = %q", sid)
	}
	if got := req.Header.Get("x-opencode-client"); got != "desktop" {
		t.Fatalf("client = %q", got)
	}
	if got := req.Header.Get(opencode.SessionIDHeader); got != "" {
		t.Fatalf("paid request synthesized session id: %q", got)
	}
}

func TestOpencodeTranslatedIdentityDoesNotLeak(t *testing.T) {
	var cap struct {
		auth      string
		session   string
		client    string
		project   string
		request   string
		ua        string
		cookie    string
		custom    string
		clientReq string
	}
	base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		cap.auth = r.Header.Get("Authorization")
		cap.session = r.Header.Get("x-opencode-session")
		cap.client = r.Header.Get("x-opencode-client")
		cap.project = r.Header.Get("x-opencode-project")
		cap.request = r.Header.Get("x-opencode-request")
		cap.ua = r.Header.Get("User-Agent")
		cap.cookie = r.Header.Get("Cookie")
		cap.custom = r.Header.Get("X-Custom")
		cap.clientReq = r.Header.Get("x-client-request-id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiSSE)
	})

	body := `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Set("Cookie", "sid=secret")
	req.Header.Set("X-Custom", "nope")
	req.Header.Set("x-client-request-id", "client-req")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if cap.auth != "Bearer public" {
		t.Fatalf("auth leaked or missing: %q", cap.auth)
	}
	if !opencode.IsNativeSessionID(cap.session) || cap.session == opencode.NativeSessionID("client-req") {
		t.Fatalf("generic request id not proxy-mapped: %q", cap.session)
	}
	if cap.clientReq != "" {
		t.Fatalf("x-client-request-id leaked: %q", cap.clientReq)
	}
	if cap.cookie != "" || cap.custom != "" {
		t.Fatalf("unrelated headers leaked cookie=%q custom=%q", cap.cookie, cap.custom)
	}
	if cap.ua != opencode.FreeUserAgent {
		t.Fatalf("non-opencode UA = %q", cap.ua)
	}
	if cap.client != opencode.FreeClient || cap.project != "global" {
		t.Fatalf("defaults missing client=%q project=%q", cap.client, cap.project)
	}
	if !opencode.IsNativeRequestID(cap.request) {
		t.Fatalf("request id = %q", cap.request)
	}
}

func TestOpencodeTranslatedExplicitIdentityUnaryAndStream(t *testing.T) {
	var last http.Header
	base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		last = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
	})

	send := func(stream bool) {
		body := `{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		nativeReq := "msg_0123456789abABCDEFGHIJKLMN"
		nativeSes := "ses_0123456789abABCDEFGHIJKLMN"
		req.Header.Set("User-Agent", "opencode/0.16.7")
		req.Header.Set("x-opencode-client", "my-cli")
		req.Header.Set("x-opencode-project", "proj-a")
		req.Header.Set("x-opencode-request", nativeReq)
		req.Header.Set("x-opencode-session", nativeSes)
		req.Header.Set("Cookie", "sid=secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v status = %d, body = %s", stream, resp.StatusCode, out)
		}
		if last.Get("x-opencode-session") != nativeSes {
			t.Fatalf("stream=%v session = %q", stream, last.Get("x-opencode-session"))
		}
		if last.Get("x-opencode-client") != "my-cli" || last.Get("x-opencode-project") != "proj-a" || last.Get("x-opencode-request") != nativeReq {
			t.Fatalf("stream=%v identity = %v", stream, last)
		}
		if last.Get("User-Agent") != "opencode/0.16.7" {
			t.Fatalf("stream=%v UA = %q", stream, last.Get("User-Agent"))
		}
		if last.Get("Authorization") != "Bearer public" {
			t.Fatalf("stream=%v auth = %q", stream, last.Get("Authorization"))
		}
		if last.Get("Cookie") != "" {
			t.Fatalf("stream=%v cookie leaked", stream)
		}
	}
	send(false)
	send(true)
}

func TestOpencodeTranslatedBodySessionField(t *testing.T) {
	var session string
	base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		session = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiSSE)
	})

	body := `{"model":"default","prompt_cache_key":"cache-from-body","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if !opencode.IsNativeSessionID(session) || session == opencode.NativeSessionID("cache-from-body") {
		t.Fatalf("body session not proxy-mapped: %q", session)
	}
}

func TestOpencodeTranslatedZenFirstTurnDistinctAcrossProxies(t *testing.T) {
	sessions := make([]string, 2)
	for i := range sessions {
		var session string
		base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
			session = r.Header.Get("x-opencode-session")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openaiSSE)
		})
		body := `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
		}
		sessions[i] = session
	}
	if sessions[0] == "" || sessions[1] == "" || sessions[0] == sessions[1] {
		t.Fatalf("zen first-turn sessions collided: %q vs %q", sessions[0], sessions[1])
	}
}

func TestOpencodeTranslatedInvalidBodySessionFallsBack(t *testing.T) {
	var session string
	base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		session = r.Header.Get("x-opencode-session")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiSSE)
	})

	body := `{"model":"default","prompt_cache_key":"bad\nvalue","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if !opencode.IsNativeSessionID(session) || strings.Contains(session, "\n") {
		t.Fatalf("fallback session = %q", session)
	}
}

func TestOpencodeTranslatedInvalidIdentityFallsBack(t *testing.T) {
	var session, ua, client string
	_, token, mux := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		session = r.Header.Get("x-opencode-session")
		ua = r.Header.Get("User-Agent")
		client = r.Header.Get("x-opencode-client")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiSSE)
	})

	body := `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
	// ServeHTTP accepts values the HTTP client would reject (CR/LF), so this
	// path can prove injection candidates are dropped before upstream.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "curl/8.0")
	req.Header.Set("x-opencode-session", "ses_ok\r\nX-Injected: 1")
	req.Header.Set("x-opencode-client", strings.Repeat("c", 300))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	resp := rec.Result()
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if !opencode.IsNativeSessionID(session) || strings.Contains(session, "\n") || strings.Contains(session, "\r") {
		t.Fatalf("fallback session = %q", session)
	}
	if ua != opencode.FreeUserAgent {
		t.Fatalf("UA = %q", ua)
	}
	if client != opencode.FreeClient {
		t.Fatalf("client = %q", client)
	}
}

func assertNoRecognizedReasoningControls(t *testing.T, m map[string]any) {
	t.Helper()
	for _, field := range []string{"reasoning_effort", "thinking", "enable_thinking", "thinking_budget"} {
		if _, ok := m[field]; ok {
			t.Fatalf("%s leaked: %v", field, m[field])
		}
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		if _, ok := r["effort"]; ok {
			t.Fatalf("reasoning.effort leaked: %v", r)
		}
	}
	if oc, ok := m["output_config"].(map[string]any); ok {
		if _, ok := oc["effort"]; ok {
			t.Fatalf("output_config.effort leaked: %v", oc)
		}
	}
}

func TestOpencodeMiniMaxMiMoCaps(t *testing.T) {
	m3 := thinkingCapsFor(t, "minimax-m3")
	if m3.Reasoning || m3.Format != thinking.FormatNone {
		t.Fatalf("zen m3 caps = %+v, want no control", m3)
	}
	mimo := thinkingCapsFor(t, "mimo-v2.5-pro")
	if mimo.Reasoning || mimo.Format != thinking.FormatNone {
		t.Fatalf("mimo caps = %+v", mimo)
	}
	glm := thinkingCapsFor(t, "glm-5.2")
	if !glm.Reasoning || glm.Format != thinking.FormatOpenAI || glm.CanDisable {
		t.Fatalf("glm-5.2 caps = %+v", glm)
	}
}

func prepareOpencodeTranslated(t *testing.T, ingress codec, ingressBody []byte, upstreamModel string) map[string]any {
	t.Helper()
	return prepareOpencodeTranslatedOn(t, opencodeTestProvider(), ingress, ingressBody, upstreamModel)
}

func prepareOpencodeTranslatedOn(t *testing.T, p *domain.Provider, ingress codec, ingressBody []byte, upstreamModel string) map[string]any {
	t.Helper()
	req, err := ingress.decodeRequest(ingressBody)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(ingressBody); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	backend := backendCodecFor(p.Protocol, p.BaseURL, upstreamModel)
	upstreamBody, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = finalizeEncodedBody(upstreamBody, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody, err = New(nil, nil).prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, upstreamBody)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(upstreamBody, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOpencodeTranslatedMiniMaxStripsControls(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	got := prepareOpencodeTranslated(t, openaiCodec, body, "minimax-m3")
	assertNoRecognizedReasoningControls(t, got)
}

func TestOpencodeCatalogEffortWire(t *testing.T) {
	chat := []byte(`{"model":"combo","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	glm := prepareOpencodeTranslated(t, openaiCodec, chat, "glm-5.3")
	if glm["reasoning_effort"] != "low" {
		t.Fatalf("glm-5.3 low = %v, want low", glm["reasoning_effort"])
	}
	medium := prepareOpencodeTranslated(t, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"medium","messages":[{"role":"user","content":"hi"}]}`), "glm-5.3")
	if medium["reasoning_effort"] != "high" {
		t.Fatalf("glm-5.3 medium = %v, want high", medium["reasoning_effort"])
	}
	kimi := prepareOpencodeTranslated(t, openaiCodec, chat, "kimi-k3")
	if kimi["reasoning_effort"] != "max" {
		t.Fatalf("kimi-k3 effort = %v, want max", kimi["reasoning_effort"])
	}
	k25 := prepareOpencodeTranslated(t, openaiCodec, chat, "kimi-k2.5")
	assertNoRecognizedReasoningControls(t, k25)

	gptBody := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	gpt := prepareOpencodeTranslated(t, openaiCodec, gptBody, "gpt-5.4")
	reasoning, _ := gpt["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("gpt reasoning = %+v", reasoning)
	}
	if _, ok := gpt["reasoning_effort"]; ok {
		t.Fatalf("gpt leaked reasoning_effort: %v", gpt["reasoning_effort"])
	}

	qwenBody := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	qwen := prepareOpencodeTranslated(t, openaiCodec, qwenBody, "qwen3.6-plus")
	th, _ := qwen["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] == nil {
		t.Fatalf("qwen thinking = %+v", th)
	}
	if _, ok := qwen["reasoning_effort"]; ok {
		t.Fatalf("qwen effort leaked: %v", qwen["reasoning_effort"])
	}
}

func TestOpencodeAnthropicEffortShapes(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	prepare := func(model string) map[string]any {
		return prepareOpencodeTranslated(t, openaiCodec, body, model)
	}
	opus45 := prepare("claude-opus-4-5")
	th, _ := opus45["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(16000) {
		t.Fatalf("opus 4.5 thinking = %+v", th)
	}
	if effortOf(t, opus45) != "high" {
		t.Fatalf("opus 4.5 effort = %v", effortOf(t, opus45))
	}

	opus46 := prepare("claude-opus-4-6")
	th, _ = opus46["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["display"] != nil || effortOf(t, opus46) != "high" {
		t.Fatalf("opus 4.6 = thinking:%+v effort:%v", th, effortOf(t, opus46))
	}

	fable := prepare("claude-fable-5")
	th, _ = fable["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["display"] != "summarized" || effortOf(t, fable) != "high" {
		t.Fatalf("fable = thinking:%+v effort:%v", th, effortOf(t, fable))
	}

	flash := prepare("qwen3.8-flash")
	if effortOf(t, flash) != "xhigh" {
		t.Fatalf("qwen3.8-flash effort = %v, want xhigh", effortOf(t, flash))
	}
	if _, ok := flash["thinking"]; ok {
		t.Fatalf("qwen3.8-flash wrote thinking: %+v", flash["thinking"])
	}
	if _, ok := flash["reasoning_effort"]; ok {
		t.Fatalf("qwen3.8-flash wrote reasoning_effort: %v", flash["reasoning_effort"])
	}
}

func TestOpencodeQwen38FlashEffortBothTiers(t *testing.T) {
	ingresses := []struct {
		name       string
		c          codec
		body       string
		completion bool
	}{
		{name: "chat", c: openaiCodec, body: `{"model":"combo","reasoning_effort":"%s","max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Find one record","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}]}`},
		{name: "chat-completion", c: openaiCodec, completion: true, body: `{"model":"combo","reasoning_effort":"%s","max_completion_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses", c: responsesCodec, body: `{"model":"combo","reasoning":{"effort":"%s"},"max_output_tokens":%d,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "anthropic", c: anthropicCodec, body: `{"model":"combo","max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000},"output_config":{"effort":"%s"}}`},
	}
	providers := []struct {
		name string
		base string
	}{
		{name: "zen", base: opencode.ZenBaseURL},
		{name: "go", base: opencode.GoBaseURL},
	}
	efforts := []struct {
		in   string
		want string
	}{
		{in: "low", want: "low"},
		{in: "medium", want: "medium"},
		{in: "xhigh", want: "xhigh"},
		{in: "high", want: "xhigh"},
	}
	caps := []int{128, 100000}
	for _, provider := range providers {
		for _, ingress := range ingresses {
			for _, effort := range efforts {
				for _, cap := range caps {
					for _, stream := range []bool{false, true} {
						name := fmt.Sprintf("%s/%s/%s/cap%d/stream%v", provider.name, ingress.name, effort.in, cap, stream)
						t.Run(name, func(t *testing.T) {
							body := qwenIngressBody(ingress.body, ingress.name == "anthropic", effort.in, cap)
							got, req, prov := prepareQwen38Flash(t, provider.base, ingress.c, body, stream)
							assertQwenEffortOnly(t, got, effort.want, cap, stream)
							if req.MaxTokens != cap || req.Model != "qwen3.8-flash" || req.Stream != stream || prov.APIKey != opencode.PublicKey {
								t.Fatalf("state req=%+v key=%s", req, prov.APIKey)
							}
							if ingress.name == "chat" {
								assertQwenTool(t, got)
							}
							if ingress.completion {
								if _, ok := got["max_completion_tokens"]; ok {
									t.Fatalf("completion alias survived: %#v", got["max_completion_tokens"])
								}
							}
						})
					}
				}
			}
		}
	}

	t.Run("lowered generic cap restored", func(t *testing.T) {
		p := opencodeTestProvider()
		req := &ir.Request{
			Model:     "qwen3.8-flash",
			MaxTokens: 128,
			Stream:    true,
			Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "low"},
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded := []byte(`{"model":"qwen3.8-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"keep":true}}`)
		out := finalizeAndPrepareQwen(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertQwenEffortOnly(t, got, "low", 128, true)
		meta, _ := got["metadata"].(map[string]any)
		if meta["keep"] != true || req.MaxTokens != 128 || p.APIKey != opencode.PublicKey {
			t.Fatalf("lowered restore changed state: %#v req=%d", got, req.MaxTokens)
		}
	})

	t.Run("no-intent", func(t *testing.T) {
		p := opencodeTestProvider()
		body := []byte(`{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		req := decodeOpencodeIngress(t, openaiCodec, body, "qwen3.8-flash")
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var encodedBody map[string]any
		if err := json.Unmarshal(encoded, &encodedBody); err != nil {
			t.Fatal(err)
		}
		got := prepareOpencodeTranslatedOn(t, p, openaiCodec, body, "qwen3.8-flash")
		assertNoRecognizedReasoningControls(t, got)
		if got["max_tokens"] != encodedBody["max_tokens"] || got["max_tokens"] != float64(128) {
			t.Fatalf("no-intent cap = %v, encoded %v", got["max_tokens"], encodedBody["max_tokens"])
		}
	})

	t.Run("assistant-final", func(t *testing.T) {
		p := opencodeTestProvider()
		body := []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"done"}]}`)
		req := decodeOpencodeIngress(t, openaiCodec, body, "qwen3.8-flash")
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var encodedBody map[string]any
		if err := json.Unmarshal(encoded, &encodedBody); err != nil {
			t.Fatal(err)
		}
		got := prepareOpencodeTranslatedOn(t, p, openaiCodec, body, "qwen3.8-flash")
		assertNoRecognizedReasoningControls(t, got)
		if got["max_tokens"] != encodedBody["max_tokens"] || req.MaxTokens != 128 {
			t.Fatalf("assistant-final cap = %v, encoded %v", got["max_tokens"], encodedBody["max_tokens"])
		}
	})

	t.Run("omitted cap follows encoded cap", func(t *testing.T) {
		for _, base := range []string{opencode.ZenBaseURL, opencode.GoBaseURL} {
			p := opencodeTestProvider()
			p.BaseURL = base
			body := []byte(`{"model":"combo","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
			req := decodeOpencodeIngress(t, openaiCodec, body, "qwen3.8-flash")
			backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
			encoded, err := backend.encodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var encodedBody map[string]any
			if err := json.Unmarshal(encoded, &encodedBody); err != nil {
				t.Fatal(err)
			}
			out := finalizeAndPrepareQwen(t, p, req, backend, encoded)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got["max_tokens"] != encodedBody["max_tokens"] || req.MaxTokens != 0 {
				t.Fatalf("%s omitted cap = %v, encoded %v, ir %d", base, got["max_tokens"], encodedBody["max_tokens"], req.MaxTokens)
			}
			if effortOf(t, got) != "low" || got["thinking"] != nil {
				t.Fatalf("%s omitted shape = %#v", base, got)
			}
		}
	})
}

func qwenIngressBody(format string, anthropicFirst bool, effort string, cap int) []byte {
	if anthropicFirst {
		return []byte(fmt.Sprintf(format, cap, effort))
	}
	return []byte(fmt.Sprintf(format, effort, cap))
}

func prepareQwen38Flash(t *testing.T, base string, ingress codec, body []byte, stream bool) (map[string]any, *ir.Request, *domain.Provider) {
	t.Helper()
	p := opencodeTestProvider()
	p.BaseURL = base
	req := decodeOpencodeIngress(t, ingress, body, "qwen3.8-flash")
	if stream {
		req.Stream = true
	}
	backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
	if backend.id != codecOpencodeMessages {
		t.Fatalf("codec = %s", backend.id)
	}
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out := finalizeAndPrepareQwen(t, p, req, backend, encoded)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got, req, p
}

func finalizeAndPrepareQwen(t *testing.T, p *domain.Provider, req *ir.Request, backend codec, encoded []byte) []byte {
	t.Helper()
	out, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err = New(nil, nil).prepareUpstreamRequestFor(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, req, out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertQwenEffortOnly(t *testing.T, got map[string]any, effort string, cap int, stream bool) {
	t.Helper()
	if got["model"] != "qwen3.8-flash" || got["max_tokens"] != float64(cap) {
		t.Fatalf("model/cap = %v/%v, want qwen3.8-flash/%d", got["model"], got["max_tokens"], cap)
	}
	if gotStream, _ := got["stream"].(bool); gotStream != stream {
		t.Fatalf("stream = %v, want %v", gotStream, stream)
	}
	if effortOf(t, got) != effort {
		t.Fatalf("effort = %v, want %s", effortOf(t, got), effort)
	}
	for _, field := range []string{"thinking", "reasoning_effort", "enable_thinking", "thinking_budget", "reasoning"} {
		if _, ok := got[field]; ok {
			t.Fatalf("%s present: %#v", field, got[field])
		}
	}
	oc, _ := got["output_config"].(map[string]any)
	if len(oc) != 1 {
		t.Fatalf("output_config = %#v", got["output_config"])
	}
}

func assertQwenTool(t *testing.T, got map[string]any) {
	t.Helper()
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", got["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "lookup" || tool["description"] != "Find one record" {
		t.Fatalf("tool identity = %#v", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	if schema["type"] != "object" || props["q"] == nil || len(required) != 1 || required[0] != "q" {
		t.Fatalf("tool schema = %#v", tool["input_schema"])
	}
	if _, ok := tool["parameters"]; ok || tool["type"] != nil {
		t.Fatalf("tool kept Chat envelope: %#v", tool)
	}
}

func TestOpencodeGoCatalogSelectsGoRow(t *testing.T) {
	p := opencodeTestProvider()
	p.BaseURL = opencode.GoBaseURL
	req, err := openaiCodec.decodeRequest([]byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Thinking = thinking.ToIR(&thinking.Config{Mode: thinking.ModeLevel, Level: "high"})
	applyUpstreamModel(req, "minimax-m3")
	backend := backendCodecFor(p.Protocol, p.BaseURL, "minimax-m3")
	if backend.id != "opencode-messages" {
		t.Fatalf("go minimax codec = %s, want opencode-messages", backend.id)
	}
	body, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err = finalizeEncodedBody(body, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "minimax-m3" {
		t.Fatalf("model = %v", got["model"])
	}
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != "adaptive" || th["budget_tokens"] != nil || len(th) != 1 {
		t.Fatalf("thinking = %#v", th)
	}
	for _, field := range []string{"reasoning_effort", "enable_thinking", "thinking_budget", "reasoning"} {
		if _, ok := got[field]; ok {
			t.Fatalf("%s leaked: %#v", field, got[field])
		}
	}
	if oc, ok := got["output_config"].(map[string]any); ok {
		if _, has := oc["effort"]; has {
			t.Fatalf("output_config.effort leaked: %#v", oc)
		}
	}
}

func TestOpencodeMiniMaxToggleMatrix(t *testing.T) {
	ingresses := []struct {
		name string
		c    codec
		body string
	}{
		{name: "chat", c: openaiCodec, body: `{"model":"combo","reasoning_effort":"%s","max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Find one record","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}]}`},
		{name: "responses", c: responsesCodec, body: `{"model":"combo","reasoning":{"effort":"%s"},"max_output_tokens":%d,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "anthropic", c: anthropicCodec, body: `{"model":"combo","max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"%s"},"output_config":{"effort":"high","format":{"type":"json_object"}}}`},
	}
	targets := []struct {
		name  string
		base  string
		model string
		free  bool
	}{
		{name: "zen-free", base: opencode.ZenBaseURL, model: "minimax-m3-free", free: true},
		{name: "go", base: opencode.GoBaseURL, model: "minimax-m3"},
	}
	intents := []struct {
		name  string
		value string
		want  string
	}{
		{name: "enable", value: "high", want: "adaptive"},
		{name: "disable", value: "none", want: "disabled"},
	}
	for _, target := range targets {
		for _, ingress := range ingresses {
			for _, intent := range intents {
				for _, cap := range []int{128, 100000} {
					for _, stream := range []bool{false, true} {
						name := fmt.Sprintf("%s/%s/%s/cap%d/stream%v", target.name, ingress.name, intent.name, cap, stream)
						t.Run(name, func(t *testing.T) {
							body := miniMaxIngressBody(ingress.body, ingress.name == "anthropic", intent.value, cap)
							got := prepareMiniMaxToggle(t, target.base, target.model, ingress.c, body, stream)
							assertMiniMaxToggle(t, got, target.model, intent.want, cap, stream)
							if ingress.name == "chat" {
								assertMiniMaxCallerTool(t, got, target.free)
							}
						})
					}
				}
			}
		}
	}

	rawCases := []struct {
		name   string
		model  string
		base   string
		raw    string
		want   string
		stream bool
	}{
		{name: "zen enable_thinking true", model: "minimax-m3-free", base: opencode.ZenBaseURL, raw: `"enable_thinking":true`, want: "adaptive"},
		{name: "go enable_thinking false", model: "minimax-m3", base: opencode.GoBaseURL, raw: `"enable_thinking":false`, want: "disabled", stream: true},
		{name: "zen thinking adaptive", model: "minimax-m3-free", base: opencode.ZenBaseURL, raw: `"thinking":{"type":"adaptive","budget_tokens":1000}`, want: "adaptive"},
		{name: "go thinking disabled", model: "minimax-m3", base: opencode.GoBaseURL, raw: `"thinking":{"type":"disabled"}`, want: "disabled"},
		{name: "zen responses effort low", model: "minimax-m3-free", base: opencode.ZenBaseURL, raw: `"reasoning":{"effort":"low"}`, want: "adaptive", stream: true},
		{name: "go responses effort off", model: "minimax-m3", base: opencode.GoBaseURL, raw: `"reasoning":{"effort":"off"}`, want: "disabled"},
		{name: "zen output effort conflicts disabled", model: "minimax-m3-free", base: opencode.ZenBaseURL, raw: `"output_config":{"effort":"high","format":{"type":"json_object"}},"thinking":{"type":"disabled"}`, want: "disabled"},
		{name: "go effort conflicts enable false", model: "minimax-m3", base: opencode.GoBaseURL, raw: `"reasoning_effort":"high","enable_thinking":false`, want: "disabled", stream: true},
		{name: "zen positive budget", model: "minimax-m3-free", base: opencode.ZenBaseURL, raw: `"thinking_budget":2048,"enable_thinking":true`, want: "adaptive"},
		{name: "go auto no enable", model: "minimax-m3", base: opencode.GoBaseURL, raw: `"reasoning_effort":"auto"`, want: ""},
		{name: "zen auto with enable", model: "minimax-m3-free", base: opencode.ZenBaseURL, raw: `"reasoning_effort":"auto","enable_thinking":true`, want: "adaptive", stream: true},
	}
	for _, tc := range rawCases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"combo","max_tokens":128,%s,"messages":[{"role":"user","content":"hi"}]}`, tc.raw))
			got := prepareMiniMaxToggle(t, tc.base, tc.model, openaiCodec, body, tc.stream)
			if tc.want == "" {
				assertNoRecognizedReasoningControls(t, got)
				if _, ok := got["stream"]; ok {
					t.Fatalf("auto wrote stream: %#v", got["stream"])
				}
				return
			}
			assertMiniMaxToggle(t, got, tc.model, tc.want, 128, tc.stream)
		})
	}

	suffixCases := []struct {
		name     string
		model    string
		base     string
		suffix   string
		body     string
		want     string
		freeTool bool
	}{
		{
			name:   "go body disable survives high suffix",
			model:  "minimax-m3",
			base:   opencode.GoBaseURL,
			suffix: "(high)",
			body:   `{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high","format":{"type":"json_object"}}}`,
			want:   "disabled",
		},
		{
			name:     "zen body disable survives high suffix",
			model:    "minimax-m3-free",
			base:     opencode.ZenBaseURL,
			suffix:   "(high)",
			body:     `{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"enable_thinking":false,"output_config":{"effort":"high"},"tools":[{"type":"function","function":{"name":"lookup","description":"Find one record","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}]}`,
			want:     "disabled",
			freeTool: true,
		},
		{
			name:   "go none suffix disables enabled body",
			model:  "minimax-m3",
			base:   opencode.GoBaseURL,
			suffix: "(none)",
			body:   `{"model":"combo","max_tokens":128,"reasoning_effort":"high","enable_thinking":true,"messages":[{"role":"user","content":"hi"}]}`,
			want:   "disabled",
		},
		{
			name:   "zen off suffix disables enabled body",
			model:  "minimax-m3-free",
			base:   opencode.ZenBaseURL,
			suffix: "(off)",
			body:   `{"model":"combo","max_output_tokens":128,"reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want:   "disabled",
		},
	}
	for _, tc := range suffixCases {
		t.Run(tc.name, func(t *testing.T) {
			ingress := openaiCodec
			if strings.Contains(tc.body, "max_output_tokens") {
				ingress = responsesCodec
			} else if strings.Contains(tc.body, `"thinking"`) {
				ingress = anthropicCodec
			}
			got := prepareMiniMaxToggle(t, tc.base, tc.model+tc.suffix, ingress, []byte(tc.body), false)
			assertMiniMaxToggle(t, got, tc.model, tc.want, 128, false)
			if tc.freeTool {
				assertMiniMaxCallerTool(t, got, true)
			}
		})
	}

	t.Run("zen chat unchanged", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		got := prepareOpencodeTranslated(t, openaiCodec, body, "minimax-m3")
		assertNoRecognizedReasoningControls(t, got)
		if got["max_tokens"] != float64(128) {
			t.Fatalf("cap = %v", got["max_tokens"])
		}
	})

	t.Run("no intent", func(t *testing.T) {
		for _, target := range targets {
			p := opencodeTestProvider()
			p.BaseURL = target.base
			body := []byte(`{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
			req := decodeOpencodeIngress(t, openaiCodec, body, target.model)
			backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
			encoded, err := backend.encodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var encodedBody map[string]any
			if err := json.Unmarshal(encoded, &encodedBody); err != nil {
				t.Fatal(err)
			}
			got := prepareOpencodeTranslatedOn(t, p, openaiCodec, body, target.model)
			assertNoRecognizedReasoningControls(t, got)
			if got["max_tokens"] != encodedBody["max_tokens"] || got["max_tokens"] != float64(128) {
				t.Fatalf("%s no-intent cap = %v encoded %v", target.name, got["max_tokens"], encodedBody["max_tokens"])
			}
		}
	})

	t.Run("assistant final", func(t *testing.T) {
		p := opencodeTestProvider()
		p.BaseURL = opencode.GoBaseURL
		body := []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"done"}]}`)
		req := decodeOpencodeIngress(t, openaiCodec, body, "minimax-m3")
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var encodedBody map[string]any
		if err := json.Unmarshal(encoded, &encodedBody); err != nil {
			t.Fatal(err)
		}
		got := prepareOpencodeTranslatedOn(t, p, openaiCodec, body, "minimax-m3")
		assertNoRecognizedReasoningControls(t, got)
		if got["max_tokens"] != encodedBody["max_tokens"] || req.MaxTokens != 128 {
			t.Fatalf("assistant cap = %v encoded %v", got["max_tokens"], encodedBody["max_tokens"])
		}
	})

	t.Run("raised cap restored", func(t *testing.T) {
		p := opencodeTestProvider()
		p.BaseURL = opencode.GoBaseURL
		req := &ir.Request{
			Model:     "minimax-m3",
			MaxTokens: 128,
			Stream:    true,
			Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded := []byte(`{"model":"minimax-m3","max_tokens":8192,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":4096},"metadata":{"keep":true}}`)
		out, err := finalizeEncodedBody(encoded, req, backend, p)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertMiniMaxToggle(t, got, "minimax-m3", "adaptive", 128, true)
		meta, _ := got["metadata"].(map[string]any)
		if meta["keep"] != true || req.MaxTokens != 128 || p.APIKey != opencode.PublicKey {
			t.Fatalf("restore changed state: %#v", got)
		}
	})

	t.Run("omitted cap unchanged", func(t *testing.T) {
		p := opencodeTestProvider()
		p.BaseURL = opencode.GoBaseURL
		body := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
		req := decodeOpencodeIngress(t, openaiCodec, body, "minimax-m3")
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var encodedBody map[string]any
		if err := json.Unmarshal(encoded, &encodedBody); err != nil {
			t.Fatal(err)
		}
		out, err := finalizeEncodedBody(encoded, req, backend, p)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got["max_tokens"] != encodedBody["max_tokens"] || req.MaxTokens != 0 {
			t.Fatalf("omitted cap = %v encoded %v ir %d", got["max_tokens"], encodedBody["max_tokens"], req.MaxTokens)
		}
		th, _ := got["thinking"].(map[string]any)
		if th["type"] != "adaptive" {
			t.Fatalf("omitted thinking = %#v", th)
		}
	})
}

func miniMaxIngressBody(format string, anthropicFirst bool, value string, cap int) []byte {
	if anthropicFirst {
		typ := "enabled"
		if value == "none" {
			typ = "disabled"
		}
		return []byte(fmt.Sprintf(format, cap, typ))
	}
	return []byte(fmt.Sprintf(format, value, cap))
}

func prepareMiniMaxToggle(t *testing.T, base, model string, ingress codec, body []byte, stream bool) map[string]any {
	t.Helper()
	rawInput := append([]byte(nil), body...)
	p := opencodeTestProvider()
	p.BaseURL = base
	p.APIKey = "synthetic-opencode-key"
	req, err := ingress.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(body); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, model)
	if stream {
		req.Stream = true
	}
	reqBefore := snapshotIR(t, req)
	providerBefore := snapshotProvider(t, p)
	backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
	if backend.id != codecOpencodeMessages {
		t.Fatalf("codec = %s", backend.id)
	}
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err = New(nil, nil).prepareUpstreamRequestFor(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, req, out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, rawInput) {
		t.Fatal("processing changed caller input")
	}
	if !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, p), providerBefore) {
		t.Fatal("processing changed request or provider")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func snapshotIR(t *testing.T, req *ir.Request) *ir.Request {
	t.Helper()
	if req == nil {
		return nil
	}
	out := *req
	out.Messages = cloneMessages(req.Messages)
	out.StopSequences = append([]string(nil), req.StopSequences...)
	out.Tools = cloneTools(req.Tools)
	if req.Temperature != nil {
		v := *req.Temperature
		out.Temperature = &v
	}
	if req.TopP != nil {
		v := *req.TopP
		out.TopP = &v
	}
	if req.ToolChoice != nil {
		v := *req.ToolChoice
		out.ToolChoice = &v
	}
	if req.Thinking != nil {
		v := *req.Thinking
		out.Thinking = &v
	}
	if req.PromptCacheKey != nil {
		v := *req.PromptCacheKey
		out.PromptCacheKey = &v
	}
	return &out
}

func cloneMessages(in []ir.Message) []ir.Message {
	if in == nil {
		return nil
	}
	out := make([]ir.Message, len(in))
	for i := range in {
		out[i] = ir.Message{Role: in[i].Role, Content: cloneBlocks(in[i].Content)}
	}
	return out
}

func cloneBlocks(in []ir.ContentBlock) []ir.ContentBlock {
	if in == nil {
		return nil
	}
	out := make([]ir.ContentBlock, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].ToolInput = append(json.RawMessage(nil), in[i].ToolInput...)
		out[i].ToolResult = cloneBlocks(in[i].ToolResult)
		if in[i].Image != nil {
			v := *in[i].Image
			out[i].Image = &v
		}
		if in[i].File != nil {
			v := *in[i].File
			out[i].File = &v
		}
	}
	return out
}

func cloneTools(in []ir.Tool) []ir.Tool {
	if in == nil {
		return nil
	}
	out := make([]ir.Tool, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Parameters = append(json.RawMessage(nil), in[i].Parameters...)
	}
	return out
}

func snapshotProvider(t *testing.T, p *domain.Provider) *domain.Provider {
	t.Helper()
	if p == nil {
		return nil
	}
	out := *p
	out.Tags = append([]string(nil), p.Tags...)
	if p.OAuthCreds != nil {
		v := *p.OAuthCreds
		if p.OAuthCreds.ExtraAuthParams != nil {
			v.ExtraAuthParams = make(map[string]string, len(p.OAuthCreds.ExtraAuthParams))
			for key, value := range p.OAuthCreds.ExtraAuthParams {
				v.ExtraAuthParams[key] = value
			}
		}
		out.OAuthCreds = &v
	}
	return &out
}

func assertMiniMaxCallerTool(t *testing.T, got map[string]any, free bool) {
	t.Helper()
	tools, _ := got["tools"].([]any)
	var caller map[string]any
	decoys := map[string]bool{}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		switch name {
		case "lookup":
			caller = tool
		case "bash", "glob", "grep", "read":
			decoys[name] = true
		default:
			t.Fatalf("unexpected tool %q", name)
		}
	}
	if caller == nil {
		t.Fatal("caller tool missing")
	}
	if caller["description"] != "Find one record" {
		t.Fatalf("caller description = %#v", caller["description"])
	}
	schema, _ := caller["input_schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	required, _ := schema["required"].([]any)
	if schema["type"] != "object" || props["q"] == nil || len(required) != 1 || required[0] != "q" {
		t.Fatalf("caller schema = %#v", caller["input_schema"])
	}
	if _, ok := caller["parameters"]; ok || caller["type"] != nil {
		t.Fatalf("caller kept Chat envelope: %#v", caller)
	}
	if !free {
		if len(tools) != 1 || len(decoys) != 0 {
			t.Fatalf("paid tools = %#v", tools)
		}
		return
	}
	if len(tools) != 5 || len(decoys) != 4 {
		t.Fatalf("free tools = %#v", tools)
	}
}

func assertMiniMaxToggle(t *testing.T, got map[string]any, model, typ string, cap int, stream bool) {
	t.Helper()
	if got["model"] != model || got["max_tokens"] != float64(cap) {
		t.Fatalf("model/cap = %v/%v, want %s/%d", got["model"], got["max_tokens"], model, cap)
	}
	gotStream, _ := got["stream"].(bool)
	if gotStream != stream {
		t.Fatalf("stream = %v, want %v", gotStream, stream)
	}
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != typ || len(th) != 1 {
		t.Fatalf("thinking = %#v, want type %s", th, typ)
	}
	for _, field := range []string{"reasoning_effort", "enable_thinking", "thinking_budget", "reasoning"} {
		if _, ok := got[field]; ok {
			t.Fatalf("%s present: %#v", field, got[field])
		}
	}
	if oc, ok := got["output_config"].(map[string]any); ok {
		if _, has := oc["effort"]; has {
			t.Fatalf("effort leaked: %#v", oc)
		}
	}
}

func TestOpencodeMiniMaxToggleHTTP(t *testing.T) {
	hits := 0
	var lastBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		lastBody, _ = io.ReadAll(r.Body)
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "go", BaseURL: opencode.GoBaseURL, APIKey: "go-key", Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = strings.TrimPrefix(upstream.URL, "http://")
		clone.RequestURI = ""
		return http.DefaultTransport.RoundTrip(clone)
	})
	px.streamClient.Transport = px.client.Transport
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cases := []struct {
		name   string
		path   string
		body   string
		stream bool
		want   string
		cap    int
	}{
		{name: "chat-enable", path: "/v1/chat/completions", stream: false, want: "adaptive", cap: 128, body: `{"model":"%s","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "chat-disable-stream", path: "/v1/chat/completions", stream: true, want: "disabled", cap: 100000, body: `{"model":"%s","stream":true,"reasoning_effort":"none","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses-disable", path: "/v1/responses", stream: false, want: "disabled", cap: 128, body: `{"model":"%s","reasoning":{"effort":"off"},"max_output_tokens":128,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "anthropic-enable-stream", path: "/v1/messages", stream: true, want: "adaptive", cap: 100000, body: `{"model":"%s","stream":true,"max_tokens":100000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096},"output_config":{"effort":"high"}}`},
		{name: "anthropic-disable", path: "/v1/messages", stream: false, want: "disabled", cap: 128, body: `{"model":"%s","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"high","format":{"type":"json_object"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.CreateCombo(ctx, &domain.Combo{Name: tc.name, Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "minimax-m3", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			before := hits
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(fmt.Sprintf(tc.body, tc.name)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if hits != before+1 {
				t.Fatalf("hits=%d before=%d", hits, before)
			}
			var got map[string]any
			if err := json.Unmarshal(lastBody, &got); err != nil {
				t.Fatal(err)
			}
			assertMiniMaxToggle(t, got, "minimax-m3", tc.want, tc.cap, tc.stream)
			if strings.Contains(string(lastBody), "go-key") || strings.Contains(string(lastBody), "private") {
				t.Fatalf("body leaked credential or private text: %s", lastBody)
			}
		})
	}
}

func effortOf(t *testing.T, body map[string]any) any {
	t.Helper()
	oc, _ := body["output_config"].(map[string]any)
	return oc["effort"]
}

func TestOpencodeTranslatedMiMoStripsReasoningControls(t *testing.T) {
	chat := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	got := prepareOpencodeTranslated(t, openaiCodec, chat, "mimo-v2.5-pro")
	if got["model"] != "mimo-v2.5-pro" {
		t.Fatalf("model = %v", got["model"])
	}
	assertNoRecognizedReasoningControls(t, got)

	anthropicBody := []byte(`{"model":"combo","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`)
	got = prepareOpencodeTranslated(t, anthropicCodec, anthropicBody, "mimo-v2.5")
	assertNoRecognizedReasoningControls(t, got)

	responsesBody := []byte(`{"model":"combo","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	got = prepareOpencodeTranslated(t, responsesCodec, responsesBody, "mimo-v2.5-pro")
	assertNoRecognizedReasoningControls(t, got)
}

func TestOpencodeTranslatedNoIntentDoesNotInjectReasoning(t *testing.T) {
	body := []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`)
	for _, model := range []string{"mimo-v2.5-pro", "minimax-m3", "glm-4.7"} {
		got := prepareOpencodeTranslated(t, openaiCodec, body, model)
		assertNoRecognizedReasoningControls(t, got)
	}
}

func TestGrokTranslatedResponsesOmitsRejectedEffort(t *testing.T) {
	req := &ir.Request{
		Model: "grok-4.6",
		Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}},
		}},
		Thinking: &ir.Thinking{Mode: ir.ThinkingNone},
	}
	encoded, err := responsesCodec.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var encodedBody map[string]any
	if err := json.Unmarshal(encoded, &encodedBody); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := encodedBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "none" {
		t.Fatalf("encoder effort = %#v, want none before finalize", reasoning["effort"])
	}

	provider := &domain.Provider{
		Protocol:         domain.ProtocolOpenAIResponses,
		ReasoningDialect: domain.ReasoningGrok,
	}
	out, err := finalizeEncodedBody(encoded, req, responsesCodec, provider)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if reasoning, ok := got["reasoning"].(map[string]any); ok {
		if _, present := reasoning["effort"]; present {
			t.Fatalf("reasoning.effort = %#v, want omitted", reasoning["effort"])
		}
	}
}

func TestOpencodeTranslatedMiMoUnaryAndStreamStripReasoning(t *testing.T) {
	upstreamBodies := make(chan []byte, 2)
	base, token, _ := setupOpencodeTranslatedModel(t, "mimo-v2.5-pro", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBodies <- body
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openaiSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	})

	for _, stream := range []bool{false, true} {
		body := `{"model":"default","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"default","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
		}
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v status=%d body=%s", stream, resp.StatusCode, out)
		}

		var got map[string]any
		if err := json.Unmarshal(<-upstreamBodies, &got); err != nil {
			t.Fatal(err)
		}
		gotStream, _ := got["stream"].(bool)
		if got["model"] != "mimo-v2.5-pro" || gotStream != stream {
			t.Fatalf("stream=%v upstream body=%v", stream, got)
		}
		assertNoRecognizedReasoningControls(t, got)
	}
}

func TestOpencodeResponsesPreservesGPTEffortAndOutputCap(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		model  string
		effort string
		stream bool
	}{
		{name: "zen max", base: opencode.ZenBaseURL, model: "gpt-5.6-luna", effort: "max", stream: false},
		{name: "zen none", base: opencode.ZenBaseURL, model: "gpt-5.6-luna", effort: "none", stream: true},
		{name: "go max", base: opencode.GoBaseURL, model: "gpt-5.6-luna", effort: "max", stream: true},
		{name: "go none", base: opencode.GoBaseURL, model: "gpt-5.6-luna", effort: "none", stream: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := opencodeTestProvider()
			p.BaseURL = tc.base
			ingresses := []struct {
				name    string
				ingress codec
				body    string
			}{
				{
					name:    "chat",
					ingress: openaiCodec,
					body:    fmt.Sprintf(`{"model":"combo","stream":%t,"reasoning_effort":%q,"max_completion_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, tc.stream, tc.effort),
				},
				{
					name:    "responses",
					ingress: responsesCodec,
					body:    fmt.Sprintf(`{"model":"combo","stream":%t,"reasoning":{"effort":%q},"max_output_tokens":8,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, tc.stream, tc.effort),
				},
			}
			for _, ing := range ingresses {
				t.Run(ing.name, func(t *testing.T) {
					got := prepareOpencodeTranslatedOn(t, p, ing.ingress, []byte(ing.body), tc.model)
					if got["model"] != tc.model {
						t.Fatalf("model = %v", got["model"])
					}
					if got["stream"] != tc.stream {
						t.Fatalf("stream = %v, want %v", got["stream"], tc.stream)
					}
					if got["max_output_tokens"] != float64(8) {
						t.Fatalf("max_output_tokens = %v, want 8", got["max_output_tokens"])
					}
					reasoning, _ := got["reasoning"].(map[string]any)
					if reasoning["effort"] != tc.effort || reasoning["summary"] != "auto" {
						t.Fatalf("reasoning = %+v, want effort %s and generated summary", reasoning, tc.effort)
					}
				})
			}
		})
	}
}

func TestOpencodeResponsesMuseSparkNormalizationSurvivesPreparation(t *testing.T) {
	cases := []struct {
		base  string
		model string
	}{
		{opencode.ZenBaseURL, "muse-spark-1.2"},
		{opencode.ZenBaseURL, "muse-spark-1.3-contributor-free"},
		{opencode.GoBaseURL, "muse-spark-1.2-contributor"},
		{opencode.GoBaseURL, "muse-spark-1.3-contributor"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			p := opencodeTestProvider()
			p.BaseURL = tc.base
			body := []byte(`{"model":"combo","reasoning":{"effort":"max"},"max_output_tokens":8,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
			got := prepareOpencodeTranslatedOn(t, p, responsesCodec, body, tc.model)
			reasoning, _ := got["reasoning"].(map[string]any)
			if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" {
				t.Fatalf("reasoning = %+v", reasoning)
			}
			if got["max_output_tokens"] != float64(16) {
				t.Fatalf("max_output_tokens = %v, want 16", got["max_output_tokens"])
			}
		})
	}
}

// Muse Spark rejects reasoning level none, so its caps cannot disable it.
func TestOpencodeMuseSparkCaps(t *testing.T) {
	caps := thinkingCapsFor(t, "muse-spark-1.2-contributor-free")
	if caps.CanDisable {
		t.Fatalf("muse-spark must not allow disabling reasoning")
	}
	if caps.Format != thinking.FormatOpenAIResponses {
		t.Fatalf("muse-spark format = %v", caps.Format)
	}
	found := false
	for _, l := range caps.Levels {
		if l == "xhigh" {
			found = true
		}
		if l == "max" || l == "ultra" {
			t.Fatalf("muse-spark advertises %q", l)
		}
	}
	if !found {
		t.Fatalf("muse-spark missing xhigh level: %v", caps.Levels)
	}
}

func TestOpencodeMessagesReconcilesFinalThinkingBudget(t *testing.T) {
	const (
		qwenHigh   = 32768
		qwenMax    = 65535
		qwenOutput = 65536
		opusBudget = 16000
	)
	p := opencodeTestProvider()

	t.Run("explicit cap preserved", func(t *testing.T) {
		cases := []struct {
			name    string
			ingress codec
			body    string
			model   string
			budget  int
			cap     int
		}{
			{name: "chat", ingress: openaiCodec, model: "qwen3.6-plus", budget: qwenHigh, cap: 40000, body: `{"model":"combo","reasoning_effort":"high","max_tokens":40000,"messages":[{"role":"user","content":"hi"}],"metadata":{"keep":true}}`},
			{name: "chat alias", ingress: openaiCodec, model: "qwen3.6-plus", budget: qwenHigh, cap: 40000, body: `{"model":"combo","reasoning_effort":"high","max_completion_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`},
			{name: "responses", ingress: responsesCodec, model: "qwen3.6-plus", budget: qwenHigh, cap: 40000, body: `{"model":"combo","reasoning":{"effort":"high"},"max_output_tokens":40000,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
			{name: "anthropic", ingress: anthropicCodec, model: "qwen3.6-plus", budget: qwenHigh, cap: 40000, body: `{"model":"combo","max_tokens":40000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000}}`},
			{name: "opus", ingress: openaiCodec, model: "claude-opus-4-5", budget: opusBudget, cap: 32000, body: `{"model":"combo","reasoning_effort":"high","max_completion_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := decodeOpencodeIngress(t, tc.ingress, []byte(tc.body), tc.model)
				backend := backendCodecFor(p.Protocol, p.BaseURL, tc.model)
				encoded, err := backend.encodeRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				out, err := finalizeEncodedBody(encoded, req, backend, p)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatal(err)
				}
				assertOpencodeBudget(t, got, tc.budget, tc.cap)
				if req.MaxTokens != tc.cap {
					t.Fatalf("IR max changed to %d", req.MaxTokens)
				}
			})
		}
	})

	t.Run("generic encoder raise is restored", func(t *testing.T) {
		req := &ir.Request{
			Model:     "qwen3.6-plus",
			MaxTokens: 40000,
			Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		// The generic Anthropic encoder can raise max_tokens before OpenCode finalization.
		// Feed that raised body directly so restoration does not depend on the current generic budget.
		encoded := []byte(`{"model":"qwen3.6-plus","max_tokens":50000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":24576},"metadata":{"keep":true}}`)
		out, err := finalizeEncodedBody(encoded, req, backend, p)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertOpencodeBudget(t, got, qwenHigh, 40000)
		meta, _ := got["metadata"].(map[string]any)
		if meta["keep"] != true {
			t.Fatalf("metadata = %#v", got["metadata"])
		}
		if req.MaxTokens != 40000 {
			t.Fatalf("IR max changed to %d", req.MaxTokens)
		}
	})

	t.Run("nested raw fields survive cap patch", func(t *testing.T) {
		req := &ir.Request{
			Model:    "qwen3.6-plus",
			Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		// Duplicate members and key order are lost by recursive map reserialization.
		nested := json.RawMessage(`{"z":1,"a":2,"a":{"k":"</script>","n":9050000000000000001}}`)
		encoded := []byte(`{"model":"qwen3.6-plus","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":32768},"metadata":` + string(nested) + `}`)
		out, err := reconcileOpencodeMessagesBudget(encoded, req, backend, p)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		// encoding/json escapes < when the changed cap is marshaled. Duplicate
		// members and key order still prove the nested value was not rebuilt.
		wantNested := `{"z":1,"a":2,"a":{"k":"\u003c/script\u003e","n":9050000000000000001}}`
		if string(doc["metadata"]) != wantNested {
			t.Fatalf("metadata raw = %s", doc["metadata"])
		}
		if string(doc["max_tokens"]) != "33792" {
			t.Fatalf("max_tokens raw = %s", doc["max_tokens"])
		}
		if req.MaxTokens != 0 || p.APIKey != opencode.PublicKey {
			t.Fatal("request or provider changed")
		}
	})

	t.Run("omitted cap grows within ceiling", func(t *testing.T) {
		got := prepareOpencodeMessagesBudget(t, p, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`), "qwen3.6-plus")
		assertOpencodeBudget(t, got, qwenHigh, qwenHigh+1024)
	})
	t.Run("max effort near ceiling", func(t *testing.T) {
		got := prepareOpencodeMessagesBudget(t, p, responsesCodec, []byte(`{"model":"combo","reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), "qwen3.6-plus")
		assertOpencodeBudget(t, got, qwenMax, qwenOutput)
	})
	t.Run("opus omitted", func(t *testing.T) {
		got := prepareOpencodeMessagesBudget(t, p, anthropicCodec, []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"low"}}`), "claude-opus-4-5")
		assertOpencodeBudget(t, got, opusBudget, opusBudget+1024)
	})
	t.Run("omitted generated cap clamps to catalog ceiling", func(t *testing.T) {
		const (
			model   = "claude-opus-4-1"
			ceiling = 32000
			budget  = ceiling - 1
		)
		req := &ir.Request{
			Model:    model,
			Thinking: &ir.Thinking{Mode: ir.ThinkingLevel, Level: "max"},
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(p.Protocol, p.BaseURL, model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		var before map[string]any
		if err := json.Unmarshal(encoded, &before); err != nil {
			t.Fatal(err)
		}
		if before["max_tokens"] != float64(64000) {
			t.Fatalf("generic encoder cap = %v, want 64000", before["max_tokens"])
		}
		originalReq := req.MaxTokens
		originalKey := p.APIKey
		out, err := finalizeEncodedBody(encoded, req, backend, p)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertOpencodeBudget(t, got, budget, ceiling)
		if req.MaxTokens != originalReq || p.APIKey != originalKey {
			t.Fatal("request or provider changed")
		}
	})

	t.Run("conflicts", func(t *testing.T) {
		cases := []struct {
			name    string
			ingress codec
			model   string
			body    string
		}{
			{name: "equal", ingress: openaiCodec, model: "qwen3.6-plus", body: fmt.Sprintf(`{"model":"combo","reasoning_effort":"high","max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, qwenHigh)},
			{name: "below", ingress: openaiCodec, model: "qwen3.6-plus", body: `{"model":"combo","reasoning_effort":"high","max_completion_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`},
			{name: "over ceiling", ingress: responsesCodec, model: "qwen3.6-plus", body: fmt.Sprintf(`{"model":"combo","reasoning":{"effort":"high"},"max_output_tokens":%d,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, qwenOutput+1)},
			{name: "opus equal", ingress: anthropicCodec, model: "claude-opus-4-5", body: fmt.Sprintf(`{"model":"combo","max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000}}`, opusBudget)},
			{name: "opus over ceiling", ingress: anthropicCodec, model: "claude-opus-4-5", body: `{"model":"combo","max_tokens":64001,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := decodeOpencodeIngress(t, tc.ingress, []byte(tc.body), tc.model)
				original := req.MaxTokens
				backend := backendCodecFor(p.Protocol, p.BaseURL, tc.model)
				encoded, err := backend.encodeRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				_, err = finalizeEncodedBody(encoded, req, backend, p)
				if !errors.Is(err, errOpencodeThinkingBudget) {
					t.Fatalf("err = %v", err)
				}
				if req.MaxTokens != original || p.APIKey != opencode.PublicKey {
					t.Fatalf("request or provider changed")
				}
			})
		}
	})
}

func TestOpencodeMessagesBudgetScopeIsolation(t *testing.T) {
	p := opencodeTestProvider()
	got := prepareOpencodeMessagesBudget(t, p, openaiCodec, []byte(`{"model":"combo","messages":[{"role":"user","content":"hi"}]}`), "qwen3.6-plus")
	assertNoRecognizedReasoningControls(t, got)
	if got["max_tokens"] != float64(4096) {
		t.Fatalf("no-intent cap = %v", got["max_tokens"])
	}

	adaptive := prepareOpencodeMessagesBudget(t, p, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`), "claude-opus-4-6")
	th, _ := adaptive["thinking"].(map[string]any)
	if th["type"] != "adaptive" || adaptive["max_tokens"] != float64(128) {
		t.Fatalf("adaptive = thinking:%+v max:%v", th, adaptive["max_tokens"])
	}

	chat := prepareOpencodeMessagesBudget(t, p, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`), "glm-5.3")
	if chat["reasoning_effort"] != "high" || chat["max_tokens"] != float64(8) {
		t.Fatalf("chat dialect = %#v", chat)
	}
	responsesBody := prepareOpencodeMessagesBudget(t, p, responsesCodec, []byte(`{"model":"combo","reasoning":{"effort":"max"},"max_output_tokens":8,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), "gpt-5.4")
	if responsesBody["max_output_tokens"] != float64(8) {
		t.Fatalf("responses cap = %v", responsesBody["max_output_tokens"])
	}

	claude := &domain.Provider{Protocol: domain.ProtocolAnthropic, ReasoningDialect: domain.ReasoningClaude}
	req := decodeOpencodeIngress(t, anthropicCodec, []byte(`{"model":"combo","max_tokens":1000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`), "claude-sonnet-4-5")
	encoded, err := anthropicCodec.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := finalizeEncodedBody(encoded, req, anthropicCodec, claude)
	if err != nil {
		t.Fatal(err)
	}
	var other map[string]any
	if err := json.Unmarshal(out, &other); err != nil {
		t.Fatal(err)
	}
	if other["max_tokens"] == float64(1000) {
		t.Fatalf("anthropic explicit cap restored: %#v", other)
	}
}

func TestOpencodeMessagesBudgetHTTPLifecycle(t *testing.T) {
	hits := 0
	var lastBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		lastBody, _ = io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(lastBody, &got); err != nil {
			t.Errorf("upstream json: %v body=%s", err, lastBody)
		} else {
			th, _ := got["thinking"].(map[string]any)
			budget, _ := th["budget_tokens"].(float64)
			cap, _ := got["max_tokens"].(float64)
			spec, ok := opencode.Lookup(opencode.Tier(opencode.ZenBaseURL), fmt.Sprint(got["model"]))
			if !ok || th["type"] != "enabled" || budget <= 0 || budget >= cap || cap > float64(spec.Output) {
				t.Errorf("invalid upstream body: %s", lastBody)
			}
		}
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cases := []struct {
		name   string
		model  string
		path   string
		body   string
		status int
		stream bool
		budget int
		cap    int
	}{
		{name: "chat-valid", model: "qwen3.6-plus", path: "/v1/chat/completions", status: 200, budget: 32768, cap: 40000, body: `{"model":"%s","reasoning_effort":"high","max_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "chat-alias", model: "qwen3.6-plus", path: "/v1/chat/completions", status: 200, stream: true, budget: 32768, cap: 40000, body: `{"model":"%s","stream":true,"reasoning_effort":"high","max_completion_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses-max", model: "qwen3.6-plus", path: "/v1/responses", status: 200, budget: 65535, cap: 65536, body: `{"model":"%s","reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "responses-max-stream", model: "qwen3.6-plus", path: "/v1/responses", status: 200, stream: true, budget: 65535, cap: 65536, body: `{"model":"%s","stream":true,"reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "anthropic-opus", model: "claude-opus-4-5", path: "/v1/messages", status: 200, budget: 16000, cap: 32000, body: `{"model":"%s","max_tokens":32000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000}}`},
		{name: "anthropic-opus-stream", model: "claude-opus-4-5", path: "/v1/messages", status: 200, stream: true, budget: 16000, cap: 32000, body: `{"model":"%s","stream":true,"max_tokens":32000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000}}`},
		{name: "opus41-omitted", model: "claude-opus-4-1", path: "/v1/chat/completions", status: 200, budget: 31999, cap: 32000, body: `{"model":"%s","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`},
		{name: "opus41-omitted-stream", model: "claude-opus-4-1", path: "/v1/messages", status: 200, stream: true, budget: 31999, cap: 32000, body: `{"model":"%s","stream":true,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"max"}}`},
		{name: "chat-equal", model: "qwen3.6-plus", path: "/v1/chat/completions", status: 400, body: `{"model":"%s","reasoning_effort":"high","max_tokens":32768,"messages":[{"role":"user","content":"private conversation"}]}`},
		{name: "chat-equal-stream", model: "qwen3.6-plus", path: "/v1/chat/completions", status: 400, stream: true, body: `{"model":"%s","stream":true,"reasoning_effort":"high","max_tokens":32768,"messages":[{"role":"user","content":"private conversation"}]}`},
		{name: "responses-over", model: "claude-opus-4-5", path: "/v1/responses", status: 400, body: `{"model":"%s","reasoning":{"effort":"high"},"max_output_tokens":64001,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"private conversation"}]}]}`},
		{name: "responses-over-stream", model: "claude-opus-4-5", path: "/v1/responses", status: 400, stream: true, body: `{"model":"%s","stream":true,"reasoning":{"effort":"high"},"max_output_tokens":64001,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"private conversation"}]}]}`},
		{name: "anthropic-equal", model: "claude-opus-4-5", path: "/v1/messages", status: 400, body: `{"model":"%s","max_tokens":16000,"messages":[{"role":"user","content":"private conversation"}],"thinking":{"type":"enabled","budget_tokens":1000}}`},
		{name: "anthropic-equal-stream", model: "claude-opus-4-5", path: "/v1/messages", status: 400, stream: true, body: `{"model":"%s","stream":true,"max_tokens":16000,"messages":[{"role":"user","content":"private conversation"}],"thinking":{"type":"enabled","budget_tokens":1000}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := st.CreateCombo(ctx, &domain.Combo{Name: tc.name, Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: tc.model, Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			before := hits
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(fmt.Sprintf(tc.body, tc.name)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if tc.status == http.StatusBadRequest {
				if hits != before {
					t.Fatalf("upstream hits %d, want %d", hits, before)
				}
				if resp.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("content-type = %q", resp.Header.Get("Content-Type"))
				}
				assertIngressErrorEnvelope(t, tc.path, out, "invalid_request_error")
				if bytes.Contains(out, []byte("private conversation")) || bytes.Contains(out, []byte("data:")) || bytes.Contains(out, []byte("event:")) {
					t.Fatalf("error leaked request text or SSE: %s", out)
				}
				return
			}
			if hits != before+1 {
				t.Fatalf("hits=%d before=%d", hits, before)
			}
			var got map[string]any
			if err := json.Unmarshal(lastBody, &got); err != nil {
				t.Fatal(err)
			}
			assertOpencodeBudget(t, got, tc.budget, tc.cap)
			if tc.stream && !bytes.Contains(out, []byte("data:")) {
				t.Fatalf("stream response missing SSE: %s", out)
			}
		})
	}
}

func TestOpencodeMessagesFinalizerFailureStays500(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "unreachable", http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	provider := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	real := backendCodecFor(provider.Protocol, provider.BaseURL, "qwen3.6-plus")
	proxy := New(newTestStore(t), nil)
	prep := &attachmentPrep{}

	t.Run("unary", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":40000,"messages":[{"role":"user","content":"private conversation"}]}`)
		assertOpencodeFinalizer500(t, proxy, provider, real, prep, false, body, &hits)
	})
	t.Run("stream", func(t *testing.T) {
		body := []byte(`{"model":"combo","stream":true,"reasoning_effort":"high","max_tokens":40000,"messages":[{"role":"user","content":"private conversation"}]}`)
		assertOpencodeFinalizer500(t, proxy, provider, real, prep, true, body, &hits)
	})
}

func assertOpencodeFinalizer500(t *testing.T, proxy *Proxy, provider *domain.Provider, real codec, prep *attachmentPrep, stream bool, body []byte, hits *int) {
	t.Helper()
	backend := real
	backend.encodeRequest = func(*ir.Request) ([]byte, error) {
		return []byte("{"), nil
	}
	rec := harlog.New("finalizer")
	ctx := WithTraceInfo(context.Background(), &TraceInfo{RequestID: "finalizer", HAR: rec})
	w := httptest.NewRecorder()
	res := &reqResult{status: http.StatusOK}
	before := *hits
	var got attemptResult
	if stream {
		got = proxy.streamTranslated(w, ctx, res, openaiCodec, backend, provider, "qwen3.6-plus", body, prep)
	} else {
		got = proxy.serveTranslated(w, ctx, res, openaiCodec, backend, provider, "qwen3.6-plus", body, prep)
	}
	if got.status != http.StatusInternalServerError || got.errType != "api_error" || got.retry || got.written || got.failed {
		t.Fatalf("attempt = %+v", got)
	}
	if got.errMsg != "failed to finalize upstream request" {
		t.Fatalf("message = %q", got.errMsg)
	}
	if *hits != before {
		t.Fatalf("upstream hits = %d, want %d", *hits, before)
	}
	if _, entries := rec.Stats(); entries != 0 {
		t.Fatalf("HAR entries = %d, want 0", entries)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("handler committed status=%d body=%s", w.Code, w.Body.Bytes())
	}
	envelope := openaiCodec.encodeError(got.errMsg, got.errType)
	assertIngressErrorEnvelope(t, "/v1/chat/completions", envelope, "api_error")
	if bytes.Contains(envelope, []byte("private conversation")) || bytes.Contains(envelope, []byte("unexpected end of JSON")) {
		t.Fatalf("envelope leaked request or malformed body: %s", envelope)
	}
}

func prepareOpencodeMessagesBudget(t *testing.T, p *domain.Provider, ingress codec, body []byte, model string) map[string]any {
	t.Helper()
	req := decodeOpencodeIngress(t, ingress, body, model)
	backend := backendCodecFor(p.Protocol, p.BaseURL, model)
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func decodeOpencodeIngress(t *testing.T, ingress codec, body []byte, model string) *ir.Request {
	t.Helper()
	req, err := ingress.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(body); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, model)
	return req
}

func assertOpencodeBudget(t *testing.T, got map[string]any, budget, cap int) {
	t.Helper()
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(budget) {
		t.Fatalf("thinking = %+v, want budget %d", th, budget)
	}
	if got["max_tokens"] != float64(cap) {
		t.Fatalf("max_tokens = %v, want %d", got["max_tokens"], cap)
	}
}

func assertIngressErrorEnvelope(t *testing.T, path string, body []byte, errType string) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	errObj, _ := got["error"].(map[string]any)
	if path == "/v1/messages" {
		if got["type"] != "error" || errObj["type"] != errType || errObj["message"] == "" {
			t.Fatalf("anthropic envelope = %s", body)
		}
		return
	}
	if errObj["type"] != errType || errObj["message"] == "" || errObj["code"] != nil {
		t.Fatalf("openai envelope = %s", body)
	}
	if path == "/v1/responses" && errObj["param"] != nil {
		t.Fatalf("responses envelope = %s", body)
	}
}

// prepareTranslatedPipeline runs the complete ingress -> IR -> backend ->
// finalize -> prepare path so tests can assert the wire body a backend
// receives. ctx carries the trace and any captured caller identity.
func prepareTranslatedPipeline(t *testing.T, px *Proxy, p *domain.Provider, ingress codec, rawBody []byte, upstreamModel string, ctx context.Context) map[string]any {
	t.Helper()
	req, err := ingress.decodeRequest(rawBody)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(rawBody); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	backend := backendCodecFor(p.Protocol, p.BaseURL, upstreamModel)
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := px.prepareUpstreamRequestFor(ctx, backend, p, req, finalized)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// prepareTranslatedWithIdentity attaches OpenCode caller identity captured from
// rawBody and h, then runs the complete pipeline.
func prepareTranslatedWithIdentity(t *testing.T, px *Proxy, p *domain.Provider, ingress codec, rawBody []byte, upstreamModel string, h http.Header) (map[string]any, *TraceInfo) {
	t.Helper()
	trace := &TraceInfo{}
	ctx := WithTraceInfo(context.Background(), trace)
	ctx = withOpencodeRequest(ctx, "test-nonce", h, rawBody)
	return prepareTranslatedPipeline(t, px, p, ingress, rawBody, upstreamModel, ctx), trace
}

// A Responses ingress prompt_cache_key survives every supported OpenCode
// Responses destination, including free-tier rewrites and muse-spark patches.
// The explicit caller session header stays independent of the body key.
func TestOpencodeResponsesPromptCacheKeyPreserved(t *testing.T) {
	const headerSession = "ses_0123456789abABCDEFGHIJKLMN"
	cases := []struct {
		name  string
		base  string
		model string
		free  bool
		key   string
	}{
		{name: "zen paid gpt", base: opencode.ZenBaseURL, model: "gpt-5.6-luna", key: "cache-zen-paid"},
		{name: "zen free muse spark", base: opencode.ZenBaseURL, model: "muse-spark-1.3-contributor-free", free: true, key: "cache-zen-free"},
		{name: "go paid muse spark", base: opencode.GoBaseURL, model: "muse-spark-1.2-contributor", key: "cache-go"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				p := opencodeTestProvider()
				p.BaseURL = tc.base
				backend := backendCodecFor(p.Protocol, p.BaseURL, tc.model)
				if backend.id != "opencode-responses" {
					t.Fatalf("backend codec = %q, want opencode-responses", backend.id)
				}
				body := fmt.Sprintf(`{"model":"combo","stream":%t,"input":"hi","prompt_cache_key":%q}`, stream, tc.key)
				h := http.Header{}
				h.Set("x-opencode-session", headerSession)
				got, trace := prepareTranslatedWithIdentity(t, New(nil, nil), p, responsesCodec, []byte(body), tc.model, h)
				if got["prompt_cache_key"] != tc.key {
					t.Fatalf("prepared key = %#v, want %q", got["prompt_cache_key"], tc.key)
				}
				if got["model"] != tc.model || got["stream"] != stream {
					t.Fatalf("model/stream = %#v/%#v", got["model"], got["stream"])
				}
				if trace.OpencodeSessionID != headerSession {
					t.Fatalf("session = %q, want header session %q", trace.OpencodeSessionID, headerSession)
				}
				if tc.free {
					// Free preparation injects decoy tools and forces a choice.
					if _, ok := got["tools"]; !ok {
						t.Fatalf("free preparation did not inject decoy tools: %v", got)
					}
					if got["tool_choice"] != "none" {
						t.Fatalf("free tool_choice = %#v, want none", got["tool_choice"])
					}
				}
			})
		}
	}
}

// Reusing one Proxy across keyed and unkeyed requests must not persist or
// generate a key: the field comes only from the current caller body.
func TestOpencodeResponsesPromptCacheKeyNoStateBleed(t *testing.T) {
	p := opencodeTestProvider()
	px := New(nil, nil)
	prepare := func(body string) map[string]any {
		t.Helper()
		got, _ := prepareTranslatedWithIdentity(t, px, p, responsesCodec, []byte(body), "gpt-5.6-luna", http.Header{})
		return got
	}
	first := prepare(`{"model":"combo","input":"hi","prompt_cache_key":"first-key"}`)
	if first["prompt_cache_key"] != "first-key" {
		t.Fatalf("first key = %#v", first["prompt_cache_key"])
	}
	second := prepare(`{"model":"combo","input":"hi","prompt_cache_key":"second-key"}`)
	if second["prompt_cache_key"] != "second-key" {
		t.Fatalf("second key = %#v", second["prompt_cache_key"])
	}
	third := prepare(`{"model":"combo","input":"hi"}`)
	if _, ok := third["prompt_cache_key"]; ok {
		t.Fatalf("omitted key gained a value: %v", third)
	}
}

// IR prompt_cache_key must not appear in Chat or Anthropic/Messages encoding,
// and Responses ingress must not leak it into OpenCode Chat/Messages backends.
func TestPromptCacheKeyDoesNotLeakBeyondResponses(t *testing.T) {
	key := "responses-only-key"
	req := &ir.Request{
		Model:          "m",
		MaxTokens:      16,
		Messages:       []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		PromptCacheKey: &key,
	}
	for _, c := range []struct {
		name   string
		encode func(*ir.Request) ([]byte, error)
	}{
		{name: "openai chat", encode: openaiCodec.encodeRequest},
		{name: "anthropic messages", encode: anthropicCodec.encodeRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.encode(req)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if _, ok := got["prompt_cache_key"]; ok {
				t.Fatalf("%s encoder leaked the key: %s", c.name, out)
			}
		})
	}

	backends := []struct {
		name  string
		base  string
		model string
	}{
		{name: "opencode chat", base: opencode.ZenBaseURL, model: "deepseek-v4-pro"},
		{name: "opencode messages", base: opencode.GoBaseURL, model: "minimax-m3"},
	}
	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			p := opencodeTestProvider()
			p.BaseURL = tc.base
			wantCodec := backendCodecFor(p.Protocol, p.BaseURL, tc.model).id
			if wantCodec != "opencode-chat" && wantCodec != "opencode-messages" {
				t.Fatalf("backend codec = %q", wantCodec)
			}
			body := `{"model":"combo","input":"hi","prompt_cache_key":"responses-only-key"}`
			got, _ := prepareTranslatedWithIdentity(t, New(nil, nil), p, responsesCodec, []byte(body), tc.model, http.Header{})
			if _, ok := got["prompt_cache_key"]; ok {
				t.Fatalf("%s leaked the Responses key: %v", wantCodec, got)
			}
		})
	}
}

// Chat/Messages ingress never create a Responses prompt_cache_key, even when
// the caller sends an explicit session. Chat ingress fields are not decoded in
// this change, so a Chat prompt_cache_key also stays out of the backend body.
func TestNonResponsesIngressDoesNotCreatePromptCacheKey(t *testing.T) {
	cases := []struct {
		name    string
		ingress codec
		body    string
	}{
		{name: "chat field dropped", ingress: openaiCodec, body: `{"model":"combo","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"chat-ingress-key"}`},
		{name: "chat session only", ingress: openaiCodec, body: `{"model":"combo","messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"chat-session"}}`},
		{name: "messages session only", ingress: anthropicCodec, body: `{"model":"combo","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"messages-session"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := opencodeTestProvider()
			got, trace := prepareTranslatedWithIdentity(t, New(nil, nil), p, tc.ingress, []byte(tc.body), "gpt-5.6-luna", http.Header{})
			if _, ok := got["prompt_cache_key"]; ok {
				t.Fatalf("Responses backend gained a key from %s ingress: %v", tc.ingress.id, got)
			}
			if !opencode.IsNativeSessionID(trace.OpencodeSessionID) {
				t.Fatalf("session identity missing for %s ingress: %q", tc.ingress.id, trace.OpencodeSessionID)
			}
		})
	}
}

// Codex keeps its generated, tenant-scoped cache key even when the caller sent
// an explicit Responses prompt_cache_key. The explicit value may act as captured
// identity input but must not become the raw body field or session_id header.
func TestCodexScopedKeyBeatsExplicitResponsesKey(t *testing.T) {
	const explicit = "client-raw-key"
	raw := []byte(`{"model":"combo","input":"hi","prompt_cache_key":"client-raw-key"}`)
	p := &domain.Provider{Name: "codex", Protocol: domain.ProtocolOpenAICodex}
	backend := backendCodecFor(p.Protocol, "", "gpt-5.3-codex")
	if backend.id != "oai-codex" {
		t.Fatalf("backend codec = %q, want oai-codex", backend.id)
	}
	req, err := responsesCodec.decodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if req.PromptCacheKey == nil || *req.PromptCacheKey != explicit {
		t.Fatalf("decoded IR key = %v", req.PromptCacheKey)
	}
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(explicit)) {
		t.Fatalf("codex encoder forwarded the explicit key: %s", encoded)
	}
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
	var got struct {
		Key string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Key == "" || got.Key == explicit {
		t.Fatalf("resolved codex key = %q", got.Key)
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
}

// The generic public Responses codec stays independent of OpenCode: an explicit
// key survives, and a missing key is never generated.
func TestPublicResponsesPromptCacheKeyPreserved(t *testing.T) {
	p := &domain.Provider{Name: "public", Protocol: domain.ProtocolOpenAIResponses}
	backend := backendCodecFor(p.Protocol, "", "gpt-5.6-luna")
	if backend.id != "oai-responses" {
		t.Fatalf("backend codec = %q, want oai-responses", backend.id)
	}
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"combo","stream":%t,"input":"hi","prompt_cache_key":"public-key"}`, stream)
		trace := &TraceInfo{}
		ctx := WithTraceInfo(context.Background(), trace)
		got := prepareTranslatedPipeline(t, New(nil, nil), p, responsesCodec, []byte(body), "gpt-5.6-luna", ctx)
		if got["prompt_cache_key"] != "public-key" {
			t.Fatalf("stream=%t prepared key = %#v", stream, got["prompt_cache_key"])
		}
	}
	got := prepareTranslatedPipeline(t, New(nil, nil), p, responsesCodec, []byte(`{"model":"combo","input":"hi"}`), "gpt-5.6-luna", WithTraceInfo(context.Background(), &TraceInfo{}))
	if _, ok := got["prompt_cache_key"]; ok {
		t.Fatalf("missing key was generated: %v", got)
	}
}

// The explicit key must not alias across snapshots or make preparation mutate
// the caller bytes, the request, or the provider.
func TestPromptCacheKeyProcessingKeepsInputsUnchanged(t *testing.T) {
	p := opencodeTestProvider()
	raw := []byte(`{"model":"combo","input":"hi","prompt_cache_key":"cache-immutable"}`)
	rawCopy := append([]byte(nil), raw...)
	req, err := responsesCodec.decodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(raw); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, "gpt-5.6-luna")
	reqBefore := snapshotIR(t, req)
	providerBefore := snapshotProvider(t, p)
	backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTraceInfo(context.Background(), &TraceInfo{})
	ctx = withOpencodeRequest(ctx, "test-nonce", http.Header{}, raw)
	out, err := New(nil, nil).prepareUpstreamRequestFor(ctx, backend, p, req, finalized)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, rawCopy) {
		t.Fatal("processing changed the caller body")
	}
	if !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, p), providerBefore) {
		t.Fatal("processing changed the request or provider")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["prompt_cache_key"] != "cache-immutable" {
		t.Fatalf("prepared key = %#v", got["prompt_cache_key"])
	}
}
