package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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
	"airouter/internal/oauth"
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
	return NewHandler(st, nil, nil)
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

func TestOAuthPresetCreatesClineConfig(t *testing.T) {
	form := url.Values{}
	form.Set("preset", "cline")
	creds, err := credsFromConnectForm(reqWithForm(form))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.ClineAuth {
		t.Error("cline preset should set ClineAuth")
	}
	if creds.ClientID != "" {
		t.Errorf("client id = %q, want empty", creds.ClientID)
	}
	if creds.PKCE {
		t.Error("cline preset should not use PKCE")
	}
	if creds.TokenURL == "" || creds.RefreshURL == "" || creds.AuthURL == "" {
		t.Errorf("urls incomplete: %+v", creds)
	}
	if creds.Mode != domain.OAuthAuto || creds.Preset != "cline" {
		t.Errorf("mode/preset = %q/%q", creds.Mode, creds.Preset)
	}
}

func TestOAuthPresetCreatesQoderConfig(t *testing.T) {
	form := url.Values{}
	form.Set("preset", "qoder")
	creds, err := credsFromConnectForm(reqWithForm(form))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.QoderAuth {
		t.Error("qoder preset should set QoderAuth")
	}
	if creds.Preset != "qoder" {
		t.Errorf("preset = %q", creds.Preset)
	}
	// Device flow does not need interactive OAuth endpoints on the form.
	if recipe, ok := recipeByID("qoder"); !ok || recipe.Protocol != domain.ProtocolQoder || recipe.Kind != kindQoder {
		t.Fatalf("recipe missing or wrong: %+v", recipe)
	}
}

func TestOAuthPresetCreatesClaudeCodeConfig(t *testing.T) {
	form := url.Values{}
	form.Set("preset", "claude")
	creds, err := credsFromConnectForm(reqWithForm(form))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.ClaudeCodeAuth {
		t.Error("claude preset should set ClaudeCodeAuth")
	}
	if creds.ClientID != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
		t.Errorf("client id = %q", creds.ClientID)
	}
	if creds.RedirectURI != "http://localhost:56124/callback" {
		t.Errorf("redirect uri = %q, want localhost loopback", creds.RedirectURI)
	}
	wantScopes := "org:create_api_key user:profile user:inference"
	if creds.Scopes != wantScopes {
		t.Errorf("scopes = %q\nwant       %q", creds.Scopes, wantScopes)
	}
	if !creds.PKCE {
		t.Error("claude should use PKCE")
	}
	if !creds.RefreshJSON {
		t.Error("claude refresh should be JSON")
	}
	if creds.ExtraAuthParams["code"] != "true" {
		t.Errorf("extra params: %+v", creds.ExtraAuthParams)
	}
	if creds.AuthURL != "https://claude.ai/oauth/authorize" {
		t.Errorf("auth url = %q, want claude.ai consent endpoint", creds.AuthURL)
	}
	if creds.TokenURL != "https://api.anthropic.com/v1/oauth/token" {
		t.Errorf("token url = %q, want api.anthropic.com", creds.TokenURL)
	}
	if recipe, ok := recipeByID("claude"); !ok || recipe.Protocol != domain.ProtocolClaudeCode || recipe.Kind != kindInteractiveOAuth {
		t.Fatalf("recipe missing or wrong: %+v", recipe)
	}
	if !providerEditInteractiveOAuth(domain.ProtocolClaudeCode) {
		t.Error("claude-code should use the interactive OAuth edit row")
	}
}

func TestOAuthPresetCreatesAntigravityConfig(t *testing.T) {
	form := url.Values{}
	form.Set("preset", "antigravity")
	creds, err := credsFromConnectForm(reqWithForm(form))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.AntigravityAuth {
		t.Error("antigravity preset should set AntigravityAuth")
	}
	if creds.ClientID == "" || creds.ClientSecret == "" {
		t.Fatalf("client credentials missing: %+v", creds)
	}
	if creds.PKCE {
		t.Error("antigravity should not use PKCE")
	}
	if creds.ExtraAuthParams["access_type"] != "offline" {
		t.Fatalf("extra params: %+v", creds.ExtraAuthParams)
	}
	if recipe, ok := recipeByID("antigravity"); !ok || recipe.Protocol != domain.ProtocolAntigravity || recipe.Kind != kindInteractiveOAuth {
		t.Fatalf("recipe missing or wrong: %+v", recipe)
	}
}

func TestOAuthPresetCreatesCursorConfig(t *testing.T) {
	form := url.Values{}
	form.Set("preset", "cursor")
	creds, err := credsFromConnectForm(reqWithForm(form))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.CursorAuth {
		t.Error("cursor preset should set CursorAuth")
	}
	if creds.Preset != "cursor" {
		t.Errorf("preset = %q", creds.Preset)
	}
	if recipe, ok := recipeByID("cursor"); !ok || recipe.Protocol != domain.ProtocolCursor || recipe.Kind != kindCursor {
		t.Fatalf("recipe missing or wrong: %+v", recipe)
	}
}

func TestCreateCursorProviderPersistsCreds(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "my-cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("access_token", "ide-tok")
	form.Set("machine_id", "m-uuid")
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	p := providers[0]
	if p.Protocol != domain.ProtocolCursor || p.Method() != domain.AuthOAuth {
		t.Errorf("protocol/method = %q/%q", p.Protocol, p.Method())
	}
	if p.OAuthCreds == nil || !p.OAuthCreds.CursorAuth {
		t.Fatalf("creds missing CursorAuth: %+v", p.OAuthCreds)
	}
	if p.OAuthCreds.AccessToken != "ide-tok" {
		t.Errorf("access token = %q", p.OAuthCreds.AccessToken)
	}
	if p.OAuthCreds.MachineID != "m-uuid" {
		t.Errorf("machine id = %q", p.OAuthCreds.MachineID)
	}
}

func TestCreateCursorProviderMissingMachineIDRejected(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "my-cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("access_token", "ide-tok")
	// machine_id intentionally omitted
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	body := rec.Body.String()
	if !strings.Contains(body, "machine id") {
		t.Fatalf("want machine id error, got: %s", body)
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 0 {
		t.Errorf("provider should not be saved, got %d", len(providers))
	}
}

func TestCreateCursorProviderWithRefreshToken(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "my-cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("access_token", "cli-tok")
	form.Set("refresh_token", "cli-rt")
	form.Set("expires_at", "1782522851")
	form.Set("machine_id", "m-uuid")
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	c := providers[0].OAuthCreds
	if c.RefreshToken != "cli-rt" || c.ExpiresAt != 1782522851 || c.MachineID != "m-uuid" {
		t.Fatalf("creds = %+v", c)
	}
}

func TestCreateCursorProviderDerivesJWTExpiry(t *testing.T) {
	h := testHandler(t)
	exp := int64(1_782_522_851)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1782522851}`))
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "my-cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("access_token", header+"."+payload+".")
	form.Set("refresh_token", "cli-rt")
	form.Set("machine_id", "m-uuid")
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 1 || providers[0].OAuthCreds.ExpiresAt != exp {
		t.Fatalf("providers = %+v", providers)
	}
}

func TestCursorBeginStoresSessionAndRendersLogin(t *testing.T) {
	restore := stubCursorPoll(t)
	defer restore()
	h := testHandler(t)
	form := url.Values{}
	form.Set("preset", "cursor")
	rec := httptest.NewRecorder()
	h.cursorConnectBegin(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Open authorization page") {
		t.Fatalf("missing login link: %s", body)
	}
	if !strings.Contains(body, "waiting for authorization") {
		t.Fatalf("status poll should be enabled: %s", body)
	}
	state := parseState(t, body)
	sess, ok := h.sessions.get(state)
	if !ok {
		t.Fatal("session not stored")
	}
	_ = sess.conn.Close()
}

func TestCreateCursorFromConnectSession(t *testing.T) {
	h := testHandler(t)
	conn := &stubCursorConn{
		state: "cursor-state-1",
		creds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, Preset: "cursor", CursorAuth: true,
			AccessToken: "sess-tok", RefreshToken: "sess-rt", MachineID: "gen-mid",
		},
	}
	h.sessions.put(conn.state, &connectSession{conn: conn, created: time.Now()}, time.Now())

	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "cursor-web")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("oauth_session", conn.state)
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 1 {
		t.Fatalf("providers = %d", len(providers))
	}
	c := providers[0].OAuthCreds
	if c.AccessToken != "sess-tok" || c.MachineID != "gen-mid" || !c.CursorAuth {
		t.Fatalf("creds = %+v", c)
	}
	if _, ok := h.sessions.get(conn.state); ok {
		t.Error("session not dropped")
	}
}

func TestUpdateCursorReconnectPreservesMachineID(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "cursor", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true, AccessToken: "old-tok", MachineID: "keep-mid",
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	conn := &stubCursorConn{
		state: "cursor-reconn",
		creds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, Preset: "cursor", CursorAuth: true,
			AccessToken: "new-tok", RefreshToken: "new-rt", MachineID: "keep-mid",
		},
	}
	h.sessions.put(conn.state, &connectSession{conn: conn, created: time.Now()}, time.Now())

	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("oauth_session", conn.state)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.AccessToken != "new-tok" || got.OAuthCreds.MachineID != "keep-mid" {
		t.Fatalf("creds = %+v", got.OAuthCreds)
	}
}

func TestUpdateCursorManualPasteKeepsMachineID(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "cursor", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true, AccessToken: "old-tok", MachineID: "keep-mid",
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("access_token", "pasted-tok")
	form.Set("refresh_token", "pasted-rt")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.AccessToken != "pasted-tok" || got.OAuthCreds.RefreshToken != "pasted-rt" {
		t.Fatalf("tokens = %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.MachineID != "keep-mid" {
		t.Fatalf("machine id = %q, want keep-mid", got.OAuthCreds.MachineID)
	}
}

func TestUpdateCursorManualRefreshTokenKeepsAccessAndMachineID(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "cursor", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true, AccessToken: "old-tok", MachineID: "keep-mid",
			ExpiresAt: 12345, Email: "user@example.com", AccountID: "account-1",
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("refresh_token", "pasted-rt")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.AccessToken != "old-tok" || got.OAuthCreds.RefreshToken != "pasted-rt" {
		t.Fatalf("tokens = %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.MachineID != "keep-mid" {
		t.Fatalf("machine id = %q, want keep-mid", got.OAuthCreds.MachineID)
	}
	if got.OAuthCreds.ExpiresAt != 12345 || got.OAuthCreds.Email != "user@example.com" || got.OAuthCreds.AccountID != "account-1" {
		t.Fatalf("metadata = %+v", got.OAuthCreds)
	}
}

func TestCursorOAuthStatusPollsPendingSession(t *testing.T) {
	restore := stubCursorPoll(t)
	defer restore()
	h := testHandler(t)
	form := url.Values{}
	form.Set("preset", "cursor")
	begin := httptest.NewRecorder()
	h.cursorConnectBegin(begin, reqWithForm(form))
	state := parseState(t, begin.Body.String())
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/oauth/status?state="+state, nil)
	rec := httptest.NewRecorder()
	h.oauthConnectStatus(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "waiting for authorization") {
		t.Fatalf("pending status = %s", body)
	}
	if sess, ok := h.sessions.get(state); ok {
		_ = sess.conn.Close()
	}
}

func TestCursorBeginUsesExistingMachineID(t *testing.T) {
	restore := stubCursorPoll(t)
	defer restore()
	h := testHandler(t)
	p := &domain.Provider{
		Name: "cursor", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{CursorAuth: true, AccessToken: "tok", MachineID: "existing-mid"},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("preset", "cursor")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.cursorConnectBegin(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, body = %s", rec.Code, rec.Body.String())
	}
	state := parseState(t, rec.Body.String())
	sess, ok := h.sessions.get(state)
	if !ok {
		t.Fatal("session missing")
	}
	defer sess.conn.Close()
	cc, ok := sess.conn.(*oauth.CursorConnect)
	if !ok {
		t.Fatalf("conn type %T", sess.conn)
	}
	if cc.MachineID() != "existing-mid" {
		t.Fatalf("machine id = %q, want existing-mid", cc.MachineID())
	}
}

func stubCursorPoll(t *testing.T) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return oauth.OverrideCursorURLs(srv.URL+"/loginDeepControl", srv.URL+"/auth/poll", "")
}

type stubCursorConn struct {
	state string
	creds *domain.OAuthCreds
}

func (s *stubCursorConn) State() string { return s.state }
func (s *stubCursorConn) Result() (*domain.OAuthCreds, error, bool) {
	return s.creds, nil, true
}
func (s *stubCursorConn) Close() error { return nil }

func TestCreateCursorProviderMissingAccessTokenRejected(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "my-cursor")
	form.Set("protocol", "cursor")
	form.Set("preset", "cursor")
	form.Set("machine_id", "m-uuid")
	// access_token intentionally omitted: the generic paste-token guard rejects
	// the save before the Cursor-specific check runs.
	rec := httptest.NewRecorder()
	h.createProvider(rec, reqWithForm(form))
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "paste") {
		t.Fatalf("want a paste-token rejection, got: %s", body)
	}
	providers, _ := h.store.ListProviders(context.Background())
	if len(providers) != 0 {
		t.Errorf("provider should not be saved, got %d", len(providers))
	}
}

func TestKiroConfigSurvivesAPIKeyEditAndOptOutFalse(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "kiro-key", BaseURL: "https://stored.example", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthBearer, APIKey: "stored-key",
		OAuthCreds: &domain.OAuthCreds{
			ProfileArn: "arn:stored", Region: "eu-central-1", KiroIDP: "Google",
			KiroTransport: "runtime", KiroDiscovery: "management", KiroAgentMode: "spec",
			KiroContentOptOut: true,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"auth_method": {"apikey"}, "name": {"kiro-key"}, "protocol": {"kiro"},
		"base_url": {"https://stored.example"},
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil || got.OAuthCreds == nil || got.OAuthCreds.KiroTransport != "runtime" || !got.OAuthCreds.KiroContentOptOut || got.OAuthCreds.ProfileArn != "arn:stored" {
		t.Fatalf("omitted config lost: %+v %v", got.OAuthCreds, err)
	}
	form.Set("kiro_content_opt_out", "false")
	form.Set("kiro_transport", "not-a-transport")
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	got, err = h.store.GetProvider(context.Background(), p.ID)
	if err != nil || got.OAuthCreds.KiroContentOptOut || got.OAuthCreds.KiroTransport != "" || got.OAuthCreds.KiroDiscovery != "management" {
		t.Fatalf("submitted false/unknown not applied: %+v %v", got.OAuthCreds, err)
	}
}

func TestKiroReconnectKeepsTransportButNotOldProfile(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "kiro", BaseURL: "https://codewhisperer.us-east-1.amazonaws.com", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "builder-id", KiroIDP: "BuilderId", ProfileArn: "arn:old-account",
			AccessToken: "old", RefreshToken: "old-refresh", KiroTransport: "runtime",
			KiroContentOptOut: true,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	conn := &stubCursorConn{state: "kiro-reconn", creds: &domain.OAuthCreds{
		KiroAuth: "idc", KiroIDP: "AWSIdC", ProfileArn: "arn:new-account",
		AccessToken: "new", RefreshToken: "new-refresh",
	}}
	h.sessions.put(conn.state, &connectSession{conn: conn, created: time.Now()}, time.Now())
	form := url.Values{
		"auth_method": {"oauth"}, "name": {"kiro"}, "protocol": {"kiro"},
		"preset": {"kiro"}, "oauth_session": {conn.state},
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.ProfileArn != "arn:new-account" || got.OAuthCreds.KiroIDP != "AWSIdC" || got.OAuthCreds.AccessToken != "new" {
		t.Fatalf("new account did not win: %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.KiroTransport != "runtime" || !got.OAuthCreds.KiroContentOptOut {
		t.Fatalf("connection config lost: %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.RefreshToken == "old-refresh" {
		t.Fatal("old credential copied")
	}
}

func TestKiroReconnectStaleFormDoesNotOverwriteNewAccount(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "kiro", BaseURL: "https://codewhisperer.us-east-1.amazonaws.com", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "builder-id", KiroIDP: "BuilderId", ProfileArn: "arn:old-account",
			Region: "us-east-1", AccessToken: "old", RefreshToken: "old-refresh",
			KiroTransport: "runtime", KiroDiscovery: "management", KiroAgentMode: "spec",
			KiroContentOptOut: true,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	conn := &stubCursorConn{state: "kiro-stale", creds: &domain.OAuthCreds{
		KiroAuth: "idc", AccessToken: "new", RefreshToken: "new-refresh",
	}}
	h.sessions.put(conn.state, &connectSession{conn: conn, created: time.Now()}, time.Now())
	form := url.Values{
		"auth_method": {"oauth"}, "name": {"kiro"}, "protocol": {"kiro"},
		"preset": {"kiro"}, "oauth_session": {conn.state},
		"profile_arn": {"arn:old-account"}, "region": {"us-east-1"},
		"kiro_auth": {"builder-id"},
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.ProfileArn != "" || got.OAuthCreds.KiroIDP != "" || got.OAuthCreds.Region != "" || got.OAuthCreds.KiroAuth != "idc" {
		t.Fatalf("stale account fields won: %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.AccessToken != "new" || got.OAuthCreds.RefreshToken != "new-refresh" {
		t.Fatalf("new credentials lost: %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.KiroTransport != "runtime" || got.OAuthCreds.KiroDiscovery != "management" || got.OAuthCreds.KiroAgentMode != "spec" || !got.OAuthCreds.KiroContentOptOut {
		t.Fatalf("omitted connection preferences lost: %+v", got.OAuthCreds)
	}
}

func TestKiroExplicitConfigResetClearsStoredFields(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "kiro", BaseURL: "https://stored.example", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "idc", KiroIDP: "AWSIdC", ProfileArn: "arn:same", Region: "eu-central-1",
			AccessToken: "tok", RefreshToken: "refresh", KiroTransport: "runtime",
			KiroDiscovery: "management", KiroAgentMode: "spec", KiroContentOptOut: true,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"auth_method": {"oauth"}, "name": {"kiro"}, "protocol": {"kiro"},
		"preset": {"kiro"}, "kiro_auth": {"idc"}, "profile_arn": {"arn:same"},
		"region":         {"eu-central-1"},
		"kiro_transport": {"codewhisperer"}, "kiro_discovery": {"legacy"},
		"kiro_idp": {""}, "kiro_agent_mode": {""}, "kiro_content_opt_out": {"false"},
	}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/"+strconv.FormatInt(p.ID, 10), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.KiroTransport != "" || got.OAuthCreds.KiroDiscovery != "" || got.OAuthCreds.KiroIDP != "" || got.OAuthCreds.KiroAgentMode != "" || got.OAuthCreds.KiroContentOptOut {
		t.Fatalf("explicit reset ignored: %+v", got.OAuthCreds)
	}
	if got.OAuthCreds.ProfileArn != "arn:same" || got.OAuthCreds.KiroAuth != "idc" || got.OAuthCreds.AccessToken != "tok" {
		t.Fatalf("same-account identity lost: %+v", got.OAuthCreds)
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
func TestOAuthRefreshCursorUsesExchange(t *testing.T) {
	h := testHandler(t)
	var sawAuth, sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"accessToken": "cursor-fresh",
		})
	}))
	t.Cleanup(srv.Close)
	restore := oauth.OverrideCursorURLs("", "", srv.URL)
	defer restore()

	p := &domain.Provider{
		Name: "cursor", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true, AccessToken: "old", RefreshToken: "rt-cli", MachineID: "mid",
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("preset", "cursor")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	form.Set("refresh_view", "status")
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))
	if sawAuth != "Bearer rt-cli" || sawBody != "{}" {
		t.Fatalf("exchange saw auth=%q body=%q", sawAuth, sawBody)
	}
	if !strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("result = %s", rec.Body.String())
	}
	got, _ := h.store.GetProvider(context.Background(), p.ID)
	if got.OAuthCreds.AccessToken != "cursor-fresh" || got.OAuthCreds.MachineID != "mid" {
		t.Fatalf("persisted = %+v", got.OAuthCreds)
	}
}

func TestOAuthRefreshCursorSessionJWTDoesNotExchange(t *testing.T) {
	h := testHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("session JWT must not hit exchange")
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	restore := oauth.OverrideCursorURLs("", "", srv.URL)
	defer restore()

	exp := time.Now().Add(time.Hour).Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://authentication.cursor.sh","exp":` + strconv.FormatInt(exp, 10) + `}`))
	sess := header + "." + payload + "."
	p := &domain.Provider{
		Name: "cursor", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			CursorAuth: true, AccessToken: sess, RefreshToken: sess, MachineID: "mid", ExpiresAt: exp,
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("preset", "cursor")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	form.Set("refresh_view", "status")
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))
	body := rec.Body.String()
	if !strings.Contains(body, "cannot be rotated") {
		t.Fatalf("result = %s", body)
	}
	if strings.Contains(body, "re-authorize") || strings.Contains(body, "reconnect") {
		t.Fatalf("must not ask to reconnect: %s", body)
	}
}

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

// TestRefreshAllOAuth: refresh-all force-refreshes every saved oauth provider,
// skips apikey providers, persists the rotated tokens, and reports the count.
func TestRefreshAllOAuth(t *testing.T) {
	h := testHandler(t)
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "rotated",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(srv.Close)

	for _, name := range []string{"grok-a", "grok-b"} {
		p := &domain.Provider{
			Name: name, BaseURL: "https://api.x.ai/v1", Protocol: domain.ProtocolOpenAI,
			AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
			OAuthCreds: &domain.OAuthCreds{
				Mode: domain.OAuthManual, AccessToken: "stale", RefreshToken: "rt-" + name,
				TokenURL: srv.URL + "/token", ClientID: "cid",
			},
		}
		if err := h.store.CreateProvider(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	// An apikey provider must be left untouched.
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "plain", BaseURL: "https://api.example.com/v1", Protocol: domain.ProtocolOpenAI,
		APIKey: "sk-x", AuthScheme: domain.AuthBearer,
	}); err != nil {
		t.Fatal(err)
	}
	// A Qoder device-token provider cannot be refreshed; bulk refresh must skip
	// it (no token-endpoint hit) rather than report a false reconnect-required.
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "qoder", BaseURL: "https://api3.qoder.sh", Protocol: domain.ProtocolQoder,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthManual, QoderAuth: true,
			AccessToken: "device-tok", RefreshToken: "ignored",
			TokenURL: srv.URL + "/should-not-hit", ClientID: "cid",
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Cursor browser session JWTs cannot rotate and must be skipped.
	exp := time.Now().Add(time.Hour).Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://authentication.cursor.sh","exp":` + strconv.FormatInt(exp, 10) + `}`))
	sess := header + "." + payload + "."
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "cursor-session", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, CursorAuth: true,
			AccessToken: sess, RefreshToken: sess, MachineID: "m2", ExpiresAt: exp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Access-only Cursor imports remain non-refreshable and must be skipped.
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "cursor-import", BaseURL: "https://api2.cursor.sh", Protocol: domain.ProtocolCursor,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthManual, CursorAuth: true,
			AccessToken: "ide-tok", MachineID: "m1",
		},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/oauth/refresh-all", nil)
	rec := httptest.NewRecorder()
	h.refreshAllOAuth(rec, req)

	if *hits != 2 {
		t.Errorf("token endpoint hits = %d, want 2", *hits)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "refreshed 2 oauth provider(s)") {
		t.Fatalf("result = %s, want refreshed 2", body)
	}
	if !strings.Contains(body, "skipped 3 (non-refreshable)") {
		t.Errorf("result = %s, want skipped 3", body)
	}
	if strings.Contains(body, "qoder: reconnect required") || strings.Contains(body, "failed") {
		t.Errorf("result = %s, qoder must not be failed", body)
	}

	providers, _ := h.store.ListProviders(context.Background())
	for _, p := range providers {
		switch p.Method() {
		case domain.AuthOAuth:
			if p.OAuthCreds.QoderAuth {
				if p.OAuthCreds.AccessToken != "device-tok" {
					t.Errorf("qoder access token = %q, want unchanged device-tok", p.OAuthCreds.AccessToken)
				}
				continue
			}
			if p.OAuthCreds.CursorAuth {
				if p.Name == "cursor-import" && p.OAuthCreds.AccessToken != "ide-tok" {
					t.Errorf("cursor access token = %q, want unchanged ide-tok", p.OAuthCreds.AccessToken)
				}
				if p.Name == "cursor-session" && p.OAuthCreds.AccessToken != sess {
					t.Errorf("cursor session access token mutated")
				}
				continue
			}
			if p.OAuthCreds.AccessToken != "rotated" {
				t.Errorf("%s access token = %q, want rotated", p.Name, p.OAuthCreds.AccessToken)
			}
		default:
			if p.APIKey != "sk-x" {
				t.Errorf("apikey provider changed: %q", p.APIKey)
			}
		}
	}
}

type fakeConnector struct {
	state string
	creds *domain.OAuthCreds
}

func (f fakeConnector) State() string { return f.state }
func (f fakeConnector) Result() (*domain.OAuthCreds, error, bool) {
	return f.creds, nil, true
}
func (f fakeConnector) Close() error { return nil }

func putKiroSession(h *Handler, creds *domain.OAuthCreds) string {
	const state = "kiro-session"
	h.sessions.put(state, &connectSession{conn: fakeConnector{state: state, creds: creds}, created: time.Now()}, time.Now())
	return state
}

func TestKiroCheckFormCredentialSources(t *testing.T) {
	h := testHandler(t)
	type captured struct {
		auth      string
		apiKey    string
		tokenType string
		body      string
		url       string
	}
	var got captured
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		got = captured{
			auth: req.Header.Get("Authorization"), apiKey: req.Header.Get("x-api-key"),
			tokenType: req.Header.Get("tokentype"), body: string(raw), url: req.URL.String(),
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"Live-Model"}]}`), nil
	})

	t.Run("stored api key uses stored config", func(t *testing.T) {
		stored := &domain.OAuthCreds{ProfileArn: "arn:stored", Region: "eu-central-1", KiroAuth: "idc"}
		p := &domain.Provider{
			Name: "kiro-key", BaseURL: "https://stored.example", Protocol: domain.ProtocolKiro,
			AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthXAPIKey, APIKey: "stored-key",
			OAuthCreds: stored,
		}
		if err := h.store.CreateProvider(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		form := url.Values{
			"protocol": {"kiro"}, "auth_method": {"apikey"}, "auth_scheme": {"x-api-key"},
			"base_url": {"https://form.example/root"}, "id": {strconv.FormatInt(p.ID, 10)},
		}
		rec := httptest.NewRecorder()
		h.checkProvider(rec, reqWithForm(form))
		if !strings.Contains(rec.Body.String(), "1 models") || got.apiKey != "stored-key" || got.tokenType != "API_KEY" {
			t.Fatalf("result=%s captured=%+v", rec.Body.String(), got)
		}
		if got.url != "https://form.example/root/" || !strings.Contains(got.body, "arn:stored") || strings.Contains(got.body, "eu-central-1") {
			t.Fatalf("request url=%s body=%s", got.url, got.body)
		}
		saved, err := h.store.GetProvider(context.Background(), p.ID)
		if err != nil || saved.OAuthCreds.ProfileArn != "arn:stored" || saved.APIKey != "stored-key" {
			t.Fatalf("stored provider mutated: %+v %v", saved, err)
		}
	})

	t.Run("new api key uses form config", func(t *testing.T) {
		form := url.Values{
			"protocol": {"kiro"}, "auth_method": {"apikey"}, "auth_scheme": {"bearer"},
			"base_url": {"https://new.example"}, "api_key": {"typed-key"},
			"profile_arn": {"arn:form"}, "region": {"us-west-2"}, "kiro_auth": {"social"},
		}
		rec := httptest.NewRecorder()
		h.checkProvider(rec, reqWithForm(form))
		if got.auth != "Bearer typed-key" || got.tokenType != "API_KEY" || !strings.Contains(got.body, "arn:form") {
			t.Fatalf("captured=%+v result=%s", got, rec.Body.String())
		}
		if strings.Contains(got.url, "us-west-2") || strings.Contains(got.body, "us-west-2") {
			t.Fatalf("region leaked into request: %+v", got)
		}
	})

	t.Run("session identity wins over stale form", func(t *testing.T) {
		sessionCreds := &domain.OAuthCreds{
			AccessToken: "session-token", RefreshToken: "session-refresh", KiroAuth: "idc",
			ProfileArn: "arn:session", Region: "ap-southeast-2", ClientID: "cid", ClientSecret: "secret",
		}
		state := putKiroSession(h, sessionCreds)
		form := url.Values{
			"protocol": {"kiro"}, "auth_method": {"oauth"}, "base_url": {"https://session.example"},
			"oauth_session": {state}, "profile_arn": {"arn:overlay"},
		}
		rec := httptest.NewRecorder()
		h.checkProvider(rec, reqWithForm(form))
		if got.auth != "Bearer session-token" || got.tokenType != "SSO_OIDC" || !strings.Contains(got.body, "arn:session") || strings.Contains(got.body, "arn:overlay") {
			t.Fatalf("captured=%+v result=%s", got, rec.Body.String())
		}
		if sessionCreds.ProfileArn != "arn:session" || sessionCreds.Region != "ap-southeast-2" {
			t.Fatalf("session creds mutated: %+v", sessionCreds)
		}
	})

	t.Run("manual tokens are not refreshed", func(t *testing.T) {
		form := url.Values{
			"protocol": {"kiro"}, "auth_method": {"oauth"}, "base_url": {"https://manual.example"},
			"preset": {"kiro"}, "kiro_auth": {"Builder-ID"}, "access_token": {"pasted-token"},
			"refresh_token": {"pasted-refresh"}, "client_id": {"cid"}, "client_secret": {"secret"},
			"profile_arn": {"arn:manual-kept"}, "region": {"eu-west-1"},
		}
		rec := httptest.NewRecorder()
		h.checkProvider(rec, reqWithForm(form))
		if got.auth != "Bearer pasted-token" || got.url != "https://manual.example/" || !strings.Contains(got.body, "arn:manual-kept") {
			t.Fatalf("captured=%+v result=%s", got, rec.Body.String())
		}
	})
}

func TestKiroCheckSavedOAuthRefreshOnce(t *testing.T) {
	h := testHandler(t)
	var tokenHits int
	var tokenBody string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		raw, _ := io.ReadAll(r.Body)
		tokenBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"rotated-token","refreshToken":"rotated-refresh","expiresIn":3600}`))
	}))
	t.Cleanup(tokenSrv.Close)
	withKiroTokenTransport(t, tokenSrv)

	var auth []string
	var bodies []string
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		auth = append(auth, req.Header.Get("Authorization"))
		bodies = append(bodies, string(raw))
		if len(auth) == 1 {
			return kiroJSONResponse(http.StatusUnauthorized, `{}`), nil
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"after-refresh"}]}`), nil
	})
	p := &domain.Provider{
		Name: "saved-kiro", BaseURL: "https://saved.example", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "idc", AccessToken: "stored-token", RefreshToken: "stored-refresh",
			ClientID: "cid", ClientSecret: "secret", ProfileArn: "arn:saved", Region: "us-east-1",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"protocol": {"kiro"}, "auth_method": {"oauth"}, "base_url": {"https://saved-form.example"},
		"id": {strconv.FormatInt(p.ID, 10)}, "profile_arn": {"arn:form"},
	}
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if tokenHits != 1 || !strings.Contains(tokenBody, "stored-refresh") {
		t.Fatalf("token hits=%d body=%s", tokenHits, tokenBody)
	}
	if strings.Join(auth, ",") != "Bearer stored-token,Bearer rotated-token" || !strings.Contains(rec.Body.String(), "1 models") {
		t.Fatalf("auth=%v result=%s", auth, rec.Body.String())
	}
	for _, body := range bodies {
		if !strings.Contains(body, "arn:form") || strings.Contains(body, "us-east-1") {
			t.Fatalf("catalog body=%s", body)
		}
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil || saved.OAuthCreds.AccessToken != "rotated-token" || saved.OAuthCreds.ProfileArn != "arn:saved" {
		t.Fatalf("persisted creds=%+v err=%v", saved.OAuthCreds, err)
	}
}

func TestKiroCheckSavedOAuthRefreshFailure(t *testing.T) {
	h := testHandler(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(tokenSrv.Close)
	withKiroTokenTransport(t, tokenSrv)
	hits := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		hits++
		return kiroJSONResponse(http.StatusForbidden, `{}`), nil
	})
	p := &domain.Provider{
		Name: "saved-kiro-fail", BaseURL: "https://saved.example", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			KiroAuth: "builder-id", AccessToken: "stored-token", RefreshToken: "stored-refresh",
			ClientID: "cid", ClientSecret: "secret", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"protocol": {"kiro"}, "auth_method": {"oauth"}, "base_url": {"https://saved-form.example"},
		"id": {strconv.FormatInt(p.ID, 10)},
	}
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if hits != 1 || !strings.Contains(rec.Body.String(), "reconnect required") {
		t.Fatalf("hits=%d result=%s", hits, rec.Body.String())
	}
	saved, _ := h.store.GetProvider(context.Background(), p.ID)
	if saved.OAuthCreds.AccessToken != "stored-token" {
		t.Fatalf("token changed after failed refresh: %+v", saved.OAuthCreds)
	}
}

func TestKiroModelsFailureKeepsManualEntry(t *testing.T) {
	h := testHandler(t)
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		return kiroJSONResponse(http.StatusUnauthorized, `{}`), nil
	})
	p := &domain.Provider{
		Name: "kiro-models", BaseURL: "https://models.example", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey, APIKey: "stored-key",
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id="+strconv.FormatInt(p.ID, 10), nil)
	rec := httptest.NewRecorder()
	h.providerModels(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "enter id manually") || strings.Contains(body, "claude-sonnet") {
		t.Fatalf("model options=%s", body)
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
