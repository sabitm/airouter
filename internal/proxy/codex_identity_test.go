package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

const codexIdentitySSE = `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"up","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","model":"up","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}

`

type codexUpstreamCapture struct {
	mu       sync.Mutex
	keys     []string
	sessions []string
}

func (c *codexUpstreamCapture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m struct {
		Key string `json:"prompt_cache_key"`
	}
	_ = json.Unmarshal(body, &m)
	c.mu.Lock()
	c.keys = append(c.keys, m.Key)
	c.sessions = append(c.sessions, r.Header.Get("session_id"))
	c.mu.Unlock()
}

func (c *codexUpstreamCapture) snapshot() (keys, sessions []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.keys...), append([]string(nil), c.sessions...)
}

func writeCodexSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, codexIdentitySSE)
}

func postCodexChat(t *testing.T, url, token, session, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if session != "" {
		req.Header.Set("session_id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(out)
}

func assertCodexKeyPair(t *testing.T, key, session string) {
	t.Helper()
	if key == "" || len(key) != 64 {
		t.Fatalf("prompt_cache_key = %q", key)
	}
	if session != key {
		t.Fatalf("session_id = %q, prompt_cache_key = %q", session, key)
	}
}

func TestCaptureCodexIdentityPrecedence(t *testing.T) {
	bodyAll := []byte(`{"prompt_cache_key":"from-cache","session_id":"from-body-session","conversation_id":"from-conv"}`)
	h := http.Header{}
	h.Set("session_id", "from-session-header")
	h.Set("x-session-affinity", "from-affinity")
	h.Set("x-session-id", "from-x-session")
	h.Set("session-id", "from-session-id")
	h.Set("x-client-request-id", "from-client-req")
	if got := captureCodexIdentity(h, bodyAll); got != "from-cache" {
		t.Fatalf("prompt_cache_key should win: %q", got)
	}

	bodyNoCache := []byte(`{"session_id":"from-body-session","conversation_id":"from-conv"}`)
	if got := captureCodexIdentity(h, bodyNoCache); got != "from-session-header" {
		t.Fatalf("session_id header should win: %q", got)
	}

	h.Del("session_id")
	if got := captureCodexIdentity(h, bodyNoCache); got != "from-affinity" {
		t.Fatalf("x-session-affinity should win: %q", got)
	}
	h.Del("x-session-affinity")
	if got := captureCodexIdentity(h, bodyNoCache); got != "from-x-session" {
		t.Fatalf("x-session-id should win: %q", got)
	}
	h.Del("x-session-id")
	if got := captureCodexIdentity(h, bodyNoCache); got != "from-session-id" {
		t.Fatalf("session-id should win: %q", got)
	}
	h.Del("session-id")
	if got := captureCodexIdentity(h, bodyNoCache); got != "from-body-session" {
		t.Fatalf("body session_id should win: %q", got)
	}
	if got := captureCodexIdentity(h, []byte(`{"conversation_id":"from-conv"}`)); got != "from-conv" {
		t.Fatalf("conversation_id should win: %q", got)
	}
	if got := captureCodexIdentity(h, nil); got != "from-client-req" {
		t.Fatalf("x-client-request-id last: %q", got)
	}
}

func TestCaptureCodexIdentityRejectsInvalid(t *testing.T) {
	h := http.Header{}
	h.Set("session_id", "ok\nX-Injected: 1")
	if got := captureCodexIdentity(h, []byte(`{"prompt_cache_key":"bad\nvalue"}`)); got != "" {
		t.Fatalf("control characters accepted: %q", got)
	}
	h = http.Header{}
	h.Add("session_id", "first")
	h.Add("session_id", "second")
	if got := captureCodexIdentity(h, nil); got != "" {
		t.Fatalf("duplicate header accepted: %q", got)
	}
	if got := captureCodexIdentity(http.Header{}, []byte(`{"prompt_cache_key":123}`)); got != "" {
		t.Fatalf("non-string body field accepted: %q", got)
	}
	if got := captureCodexIdentity(http.Header{}, []byte(`{"prompt_cache_key":"`+strings.Repeat("a", 257)+`"}`)); got != "" {
		t.Fatalf("oversized accepted")
	}
	h = http.Header{}
	h.Set("session_id", "  trimmed-ok  ")
	if got := captureCodexIdentity(h, nil); got != "trimmed-ok" {
		t.Fatalf("trim failed: %q", got)
	}
}

func TestCaptureCodexIdentitySkipsInvalidThenValid(t *testing.T) {
	h := http.Header{}
	h.Set("session_id", "from-session-header")
	body := []byte(`{"prompt_cache_key":"bad\nvalue","session_id":"from-body-session"}`)
	if got := captureCodexIdentity(h, body); got != "from-session-header" {
		t.Fatalf("valid lower-priority header skipped: %q", got)
	}
}

func TestCaptureCodexIdentitySkipsInvalidBodyThenValidBody(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"bad\nvalue","session_id":"from-body-session"}`)
	if got := captureCodexIdentity(http.Header{}, body); got != "from-body-session" {
		t.Fatalf("valid body session_id skipped: %q", got)
	}
	body = []byte(`{"prompt_cache_key":123,"conversation_id":"from-conv"}`)
	if got := captureCodexIdentity(http.Header{}, body); got != "from-conv" {
		t.Fatalf("valid body conversation_id skipped: %q", got)
	}
}

func TestCaptureCodexIdentityIgnoresLargeUnrelatedField(t *testing.T) {
	pad := strings.Repeat("x", 32<<10)
	body := []byte(`{"messages":[{"role":"user","content":"` + pad + `"}],"session_id":"from-body-session"}`)
	if got := captureCodexIdentity(http.Header{}, body); got != "from-body-session" {
		t.Fatalf("identity lost beside large messages: %q", got)
	}
	body = []byte(`{"input":"` + pad + `","conversation_id":"from-conv"}`)
	if got := captureCodexIdentity(http.Header{}, body); got != "from-conv" {
		t.Fatalf("identity lost beside large input: %q", got)
	}
}

func TestCodexCacheKeyStableAndScoped(t *testing.T) {
	p := &domain.Provider{ID: 7, BaseURL: "https://chatgpt.com/backend-api", Protocol: domain.ProtocolOpenAICodex}
	h := http.Header{}
	h.Set("session_id", "conv-1")
	ctx1 := withCodexRequest(context.Background(), "hash-a", h, nil)
	ctx2 := withCodexRequest(context.Background(), "hash-a", h, nil)
	k1 := resolveCodexCacheKey(ctx1, p)
	k2 := resolveCodexCacheKey(ctx2, p)
	if k1 != k2 || len(k1) != 64 {
		t.Fatalf("same identity+provider not stable: %q %q", k1, k2)
	}
	h2 := http.Header{}
	h2.Set("session_id", "conv-2")
	ctxOther := withCodexRequest(context.Background(), "hash-a", h2, nil)
	if resolveCodexCacheKey(ctxOther, p) == k1 {
		t.Fatal("different identities collided")
	}
	ctxScope := withCodexRequest(context.Background(), "hash-b", h, nil)
	if resolveCodexCacheKey(ctxScope, p) == k1 {
		t.Fatal("different access-key scopes collided")
	}
	if resolveCodexCacheKey(ctx1, p) != k1 {
		t.Fatal("same request/provider re-prepare not deterministic")
	}
	p2 := &domain.Provider{ID: 8, BaseURL: p.BaseURL, Protocol: domain.ProtocolOpenAICodex}
	if resolveCodexCacheKey(ctx1, p2) == k1 {
		t.Fatal("different provider ids collided")
	}
	pAcct := &domain.Provider{
		ID: 7, BaseURL: p.BaseURL, Protocol: domain.ProtocolOpenAICodex,
		OAuthCreds: &domain.OAuthCreds{AccountID: "acct-2"},
	}
	if resolveCodexCacheKey(ctx1, pAcct) == k1 {
		t.Fatal("different account ids collided")
	}
}

func TestCodexConversationFallbackStable(t *testing.T) {
	p := &domain.Provider{ID: 3, BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAICodex}
	first := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	later := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`)
	other := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"other"}]}`)
	ctxA := withCodexRequest(context.Background(), "tenant-hash", nil, first)
	ctxB := withCodexRequest(context.Background(), "tenant-hash", nil, later)
	ctxC := withCodexRequest(context.Background(), "tenant-hash", nil, other)
	a := resolveCodexCacheKey(ctxA, p)
	b := resolveCodexCacheKey(ctxB, p)
	c := resolveCodexCacheKey(ctxC, p)
	if a != b || len(a) != 64 {
		t.Fatalf("same conversation prefix not stable: %q %q", a, b)
	}
	if a == c {
		t.Fatal("different conversation prefixes collided")
	}
	ctxTenant := withCodexRequest(context.Background(), "other-hash", nil, first)
	if resolveCodexCacheKey(ctxTenant, p) == a {
		t.Fatal("conversation fallback ignored tenant scope")
	}
}

func TestCodexConversationFallbackCanonicalJSON(t *testing.T) {
	p := &domain.Provider{ID: 3, BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAICodex}
	a := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	b := []byte(`{ "messages" : [ { "content" : "hi", "role" : "user" } ] }`)
	ctxA := withCodexRequest(context.Background(), "tenant-hash", nil, a)
	ctxB := withCodexRequest(context.Background(), "tenant-hash", nil, b)
	if resolveCodexCacheKey(ctxA, p) != resolveCodexCacheKey(ctxB, p) {
		t.Fatal("canonical JSON fallback differed")
	}
}

func TestCodexConversationFallbackResponsesInput(t *testing.T) {
	p := &domain.Provider{ID: 3, BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAICodex}
	first := []byte(`{"instructions":"sys","input":[{"role":"user","content":"hi"}]}`)
	later := []byte(`{"instructions":"sys","input":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`)
	ctxA := withCodexRequest(context.Background(), "tenant-hash", nil, first)
	ctxB := withCodexRequest(context.Background(), "tenant-hash", nil, later)
	a := resolveCodexCacheKey(ctxA, p)
	b := resolveCodexCacheKey(ctxB, p)
	if a != b || len(a) != 64 {
		t.Fatalf("responses input prefix not stable: %q %q", a, b)
	}
	str := []byte(`{"input":"hello there"}`)
	ctxS1 := withCodexRequest(context.Background(), "tenant-hash", nil, str)
	ctxS2 := withCodexRequest(context.Background(), "tenant-hash", nil, []byte(`{"model":"x","input":"hello there"}`))
	if resolveCodexCacheKey(ctxS1, p) != resolveCodexCacheKey(ctxS2, p) {
		t.Fatal("responses string input fallback differed")
	}
}

func TestCodexNoAnchorRandomFallback(t *testing.T) {
	p := &domain.Provider{ID: 3, BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAICodex}
	for _, tenant := range []string{"tenant-hash", openTenantScope} {
		ctxA := withCodexRequest(context.Background(), tenant, nil, nil)
		ctxB := withCodexRequest(context.Background(), tenant, nil, []byte(`{"messages":[]}`))
		a1 := resolveCodexCacheKey(ctxA, p)
		a2 := resolveCodexCacheKey(ctxA, p)
		b := resolveCodexCacheKey(ctxB, p)
		if a1 != a2 || len(a1) != 64 {
			t.Fatalf("random fallback not stable within request (%s): %q %q", tenant, a1, a2)
		}
		if a1 == b {
			t.Fatalf("unidentified requests shared a key (%s)", tenant)
		}
	}
}

func TestCodexConversationFallbackBoundedScan(t *testing.T) {
	p := &domain.Provider{ID: 3, BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAICodex}
	pad := strings.Repeat("a", 2*codexMaxAnchorScanBytes)

	largeUser := []byte(`{"messages":[{"role":"user","content":"` + pad + `"}]}`)
	if conversationAnchor(largeUser) != "" {
		t.Fatal("oversized first user item produced an anchor")
	}
	ctxA := withCodexRequest(context.Background(), "tenant-hash", nil, largeUser)
	ctxB := withCodexRequest(context.Background(), "tenant-hash", nil, largeUser)
	if resolveCodexCacheKey(ctxA, p) == resolveCodexCacheKey(ctxB, p) {
		t.Fatal("oversized first user item shared a key")
	}

	padded := []byte(`{"pad":"` + pad + `","messages":[{"role":"user","content":"hi"}]}`)
	if conversationAnchor(padded) != "" {
		t.Fatal("request with large data before messages produced an anchor")
	}
	ctxC := withCodexRequest(context.Background(), "tenant-hash", nil, padded)
	ctxD := withCodexRequest(context.Background(), "tenant-hash", nil, padded)
	if resolveCodexCacheKey(ctxC, p) == resolveCodexCacheKey(ctxD, p) {
		t.Fatal("padded requests shared a key")
	}
}

func TestCodexPrepareBodyMatchesSessionHeader(t *testing.T) {
	p := &domain.Provider{ID: 1, BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAICodex}
	h := http.Header{}
	h.Set("session_id", "client-sess")
	trace := &TraceInfo{}
	ctx := WithTraceInfo(withCodexRequest(context.Background(), "tenant-hash", h, nil), trace)
	body, err := prepareUpstreamRequest(ctx, codexCodec, p, []byte(`{"model":"gpt-5.3-codex","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	key, _ := got["prompt_cache_key"].(string)
	if key == "" || key != trace.CodexSessionID || len(key) != 64 {
		t.Fatalf("prompt_cache_key=%q trace=%q", key, trace.CodexSessionID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyCodexHeaders(req, p, ctx)
	if req.Header.Get("session_id") != key {
		t.Fatalf("session_id=%q, want %q", req.Header.Get("session_id"), key)
	}
}

func TestCodexZeroIDProviderUsesBaseURL(t *testing.T) {
	p := &domain.Provider{BaseURL: "https://chatgpt.com/backend-api/", Protocol: domain.ProtocolOpenAICodex}
	h := http.Header{}
	h.Set("session_id", "s")
	ctx := withCodexRequest(context.Background(), "t", h, nil)
	k1 := resolveCodexCacheKey(ctx, p)
	p2 := &domain.Provider{BaseURL: "https://chatgpt.com/backend-api", Protocol: domain.ProtocolOpenAICodex}
	k2 := resolveCodexCacheKey(ctx, p2)
	if k1 != k2 {
		t.Fatalf("base URL normalization differed: %q %q", k1, k2)
	}
}

func TestCodexOAuthRetryReusesPreparedKey(t *testing.T) {
	var cap codexUpstreamCapture
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		cap.record(r)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		writeCodexSSE(w)
	}))
	t.Cleanup(up.Close)
	te := newTokenEndpoint(t, "tok-good")

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "codex", BaseURL: up.URL, Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, AccessToken: "tok-revoked", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			TokenURL:  te.server.URL, ClientID: "cid", PKCE: true,
			AccountID: "acct-1",
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	keys, sessions := cap.snapshot()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("prepared keys not reused: %v", keys)
	}
	if len(sessions) != 2 || sessions[0] != keys[0] || sessions[1] != keys[1] {
		t.Fatalf("session_id mismatch keys=%v sessions=%v", keys, sessions)
	}
	if te.hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1", te.hits.Load())
	}
}

func TestCodexHTTPIdentityAndTenantIsolation(t *testing.T) {
	var cap codexUpstreamCapture
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeCodexSSE(w)
	}))
	t.Cleanup(up.Close)

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "codex", BaseURL: up.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	keyA, err := st.NewAccessKey(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := st.NewAccessKey(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body := `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, out := postCodexChat(t, ts.URL+"/v1/chat/completions", keyA.Token, "conv-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	resp, out = postCodexChat(t, ts.URL+"/v1/chat/completions", keyA.Token, "conv-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	resp, out = postCodexChat(t, ts.URL+"/v1/chat/completions", keyA.Token, "conv-2", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	resp, out = postCodexChat(t, ts.URL+"/v1/chat/completions", keyB.Token, "conv-1", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}

	keys, sessions := cap.snapshot()
	if len(keys) != 4 || len(sessions) != 4 {
		t.Fatalf("captures keys=%v sessions=%v", keys, sessions)
	}
	for i := range keys {
		assertCodexKeyPair(t, keys[i], sessions[i])
	}
	if keys[0] != keys[1] {
		t.Fatalf("same client identity not reused: %q %q", keys[0], keys[1])
	}
	if keys[0] == keys[2] {
		t.Fatal("different client identities collided")
	}
	if keys[0] == keys[3] {
		t.Fatal("tenants sharing client identity collided")
	}
}

func TestCodexHTTPFailoverProviderScopedKeys(t *testing.T) {
	var cap1, cap2 codexUpstreamCapture
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap1.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream is down"}}`)
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap2.record(r)
		writeCodexSSE(w)
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "codex-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	p2 := &domain.Provider{Name: "codex-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
	}}); err != nil {
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

	body := `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, out := postCodexChat(t, ts.URL+"/v1/chat/completions", key.Token, "conv-shared", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}

	k1, s1 := cap1.snapshot()
	k2, s2 := cap2.snapshot()
	if len(k1) != 1 || len(k2) != 1 {
		t.Fatalf("provider hits keys1=%v keys2=%v", k1, k2)
	}
	assertCodexKeyPair(t, k1[0], s1[0])
	assertCodexKeyPair(t, k2[0], s2[0])
	if k1[0] == k2[0] {
		t.Fatal("provider-scoped keys collided")
	}
}

func TestCodexPrepareWithoutCaptureStillNonempty(t *testing.T) {
	out, err := prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), codexCodec, &domain.Provider{}, []byte(`{"model":"gpt-5.3-codex","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	key, _ := got["prompt_cache_key"].(string)
	if len(key) != 64 {
		t.Fatalf("uncaptured prepare key = %q", key)
	}
}
