package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
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
	chat := backendCodec(domain.ProtocolOpencode, "big-pickle")
	if chat.id != "opencode-chat" {
		t.Fatalf("big-pickle codec = %q", chat.id)
	}
	if chat.upstreamPath != opencode.ChatPath {
		t.Fatalf("chat path = %q", chat.upstreamPath)
	}
	resp := backendCodec(domain.ProtocolOpencode, "muse-spark-1.2-contributor-free")
	if resp.id != "opencode-responses" {
		t.Fatalf("muse-spark codec = %q", resp.id)
	}
	if resp.upstreamPath != opencode.ResponsesPath {
		t.Fatalf("responses path = %q", resp.upstreamPath)
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
	if !strings.HasPrefix(trace.OpencodeSessionID, "ses_") {
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
	// deepseek dialect writes enable_thinking + thinking.type=enabled on chat.
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Fatalf("deepseek thinking = %+v, want type enabled", th)
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
	req, _ := http.NewRequestWithContext(WithTraceInfo(context.Background(), trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	// A forwarded client UA that is not opencode must be replaced.
	req.Header.Set("User-Agent", "some-agent/1.0")
	applyUpstreamHeaders(req, opencodeTestProvider(), http.Header{}, req.Context(), nil)
	if got := req.Header.Get("User-Agent"); got != "opencode" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("x-opencode-session"); got != "ses_abc" {
		t.Fatalf("x-opencode-session = %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "desktop" {
		t.Fatalf("x-opencode-client = %q", got)
	}
	if got := req.Header.Get("x-opencode-project"); got != "global" {
		t.Fatalf("x-opencode-project = %q", got)
	}
	if got := req.Header.Get("x-opencode-request"); !strings.HasPrefix(got, "msg_") {
		t.Fatalf("x-opencode-request = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer public" {
		t.Fatalf("Authorization = %q", got)
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
	respBody := []byte(`{"model":"muse-spark-1.2","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	chatTrace := &TraceInfo{}
	respTrace := &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), chatTrace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, chatBody); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), respTrace), backendCodec(domain.ProtocolOpencode, "muse-spark-1.2"), p, respBody); err != nil {
		t.Fatal(err)
	}
	if chatTrace.OpencodeSessionID == "" || chatTrace.OpencodeSessionID != respTrace.OpencodeSessionID {
		t.Fatalf("chat/responses first-turn sessions differ: %q vs %q", chatTrace.OpencodeSessionID, respTrace.OpencodeSessionID)
	}

	h := http.Header{}
	h.Set("x-opencode-session", "ses_shared")
	ctx := withOpencodeRequest(context.Background(), px.opencodeNonce, h, nil)
	chatTrace = &TraceInfo{}
	respTrace = &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, chatTrace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, chatBody); err != nil {
		t.Fatal(err)
	}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, respTrace), backendCodec(domain.ProtocolOpencode, "muse-spark-1.2"), p, respBody); err != nil {
		t.Fatal(err)
	}
	if chatTrace.OpencodeSessionID != "ses_shared" || respTrace.OpencodeSessionID != "ses_shared" {
		t.Fatalf("explicit session not shared: %q vs %q", chatTrace.OpencodeSessionID, respTrace.OpencodeSessionID)
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
	applyUpstreamHeaders(req1, p, nil, req1.Context(), nil)
	req2, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req2, p, nil, req2.Context(), nil)
	if req1.Header.Get("x-opencode-session") == "" || req1.Header.Get("x-opencode-session") != req2.Header.Get("x-opencode-session") {
		t.Fatalf("session not stable across sends: %q vs %q", req1.Header.Get("x-opencode-session"), req2.Header.Get("x-opencode-session"))
	}
	if req1.Header.Get("x-opencode-request") == "" || req1.Header.Get("x-opencode-request") == req2.Header.Get("x-opencode-request") {
		t.Fatalf("request id not fresh: %q vs %q", req1.Header.Get("x-opencode-request"), req2.Header.Get("x-opencode-request"))
	}

	h := http.Header{}
	h.Set("x-opencode-request", "msg_client")
	ctx = withOpencodeRequest(context.Background(), px.opencodeNonce, h, body)
	trace = &TraceInfo{}
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, body); err != nil {
		t.Fatal(err)
	}
	req3, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req3, p, nil, req3.Context(), nil)
	if req3.Header.Get("x-opencode-request") != "msg_client" {
		t.Fatalf("client request id lost: %q", req3.Header.Get("x-opencode-request"))
	}
}

func TestApplyOpencodeHeadersFromCapturedIdentity(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "opencode/0.16.7")
	h.Set("x-opencode-client", "my-cli")
	h.Set("x-opencode-project", "proj-a")
	h.Set("x-opencode-request", "msg_client")
	h.Set("x-opencode-session", "ses_client")
	ctx := withOpencodeRequest(context.Background(), "nonce", h, nil)
	trace := &TraceInfo{}
	px := New(nil, nil)
	p := opencodeTestProvider()
	if _, err := px.prepareUpstreamRequest(WithTraceInfo(ctx, trace), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(WithTraceInfo(ctx, trace), http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	applyUpstreamHeaders(req, p, nil, req.Context(), nil)
	if got := req.Header.Get("User-Agent"); got != "opencode/0.16.7" {
		t.Fatalf("UA = %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "my-cli" {
		t.Fatalf("client = %q", got)
	}
	if got := req.Header.Get("x-opencode-project"); got != "proj-a" {
		t.Fatalf("project = %q", got)
	}
	if got := req.Header.Get("x-opencode-request"); got != "msg_client" {
		t.Fatalf("request = %q", got)
	}
	if got := req.Header.Get("x-opencode-session"); got != "ses_client" {
		t.Fatalf("session = %q", got)
	}
}

func TestApplyOpencodeHeadersIgnoresInvalidPassthroughIdentity(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", nil)
	clientHeaders := http.Header{}
	clientHeaders.Set("User-Agent", "curl/8.0")
	clientHeaders.Set("x-opencode-session", "ses_ok\r\nX-Injected: 1")
	clientHeaders.Set("x-opencode-client", strings.Repeat("c", 300))
	trace := &TraceInfo{OpencodeSessionID: "ses_fallback"}
	applyUpstreamHeaders(req, opencodeTestProvider(), clientHeaders, WithTraceInfo(context.Background(), trace), nil)
	if got := req.Header.Get("User-Agent"); got != "opencode" {
		t.Fatalf("UA = %q", got)
	}
	if got := req.Header.Get("x-opencode-session"); got != "ses_fallback" {
		t.Fatalf("session = %q", got)
	}
	if got := req.Header.Get("x-opencode-client"); got != "desktop" {
		t.Fatalf("client = %q", got)
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
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
	if cap.session != "client-req" {
		t.Fatalf("generic request id not used as session: %q", cap.session)
	}
	if cap.clientReq != "" {
		t.Fatalf("x-client-request-id leaked: %q", cap.clientReq)
	}
	if cap.cookie != "" || cap.custom != "" {
		t.Fatalf("unrelated headers leaked cookie=%q custom=%q", cap.cookie, cap.custom)
	}
	if cap.ua != "opencode" {
		t.Fatalf("non-opencode UA = %q", cap.ua)
	}
	if cap.client != "desktop" || cap.project != "global" {
		t.Fatalf("defaults missing client=%q project=%q", cap.client, cap.project)
	}
	if !strings.HasPrefix(cap.request, "msg_") {
		t.Fatalf("request id = %q", cap.request)
	}
}

func TestOpencodeTranslatedExplicitIdentityUnaryAndStream(t *testing.T) {
	var last http.Header
	base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		last = r.Header.Clone()
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, openaiSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	})

	send := func(stream bool) {
		body := `{"model":"default","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", "opencode/0.16.7")
		req.Header.Set("x-opencode-client", "my-cli")
		req.Header.Set("x-opencode-project", "proj-a")
		req.Header.Set("x-opencode-request", "msg_client")
		req.Header.Set("x-opencode-session", "ses_client")
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
		if last.Get("x-opencode-session") != "ses_client" {
			t.Fatalf("stream=%v session = %q", stream, last.Get("x-opencode-session"))
		}
		if last.Get("x-opencode-client") != "my-cli" || last.Get("x-opencode-project") != "proj-a" || last.Get("x-opencode-request") != "msg_client" {
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
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
	if session != "cache-from-body" {
		t.Fatalf("body session = %q", session)
	}
}

func TestOpencodeTranslatedZenFirstTurnDistinctAcrossProxies(t *testing.T) {
	sessions := make([]string, 2)
	for i := range sessions {
		var session string
		base, token, _ := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
			session = r.Header.Get("x-opencode-session")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, openaiUpstreamBody)
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
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
	if session == "" || !strings.HasPrefix(session, "ses_") || strings.Contains(session, "\n") {
		t.Fatalf("fallback session = %q", session)
	}
}

func TestOpencodeTranslatedInvalidIdentityFallsBack(t *testing.T) {
	var session, ua, client string
	_, token, mux := setupOpencodeTranslated(t, func(w http.ResponseWriter, r *http.Request) {
		session = r.Header.Get("x-opencode-session")
		ua = r.Header.Get("User-Agent")
		client = r.Header.Get("x-opencode-client")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
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
	if session == "" || !strings.HasPrefix(session, "ses_") || strings.Contains(session, "\n") || strings.Contains(session, "\r") {
		t.Fatalf("fallback session = %q", session)
	}
	if ua != "opencode" {
		t.Fatalf("UA = %q", ua)
	}
	if client != "desktop" {
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
	if !m3.Reasoning || m3.Format != thinking.FormatMiniMax || !m3.CanDisable {
		t.Fatalf("m3 caps = %+v", m3)
	}
	m27 := thinkingCapsFor(t, "minimax-m2.7")
	if !m27.Reasoning || m27.Format != thinking.FormatMiniMax || m27.CanDisable {
		t.Fatalf("m2.7 caps = %+v", m27)
	}
	m25 := thinkingCapsFor(t, "minimax-m2.5")
	if !m25.Reasoning || m25.Format != thinking.FormatMiniMax || m25.CanDisable {
		t.Fatalf("m2.5 caps = %+v", m25)
	}
	mimo := thinkingCapsFor(t, "mimo-v2.5-pro")
	if mimo.Reasoning || mimo.Format != thinking.FormatNone || mimo.MaxOutput != 131072 {
		t.Fatalf("mimo caps = %+v", mimo)
	}
	glm := thinkingCapsFor(t, "glm-4.7")
	if glm.Format != thinking.FormatZAI {
		t.Fatalf("glm format = %v", glm.Format)
	}
}

func prepareOpencodeTranslated(t *testing.T, ingress codec, ingressBody []byte, upstreamModel string) map[string]any {
	t.Helper()
	p := opencodeTestProvider()
	req, err := ingress.decodeRequest(ingressBody)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(ingressBody); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, upstreamModel)
	backend := backendCodec(p.Protocol, upstreamModel)
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

func TestOpencodeTranslatedMiniMaxAdaptive(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	got := prepareOpencodeTranslated(t, openaiCodec, body, "minimax-m3")
	th, _ := got["thinking"].(map[string]any)
	if th == nil || th["type"] != "adaptive" {
		t.Fatalf("minimax thinking = %+v, want adaptive", th)
	}
	if _, ok := got["enable_thinking"]; ok {
		t.Fatalf("enable_thinking leaked: %v", got)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort leaked: %v", got)
	}
	if th["type"] == "enabled" {
		t.Fatalf("minimax must not emit enabled: %v", got)
	}
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
