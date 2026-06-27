package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
