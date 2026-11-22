package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/store"
)

type localOnlyTransport struct {
	next     http.RoundTripper
	mu       sync.Mutex
	allowed  map[string]struct{}
	rejected atomic.Int32
}

func installLocalTransport(t *testing.T) *localOnlyTransport {
	t.Helper()
	next := http.DefaultTransport
	tr := &localOnlyTransport{next: next, allowed: map[string]struct{}{}}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = next })
	return tr
}

func (tr *localOnlyTransport) allow(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("allow %q: %v", raw, err)
	}
	tr.mu.Lock()
	tr.allowed[u.Host] = struct{}{}
	tr.mu.Unlock()
}

func (tr *localOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	_, ok := tr.allowed[req.URL.Host]
	tr.mu.Unlock()
	if !ok {
		tr.rejected.Add(1)
		return nil, fmt.Errorf("test transport rejected %s %s", req.Method, req.URL.Redacted())
	}
	return tr.next.RoundTrip(req)
}

type authAttempt struct {
	method, uri, auth, beta, session, account, device, bodySession string
	helper                                                         bool
	body                                                           []byte
}

func parseAuthAttempt(r *http.Request, body []byte) authAttempt {
	got := authAttempt{
		method: r.Method, uri: r.URL.RequestURI(), auth: r.Header.Get("Authorization"),
		beta: r.Header.Get("anthropic-beta"), session: r.Header.Get(claudecode.SessionIDHeader),
		body: append([]byte(nil), body...),
	}
	_, got.helper = r.Header["X-Stainless-Helper-Method"]
	var doc struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.Metadata.UserID != "" {
		var uid claudecode.UserID
		if json.Unmarshal([]byte(doc.Metadata.UserID), &uid) == nil {
			got.account = uid.AccountUUID
			got.device = uid.DeviceID
			got.bodySession = uid.SessionID
		}
	}
	return got
}

func betaExact(header, flag string) bool {
	count := 0
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == flag {
			count++
		}
	}
	return count == 1
}

func betaAbsent(header, flag string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == flag {
			return false
		}
	}
	return true
}

func writeClaudeResult(w http.ResponseWriter, body []byte, status int) {
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"no"}}`)
		return
	}
	if strings.Contains(string(body), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, finding9SSE(
			finding9Frame("message_start", `{"type":"message_start","message":{"id":"msg_auth","type":"message","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":2}}}`),
			finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			finding9Frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			finding9Frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
			finding9Frame("message_stop", `{"type":"message_stop"}`),
		))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"msg_auth","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
}

func postAuth(t *testing.T, rawURL, token, body string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
	req.Header.Set("anthropic-beta", claudecode.BetaOAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func newClaudeAuthProxy(t *testing.T, tr *localOnlyTransport, effective string, creds domain.OAuthCreds, method domain.AuthMethod, upstream http.HandlerFunc, tokenHits *atomic.Int32, onToken func(http.ResponseWriter, *http.Request)) (string, string, *store.Store, int64) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	tr.allow(t, up.URL)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tokenHits != nil {
			tokenHits.Add(1)
		}
		if onToken == nil {
			t.Errorf("unexpected token request")
			http.Error(w, "unexpected", http.StatusBadGateway)
			return
		}
		onToken(w, r)
	}))
	t.Cleanup(tokenSrv.Close)
	tr.allow(t, tokenSrv.URL)
	if creds.TokenURL == "" {
		creds.TokenURL = tokenSrv.URL
	}
	if creds.ClientID == "" {
		creds.ClientID = "cid-synthetic"
	}
	if creds.RefreshToken == "" {
		creds.RefreshToken = "rt-synthetic"
	}
	if creds.ClaudeDeviceID == "" {
		creds.ClaudeDeviceID = strings.Repeat("ab", 32)
	}
	creds.ClaudeCodeAuth = true
	st := newTestStore(t)
	if method == "" {
		method = domain.AuthAPIKey
	}
	prov := &domain.Provider{
		Name: "cc", BaseURL: up.URL, Protocol: domain.ProtocolClaudeCode,
		APIKey: effective, AuthMethod: method, AuthScheme: domain.AuthBearer, OAuthCreds: &creds,
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{
		Name: "default", Strategy: domain.StrategyFailover,
		Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-5", Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	tr.allow(t, ts.URL)
	return ts.URL, key.Token, st, prov.ID
}

func assertPairedAttempt(t *testing.T, got authAttempt, token, account, device string, wantOAuth bool) {
	t.Helper()
	if got.method != http.MethodPost || got.uri != "/messages?beta=true" {
		t.Fatalf("upstream request = %s %s", got.method, got.uri)
	}
	if got.helper {
		t.Fatal("request claimed an SDK helper")
	}
	if got.auth != "Bearer "+token {
		t.Fatalf("auth = %q", got.auth)
	}
	if wantOAuth {
		if !betaExact(got.beta, claudecode.BetaOAuth) {
			t.Fatalf("oauth beta missing or repeated: %q", got.beta)
		}
	} else if !betaAbsent(got.beta, claudecode.BetaOAuth) {
		t.Fatalf("caller oauth beta was not filtered: %q", got.beta)
	}
	if got.account != account || got.device != device {
		t.Fatalf("identity account %q device %q, want %q / %q; body %s", got.account, got.device, account, device, got.body)
	}
	if got.session == "" || got.session != got.bodySession {
		t.Fatalf("session header %q body %q; body %s", got.session, got.bodySession, got.body)
	}
	if token != "" && strings.Contains(string(got.body), token) {
		t.Fatal("token leaked into body")
	}
}

func TestClaudeCodeAuthPolicyIngressMatrix(t *testing.T) {
	const device = "abababababababababababababababababababababababababababababababab"
	const oat = "sk-ant-oat-synthetic"
	const opaque = "opaque-synthetic"
	cases := []struct {
		name, token, scopes, account string
		wantOAuth                    bool
	}{
		{name: "verified inference", token: opaque, scopes: "user:inference", account: "acct-inference", wantOAuth: true},
		{name: "verified non-inference", token: oat, scopes: "user:profile", account: "acct-profile", wantOAuth: false},
		{name: "unknown oat", token: oat, scopes: "", account: "acct-unknown", wantOAuth: true},
		{name: "unknown opaque", token: opaque, scopes: " \t\n", account: "acct-blank", wantOAuth: false},
	}
	ingresses := []struct{ name, path, body string }{
		{"chat/unary", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}]}`},
		{"chat/sse", "/v1/chat/completions", `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses/unary", "/v1/responses", `{"model":"default","input":"hi"}`},
		{"responses/sse", "/v1/responses", `{"model":"default","stream":true,"input":"hi"}`},
		{"messages/unary", "/v1/messages", `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"messages/sse", "/v1/messages", `{"model":"default","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installLocalTransport(t)
			var mu sync.Mutex
			var got []authAttempt
			var tokenHits atomic.Int32
			upstream := func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				got = append(got, parseAuthAttempt(r, body))
				mu.Unlock()
				writeClaudeResult(w, body, http.StatusOK)
			}
			base, key, _, _ := newClaudeAuthProxy(t, tr, "", domain.OAuthCreds{
				AccessToken: tc.token, ClaudeScopes: tc.scopes, ClaudeAccountUUID: tc.account,
				ClaudeDeviceID: device, ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}, domain.AuthOAuth, upstream, &tokenHits, nil)
			var session string
			for _, ing := range ingresses {
				t.Run(ing.name, func(t *testing.T) {
					resp, out := postAuth(t, base+ing.path, key, ing.body)
					if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
						t.Fatalf("status %d body %s", resp.StatusCode, out)
					}
				})
			}
			if tokenHits.Load() != 0 || tr.rejected.Load() != 0 || len(got) != len(ingresses) {
				t.Fatalf("token hits %d rejected %d upstream %d", tokenHits.Load(), tr.rejected.Load(), len(got))
			}
			for i, attempt := range got {
				assertPairedAttempt(t, attempt, tc.token, tc.account, device, tc.wantOAuth)
				if i == 0 {
					session = attempt.session
				} else if attempt.session != session {
					t.Fatalf("session changed across ingresses: %s -> %s", session, attempt.session)
				}
			}
		})
	}
}

func TestClaudeCodeAuthPolicyAPIKeyIgnoresConfigGrant(t *testing.T) {
	const configDevice = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	cases := []struct {
		name, token, access, scopes string
		wantOAuth                   bool
	}{
		{name: "matching inference grant", token: "opaque-static", access: "opaque-static", scopes: "user:inference", wantOAuth: false},
		{name: "mismatched non-inference grant", token: "sk-ant-oat-static", access: "other-static", scopes: "user:profile", wantOAuth: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installLocalTransport(t)
			var got authAttempt
			var tokenHits atomic.Int32
			upstream := func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = parseAuthAttempt(r, body)
				writeClaudeResult(w, body, http.StatusOK)
			}
			base, key, st, id := newClaudeAuthProxy(t, tr, tc.token, domain.OAuthCreds{
				AccessToken: tc.access, ClaudeScopes: tc.scopes, ClaudeAccountUUID: "config-account",
				ClaudeDeviceID: configDevice, ExpiresAt: time.Now().Add(time.Minute).Unix(),
			}, domain.AuthAPIKey, upstream, &tokenHits, nil)
			resp, out := postAuth(t, base+"/v1/messages", key, `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
				t.Fatalf("status %d body %s", resp.StatusCode, out)
			}
			saved, err := st.GetProvider(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			assertPairedAttempt(t, got, tc.token, "", syntheticAPIKeyDevice(saved), tc.wantOAuth)
			if got.device == configDevice || tokenHits.Load() != 0 || tr.rejected.Load() != 0 {
				t.Fatalf("api-key request used oauth identity or refresh: device %s hits %d", got.device, tokenHits.Load())
			}
		})
	}
}

func TestClaudeCodeAuthPolicyRefreshGrantDirections(t *testing.T) {
	const device = "abababababababababababababababababababababababababababababababab"
	directions := []struct {
		name, oldToken, oldScopes, oldAccount, newToken, newScopes, newAccount string
		oldOAuth, newOAuth                                                     bool
	}{
		{name: "inference to profile", oldToken: "opaque-old", oldScopes: "user:inference", oldAccount: "acct-old", newToken: "sk-ant-oat-new", newScopes: "user:profile", newAccount: "acct-new", oldOAuth: true, newOAuth: false},
		{name: "profile to inference", oldToken: "sk-ant-oat-old", oldScopes: "user:profile", oldAccount: "acct-old", newToken: "opaque-new", newScopes: "user:inference", newAccount: "acct-new", oldOAuth: false, newOAuth: true},
	}
	modes := []struct {
		name      string
		proactive bool
	}{
		{name: "proactive", proactive: true},
		{name: "reactive", proactive: false},
	}
	ingresses := []struct{ name, path, body string }{
		{"chat/unary", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}]}`},
		{"chat/sse", "/v1/chat/completions", `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses/unary", "/v1/responses", `{"model":"default","input":"hi"}`},
		{"responses/sse", "/v1/responses", `{"model":"default","stream":true,"input":"hi"}`},
		{"messages/unary", "/v1/messages", `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{"messages/sse", "/v1/messages", `{"model":"default","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, dir := range directions {
		for _, mode := range modes {
			for _, ing := range ingresses {
				t.Run(dir.name+"/"+mode.name+"/"+ing.name, func(t *testing.T) {
					tr := installLocalTransport(t)
					var mu sync.Mutex
					var got []authAttempt
					var calls atomic.Int32
					var tokenHits atomic.Int32
					upstream := func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						n := calls.Add(1)
						mu.Lock()
						got = append(got, parseAuthAttempt(r, body))
						mu.Unlock()
						if !mode.proactive && n == 1 {
							writeClaudeResult(w, body, http.StatusUnauthorized)
							return
						}
						writeClaudeResult(w, body, http.StatusOK)
					}
					expiry := time.Now().Add(time.Hour).Unix()
					if mode.proactive {
						expiry = time.Now().Add(time.Minute).Unix()
					}
					onToken := func(w http.ResponseWriter, r *http.Request) {
						var payload map[string]string
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Errorf("token body: %v", err)
						}
						if payload["grant_type"] != "refresh_token" || payload["refresh_token"] != "rt-old" || payload["client_id"] != "cid-synthetic" || payload["scope"] != dir.oldScopes {
							t.Errorf("token payload = %#v", payload)
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]any{
							"access_token": dir.newToken, "refresh_token": "rt-next", "expires_in": 3600,
							"scope": dir.newScopes, "account": map[string]string{"uuid": dir.newAccount},
						})
					}
					base, key, st, id := newClaudeAuthProxy(t, tr, "", domain.OAuthCreds{
						AccessToken: dir.oldToken, RefreshToken: "rt-old", ClientID: "cid-synthetic",
						ClaudeScopes: dir.oldScopes, ClaudeAccountUUID: dir.oldAccount, ClaudeDeviceID: device,
						ExpiresAt: expiry,
					}, domain.AuthOAuth, upstream, &tokenHits, onToken)
					resp, out := postAuth(t, base+ing.path, key, ing.body)
					if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
						t.Fatalf("status %d body %s", resp.StatusCode, out)
					}
					wantCalls := 1
					if !mode.proactive {
						wantCalls = 2
					}
					if int(calls.Load()) != wantCalls || tokenHits.Load() != 1 || tr.rejected.Load() != 0 || len(got) != wantCalls {
						t.Fatalf("calls %d token %d rejected %d captured %d", calls.Load(), tokenHits.Load(), tr.rejected.Load(), len(got))
					}
					if mode.proactive {
						assertPairedAttempt(t, got[0], dir.newToken, dir.newAccount, device, dir.newOAuth)
					} else {
						assertPairedAttempt(t, got[0], dir.oldToken, dir.oldAccount, device, dir.oldOAuth)
						assertPairedAttempt(t, got[1], dir.newToken, dir.newAccount, device, dir.newOAuth)
						if got[0].session == got[1].session {
							t.Fatal("account change kept the same session")
						}
					}
					saved, err := st.GetProvider(context.Background(), id)
					if err != nil {
						t.Fatal(err)
					}
					if saved.OAuthCreds.AccessToken != dir.newToken || saved.OAuthCreds.ClaudeScopes != dir.newScopes || saved.OAuthCreds.ClaudeAccountUUID != dir.newAccount || saved.OAuthCreds.ClaudeDeviceID != device {
						t.Fatalf("stored snapshot = token %q scopes %q account %q device %q", saved.OAuthCreds.AccessToken, saved.OAuthCreds.ClaudeScopes, saved.OAuthCreds.ClaudeAccountUUID, saved.OAuthCreds.ClaudeDeviceID)
					}
				})
			}
		}
	}
}

func TestClaudeCodeAuthPolicySingleReactiveAndNoRetryAfterCommit(t *testing.T) {
	const device = "abababababababababababababababababababababababababababababababab"
	t.Run("second rejection does not refresh again", func(t *testing.T) {
		for _, ing := range []struct{ name, path, body string }{
			{"messages/unary", "/v1/messages", `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
			{"messages/sse", "/v1/messages", `{"model":"default","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		} {
			t.Run(ing.name, func(t *testing.T) {
				tr := installLocalTransport(t)
				var calls atomic.Int32
				var tokenHits atomic.Int32
				upstream := func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					calls.Add(1)
					writeClaudeResult(w, body, http.StatusUnauthorized)
				}
				onToken := func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"access_token":"opaque-refreshed","refresh_token":"rt-next","expires_in":3600,"scope":"user:profile","account":{"uuid":"acct-new"}}`)
				}
				base, key, _, _ := newClaudeAuthProxy(t, tr, "", domain.OAuthCreds{
					AccessToken: "sk-ant-oat-old", RefreshToken: "rt-old", ClaudeScopes: "user:inference",
					ClaudeAccountUUID: "acct-old", ClaudeDeviceID: device, ExpiresAt: time.Now().Add(time.Hour).Unix(),
				}, domain.AuthOAuth, upstream, &tokenHits, onToken)
				resp, _ := postAuth(t, base+ing.path, key, ing.body)
				if resp.StatusCode == http.StatusOK || calls.Load() != 2 || tokenHits.Load() != 1 || tr.rejected.Load() != 0 {
					t.Fatalf("status %d calls %d token %d rejected %d", resp.StatusCode, calls.Load(), tokenHits.Load(), tr.rejected.Load())
				}
			})
		}
	})
	t.Run("committed stream does not refresh", func(t *testing.T) {
		tr := installLocalTransport(t)
		var calls atomic.Int32
		var tokenHits atomic.Int32
		upstream := func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, finding9SSE(
				finding9Frame("message_start", `{"type":"message_start","message":{"id":"msg_auth","type":"message","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":2}}}`),
				finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			))
		}
		base, key, _, _ := newClaudeAuthProxy(t, tr, "", domain.OAuthCreds{
			AccessToken: "opaque-live", ClaudeScopes: "user:inference", ClaudeAccountUUID: "acct-live",
			ClaudeDeviceID: device, ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}, domain.AuthOAuth, upstream, &tokenHits, nil)
		resp, out := postAuth(t, base+"/v1/chat/completions", key, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
			t.Fatalf("status %d body %s", resp.StatusCode, out)
		}
		if calls.Load() != 1 || tokenHits.Load() != 0 || tr.rejected.Load() != 0 {
			t.Fatalf("calls %d token %d rejected %d", calls.Load(), tokenHits.Load(), tr.rejected.Load())
		}
	})
}
