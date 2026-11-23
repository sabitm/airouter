package web

import (
	"context"
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
	if strings.Contains(html, "api.x.ai") || strings.Contains(html, "access token") {
		t.Fatalf("form offers public host or token paste: %s", html)
	}
	if !strings.Contains(html, "/dashboard/providers/grok-build/device/begin") {
		t.Fatalf("missing device begin: %s", html)
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
