package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/store"
)

func TestGrokBuildAddAndMetadataEdit(t *testing.T) {
	h := testHandler(t)
	creds := grokBuildTestCreds()
	conn := &grokBuildStub{state: "gb-create", creds: creds}
	h.sessions.putBound(conn.State(), conn, grokBuildFlow, 0, time.Now())
	form := url.Values{}
	form.Set("name", "build")
	form.Set("protocol", "grok-build")
	form.Set("auth_method", "oauth")
	form.Set("preset", "grok-build")
	form.Set("oauth_session", conn.State())
	form.Set("base_url", "https://api.x.ai/v1")
	form.Set("api_key", "should-not-store")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status %d body %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.BaseURL != grokbuild.BaseURL || saved.APIKey != "" || saved.Protocol != domain.ProtocolGrokBuild {
		t.Fatalf("saved host/key/protocol = %s %q %s", saved.BaseURL, saved.APIKey, saved.Protocol)
	}
	if !saved.OAuthCreds.GrokBuildAuth || saved.OAuthCreds.RefreshToken != "refresh-1" {
		t.Fatalf("creds = %+v", saved.OAuthCreds)
	}

	edit := url.Values{}
	edit.Set("name", "build renamed")
	edit.Set("protocol", "grok-build")
	edit.Set("auth_method", "oauth")
	edit.Set("preset", "grok-build")
	edit.Set("tags", "team")
	edit.Set("base_url", "https://evil.example/v1")
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/1", strings.NewReader(edit.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status %d body %s", rec.Code, rec.Body.String())
	}
	saved, err = h.store.GetProvider(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "build renamed" || saved.BaseURL != grokbuild.BaseURL || saved.OAuthCreds.RefreshToken != "refresh-1" {
		t.Fatalf("metadata edit changed connection: %+v creds=%+v", saved, saved.OAuthCreds)
	}
	if len(saved.Tags) != 1 || saved.Tags[0] != "team" {
		t.Fatalf("tags = %#v", saved.Tags)
	}
}

func TestGrokBuildRejectsXAISessionAndWrongTarget(t *testing.T) {
	h := testHandler(t)
	xai := &grokBuildStub{state: "xai", creds: &domain.OAuthCreds{Preset: "xai", AccessToken: "x", RefreshToken: "r", ClientID: grokbuild.ClientID}}
	h.sessions.put(xai.State(), &connectSession{conn: xai, created: time.Now()}, time.Now())
	form := url.Values{}
	form.Set("name", "stolen")
	form.Set("protocol", "grok-build")
	form.Set("auth_method", "oauth")
	form.Set("preset", "grok-build")
	form.Set("oauth_session", xai.State())
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("xAI session status %d", rec.Code)
	}
	if n, _ := h.store.ListProviders(context.Background()); len(n) != 0 {
		t.Fatal("xAI session created a provider")
	}

	good := grokBuildTestCreds()
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: good,
	}); err != nil {
		t.Fatal(err)
	}
	other := &grokBuildStub{state: "other", creds: grokBuildTestCreds()}
	h.sessions.putBound(other.State(), other, grokBuildFlow, 99, time.Now())
	edit := url.Values{}
	edit.Set("name", "build")
	edit.Set("protocol", "grok-build")
	edit.Set("auth_method", "oauth")
	edit.Set("preset", "grok-build")
	edit.Set("oauth_session", other.State())
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/1", strings.NewReader(edit.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-target status %d", rec.Code)
	}
	saved, err := h.store.GetProvider(context.Background(), 1)
	if err != nil || saved.OAuthCreds.AccessToken != "access-1" {
		t.Fatalf("cross-target rewrote credentials: %+v %v", saved, err)
	}
}

func TestXAIRecipeUnchanged(t *testing.T) {
	r, ok := recipeByID("xai")
	if !ok || r.Protocol != domain.ProtocolOpenAI || r.Preset != "xai" || r.BaseURL != "https://api.x.ai/v1" {
		t.Fatalf("xai recipe changed: %+v", r)
	}
	gb, ok := recipeByID("grok-build")
	if !ok || gb.Protocol != domain.ProtocolGrokBuild || gb.BaseURL != grokbuild.BaseURL || gb.Preset != grokbuild.Preset {
		t.Fatalf("grok-build recipe = %+v", gb)
	}
	html := renderComponent(t, ProviderRecipeForm(gb))
	if strings.Contains(html, "api.x.ai") {
		t.Fatalf("form offers the public host: %s", html)
	}
	for _, want := range []string{"/dashboard/providers/grok-build/device/begin", `name="grok_build_mode"`, "Device sign-in", "Paste tokens", `name="access_token"`, `name="refresh_token"`, `name="id_token"`, `name="expires_at"`, "data-oauth-lock"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q: %s", want, html)
		}
	}
}

func TestGrokBuildPresetCannotUseGenericOAuth(t *testing.T) {
	h := testHandler(t)
	form := url.Values{}
	form.Set("name", "forged")
	form.Set("protocol", "openai")
	form.Set("auth_method", "oauth")
	form.Set("preset", "grok-build")
	form.Set("base_url", "https://api.x.ai/v1")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	if n, _ := h.store.ListProviders(context.Background()); len(n) != 0 {
		t.Fatal("forged preset created a provider")
	}
}

func TestGrokBuildImportRejectsPublicHost(t *testing.T) {
	h := testHandler(t)
	raw := `{
		"version":1,
		"providers":[{
			"name":"bad","base_url":"https://api.x.ai/v1","protocol":"grok-build","auth_method":"oauth","auth_scheme":"bearer",
			"oauth":{"grok_build_auth":true,"access_token":"a","refresh_token":"r","client_id":"` + grokbuild.ClientID + `","preset":"grok-build"}
		}]
	}`
	sum, err := h.store.Import(context.Background(), strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failures) != 1 || sum.ProvidersCreated != 0 {
		t.Fatalf("import = %+v", sum)
	}
	marked := `{
		"version":1,
		"providers":[{
			"name":"marked","base_url":"https://api.x.ai/v1","protocol":"openai","auth_method":"oauth","auth_scheme":"bearer",
			"oauth":{"grok_build_auth":true,"access_token":"a","refresh_token":"r","preset":"grok-build"}
		}]
	}`
	sum, err = h.store.Import(context.Background(), strings.NewReader(marked))
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Failures) != 1 || sum.ProvidersCreated != 0 {
		t.Fatalf("marked import = %+v", sum)
	}
}

func TestGrokBuildDeviceViewDoesNotEchoDeviceCode(t *testing.T) {
	h := testHandler(t)
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		return grokJSON(http.StatusOK, map[string]any{
			"device_code": "device-secret-value", "user_code": "ABCD-EFGH",
			"verification_uri": "https://auth.x.ai/oauth/device", "expires_in": 600, "interval": 5,
		}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/grok-build/device/begin", strings.NewReader("preset=grok-build"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.grokBuildDeviceBegin(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "ABCD-EFGH") || strings.Contains(body, "device-secret-value") {
		t.Fatalf("status %d body %s", rec.Code, body)
	}
	if !strings.Contains(body, "https://auth.x.ai/oauth/device") {
		t.Fatalf("missing verification link: %s", body)
	}
}

func TestGrokBuildSessionDoesNotReachGenericCheckOrSave(t *testing.T) {
	h := testHandler(t)
	foreign := int32(0)
	previous := upstreamClient
	upstreamClient = &http.Client{Transport: grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&foreign, 1)
		t.Errorf("generic check sent to %s", r.URL.String())
		return grokJSON(http.StatusOK, map[string]any{"data": []any{}}), nil
	})}
	t.Cleanup(func() { upstreamClient = previous })
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		if !grokbuild.IsFixedInferenceURL(r.URL.String()) {
			atomic.AddInt32(&foreign, 1)
			t.Errorf("subscription token left the official host: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "" && got != "Bearer access-1" {
			t.Errorf("unexpected token %s", got)
		}
		return grokJSON(http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "grok-build"}}}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	creds := grokBuildTestCreds()
	conn := &grokBuildStub{state: "gb-session", creds: creds}
	h.sessions.putBound(conn.State(), conn, grokBuildFlow, 0, time.Now())

	generic := url.Values{}
	generic.Set("protocol", "openai")
	generic.Set("auth_method", "oauth")
	generic.Set("preset", "xai")
	generic.Set("base_url", "https://evil.example/v1")
	generic.Set("oauth_session", conn.State())
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/check", strings.NewReader(generic.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("generic check status %d body %s", rec.Code, rec.Body.String())
	}

	create := url.Values{}
	create.Set("name", "stolen")
	create.Set("protocol", "openai")
	create.Set("auth_method", "oauth")
	create.Set("preset", "xai")
	create.Set("base_url", "https://evil.example/v1")
	create.Set("oauth_session", conn.State())
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(create.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("generic create status %d body %s", rec.Code, rec.Body.String())
	}
	if n, _ := h.store.ListProviders(context.Background()); len(n) != 0 {
		t.Fatal("generic create stored a Grok Build session")
	}
	if _, ok := h.sessions.peekGrokBuild(conn.State(), 0); !ok {
		t.Fatal("generic create consumed the Grok Build session")
	}

	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "plain", BaseURL: "https://api.example/v1", Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "plain-access", RefreshToken: "plain-refresh", ClientID: "client", TokenURL: "https://auth.example/token"},
	}); err != nil {
		t.Fatal(err)
	}
	edit := url.Values{}
	edit.Set("name", "plain")
	edit.Set("protocol", "openai")
	edit.Set("auth_method", "oauth")
	edit.Set("preset", "custom")
	edit.Set("base_url", "https://evil.example/v1")
	edit.Set("oauth_session", conn.State())
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/1", strings.NewReader(edit.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("generic edit status %d body %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), 1)
	if err != nil || saved.OAuthCreds.AccessToken != "plain-access" || saved.BaseURL != "https://api.example/v1" {
		t.Fatalf("generic edit changed row: %+v %v", saved, err)
	}

	marked := grokBuildTestCreds()
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "marked", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: marked,
	}); err != nil {
		t.Fatal(err)
	}
	byID := url.Values{}
	byID.Set("id", "2")
	byID.Set("protocol", "openai")
	byID.Set("auth_method", "oauth")
	byID.Set("base_url", "https://evil.example/v1")
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/check", strings.NewReader(byID.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "access-1") || !strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("saved generic check body %s", rec.Body.String())
	}
	if atomic.LoadInt32(&foreign) != 0 {
		t.Fatalf("non-subscription sends = %d", foreign)
	}

	bound := url.Values{}
	bound.Set("protocol", "grok-build")
	bound.Set("auth_method", "oauth")
	bound.Set("preset", "grok-build")
	bound.Set("base_url", "https://evil.example/v1")
	bound.Set("oauth_session", conn.State())
	req = httptest.NewRequest(http.MethodPost, "/dashboard/providers/check", strings.NewReader(bound.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "1 models") {
		t.Fatalf("bound check status %d body %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&foreign) != 0 {
		t.Fatalf("bound check left the official host: %d", foreign)
	}
}

func TestGrokBuildSavedCheckUsesOfficialHostOnly(t *testing.T) {
	h := testHandler(t)
	foreign := int32(0)
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		if !grokbuild.IsFixedInferenceURL(r.URL.String()) {
			atomic.AddInt32(&foreign, 1)
		}
		return grokJSON(http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "grok-build"}}}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	creds := grokBuildTestCreds()
	creds.ExpiresAt = time.Now().Add(time.Hour).Unix()
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
	}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("id", "1")
	form.Set("protocol", "grok-build")
	form.Set("auth_method", "oauth")
	form.Set("preset", "grok-build")
	form.Set("base_url", "https://evil.example/v1")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/check", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "1 models") {
		t.Fatalf("saved check status %d body %s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&foreign) != 0 {
		t.Fatal("saved check left the official host")
	}
}

func TestGrokBuildReconnectStaysBound(t *testing.T) {
	h := testHandler(t)
	creds := grokBuildTestCreds()
	if err := h.store.CreateProvider(context.Background(), &domain.Provider{
		Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
	}); err != nil {
		t.Fatal(err)
	}
	replaced := grokBuildTestCreds()
	replaced.AccessToken = "access-2"
	replaced.RefreshToken = "refresh-2"
	replaced.GrokBuildConnection = "connection-2"
	conn := &grokBuildStub{state: "gb-reconnect", creds: replaced}
	h.sessions.putBound(conn.State(), conn, grokBuildFlow, 1, time.Now())
	form := url.Values{}
	form.Set("name", "build")
	form.Set("protocol", "grok-build")
	form.Set("auth_method", "oauth")
	form.Set("preset", "grok-build")
	form.Set("oauth_session", conn.State())
	form.Set("base_url", "https://evil.example/v1")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/1", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect status %d body %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.BaseURL != grokbuild.BaseURL || saved.OAuthCreds.AccessToken != "access-2" || saved.OAuthCreds.GrokBuildConnection != "connection-2" {
		t.Fatalf("reconnect result = %+v creds=%+v", saved, saved.OAuthCreds)
	}
}

func grokBuildTestCreds() *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: grokbuild.Preset, GrokBuildAuth: true,
		AccessToken: "access-1", RefreshToken: "refresh-1", Scopes: grokbuild.Scopes,
		AuthURL: grokbuild.Issuer, TokenURL: grokbuild.TokenURL, ClientID: grokbuild.ClientID,
	}
}

type grokBuildStub struct {
	state string
	creds *domain.OAuthCreds
}

func (s *grokBuildStub) State() string { return s.state }
func (s *grokBuildStub) Result() (*domain.OAuthCreds, error, bool) {
	return s.creds, nil, true
}
func (s *grokBuildStub) Close() error { return nil }

type grokRoundTrip func(*http.Request) (*http.Response, error)

func (f grokRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func grokJSON(status int, doc map[string]any) *http.Response {
	raw, _ := json.Marshal(doc)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}
}

func TestGrokBuildManualFormModesAndValidation(t *testing.T) {
	longToken := strings.Repeat("a", 64<<10+1)
	exp := int64(1900000000)
	accessWithExp := grokBuildTestJWT(map[string]any{"sub": "access-user", "exp": exp})
	idWithEmail := grokBuildTestJWT(map[string]any{"sub": "id-user", "email": "id@example.com"})
	for _, tc := range []struct {
		name       string
		values     url.Values
		wantErr    string
		wantManual bool
		wantBlank  bool
		wantExpiry int64
		wantUserID string
	}{
		{
			name:   "explicit device",
			values: url.Values{"grok_build_mode": {"device"}, "oauth_session": {"state-1"}},
		},
		{
			name:   "legacy device without mode",
			values: url.Values{"oauth_session": {"state-1"}},
		},
		{
			name:      "manual blank omitted",
			values:    url.Values{"grok_build_mode": {"manual"}},
			wantBlank: true,
		},
		{
			name: "manual blank empty",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {""},
				"refresh_token":   {""},
				"id_token":        {""},
				"expires_at":      {""},
			},
			wantBlank: true,
		},
		{
			name: "manual blank whitespace",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"  "},
				"refresh_token":   {"\t"},
				"id_token":        {" "},
				"expires_at":      {"\n"},
			},
			wantBlank: true,
		},
		{
			name: "manual blank with device session",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"oauth_session":   {"state-1"},
			},
			wantErr: "cannot be combined",
		},
		{
			name: "manual blank with device session after blank value",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"oauth_session":   {"", "state-1"},
			},
			wantErr: "cannot be combined",
		},
		{
			name: "manual blank access with refresh",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {""},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "access token is required",
		},
		{
			name: "manual pair",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantManual: true,
		},
		{
			name: "manual expiry rfc3339",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
				"expires_at":      {"2030-01-02T03:04:05Z"},
			},
			wantManual: true,
			wantExpiry: 1893553445,
		},
		{
			name: "manual expiry unix and id identity",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
				"id_token":        {idWithEmail},
				"expires_at":      {"1900000000"},
			},
			wantManual: true,
			wantExpiry: 1900000000,
			wantUserID: "id-user",
		},
		{
			name: "manual jwt expiry hint",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {accessWithExp},
				"refresh_token":   {"refresh-token-value"},
			},
			wantManual: true,
			wantExpiry: exp,
		},
		{
			name: "expired access allowed",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
				"expires_at":      {"1"},
			},
			wantManual: true,
			wantExpiry: 1,
		},
		{name: "unknown mode", values: url.Values{"grok_build_mode": {"browser"}}, wantErr: "mode is invalid"},
		{name: "empty mode", values: url.Values{"grok_build_mode": {""}}, wantErr: "mode is invalid"},
		{
			name: "duplicate mode",
			values: url.Values{
				"grok_build_mode": {"device", "manual"},
			},
			wantErr: "mode is invalid",
		},
		{
			name: "manual with device session",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"oauth_session":   {"state-1"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "cannot be combined",
		},
		{
			name: "manual with device session after blank value",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"oauth_session":   {"", "state-1"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "cannot be combined",
		},
		{
			name:    "device with manual token",
			values:  url.Values{"grok_build_mode": {"device"}, "access_token": {"access-token-value"}},
			wantErr: "cannot be combined",
		},
		{
			name:    "no mode with optional metadata only",
			values:  url.Values{"id_token": {idWithEmail}},
			wantErr: "mode is required",
		},
		{
			name: "manual missing refresh",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
			},
			wantErr: "refresh token is required",
		},
		{
			name: "manual optional only",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"id_token":        {idWithEmail},
				"expires_at":      {"1900000000"},
			},
			wantErr: "access token is required",
		},
		{
			name: "duplicate access",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-one", "access-token-two"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "duplicate",
		},
		{
			name: "malformed expiry",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
				"expires_at":      {"tomorrow"},
			},
			wantErr: "RFC3339 or unix seconds",
		},
		{
			name: "zero expiry",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token-value"},
				"refresh_token":   {"refresh-token-value"},
				"expires_at":      {"0"},
			},
			wantErr: "expiry is invalid",
		},
		{
			name: "public api key",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"xai-public-api-key"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "public xAI API key",
		},
		{
			name: "control character",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"access-token\nvalue"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "malformed",
		},
		{
			name: "short token",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {"short"},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "too short",
		},
		{
			name: "long token",
			values: url.Values{
				"grok_build_mode": {"manual"},
				"access_token":    {longToken},
				"refresh_token":   {"refresh-token-value"},
			},
			wantErr: "too long",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := grokBuildFormRequest(t, tc.values)
			input, err := parseGrokBuildForm(req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				for _, secret := range tc.values["access_token"] {
					if len(secret) >= 8 && strings.Contains(err.Error(), secret) {
						t.Fatalf("error leaked token: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if input.manualBlank != tc.wantBlank {
				t.Fatalf("manualBlank = %v, want %v", input.manualBlank, tc.wantBlank)
			}
			if (input.manual != nil) != tc.wantManual {
				t.Fatalf("manual = %+v, want %v", input.manual, tc.wantManual)
			}
			if tc.wantManual && (input.manual.ExpiresAt != tc.wantExpiry || input.manual.UserID != tc.wantUserID) {
				t.Fatalf("manual expiry/user = %d/%q, want %d/%q", input.manual.ExpiresAt, input.manual.UserID, tc.wantExpiry, tc.wantUserID)
			}
			if !tc.wantManual && input.session != tc.values.Get("oauth_session") {
				t.Fatalf("session = %q", input.session)
			}
		})
	}
}

func TestGrokBuildManualAddEditAndMetadata(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	form := grokBuildManualForm()
	req := grokBuildFormRequest(t, form)
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual add status %d body %s", rec.Code, rec.Body.String())
	}
	stored, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	saved := stored.Provider
	creds := saved.OAuthCreds
	if saved.BaseURL != grokbuild.BaseURL || saved.APIKey != "" || saved.Protocol != domain.ProtocolGrokBuild {
		t.Fatalf("saved row = %+v", saved)
	}
	if creds == nil || !creds.GrokBuildAuth || creds.Mode != domain.OAuthManual || creds.Preset != grokbuild.Preset {
		t.Fatalf("manual creds markers = %+v", creds)
	}
	if creds.ClientID != grokbuild.ClientID || creds.TokenURL != grokbuild.TokenURL || creds.AuthURL != grokbuild.Issuer || creds.Scopes != grokbuild.Scopes {
		t.Fatalf("manual creds metadata = %+v", creds)
	}
	if creds.GrokBuildConnection == "" || creds.AccessToken != grokBuildTestJWT(map[string]any{"sub": "access-user"}) || creds.RefreshToken != "refresh-manual-1" {
		t.Fatalf("manual creds tokens/generation = %+v", creds)
	}
	if creds.UserID != "id-user" || creds.Email != "id@example.com" || creds.IDToken != "" {
		t.Fatalf("manual identity = %+v", creds)
	}
	if creds.ExpiresAt != 1893553445 {
		t.Fatalf("manual expiry = %d", creds.ExpiresAt)
	}
	oldGeneration := creds.GrokBuildConnection
	if err := h.store.CreateCombo(ctx, &domain.Combo{Name: "build", Targets: []domain.ComboTarget{
		{ProviderID: saved.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}

	meta := url.Values{
		"grok_build_mode": {"device"},
		"name":            {"build renamed"},
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"tags":            {"team"},
		"base_url":        {"https://evil.example/v1"},
	}
	req = grokBuildFormRequest(t, meta)
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata edit status %d body %s", rec.Code, rec.Body.String())
	}
	afterMeta, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if afterMeta.OAuthBlob != stored.OAuthBlob {
		t.Fatal("metadata edit rewrote the credential blob")
	}
	if afterMeta.Provider.Name != "build renamed" || afterMeta.Provider.BaseURL != grokbuild.BaseURL {
		t.Fatalf("metadata row = %+v", afterMeta.Provider)
	}

	replacement := grokBuildManualForm()
	replacement.Set("name", "build renamed")
	replacement.Set("access_token", "replacement-access-token")
	replacement.Set("refresh_token", "replacement-refresh-token")
	replacement.Del("id_token")
	replacement.Set("expires_at", "1900000000")
	req = grokBuildFormRequest(t, replacement)
	req.SetPathValue("id", "1")
	rec = httptest.NewRecorder()
	h.updateProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual replacement status %d body %s", rec.Code, rec.Body.String())
	}
	replaced, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Provider.ID != saved.ID {
		t.Fatalf("provider id changed: %d", replaced.Provider.ID)
	}
	got := replaced.Provider.OAuthCreds
	if got.AccessToken != "replacement-access-token" || got.RefreshToken != "replacement-refresh-token" || got.ExpiresAt != 1900000000 {
		t.Fatalf("replacement tokens = %+v", got)
	}
	if got.GrokBuildConnection == "" || got.GrokBuildConnection == oldGeneration {
		t.Fatalf("replacement generation = %q, old %q", got.GrokBuildConnection, oldGeneration)
	}
	if got.UserID != "" || got.Email != "" || got.IDToken != "" {
		t.Fatalf("replacement copied old identity = %+v", got)
	}
	combo, err := h.store.GetComboByName(ctx, "build")
	if err != nil || len(combo.Targets) != 1 || combo.Targets[0].ProviderID != saved.ID {
		t.Fatalf("combo link changed: %+v err=%v", combo, err)
	}
}

func TestGrokBuildManualBlankEditKeepsStoredOAuth(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	req := grokBuildFormRequest(t, grokBuildManualForm())
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual add status %d body %s", rec.Code, rec.Body.String())
	}
	before, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		tokens   url.Values
		wantName string
	}{
		{name: "omitted", tokens: url.Values{}, wantName: "blank omitted"},
		{name: "empty", wantName: "blank empty", tokens: url.Values{
			"access_token": {""}, "refresh_token": {""}, "id_token": {""}, "expires_at": {""},
		}},
		{name: "whitespace", wantName: "blank whitespace", tokens: url.Values{
			"access_token": {"  "}, "refresh_token": {"\t"}, "id_token": {" "}, "expires_at": {"\n"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{
				"name":            {tc.wantName},
				"protocol":        {"grok-build"},
				"auth_method":     {"oauth"},
				"preset":          {"grok-build"},
				"grok_build_mode": {"manual"},
				"tags":            {"edited"},
			}
			for key, values := range tc.tokens {
				form[key] = values
			}
			req := grokBuildFormRequest(t, form)
			req.SetPathValue("id", "1")
			rec := httptest.NewRecorder()
			h.updateProvider(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("blank manual edit status %d body %s", rec.Code, rec.Body.String())
			}
			after, err := h.store.GetProviderOAuthStored(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if after.OAuthBlob != before.OAuthBlob {
				t.Fatal("blank manual edit rewrote the credential blob")
			}
			if after.Provider.Name != tc.wantName || after.Provider.OAuthCreds == nil ||
				after.Provider.OAuthCreds.GrokBuildConnection != before.Provider.OAuthCreds.GrokBuildConnection {
				t.Fatalf("blank manual edit row = %+v creds=%+v", after.Provider, after.Provider.OAuthCreds)
			}
			if len(after.Provider.Tags) != 1 || after.Provider.Tags[0] != "edited" {
				t.Fatalf("blank manual edit tags = %#v", after.Provider.Tags)
			}
		})
	}

	blankAdd := url.Values{
		"name": {"blank add"}, "protocol": {"grok-build"}, "auth_method": {"oauth"},
		"preset": {"grok-build"}, "grok_build_mode": {"manual"},
		"access_token": {""}, "refresh_token": {""}, "id_token": {""}, "expires_at": {""},
	}
	rec = httptest.NewRecorder()
	h.createProvider(rec, grokBuildFormRequest(t, blankAdd))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "paste both") {
		t.Fatalf("blank manual add status %d body %s", rec.Code, rec.Body.String())
	}
	providers, err := h.store.ListProviders(ctx)
	if err != nil || len(providers) != 1 {
		t.Fatalf("blank manual add providers = %d err=%v", len(providers), err)
	}

	var calls int
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return grokJSON(http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "grok-build"}}}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	blankCheck := url.Values{
		"id": {"1"}, "protocol": {"grok-build"}, "auth_method": {"oauth"}, "preset": {"grok-build"},
		"grok_build_mode": {"manual"}, "access_token": {" "}, "refresh_token": {""},
		"id_token": {""}, "expires_at": {""},
	}
	rec = httptest.NewRecorder()
	h.checkProvider(rec, grokBuildFormRequest(t, blankCheck))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "paste both") {
		t.Fatalf("blank manual check status %d body %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("blank manual check fell back to saved credentials: %d calls", calls)
	}
	afterCheck, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if afterCheck.OAuthBlob != before.OAuthBlob {
		t.Fatal("blank manual check wrote the saved credential blob")
	}
}

func TestGrokBuildManualCheckUsesCandidateOnly(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	saved := grokBuildTestCreds()
	saved.AccessToken = "saved-access-token"
	saved.RefreshToken = "saved-refresh-token"
	saved.ExpiresAt = 1
	if err := h.store.CreateProvider(ctx, &domain.Provider{
		Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: saved,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Userid"))
		if r.Method != http.MethodGet || r.URL.String() != grokbuild.ModelsURL {
			t.Errorf("manual check left /models: %s %s", r.Method, r.URL)
		}
		return grokJSON(http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "grok-build"}}}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	access := grokBuildTestJWT(map[string]any{"sub": "access-user"})
	id := grokBuildTestJWT(map[string]any{"sub": "candidate-user"})
	form := url.Values{
		"id":              {"1"},
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"base_url":        {"https://evil.example/v1"},
		"grok_build_mode": {"manual"},
		"access_token":    {access},
		"refresh_token":   {"candidate-refresh-token"},
		"id_token":        {id},
		"expires_at":      {"1"},
	}
	req := grokBuildFormRequest(t, form)
	rec := httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "1 models") {
		t.Fatalf("manual check status %d body %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 || !strings.Contains(calls[0], "Bearer "+access) || !strings.Contains(calls[0], "candidate-user") {
		t.Fatalf("manual check calls = %#v", calls)
	}
	after, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuthBlob != before.OAuthBlob {
		t.Fatal("manual check wrote saved credentials")
	}

	calls = nil
	bad := url.Values{
		"id":              {"1"},
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"grok_build_mode": {"manual"},
		"access_token":    {"xai-public-api-key"},
		"refresh_token":   {"candidate-refresh-token"},
	}
	req = grokBuildFormRequest(t, bad)
	rec = httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "public xAI API key") {
		t.Fatalf("bad manual check status %d body %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("bad manual check fell back to saved or device credentials: %#v", calls)
	}
}

func TestGrokBuildManualCheckRejectsSessionWithoutConsumingIt(t *testing.T) {
	h := testHandler(t)
	var calls int
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return grokJSON(http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "grok-build"}}}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	connCreds := grokBuildTestCreds()
	conn := &grokBuildStub{state: "gb-session", creds: connCreds}
	h.sessions.putBound(conn.State(), conn, grokBuildFlow, 0, time.Now())

	mixed := url.Values{
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"oauth_session":   {conn.State()},
		"grok_build_mode": {"manual"},
		"access_token":    {"candidate-access-token"},
		"refresh_token":   {"candidate-refresh-token"},
	}
	req := grokBuildFormRequest(t, mixed)
	rec := httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cannot be combined") {
		t.Fatalf("mixed check status %d body %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("mixed check sent %d requests", calls)
	}
	if _, ok := h.sessions.peekGrokBuild(conn.State(), 0); !ok {
		t.Fatal("mixed check consumed the device session")
	}

	device := url.Values{
		"protocol":      {"grok-build"},
		"auth_method":   {"oauth"},
		"preset":        {"grok-build"},
		"oauth_session": {conn.State()},
	}
	req = grokBuildFormRequest(t, device)
	rec = httptest.NewRecorder()
	h.checkProvider(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "1 models") || calls != 1 {
		t.Fatalf("device check status %d calls %d body %s", rec.Code, calls, rec.Body.String())
	}
}

func TestGrokBuildManualReplacementCAS(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	old := grokBuildTestCreds()
	old.GrokBuildConnection = "connection-old"
	if err := h.store.CreateProvider(ctx, &domain.Provider{
		Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: old,
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := h.store.GetProviderOAuthStored(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := grokBuildTestCreds()
	concurrent.AccessToken = "concurrent-access-token"
	concurrent.RefreshToken = "concurrent-refresh-token"
	concurrent.GrokBuildConnection = "connection-concurrent"
	if err := h.store.UpdateProviderIfConnection(ctx, 1, store.ProviderConnectionSave{
		ProviderMeta: store.ProviderMeta{
			Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
			AuthScheme: domain.AuthBearer, AuthMethod: domain.AuthOAuth,
		},
		OAuthCreds: concurrent,
	}, stale.Provider.AuthMethod, stale.Provider.Protocol, stale.OAuthBlob); err != nil {
		t.Fatal(err)
	}

	form := grokBuildManualForm()
	form.Set("name", "build")
	req := grokBuildFormRequest(t, form)
	rec := httptest.NewRecorder()
	h.saveGrokBuildProvider(rec, req, stale.Provider, stale.OAuthBlob)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stale manual replacement status %d body %s", rec.Code, rec.Body.String())
	}
	after, err := h.store.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuthCreds.GrokBuildConnection != "connection-concurrent" || after.OAuthCreds.AccessToken != "concurrent-access-token" {
		t.Fatalf("stale replacement overwrote concurrent connection: %+v", after.OAuthCreds)
	}
}

func TestGrokBuildManualEditDoesNotRenderStoredSecrets(t *testing.T) {
	p := &domain.Provider{
		ID: 1, Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			GrokBuildAuth: true, Preset: grokbuild.Preset, AccessToken: "stored-access-secret",
			RefreshToken: "stored-refresh-secret", IDToken: "stored-id-secret", UserID: "user-1",
		},
	}
	html := renderComponent(t, providerEditRowGrokBuild(p))
	for _, secret := range []string{"stored-access-secret", "stored-refresh-secret", "stored-id-secret"} {
		if strings.Contains(html, secret) {
			t.Fatalf("edit form rendered %s: %s", secret, html)
		}
	}
	for _, want := range []string{"data-oauth-mode=\"web\"", "data-oauth-lock", "Device sign-in", "Paste tokens", "Reconnect (Grok Build)"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q: %s", want, html)
		}
	}
}

func grokBuildManualForm() url.Values {
	return url.Values{
		"name":            {"build"},
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"grok_build_mode": {"manual"},
		"access_token":    {grokBuildTestJWT(map[string]any{"sub": "access-user"})},
		"refresh_token":   {"refresh-manual-1"},
		"id_token":        {grokBuildTestJWT(map[string]any{"sub": "id-user", "email": "id@example.com"})},
		"expires_at":      {"2030-01-02T03:04:05Z"},
	}
}

func grokBuildFormRequest(t *testing.T, values url.Values) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return req
}

func grokBuildTestJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestGrokBuildSavedRefreshUsesStoredGrantOnly(t *testing.T) {
	h := testHandler(t)
	ctx := context.Background()
	saved := grokBuildTestCreds()
	saved.Mode = domain.OAuthManual
	saved.AccessToken = "saved-access-token"
	saved.RefreshToken = "saved-refresh-token"
	saved.GrokBuildConnection = "connection-saved"
	saved.UserID = "user-1"
	saved.ExpiresAt = 1
	if err := h.store.CreateProvider(ctx, &domain.Provider{
		Name: "build", BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: saved,
	}); err != nil {
		t.Fatal(err)
	}
	var hits int
	var submitted string
	grokbuild.SetTestTransport(grokRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != grokbuild.TokenURL {
			t.Errorf("saved refresh left the fixed token host: %s", r.URL)
		}
		hits++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		submitted = r.Form.Get("refresh_token")
		return grokJSON(http.StatusOK, map[string]any{
			"access_token": "rotated-access-token", "refresh_token": "rotated-refresh-token",
			"expires_in": 600, "id_token": grokBuildTestJWT(map[string]any{"sub": "user-1"}),
		}), nil
	}))
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })

	form := url.Values{
		"id":              {"1"},
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"grok_build_mode": {"manual"},
		"access_token":    {"candidate-access-token"},
		"refresh_token":   {"candidate-refresh-token"},
	}
	req := grokBuildFormRequest(t, form)
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("saved refresh status %d body %s", rec.Code, rec.Body.String())
	}
	if hits != 1 || submitted != "saved-refresh-token" {
		t.Fatalf("hits=%d submitted=%q", hits, submitted)
	}
	after, err := h.store.GetProvider(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuthCreds.AccessToken != "rotated-access-token" || after.OAuthCreds.RefreshToken != "rotated-refresh-token" {
		t.Fatalf("saved refresh result = %+v", after.OAuthCreds)
	}

	hits = 0
	unsaved := url.Values{
		"protocol":        {"grok-build"},
		"auth_method":     {"oauth"},
		"preset":          {"grok-build"},
		"grok_build_mode": {"manual"},
		"access_token":    {"candidate-access-token"},
		"refresh_token":   {"candidate-refresh-token"},
	}
	req = grokBuildFormRequest(t, unsaved)
	rec = httptest.NewRecorder()
	h.oauthRefreshTokens(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "save the Grok Build provider") {
		t.Fatalf("unsaved refresh status %d body %s", rec.Code, rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("unsaved refresh hit the token endpoint %d times", hits)
	}
}
