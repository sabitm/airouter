package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/crypto"
	"airouter/internal/domain"
	"airouter/internal/store"
)

func testHandler(t *testing.T) *Handler {
	t.Helper()
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewHandler(st, false)
}

// tokenServer is a mock OAuth token endpoint that issues a fixed token for the
// authorization_code grant. It records whether it was hit.
func tokenServer(t *testing.T, accessToken string) (*httptest.Server, *int) {
	t.Helper()
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint parse form: %v", err)
		}
		if g := r.FormValue("grant_type"); g != "authorization_code" {
			t.Errorf("grant_type = %q, want authorization_code", g)
		}
		if r.FormValue("code") == "" {
			t.Error("token endpoint: empty code")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  accessToken,
			"refresh_token": "refresh-xyz",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// beginManualConnect drives beginOAuthConnect with manual config pointing at the
// given token/auth URLs, returning the parsed connect state token.
func beginManualConnect(t *testing.T, h *Handler, tokenURL string) string {
	t.Helper()
	form := url.Values{}
	form.Set("preset", "custom")
	form.Set("auth_url", tokenURL+"/authorize")
	form.Set("token_url", tokenURL+"/token")
	form.Set("client_id", "test-client")
	form.Set("scopes", "openid")
	// Empty redirect URI so loopbackPort rejects it and no real port is bound;
	// the manual-paste path is what the test exercises.
	form.Set("redirect_uri", "")
	form.Set("pkce", "on")

	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/oauth/begin", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.beginOAuthConnect(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return parseState(t, rec.Body.String())
}

var stateRe = regexp.MustCompile(`state=([0-9a-zA-Z_\-]+)`)

func parseState(t *testing.T, body string) string {
	t.Helper()
	m := stateRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no state token in connect view: %s", body)
	}
	return m[1]
}

func TestOAuthBeginRendersAuthorizeLink(t *testing.T) {
	h := testHandler(t)
	srv, _ := tokenServer(t, "tok-1")
	state := beginManualConnect(t, h, srv.URL)
	if state == "" {
		t.Fatal("empty state")
	}
	if _, ok := h.sessions.get(state); !ok {
		t.Fatal("session not stored under state")
	}
}

func TestOAuthExchangeThenCreate(t *testing.T) {
	h := testHandler(t)
	srv, hits := tokenServer(t, "tok-create")
	state := beginManualConnect(t, h, srv.URL)

	// Manual paste of the authorization code completes the flow.
	form := url.Values{}
	form.Set("state", state)
	form.Set("code", "auth-code-123")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/oauth/exchange", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.oauthConnectExchange(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("exchange status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connected") {
		t.Fatalf("exchange did not report connected: %s", rec.Body.String())
	}
	if *hits != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", *hits)
	}

	// Save the provider, claiming the connected session by its state.
	cform := url.Values{}
	cform.Set("auth_method", "oauth")
	cform.Set("name", "grok")
	cform.Set("base_url", "https://api.x.ai/v1")
	cform.Set("protocol", "openai")
	cform.Set("oauth_session", state)
	creq := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(cform.Encode()))
	creq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crec := httptest.NewRecorder()
	h.createProvider(crec, creq)
	if crec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", crec.Code, crec.Body.String())
	}

	providers, err := h.store.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.Method() != domain.AuthOAuth {
		t.Errorf("method = %q, want oauth", p.Method())
	}
	if p.APIKey != "" {
		t.Errorf("oauth provider APIKey = %q, want empty", p.APIKey)
	}
	if p.OAuthCreds == nil || p.OAuthCreds.AccessToken != "tok-create" {
		t.Fatalf("stored creds = %+v, want access_token tok-create", p.OAuthCreds)
	}
	if p.OAuthCreds.RefreshToken != "refresh-xyz" {
		t.Errorf("refresh token = %q, want refresh-xyz", p.OAuthCreds.RefreshToken)
	}
	if p.Auth() != domain.AuthBearer {
		t.Errorf("auth scheme = %q, want bearer", p.Auth())
	}
	// The session is consumed on save.
	if _, ok := h.sessions.get(state); ok {
		t.Error("session not dropped after create")
	}
}

func TestOAuthCreateWithoutConnectRejected(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "grok")
	form.Set("base_url", "https://api.x.ai/v1")
	form.Set("protocol", "openai")
	form.Set("oauth_session", "nonexistent")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 0 {
		t.Fatalf("providers = %d, want 0", len(providers))
	}
}

func TestOAuthStatusUnknownSession(t *testing.T) {
	h := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/oauth/status?state=ghost", nil)
	rec := httptest.NewRecorder()
	h.oauthConnectStatus(rec, req)
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("status of unknown session: %s", rec.Body.String())
	}
}

func TestOAuthCancelDropsSession(t *testing.T) {
	h := testHandler(t)
	srv, _ := tokenServer(t, "tok-x")
	state := beginManualConnect(t, h, srv.URL)
	form := url.Values{}
	form.Set("state", state)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/oauth/cancel", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.oauthConnectCancel(rec, req)
	if _, ok := h.sessions.get(state); ok {
		t.Error("session still present after cancel")
	}
}

func TestOAuthPresetCreatesXAIConfig(t *testing.T) {
	form := url.Values{}
	form.Set("preset", "xai")
	creds, err := credsFromConnectForm(reqWithForm(form))
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Errorf("client id = %q", creds.ClientID)
	}
	if !creds.PKCE {
		t.Error("xai preset should be PKCE")
	}
	if creds.Mode != domain.OAuthAuto {
		t.Errorf("mode = %q, want auto", creds.Mode)
	}
}

func reqWithForm(form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// TestOAuthCheckWithSession: right after Connect (before save), Check probes the
// upstream /models with the session's access token and reports the model count.
func TestOAuthCheckWithSession(t *testing.T) {
	h := testHandler(t)
	srv, _ := tokenServer(t, "tok-check")

	// Upstream /models that accepts only the connected bearer token.
	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if sawAuth != "Bearer tok-check" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"grok-4"},{"id":"grok-3"}]}`))
	}))
	t.Cleanup(up.Close)

	state := beginManualConnect(t, h, srv.URL)
	exchangeConnect(t, h, state, "code-1")

	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("base_url", up.URL)
	form.Set("protocol", "openai")
	form.Set("oauth_session", state)
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))

	body := rec.Body.String()
	if sawAuth != "Bearer tok-check" {
		t.Errorf("upstream saw auth = %q, want Bearer tok-check", sawAuth)
	}
	if !strings.Contains(body, "2 models") {
		t.Fatalf("check result = %s, want 2 models", body)
	}
}

// TestOAuthCheckSavedProvider: a saved, connected oauth provider can be checked
// by id.
func TestOAuthCheckSavedProvider(t *testing.T) {
	h := testHandler(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stored-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	t.Cleanup(up.Close)

	p := &domain.Provider{
		Name: "grok", BaseURL: up.URL, Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{Mode: domain.OAuthAuto, AccessToken: "stored-tok"},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("base_url", up.URL)
	form.Set("protocol", "openai")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "1 models") {
		t.Fatalf("check result = %s, want 1 models", rec.Body.String())
	}
}

// TestOAuthCheckNotConnected: a Check with neither a saved id nor a connected
// session reports that connect is needed.
func TestOAuthCheckNotConnected(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("base_url", "https://api.x.ai/v1")
	form.Set("protocol", "openai")
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("check result = %s, want not connected", rec.Body.String())
	}
}

// TestOAuthCreateManualTokens: an oauth provider can be created from pasted
// tokens (no connect session), pulling its config from the chosen preset.
func TestOAuthCreateManualTokens(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "grok")
	form.Set("base_url", "https://api.x.ai/v1")
	form.Set("protocol", "openai")
	form.Set("preset", "xai")
	form.Set("access_token", "imported-access")
	form.Set("refresh_token", "imported-refresh")
	form.Set("expires_at", "2026-06-27T07:14:11Z")
	form.Set("email", "user@example.com")
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	providers, err := h.store.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.Method() != domain.AuthOAuth || p.Auth() != domain.AuthBearer {
		t.Errorf("method/scheme = %q/%q, want oauth/bearer", p.Method(), p.Auth())
	}
	if p.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", p.APIKey)
	}
	c := p.OAuthCreds
	if c == nil {
		t.Fatal("nil creds")
	}
	if c.AccessToken != "imported-access" || c.RefreshToken != "imported-refresh" {
		t.Errorf("tokens = %q/%q", c.AccessToken, c.RefreshToken)
	}
	if c.Email != "user@example.com" {
		t.Errorf("email = %q", c.Email)
	}
	wantExp, _ := time.Parse(time.RFC3339, "2026-06-27T07:14:11Z")
	if c.ExpiresAt != wantExp.Unix() {
		t.Errorf("expires_at = %d, want %d", c.ExpiresAt, wantExp.Unix())
	}
	// Config came from the xAI preset, so refresh works without a connect flow.
	if c.ClientID != "b1a00492-073a-47ea-816f-4c329264a828" || c.TokenURL == "" {
		t.Errorf("preset config not applied: client_id=%q token_url=%q", c.ClientID, c.TokenURL)
	}
}

// TestOAuthCreateManualNoTokensRejected: oauth create with config but neither a
// connect session nor pasted tokens is rejected and stores nothing.
func TestOAuthCreateManualNoTokensRejected(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "grok")
	form.Set("base_url", "https://api.x.ai/v1")
	form.Set("protocol", "openai")
	form.Set("preset", "xai")
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 0 {
		t.Fatalf("providers = %d, want 0", len(providers))
	}
}

func TestParseExpiresAt(t *testing.T) {
	rfc := "2026-06-27T07:14:11Z"
	want, _ := time.Parse(time.RFC3339, rfc)
	cases := map[string]int64{
		"":             0,
		"   ":          0,
		"not-a-time":   0,
		"1782522851":   1782522851,
		rfc:            want.Unix(),
		" 1782522851 ": 1782522851,
	}
	for in, exp := range cases {
		if got := parseExpiresAt(in); got != exp {
			t.Errorf("parseExpiresAt(%q) = %d, want %d", in, got, exp)
		}
	}
}

// TestOAuthCheckManualTokens: Check probes pasted form tokens (no connect
// session, no saved id) as-is and reports the model count.
func TestOAuthCheckManualTokens(t *testing.T) {
	h := testHandler(t)
	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if sawAuth != "Bearer pasted-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"grok-4"}]}`))
	}))
	t.Cleanup(up.Close)

	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("base_url", up.URL)
	form.Set("protocol", "openai")
	form.Set("preset", "xai")
	form.Set("access_token", "pasted-access")
	form.Set("refresh_token", "pasted-refresh")
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))

	if sawAuth != "Bearer pasted-access" {
		t.Errorf("upstream saw auth = %q, want Bearer pasted-access", sawAuth)
	}
	if !strings.Contains(rec.Body.String(), "1 models") {
		t.Fatalf("check result = %s, want 1 models", rec.Body.String())
	}
}

// TestOAuthRefreshTokens: the Refresh button mints a new access token from the
// pasted refresh token + config and re-renders the fields with it.
func TestOAuthRefreshTokens(t *testing.T) {
	h := testHandler(t)
	var sawGrant, sawRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sawGrant = r.FormValue("grant_type")
		sawRefresh = r.FormValue("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "rotated-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)

	form := url.Values{}
	form.Set("preset", "custom")
	form.Set("token_url", srv.URL+"/token")
	form.Set("client_id", "test-client")
	form.Set("access_token", "expired-access")
	form.Set("refresh_token", "old-refresh")
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))

	if sawGrant != "refresh_token" || sawRefresh != "old-refresh" {
		t.Errorf("token endpoint saw grant=%q refresh=%q", sawGrant, sawRefresh)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="fresh-access"`) {
		t.Errorf("refreshed access token not in fields: %s", body)
	}
	if !strings.Contains(body, `value="rotated-refresh"`) {
		t.Errorf("rotated refresh token not in fields: %s", body)
	}
	if !strings.Contains(body, "refreshed") {
		t.Errorf("no success status: %s", body)
	}
}

// TestOAuthRefreshNoRefreshToken: refreshing without a refresh token reports the
// requirement and does not call any endpoint.
func TestOAuthRefreshNoRefreshToken(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("preset", "custom")
	form.Set("token_url", "https://example.com/token")
	form.Set("client_id", "test-client")
	form.Set("access_token", "only-access")
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "paste a refresh token") {
		t.Fatalf("result = %s, want refresh-token requirement", rec.Body.String())
	}
}

// exchangeConnect completes a connect session via the manual-paste path.
func exchangeConnect(t *testing.T, h *Handler, state, code string) {
	t.Helper()
	form := url.Values{}
	form.Set("state", state)
	form.Set("code", code)
	rec := httptest.NewRecorder()
	h.oauthConnectExchange(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "connected") {
		t.Fatalf("exchange did not connect: %s", rec.Body.String())
	}
}
