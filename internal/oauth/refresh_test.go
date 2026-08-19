package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
)

// fakeStore is a ProviderStore that records OAuth writes and lets tests inspect
// the last persisted creds. It also counts refresh-triggered writes.
type fakeStore struct {
	mu       sync.Mutex
	creds    map[int64]*domain.OAuthCreds
	writes   atomic.Int64
	writeErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{creds: map[int64]*domain.OAuthCreds{}}
}

func (f *fakeStore) GetProvider(_ context.Context, id int64) (*domain.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *c
	return &domain.Provider{ID: id, AuthMethod: domain.AuthOAuth, OAuthCreds: &cp}, nil
}

func (f *fakeStore) UpdateProviderOAuth(_ context.Context, id int64, creds *domain.OAuthCreds) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	cp := *creds
	f.creds[id] = &cp
	f.writes.Add(1)
	return nil
}

// tokenTestServer returns a token endpoint whose handler inspects the refresh
// request and responds with a configurable body + status. The handler records
// the form values seen.
func tokenTestServer(t *testing.T, fn func(form url.Values) (status int, body string)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = r.ParseForm()
		status, body := fn(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newCreds(srv *httptest.Server) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode:         domain.OAuthManual,
		TokenURL:     srv.URL,
		ClientID:     "cid",
		RefreshToken: "rt-old",
		AccessToken:  "tok-old",
		ExpiresAt:    1, // expired
	}
}

func TestRefreshSuccess(t *testing.T) {
	srv, hits := tokenTestServer(t, func(form url.Values) (int, string) {
		if form.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", form.Get("grant_type"))
		}
		if form.Get("client_id") != "cid" {
			t.Errorf("client_id = %q", form.Get("client_id"))
		}
		if form.Get("refresh_token") != "rt-old" {
			t.Errorf("refresh_token = %q", form.Get("refresh_token"))
		}
		if form.Get("client_secret") != "" {
			t.Errorf("public client should not send client_secret")
		}
		return 200, `{"access_token":"tok-new","refresh_token":"rt-new","expires_in":3600,"id_token":"` + idToken(t, "u@x.com") + `"}`
	})
	c := newCreds(srv)
	now := time.Unix(1000, 0)

	if err := refresh(context.Background(), c, now); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "tok-new" {
		t.Errorf("access = %q", c.AccessToken)
	}
	if c.RefreshToken != "rt-new" {
		t.Errorf("refresh = %q (want rotated)", c.RefreshToken)
	}
	if c.Email != "u@x.com" {
		t.Errorf("email = %q", c.Email)
	}
	if c.ExpiresAt != now.Add(3600*time.Second).Unix() {
		t.Errorf("expires_at = %d", c.ExpiresAt)
	}
	if hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1", hits.Load())
	}
}

func TestRefreshKeepsOldTokenWhenNotRotated(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-new","expires_in":60}`
	})
	c := newCreds(srv)
	if err := refresh(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != "rt-old" {
		t.Errorf("refresh = %q, want kept rt-old", c.RefreshToken)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 400, `{"error":"invalid_grant"}`
	})
	c := newCreds(srv)
	err := refresh(context.Background(), c, time.Now())
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	// Failed refresh must not mutate the caller's creds.
	if c.AccessToken != "tok-old" {
		t.Errorf("access changed on failure: %q", c.AccessToken)
	}
}

// TestRefreshInvalidGrantNestedEnvelope covers the ChatGPT/OpenAI backend, which
// nests the error as an object rather than the OAuth2 flat string. The nested
// shape must still classify as ErrInvalidGrant instead of failing the decode.
func TestRefreshInvalidGrantNestedEnvelope(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 401, `{"error":{"message":"Your authentication token has been invalidated. Please try signing in again.","type":"invalid_request_error","code":"token_invalidated","param":null},"status":401}`
	})
	c := newCreds(srv)
	c.RefreshJSON = true
	err := refresh(context.Background(), c, time.Now())
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	if c.AccessToken != "tok-old" {
		t.Errorf("access changed on failure: %q", c.AccessToken)
	}
}

// kiroJSONServer returns a server that decodes the JSON refresh body (Kiro uses
// JSON, not form) and responds with the given status + body. It records the last
// decoded body so branch-specific fields can be asserted.
func kiroJSONServer(t *testing.T, status int, body string) (*httptest.Server, *map[string]any) {
	t.Helper()
	last := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &last)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

// TestRefreshKiroOIDC covers the AWS SSO OIDC branch: clientId + clientSecret
// present, JSON body, camelCase response, profileArn patched when unset.
func TestRefreshKiroOIDC(t *testing.T) {
	srv, seen := kiroJSONServer(t, 200,
		`{"accessToken":"tok-new","refreshToken":"rt-new","expiresIn":3600,"profileArn":"arn:from-refresh"}`)
	orig := kiroOIDCTokenURL
	kiroOIDCTokenURL = func(string) string { return srv.URL }
	t.Cleanup(func() { kiroOIDCTokenURL = orig })

	c := &domain.OAuthCreds{
		KiroAuth: "idc", ClientID: "cid", ClientSecret: "secret",
		RefreshToken: "rt-old", AccessToken: "tok-old", ExpiresAt: 1,
	}
	now := time.Unix(1000, 0)
	if err := refresh(context.Background(), c, now); err != nil {
		t.Fatal(err)
	}
	if (*seen)["clientId"] != "cid" || (*seen)["clientSecret"] != "secret" || (*seen)["grantType"] != "refresh_token" {
		t.Errorf("OIDC body = %+v", *seen)
	}
	if c.AccessToken != "tok-new" || c.RefreshToken != "rt-new" {
		t.Errorf("tokens = %q/%q", c.AccessToken, c.RefreshToken)
	}
	if c.ExpiresAt != now.Add(3600*time.Second).Unix() {
		t.Errorf("expires_at = %d", c.ExpiresAt)
	}
	if c.ProfileArn != "arn:from-refresh" {
		t.Errorf("profileArn = %q, want patched from refresh response", c.ProfileArn)
	}
}

// TestRefreshKiroOIDCUnauthorized verifies the OIDC branch treats a bare 401 with
// no parseable error body (a revoked refresh token or dead client registration)
// as ErrInvalidGrant, so refresh-all flags reconnect instead of a raw failure.
func TestRefreshKiroOIDCUnauthorized(t *testing.T) {
	srv, _ := kiroJSONServer(t, 401, `{}`)
	orig := kiroOIDCTokenURL
	kiroOIDCTokenURL = func(string) string { return srv.URL }
	t.Cleanup(func() { kiroOIDCTokenURL = orig })

	c := &domain.OAuthCreds{
		KiroAuth: "builder-id", ClientID: "cid", ClientSecret: "secret",
		RefreshToken: "rt-old", AccessToken: "tok-old", ExpiresAt: 1,
	}
	err := refresh(context.Background(), c, time.Unix(1000, 0))
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	if c.AccessToken != "tok-old" {
		t.Errorf("access changed on failure: %q", c.AccessToken)
	}
}

// TestRefreshKiroSocial covers the social branch: no client secret, fixed social
// endpoint, camelCase response. A configured profileArn is not overwritten.
func TestRefreshKiroSocial(t *testing.T) {
	srv, seen := kiroJSONServer(t, 200,
		`{"accessToken":"tok-s","refreshToken":"rt-s","expiresIn":3600,"profileArn":"arn:ignored"}`)
	orig := kiroSocialRefreshURL
	kiroSocialRefreshURL = srv.URL
	t.Cleanup(func() { kiroSocialRefreshURL = orig })

	c := &domain.OAuthCreds{
		KiroAuth: "social", RefreshToken: "rt-old", AccessToken: "tok-old",
		ProfileArn: "arn:configured", ExpiresAt: 1,
	}
	if err := refresh(context.Background(), c, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if (*seen)["refreshToken"] != "rt-old" {
		t.Errorf("social body = %+v", *seen)
	}
	if _, ok := (*seen)["clientId"]; ok {
		t.Errorf("social branch should not send clientId: %+v", *seen)
	}
	if c.AccessToken != "tok-s" {
		t.Errorf("access = %q", c.AccessToken)
	}
	if c.ProfileArn != "arn:configured" {
		t.Errorf("profileArn = %q, want kept configured value", c.ProfileArn)
	}
}

// TestRefreshKiroExternalIDPUsesGenericPath verifies external_idp is NOT routed
// to the Kiro JSON refresh: it falls through to the generic form-based path,
// hitting the standard TokenURL.
func TestRefreshKiroExternalIDPUsesGenericPath(t *testing.T) {
	srv, hits := tokenTestServer(t, func(form url.Values) (int, string) {
		if form.Get("grant_type") != "refresh_token" {
			t.Errorf("expected form body, got %+v", form)
		}
		return 200, `{"access_token":"tok-eidp","expires_in":3600}`
	})
	c := &domain.OAuthCreds{
		KiroAuth: "external_idp", TokenURL: srv.URL, ClientID: "cid", RefreshToken: "rt-old",
	}
	if err := refresh(context.Background(), c, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "tok-eidp" {
		t.Errorf("access = %q", c.AccessToken)
	}
	if hits.Load() != 1 {
		t.Errorf("generic token endpoint hits = %d, want 1", hits.Load())
	}
}

func TestCanRefresh(t *testing.T) {
	cases := []struct {
		name  string
		creds *domain.OAuthCreds
		want  bool
	}{
		{"nil", nil, false},
		{"qoder device", &domain.OAuthCreds{QoderAuth: true, RefreshToken: "rt"}, false},
		{"qoder even when expired", &domain.OAuthCreds{QoderAuth: true, ExpiresAt: 1}, false},
		{"cursor access-only import", &domain.OAuthCreds{CursorAuth: true, ExpiresAt: 1}, false},
		{"cursor session jwt", &domain.OAuthCreds{CursorAuth: true, RefreshToken: cursorSessionJWTForTest(t, time.Now().Add(time.Hour).Unix())}, false},
		{"cursor with refresh token", &domain.OAuthCreds{CursorAuth: true, RefreshToken: "rt"}, true},
		{"plain oauth", &domain.OAuthCreds{RefreshToken: "rt"}, true},
		{"kiro", &domain.OAuthCreds{KiroAuth: "builder-id", RefreshToken: "rt"}, true},
		{"cline", &domain.OAuthCreds{ClineAuth: true, RefreshToken: "rt"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanRefresh(tc.creds); got != tc.want {
				t.Errorf("CanRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldRefreshGate(t *testing.T) {
	now := time.Unix(10000, 0)
	cases := []struct {
		name  string
		creds *domain.OAuthCreds
		want  bool
	}{
		{"nil", nil, false},
		{"unknown expiry", &domain.OAuthCreds{ExpiresAt: 0}, false},
		{"far future", &domain.OAuthCreds{ExpiresAt: now.Add(1 * time.Hour).Unix()}, false},
		{"within lead", &domain.OAuthCreds{ExpiresAt: now.Add(2 * time.Minute).Unix()}, true},
		{"already expired", &domain.OAuthCreds{ExpiresAt: now.Add(-1 * time.Minute).Unix()}, true},
		{"cursor access-only expired", &domain.OAuthCreds{CursorAuth: true, ExpiresAt: now.Add(-1 * time.Minute).Unix()}, false},
		{"cursor refreshable near expiry", &domain.OAuthCreds{CursorAuth: true, RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Minute).Unix()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRefresh(tc.creds, now); got != tc.want {
				t.Errorf("shouldRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveAPIKeyPassthrough(t *testing.T) {
	s := New(newFakeStore())
	tok, err := s.Resolve(context.Background(), &domain.Provider{
		AuthMethod: domain.AuthAPIKey, APIKey: "static-key",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "static-key" {
		t.Errorf("apikey resolve = %q", tok)
	}
}

func TestResolveProactiveRefreshPersists(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-fresh","refresh_token":"rt-fresh","expires_in":3600}`
	})
	store := newFakeStore()
	// Seed the store with the provider's existing creds; Resolve re-reads from
	// the store (doRefresh works against the live creds).
	creds := newCreds(srv)
	creds.ExpiresAt = 1
	store.creds[7] = creds

	s := New(store)
	p := &domain.Provider{ID: 7, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}

	tok, err := s.Resolve(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-fresh" {
		t.Errorf("token = %q, want tok-fresh", tok)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
	// Persisted creds must reflect the rotation.
	store.mu.Lock()
	persisted := store.creds[7]
	store.mu.Unlock()
	if persisted.AccessToken != "tok-fresh" || persisted.RefreshToken != "rt-fresh" {
		t.Errorf("persisted = %+v", persisted)
	}
}

func TestResolveSkipsRefreshWhenNotExpiring(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"should-not-happen"}`
	})
	store := newFakeStore()
	s := New(store)
	creds := newCreds(srv)
	creds.ExpiresAt = s.now().Add(1 * time.Hour).Unix() // far future
	store.creds[1] = creds

	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-old" {
		t.Errorf("token = %q, want existing tok-old", tok)
	}
	if hits.Load() != 0 {
		t.Errorf("token endpoint should not be hit, got %d", hits.Load())
	}
}

func TestResolveForcedRefreshOn401(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-forced","refresh_token":"rt-2","expires_in":3600}`
	})
	store := newFakeStore()
	s := New(store)
	creds := newCreds(srv)
	creds.ExpiresAt = s.now().Add(1 * time.Hour).Unix() // not near expiry
	store.creds[1] = creds

	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true) // force (reactive 401)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-forced" {
		t.Errorf("token = %q, want tok-forced", tok)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

// TestResolveDedupesConcurrentRefreshes fires N concurrent proactive resolves
// for an expired token; the token endpoint must be hit exactly once.
func TestResolveDedupesConcurrentRefreshes(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		time.Sleep(20 * time.Millisecond) // widen the window so callers overlap
		return 200, `{"access_token":"tok-x","refresh_token":"rt-x","expires_in":3600}`
	})
	store := newFakeStore()
	creds := newCreds(srv)
	creds.ExpiresAt = 1
	store.creds[1] = creds

	s := New(store)
	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Resolve(context.Background(), p, false); err != nil {
				t.Errorf("resolve err: %v", err)
			}
		}()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (dedup)", hits.Load())
	}
}

func TestResolveForcedFailureReturnsOldToken(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		return 400, `{"error":"server_error","error_description":"boom"}`
	})
	store := newFakeStore()
	creds := newCreds(srv)
	creds.AccessToken = "tok-stale"
	store.creds[1] = creds

	s := New(store)
	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true)
	if err == nil {
		t.Fatal("want error from forced refresh failure")
	}
	if tok != "tok-stale" {
		t.Errorf("token = %q, want fallback tok-stale", tok)
	}
}

// TestResolveForcedQoderSurfacesInvalidGrant confirms the reactive 401 path is
// not weakened by the bulk-refresh skip: a forced Resolve on a Qoder device
// token still returns ErrInvalidGrant (so the proxy can prompt reconnect) and
// falls back to the current token. No token-endpoint HTTP is made.
func TestResolveForcedQoderSurfacesInvalidGrant(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		t.Error("qoder refresh must not hit the token endpoint")
		return 500, ""
	})
	store := newFakeStore()
	creds := &domain.OAuthCreds{
		Mode: domain.OAuthManual, QoderAuth: true,
		AccessToken: "device-tok", RefreshToken: "ignored",
		TokenURL: srv.URL, ClientID: "cid",
	}
	store.creds[1] = creds
	s := New(store)
	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true)
	if !IsInvalidGrant(err) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	if tok != "device-tok" {
		t.Errorf("token = %q, want fallback device-tok", tok)
	}
	if hits.Load() != 0 {
		t.Errorf("token endpoint hits = %d, want 0", hits.Load())
	}
}

// idToken builds an unsigned JWT with the given email claim for tests.
func idToken(t *testing.T, email string) string {
	return idTokenWith(t, email, "")
}

// idTokenWith builds an unsigned JWT carrying an email and, when non-empty, a
// ChatGPT account id under OpenAI's namespaced claim (with the flat fallback
// claim too, so the extractor's precedence is exercised).
func idTokenWith(t *testing.T, email, accountID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := fmt.Sprintf(`{"email":%q`, email)
	if accountID != "" {
		payload += fmt.Sprintf(`,"https://api.openai.com/auth":{"chatgpt_account_id":%q},"account_id":%q`, accountID, accountID)
	}
	payload += "}"
	return header + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + "."
}

// jsonTokenServer is a token endpoint that captures the raw request body and
// content-type, for asserting the JSON refresh path used by Codex.
func jsonTokenServer(t *testing.T, fn func(body []byte, contentType string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		status, resp := fn(body, r.Header.Get("Content-Type"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshJSONBody(t *testing.T) {
	srv := jsonTokenServer(t, func(body []byte, ct string) (int, string) {
		if ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		var m map[string]string
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if m["grant_type"] != "refresh_token" || m["refresh_token"] != "rt-old" || m["client_id"] != "cid" {
			t.Errorf("unexpected JSON body: %v", m)
		}
		return 200, `{"access_token":"tok-json","refresh_token":"rt-json","expires_in":3600,"id_token":"` + idTokenWith(t, "u@codex.com", "acct-9") + `"}`
	})
	c := newCreds(srv)
	c.RefreshJSON = true
	if err := refresh(context.Background(), c, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "tok-json" || c.RefreshToken != "rt-json" {
		t.Errorf("tokens = %+v", c)
	}
	if c.AccountID != "acct-9" || c.Email != "u@codex.com" {
		t.Errorf("claims = email %q account %q", c.Email, c.AccountID)
	}
}

func TestRefreshNoRefreshToken(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		t.Error("token endpoint should not be hit without a refresh token")
		return 500, ""
	})
	c := newCreds(srv)
	c.RefreshToken = ""
	if err := refresh(context.Background(), c, time.Now()); err == nil {
		t.Fatal("want error when no refresh token is present")
	}
}

// TestRefreshCursorAccessOnlySurfacesInvalidGrant confirms access-only Cursor
// imports (no refresh token) classify as ErrInvalidGrant and never hit a
// generic token URL, so the reactive 401 path prompts reconnect/re-paste.
func TestRefreshCursorAccessOnlySurfacesInvalidGrant(t *testing.T) {
	srv, _ := tokenTestServer(t, func(url.Values) (int, string) {
		t.Error("cursor access-only refresh must not hit the generic token endpoint")
		return 500, ""
	})
	c := &domain.OAuthCreds{
		Mode: domain.OAuthManual, CursorAuth: true,
		AccessToken: "ide-tok",
		TokenURL:    srv.URL, ClientID: "cid",
	}
	if err := refresh(context.Background(), c, time.Unix(1000, 0)); !IsInvalidGrant(err) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	if c.AccessToken != "ide-tok" {
		t.Errorf("access changed on failure: %q", c.AccessToken)
	}
}

// TestResolveForcedCursorAccessOnlySurfacesInvalidGrant mirrors the Qoder
// regression for access-only Cursor imports: forced Resolve returns
// ErrInvalidGrant + the fallback token, with zero generic token-endpoint hits.
func TestResolveForcedCursorAccessOnlySurfacesInvalidGrant(t *testing.T) {
	srv, hits := tokenTestServer(t, func(url.Values) (int, string) {
		t.Error("cursor access-only refresh must not hit the generic token endpoint")
		return 500, ""
	})
	store := newFakeStore()
	creds := &domain.OAuthCreds{
		Mode: domain.OAuthManual, CursorAuth: true,
		AccessToken: "ide-tok",
		TokenURL:    srv.URL, ClientID: "cid",
	}
	store.creds[1] = creds
	s := New(store)
	p := &domain.Provider{ID: 1, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	tok, err := s.Resolve(context.Background(), p, true)
	if !IsInvalidGrant(err) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	if tok != "ide-tok" {
		t.Errorf("token = %q, want fallback ide-tok", tok)
	}
	if hits.Load() != 0 {
		t.Errorf("token endpoint hits = %d, want 0", hits.Load())
	}
}
