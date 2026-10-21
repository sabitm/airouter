package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
)

// oauthUpstream is a mock OpenAI provider that accepts only a specific bearer
// token, returning 401 otherwise. The accepted token can be changed to simulate
// a rotation a refresh would pick up.
type oauthUpstream struct {
	server   *httptest.Server
	accept   atomic.Value // string: the currently valid access token
	seenAuth atomic.Value // string: Authorization header of the last accepted call
	calls    atomic.Int64
}

func newOAuthUpstream(t *testing.T, acceptToken string) *oauthUpstream {
	t.Helper()
	u := &oauthUpstream{}
	u.accept.Store(acceptToken)
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		want := "Bearer " + u.accept.Load().(string)
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		u.seenAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	}))
	t.Cleanup(u.server.Close)
	return u
}

// tokenEndpoint serves the OAuth refresh grant, issuing the configured new
// access token and counting hits.
type tokenEndpoint struct {
	server   *httptest.Server
	newToken string
	hits     atomic.Int64
}

func newTokenEndpoint(t *testing.T, newToken string) *tokenEndpoint {
	t.Helper()
	te := &tokenEndpoint{newToken: newToken}
	te.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		te.hits.Add(1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"access_token":  te.newToken,
			"refresh_token": "rt-next",
			"expires_in":    3600,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(te.server.Close)
	return te
}

// setupOAuthCombo wires a store + proxy with one oauth provider (openai backend)
// whose creds point at the given token endpoint. expiresAt sets the stored
// access token's expiry (use a past value to force a proactive refresh, a future
// value to exercise the reactive path).
func setupOAuthCombo(t *testing.T, up *oauthUpstream, te *tokenEndpoint, accessToken string, expiresAt int64) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	prov := &domain.Provider{
		Name: "grok", BaseURL: up.server.URL, Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, Preset: "xai",
			AccessToken: accessToken, RefreshToken: "rt-old", ExpiresAt: expiresAt,
			TokenURL: te.server.URL, ClientID: "cid", PKCE: true,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "grok-4", Enabled: true},
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
	return ts.URL, key.Token
}

const chatBody = `{"model":"default","messages":[{"role":"user","content":"hi"}]}`

// TestOAuthProactiveRefresh: a near-expiry token is refreshed before the
// upstream call, which then succeeds with the new bearer.
func TestOAuthProactiveRefresh(t *testing.T) {
	up := newOAuthUpstream(t, "tok-new") // upstream accepts only the refreshed token
	te := newTokenEndpoint(t, "tok-new")
	base, token := setupOAuthCombo(t, up, te, "tok-stale", time.Now().Add(1*time.Minute).Unix())

	resp, body := post(t, base+"/v1/chat/completions", token, chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if te.hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (proactive)", te.hits.Load())
	}
	if got := up.seenAuth.Load(); got != "Bearer tok-new" {
		t.Errorf("upstream saw auth = %v, want Bearer tok-new", got)
	}
}

// TestOAuthReactiveRefresh: a token that looks valid (future expiry) but is
// rejected upstream (401) triggers a forced refresh and one retry.
func TestOAuthReactiveRefresh(t *testing.T) {
	up := newOAuthUpstream(t, "tok-good")
	te := newTokenEndpoint(t, "tok-good")
	// Stored token is "tok-revoked" with a far-future expiry, so no proactive
	// refresh; the upstream 401s, forcing the reactive path.
	base, token := setupOAuthCombo(t, up, te, "tok-revoked", time.Now().Add(1*time.Hour).Unix())

	resp, body := post(t, base+"/v1/chat/completions", token, chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if te.hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (reactive)", te.hits.Load())
	}
	if got := up.seenAuth.Load(); got != "Bearer tok-good" {
		t.Errorf("upstream saw auth = %v, want Bearer tok-good", got)
	}
	// Upstream is hit twice: the rejected attempt, then the retry.
	if up.calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2 (reject + retry)", up.calls.Load())
	}
}

// TestOAuthNoRefreshWhenValid: a valid, non-expiring token is used directly with
// no token-endpoint call.
func TestOAuthNoRefreshWhenValid(t *testing.T) {
	up := newOAuthUpstream(t, "tok-valid")
	te := newTokenEndpoint(t, "tok-should-not-issue")
	base, token := setupOAuthCombo(t, up, te, "tok-valid", time.Now().Add(1*time.Hour).Unix())

	resp, body := post(t, base+"/v1/chat/completions", token, chatBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if te.hits.Load() != 0 {
		t.Errorf("token endpoint hits = %d, want 0", te.hits.Load())
	}
	if got := up.seenAuth.Load(); got != "Bearer tok-valid" {
		t.Errorf("upstream saw auth = %v, want Bearer tok-valid", got)
	}
}

// TestOAuthRefreshPersisted: after a proactive refresh, the rotated token is
// written back so a second request needs no refresh.
func TestOAuthRefreshPersisted(t *testing.T) {
	up := newOAuthUpstream(t, "tok-new")
	te := newTokenEndpoint(t, "tok-new")
	base, token := setupOAuthCombo(t, up, te, "tok-stale", time.Now().Add(1*time.Minute).Unix())

	for i := 0; i < 2; i++ {
		resp, body := post(t, base+"/v1/chat/completions", token, chatBody)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, resp.StatusCode, body)
		}
	}
	// The first request refreshed and persisted tok-new (expiry 1h out); the
	// second must reuse it without a second token-endpoint hit.
	if te.hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1 across two requests", te.hits.Load())
	}
}

// TestOAuthStreamingReactiveRefresh exercises the streaming forward path's
// reactive refresh: the first stream attempt 401s, the token is refreshed, and
// the retried stream succeeds.
func TestOAuthStreamingReactiveRefresh(t *testing.T) {
	// A streaming-aware oauth upstream: 401 on the bad token, SSE on the good one.
	var accept atomic.Value
	accept.Store("tok-good")
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer "+accept.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: "+openaiStreamChunk+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	te := newTokenEndpoint(t, "tok-good")

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "grok", BaseURL: srv.URL, Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, AccessToken: "tok-revoked", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			TokenURL:  te.server.URL, ClientID: "cid", PKCE: true,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "grok-4", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, _ := st.NewAccessKey(ctx, "test")
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := post(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if te.hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1", te.hits.Load())
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("stream body missing content: %s", body)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream stream calls = %d, want 2 (reject + retry)", calls.Load())
	}
}

const openaiStreamChunk = `{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`

func TestGrokDefaultTransportUsesChatCompletions(t *testing.T) {
	preset, ok := oauth.PresetByName("xai")
	if !ok {
		t.Fatal("missing xai preset")
	}
	if preset.Protocol != domain.ProtocolOpenAI || preset.APIBase != "https://api.x.ai/v1" {
		t.Fatalf("xai preset transport = %q %q", preset.Protocol, preset.APIBase)
	}
	if preset.TokenURL != "https://auth.x.ai/oauth2/token" || preset.ClientID != "b1a00492-073a-47ea-816f-4c329264a828" || !preset.PKCE {
		t.Fatalf("xai oauth config changed: token=%q client=%q pkce=%v", preset.TokenURL, preset.ClientID, preset.PKCE)
	}

	var got grokCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.record(r, body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions") && isStreamBody(body):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openAIToolSSE)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = io.WriteString(w, grokUnaryToolBody)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "grok", BaseURL: upstream.URL, Protocol: preset.Protocol,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		ReasoningDialect: domain.ReasoningGrok,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, Preset: "xai", AccessToken: "tok-grok",
			RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			TokenURL: preset.TokenURL, ClientID: preset.ClientID, PKCE: preset.PKCE,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "grok-4", Enabled: true},
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

	resp, out := post(t, ts.URL+"/v1/responses", key.Token, grokResponsesIngress)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unary status = %d, body = %s", resp.StatusCode, out)
	}
	assertGrokChatTransport(t, got, false)
	var unary struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &unary); err != nil {
		t.Fatal(err)
	}
	if len(unary.Output) != 1 || unary.Output[0].Type != "function_call" || unary.Output[0].Name != "get_weather" || unary.Output[0].Arguments != `{"city":"paris"}` {
		t.Fatalf("translated tool = %+v", unary.Output)
	}
	if unary.Usage.InputTokens != 3 || unary.Usage.OutputTokens != 9 {
		t.Fatalf("unary usage = %+v", unary.Usage)
	}

	got = grokCapture{}
	streamResp, streamBody := postStream(t, ts.URL+"/v1/responses", key.Token, strings.Replace(grokResponsesIngress, `{`, `{"stream":true,`, 1))
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, body = %s", streamResp.StatusCode, streamBody)
	}
	assertGrokChatTransport(t, got, true)
	name, args, done := collectResponsesToolStream(t, streamBody)
	if name != "get_weather" || args != `{"city":"paris"}` || done != `{"city":"paris"}` {
		t.Fatalf("stream tool = %q %q %q", name, args, done)
	}
	in, outTok, total := collectResponsesUsage(t, streamBody)
	if in != 3 || outTok != 9 || total != 12 {
		t.Fatalf("stream usage = %d %d %d", in, outTok, total)
	}
}

func TestExplicitResponsesGrokKeepsResponsesPath(t *testing.T) {
	var got grokCapture
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.record(r, body)
		if !strings.HasSuffix(r.URL.Path, "/responses") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responsesUpstreamBody)
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "grok-explicit", BaseURL: upstream.URL, APIKey: "up-key",
		Protocol: domain.ProtocolOpenAIResponses, AuthMethod: domain.AuthAPIKey,
		ReasoningDialect: domain.ReasoningGrok,
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Targets: []domain.ComboTarget{
		{ProviderID: prov.ID, UpstreamModel: "grok-4", Enabled: true},
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

	resp, body := post(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got.path != "/responses" || got.auth != "Bearer up-key" {
		t.Fatalf("explicit responses transport path=%q auth=%q", got.path, got.auth)
	}
	if !strings.Contains(string(body), "hello from responses") {
		t.Fatalf("responses body not relayed: %s", body)
	}
}

type grokCapture struct {
	path    string
	auth    string
	headers http.Header
	body    []byte
}

func (g *grokCapture) record(r *http.Request, body []byte) {
	g.path = r.URL.Path
	g.auth = r.Header.Get("Authorization")
	g.headers = r.Header.Clone()
	g.body = append([]byte(nil), body...)
}

func assertGrokChatTransport(t *testing.T, got grokCapture, stream bool) {
	t.Helper()
	if got.path != "/chat/completions" {
		t.Fatalf("upstream path = %q, want /chat/completions", got.path)
	}
	if got.auth != "Bearer tok-grok" {
		t.Fatalf("auth = %q, want bearer", got.auth)
	}
	for key, values := range got.headers {
		joined := strings.ToLower(key + ":" + strings.Join(values, ","))
		if strings.Contains(joined, "grok-cli") || strings.Contains(joined, "cli-chat-proxy") || strings.Contains(key, "X-Grok") {
			t.Fatalf("unexpected CLI header %s", key)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "grok-4" {
		t.Fatalf("model = %v", body["model"])
	}
	if _, ok := body["input"]; ok {
		t.Fatal("chat body still has responses input")
	}
	if stream {
		opts, _ := body["stream_options"].(map[string]any)
		if opts["include_usage"] != true {
			t.Fatalf("stream_options = %#v", body["stream_options"])
		}
	}
}

func isStreamBody(body []byte) bool {
	var m struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &m) == nil && m.Stream
}

const grokResponsesIngress = `{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`

const grokUnaryToolBody = `{"id":"chatcmpl-9","object":"chat.completion","model":"grok-4","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}`
