package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/qoder"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
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
	got, _, req, p := prepareQwen38FlashRaw(t, base, ingress, body, stream)
	return got, req, p
}

// prepareQwen38FlashRaw runs the complete Qwen pipeline and returns the exact
// outgoing bytes. Snapshots are independent copies taken before decode and
// before encode, then compared after the complete finalizer and prepare path.
func prepareQwen38FlashRaw(t *testing.T, base string, ingress codec, body []byte, stream bool) (map[string]any, []byte, *ir.Request, *domain.Provider) {
	t.Helper()
	rawInput := append([]byte(nil), body...)
	p := opencodeTestProvider()
	p.BaseURL = base
	req := decodeOpencodeIngress(t, ingress, body, "qwen3.8-flash")
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
	out := finalizeAndPrepareQwen(t, p, req, backend, encoded)
	if !bytes.Equal(body, rawInput) {
		t.Fatal("processing changed the caller body")
	}
	if !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, p), providerBefore) {
		t.Fatal("processing changed the request or provider")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got, out, req, p
}

func finalizeAndPrepareQwen(t *testing.T, p *domain.Provider, req *ir.Request, backend codec, encoded []byte) []byte {
	t.Helper()
	_, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
	return out
}

// finalizeAndPrepareQwenObserved snapshots the caller IR, provider, and encoded
// bytes before the finalizer. The copies are independent of the live values.
func finalizeAndPrepareQwenObserved(t *testing.T, p *domain.Provider, req *ir.Request, backend codec, encoded []byte) ([]byte, []byte) {
	t.Helper()
	encodedInput := append([]byte(nil), encoded...)
	reqBefore := snapshotIR(t, req)
	providerBefore := snapshotProvider(t, p)
	finalized, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := New(nil, nil).prepareUpstreamRequestFor(WithTraceInfo(context.Background(), &TraceInfo{}), backend, p, req, finalized)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, encodedInput) {
		t.Fatal("processing changed the encoded body")
	}
	if !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, p), providerBefore) {
		t.Fatal("processing changed the request or provider")
	}
	return finalized, out
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

func TestOpencodeQwen38FlashDisableBothTiers(t *testing.T) {
	ingresses := []struct {
		name       string
		c          codec
		body       string
		completion bool
	}{
		{name: "chat", c: openaiCodec, body: `{"model":"combo","reasoning_effort":"%s","max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Find one record","parameters":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}}]}`},
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
	disables := []struct {
		in string
	}{
		{in: "none"},
		{in: "off"},
	}
	caps := []int{128, 100000}
	for _, provider := range providers {
		for _, ingress := range ingresses {
			for _, disable := range disables {
				for _, cap := range caps {
					for _, stream := range []bool{false, true} {
						name := fmt.Sprintf("%s/%s/%s/cap%d/stream%v", provider.name, ingress.name, disable.in, cap, stream)
						t.Run(name, func(t *testing.T) {
							body := qwenIngressBody(ingress.body, ingress.name == "anthropic", disable.in, cap)
							got, req, prov := prepareQwen38Flash(t, provider.base, ingress.c, body, stream)
							assertQwenDisabled(t, got, cap, stream)
							if req.MaxTokens != cap || req.Model != "qwen3.8-flash" || req.Stream != stream || prov.APIKey != opencode.PublicKey {
								t.Fatalf("state req=%+v key=%s", req, prov.APIKey)
							}
							if ingress.name == "chat" {
								assertQwenIntegerTool(t, got)
							}
							if ingress.completion {
								if _, ok := got["max_completion_tokens"]; ok {
									t.Fatalf("completion alias survived: %#v", got["max_completion_tokens"])
								}
							}
							if ingress.name == "anthropic" && got["output_config"] != nil {
								t.Fatalf("anthropic effort-only output_config survived: %#v", got["output_config"])
							}
						})
					}
				}
			}
		}
	}

	t.Run("chat thinking disabled", func(t *testing.T) {
		body := []byte(`{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1000}}`)
		got, req, _ := prepareQwen38Flash(t, opencode.ZenBaseURL, openaiCodec, body, true)
		assertQwenDisabled(t, got, 128, true)
		if req.MaxTokens != 128 || got["output_config"] != nil {
			t.Fatalf("output_config = %#v req=%+v", got["output_config"], req)
		}
	})

	t.Run("anthropic thinking disabled", func(t *testing.T) {
		body := []byte(`{"model":"combo","max_tokens":100000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1000},"output_config":{"effort":"high"}}`)
		got, _, _ := prepareQwen38Flash(t, opencode.GoBaseURL, anthropicCodec, body, false)
		assertQwenDisabled(t, got, 100000, false)
	})

	t.Run("enable thinking false", func(t *testing.T) {
		for _, base := range []string{opencode.ZenBaseURL, opencode.GoBaseURL} {
			body := []byte(`{"model":"combo","enable_thinking":false,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
			got, req, prov := prepareQwen38Flash(t, base, openaiCodec, body, false)
			assertQwenDisabled(t, got, 128, false)
			if req.MaxTokens != 128 || prov.APIKey != opencode.PublicKey {
				t.Fatalf("state req=%+v key=%s", req, prov.APIKey)
			}
		}
	})

	t.Run("disable beats positive effort", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"high","enable_thinking":false,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		got, _, _ := prepareQwen38Flash(t, opencode.GoBaseURL, openaiCodec, body, true)
		assertQwenDisabled(t, got, 128, true)
	})

	t.Run("suffix high keeps body disable", func(t *testing.T) {
		body := []byte(`{"model":"combo","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"output_config":{"effort":"xhigh","format":{"type":"json_object"}}}`)
		p := opencodeTestProvider()
		req := decodeOpencodeIngress(t, anthropicCodec, body, "qwen3.8-flash(high)")
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		out := finalizeAndPrepareQwen(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertQwenDisabled(t, got, 128, false)
		if req.Model != "qwen3.8-flash" || req.MaxTokens != 128 {
			t.Fatalf("suffix mutated request: %+v", req)
		}
	})

	t.Run("suffix none disables positive body", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		got := prepareOpencodeTranslated(t, openaiCodec, body, "qwen3.8-flash(none)")
		assertQwenDisabled(t, got, 128, false)
	})

	t.Run("lowered generic cap restored", func(t *testing.T) {
		p := opencodeTestProvider()
		req := &ir.Request{
			Model:     "qwen3.8-flash",
			MaxTokens: 128,
			Stream:    true,
			Thinking:  &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled},
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded := []byte(`{"model":"qwen3.8-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":1024},"metadata":{"keep":true}}`)
		out := finalizeAndPrepareQwen(t, p, req, backend, encoded)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		assertQwenDisabled(t, got, 128, true)
		meta, _ := got["metadata"].(map[string]any)
		if meta["keep"] != true || req.MaxTokens != 128 || p.APIKey != opencode.PublicKey {
			t.Fatalf("lowered restore changed state: %#v req=%d", got, req.MaxTokens)
		}
	})

	t.Run("positive effort unchanged", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		got, req, _ := prepareQwen38Flash(t, opencode.GoBaseURL, openaiCodec, body, false)
		assertQwenEffortOnly(t, got, "xhigh", 128, false)
		if req.Thinking == nil || req.Thinking.Mode != ir.ThinkingLevel || req.Thinking.Level != "high" {
			t.Fatalf("positive IR = %+v", req.Thinking)
		}
	})

	t.Run("neighbors unchanged", func(t *testing.T) {
		body := []byte(`{"model":"combo","reasoning_effort":"none","enable_thinking":false,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		for _, model := range []string{"qwen3.8-max", "qwen3.8-flash-next"} {
			got := prepareOpencodeTranslated(t, openaiCodec, body, model)
			assertNoRecognizedReasoningControls(t, got)
			if got["max_tokens"] != float64(128) {
				t.Fatalf("%s cap = %v", model, got["max_tokens"])
			}
		}
		// qwen3.6-plus remains budget-only. none still maps to its positive budget,
		// which cannot fit this caller cap. The new disable must not change that row.
		req, err := openaiCodec.decodeRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		if captured := thinking.Capture(body); captured != nil {
			req.Thinking = thinking.ToIR(captured)
		}
		applyUpstreamModel(req, "qwen3.6-plus")
		p := opencodeTestProvider()
		backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
		encoded, err := backend.encodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := finalizeEncodedBody(encoded, req, backend, p); err == nil || !errors.Is(err, errOpencodeThinkingBudget) {
			t.Fatalf("qwen3.6-plus error = %v, want budget rejection", err)
		}
	})
}

func TestOpencodeQwen38FlashDisableHTTP(t *testing.T) {
	hits := 0
	var lastBody []byte
	var lastURL string
	var lastMethod string
	var lastAccept string
	var lastAuth string
	var lastAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		lastBody, _ = io.ReadAll(r.Body)
		lastURL = r.URL.String()
		lastMethod = r.Method
		lastAccept = r.Header.Get("Accept")
		lastAuth = r.Header.Get("Authorization")
		lastAPIKey = r.Header.Get("x-api-key")
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

	providers := []struct {
		name    string
		base    string
		apiKey  string
		wantKey string
	}{
		{name: "zen", base: opencode.ZenBaseURL, apiKey: opencode.PublicKey, wantKey: opencode.PublicKey},
		{name: "go", base: opencode.GoBaseURL, apiKey: "go-key", wantKey: "go-key"},
	}
	cases := []struct {
		name   string
		path   string
		stream bool
		cap    int
		body   string
	}{
		{name: "chat-none", path: "/v1/chat/completions", cap: 128, body: `{"model":"%s","reasoning_effort":"none","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","description":"Find one record","parameters":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}}]}`},
		{name: "chat-off-stream", path: "/v1/chat/completions", stream: true, cap: 100000, body: `{"model":"%s","stream":true,"reasoning_effort":"off","max_completion_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "chat-thinking-disabled", path: "/v1/chat/completions", cap: 128, body: `{"model":"%s","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1000}}`},
		{name: "chat-enable-false-stream", path: "/v1/chat/completions", stream: true, cap: 100000, body: `{"model":"%s","stream":true,"enable_thinking":false,"reasoning_effort":"high","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses-off", path: "/v1/responses", cap: 128, body: `{"model":"%s","reasoning":{"effort":"off"},"max_output_tokens":128,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "responses-none-stream", path: "/v1/responses", stream: true, cap: 100000, body: `{"model":"%s","stream":true,"reasoning":{"effort":"none"},"max_output_tokens":100000,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "anthropic-disabled", path: "/v1/messages", cap: 128, body: `{"model":"%s","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled","budget_tokens":1000},"output_config":{"effort":"high"}}`},
		{name: "anthropic-effort-none-stream", path: "/v1/messages", stream: true, cap: 100000, body: `{"model":"%s","stream":true,"max_tokens":100000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000},"output_config":{"effort":"none"}}`},
	}
	for _, provider := range providers {
		prov := &domain.Provider{Name: provider.name, BaseURL: provider.base, APIKey: provider.apiKey, Protocol: domain.ProtocolOpencode}
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			t.Run(provider.name+"/"+tc.name, func(t *testing.T) {
				if err := st.CreateCombo(ctx, &domain.Combo{Name: provider.name + "-" + tc.name, Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "qwen3.8-flash", Enabled: true}}}); err != nil {
					t.Fatal(err)
				}
				before := hits
				req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(fmt.Sprintf(tc.body, provider.name+"-"+tc.name)))
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
				if hits != before+1 || lastMethod != http.MethodPost || !strings.HasSuffix(lastURL, "/messages") {
					t.Fatalf("upstream hits=%d method=%s url=%s", hits-before, lastMethod, lastURL)
				}
				if lastAuth != "Bearer "+provider.wantKey || lastAPIKey != "" || strings.Contains(string(lastBody), key.Token) || strings.Contains(string(lastBody), provider.wantKey) {
					t.Fatalf("auth forwarding mismatch")
				}
				if tc.stream {
					if lastAccept != "text/event-stream" || !bytes.Contains(out, []byte("data:")) {
						t.Fatalf("stream accept=%q response=%s", lastAccept, out)
					}
				}
				var got map[string]any
				if err := json.Unmarshal(lastBody, &got); err != nil {
					t.Fatal(err)
				}
				assertQwenDisabled(t, got, tc.cap, tc.stream)
				if tc.name == "chat-none" {
					assertQwenIntegerTool(t, got)
				}
				if tc.name == "chat-thinking-disabled" && got["output_config"] != nil {
					t.Fatalf("output_config = %#v", got["output_config"])
				}
			})
		}

		t.Run(provider.name+"/state-isolation", func(t *testing.T) {
			comboName := provider.name + "-isolation"
			if err := st.CreateCombo(ctx, &domain.Combo{Name: comboName, Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "qwen3.8-flash", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			disabledBody := fmt.Sprintf(`{"model":%q,"reasoning_effort":"none","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`, comboName)
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(disabledBody))
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
				t.Fatalf("disabled status=%d body=%s", resp.StatusCode, out)
			}
			var disabled map[string]any
			if err := json.Unmarshal(lastBody, &disabled); err != nil {
				t.Fatal(err)
			}
			assertQwenDisabled(t, disabled, 128, false)

			plainBody := fmt.Sprintf(`{"model":%q,"max_tokens":128,"messages":[{"role":"user","content":"next"}]}`, comboName)
			req, err = http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(plainBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key.Token)
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("plain status=%d body=%s", resp.StatusCode, out)
			}
			var plain map[string]any
			if err := json.Unmarshal(lastBody, &plain); err != nil {
				t.Fatal(err)
			}
			assertNoRecognizedReasoningControls(t, plain)
			if plain["model"] != "qwen3.8-flash" || plain["max_tokens"] != float64(128) || plain["stream"] != nil {
				t.Fatalf("plain request = %#v", plain)
			}
			assertQwenDisabled(t, disabled, 128, false)
		})
	}
}

func TestOpencodeQwen38FlashDisableHistoryAndSchema(t *testing.T) {
	const large = "9007199254740993"
	args := `{"id":` + large + `}`
	schema := `{"type":"object","properties":{"id":{"type":"integer","minimum":` + large + `},"nested":{"type":"object","properties":{"n":{"const":` + large + `}}}},"required":["id"]}`
	ingresses := []struct {
		name string
		c    codec
		body string
	}{
		{
			name: "chat",
			c:    openaiCodec,
			body: `{"model":"combo","reasoning_effort":"none","max_tokens":128,"tool_choice":{"type":"function","function":{"name":"lookup"}},"messages":[` +
				`{"role":"user","content":"history-user-1"},` +
				`{"role":"assistant","content":"history-assistant-text","tool_calls":[{"id":"call-history-1","type":"function","function":{"name":"lookup","arguments":` + strconv.Quote(args) + `}}]},` +
				`{"role":"tool","tool_call_id":"call-history-1","content":"history-tool-result"},` +
				`{"role":"user","content":"history-user-final"}` +
				`],"tools":[{"type":"function","function":{"name":"lookup","description":"Find one record","parameters":` + schema + `}}]}`,
		},
		{
			name: "responses",
			c:    responsesCodec,
			body: `{"model":"combo","reasoning":{"effort":"off"},"max_output_tokens":128,"tool_choice":{"type":"function","name":"lookup"},"input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"history-user-1"}]},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"history-assistant-text"}]},` +
				`{"type":"function_call","call_id":"call-history-1","name":"lookup","arguments":` + strconv.Quote(args) + `},` +
				`{"type":"function_call_output","call_id":"call-history-1","output":"history-tool-result"},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"history-user-final"}]}` +
				`],"tools":[{"type":"function","name":"lookup","description":"Find one record","parameters":` + schema + `}]}`,
		},
		{
			name: "anthropic",
			c:    anthropicCodec,
			body: `{"model":"combo","max_tokens":128,"thinking":{"type":"disabled"},"tool_choice":{"type":"tool","name":"lookup"},"messages":[` +
				`{"role":"user","content":[{"type":"text","text":"history-user-1"}]},` +
				`{"role":"assistant","content":[{"type":"text","text":"history-assistant-text"},{"type":"tool_use","id":"call-history-1","name":"lookup","input":` + args + `}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-history-1","content":[{"type":"text","text":"history-tool-result"}]}]},` +
				`{"role":"user","content":[{"type":"text","text":"history-user-final"}]}` +
				`],"tools":[{"name":"lookup","description":"Find one record","input_schema":` + schema + `}]}`,
		},
	}
	providers := []struct {
		name string
		base string
		key  string
	}{
		{name: "zen", base: opencode.ZenBaseURL, key: opencode.PublicKey},
		{name: "go", base: opencode.GoBaseURL, key: "go-history-key"},
	}
	for _, provider := range providers {
		for _, ingress := range ingresses {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream%v", provider.name, ingress.name, stream), func(t *testing.T) {
					got, raw, req, prov := prepareQwen38FlashRaw(t, provider.base, ingress.c, []byte(ingress.body), stream)
					prov.APIKey = provider.key
					if prov.APIKey != provider.key || req.Model != "qwen3.8-flash" || req.Stream != stream {
						t.Fatalf("caller state changed: key=%s req=%+v", prov.APIKey, req)
					}
					assertQwenDisabled(t, got, 128, stream)
					assertQwenExactHistory(t, got, large)
					assertExactJSONNumber(t, raw, []string{"tools", "0", "input_schema", "properties", "id", "minimum"}, large)
					assertExactJSONNumber(t, raw, []string{"tools", "0", "input_schema", "properties", "nested", "properties", "n", "const"}, large)
					assertExactJSONNumber(t, raw, []string{"messages", "1", "content", "1", "input", "id"}, large)
				})
			}
		}
	}

	t.Run("http raw body", func(t *testing.T) {
		var captured []byte
		hits := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			captured, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		t.Cleanup(upstream.Close)
		st := newTestStore(t)
		ctx := context.Background()
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
		prov := &domain.Provider{Name: "go-history", BaseURL: opencode.GoBaseURL, APIKey: "go-history-key", Protocol: domain.ProtocolOpencode}
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateCombo(ctx, &domain.Combo{Name: "history-combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "qwen3.8-flash", Enabled: true}}}); err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(strings.Replace(ingresses[0].body, `"model":"combo"`, `"model":"history-combo"`, 1)))
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
		if resp.StatusCode != http.StatusOK || hits != 1 {
			t.Fatalf("status=%d hits=%d body=%s", resp.StatusCode, hits, out)
		}
		var got map[string]any
		if err := json.Unmarshal(captured, &got); err != nil {
			t.Fatal(err)
		}
		assertQwenDisabled(t, got, 128, false)
		assertQwenExactHistory(t, got, large)
		assertExactJSONNumber(t, captured, []string{"tools", "0", "input_schema", "properties", "id", "minimum"}, large)
		assertExactJSONNumber(t, captured, []string{"messages", "1", "content", "1", "input", "id"}, large)
	})

	t.Run("exact number assertion rejects rounded json", func(t *testing.T) {
		rounded := []byte(`{"tools":[{"input_schema":{"properties":{"id":{"minimum":9007199254740992}}}}]}`)
		err := exactJSONNumberError(rounded, []string{"tools", "0", "input_schema", "properties", "id", "minimum"}, large)
		if err == nil {
			t.Fatal("rounded integer passed the exact assertion")
		}
	})
}

func TestOpencodeQwen38FlashOutputConfigBoundary(t *testing.T) {
	const large = "9007199254740993"
	encoded := []byte(`{"model":"qwen3.8-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"boundary-user"}]}],"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"xhigh","format":{"type":"json_object"},"raw":{"n":` + large + `},"flag":true},"metadata":{"keep":` + large + `}}`)
	p := opencodeTestProvider()
	p.BaseURL = opencode.GoBaseURL
	p.APIKey = "boundary-key"
	req := &ir.Request{
		Model:     "qwen3.8-flash",
		MaxTokens: 128,
		Stream:    true,
		Thinking:  &ir.Thinking{Mode: ir.ThinkingNone, Enable: ir.ThinkingEnableDisabled},
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "boundary-user"}}}},
	}
	backend := backendCodecFor(p.Protocol, p.BaseURL, req.Model)
	finalized, out := finalizeAndPrepareQwenObserved(t, p, req, backend, encoded)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	assertQwenDisabled(t, got, 128, true)
	oc, _ := got["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	raw, _ := oc["raw"].(map[string]any)
	if _, has := oc["effort"]; has || format["type"] != "json_object" || oc["flag"] != true || raw["n"] != float64(9007199254740992) {
		t.Fatalf("disabled output_config = %#v", oc)
	}
	meta, _ := got["metadata"].(map[string]any)
	if meta["keep"] != float64(9007199254740992) || p.APIKey != "boundary-key" || req.MaxTokens != 128 {
		t.Fatalf("boundary siblings/state = %#v key=%s cap=%d", got, p.APIKey, req.MaxTokens)
	}
	assertExactJSONNumber(t, out, []string{"output_config", "raw", "n"}, large)
	assertExactJSONNumber(t, out, []string{"metadata", "keep"}, large)
	assertExactJSONNumber(t, finalized, []string{"output_config", "raw", "n"}, large)

	positive := &ir.Request{
		Model:     "qwen3.8-flash",
		MaxTokens: 128,
		Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "boundary-user"}}}},
	}
	encodedPositive := []byte(`{"model":"qwen3.8-flash","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"boundary-user"}]}],"output_config":{"effort":"low","format":{"type":"json_object"},"raw":{"n":` + large + `}}}`)
	finalizedPositive, outPositive := finalizeAndPrepareQwenObserved(t, p, positive, backend, encodedPositive)
	var gotPositive map[string]any
	if err := json.Unmarshal(outPositive, &gotPositive); err != nil {
		t.Fatal(err)
	}
	// A sibling makes this body fail the effort-only predicate, so the generic
	// cap restore does not run. The test checks sibling survival, not restore.
	if gotPositive["max_tokens"] != float64(64) || effortOf(t, gotPositive) != "xhigh" || gotPositive["thinking"] != nil {
		t.Fatalf("positive boundary = %#v", gotPositive)
	}
	positiveConfig, _ := gotPositive["output_config"].(map[string]any)
	positiveFormat, _ := positiveConfig["format"].(map[string]any)
	if positiveFormat["type"] != "json_object" {
		t.Fatalf("positive sibling lost: %#v", gotPositive["output_config"])
	}
	assertExactJSONNumber(t, outPositive, []string{"output_config", "raw", "n"}, large)
	assertExactJSONNumber(t, finalizedPositive, []string{"output_config", "raw", "n"}, large)

	encodedRestore := []byte(`{"model":"qwen3.8-flash","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"boundary-user"}]}],"output_config":{"effort":"low"}}`)
	restoreReq := &ir.Request{
		Model:     "qwen3.8-flash",
		MaxTokens: 128,
		Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "high"},
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "boundary-user"}}}},
	}
	_, restored := finalizeAndPrepareQwenObserved(t, p, restoreReq, backend, encodedRestore)
	var gotRestore map[string]any
	if err := json.Unmarshal(restored, &gotRestore); err != nil {
		t.Fatal(err)
	}
	assertQwenEffortOnly(t, gotRestore, "xhigh", 128, false)
}

func assertQwenExactHistory(t *testing.T, got map[string]any, large string) {
	t.Helper()
	messages, _ := got["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	want := []struct {
		role string
		text string
	}{
		{role: "user", text: "history-user-1"},
		{role: "assistant", text: "history-assistant-text"},
		{role: "user", text: "history-tool-result"},
		{role: "user", text: "history-user-final"},
	}
	for i, message := range want {
		gotMessage, _ := messages[i].(map[string]any)
		content, _ := gotMessage["content"].([]any)
		text := ""
		if len(content) > 0 {
			block, _ := content[0].(map[string]any)
			text, _ = block["text"].(string)
			if text == "" {
				nested, _ := block["content"].([]any)
				if len(nested) == 1 {
					nestedBlock, _ := nested[0].(map[string]any)
					text, _ = nestedBlock["text"].(string)
				}
			}
		}
		if gotMessage["role"] != message.role || text != message.text {
			t.Fatalf("message %d = %#v", i, gotMessage)
		}
	}
	assistant, _ := messages[1].(map[string]any)
	assistantContent, _ := assistant["content"].([]any)
	if len(assistantContent) != 2 {
		t.Fatalf("assistant content = %#v", assistantContent)
	}
	call, _ := assistantContent[1].(map[string]any)
	input, _ := call["input"].(map[string]any)
	if call["type"] != "tool_use" || call["id"] != "call-history-1" || call["name"] != "lookup" || input["id"] != float64(9007199254740992) {
		t.Fatalf("tool call = %#v", call)
	}
	resultMessage, _ := messages[2].(map[string]any)
	resultContent, _ := resultMessage["content"].([]any)
	if len(resultContent) != 1 {
		t.Fatalf("tool result = %#v", resultContent)
	}
	result, _ := resultContent[0].(map[string]any)
	nested, _ := result["content"].([]any)
	nestedText := ""
	if len(nested) == 1 {
		block, _ := nested[0].(map[string]any)
		nestedText, _ = block["text"].(string)
	}
	if result["type"] != "tool_result" || result["tool_use_id"] != "call-history-1" || nestedText != "history-tool-result" {
		t.Fatalf("tool result block = %#v", result)
	}

	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "lookup" || tool["description"] != "Find one record" || tool["type"] != nil || tool["parameters"] != nil {
		t.Fatalf("tool = %#v", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	id, _ := props["id"].(map[string]any)
	nestedSchema, _ := props["nested"].(map[string]any)
	nestedProps, _ := nestedSchema["properties"].(map[string]any)
	n, _ := nestedProps["n"].(map[string]any)
	required, _ := schema["required"].([]any)
	if schema["type"] != "object" || id["type"] != "integer" || id["minimum"] != float64(9007199254740992) || nestedSchema["type"] != "object" || n["const"] != float64(9007199254740992) || len(required) != 1 || required[0] != "id" {
		t.Fatalf("schema = %#v", schema)
	}
	choice, _ := got["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}
}

func assertExactJSONNumber(t *testing.T, raw []byte, path []string, want string) {
	t.Helper()
	if err := exactJSONNumberError(raw, path, want); err != nil {
		t.Fatal(err)
	}
}

func exactJSONNumberError(raw []byte, path []string, want string) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		return err
	}
	cur := any(got)
	for _, key := range path {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[key]
		case []any:
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(node) {
				return fmt.Errorf("path %v missing index %s", path, key)
			}
			cur = node[index]
		default:
			return fmt.Errorf("path %v stopped at %T", path, cur)
		}
	}
	number, ok := cur.(json.Number)
	if !ok || number.String() != want {
		return fmt.Errorf("path %v = %#v, want exact %s", path, cur, want)
	}
	return nil
}

func assertQwenDisabled(t *testing.T, got map[string]any, cap int, stream bool) {
	t.Helper()
	if got["model"] != "qwen3.8-flash" || got["max_tokens"] != float64(cap) {
		t.Fatalf("model/cap = %v/%v, want qwen3.8-flash/%d", got["model"], got["max_tokens"], cap)
	}
	if gotStream, _ := got["stream"].(bool); gotStream != stream {
		t.Fatalf("stream = %v, want %v", gotStream, stream)
	}
	th, _ := got["thinking"].(map[string]any)
	if len(th) != 1 || th["type"] != "disabled" {
		t.Fatalf("thinking = %#v, want type disabled", th)
	}
	for _, field := range []string{"reasoning_effort", "enable_thinking", "thinking_budget", "reasoning"} {
		if _, ok := got[field]; ok {
			t.Fatalf("%s present: %#v", field, got[field])
		}
	}
	if oc, ok := got["output_config"].(map[string]any); ok {
		if _, has := oc["effort"]; has {
			t.Fatalf("effort present: %#v", oc)
		}
	}
}

func assertQwenIntegerTool(t *testing.T, got map[string]any) {
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
	id, _ := props["id"].(map[string]any)
	required, _ := schema["required"].([]any)
	if schema["type"] != "object" || id["type"] != "integer" || len(required) != 1 || required[0] != "id" {
		t.Fatalf("tool schema = %#v", tool["input_schema"])
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
	out.SystemBlocks = cloneSystemBlocks(req.SystemBlocks)
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
	if req.Store != nil {
		v := *req.Store
		out.Store = &v
	}
	if req.Include != nil {
		v := *req.Include
		if v != nil {
			v = append([]string{}, v...)
		}
		out.Include = &v
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
		if in[i].CacheControl != nil {
			v := *in[i].CacheControl
			out[i].CacheControl = &v
		}
		if in[i].ResponsesReasoning != nil {
			out[i].ResponsesReasoning = in[i].ResponsesReasoning.Clone()
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
		if in[i].CacheControl != nil {
			v := *in[i].CacheControl
			out[i].CacheControl = &v
		}
	}
	return out
}

func cloneSystemBlocks(in []ir.SystemBlock) []ir.SystemBlock {
	if in == nil {
		return nil
	}
	out := make([]ir.SystemBlock, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].CacheControl != nil {
			v := *in[i].CacheControl
			out[i].CacheControl = &v
		}
	}
	return out
}

// TestSnapshotIRDeepCopiesSystemBlocks guards the pipeline snapshots: a later
// mutation of the live IR must not alter the captured copy.
func TestSnapshotIRDeepCopiesSystemBlocks(t *testing.T) {
	req := &ir.Request{
		System:       "sys",
		SystemBlocks: []ir.SystemBlock{{Text: "sys", CacheControl: anthCacheHint("1h")}},
	}
	got := snapshotIR(t, req)
	req.SystemBlocks[0].Text = "mutated"
	*req.SystemBlocks[0].CacheControl = ir.CacheControl{Type: "ephemeral", TTL: "5m"}
	if got.SystemBlocks[0].Text != "sys" || got.SystemBlocks[0].CacheControl.TTL != "1h" {
		t.Fatalf("snapshot aliased system blocks: %+v", got.SystemBlocks)
	}
	if req.SystemBlocks[0].CacheControl == got.SystemBlocks[0].CacheControl {
		t.Fatal("snapshot aliased the hint pointer")
	}
}

// TestSnapshotIRDeepCopiesStoreInclude guards the pipeline snapshots: mutating
// the live store bool or include slice after the snapshot must not alter the
// copy, and nil versus non-nil empty include must stay distinguishable.
func TestSnapshotIRDeepCopiesStoreInclude(t *testing.T) {
	store := true
	include := []string{"a", "b"}
	req := &ir.Request{Model: "m", Store: &store, Include: &include}
	got := snapshotIR(t, req)
	*req.Store = false
	include[0] = "mutated"
	include = append(include, "extra")
	if *got.Store != true {
		t.Fatalf("snapshot aliased store: %v", *got.Store)
	}
	if !reflect.DeepEqual(*got.Include, []string{"a", "b"}) {
		t.Fatalf("snapshot aliased include: %#v", *got.Include)
	}
	if got.Store == req.Store || got.Include == req.Include {
		t.Fatal("snapshot reused pointer identity")
	}

	empty := []string{}
	req = &ir.Request{Model: "m", Include: &empty}
	got = snapshotIR(t, req)
	if got.Include == nil || *got.Include == nil {
		t.Fatal("snapshot collapsed a non-nil empty include")
	}
	var nilSlice []string
	req = &ir.Request{Model: "m", Include: &nilSlice}
	got = snapshotIR(t, req)
	if got.Include == nil || *got.Include != nil {
		t.Fatal("snapshot lost the nil include slice")
	}
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
		if before["max_tokens"] != float64(ceiling) {
			t.Fatalf("generic encoder cap = %v, want source-backed %d", before["max_tokens"], ceiling)
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

func TestOpencodeExplicitBudgetProtocolGate(t *testing.T) {
	const largeInteger = "9007199254740993"
	models := []string{"claude-haiku-4-5", "claude-opus-4-1", "claude-sonnet-4", "claude-sonnet-4-5"}
	ingresses := []struct {
		name string
		c    codec
	}{
		{"chat", openaiCodec},
		{"responses", responsesCodec},
		{"anthropic", anthropicCodec},
	}
	for _, model := range models {
		for _, ingress := range ingresses {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model, ingress.name, stream), func(t *testing.T) {
					provider := &domain.Provider{Protocol: domain.ProtocolAnthropic, ReasoningDialect: domain.ReasoningOpencode, APIKey: "k"}
					raw := explicitBudgetIngress(t, ingress.c, model, 8192, 16000, stream, largeInteger)
					got, out := prepareExplicitBudget(t, provider, ingress.c, raw, model)
					wantBudget := 32000
					if model == "claude-opus-4-1" {
						// The existing OpenCode high preset is half the catalog output limit.
						wantBudget = 16000
					}
					assertExactEnabledBudget(t, out, wantBudget, 16000)
					assertExplicitBudgetPayload(t, got, out, model, largeInteger, stream, ingress.c.id == anthropicCodec.id)
				})
			}
		}
	}

	t.Run("opencode protocol keeps exact budget", func(t *testing.T) {
		provider := opencodeTestProvider()
		raw := explicitBudgetIngress(t, anthropicCodec, "claude-sonnet-4-5", 8192, 16000, false, largeInteger)
		_, out := prepareExplicitBudget(t, provider, anthropicCodec, raw, "claude-sonnet-4-5")
		assertExactEnabledBudget(t, out, 8192, 16000)
	})
	t.Run("opencode protocol rejects invalid exact budget", func(t *testing.T) {
		provider := opencodeTestProvider()
		invalid := []struct {
			name   string
			budget int
			cap    int
		}{
			{"below minimum", 1023, 16000},
			{"at output ceiling", 64000, 0},
			{"above caller cap", 8192, 8000},
		}
		for _, tc := range invalid {
			t.Run(tc.name, func(t *testing.T) {
				raw := explicitBudgetIngress(t, anthropicCodec, "claude-sonnet-4-5", tc.budget, tc.cap, true, largeInteger)
				err := explicitBudgetFinalizeError(t, provider, anthropicCodec, raw, "claude-sonnet-4-5")
				if !errors.Is(err, thinking.ErrOpencodeExplicitBudget) {
					t.Fatalf("err = %v", err)
				}
			})
		}
	})
	t.Run("non-opencode protocol does not validate exact budget", func(t *testing.T) {
		provider := &domain.Provider{Protocol: domain.ProtocolAnthropic, ReasoningDialect: domain.ReasoningOpencode, APIKey: "k"}
		raw := explicitBudgetIngress(t, openaiCodec, "claude-sonnet-4-5", 1023, 16000, false, largeInteger)
		got, out := prepareExplicitBudget(t, provider, openaiCodec, raw, "claude-sonnet-4-5")
		assertExactEnabledBudget(t, out, 32000, 16000)
		if _, ok := got["thinking"].(map[string]any); !ok {
			t.Fatal("preset budget missing")
		}
	})
	t.Run("opencode protocol keeps non-opencode dialect", func(t *testing.T) {
		provider := &domain.Provider{Protocol: domain.ProtocolOpencode, ReasoningDialect: domain.ReasoningClaude, BaseURL: opencode.ZenBaseURL, APIKey: "k"}
		raw := explicitBudgetIngress(t, anthropicCodec, "claude-sonnet-4-5", 8192, 16000, false, largeInteger)
		_, out := prepareExplicitBudget(t, provider, anthropicCodec, raw, "claude-sonnet-4-5")
		assertExactEnabledBudget(t, out, 8192, 16000)
	})
}

func TestOpencodeExplicitNumericBudget(t *testing.T) {
	const largeInteger = "9007199254740993"
	models := []struct {
		id      string
		ceiling int
	}{
		{"claude-haiku-4-5", 64000},
		{"claude-opus-4-1", 32000},
		{"claude-sonnet-4", 64000},
		{"claude-sonnet-4-5", 64000},
	}
	ingresses := []struct {
		name string
		c    codec
	}{
		{"chat", openaiCodec},
		{"responses", responsesCodec},
		{"anthropic", anthropicCodec},
	}
	for _, model := range models {
		for _, ingress := range ingresses {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", model.id, ingress.name, stream), func(t *testing.T) {
					provider := opencodeTestProvider()
					raw := explicitBudgetIngress(t, ingress.c, model.id, 8192, 16000, stream, largeInteger)
					got, out := prepareExplicitBudget(t, provider, ingress.c, raw, model.id)
					assertExactEnabledBudget(t, out, 8192, 16000)
					assertExplicitBudgetPayload(t, got, out, model.id, largeInteger, stream, ingress.c.id == anthropicCodec.id)
				})
			}
		}
	}

	t.Run("vendor prefix and generic encoder clamp", func(t *testing.T) {
		provider := opencodeTestProvider()
		raw := explicitBudgetIngress(t, anthropicCodec, "anthropic/claude-sonnet-4-5", 8192, 16000, false, largeInteger)
		_, out := prepareExplicitBudget(t, provider, anthropicCodec, raw, "anthropic/claude-sonnet-4-5")
		assertExactEnabledBudget(t, out, 8192, 16000)
		if !bytes.Contains(out, []byte(`"model":"anthropic/claude-sonnet-4-5"`)) {
			t.Fatalf("model changed: %s", out)
		}

		req := &ir.Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 16000,
			Thinking:  &ir.Thinking{Mode: ir.ThinkingBudget, Budget: 8192, Enable: ir.ThinkingEnableEnabled, BudgetSet: true},
			Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		}
		backend := backendCodecFor(provider.Protocol, provider.BaseURL, req.Model)
		encoded := []byte(`{"model":"claude-sonnet-4-5","max_tokens":9216,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":4096}}`)
		finalized, err := finalizeEncodedBody(encoded, req, backend, provider)
		if err != nil {
			t.Fatal(err)
		}
		assertExactEnabledBudget(t, finalized, 8192, 16000)
		if req.MaxTokens != 16000 || req.Thinking.Budget != 8192 {
			t.Fatal("finalizer changed request")
		}
	})

	t.Run("anthropic body forms and suffix", func(t *testing.T) {
		provider := opencodeTestProvider()
		forms := []struct {
			name   string
			body   string
			model  string
			budget int
			cap    int
		}{
			{"enabled", `{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192}}`, "claude-sonnet-4-5", 8192, 16000},
			{"adaptive", `{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","budget_tokens":8192}}`, "claude-sonnet-4", 8192, 16000},
			{"enable field", `{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"enable_thinking":true,"thinking_budget":8192}`, "claude-haiku-4-5", 8192, 16000},
			{"suffix", `{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}]}`, "claude-opus-4-1(8192)", 8192, 16000},
		}
		for _, tc := range forms {
			t.Run(tc.name, func(t *testing.T) {
				_, out := prepareExplicitBudget(t, provider, anthropicCodec, []byte(tc.body), tc.model)
				assertExactEnabledBudget(t, out, tc.budget, tc.cap)
				if strings.Contains(string(out), "(") {
					t.Fatalf("suffix remained on wire model: %s", out)
				}
			})
		}
	})

	t.Run("range boundaries", func(t *testing.T) {
		provider := opencodeTestProvider()
		valid := []struct {
			name   string
			model  string
			budget int
			cap    int
		}{
			{"minimum", "claude-sonnet-4-5", 1024, 2048},
			{"ceiling minus one", "claude-opus-4-1", 31999, 0},
			{"ceiling minus one explicit", "claude-haiku-4-5", 63999, 64000},
		}
		for _, tc := range valid {
			t.Run(tc.name, func(t *testing.T) {
				raw := explicitBudgetIngress(t, openaiCodec, tc.model, tc.budget, tc.cap, false, largeInteger)
				_, out := prepareExplicitBudget(t, provider, openaiCodec, raw, tc.model)
				wantCap := tc.cap
				if wantCap == 0 {
					wantCap = 32000
				}
				assertExactEnabledBudget(t, out, tc.budget, wantCap)
			})
		}
		invalid := []struct {
			name   string
			model  string
			budget int
			cap    int
		}{
			{"below minimum", "claude-sonnet-4-5", 1023, 16000},
			{"ceiling", "claude-opus-4-1", 32000, 0},
			{"above ceiling", "claude-sonnet-4", 64001, 0},
			{"budget equals cap", "claude-sonnet-4-5", 8192, 8192},
			{"budget above cap", "claude-haiku-4-5", 8192, 8000},
			{"cap above ceiling", "claude-opus-4-1", 8192, 32001},
		}
		for _, tc := range invalid {
			t.Run(tc.name, func(t *testing.T) {
				raw := explicitBudgetIngress(t, responsesCodec, tc.model, tc.budget, tc.cap, true, largeInteger)
				err := explicitBudgetFinalizeError(t, provider, responsesCodec, raw, tc.model)
				if !errors.Is(err, thinking.ErrOpencodeExplicitBudget) {
					t.Fatalf("err = %v", err)
				}
			})
		}
	})

	t.Run("suffix and effort replace numeric budget", func(t *testing.T) {
		provider := opencodeTestProvider()
		for _, suffix := range []string{"(high)", "(max)", "(auto)", "(none)"} {
			t.Run(suffix, func(t *testing.T) {
				raw := []byte(`{"model":"combo","max_tokens":64000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"low","format":{"type":"json_object"}}}`)
				got, out := prepareExplicitBudget(t, provider, anthropicCodec, raw, "claude-sonnet-4-5"+suffix)
				switch suffix {
				case "(high)":
					assertExactEnabledBudget(t, out, 32000, 64000)
				case "(none)":
					// An explicit none suffix disables the approved row instead of
					// upgrading to the lowest listed level.
					assertClaudeDisabled(t, got, "claude-sonnet-4-5", 64000, false)
				case "(max)":
					assertExactEnabledBudget(t, out, 63999, 64000)
				case "(auto)":
					if _, ok := got["thinking"]; ok {
						t.Fatalf("%s wrote thinking: %s", suffix, out)
					}
					if got["max_tokens"] != float64(64000) {
						t.Fatalf("cap = %v", got["max_tokens"])
					}
				}
			})
		}
		raw := []byte(`{"model":"combo","max_tokens":64000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"max"}}`)
		_, out := prepareExplicitBudget(t, provider, anthropicCodec, raw, "claude-sonnet-4-5")
		assertExactEnabledBudget(t, out, 63999, 64000)
	})

	t.Run("assistant final does not reject or emit", func(t *testing.T) {
		provider := opencodeTestProvider()
		raw := []byte(`{"model":"combo","max_tokens":512,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"done"}],"thinking":{"type":"enabled","budget_tokens":1023}}`)
		got, out := prepareExplicitBudget(t, provider, anthropicCodec, raw, "claude-sonnet-4-5")
		if _, ok := got["thinking"]; ok {
			t.Fatalf("assistant-final emitted thinking: %s", out)
		}
		if got["max_tokens"] != float64(512) {
			t.Fatalf("assistant-final cap = %v", got["max_tokens"])
		}
	})

	t.Run("scope stays outside the four rows", func(t *testing.T) {
		provider := opencodeTestProvider()
		lookalikes := []string{"anthropic/claude-sonnet-4-5-extra", "claude-sonnet-4-5-extra", "claude-sonnet-4.5", "unknown-model"}
		for _, model := range lookalikes {
			t.Run(model, func(t *testing.T) {
				raw := explicitBudgetIngress(t, openaiCodec, model, 8192, 16000, false, largeInteger)
				got, out := prepareExplicitBudget(t, provider, openaiCodec, raw, model)
				if _, ok := got["thinking"]; ok {
					t.Fatalf("lookalike wrote thinking: %s", out)
				}
			})
		}
		goProvider := opencodeTestProvider()
		goProvider.BaseURL = opencode.GoBaseURL
		raw := explicitBudgetIngress(t, anthropicCodec, "claude-sonnet-4-5", 8192, 16000, false, largeInteger)
		got, out := prepareExplicitBudget(t, goProvider, anthropicCodec, raw, "claude-sonnet-4-5")
		if _, ok := got["thinking"]; ok {
			t.Fatalf("Go row wrote thinking: %s", out)
		}

		claude := &domain.Provider{Protocol: domain.ProtocolAnthropic, ReasoningDialect: domain.ReasoningClaude, APIKey: "k"}
		err := explicitBudgetFinalizeError(t, claude, anthropicCodec, raw, "claude-sonnet-4-5")
		if err != nil {
			t.Fatal(err)
		}
		other := prepareOpencodeMessagesBudget(t, provider, openaiCodec, []byte(`{"model":"combo","reasoning_effort":"high","max_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`), "qwen3.6-plus")
		assertOpencodeBudget(t, other, 32768, 40000)
	})

	t.Run("nonpositive and malformed body stays existing", func(t *testing.T) {
		provider := opencodeTestProvider()
		bodies := []string{
			`{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":0}}`,
			`{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":-5}}`,
			`{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"enable_thinking":true,"thinking_budget":"8192"}`,
		}
		for _, body := range bodies {
			got, out := prepareExplicitBudget(t, provider, anthropicCodec, []byte(body), "claude-sonnet-4-5")
			if _, ok := got["thinking"]; ok {
				t.Fatalf("nonpositive or nonnumeric body wrote thinking: %s", out)
			}
		}
		// Existing Capture truncates a fractional JSON number. This is not a new
		// validation guarantee, and the truncated positive value stays exact.
		fractional := []byte(`{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}],"enable_thinking":true,"thinking_budget":8192.5}`)
		_, fractionalOut := prepareExplicitBudget(t, provider, anthropicCodec, fractional, "claude-sonnet-4-5")
		assertExactEnabledBudget(t, fractionalOut, 8192, 16000)
		if _, err := anthropicCodec.decodeRequest([]byte(`{"model":"combo","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":8192.5}}`)); err == nil {
			t.Fatal("fractional budget_tokens decoded")
		}
		suffix, out := prepareExplicitBudget(t, provider, openaiCodec, []byte(`{"model":"combo","max_tokens":16000,"messages":[{"role":"user","content":"hi"}]}`), "claude-sonnet-4-5(0)")
		if _, ok := suffix["thinking"]; ok {
			t.Fatalf("zero suffix wrote thinking: %s", out)
		}
	})
}

func TestOpencodeExplicitNumericBudgetHTTP(t *testing.T) {
	hits := map[string]int{}
	var mu sync.Mutex
	var lastBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		lastBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
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
	first := &domain.Provider{Name: "first", BaseURL: upstream.URL + "/first", APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	second := &domain.Provider{Name: "second", BaseURL: upstream.URL + "/second", APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, second); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	proxy := New(st, nil)
	mux := http.NewServeMux()
	proxy.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	paths := []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
	models := []string{"claude-haiku-4-5", "claude-opus-4-1", "claude-sonnet-4", "claude-sonnet-4-5"}
	for _, path := range paths {
		for _, model := range models {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", path, model, stream), func(t *testing.T) {
					combo := fmt.Sprintf("ok-%s-%t-%s", model, stream, strings.Trim(path, "/"))
					if err := st.CreateCombo(ctx, &domain.Combo{Name: combo, Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
						{ProviderID: first.ID, UpstreamModel: model, Enabled: true},
						{ProviderID: second.ID, UpstreamModel: model, Enabled: true},
					}}); err != nil {
						t.Fatal(err)
					}
					body := explicitBudgetHTTPBody(path, combo, 8192, 16000, stream)
					resp, out := postWithHeaders(t, ts.URL+path, key.Token, body, nil)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("status=%d body=%s", resp.StatusCode, out)
					}
					mu.Lock()
					defer mu.Unlock()
					if hits["/first/messages"] != 1 || len(hits) != 1 {
						t.Fatalf("hits=%v", hits)
					}
					clear(hits)
					assertExactEnabledBudget(t, lastBody, 8192, 16000)
				})
			}
		}
	}

	invalidPaths := []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
	proxy.penalizeProvider(first.ID)
	proxy.penalizeProvider(second.ID)
	for _, path := range invalidPaths {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("invalid%s/stream=%t", path, stream), func(t *testing.T) {
				combo := fmt.Sprintf("bad-%t-%s", stream, strings.Trim(path, "/"))
				if err := st.CreateCombo(ctx, &domain.Combo{Name: combo, Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
					{ProviderID: first.ID, UpstreamModel: "claude-sonnet-4-5", Enabled: true},
					{ProviderID: second.ID, UpstreamModel: "claude-sonnet-4-5", Enabled: true},
				}}); err != nil {
					t.Fatal(err)
				}
				beforeFirst := backoffSkips(proxy, first.ID)
				beforeSecond := backoffSkips(proxy, second.ID)
				resp, out := postWithHeaders(t, ts.URL+path, key.Token, explicitBudgetInvalidHTTPBody(path, combo, stream), nil)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", resp.StatusCode, out)
				}
				assertIngressErrorEnvelope(t, path, out, "invalid_request_error")
				if bytes.Contains(out, []byte("private conversation")) || bytes.Contains(out, []byte("data:")) {
					t.Fatalf("error leaked payload or SSE: %s", out)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(hits) != 0 {
					t.Fatalf("invalid request contacted upstream: %v", hits)
				}
				// orderTargets consumes one existing skip credit from each target
				// before the finalizer rejects the request. A zero credit stays zero.
				// The rejection adds no failure and does not raise either credit.
				if failures := backoffFailures(proxy, first.ID); failures != 1 {
					t.Fatalf("first failures = %d, want 1", failures)
				}
				if failures := backoffFailures(proxy, second.ID); failures != 1 {
					t.Fatalf("second failures = %d, want 1", failures)
				}
				if skips := backoffSkips(proxy, first.ID); skips != consumedSkipCredit(beforeFirst) {
					t.Fatalf("first skips = %d, want %d", skips, consumedSkipCredit(beforeFirst))
				}
				if skips := backoffSkips(proxy, second.ID); skips != consumedSkipCredit(beforeSecond) {
					t.Fatalf("second skips = %d, want %d", skips, consumedSkipCredit(beforeSecond))
				}
			})
		}
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "plain", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: first.ID, UpstreamModel: "claude-sonnet-4-5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	resp, out := postWithHeaders(t, ts.URL+"/v1/messages", key.Token, `{"model":"plain","max_tokens":128,"messages":[{"role":"user","content":"plain-hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("plain status=%d body=%s", resp.StatusCode, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if bytes.Contains(lastBody, []byte("budget_tokens")) || bytes.Contains(lastBody, []byte("private conversation")) || !bytes.Contains(lastBody, []byte("plain-hi")) {
		t.Fatalf("plain request inherited state: %s", lastBody)
	}
}

func consumedSkipCredit(before int) int {
	if before <= 0 {
		return 0
	}
	return before - 1
}

func backoffFailures(p *Proxy, providerID int64) int {
	p.boMu.Lock()
	defer p.boMu.Unlock()
	st := p.bo[providerID]
	if st == nil {
		return 0
	}
	return st.failures
}

func prepareExplicitBudget(t *testing.T, provider *domain.Provider, ingress codec, raw []byte, model string) (map[string]any, []byte) {
	t.Helper()
	got, out := prepareTranslatedPipeline(t, New(nil, nil), provider, ingress, raw, model, WithTraceInfo(context.Background(), &TraceInfo{}))
	return got, out
}

func explicitBudgetFinalizeError(t *testing.T, provider *domain.Provider, ingress codec, raw []byte, model string) error {
	t.Helper()
	rawInput := append([]byte(nil), raw...)
	req, err := ingress.decodeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(raw); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, model)
	backend := backendCodecFor(provider.Protocol, provider.BaseURL, model)
	reqBefore := snapshotIR(t, req)
	providerBefore := snapshotProvider(t, provider)
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = finalizeEncodedBody(encoded, req, backend, provider)
	if !bytes.Equal(raw, rawInput) || !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, provider), providerBefore) {
		t.Fatal("rejected request changed caller state")
	}
	return err
}

func explicitBudgetIngress(t *testing.T, ingress codec, model string, budget, cap int, stream bool, large string) []byte {
	t.Helper()
	_ = model
	switch ingress.id {
	case openaiCodec.id:
		body := map[string]any{
			"model":       "combo",
			"messages":    explicitBudgetChatHistory(large),
			"thinking":    map[string]any{"type": "enabled", "budget_tokens": budget},
			"user":        "caller-user",
			"metadata":    map[string]any{"n": json.RawMessage(large)},
			"tools":       explicitBudgetTools(large),
			"tool_choice": "auto",
		}
		if stream {
			body["stream"] = true
		}
		if cap > 0 {
			body["max_tokens"] = cap
		}
		out, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	case responsesCodec.id:
		body := map[string]any{
			"model":    "combo",
			"input":    explicitBudgetResponsesInput(large),
			"thinking": map[string]any{"type": "enabled", "budget_tokens": budget},
			"metadata": map[string]any{"n": json.RawMessage(large)},
			"tools":    explicitBudgetResponsesTools(large),
		}
		if stream {
			body["stream"] = true
		}
		if cap > 0 {
			body["max_output_tokens"] = cap
		}
		out, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	default:
		body := map[string]any{
			"model":    "combo",
			"messages": explicitBudgetHistory(large),
			"thinking": map[string]any{"type": "enabled", "budget_tokens": budget},
			"tools":    explicitBudgetAnthropicTools(large),
		}
		if stream {
			body["stream"] = true
		}
		if cap > 0 {
			body["max_tokens"] = cap
		}
		out, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func explicitBudgetTools(large string) []any {
	return []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "lookup", "description": "find",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"n": map[string]any{"type": "integer", "maximum": json.RawMessage(large)},
				},
			},
		},
	}}
}

func explicitBudgetResponsesTools(large string) []any {
	return []any{map[string]any{
		"type": "function", "name": "lookup", "description": "find",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"n": map[string]any{"type": "integer", "maximum": json.RawMessage(large)},
			},
		},
	}}
}

func explicitBudgetAnthropicTools(large string) []any {
	return []any{map[string]any{
		"name": "lookup", "description": "find",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"n": map[string]any{"type": "integer", "maximum": json.RawMessage(large)},
			},
		},
	}}
}

func explicitBudgetResponsesInput(large string) []any {
	return []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "first"}}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "calling"}}},
		map[string]any{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": explicitBudgetCallArguments(large)},
		map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "result-1"},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "final-user"}}},
	}
}

func explicitBudgetChatHistory(large string) []any {
	return []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "calling", "tool_calls": []any{
			map[string]any{"id": "call-1", "type": "function", "function": map[string]any{
				"name": "lookup", "arguments": explicitBudgetCallArguments(large),
			}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "result-1"},
		map[string]any{"role": "user", "content": "final-user"},
	}
}

func explicitBudgetHistory(large string) []any {
	return []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "calling"},
			map[string]any{"type": "tool_use", "id": "call-1", "name": "lookup", "input": explicitBudgetAnthropicInput(large)},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "result-1"},
			map[string]any{"type": "text", "text": "final-user"},
		}},
	}
}

func explicitBudgetCallArguments(large string) string {
	return `{"q":"exact","n":` + large + `}`
}

func explicitBudgetAnthropicInput(large string) json.RawMessage {
	return json.RawMessage(explicitBudgetCallArguments(large))
}

func explicitBudgetInvalidHTTPBody(path, model string, stream bool) string {
	streamField := ""
	if stream {
		streamField = `,"stream":true`
	}
	switch path {
	case "/v1/responses":
		return fmt.Sprintf(`{"model":%q%s,"max_output_tokens":16000,"input":"private conversation","thinking":{"type":"enabled","budget_tokens":64001}}`, model, streamField)
	case "/v1/messages":
		return fmt.Sprintf(`{"model":%q%s,"max_tokens":8192,"messages":[{"role":"user","content":"private conversation"}],"thinking":{"type":"enabled","budget_tokens":8192}}`, model, streamField)
	default:
		return fmt.Sprintf(`{"model":%q%s,"max_tokens":16000,"messages":[{"role":"user","content":"private conversation"}],"thinking":{"type":"enabled","budget_tokens":1023}}`, model, streamField)
	}
}

func explicitBudgetHTTPBody(path, model string, budget, cap int, stream bool) string {
	switch path {
	case "/v1/responses":
		streamField := ""
		if stream {
			streamField = `,"stream":true`
		}
		return fmt.Sprintf(`{"model":%q%s,"max_output_tokens":%d,"input":"hi","thinking":{"type":"enabled","budget_tokens":%d}}`, model, streamField, cap, budget)
	case "/v1/messages":
		streamField := ""
		if stream {
			streamField = `,"stream":true`
		}
		return fmt.Sprintf(`{"model":%q%s,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":%d}}`, model, streamField, cap, budget)
	default:
		streamField := ""
		if stream {
			streamField = `,"stream":true`
		}
		return fmt.Sprintf(`{"model":%q%s,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":%d}}`, model, streamField, cap, budget)
	}
}

func assertExactEnabledBudget(t *testing.T, body []byte, budget, cap int) {
	t.Helper()
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var thinkingObj map[string]json.RawMessage
	if err := json.Unmarshal(doc["thinking"], &thinkingObj); err != nil {
		t.Fatalf("thinking raw = %s", doc["thinking"])
	}
	if string(thinkingObj["type"]) != `"enabled"` || string(thinkingObj["budget_tokens"]) != strconv.Itoa(budget) {
		t.Fatalf("thinking raw = %s, want budget %d", doc["thinking"], budget)
	}
	if string(doc["max_tokens"]) != strconv.Itoa(cap) {
		t.Fatalf("max_tokens raw = %s, want %d", doc["max_tokens"], cap)
	}
}

func assertExplicitBudgetPayload(t *testing.T, got map[string]any, raw []byte, model, large string, stream, groupedFinal bool) {
	t.Helper()
	if got["model"] != model {
		t.Fatalf("model = %v", got["model"])
	}
	if stream && got["stream"] != true {
		t.Fatalf("stream = %v, want true", got["stream"])
	}
	if !stream && got["stream"] != nil {
		t.Fatalf("stream = %v, want omitted", got["stream"])
	}
	doc := decodeJSONNumberObject(t, raw)
	assertExplicitBudgetTool(t, doc, large)
	messages, ok := doc["messages"].([]any)
	wantMessages := 4
	if groupedFinal {
		wantMessages = 3
	}
	if !ok || len(messages) != wantMessages {
		t.Fatalf("messages = %#v, want %d canonical groups", doc["messages"], wantMessages)
	}
	assertMessageText(t, messages[0], "user", "first")
	assertToolUse(t, messages[1], "call-1", "lookup", large)
	if groupedFinal {
		assertToolResultAndText(t, messages[2], "call-1", "result-1", "final-user")
		return
	}
	assertToolResult(t, messages[2], "call-1", "result-1")
	assertMessageText(t, messages[3], "user", "final-user")
}

func decodeJSONNumberObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func assertJSONNumberAt(t *testing.T, root any, want string, path ...any) {
	t.Helper()
	cur := root
	for _, step := range path {
		switch key := step.(type) {
		case string:
			obj, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("path %v reached %#v, want object", path, cur)
			}
			cur = obj[key]
		case int:
			arr, ok := cur.([]any)
			if !ok || key < 0 || key >= len(arr) {
				t.Fatalf("path %v reached %#v, want index %d", path, cur, key)
			}
			cur = arr[key]
		default:
			t.Fatalf("bad path step %v", step)
		}
	}
	n, ok := cur.(json.Number)
	if !ok || n.String() != want {
		t.Fatalf("path %v = %#v, want json.Number %s", path, cur, want)
	}
}

func assertMessageText(t *testing.T, raw any, role, text string) {
	t.Helper()
	msg, ok := raw.(map[string]any)
	if !ok || msg["role"] != role {
		t.Fatalf("message = %#v, want role %s", raw, role)
	}
	blocks, ok := msg["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content = %#v, want one text block", msg["content"])
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "text" || block["text"] != text {
		t.Fatalf("text block = %#v, want %s", block, text)
	}
}

func assertExplicitBudgetTool(t *testing.T, doc map[string]any, large string) {
	t.Helper()
	tools, ok := doc["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one declared tool", doc["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["name"] != "lookup" || tool["description"] != "find" {
		t.Fatalf("tool = %#v, want lookup/find", tools[0])
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok || schema["type"] != "object" || len(schema) != 2 {
		t.Fatalf("input_schema = %#v, want object type and properties", tool["input_schema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 1 {
		t.Fatalf("properties = %#v, want only n", schema["properties"])
	}
	n, ok := properties["n"].(map[string]any)
	if !ok || n["type"] != "integer" || len(n) != 2 {
		t.Fatalf("n schema = %#v, want integer maximum", properties["n"])
	}
	assertJSONNumberAt(t, doc, large, "tools", 0, "input_schema", "properties", "n", "maximum")
}

func assertToolUse(t *testing.T, raw any, id, name, large string) {
	t.Helper()
	msg, ok := raw.(map[string]any)
	if !ok || msg["role"] != "assistant" || len(msg) != 2 {
		t.Fatalf("assistant = %#v", raw)
	}
	blocks, ok := msg["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("assistant content = %#v, want text then tool_use", msg["content"])
	}
	text, ok := blocks[0].(map[string]any)
	if !ok || text["type"] != "text" || text["text"] != "calling" || len(text) != 2 {
		t.Fatalf("assistant text = %#v, want calling", blocks[0])
	}
	call, ok := blocks[1].(map[string]any)
	if !ok || call["type"] != "tool_use" || call["id"] != id || call["name"] != name || len(call) != 4 {
		t.Fatalf("tool_use = %#v, want %s %s", blocks[1], id, name)
	}
	input, ok := call["input"].(map[string]any)
	if !ok || input["q"] != "exact" || len(input) != 2 {
		t.Fatalf("tool input = %#v, want q and exact n", call["input"])
	}
	n, ok := input["n"].(json.Number)
	if !ok || n.String() != large {
		t.Fatalf("tool input n = %#v, want json.Number %s", input["n"], large)
	}
}

func assertToolResultAndText(t *testing.T, raw any, id, result, text string) {
	t.Helper()
	msg, ok := raw.(map[string]any)
	if !ok || msg["role"] != "user" {
		t.Fatalf("grouped final message = %#v", raw)
	}
	blocks, ok := msg["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("grouped content = %#v, want tool_result then text", msg["content"])
	}
	assertToolResult(t, map[string]any{"role": "user", "content": []any{blocks[0]}}, id, result)
	gotText, _ := blocks[1].(map[string]any)
	if gotText["type"] != "text" || gotText["text"] != text {
		t.Fatalf("final text = %#v, want %s", gotText, text)
	}
}

func assertToolResult(t *testing.T, raw any, id, result string) {
	t.Helper()
	msg, ok := raw.(map[string]any)
	if !ok || msg["role"] != "user" {
		t.Fatalf("tool result message = %#v", raw)
	}
	blocks, ok := msg["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("tool result content = %#v, want one tool_result", msg["content"])
	}
	gotResult, _ := blocks[0].(map[string]any)
	if gotResult["type"] != "tool_result" || gotResult["tool_use_id"] != id {
		t.Fatalf("tool_result = %#v, want id %s", gotResult, id)
	}
	content, ok := gotResult["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("tool_result content = %#v, want one text block", gotResult["content"])
	}
	nested, _ := content[0].(map[string]any)
	if nested["type"] != "text" || nested["text"] != result {
		t.Fatalf("tool_result text = %#v, want %s", nested, result)
	}
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
// receives. ctx carries the trace and any captured caller identity. It
// snapshots the caller bytes, IR, and provider before the transforming steps
// and fails if any changed. The returned raw bytes are the exact upstream body.
func prepareTranslatedPipeline(t *testing.T, px *Proxy, p *domain.Provider, ingress codec, rawBody []byte, upstreamModel string, ctx context.Context) (map[string]any, []byte) {
	t.Helper()
	return prepareTranslatedPipelineWith(t, px, p, ingress, rawBody, upstreamModel, ctx, false)
}

// prepareTranslatedPipelineWith is prepareTranslatedPipeline plus the
// serveTranslated transport override: when forceStreamOnly is set and the
// selected backend is stream-only, req.Stream is forced true before encoding,
// exactly as the unary serve path does. Paid backends keep the caller value.
func prepareTranslatedPipelineWith(t *testing.T, px *Proxy, p *domain.Provider, ingress codec, rawBody []byte, upstreamModel string, ctx context.Context, forceStreamOnly bool) (map[string]any, []byte) {
	t.Helper()
	rawInput := append([]byte(nil), rawBody...)
	req, err := ingress.decodeRequest(rawBody)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(rawBody); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	backend := backendCodecFor(p.Protocol, p.BaseURL, upstreamModel)
	if forceStreamOnly && backend.streamOnly {
		req.Stream = true
	}
	reqBefore := snapshotIR(t, req)
	providerBefore := snapshotProvider(t, p)
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
	if !bytes.Equal(rawBody, rawInput) {
		t.Fatal("processing changed the caller body")
	}
	if !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, p), providerBefore) {
		t.Fatal("processing changed the request or provider")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("prepared body invalid: %v (%s)", err, out)
	}
	return got, out
}

// prepareTranslatedWithIdentity attaches OpenCode caller identity captured from
// rawBody and h, then runs the complete pipeline.
func prepareTranslatedWithIdentity(t *testing.T, px *Proxy, p *domain.Provider, ingress codec, rawBody []byte, upstreamModel string, h http.Header) (map[string]any, *TraceInfo) {
	t.Helper()
	trace := &TraceInfo{}
	ctx := WithTraceInfo(context.Background(), trace)
	ctx = withOpencodeRequest(ctx, "test-nonce", h, rawBody)
	got, _ := prepareTranslatedPipeline(t, px, p, ingress, rawBody, upstreamModel, ctx)
	return got, trace
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
		got, _ := prepareTranslatedPipeline(t, New(nil, nil), p, responsesCodec, []byte(body), "gpt-5.6-luna", ctx)
		if got["prompt_cache_key"] != "public-key" {
			t.Fatalf("stream=%t prepared key = %#v", stream, got["prompt_cache_key"])
		}
	}
	got, _ := prepareTranslatedPipeline(t, New(nil, nil), p, responsesCodec, []byte(`{"model":"combo","input":"hi"}`), "gpt-5.6-luna", WithTraceInfo(context.Background(), &TraceInfo{}))
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

// TestOpencodeResponsesStoreIncludePreserved drives explicit Responses store
// and include intent through the full OpenCode Responses pipeline for paid and
// free models. Values stay exact, absent and null stay absent, and the
// muse-spark and free-tier normalizers remain in force.
func TestOpencodeResponsesStoreIncludePreserved(t *testing.T) {
	const (
		headerSession = "ses_0123456789abABCDEFGHIJKLMN"
		promptKey     = "options-prompt-key"
	)
	trueValue, falseValue := true, false
	optionCases := []struct {
		name           string
		members        string
		wantStore      *bool
		wantInclude    []string
		wantIncludeSet bool
	}{
		{name: "missing", members: ""},
		{name: "null", members: `,"store":null,"include":null`},
		{name: "store true", members: `,"store":true`, wantStore: &trueValue},
		{name: "store false", members: `,"store":false`, wantStore: &falseValue},
		{name: "include empty", members: `,"include":[]`, wantIncludeSet: true, wantInclude: []string{}},
		{name: "include nonempty", members: `,"include":["reasoning.encrypted_content","future.option",""]`, wantIncludeSet: true, wantInclude: []string{"reasoning.encrypted_content", "future.option", ""}},
		{name: "combined", members: `,"store":false,"include":["a","a"]`, wantStore: &falseValue, wantIncludeSet: true, wantInclude: []string{"a", "a"}},
	}
	models := []struct {
		name  string
		base  string
		model string
		free  bool
		spark bool
	}{
		{name: "zen paid gpt", base: opencode.ZenBaseURL, model: "gpt-5.6-luna"},
		{name: "zen free muse", base: opencode.ZenBaseURL, model: "muse-spark-1.3-contributor-free", free: true, spark: true},
		{name: "go paid muse", base: opencode.GoBaseURL, model: "muse-spark-1.2-contributor", spark: true},
	}
	for _, model := range models {
		for _, stream := range []bool{false, true} {
			for _, tc := range optionCases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", model.name, stream, tc.name), func(t *testing.T) {
					p := opencodeTestProvider()
					p.BaseURL = model.base
					backend := backendCodecFor(p.Protocol, p.BaseURL, model.model)
					if backend.id != "opencode-responses" {
						t.Fatalf("backend codec = %q, want opencode-responses", backend.id)
					}
					raw := []byte(fmt.Sprintf(`{"model":"combo","stream":%t,"input":"hi","prompt_cache_key":"%s","reasoning":{"effort":"max"},"max_output_tokens":4,"tools":[{"type":"function","name":"lookup","description":"d","parameters":{"type":"object","properties":{"q":{"type":"string"},"n":{"maximum":9007199254740993}},"required":["q"]}}]%s}`, stream, promptKey, tc.members))
					h := http.Header{}
					h.Set("x-opencode-session", headerSession)
					trace := &TraceInfo{}
					ctx := WithTraceInfo(context.Background(), trace)
					ctx = withOpencodeRequest(ctx, "test-nonce", h, raw)
					got, out := prepareTranslatedPipeline(t, New(nil, nil), p, responsesCodec, raw, model.model, ctx)

					if got["model"] != model.model {
						t.Fatalf("model = %#v, want %q", got["model"], model.model)
					}
					if got["stream"] != stream {
						t.Fatalf("stream = %#v, want %t", got["stream"], stream)
					}
					if got["prompt_cache_key"] != promptKey {
						t.Fatalf("prompt_cache_key = %#v, want %q", got["prompt_cache_key"], promptKey)
					}
					if trace.OpencodeSessionID != headerSession {
						t.Fatalf("session = %q, want caller session %q", trace.OpencodeSessionID, headerSession)
					}

					var fields map[string]json.RawMessage
					if err := json.Unmarshal(out, &fields); err != nil {
						t.Fatal(err)
					}
					rawStore, hasStore := fields["store"]
					if hasStore != (tc.wantStore != nil) {
						t.Fatalf("store presence = %v, want %v; body=%s", hasStore, tc.wantStore != nil, out)
					}
					if hasStore && string(rawStore) != fmt.Sprintf("%t", *tc.wantStore) {
						t.Fatalf("store = %s, want %v", rawStore, *tc.wantStore)
					}
					rawInclude, hasInclude := fields["include"]
					if hasInclude != tc.wantIncludeSet {
						t.Fatalf("include presence = %v, want %v; body=%s", hasInclude, tc.wantIncludeSet, out)
					}
					if hasInclude {
						var values []string
						if err := json.Unmarshal(rawInclude, &values); err != nil {
							t.Fatalf("include = %s: %v", rawInclude, err)
						}
						if values == nil {
							t.Fatalf("include = %s, want an array", rawInclude)
						}
						if !reflect.DeepEqual(values, tc.wantInclude) {
							t.Fatalf("include = %#v, want %#v", values, tc.wantInclude)
						}
					}

					reasoning := mustMapAny(t, "reasoning", got["reasoning"])
					wantEffort := "max"
					if model.spark {
						wantEffort = "xhigh"
					}
					if reasoning["effort"] != wantEffort {
						t.Fatalf("reasoning effort = %#v, want %q; body=%s", reasoning["effort"], wantEffort, out)
					}
					if model.spark && got["max_output_tokens"] != float64(16) {
						t.Fatalf("muse-spark max_output_tokens = %#v, want 16", got["max_output_tokens"])
					}

					tools := mustListAny(t, "tools", got["tools"])
					if model.free {
						if len(tools) != 5 {
							t.Fatalf("free tools = %d, want 5", len(tools))
						}
					} else if len(tools) != 1 {
						t.Fatalf("paid tools = %d, want 1", len(tools))
					}
					if !bytes.Contains(out, []byte("9007199254740993")) {
						t.Fatalf("raw tool schema number changed: %s", out)
					}
				})
			}
		}
	}
}

// Reusing one Proxy across store/include variants must not persist or generate
// intent: the fields come only from the current caller body.
func TestOpencodeResponsesStoreIncludeNoStateBleed(t *testing.T) {
	p := opencodeTestProvider()
	px := New(nil, nil)
	prepare := func(body string) map[string]any {
		t.Helper()
		got, _ := prepareTranslatedWithIdentity(t, px, p, responsesCodec, []byte(body), "gpt-5.6-luna", http.Header{})
		return got
	}
	first := prepare(`{"model":"combo","input":"hi","store":true,"include":["first"]}`)
	if first["store"] != true {
		t.Fatalf("first store = %#v", first["store"])
	}
	if !reflect.DeepEqual(first["include"], []any{"first"}) {
		t.Fatalf("first include = %#v", first["include"])
	}
	second := prepare(`{"model":"combo","input":"hi","store":false,"include":[]}`)
	if second["store"] != false {
		t.Fatalf("second store = %#v", second["store"])
	}
	if include, ok := second["include"].([]any); !ok || len(include) != 0 {
		t.Fatalf("second include = %#v", second["include"])
	}
	third := prepare(`{"model":"combo","input":"hi"}`)
	if _, ok := third["store"]; ok {
		t.Fatalf("omitted store gained a value: %v", third)
	}
	if _, ok := third["include"]; ok {
		t.Fatalf("omitted include gained a value: %v", third)
	}
	fourth := prepare(`{"model":"combo","input":"hi","store":null,"include":null}`)
	if _, ok := fourth["store"]; ok {
		t.Fatalf("null store gained a value: %v", fourth)
	}
	if _, ok := fourth["include"]; ok {
		t.Fatalf("null include gained a value: %v", fourth)
	}
}

// IR store/include must not appear in Chat or Anthropic encoding, and a
// Chat/Messages ingress must not generate them for a Responses backend from
// unknown raw fields or session metadata. Top-level keys are checked by JSON
// key, not substring, because tool schemas may legitimately contain properties
// named store or include.
func TestStoreIncludeDoesNotLeakBeyondResponses(t *testing.T) {
	store := true
	include := []string{"responses-only.option"}
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 16,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
		Store:     &store,
		Include:   &include,
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
			if _, ok := got["store"]; ok {
				t.Fatalf("%s encoder leaked store: %s", c.name, out)
			}
			if _, ok := got["include"]; ok {
				t.Fatalf("%s encoder leaked include: %s", c.name, out)
			}
		})
	}

	ingressCases := []struct {
		name    string
		ingress codec
		body    string
	}{
		{
			name:    "chat unknown fields",
			ingress: openaiCodec,
			body:    `{"model":"combo","messages":[{"role":"user","content":"hi"}],"store":true,"include":["x.option"],"metadata":{"user_id":"chat-session"}}`,
		},
		{
			name:    "messages unknown fields",
			ingress: anthropicCodec,
			body:    `{"model":"combo","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"store":true,"include":["x.option"],"metadata":{"user_id":"messages-session"}}`,
		},
		{
			name:    "chat tool schema names",
			ingress: openaiCodec,
			body:    `{"model":"combo","messages":[{"role":"user","content":"hi"}],"store":true,"include":["x.option"],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"store":{"type":"boolean"},"include":{"type":"string"}}}}}]}`,
		},
	}
	for _, tc := range ingressCases {
		t.Run(tc.name, func(t *testing.T) {
			p := opencodeTestProvider()
			got, _ := prepareTranslatedWithIdentity(t, New(nil, nil), p, tc.ingress, []byte(tc.body), "gpt-5.6-luna", http.Header{})
			if _, ok := got["store"]; ok {
				t.Fatalf("%s gained a top-level store: %v", tc.ingress.id, got)
			}
			if _, ok := got["include"]; ok {
				t.Fatalf("%s gained a top-level include: %v", tc.ingress.id, got)
			}
			if tc.name == "chat tool schema names" {
				tools, ok := got["tools"].([]any)
				if !ok || len(tools) == 0 {
					t.Fatalf("chat tool schema vanished: %v", got["tools"])
				}
				params := mustMapAny(t, "tools[0].parameters", mustMapAny(t, "tools[0]", tools[0])["parameters"])
				props := mustMapAny(t, "tools[0].parameters.properties", params["properties"])
				if props["store"] == nil || props["include"] == nil {
					t.Fatalf("tool schema property names changed: %#v", props)
				}
			}
		})
	}
}

type anthropicCacheCase struct {
	name  string
	base  string
	model string
	free  bool
}

func anthCacheHint(ttl string) *ir.CacheControl {
	return &ir.CacheControl{Type: "ephemeral", TTL: ttl}
}

// prepareAnthropicCachePipeline mirrors serveTranslated for an Anthropic ingress
// request: decode, capture thinking intent, apply the upstream model, encode
// with the resolved OpenCode Messages backend, finalize, and prepare. It
// snapshots the caller bytes, IR, and provider before the transforming steps
// and fails if any changed. The returned raw bytes are the exact upstream body.
func prepareAnthropicCachePipeline(t *testing.T, p *domain.Provider, ingress codec, rawBody []byte, upstreamModel string, stream bool) (map[string]any, []byte) {
	t.Helper()
	rawInput := append([]byte(nil), rawBody...)
	req, err := ingress.decodeRequest(rawBody)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(rawBody); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	req.Stream = stream
	reqBefore := snapshotIR(t, req)
	providerBefore := snapshotProvider(t, p)
	backend := backendCodecFor(p.Protocol, p.BaseURL, upstreamModel)
	if backend.id != codecOpencodeMessages {
		t.Fatalf("backend codec = %q, want opencode-messages", backend.id)
	}
	encoded, err := backend.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := finalizeEncodedBody(encoded, req, backend, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTraceInfo(context.Background(), &TraceInfo{})
	out, err := New(nil, nil).prepareUpstreamRequestFor(ctx, backend, p, req, finalized)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawBody, rawInput) {
		t.Fatal("processing changed the caller body")
	}
	if !reflect.DeepEqual(snapshotIR(t, req), reqBefore) || !reflect.DeepEqual(snapshotProvider(t, p), providerBefore) {
		t.Fatal("processing changed the request or provider")
	}
	var got map[string]any
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("prepared body invalid: %v (%s)", err, out)
	}
	return got, out
}

func mustMapAny(t *testing.T, path string, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want object, got %T", path, v)
	}
	return m
}

func mustListAny(t *testing.T, path string, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want array, got %T", path, v)
	}
	return l
}

func requireMapMarker(t *testing.T, path string, v any, typ, ttl string) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: cache_control = %T, want object", path, v)
	}
	gotTTL, _ := m["ttl"].(string)
	if m["type"] != typ || gotTTL != ttl {
		t.Fatalf("%s: cache_control = %#v, want type %q ttl %q", path, m, typ, ttl)
	}
	if ttl == "" {
		if _, has := m["ttl"]; has {
			t.Fatalf("%s: empty ttl emitted: %#v", path, m)
		}
	}
}

func requireAbsentKey(t *testing.T, path string, m map[string]any, key string) {
	t.Helper()
	if _, ok := m[key]; ok {
		t.Fatalf("%s: unexpected %q: %#v", path, key, m[key])
	}
}

// requireEnvAppended verifies the free-tier environment section was appended
// after the caller text. Host-specific field values are not hardcoded.
func requireEnvAppended(t *testing.T, path, text, caller string) {
	t.Helper()
	heading := "Here is some useful information about the environment you are running in:"
	if !strings.HasPrefix(text, caller+"\n\n"+heading+"\n<env>\n") {
		t.Fatalf("%s: missing environment prefix: %q", path, text)
	}
	if !strings.HasSuffix(text, "\n</env>") {
		t.Fatalf("%s: missing environment suffix: %q", path, text)
	}
	for _, field := range []string{"\n  Working directory: ", "\n  Workspace root folder: ", "\n  Is directory a git repo: ", "\n  Platform: "} {
		if !strings.Contains(text, field) {
			t.Fatalf("%s: environment field %q missing: %q", path, field, text)
		}
	}
}

// TestOpencodeMessagesCacheControlPipeline covers the complete Anthropic
// ingress path into the real OpenCode Messages encoder for paid Zen/Go rows and
// the free Zen minimax row. Caller markers survive tool canonicalization and
// decoy injection; decoys stay unmarked; names, schemas, descriptions, IDs, and
// raw argument numbers are preserved.
func TestOpencodeMessagesCacheControlPipeline(t *testing.T) {
	const schema = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"minimum":1}`
	const requestFmt = `{
	  "model":"combo","stream":%t,"max_tokens":4000,
	  "system":"sys",
	  "tools":[
	    {"name":"lookup","description":"Find one record","input_schema":` + schema + `,"cache_control":{"type":"ephemeral","ttl":"5m"}},
	    {"name":"Read","description":"read a file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}
	  ],
	  "tool_choice":{"type":"tool","name":"lookup"},
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"plan","signature":"s","cache_control":{"type":"ephemeral","ttl":"1h"}},
	      {"type":"text","text":"answer","cache_control":{"type":"ephemeral","ttl":"5m"}},
	      {"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x","n":1.0},"cache_control":{"type":"ephemeral","ttl":"1h"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"sunny","cache_control":{"type":"ephemeral","ttl":"1h"}}],"cache_control":{"type":"ephemeral","ttl":"5m"}},
	      {"type":"text","text":"tail","cache_control":{"type":"ephemeral","ttl":"1h"}}
	    ]}
	  ]
	}`
	cases := []anthropicCacheCase{
		{name: "zen-paid-claude", base: opencode.ZenBaseURL, model: "claude-sonnet-4-5"},
		{name: "zen-paid-qwen", base: opencode.ZenBaseURL, model: "qwen3.6-plus"},
		{name: "go-paid-minimax25", base: opencode.GoBaseURL, model: "minimax-m2.5"},
		{name: "go-paid-minimax3", base: opencode.GoBaseURL, model: "minimax-m3"},
		{name: "zen-free-minimax3", base: opencode.ZenBaseURL, model: "minimax-m3-free", free: true},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				p := opencodeTestProvider()
				p.BaseURL = tc.base
				raw := []byte(fmt.Sprintf(requestFmt, stream))
				got, out := prepareAnthropicCachePipeline(t, p, anthropicCodec, raw, tc.model, stream)
				assertAnthropicCachePipeline(t, tc, stream, got, out)
			})
		}
	}
}

func assertAnthropicCachePipeline(t *testing.T, tc anthropicCacheCase, stream bool, got map[string]any, out []byte) {
	t.Helper()
	if got["model"] != tc.model {
		t.Fatalf("model = %#v, want %q", got["model"], tc.model)
	}
	if stream {
		if got["stream"] != true {
			t.Fatalf("stream = %#v, want true", got["stream"])
		}
	} else {
		requireAbsentKey(t, "body", got, "stream")
	}
	// System stays a flat string; no system block hints. Free rows carry the
	// environment section after the caller prompt.
	if tc.free {
		requireEnvAppended(t, "system", got["system"].(string), "sys")
	} else if got["system"] != "sys" {
		t.Fatalf("system = %#v, want flat string", got["system"])
	}
	if bytes.Count(out, []byte("cache_control")) != 7 {
		t.Fatalf("marker count = %d, want 7 (nested and thinking markers dropped): %s", bytes.Count(out, []byte("cache_control")), out)
	}

	tools := mustListAny(t, "tools", got["tools"])
	byName := map[string]map[string]any{}
	for i, rawTool := range tools {
		tool := mustMapAny(t, fmt.Sprintf("tools[%d]", i), rawTool)
		name, _ := tool["name"].(string)
		byName[name] = tool
	}
	lookup, ok := byName["lookup"]
	if !ok {
		t.Fatalf("caller tool lookup missing: %v", byName)
	}
	readName := "Read"
	if tc.free {
		readName = "read"
	}
	read, ok := byName[readName]
	if !ok {
		t.Fatalf("caller tool %q missing: %v", readName, byName)
	}
	requireMapMarker(t, "lookup tool", lookup["cache_control"], "ephemeral", "5m")
	requireMapMarker(t, readName+" tool", read["cache_control"], "ephemeral", "1h")
	if lookup["description"] != "Find one record" || read["description"] != "read a file" {
		t.Fatalf("descriptions changed: %#v", byName)
	}
	lookupSchema := mustMapAny(t, "lookup schema", lookup["input_schema"])
	props := mustMapAny(t, "lookup properties", lookupSchema["properties"])
	if lookupSchema["type"] != "object" || props["q"] == nil || lookupSchema["minimum"] != json.Number("1") {
		t.Fatalf("lookup schema changed: %#v", lookupSchema)
	}
	required := mustListAny(t, "lookup required", lookupSchema["required"])
	if len(required) != 1 || required[0] != "q" {
		t.Fatalf("lookup required changed: %#v", required)
	}
	if tc.free {
		if len(tools) != 5 {
			t.Fatalf("free tools = %d, want caller 2 + decoys 3", len(tools))
		}
		for _, name := range []string{"bash", "glob", "grep"} {
			decoy, ok := byName[name]
			if !ok {
				t.Fatalf("free decoy %q missing: %v", name, byName)
			}
			requireAbsentKey(t, "decoy "+name, decoy, "cache_control")
		}
	} else if len(tools) != 2 {
		t.Fatalf("paid tools = %d, want 2", len(tools))
	}
	choice := mustMapAny(t, "tool_choice", got["tool_choice"])
	if choice["type"] != "tool" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice changed: %#v", choice)
	}

	messages := mustListAny(t, "messages", got["messages"])
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(messages))
	}
	user0 := mustListAny(t, "messages[0]", mustMapAny(t, "messages[0]", messages[0])["content"])
	requireMapMarker(t, "user text", mustMapAny(t, "user text", user0[0])["cache_control"], "ephemeral", "")

	assistant := mustListAny(t, "messages[1]", mustMapAny(t, "messages[1]", messages[1])["content"])
	if len(assistant) != 3 {
		t.Fatalf("assistant blocks = %d, want 3", len(assistant))
	}
	thinking := mustMapAny(t, "thinking", assistant[0])
	if thinking["type"] != "thinking" || thinking["thinking"] != "plan" {
		t.Fatalf("thinking changed: %#v", thinking)
	}
	requireAbsentKey(t, "thinking", thinking, "cache_control")
	answer := mustMapAny(t, "assistant text", assistant[1])
	if answer["text"] != "answer" {
		t.Fatalf("assistant text changed: %#v", answer)
	}
	requireMapMarker(t, "assistant text", answer["cache_control"], "ephemeral", "5m")
	toolUse := mustMapAny(t, "assistant tool_use", assistant[2])
	if toolUse["id"] != "toolu_1" || toolUse["name"] != "lookup" {
		t.Fatalf("tool_use identity changed: %#v", toolUse)
	}
	requireMapMarker(t, "assistant tool_use", toolUse["cache_control"], "ephemeral", "1h")

	user1 := mustListAny(t, "messages[2]", mustMapAny(t, "messages[2]", messages[2])["content"])
	outer := mustMapAny(t, "outer tool_result", user1[0])
	if outer["type"] != "tool_result" || outer["tool_use_id"] != "toolu_1" {
		t.Fatalf("outer tool_result changed: %#v", outer)
	}
	requireMapMarker(t, "outer tool_result", outer["cache_control"], "ephemeral", "5m")
	nested := mustListAny(t, "nested content", outer["content"])
	if len(nested) != 1 || mustMapAny(t, "nested text", nested[0])["text"] != "sunny" {
		t.Fatalf("nested content changed: %#v", nested)
	}
	requireAbsentKey(t, "nested text", mustMapAny(t, "nested text", nested[0]), "cache_control")
	tail := mustMapAny(t, "tail text", user1[1])
	if tail["text"] != "tail" {
		t.Fatalf("tail text changed: %#v", tail)
	}
	requireMapMarker(t, "tail text", tail["cache_control"], "ephemeral", "1h")

	// Raw argument numbers survive the full pipeline as written by the caller.
	rawAssistant := mustListAny(t, "raw messages[1]", mustMapAny(t, "raw messages[1]", mustListAny(t, "raw messages", got["messages"])[1])["content"])
	rawInput := mustMapAny(t, "raw tool input", mustMapAny(t, "raw tool_use", rawAssistant[2])["input"])
	if n, ok := rawInput["n"].(json.Number); !ok || n.String() != "1.0" {
		t.Fatalf("raw argument number changed: %#v", rawInput["n"])
	}
}

// TestOpencodeMessagesSystemCacheControlPipeline drives a system block array
// with an explicit hint through the complete Anthropic ingress path. Caller
// tool and message markers stay additive, and free-tier decoys stay unmarked.
func TestOpencodeMessagesSystemCacheControlPipeline(t *testing.T) {
	const requestFmt = `{
	  "model":"combo","stream":%t,"max_tokens":4000,
	  "system":[
	    {"type":"text","text":"sys one"},
	    {"type":"text","text":" sys two","cache_control":{"type":"ephemeral","ttl":"1h"}}
	  ],
	  "tools":[
	    {"name":"lookup","description":"Find one record","input_schema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"cache_control":{"type":"ephemeral","ttl":"5m"}}
	  ],
	  "messages":[
	    {"role":"user","content":[
	      {"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},
	      {"type":"text","text":"tail","cache_control":{"type":"ephemeral","ttl":"5m"}}
	    ]}
	  ]
	}`
	cases := []anthropicCacheCase{
		{name: "zen-paid-claude", base: opencode.ZenBaseURL, model: "claude-sonnet-4-5"},
		{name: "go-paid-minimax3", base: opencode.GoBaseURL, model: "minimax-m3"},
		{name: "zen-free-minimax3", base: opencode.ZenBaseURL, model: "minimax-m3-free", free: true},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				p := opencodeTestProvider()
				p.BaseURL = tc.base
				raw := []byte(fmt.Sprintf(requestFmt, stream))
				got, out := prepareAnthropicCachePipeline(t, p, anthropicCodec, raw, tc.model, stream)
				if got["model"] != tc.model {
					t.Fatalf("model = %#v, want %q", got["model"], tc.model)
				}
				if stream {
					if got["stream"] != true {
						t.Fatalf("stream = %#v, want true", got["stream"])
					}
				} else {
					requireAbsentKey(t, "body", got, "stream")
				}

				sys := mustListAny(t, "system", got["system"])
				if len(sys) != 2 {
					t.Fatalf("system blocks = %d, want 2", len(sys))
				}
				first := mustMapAny(t, "system[0]", sys[0])
				if first["type"] != "text" || first["text"] != "sys one" {
					t.Fatalf("system[0] = %#v", first)
				}
				requireAbsentKey(t, "system[0]", first, "cache_control")
				second := mustMapAny(t, "system[1]", sys[1])
				if tc.free {
					if second["type"] != "text" {
						t.Fatalf("system[1] = %#v", second)
					}
					requireEnvAppended(t, "system[1]", second["text"].(string), " sys two")
				} else if second["type"] != "text" || second["text"] != " sys two" {
					t.Fatalf("system[1] = %#v", second)
				}
				requireMapMarker(t, "system[1]", second["cache_control"], "ephemeral", "1h")

				tools := mustListAny(t, "tools", got["tools"])
				if len(tools) == 0 {
					t.Fatal("tools missing")
				}
				lookup := mustMapAny(t, "tools[0]", tools[0])
				if lookup["name"] != "lookup" {
					t.Fatalf("caller tool renamed: %#v", lookup["name"])
				}
				requireMapMarker(t, "lookup tool", lookup["cache_control"], "ephemeral", "5m")
				props := mustMapAny(t, "lookup properties", mustMapAny(t, "lookup schema", lookup["input_schema"])["properties"])
				if props["q"] == nil {
					t.Fatalf("lookup schema changed: %#v", lookup["input_schema"])
				}
				for i := 1; i < len(tools); i++ {
					requireAbsentKey(t, fmt.Sprintf("decoy tool[%d]", i), mustMapAny(t, fmt.Sprintf("tools[%d]", i), tools[i]), "cache_control")
				}

				messages := mustListAny(t, "messages", got["messages"])
				if len(messages) != 1 {
					t.Fatalf("messages = %d, want 1", len(messages))
				}
				blocks := mustListAny(t, "messages[0]", mustMapAny(t, "messages[0]", messages[0])["content"])
				if len(blocks) != 2 {
					t.Fatalf("message blocks = %d, want 2", len(blocks))
				}
				hello := mustMapAny(t, "hello", blocks[0])
				if hello["text"] != "hello" {
					t.Fatalf("hello = %#v", hello)
				}
				requireMapMarker(t, "hello", hello["cache_control"], "ephemeral", "")
				tail := mustMapAny(t, "tail", blocks[1])
				if tail["text"] != "tail" {
					t.Fatalf("tail = %#v", tail)
				}
				requireMapMarker(t, "tail", tail["cache_control"], "ephemeral", "5m")

				if got := bytes.Count(out, []byte("cache_control")); got != 4 {
					t.Fatalf("marker count = %d, want 4: %s", got, out)
				}
			})
		}
	}
}

// TestAnthropicCacheControlInvalidHTTP drives the real mounted handler and
// asserts a malformed supported-position marker yields an Anthropic 400 with
// zero upstream contact and no caller text in the error.
func TestAnthropicCacheControlInvalidHTTP(t *testing.T) {
	const secret = "cache-secret-text-must-not-leak"
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
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
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "qwen3.6-plus", Enabled: true}}}); err != nil {
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

	body := fmt.Sprintf(`{"model":"default","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral","ttl":"2m"}}]}]}`, secret)
	streamBody := fmt.Sprintf(`{"model":"default","stream":true,"max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral","ttl":"2m"}}]}]}`, secret)
	systemBody := fmt.Sprintf(`{"model":"default","max_tokens":16,"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral","ttl":"2m"}}],"messages":[{"role":"user","content":"hi"}]}`, secret)
	systemStreamBody := fmt.Sprintf(`{"model":"default","stream":true,"max_tokens":16,"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral","ttl":"2m"}}],"messages":[{"role":"user","content":"hi"}]}`, secret)
	cases := []struct {
		name string
		body string
	}{
		{"message marker", body},
		{"message marker stream", streamBody},
		{"system marker", systemBody},
		{"system marker stream", systemStreamBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := hits
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(tc.body))
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
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body = %s, want 400", resp.StatusCode, out)
			}
			if hits != before {
				t.Fatalf("upstream contacted %d time(s) for an invalid marker", hits-before)
			}
			assertIngressErrorEnvelope(t, "/v1/messages", out, "invalid_request_error")
			if bytes.Contains(out, []byte(secret)) {
				t.Fatalf("error leaked caller text: %s", out)
			}
			if bytes.Contains(out, []byte("data:")) || bytes.Contains(out, []byte("event:")) {
				t.Fatalf("error leaked SSE: %s", out)
			}
		})
	}
}

// TestCacheControlDoesNotLeakIntoOtherCodecs decodes an Anthropic request with
// markers and encodes it with every non-Anthropic JSON backend. The known
// reasoning writers re-marshal via generic maps, so this guards the invariant
// that cache hints never become wire fields on those codecs. The Cursor encoder
// is protobuf and has no cache hint field or shared code path; its output is
// binary and is not string-scanned here.
func TestCacheControlDoesNotLeakIntoOtherCodecs(t *testing.T) {
	body := []byte(`{"model":"combo","max_tokens":16,
	  "system":[{"type":"text","text":"flat-system-text","cache_control":{"type":"ephemeral","ttl":"5m"}}],
	  "tools":[{"name":"lookup","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"5m"}}],
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"},"cache_control":{"type":"ephemeral"}},
	    {"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"r"}],"cache_control":{"type":"ephemeral","ttl":"5m"}}
	  ]}]}`)
	req, err := anthropicCodec.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "flat-system-text" || len(req.SystemBlocks) != 1 || req.SystemBlocks[0].CacheControl == nil {
		t.Fatalf("system = %q blocks = %+v", req.System, req.SystemBlocks)
	}
	encoders := []struct {
		name string
		enc  func(*ir.Request) ([]byte, error)
	}{
		{"oai-chat", openai.EncodeRequest},
		{"oai-responses", responses.EncodeRequest},
		{"oai-codex", responses.EncodeCodexRequest},
		{"kiro", kiro.EncodeRequest},
		{"kiro-runtime", kiro.EncodeRuntimeRequest},
		{"qoder", qoder.EncodeRequest},
		{"antigravity", antigravity.EncodeRequest},
	}
	for _, e := range encoders {
		raw, err := e.enc(req)
		if err != nil {
			t.Fatalf("%s: %v", e.name, err)
		}
		if bytes.Contains(raw, []byte("cache_control")) {
			t.Fatalf("%s leaked cache_control: %s", e.name, raw)
		}
	}
	// Ordinary text and tool semantics survive the translation.
	chat, err := openai.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(chat, []byte("hello")) || !bytes.Contains(chat, []byte("lookup")) {
		t.Fatalf("chat semantics lost: %s", chat)
	}
	if !bytes.Contains(chat, []byte("flat-system-text")) {
		t.Fatalf("chat dropped the flat system: %s", chat)
	}
	codex, err := responses.EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(codex, []byte("hello")) {
		t.Fatalf("codex semantics lost: %s", codex)
	}
	if !bytes.Contains(codex, []byte("flat-system-text")) {
		t.Fatalf("codex dropped the flat system: %s", codex)
	}

	// Stale structured blocks behind a changed flat string must not reach any
	// non-Anthropic encoder; no other codec reads SystemBlocks.
	req.System = "changed-flat-system"
	req.SystemBlocks = []ir.SystemBlock{{Text: "stale-structured-text", CacheControl: anthCacheHint("1h")}}
	for _, e := range encoders {
		raw, err := e.enc(req)
		if err != nil {
			t.Fatalf("%s: %v", e.name, err)
		}
		if bytes.Contains(raw, []byte("stale-structured-text")) || bytes.Contains(raw, []byte("cache_control")) {
			t.Fatalf("%s read stale structured system: %s", e.name, raw)
		}
	}
}

func TestSnapshotIRClonesResponsesReasoning(t *testing.T) {
	enc := "enc"
	req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{
			Type: ir.BlockReasoning, Text: "chain",
			ResponsesReasoning: &ir.ResponsesReasoning{
				ID: "rs_1", Status: "completed", EncryptedContent: &enc,
				Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
			},
			ToolResult: []ir.ContentBlock{{Type: ir.BlockText, Text: "nested"}},
		},
	}}}}
	snap := snapshotIR(t, req)
	orig := req.Messages[0].Content[0].ResponsesReasoning
	cp := snap.Messages[0].Content[0].ResponsesReasoning
	if cp == nil || cp == orig {
		t.Fatalf("snapshot aliased reasoning pointer: %+v", cp)
	}
	orig.ID = "mutated-id"
	orig.Status = "mutated-status"
	*orig.EncryptedContent = "mutated-cipher"
	orig.Summary[0].Text = "mutated-summary"
	orig.Summary = append(orig.Summary, ir.ReasoningSummaryPart{Type: "summary_text", Text: "extra"})
	req.Messages[0].Content[0].ToolResult[0].Text = "mutated-nested"
	if cp.ID != "rs_1" || cp.Status != "completed" || cp.EncryptedContent == nil || *cp.EncryptedContent != "enc" {
		t.Fatalf("snapshot metadata aliased: %+v", cp)
	}
	if len(cp.Summary) != 1 || cp.Summary[0].Text != "chain" {
		t.Fatalf("snapshot summary aliased: %+v", cp.Summary)
	}
	if snap.Messages[0].Content[0].ToolResult[0].Text != "nested" {
		t.Fatalf("nested tool result aliased: %+v", snap.Messages[0].Content[0].ToolResult)
	}
}

// opencodeTierUpstream records one upstream request and replies in the family
// and transport the request asked for. Tests reach it through a transport that
// dials this server for the official OpenCode host names, so codec and tier
// selection run against the real base URLs without network access.
type opencodeTierUpstream struct {
	hits    atomic.Int64
	path    string
	host    string
	auth    string
	ua      string
	session string
	body    map[string]any
}

func (u *opencodeTierUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.path = r.URL.Path
		u.host = r.Host
		u.auth = r.Header.Get("Authorization")
		u.ua = r.Header.Get("User-Agent")
		u.session = r.Header.Get("x-opencode-session")
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.body = body
		if strings.HasSuffix(r.URL.Path, "/responses") {
			if stream, _ := u.body["stream"].(bool); stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(responsesReasoningSuccessSSE))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(routingResponsesUpstreamBody))
			return
		}
		if stream, _ := u.body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
				"data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
}

func setupOpencodeTierProxy(t *testing.T, base, model, apiKey string) (*opencodeTierUpstream, string, string) {
	t.Helper()
	up := &opencodeTierUpstream{}
	server := httptest.NewTLSServer(up.handler(t))
	t.Cleanup(server.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "opencode", BaseURL: base, APIKey: apiKey, Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: model, Enabled: true},
	}}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	px := New(st, nil)
	// Every official OpenCode host dials the local TLS test server.
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(server.URL, "https://"))
		},
	}
	px.client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	px.streamClient = &http.Client{Transport: tr}
	mux := http.NewServeMux()
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return up, ts.URL, key.Token
}

// TestOpencodeTierHTTPLifecycle runs the real ingress -> IR -> OpenCode
// encode/finalize/prepare path over local HTTP for Zen paid, Zen free, and Go,
// with the client stream flag both off (normal unary or forced stream-to-unary)
// and on (SSE). Each case sends two requests through the same proxy and checks
// the second body does not inherit the first request's state.
func TestOpencodeTierHTTPLifecycle(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		model    string
		apiKey   string
		free     bool
		endpoint string
		prefix   string
	}{
		{"zen paid chat", opencode.ZenBaseURL, "deepseek-v4-pro", "sk-zen", false, "/chat/completions", "/inference/v1"},
		{"zen paid responses", opencode.ZenBaseURL, "gpt-5.6-luna", "sk-zen", false, "/responses", "/inference/v1"},
		{"zen free chat", opencode.ZenBaseURL, "deepseek-v4-flash-free", opencode.PublicKey, true, "/chat/completions", "/inference/v1"},
		{"zen free responses", opencode.ZenBaseURL, "muse-spark-1.3-contributor-free", opencode.PublicKey, true, "/responses", "/inference/v1"},
		{"go paid chat", opencode.GoBaseURL, "glm-5", "sk-go", false, "/chat/completions", "/zen/go/v1"},
		{"go paid responses", opencode.GoBaseURL, "muse-spark-1.2-contributor", "sk-go", false, "/responses", "/zen/go/v1"},
	}
	const tools = `"tools":[{"type":"function","name":"Read","parameters":{"type":"object","properties":{}}}]`
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				up, clientBase, token := setupOpencodeTierProxy(t, tc.base, tc.model, tc.apiKey)
				first := fmt.Sprintf(`{"model":"default","stream":%t,"input":"hi","prompt_cache_key":"key-one",%s}`, stream, tools)
				resp, out := post(t, clientBase+"/v1/responses", token, first)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d body=%s", resp.StatusCode, out)
				}
				if !strings.Contains(string(out), "ok") {
					t.Fatalf("response lost text: %s", out)
				}
				if stream {
					if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
						t.Fatalf("stream content type = %q", ct)
					}
				} else if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
					t.Fatalf("unary content type = %q", ct)
				}
				if up.host != "opencode.ai" {
					t.Fatalf("host = %q", up.host)
				}
				if up.path != tc.prefix+tc.endpoint {
					t.Fatalf("path = %q want %q", up.path, tc.prefix+tc.endpoint)
				}
				if up.auth != "Bearer "+tc.apiKey {
					t.Fatalf("auth = %q", up.auth)
				}
				if !strings.HasPrefix(up.ua, "opencode/") {
					t.Fatalf("user agent = %q", up.ua)
				}
				if up.session == "" {
					t.Fatal("missing x-opencode-session header")
				}
				if !tc.free {
					streamVal, _ := up.body["stream"].(bool)
					if streamVal != stream {
						t.Fatalf("upstream stream = %v, want %v", up.body["stream"], stream)
					}
				} else if streamVal, _ := up.body["stream"].(bool); !streamVal {
					t.Fatalf("free stream-only target did not force stream: %#v", up.body["stream"])
				}
				toolName := func() string {
					list, _ := up.body["tools"].([]any)
					if len(list) == 0 {
						t.Fatalf("preparation lost tools: %#v", up.body)
					}
					first := mustMapAny(t, "tools[0]", list[0])
					if tc.endpoint == "/responses" {
						name, _ := first["name"].(string)
						return name
					}
					name, _ := mustMapAny(t, "tools[0].function", first["function"])["name"].(string)
					return name
				}
				if tc.free {
					if got := toolName(); got != "read" {
						t.Fatalf("free canonical tool name = %q", got)
					}
					if list, _ := up.body["tools"].([]any); len(list) < 2 {
						t.Fatalf("free preparation injected no decoys: %#v", up.body["tools"])
					}
				} else if got := toolName(); got != "Read" {
					t.Fatalf("paid tool name = %q, want caller spelling", got)
				}
				if tc.endpoint == "/responses" {
					if up.body["prompt_cache_key"] != "key-one" {
						t.Fatalf("responses key = %#v", up.body["prompt_cache_key"])
					}
				} else if _, ok := up.body["prompt_cache_key"]; ok {
					t.Fatalf("chat backend gained a responses-only key: %#v", up.body)
				}

				second := fmt.Sprintf(`{"model":"default","stream":%t,"input":"hi",%s}`, stream, tools)
				resp2, out2 := post(t, clientBase+"/v1/responses", token, second)
				if resp2.StatusCode != http.StatusOK {
					t.Fatalf("second status=%d body=%s", resp2.StatusCode, out2)
				}
				if !strings.Contains(string(out2), "ok") {
					t.Fatalf("second response lost text: %s", out2)
				}
				rawBody, _ := json.Marshal(up.body)
				if strings.Contains(string(rawBody), "key-one") {
					t.Fatalf("second request inherited prior key: %s", rawBody)
				}
				if up.hits.Load() != 2 {
					t.Fatalf("upstream hits = %d, want 2", up.hits.Load())
				}
			})
		}
	}
}

func respStrPtr(s string) *string { return &s }

func assertRawString(t *testing.T, path string, raw json.RawMessage, want string) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("%s: field absent, want %q", path, want)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// assertOptionalRawString checks exact presence: a non-nil want must be present
// with that exact value (including the empty string); a nil want must be absent.
func assertOptionalRawString(t *testing.T, path string, raw json.RawMessage, want *string) {
	t.Helper()
	if want == nil {
		if len(raw) != 0 {
			t.Fatalf("%s = %s, want absent", path, raw)
		}
		return
	}
	assertRawString(t, path, raw, *want)
}

// opencodeEncryptedHistoryBody returns a Responses request whose input carries
// the complete mixed history: an opaque reasoning item with ordered summary
// parts including an empty part, assistant text, a function_call, its
// function_call_output, and a trailing user turn. Tool parameters keep a nested
// value and a large integer literal so preparation can be checked exactly.
func opencodeEncryptedHistoryBody(stream bool) string {
	return fmt.Sprintf(`{"model":"default","stream":%t,"prompt_cache_key":"caller-cache-key","store":false,"include":["reasoning.encrypted_content","custom.include"],"reasoning":{"effort":"max"},"max_output_tokens":4,"tools":[{"type":"function","name":"Read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","default":9007199254740993},"nested":{"type":"object","properties":{"deep":{"type":"integer","const":9007199254740993}}}},"required":["path"]}}],"input":[{"type":"reasoning","id":"rs_hist","status":"completed","encrypted_content":"cipher-hist","summary":[{"type":"summary_text","text":"part-one"},{"type":"summary_text","text":""},{"type":"summary_text","text":"part-three"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"assistant-answer"}]},{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"/tmp/x\",\"limit\":9007199254740993}"},{"type":"function_call_output","call_id":"call_1","output":"tool-output-value"},{"type":"message","role":"user","content":[{"type":"input_text","text":"final-user"}]}]}`, stream)
}

// TestOpencodeResponsesEncryptedHistoryPrepareMatrix drives the full
// ingress -> IR -> OpenCode Responses encode -> finalize -> prepare path for
// the paid Zen, free Zen muse, and Go muse Responses rows and both client
// stream flags. It checks real codec selection (opencode-responses, not the
// generic oai-responses), the forced stream:true for the free stream-only row,
// and the whole supported history after preparation: order, item IDs, status,
// ciphertext, ordered summary parts including an empty part, tool schema with a
// nested value and a large integer literal, store/include/cache intent, and the
// independent caller session.
func TestOpencodeResponsesEncryptedHistoryPrepareMatrix(t *testing.T) {
	const headerSession = "ses_0123456789abABCDEFGHIJKLMN"
	cases := []struct {
		name  string
		base  string
		model string
		free  bool
		spark bool
	}{
		{name: "zen paid responses", base: opencode.ZenBaseURL, model: "gpt-5.6-luna"},
		{name: "zen free muse responses", base: opencode.ZenBaseURL, model: "muse-spark-1.3-contributor-free", free: true, spark: true},
		{name: "go paid muse responses", base: opencode.GoBaseURL, model: "muse-spark-1.2-contributor", spark: true},
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
				if backend.id == responsesCodec.id {
					t.Fatalf("OpenCode backend reused the generic Responses id %q", backend.id)
				}
				if backend.streamOnly != tc.free {
					t.Fatalf("streamOnly = %v, want %v", backend.streamOnly, tc.free)
				}
				h := http.Header{}
				h.Set("x-opencode-session", headerSession)
				raw := []byte(opencodeEncryptedHistoryBody(stream))
				trace := &TraceInfo{}
				ctx := WithTraceInfo(context.Background(), trace)
				ctx = withOpencodeRequest(ctx, "test-nonce", h, raw)
				_, out := prepareTranslatedPipelineWith(t, New(nil, nil), p, responsesCodec, raw, tc.model, ctx, true)

				var got map[string]any
				dec := json.NewDecoder(bytes.NewReader(out))
				dec.UseNumber()
				if err := dec.Decode(&got); err != nil {
					t.Fatalf("prepared body invalid: %v", err)
				}
				wantStream := stream || tc.free
				if got["stream"] != wantStream {
					t.Fatalf("prepared stream = %#v, want %v", got["stream"], wantStream)
				}
				if got["model"] != tc.model {
					t.Fatalf("prepared model = %#v", got["model"])
				}
				if store, ok := got["store"].(bool); !ok || store {
					t.Fatalf("prepared store = %#v, want explicit false", got["store"])
				}
				if !reflect.DeepEqual(got["include"], []any{"reasoning.encrypted_content", "custom.include"}) {
					t.Fatalf("prepared include = %#v", got["include"])
				}
				if got["prompt_cache_key"] != "caller-cache-key" {
					t.Fatalf("prepared key = %#v", got["prompt_cache_key"])
				}
				wantEffort := "max"
				wantMax := int64(4)
				if tc.spark {
					wantEffort = "xhigh"
					wantMax = 16
				}
				if effort := mustMapAny(t, "reasoning", got["reasoning"])["effort"]; effort != wantEffort {
					t.Fatalf("reasoning effort = %#v, want %q", effort, wantEffort)
				}
				maxRaw, ok := got["max_output_tokens"].(json.Number)
				if !ok {
					t.Fatalf("max_output_tokens = %#v", got["max_output_tokens"])
				}
				if n, _ := maxRaw.Int64(); n != wantMax {
					t.Fatalf("max_output_tokens = %s, want %d", maxRaw, wantMax)
				}
				if trace.OpencodeSessionID != headerSession {
					t.Fatalf("session = %q, want %q", trace.OpencodeSessionID, headerSession)
				}

				wantName := "Read"
				tools := mustListAny(t, "tools", got["tools"])
				if tc.free {
					wantName = "read"
					if len(tools) != 4 {
						t.Fatalf("free tools = %d, want caller plus 3 decoys", len(tools))
					}
				} else if len(tools) != 1 {
					t.Fatalf("paid tools = %d, want 1", len(tools))
				}
				caller := false
				for _, item := range tools {
					if mustMapAny(t, "tools[]", item)["name"] == wantName {
						caller = true
					}
				}
				if !caller {
					t.Fatalf("prepared tools lost %q: %#v", wantName, tools)
				}
				if !bytes.Contains(out, []byte("9007199254740993")) {
					t.Fatalf("prepared tool schema rounded or dropped the large integer: %s", out)
				}
				assertPreparedToolLargeInteger(t, out, wantName)
				assertPreparedEncryptedHistory(t, out, wantName)
			})
		}
	}
}

// TestOpencodeResponsesEncryptedHistoryOpaqueVariants pins the exact opaque
// presence variants through the real OpenCode Responses pipeline: non-empty and
// explicit empty ciphertext, absent and null ciphertext, absent status and ID,
// and nil, empty, and empty-part summary arrays. The prepare helper also checks
// that the caller bytes, IR, and provider are unchanged.
func TestOpencodeResponsesEncryptedHistoryOpaqueVariants(t *testing.T) {
	cases := []struct {
		name      string
		reasoning string
		wantID    *string
		wantStat  *string
		wantEnc   *string
		wantTexts []string
	}{
		{
			name:      "cipher and ordered parts",
			reasoning: `{"type":"reasoning","id":"rs_a","status":"completed","encrypted_content":"cipher","summary":[{"type":"summary_text","text":"one"},{"type":"summary_text","text":""},{"type":"summary_text","text":"three"}]}`,
			wantID:    respStrPtr("rs_a"),
			wantStat:  respStrPtr("completed"),
			wantEnc:   respStrPtr("cipher"),
			wantTexts: []string{"one", "", "three"},
		},
		{
			name:      "explicit empty cipher",
			reasoning: `{"type":"reasoning","id":"rs_b","status":"in_progress","encrypted_content":"","summary":[]}`,
			wantID:    respStrPtr("rs_b"),
			wantStat:  respStrPtr("in_progress"),
			wantEnc:   respStrPtr(""),
			wantTexts: []string{},
		},
		{
			name:      "absent cipher",
			reasoning: `{"type":"reasoning","id":"rs_c","status":"completed","summary":[{"type":"summary_text","text":"only"}]}`,
			wantID:    respStrPtr("rs_c"),
			wantStat:  respStrPtr("completed"),
			wantEnc:   nil,
			wantTexts: []string{"only"},
		},
		{
			name:      "null cipher and null summary",
			reasoning: `{"type":"reasoning","id":"rs_d","encrypted_content":null,"summary":null}`,
			wantID:    respStrPtr("rs_d"),
			wantStat:  nil,
			wantEnc:   nil,
			wantTexts: nil,
		},
		{
			name:      "empty summary text part only",
			reasoning: `{"type":"reasoning","id":"rs_e","status":"completed","encrypted_content":"c","summary":[{"type":"summary_text","text":""}]}`,
			wantID:    respStrPtr("rs_e"),
			wantStat:  respStrPtr("completed"),
			wantEnc:   respStrPtr("c"),
			wantTexts: []string{""},
		},
		{
			name:      "no id no status",
			reasoning: `{"type":"reasoning","encrypted_content":"c","summary":[]}`,
			wantID:    nil,
			wantStat:  nil,
			wantEnc:   respStrPtr("c"),
			wantTexts: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := opencodeTestProvider()
			body := []byte(fmt.Sprintf(`{"model":"combo","input":[%s]}`, tc.reasoning))
			_, out := prepareTranslatedPipelineWith(t, New(nil, nil), p, responsesCodec, body, "gpt-5.6-luna", WithTraceInfo(context.Background(), &TraceInfo{}), true)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(out, &fields); err != nil {
				t.Fatal(err)
			}
			var items []map[string]json.RawMessage
			if err := json.Unmarshal(fields["input"], &items); err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 {
				t.Fatalf("history items = %d, want 1: %s", len(items), fields["input"])
			}
			assertRawString(t, "type", items[0]["type"], "reasoning")
			assertOptionalRawString(t, "id", items[0]["id"], tc.wantID)
			assertOptionalRawString(t, "status", items[0]["status"], tc.wantStat)
			assertOptionalRawString(t, "encrypted_content", items[0]["encrypted_content"], tc.wantEnc)
			var summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(items[0]["summary"], &summary); err != nil {
				t.Fatal(err)
			}
			if len(summary) != len(tc.wantTexts) {
				t.Fatalf("summary = %#v, want %d parts", summary, len(tc.wantTexts))
			}
			for i, want := range tc.wantTexts {
				if summary[i].Type != "summary_text" || summary[i].Text != want {
					t.Fatalf("summary[%d] = %#v, want type summary_text text %q", i, summary[i], want)
				}
			}
		})
	}
}

// TestSnapshotIRClonesResponsesReasoningEmptyVariants guards the pipeline
// snapshots for the opaque presence variants: absent, nil encrypted, explicit
// empty encrypted, nil versus non-nil empty summary, and an empty summary part.
func TestSnapshotIRClonesResponsesReasoningEmptyVariants(t *testing.T) {
	empty := "cipher-empty"
	nilSummary := &ir.ResponsesReasoning{ID: "rs_nil", EncryptedContent: &empty}
	explicitEmpty := &ir.ResponsesReasoning{Summary: []ir.ReasoningSummaryPart{}}
	req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "no-state"},
		{Type: ir.BlockReasoning, Text: "nil-summary", ResponsesReasoning: nilSummary},
		{Type: ir.BlockReasoning, Text: "empty-summary", ResponsesReasoning: explicitEmpty},
		{Type: ir.BlockReasoning, Text: "empty-part", ResponsesReasoning: &ir.ResponsesReasoning{
			ID: "rs_part", EncryptedContent: respStrPtr(""), Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: ""}},
		}},
	}}}}
	snap := snapshotIR(t, req)
	blocks := snap.Messages[0].Content
	if blocks[0].ResponsesReasoning != nil {
		t.Fatalf("nil reasoning was snapshotted as non-nil: %+v", blocks[0].ResponsesReasoning)
	}
	if blocks[1].ResponsesReasoning == nil || blocks[1].ResponsesReasoning == nilSummary || blocks[1].ResponsesReasoning.EncryptedContent == nil || *blocks[1].ResponsesReasoning.EncryptedContent != "cipher-empty" {
		t.Fatalf("encrypted pointer aliased or lost: %+v", blocks[1].ResponsesReasoning)
	}
	if blocks[2].ResponsesReasoning == nil || len(blocks[2].ResponsesReasoning.Summary) != 0 {
		t.Fatalf("empty summary did not stay empty: %+v", blocks[2].ResponsesReasoning)
	}
	if blocks[3].ResponsesReasoning == nil || blocks[3].ResponsesReasoning.EncryptedContent == nil || *blocks[3].ResponsesReasoning.EncryptedContent != "" {
		t.Fatalf("explicit empty ciphertext lost: %+v", blocks[3].ResponsesReasoning)
	}
	if blocks[3].ResponsesReasoning.Summary == nil || len(blocks[3].ResponsesReasoning.Summary) != 1 || blocks[3].ResponsesReasoning.Summary[0].Text != "" {
		t.Fatalf("empty summary part lost: %+v", blocks[3].ResponsesReasoning.Summary)
	}
	// Mutate the live state through every shared reference and confirm the
	// snapshot does not observe any of it. Clone collapses a non-nil empty
	// summary slice to nil, which is wire-equivalent because encoding emits an
	// empty array for both, so the live slice is still checked for aliasing.
	live := req.Messages[0].Content
	*live[1].ResponsesReasoning.EncryptedContent = "mutated"
	live[2].ResponsesReasoning.Summary = append(live[2].ResponsesReasoning.Summary, ir.ReasoningSummaryPart{Type: "summary_text", Text: "added"})
	*live[3].ResponsesReasoning.EncryptedContent = "mutated"
	live[3].ResponsesReasoning.Summary[0].Text = "mutated"
	if *blocks[1].ResponsesReasoning.EncryptedContent != "cipher-empty" {
		t.Fatal("snapshot observed a live encrypted mutation")
	}
	if len(blocks[2].ResponsesReasoning.Summary) != 0 {
		t.Fatal("snapshot observed a live summary append")
	}
	if *blocks[3].ResponsesReasoning.EncryptedContent != "" || blocks[3].ResponsesReasoning.Summary[0].Text != "" {
		t.Fatal("snapshot observed a live empty-state mutation")
	}
}

// opencodeHistoryUpstream records every translated upstream request the proxy
// makes in the encrypted-history HTTP matrix. Requests run one after another,
// but the mutex keeps the race detector honest.
type opencodeHistoryUpstream struct {
	mu       sync.Mutex
	bodies   []map[string]any
	raw      [][]byte
	hosts    []string
	paths    []string
	sessions []string
	auths    []string
}

func (u *opencodeHistoryUpstream) record(r *http.Request) []byte {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies = append(u.bodies, body)
	u.raw = append(u.raw, raw)
	u.hosts = append(u.hosts, r.Host)
	u.paths = append(u.paths, r.URL.Path)
	u.sessions = append(u.sessions, r.Header.Get("x-opencode-session"))
	u.auths = append(u.auths, r.Header.Get("Authorization"))
	return raw
}

func (u *opencodeHistoryUpstream) last() ([]byte, map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.raw[len(u.raw)-1], u.bodies[len(u.bodies)-1]
}

func (u *opencodeHistoryUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.raw)
}

func opencodeHistoryUnaryBody(toolName string) string {
	return `{"id":"resp_up","model":"m","status":"completed","output":[` +
		`{"id":"rs_resp","type":"reasoning","status":"completed","encrypted_content":"cipher-resp","summary":[{"type":"summary_text","text":"resp-part-one"},{"type":"summary_text","text":""},{"type":"summary_text","text":"resp-part-three"}]},` +
		`{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"resp-ok"}]},` +
		`{"id":"fc_resp","type":"function_call","status":"completed","call_id":"call_resp","name":` + jsonString(toolName) + `,"arguments":"{\"path\":\"/tmp/y\",\"limit\":9007199254740993}"}` +
		`],"usage":{"input_tokens":3,"output_tokens":2}}`
}

func opencodeHistorySSEBody(toolName string) string {
	reasoning := `{"id":"rs_resp","type":"reasoning","status":"completed","encrypted_content":"cipher-resp","summary":[{"type":"summary_text","text":"resp-part-one"},{"type":"summary_text","text":""},{"type":"summary_text","text":"resp-part-three"}]}`
	message := `{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"resp-ok"}]}`
	call := `{"id":"fc_resp","type":"function_call","status":"completed","call_id":"call_resp","name":` + jsonString(toolName) + `,"arguments":"{\"path\":\"/tmp/y\",\"limit\":9007199254740993}"}`
	return joinSSE(
		"event: response.created\ndata: "+`{"type":"response.created","response":{"id":"resp_up","model":"m","status":"in_progress","output":[]}}`,
		"event: response.output_item.done\ndata: "+`{"type":"response.output_item.done","output_index":0,"item":`+reasoning+`}`,
		"event: response.output_text.delta\ndata: "+`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"resp-ok"}`,
		"event: response.output_item.added\ndata: "+`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_resp","name":`+jsonString(toolName)+`,"arguments":""}}`,
		"event: response.function_call_arguments.delta\ndata: "+`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":\"/tmp/y\",\"limit\":9007199254740993}"}`,
		"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_up","model":"m","status":"completed","output":[`+reasoning+`,`+message+`,`+call+`],"usage":{"input_tokens":3,"output_tokens":2}}}`,
	)
}

func opencodeHistoryPlainUnaryBody() string {
	return `{"id":"resp_plain","model":"m","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"plain-ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
}

func opencodeHistoryPlainSSEBody() string {
	return joinSSE(
		"event: response.created\ndata: "+`{"type":"response.created","response":{"id":"resp_plain","model":"m","status":"in_progress","output":[]}}`,
		"event: response.output_text.delta\ndata: "+`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"plain-ok"}`,
		"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_plain","model":"m","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"plain-ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
}

// historyRequestHasOpaque reports whether the upstream request still carries an
// opaque reasoning item. The plain isolation turn has none, so its mock response
// must not call a tool (a free-tier decoy call would be rejected).
func historyRequestHasOpaque(body map[string]any) bool {
	items, _ := body["input"].([]any)
	for _, item := range items {
		m, _ := item.(map[string]any)
		if m["type"] == "reasoning" {
			return true
		}
	}
	return false
}

func setupOpencodeHistoryProxy(t *testing.T, base, model, apiKey, toolName string) (*opencodeHistoryUpstream, string, string) {
	t.Helper()
	up := &opencodeHistoryUpstream{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := up.record(r)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		withTool := historyRequestHasOpaque(body)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			if withTool {
				_, _ = w.Write([]byte(opencodeHistorySSEBody(toolName)))
			} else {
				_, _ = w.Write([]byte(opencodeHistoryPlainSSEBody()))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if withTool {
			_, _ = w.Write([]byte(opencodeHistoryUnaryBody(toolName)))
		} else {
			_, _ = w.Write([]byte(opencodeHistoryPlainUnaryBody()))
		}
	}))
	t.Cleanup(server.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "opencode", BaseURL: base, APIKey: apiKey, Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: model, Enabled: true},
	}}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	px := New(st, nil)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial(network, strings.TrimPrefix(server.URL, "https://"))
		},
	}
	px.client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	px.streamClient = &http.Client{Transport: tr}
	mux := http.NewServeMux()
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return up, ts.URL, key.Token
}

func postWithHeaders(t *testing.T, url, token, body string, h http.Header) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

// assertPreparedEncryptedHistory checks the full decoded history on a prepared
// upstream body: item order, reasoning ID/status/ciphertext, ordered summary
// parts including an empty part, assistant text, function_call name and
// arguments, function_call_output, and the trailing user turn.
func assertPreparedEncryptedHistory(t *testing.T, out []byte, functionCallName string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(fields["input"], &items); err != nil {
		t.Fatalf("prepared input: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("history items = %d, want 5: %s", len(items), fields["input"])
	}
	assertRawString(t, "input[0].type", items[0]["type"], "reasoning")
	assertRawString(t, "input[0].id", items[0]["id"], "rs_hist")
	assertRawString(t, "input[0].status", items[0]["status"], "completed")
	assertRawString(t, "input[0].encrypted_content", items[0]["encrypted_content"], "cipher-hist")
	var summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(items[0]["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary) != 3 || summary[0].Text != "part-one" || summary[1].Text != "" || summary[2].Text != "part-three" {
		t.Fatalf("summary = %#v", summary)
	}
	assertRawString(t, "input[1].type", items[1]["type"], "message")
	assertRawString(t, "input[1].role", items[1]["role"], "assistant")
	if !bytes.Contains(items[1]["content"], []byte("assistant-answer")) {
		t.Fatalf("assistant text lost: %s", items[1]["content"])
	}
	assertRawString(t, "input[2].type", items[2]["type"], "function_call")
	assertRawString(t, "input[2].call_id", items[2]["call_id"], "call_1")
	assertRawString(t, "input[2].name", items[2]["name"], functionCallName)
	var args string
	if err := json.Unmarshal(items[2]["arguments"], &args); err != nil {
		t.Fatal(err)
	}
	if args != `{"path":"/tmp/x","limit":9007199254740993}` {
		t.Fatalf("function_call arguments = %q", args)
	}
	assertRawString(t, "input[3].type", items[3]["type"], "function_call_output")
	assertRawString(t, "input[3].call_id", items[3]["call_id"], "call_1")
	assertRawString(t, "input[3].output", items[3]["output"], "tool-output-value")
	assertRawString(t, "input[4].type", items[4]["type"], "message")
	assertRawString(t, "input[4].role", items[4]["role"], "user")
	if !bytes.Contains(items[4]["content"], []byte("final-user")) {
		t.Fatalf("final user text lost: %s", items[4]["content"])
	}
}

// assertPreparedToolLargeInteger finds the caller tool by name and checks that
// its nested object and both large integer literals survive preparation exactly.
// It decodes with UseNumber so a float conversion would be caught as a changed
// json.Number, independent of JSON key order.
func assertPreparedToolLargeInteger(t *testing.T, raw []byte, toolName string) {
	t.Helper()
	var root struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	for _, tool := range root.Tools {
		var name string
		if err := json.Unmarshal(tool["name"], &name); err != nil || name != toolName {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(tool["parameters"]))
		dec.UseNumber()
		var params map[string]any
		if err := dec.Decode(&params); err != nil {
			t.Fatal(err)
		}
		props, _ := params["properties"].(map[string]any)
		limit, _ := props["limit"].(map[string]any)
		if n, ok := limit["default"].(json.Number); !ok || n.String() != "9007199254740993" {
			t.Fatalf("limit.default = %#v, want json.Number 9007199254740993", limit["default"])
		}
		nested, _ := props["nested"].(map[string]any)
		nestedProps, _ := nested["properties"].(map[string]any)
		deep, _ := nestedProps["deep"].(map[string]any)
		if n, ok := deep["const"].(json.Number); !ok || n.String() != "9007199254740993" {
			t.Fatalf("nested deep const = %#v, want json.Number 9007199254740993", deep["const"])
		}
		return
	}
	t.Fatalf("prepared tool %q not found", toolName)
}

func assertHistoryReasoningItem(t *testing.T, path string, item map[string]json.RawMessage) {
	t.Helper()
	assertRawString(t, path+".type", item["type"], "reasoning")
	assertRawString(t, path+".id", item["id"], "rs_resp")
	assertRawString(t, path+".status", item["status"], "completed")
	assertRawString(t, path+".encrypted_content", item["encrypted_content"], "cipher-resp")
	var summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(item["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary) != 3 || summary[0].Type != "summary_text" || summary[0].Text != "resp-part-one" || summary[1].Text != "" || summary[2].Text != "resp-part-three" {
		t.Fatalf("%s.summary = %#v", path, summary)
	}
}

// opencodeHistoryOutput returns the terminal output array from a unary client
// response or from a stream's response.completed event.
func opencodeHistoryOutput(t *testing.T, stream bool, body []byte) json.RawMessage {
	t.Helper()
	if !stream {
		var obj struct {
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("unary response: %v (%s)", err, body)
		}
		if len(obj.Output) == 0 {
			t.Fatalf("unary response lost output: %s", body)
		}
		return obj.Output
	}
	reader := sse.NewReader(bytes.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Output json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Data, &frame) != nil {
			continue
		}
		if frame.Type == "response.completed" && len(frame.Response.Output) > 0 {
			return frame.Response.Output
		}
	}
	t.Fatalf("stream lost response.completed output: %s", body)
	return nil
}

// opencodeHistoryItemDoneItems collects the items from every
// response.output_item.done frame in order.
func opencodeHistoryItemDoneItems(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var items []map[string]json.RawMessage
	reader := sse.NewReader(bytes.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type string          `json:"type"`
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(ev.Data, &frame) != nil {
			continue
		}
		if frame.Type != "response.output_item.done" {
			continue
		}
		var item map[string]json.RawMessage
		if err := json.Unmarshal(frame.Item, &item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

// opencodeHistoryReplayBody feeds the assistant output items from the first
// response back as the next request input, then adds the tool result and a
// follow-up user turn. It carries the same tool declaration and explicit
// include/cache intent so the replay is a realistic continuation.
func opencodeHistoryReplayBody(t *testing.T, stream bool, output json.RawMessage) string {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(output, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) < 3 {
		t.Fatalf("first response output items = %d, want at least 3", len(items))
	}
	input := make([]json.RawMessage, 0, len(items)+2)
	input = append(input, items...)
	input = append(input,
		json.RawMessage(`{"type":"function_call_output","call_id":"call_resp","output":"tool-out"}`),
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`),
	)
	body := map[string]any{
		"model":            "default",
		"stream":           stream,
		"input":            input,
		"store":            false,
		"include":          []string{"reasoning.encrypted_content"},
		"prompt_cache_key": "replay-key",
		"tools":            []map[string]any{{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestOpencodeTierEncryptedHistoryHTTPLifecycle drives the real ingress HTTP
// path for the paid Zen, free Zen muse, and Go muse Responses rows and both
// client stream flags. It sends an encrypted-history first turn, checks the
// upstream wire body and the client response (unary or SSE, including the
// output_item.done frame and final response.completed state), replays the
// returned opaque output as the next turn to the same selected provider and
// checks continuity is forwarded, then sends a plain turn through the same
// Proxy and checks nothing was inherited.
func TestOpencodeTierEncryptedHistoryHTTPLifecycle(t *testing.T) {
	const headerSession = "ses_0123456789abABCDEFGHIJKLMN"
	cases := []struct {
		name     string
		base     string
		model    string
		apiKey   string
		toolName string
		free     bool
		prefix   string
	}{
		{"zen paid responses", opencode.ZenBaseURL, "gpt-5.6-luna", "sk-zen", "Read", false, "/inference/v1"},
		{"zen free muse responses", opencode.ZenBaseURL, "muse-spark-1.3-contributor-free", opencode.PublicKey, "read", true, "/inference/v1"},
		{"go paid muse responses", opencode.GoBaseURL, "muse-spark-1.2-contributor", "sk-go", "Read", false, "/zen/go/v1"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				up, clientBase, token := setupOpencodeHistoryProxy(t, tc.base, tc.model, tc.apiKey, tc.toolName)
				h := http.Header{}
				h.Set("x-opencode-session", headerSession)
				resp, out := postWithHeaders(t, clientBase+"/v1/responses", token, opencodeEncryptedHistoryBody(stream), h)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("first status=%d body=%s", resp.StatusCode, out)
				}

				// 1. The upstream received the complete supported history.
				raw1, body1 := up.last()
				if body1["model"] != tc.model {
					t.Fatalf("upstream model = %#v, want %q", body1["model"], tc.model)
				}
				wantStream := stream || tc.free
				if body1["stream"] != wantStream {
					t.Fatalf("upstream stream = %#v, want %v", body1["stream"], wantStream)
				}
				if !bytes.Contains(raw1, []byte("9007199254740993")) {
					t.Fatalf("upstream schema lost the large integer: %s", raw1)
				}
				assertPreparedToolLargeInteger(t, raw1, tc.toolName)
				if store, ok := body1["store"].(bool); !ok || store {
					t.Fatalf("upstream store = %#v, want explicit false", body1["store"])
				}
				if !reflect.DeepEqual(body1["include"], []any{"reasoning.encrypted_content", "custom.include"}) {
					t.Fatalf("upstream include = %#v", body1["include"])
				}
				if body1["prompt_cache_key"] != "caller-cache-key" {
					t.Fatalf("upstream cache key = %#v", body1["prompt_cache_key"])
				}
				assertPreparedEncryptedHistory(t, raw1, tc.toolName)
				if tc.free {
					if !bytes.Contains(raw1, []byte(`"name":"read"`)) {
						t.Fatalf("free upstream did not canonicalize the caller tool: %s", raw1)
					}
				} else if !bytes.Contains(raw1, []byte(`"name":"Read"`)) {
					t.Fatalf("paid upstream changed the caller tool: %s", raw1)
				}
				if up.hosts[0] != "opencode.ai" || up.paths[0] != tc.prefix+"/responses" {
					t.Fatalf("upstream destination = %s%s", up.hosts[0], up.paths[0])
				}
				if up.sessions[0] != headerSession {
					t.Fatalf("upstream session = %q, want %q", up.sessions[0], headerSession)
				}
				if up.auths[0] != "Bearer "+tc.apiKey {
					t.Fatalf("upstream auth = %q", up.auths[0])
				}

				// 2. The client response carries the opaque reasoning item, readable
				// summaries, text, and the restored function call.
				output := opencodeHistoryOutput(t, stream, out)
				var outputItems []map[string]json.RawMessage
				if err := json.Unmarshal(output, &outputItems); err != nil {
					t.Fatal(err)
				}
				if len(outputItems) != 3 {
					t.Fatalf("client output items = %d, want reasoning, message, function_call: %s", len(outputItems), output)
				}
				assertHistoryReasoningItem(t, "output[0]", outputItems[0])
				if outputItems[1]["type"] == nil || !bytes.Contains(outputItems[1]["content"], []byte("resp-ok")) {
					t.Fatalf("client text lost: %s", outputItems[1])
				}
				assertRawString(t, "output[2].type", outputItems[2]["type"], "function_call")
				assertRawString(t, "output[2].call_id", outputItems[2]["call_id"], "call_resp")
				assertRawString(t, "output[2].name", outputItems[2]["name"], "Read")
				if stream {
					dones := opencodeHistoryItemDoneItems(t, out)
					var doneReasoning map[string]json.RawMessage
					for _, item := range dones {
						if string(item["type"]) == `"reasoning"` {
							doneReasoning = item
						}
					}
					if doneReasoning == nil {
						t.Fatalf("stream emitted no reasoning output_item.done: %s", out)
					}
					assertHistoryReasoningItem(t, "item.done[reasoning]", doneReasoning)
				}

				// 3. Replay the returned opaque state to the same provider/model and
				// assert the continuity bytes are forwarded.
				replay := opencodeHistoryReplayBody(t, stream, output)
				resp2, out2 := postWithHeaders(t, clientBase+"/v1/responses", token, replay, nil)
				if resp2.StatusCode != http.StatusOK {
					t.Fatalf("replay status=%d body=%s", resp2.StatusCode, out2)
				}
				raw2, body2 := up.last()
				if !bytes.Contains(raw2, []byte(`"encrypted_content":"cipher-resp"`)) || !bytes.Contains(raw2, []byte(`"id":"rs_resp"`)) {
					t.Fatalf("replay did not forward continuity state: %s", raw2)
				}
				if bytes.Contains(raw2, []byte("cipher-hist")) {
					t.Fatalf("replay retained the previous turn's ciphertext: %s", raw2)
				}
				if !bytes.Contains(raw2, []byte("resp-part-one")) || !bytes.Contains(raw2, []byte("resp-part-three")) {
					t.Fatalf("replay lost summary boundaries: %s", raw2)
				}
				if body2["prompt_cache_key"] != "replay-key" {
					t.Fatalf("replay key = %#v", body2["prompt_cache_key"])
				}
				if stream2, _ := body2["stream"].(bool); stream2 != wantStream {
					t.Fatalf("replay upstream stream = %#v, want %v", body2["stream"], wantStream)
				}

				// 4. A plain request through the same Proxy must inherit nothing.
				plain := fmt.Sprintf(`{"model":"default","stream":%t,"input":"plain-hi"}`, stream)
				resp3, out3 := postWithHeaders(t, clientBase+"/v1/responses", token, plain, nil)
				if resp3.StatusCode != http.StatusOK {
					t.Fatalf("plain status=%d body=%s", resp3.StatusCode, out3)
				}
				raw3, body3 := up.last()
				for _, leaked := range []string{"cipher-hist", "cipher-resp", "rs_hist", "rs_resp", "caller-cache-key", "custom.include", "assistant-answer", "final-user", "resp-ok"} {
					if bytes.Contains(raw3, []byte(leaked)) {
						t.Fatalf("plain request inherited %q: %s", leaked, raw3)
					}
				}
				for _, key := range []string{"store", "include", "prompt_cache_key"} {
					if _, ok := body3[key]; ok {
						t.Fatalf("plain request inherited option %q: %s", key, raw3)
					}
				}
				var plainItems []map[string]json.RawMessage
				var plainFields map[string]json.RawMessage
				if err := json.Unmarshal(raw3, &plainFields); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(plainFields["input"], &plainItems); err != nil {
					t.Fatal(err)
				}
				if len(plainItems) != 1 || !bytes.Contains(plainItems[0]["content"], []byte("plain-hi")) {
					t.Fatalf("plain input not isolated: %s", plainFields["input"])
				}
				if up.count() != 3 {
					t.Fatalf("upstream hits = %d, want 3", up.count())
				}
			})
		}
	}
}
