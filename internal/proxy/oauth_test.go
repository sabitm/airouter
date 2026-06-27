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
		{ProviderID: prov.ID, UpstreamModel: "grok-4"},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	New(st, false, nil).Mount(mux)
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
		{ProviderID: prov.ID, UpstreamModel: "grok-4"},
	}}); err != nil {
		t.Fatal(err)
	}
	key, _ := st.NewAccessKey(ctx, "test")
	mux := http.NewServeMux()
	New(st, false, nil).Mount(mux)
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
