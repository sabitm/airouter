package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/claudecode"
)

func TestReconnectExchangeKeepsStoredDevice(t *testing.T) {
	h := testHandler(t)
	device := strings.Repeat("ab", 32)
	account := "11111111-1111-4111-8111-111111111111"
	prev := &domain.OAuthCreds{
		Mode:              domain.OAuthAuto,
		Preset:            "claude",
		ClaudeCodeAuth:    true,
		AccessToken:       "old-access",
		RefreshToken:      "old-refresh",
		TokenURL:          claudecode.TokenURL,
		ClientID:          claudecode.ClientID,
		ClaudeAccountUUID: account,
		AccountID:         account,
		ClaudeDeviceID:    device,
		ClaudeScopes:      "user:inference",
	}
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: prev,
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	var seenScope string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		seenScope = req["scope"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
			"scope":         "user:inference user:profile",
			"account":       map[string]string{"uuid": account},
		})
	}))
	t.Cleanup(tokenSrv.Close)

	creds, err := credsFromConnectForm(formRequest(url.Values{"preset": {"claude"}}))
	if err != nil {
		t.Fatal(err)
	}
	creds.TokenURL = tokenSrv.URL + "/token"
	conn, err := oauth.NewConnect(creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExchangeCode(context.Background(), "auth-code"); err != nil {
		t.Fatal(err)
	}
	exchanged, _, done := conn.Result()
	if !done || exchanged == nil || exchanged.ClaudeDeviceID == "" || exchanged.ClaudeDeviceID == device {
		t.Fatalf("exchange did not generate a distinct device before save: %+v", exchanged)
	}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())

	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "renamed")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://api.anthropic.com")
	form.Set("oauth_session", conn.State())
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, form))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.ClaudeDeviceID != device {
		t.Fatalf("same-account reconnect rotated device to %s", saved.OAuthCreds.ClaudeDeviceID)
	}
	if saved.OAuthCreds.AccessToken != "new-access" || saved.OAuthCreds.RefreshToken != "new-refresh" {
		t.Fatalf("tokens not replaced: %+v", saved.OAuthCreds)
	}
	if saved.OAuthCreds.ClaudeAccountUUID != account {
		t.Fatalf("account = %s", saved.OAuthCreds.ClaudeAccountUUID)
	}
	if saved.Name != "renamed" {
		t.Fatalf("name = %s", saved.Name)
	}
	if seenScope != "" {
		t.Fatalf("exchange sent scope %q; authorize exchange must not invent a grant", seenScope)
	}
}

func TestAccessOnlyDifferentAccountImportDoesNotBorrow(t *testing.T) {
	h := testHandler(t)
	prev := &domain.OAuthCreds{
		ClaudeCodeAuth:    true,
		AccessToken:       "old-access",
		RefreshToken:      "old-refresh",
		ClaudeAccountUUID: "old-account",
		AccountID:         "old-account",
		ClaudeDeviceID:    strings.Repeat("a", 64),
		ClaudeScopes:      "user:inference user:profile",
		ClaudeOrgUUID:     "old-org",
	}
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: prev,
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "cc")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://api.anthropic.com")
	form.Set("preset", "claude")
	form.Set("access_token", "different-access-only")
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, form))
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d body = %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := saved.OAuthCreds
	if c.ClaudeAccountUUID != "" || c.AccountID != "" || c.ClaudeDeviceID != "" || c.ClaudeScopes != "" || c.ClaudeOrgUUID != "" {
		t.Fatalf("unknown access-only import borrowed identity: %+v", c)
	}
	if c.AccessToken != "different-access-only" {
		t.Fatalf("access = %q", c.AccessToken)
	}
	if c.RefreshToken != "" {
		t.Fatalf("empty refresh was treated as the old refresh: %q", c.RefreshToken)
	}
}

func TestNormalEditPreservesClaudeIdentity(t *testing.T) {
	h := testHandler(t)
	prev := &domain.OAuthCreds{
		ClaudeCodeAuth:    true,
		AccessToken:       "keep-access",
		RefreshToken:      "keep-refresh",
		ClaudeAccountUUID: "keep-account",
		ClaudeDeviceID:    strings.Repeat("b", 64),
		ClaudeScopes:      "user:inference",
	}
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: prev,
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "cc-renamed")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://api.anthropic.com")
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, form))
	if rec.Code != http.StatusOK {
		t.Fatalf("edit status = %d body = %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.ClaudeDeviceID != prev.ClaudeDeviceID || saved.OAuthCreds.ClaudeAccountUUID != prev.ClaudeAccountUUID || saved.OAuthCreds.ClaudeScopes != prev.ClaudeScopes {
		t.Fatalf("normal edit dropped identity: %+v", saved.OAuthCreds)
	}
	if saved.Name != "cc-renamed" {
		t.Fatalf("name = %s", saved.Name)
	}
}

func formRequest(form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = req.ParseForm()
	return req
}

func postForm(id int64, form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers/x", strings.NewReader(form.Encode()))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestRefreshDifferentClaudeRefreshTokenDoesNotBorrowIdentity(t *testing.T) {
	h := testHandler(t)
	oldDevice := strings.Repeat("ab", 32)
	prev := &domain.OAuthCreds{
		Mode:                   domain.OAuthAuto,
		Preset:                 "claude",
		ClaudeCodeAuth:         true,
		AccessToken:            "old-access",
		RefreshToken:           "old-refresh",
		ExpiresAt:              1,
		Email:                  "old@example.com",
		AccountID:              "old-account",
		ClaudeAccountUUID:      "old-account",
		ClaudeOrgUUID:          "old-org",
		ClaudeDeviceID:         oldDevice,
		ClaudeScopes:           "user:inference user:profile",
		ClaudeRefreshExpiresAt: 123,
		TokenURL:               "pending",
		ClientID:               "cid",
		Scopes:                 "preset-scope",
		RefreshJSON:            true,
	}
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	prev.TokenURL = srv.URL
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: prev,
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("preset", "custom")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	form.Set("refresh_token", "new-refresh")
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body = %s", rec.Code, rec.Body.String())
	}
	if body["refresh_token"] != "new-refresh" {
		t.Fatalf("token endpoint refresh token = %q", body["refresh_token"])
	}
	if _, ok := body["scope"]; ok {
		t.Fatalf("replacement grant sent stored scopes: %v", body)
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := saved.OAuthCreds
	if c.AccessToken != "fresh-access" || c.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %+v", c)
	}
	if c.ClaudeAccountUUID != "" || c.AccountID != "" || c.ClaudeOrgUUID != "" || c.OrganizationID != "" || c.ClaudeScopes != "" || c.Email != "" {
		t.Fatalf("unknown replacement grant borrowed identity: %+v", c)
	}
	if c.ClaudeDeviceID == "" || c.ClaudeDeviceID == oldDevice {
		t.Fatalf("replacement device was borrowed or empty: %q", c.ClaudeDeviceID)
	}
	if c.TokenURL != srv.URL || c.ClientID != "cid" {
		t.Fatalf("endpoint/client config was not carried: %+v", c)
	}
}

func TestRefreshSameClaudeRefreshTokenKeepsIdentity(t *testing.T) {
	h := testHandler(t)
	device := strings.Repeat("cd", 32)
	prev := &domain.OAuthCreds{
		Mode:              domain.OAuthAuto,
		Preset:            "claude",
		ClaudeCodeAuth:    true,
		AccessToken:       "old-access",
		RefreshToken:      "same-refresh",
		ExpiresAt:         1,
		Email:             "keep@example.com",
		AccountID:         "keep-account",
		ClaudeAccountUUID: "keep-account",
		ClaudeOrgUUID:     "keep-org",
		ClaudeDeviceID:    device,
		ClaudeScopes:      "user:inference",
		TokenURL:          "pending",
		ClientID:          "cid",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"same-fresh","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	prev.TokenURL = srv.URL
	p := &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: prev,
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("preset", "custom")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d body = %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := saved.OAuthCreds
	if c.AccessToken != "same-fresh" || c.RefreshToken != "same-refresh" {
		t.Fatalf("tokens = %+v", c)
	}
	if c.ClaudeAccountUUID != "keep-account" || c.AccountID != "keep-account" || c.ClaudeOrgUUID != "keep-org" {
		t.Fatalf("same refresh dropped account/org: %+v", c)
	}
	if c.ClaudeDeviceID != device || c.ClaudeScopes != "user:inference" || c.Email != "keep@example.com" {
		t.Fatalf("same refresh dropped device/scopes/email: %+v", c)
	}
}

type claudeRefreshCapture struct {
	method string
	url    string
	body   map[string]string
}

// claudeRefreshRoundTrip is the serial guard for saved Claude refresh tests.
// It rejects every request before networking unless the URL is the exact local
// fixture. A regression that selects the public preset must fail closed.
type claudeRefreshRoundTrip struct {
	local    http.RoundTripper
	allowed  string
	rejected *string
}

func (rt claudeRefreshRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.String() != rt.allowed {
		got := ""
		if req != nil && req.URL != nil {
			got = req.URL.Redacted()
		}
		if rt.rejected != nil {
			*rt.rejected = got
		}
		return nil, errors.New("claude refresh test blocked unexpected request")
	}
	return rt.local.RoundTrip(req)
}

func withClaudeRefreshTransport(t *testing.T, allowed string) {
	t.Helper()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		t.Fatal("default transport is not *http.Transport")
	}
	local := base.Clone()
	local.Proxy = nil
	previous := http.DefaultTransport
	http.DefaultTransport = claudeRefreshRoundTrip{local: local, allowed: allowed}
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func TestClaudeRefreshTransportRejectsUnexpectedRequest(t *testing.T) {
	rejected := "unset"
	rt := claudeRefreshRoundTrip{allowed: "http://127.0.0.1:1/oauth/token", rejected: &rejected}
	req := httptest.NewRequest(http.MethodPost, claudecode.TokenURL, strings.NewReader(`{"refresh_token":"do-not-send"}`))
	resp, err := rt.RoundTrip(req)
	if resp != nil || err == nil || !strings.Contains(err.Error(), "blocked unexpected request") {
		t.Fatalf("public request resp=%v err=%v", resp, err)
	}
	if rejected != claudecode.TokenURL {
		t.Fatalf("rejected URL = %q", rejected)
	}
}

func TestSavedClaudeRefreshKeepsStoredConfig(t *testing.T) {
	tests := []struct {
		name            string
		preset          string
		emptyConfig     bool
		unknownScopes   bool
		pastedRefresh   string
		response        string
		wantScope       string
		wantScopeAbsent bool
		wantAccount     string
		wantOrg         string
		wantEmail       string
		wantScopes      string
		keepDevice      bool
		wantSentRefresh string
		wantRefresh     string
		wantExpiry      bool
	}{
		{
			name:            "ordinary stored refresh",
			preset:          "claude",
			response:        `{"access_token":"rotated-access","expires_in":3600}`,
			wantScope:       "user:inference",
			wantAccount:     "acct-stored",
			wantOrg:         "org-stored",
			wantEmail:       "stored@example.com",
			wantScopes:      "user:inference",
			keepDevice:      true,
			wantSentRefresh: "stored-refresh",
			wantRefresh:     "stored-refresh",
		},
		{
			name:            "same pasted refresh",
			preset:          "claude",
			pastedRefresh:   "stored-refresh",
			response:        `{"access_token":"same-rotated","expires_in":3600}`,
			wantScope:       "user:inference",
			wantAccount:     "acct-stored",
			wantOrg:         "org-stored",
			wantEmail:       "stored@example.com",
			wantScopes:      "user:inference",
			keepDevice:      true,
			wantSentRefresh: "stored-refresh",
			wantRefresh:     "stored-refresh",
		},
		{
			name:            "different pasted refresh",
			preset:          "claude",
			pastedRefresh:   "replacement-refresh",
			response:        `{"access_token":"replacement-access","expires_in":3600}`,
			wantScopeAbsent: true,
			wantSentRefresh: "replacement-refresh",
			wantRefresh:     "replacement-refresh",
		},
		{
			name:            "custom preset refresh",
			preset:          "custom",
			response:        `{"access_token":"custom-rotated","expires_in":3600}`,
			wantScope:       "user:inference",
			wantAccount:     "acct-stored",
			wantOrg:         "org-stored",
			wantEmail:       "stored@example.com",
			wantScopes:      "user:inference",
			keepDevice:      true,
			wantSentRefresh: "stored-refresh",
			wantRefresh:     "stored-refresh",
		},
		{
			name:            "confirmed replacement account",
			preset:          "claude",
			pastedRefresh:   "replacement-refresh",
			response:        `{"access_token":"confirmed-access","refresh_token":"endpoint-refresh","expires_in":3600,"refresh_token_expires_in":7200,"scope":"user:profile","account":{"uuid":"acct-new","email_address":"new@example.com"},"organization":{"uuid":"org-new"}}`,
			wantScopeAbsent: true,
			wantAccount:     "acct-new",
			wantOrg:         "org-new",
			wantEmail:       "new@example.com",
			wantScopes:      "user:profile",
			wantSentRefresh: "replacement-refresh",
			wantRefresh:     "endpoint-refresh",
			wantExpiry:      true,
		},
		{
			name:            "empty optional config",
			preset:          "claude",
			emptyConfig:     true,
			response:        `{"access_token":"empty-config-access","expires_in":3600}`,
			wantScope:       "user:inference",
			wantAccount:     "acct-stored",
			wantOrg:         "org-stored",
			wantEmail:       "stored@example.com",
			wantScopes:      "user:inference",
			keepDevice:      true,
			wantSentRefresh: "stored-refresh",
			wantRefresh:     "stored-refresh",
		},
		{
			name:            "unknown granted scopes",
			preset:          "claude",
			unknownScopes:   true,
			response:        `{"access_token":"unknown-scope-access","expires_in":3600}`,
			wantScopeAbsent: true,
			wantAccount:     "acct-stored",
			wantOrg:         "org-stored",
			wantEmail:       "stored@example.com",
			keepDevice:      true,
			wantSentRefresh: "stored-refresh",
			wantRefresh:     "stored-refresh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := testHandler(t)
			device := strings.Repeat("ef", 32)
			var saw claudeRefreshCapture
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				saw.method = r.Method
				saw.url = r.URL.String()
				_ = json.Unmarshal(raw, &saw.body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(srv.Close)
			allowed := srv.URL + "/oauth/token"
			withClaudeRefreshTransport(t, allowed)

			stored := &domain.OAuthCreds{
				Mode:                   domain.OAuthManual,
				Preset:                 "custom",
				ClaudeCodeAuth:         true,
				AccessToken:            "stored-access",
				RefreshToken:           "stored-refresh",
				ExpiresAt:              11,
				Email:                  "stored@example.com",
				IDToken:                "stored-id-token",
				AccountID:              "acct-stored",
				OrganizationID:         "org-stored",
				AuthURL:                "https://stored.example/authorize",
				TokenURL:               allowed,
				ClientID:               "stored-client",
				ClientSecret:           "stored-secret",
				Scopes:                 "stored-configured-scope",
				RedirectURI:            "http://127.0.0.1:9/stored",
				PKCE:                   false,
				ExtraAuthParams:        map[string]string{"legacy": "1"},
				RefreshJSON:            false,
				RefreshURL:             "https://stored.example/refresh",
				ClaudeAccountUUID:      "acct-stored",
				ClaudeOrgUUID:          "org-stored",
				ClaudeDeviceID:         device,
				ClaudeScopes:           "user:inference",
				ClaudeRefreshExpiresAt: 99,
			}
			if tt.emptyConfig {
				stored.Preset = ""
				stored.AuthURL = ""
				stored.ClientSecret = ""
				stored.Scopes = ""
				stored.RedirectURI = ""
				stored.RefreshURL = ""
				stored.ExtraAuthParams = nil
			}
			if tt.unknownScopes {
				stored.ClaudeScopes = ""
				stored.Scopes = "stored-configured-scope"
			}
			p := &domain.Provider{
				Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
				AuthMethod: domain.AuthOAuth, OAuthCreds: stored,
			}
			if err := h.store.CreateProvider(context.Background(), p); err != nil {
				t.Fatal(err)
			}

			form := url.Values{}
			form.Set("id", strconv.FormatInt(p.ID, 10))
			form.Set("preset", tt.preset)
			form.Set("refresh_view", "status")
			form.Set("auth_url", claudecode.AuthorizeURL)
			form.Set("token_url", claudecode.TokenURL)
			form.Set("client_id", claudecode.ClientID)
			form.Set("client_secret", "preset-secret")
			form.Set("scopes", claudecode.DefaultScopes)
			form.Set("redirect_uri", "http://localhost:56124/callback")
			form.Set("refresh_url", "https://preset.example/refresh")
			form.Set("pkce", "true")
			form.Set("access_token", "stale-form-access")
			form.Set("email", "stale@example.com")
			if tt.pastedRefresh != "" {
				form.Set("refresh_token", tt.pastedRefresh)
			}
			rec := httptest.NewRecorder()
			h.oauthRefreshTokens(rec, reqWithForm(form))
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "refresh failed") || strings.Contains(rec.Body.String(), "required") {
				t.Fatalf("refresh status = %d body = %s saw = %+v", rec.Code, rec.Body.String(), saw)
			}

			if saw.method != http.MethodPost || saw.url != "/oauth/token" {
				t.Fatalf("token request = %s %s, want POST /oauth/token", saw.method, saw.url)
			}
			if saw.body["grant_type"] != "refresh_token" || saw.body["client_id"] != "stored-client" || saw.body["refresh_token"] != tt.wantSentRefresh {
				t.Fatalf("token body = %#v", saw.body)
			}
			if tt.wantScopeAbsent {
				if _, ok := saw.body["scope"]; ok {
					t.Fatalf("grant request sent scope %#v", saw.body)
				}
			} else if saw.body["scope"] != tt.wantScope {
				t.Fatalf("scope = %q, want stored grant %q", saw.body["scope"], tt.wantScope)
			}

			saved, err := h.store.GetProvider(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			c := saved.OAuthCreds
			if c.AuthURL != stored.AuthURL || c.TokenURL != stored.TokenURL || c.ClientID != stored.ClientID || c.ClientSecret != stored.ClientSecret || c.Scopes != stored.Scopes || c.RedirectURI != stored.RedirectURI || c.PKCE != stored.PKCE || c.Mode != stored.Mode || c.Preset != stored.Preset || c.RefreshJSON != stored.RefreshJSON || c.RefreshURL != stored.RefreshURL {
				t.Fatalf("stored config migrated: %+v", c)
			}
			if !reflect.DeepEqual(c.ExtraAuthParams, stored.ExtraAuthParams) {
				t.Fatalf("extra auth params = %#v", c.ExtraAuthParams)
			}
			if c.AccessToken == "" || c.AccessToken == "stored-access" || c.RefreshToken != tt.wantRefresh {
				t.Fatalf("tokens = access %q refresh %q", c.AccessToken, c.RefreshToken)
			}
			if c.ClaudeAccountUUID != tt.wantAccount || c.AccountID != tt.wantAccount || c.ClaudeOrgUUID != tt.wantOrg || c.OrganizationID != tt.wantOrg || c.Email != tt.wantEmail || c.ClaudeScopes != tt.wantScopes {
				t.Fatalf("grant identity = account %q org %q email %q scopes %q", c.ClaudeAccountUUID, c.ClaudeOrgUUID, c.Email, c.ClaudeScopes)
			}
			if tt.keepDevice {
				if c.ClaudeDeviceID != device || c.IDToken != "stored-id-token" || c.ClaudeRefreshExpiresAt != 99 {
					t.Fatalf("same grant changed device/id token/expiry: %+v", c)
				}
			} else {
				if !isHexDevice(c.ClaudeDeviceID) || c.ClaudeDeviceID == device || c.IDToken != "" {
					t.Fatalf("replacement borrowed device or id token: %+v", c)
				}
				if tt.wantExpiry {
					if c.ClaudeRefreshExpiresAt == 0 || c.ClaudeRefreshExpiresAt == 99 {
						t.Fatalf("confirmed refresh expiry was not applied: %d", c.ClaudeRefreshExpiresAt)
					}
				} else if c.ClaudeRefreshExpiresAt != 0 {
					t.Fatalf("omitted refresh expiry borrowed old value: %d", c.ClaudeRefreshExpiresAt)
				}
			}
		})
	}
}

func TestSavedClaudeRefreshCredsCopiesSource(t *testing.T) {
	device := strings.Repeat("ab", 32)
	original := domain.OAuthCreds{
		Mode:                   domain.OAuthManual,
		Preset:                 "custom",
		ClaudeCodeAuth:         true,
		AccessToken:            "stored-access",
		RefreshToken:           "stored-refresh",
		ExpiresAt:              11,
		Email:                  "stored@example.com",
		IDToken:                "stored-id-token",
		AccountID:              "acct-stored",
		OrganizationID:         "org-stored",
		AuthURL:                "https://stored.example/authorize",
		TokenURL:               "https://stored.example/token",
		ClientID:               "stored-client",
		ClientSecret:           "stored-secret",
		Scopes:                 "stored-configured-scope",
		RedirectURI:            "http://127.0.0.1:9/stored",
		PKCE:                   false,
		ExtraAuthParams:        map[string]string{"legacy": "1"},
		RefreshJSON:            false,
		RefreshURL:             "https://stored.example/refresh",
		ClaudeAccountUUID:      "acct-stored",
		ClaudeOrgUUID:          "org-stored",
		ClaudeDeviceID:         device,
		ClaudeScopes:           "user:inference",
		ClaudeRefreshExpiresAt: 99,
	}
	tests := []struct {
		name    string
		pasted  string
		replace bool
	}{
		{name: "no pasted token"},
		{name: "same trimmed token", pasted: "  stored-refresh  "},
		{name: "replacement token", pasted: "replacement-refresh", replace: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := original
			source.ExtraAuthParams = map[string]string{"legacy": "1"}
			want := source
			want.ExtraAuthParams = map[string]string{"legacy": "1"}
			form := url.Values{}
			if tt.pasted != "" {
				form.Set("refresh_token", tt.pasted)
			}
			got := savedClaudeRefreshCreds(&source, formRequest(form))
			if got == &source {
				t.Fatal("returned source pointer")
			}
			if !reflect.DeepEqual(source, want) {
				t.Fatal("source mutated")
			}
			wantNext := want
			if tt.replace {
				wantNext.RefreshToken = "replacement-refresh"
				wantNext.AccessToken = ""
				wantNext.ExpiresAt = 0
				wantNext.Email = ""
				wantNext.IDToken = ""
				wantNext.AccountID = ""
				wantNext.OrganizationID = ""
				wantNext.ClaudeAccountUUID = ""
				wantNext.ClaudeOrgUUID = ""
				wantNext.ClaudeDeviceID = ""
				wantNext.ClaudeScopes = ""
				wantNext.ClaudeRefreshExpiresAt = 0
			}
			if !reflect.DeepEqual(*got, wantNext) {
				t.Fatalf("copy = %+v", got)
			}
			if got.ExtraAuthParams == nil || reflect.ValueOf(got.ExtraAuthParams).Pointer() == reflect.ValueOf(source.ExtraAuthParams).Pointer() {
				t.Fatal("extra auth params alias source")
			}
			got.ExtraAuthParams["legacy"] = "changed"
			if source.ExtraAuthParams["legacy"] != "1" || !reflect.DeepEqual(source, want) {
				t.Fatal("returned map write mutated source")
			}
		})
	}
}

func isHexDevice(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func TestDashboardRefreshLosesToReconnect(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"obsolete-access","expires_in":3600}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "")))
		done <- rec
	}()
	waitSignal(t, entered)

	reconnect := *stored
	reconnect.AccessToken = "reconnect-access"
	reconnect.RefreshToken = "reconnect-refresh"
	reconnect.ClaudeAccountUUID = "reconnect-account"
	reconnect.AccountID = "reconnect-account"
	reconnect.ClaudeDeviceID = strings.Repeat("cd", 32)
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &reconnect); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "credentials changed during refresh") || strings.Contains(body, "refreshed") {
		t.Fatalf("status %d body %s", rec.Code, body)
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.AccessToken != "reconnect-access" || saved.OAuthCreds.RefreshToken != "reconnect-refresh" || saved.OAuthCreds.ClaudeAccountUUID != "reconnect-account" {
		t.Fatalf("refresh overwrote reconnect: %+v", saved.OAuthCreds)
	}
}

func TestDashboardRefreshLosesToSameTokenRotation(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"obsolete-access","expires_in":60}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "")))
		done <- rec
	}()
	waitSignal(t, entered)

	winner := *stored
	winner.AccessToken = "winner-access"
	winner.ExpiresAt = 4242
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &winner); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	if !strings.Contains(rec.Body.String(), "credentials changed during refresh") {
		t.Fatalf("same-token race body %s", rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.AccessToken != "winner-access" || saved.OAuthCreds.ExpiresAt != 4242 {
		t.Fatalf("winning rotation lost: %+v", saved.OAuthCreds)
	}
	if saved.OAuthCreds.RefreshToken != stored.RefreshToken || saved.OAuthCreds.ClaudeDeviceID != stored.ClaudeDeviceID {
		t.Fatalf("same-token rotation changed identity: %+v", saved.OAuthCreds)
	}
}

func TestDashboardReplacementConflictsDuringCall(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"replacement-access","expires_in":3600}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "replacement-refresh")))
		done <- rec
	}()
	waitSignal(t, entered)
	reconnect := *stored
	reconnect.AccessToken = "reconnect-access"
	reconnect.RefreshToken = "reconnect-refresh"
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &reconnect); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	if !strings.Contains(rec.Body.String(), "credentials changed during refresh") || strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("replacement conflict body %s", rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.AccessToken != "reconnect-access" || saved.OAuthCreds.RefreshToken != "reconnect-refresh" || saved.OAuthCreds.ClaudeAccountUUID != stored.ClaudeAccountUUID {
		t.Fatalf("replacement overwrote reconnect: %+v", saved.OAuthCreds)
	}
}

func TestDashboardMetadataSaveKeepsRotatedCredentials(t *testing.T) {
	h := testHandler(t)
	stored := dashboardClaudeCreds("https://stored.example/token")
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cur, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated := *stored
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 77
	rotated.ClaudeDeviceID = strings.Repeat("ef", 32)
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "renamed-after-rotation")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://renamed.example")
	form.Set("tags", "kept")
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, form), cur, domain.ProtocolClaudeCode, "stale-handler-snapshot")
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata save status %d body %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "renamed-after-rotation" || saved.BaseURL != "https://renamed.example" {
		t.Fatalf("metadata not saved: %+v", saved)
	}
	if saved.OAuthCreds.AccessToken != "rotated-access" || saved.OAuthCreds.ExpiresAt != 77 || saved.OAuthCreds.ClaudeDeviceID != rotated.ClaudeDeviceID {
		t.Fatalf("metadata save restored stale credentials: %+v", saved.OAuthCreds)
	}
}

func TestDashboardReconnectSaveConflictKeepsSession(t *testing.T) {
	h := testHandler(t)
	stored := dashboardClaudeCreds("https://stored.example/token")
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Same account as the stored row but a fresh exchange-generated device:
	// preserveClaudeIdentity replaces that device with the stored one during
	// the save, so a lost session clone would corrupt the retained session.
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		TokenURL: stored.TokenURL, ClientID: stored.ClientID, ClaudeAccountUUID: "acct-stored",
		AccountID:      "acct-stored",
		ClaudeDeviceID: strings.Repeat("11", 32),
	}
	sessionOriginal := &domain.OAuthCreds{}
	*sessionOriginal = *sessionCreds
	conn := &fixedConnect{state: "state-1", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "should-not-save")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://should-not.example")
	form.Set("oauth_session", conn.State())

	rotated := *stored
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 88
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, form), before.Provider, domain.ProtocolClaudeCode, before.OAuthBlob)
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("conflict body %s", rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "cc" || saved.BaseURL != "https://api.anthropic.com" || saved.OAuthCreds.AccessToken != "rotated-access" || saved.OAuthCreds.ExpiresAt != 88 {
		t.Fatalf("conflict wrote partial row: %+v creds %+v", saved, saved.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); !ok {
		t.Fatal("conflict dropped connect session")
	}
	held, _, _ := conn.Result()
	if !reflect.DeepEqual(held, sessionOriginal) {
		t.Fatalf("conflict mutated session credentials: held %+v original %+v", held, sessionOriginal)
	}

	fresh, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	h.updateOAuthProvider(retry, postForm(p.ID, form), fresh.Provider, domain.ProtocolClaudeCode, fresh.OAuthBlob)
	if retry.Code != http.StatusOK || strings.Contains(retry.Body.String(), "credentials changed") {
		t.Fatalf("retry status %d body %s", retry.Code, retry.Body.String())
	}
	saved, err = h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "should-not-save" || saved.OAuthCreds.AccessToken != "session-access" || saved.OAuthCreds.RefreshToken != "session-refresh" {
		t.Fatalf("retry did not consume session: %+v creds %+v", saved, saved.OAuthCreds)
	}
	// The same-account merge replaced the exchange-generated device with the
	// stored device on the saved copy.
	if saved.OAuthCreds.ClaudeDeviceID != stored.ClaudeDeviceID {
		t.Fatalf("retry saved generated device: %+v", saved.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); ok {
		t.Fatal("successful retry kept connect session")
	}
}

func TestDashboardImportSaveConflictLeavesRow(t *testing.T) {
	h := testHandler(t)
	stored := dashboardClaudeCreds("https://stored.example/token")
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "imported-name")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://imported.example")
	form.Set("preset", "custom")
	form.Set("token_url", stored.TokenURL)
	form.Set("client_id", stored.ClientID)
	form.Set("access_token", "imported-access")
	form.Set("refresh_token", "imported-refresh")
	rotated := *stored
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 99
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, form), before.Provider, domain.ProtocolClaudeCode, before.OAuthBlob)
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("import conflict body %s", rec.Body.String())
	}
	saved, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "cc" || saved.BaseURL != p.BaseURL || saved.OAuthCreds.AccessToken != "rotated-access" || saved.OAuthCreds.ExpiresAt != 99 || saved.OAuthCreds.RefreshToken != stored.RefreshToken {
		t.Fatalf("import conflict changed row: %+v creds %+v", saved, saved.OAuthCreds)
	}
}

func TestDashboardSaveDeletedAndAuthSwitch(t *testing.T) {
	h := testHandler(t)
	p := dashboardClaudeProvider(dashboardClaudeCreds("https://stored.example/token"))
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cur, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteProvider(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "gone")
	form.Set("protocol", string(domain.ProtocolClaudeCode))
	form.Set("base_url", "https://gone.example")
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, form), cur, domain.ProtocolClaudeCode, "deleted-basis")
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("deleted save body %s", rec.Body.String())
	}

	p = dashboardClaudeProvider(dashboardClaudeCreds("https://stored.example/token"))
	p.Name = "switch"
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cur, err = h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	switched := *cur
	switched.AuthMethod = domain.AuthAPIKey
	switched.Protocol = domain.ProtocolOpenAI
	switched.APIKey = "static"
	switched.OAuthCreds = nil
	if err := h.store.UpdateProvider(context.Background(), &switched); err != nil {
		t.Fatal(err)
	}
	form.Set("name", "must-not-mix")
	rec = httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, form), cur, domain.ProtocolClaudeCode, "old")
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("auth switch body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "switch" || got.AuthMethod != domain.AuthAPIKey || got.Protocol != domain.ProtocolOpenAI || got.APIKey != "static" || got.OAuthCreds != nil {
		t.Fatalf("auth switch mixed stale metadata: %+v", got)
	}
}

func dashboardClaudeProvider(creds *domain.OAuthCreds) *domain.Provider {
	return &domain.Provider{
		Name: "cc", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, OAuthCreds: creds,
	}
}

func dashboardClaudeCreds(tokenURL string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthManual, Preset: "custom", ClaudeCodeAuth: true,
		AccessToken: "stored-access", RefreshToken: "stored-refresh", ExpiresAt: 11,
		Email: "stored@example.com", TokenURL: tokenURL, ClientID: "stored-client",
		ClaudeAccountUUID: "acct-stored", AccountID: "acct-stored", ClaudeOrgUUID: "org-stored",
		ClaudeDeviceID: strings.Repeat("ab", 32), ClaudeScopes: "user:inference", ClaudeRefreshExpiresAt: 99,
	}
}

func dashboardRefreshForm(id int64, pasted string) url.Values {
	form := url.Values{}
	form.Set("id", strconv.FormatInt(id, 10))
	form.Set("preset", "claude")
	form.Set("refresh_view", "status")
	form.Set("token_url", "https://preset.example/token")
	form.Set("client_id", "preset-client")
	if pasted != "" {
		form.Set("refresh_token", pasted)
	}
	return form
}

func blockingJSONTokenServer(t *testing.T, entered chan<- struct{}, release <-chan struct{}, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		case <-time.After(5 * time.Second):
			http.Error(w, "entered stalled", http.StatusInternalServerError)
			return
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			http.Error(w, "release stalled", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for token endpoint")
	}
}

type fixedConnect struct {
	state string
	creds *domain.OAuthCreds
}

func (f *fixedConnect) State() string { return f.state }

func (f *fixedConnect) Result() (*domain.OAuthCreds, error, bool) {
	return f.creds, nil, true
}

func (f *fixedConnect) Close() error { return nil }

func anthropicOAuthCreds() *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthManual, Preset: "custom",
		AccessToken: "anthropic-access", RefreshToken: "anthropic-refresh", ExpiresAt: 5,
		Email: "old@example.com", TokenURL: "https://old.example/token", ClientID: "old-client",
		AccountID: "acct-old",
	}
}

func anthropicOAuthProvider() *domain.Provider {
	return &domain.Provider{
		Name: "p", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolAnthropic,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: anthropicOAuthCreds(),
	}
}

func providerForm(proto domain.Protocol, session string) url.Values {
	form := url.Values{}
	form.Set("auth_method", "oauth")
	form.Set("name", "renamed")
	form.Set("protocol", string(proto))
	form.Set("base_url", "https://api.anthropic.com")
	if session != "" {
		form.Set("oauth_session", session)
	}
	return form
}

func TestDashboardTransitionIntoClaudeMetadata(t *testing.T) {
	h := testHandler(t)
	p := anthropicOAuthProvider()
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// A rotation that commits before the save must be preserved: a metadata
	// transition keeps the current credentials instead of replaying the
	// snapshot the edit form was rendered from.
	rotated := *p.OAuthCreds
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 66
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, "")))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("metadata transition status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolClaudeCode || got.Name != "renamed" {
		t.Fatalf("metadata transition not saved: %+v", got)
	}
	if got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.ExpiresAt != 66 {
		t.Fatalf("metadata transition replayed stale credentials: %+v", got.OAuthCreds)
	}
}

func TestDashboardTransitionIntoClaudeMetadataConflict(t *testing.T) {
	h := testHandler(t)
	p := anthropicOAuthProvider()
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The row leaves the observed connection while the form is open.
	switched := *before.Provider
	switched.AuthMethod = domain.AuthAPIKey
	switched.Protocol = domain.ProtocolOpenAI
	switched.APIKey = "static"
	switched.OAuthCreds = nil
	if err := h.store.UpdateProvider(context.Background(), &switched); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, "")), before.Provider, domain.ProtocolClaudeCode, before.OAuthBlob)
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("metadata transition conflict body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthAPIKey || got.Protocol != domain.ProtocolOpenAI || got.APIKey != "static" || got.OAuthCreds != nil {
		t.Fatalf("conflict mutated switched row: %+v", got)
	}
}

func TestDashboardTransitionIntoClaudeReconnect(t *testing.T) {
	h := testHandler(t)
	p := anthropicOAuthProvider()
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
		ClaudeAccountUUID: "acct-new", AccountID: "acct-new", ClaudeDeviceID: strings.Repeat("11", 32),
	}
	conn := &fixedConnect{state: "state-into", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("reconnect transition status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolClaudeCode || got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "session-access" || got.OAuthCreds.RefreshToken != "session-refresh" {
		t.Fatalf("reconnect transition not saved: %+v creds %+v", got, got.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); ok {
		t.Fatal("successful reconnect kept the session")
	}
}

func TestDashboardTransitionIntoClaudeReconnectConflict(t *testing.T) {
	h := testHandler(t)
	p := anthropicOAuthProvider()
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
		ClaudeAccountUUID: "acct-new", AccountID: "acct-new", ClaudeDeviceID: strings.Repeat("11", 32),
	}
	conn := &fixedConnect{state: "state-into-conflict", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	// A rotation on the old connection commits before the save. The observed
	// credential basis is stale, so the Claude reconnect must conflict instead
	// of replacing the rotated grant.
	rotated := *p.OAuthCreds
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 77
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())), before.Provider, domain.ProtocolClaudeCode, before.OAuthBlob)
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("reconnect transition conflict body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolAnthropic || got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.ExpiresAt != 77 {
		t.Fatalf("conflict wrote partial transition: %+v creds %+v", got, got.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); !ok {
		t.Fatal("conflict dropped connect session")
	}
	held, _, _ := conn.Result()
	if held.AccessToken != "session-access" || held.ClaudeDeviceID != sessionCreds.ClaudeDeviceID {
		t.Fatalf("conflict mutated session credentials: %+v", held)
	}
}

func TestDashboardTransitionOutOfClaudeKeepsRotation(t *testing.T) {
	h := testHandler(t)
	stored := dashboardClaudeCreds("https://stored.example/token")
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cur, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A rotation commits between the handler snapshot and the save. A
	// whole-row write would restore the stale snapshot; the guarded metadata
	// write must keep the rotated credentials and still switch the protocol.
	rotated := *stored
	rotated.AccessToken = "rotated-access"
	rotated.ExpiresAt = 77
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &rotated); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, providerForm(domain.ProtocolAnthropic, "")), cur, domain.ProtocolAnthropic, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("transition out status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolAnthropic || got.Name != "renamed" {
		t.Fatalf("transition out not saved: %+v", got)
	}
	if got.OAuthCreds.AccessToken != "rotated-access" || got.OAuthCreds.ExpiresAt != 77 {
		t.Fatalf("transition out restored stale credentials: %+v", got.OAuthCreds)
	}
}

func TestDashboardTransitionOutOfClaudeConflict(t *testing.T) {
	h := testHandler(t)
	stored := dashboardClaudeCreds("https://stored.example/token")
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	cur, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	switched := *cur
	switched.AuthMethod = domain.AuthAPIKey
	switched.Protocol = domain.ProtocolOpenAI
	switched.APIKey = "static"
	switched.OAuthCreds = nil
	if err := h.store.UpdateProvider(context.Background(), &switched); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, providerForm(domain.ProtocolAnthropic, "")), cur, domain.ProtocolAnthropic, "")
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("transition out conflict body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthAPIKey || got.Protocol != domain.ProtocolOpenAI || got.APIKey != "static" || got.OAuthCreds != nil {
		t.Fatalf("conflict mutated switched row: %+v", got)
	}
}

func TestDashboardRefreshConflictsOnProtocolSwitch(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"rotated-after-switch","expires_in":3600}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "")))
		done <- rec
	}()
	waitSignal(t, entered)

	live, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	live.Protocol = domain.ProtocolAnthropic
	if err := h.store.UpdateProvider(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	if !strings.Contains(rec.Body.String(), "credentials changed during refresh") || strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("protocol switch refresh body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolAnthropic || got.OAuthCreds.AccessToken != "stored-access" {
		t.Fatalf("refresh wrote into switched row: %+v creds %+v", got, got.OAuthCreds)
	}
}

func TestDashboardRefreshConflictsOnDeletion(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"rotated-after-delete","expires_in":3600}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "")))
		done <- rec
	}()
	waitSignal(t, entered)

	if err := h.store.DeleteProvider(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	if !strings.Contains(rec.Body.String(), "credentials changed during refresh") || strings.Contains(rec.Body.String(), "not found") || strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("deleted row refresh body %s", rec.Body.String())
	}
}

func TestDashboardRefreshConflictsOnAuthSwitch(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"rotated-after-switch","expires_in":3600}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "")))
		done <- rec
	}()
	waitSignal(t, entered)

	live, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	live.AuthMethod = domain.AuthAPIKey
	live.Protocol = domain.ProtocolOpenAI
	live.APIKey = "static"
	live.OAuthCreds = nil
	if err := h.store.UpdateProvider(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	if !strings.Contains(rec.Body.String(), "credentials changed during refresh") || strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("auth switch refresh body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthAPIKey || got.Protocol != domain.ProtocolOpenAI || got.OAuthCreds != nil {
		t.Fatalf("refresh wrote into auth-switched row: %+v", got)
	}
}

func TestDashboardRefreshConflictsOnConfigChange(t *testing.T) {
	h := testHandler(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := blockingJSONTokenServer(t, entered, release, `{"access_token":"rotated-after-config","expires_in":3600}`)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)
	stored := dashboardClaudeCreds(allowed)
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.oauthRefreshTokens(rec, reqWithForm(dashboardRefreshForm(p.ID, "")))
		done <- rec
	}()
	waitSignal(t, entered)

	changed := *stored
	changed.TokenURL = "https://changed.example/token"
	if err := h.store.UpdateProviderOAuth(context.Background(), p.ID, &changed); err != nil {
		t.Fatal(err)
	}
	close(release)
	rec := <-done
	if !strings.Contains(rec.Body.String(), "credentials changed during refresh") || strings.Contains(rec.Body.String(), "refreshed") {
		t.Fatalf("config change refresh body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds.TokenURL != "https://changed.example/token" || got.OAuthCreds.AccessToken != "stored-access" {
		t.Fatalf("refresh overwrote changed config: %+v", got.OAuthCreds)
	}
}

func TestDashboardNilCredClaudeRowRepair(t *testing.T) {
	h := testHandler(t)
	p := dashboardClaudeProvider(nil)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	// A metadata-only save has no credentials to keep and still requires an
	// explicit reconnect or import, as before.
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, "")))
	if !strings.Contains(rec.Body.String(), "connect this provider or paste an access/refresh token") {
		t.Fatalf("metadata-only repair body %s", rec.Body.String())
	}
	empty, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if empty.OAuthCreds != nil || empty.Name != "cc" {
		t.Fatalf("metadata-only repair changed the row: %+v", empty)
	}

	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
		ClaudeAccountUUID: "acct-new", AccountID: "acct-new", ClaudeDeviceID: strings.Repeat("11", 32),
	}
	conn := &fixedConnect{state: "state-repair", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	rec = httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("reconnect repair status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "session-access" || got.OAuthCreds.RefreshToken != "session-refresh" {
		t.Fatalf("nil credential row not repaired: %+v creds %+v", got, got.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); ok {
		t.Fatal("successful repair kept the session")
	}
}

func TestDashboardAPIKeyToClaudeReconnect(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "legacy", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolAnthropic,
		AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthXAPIKey, APIKey: "static-key",
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
		ClaudeAccountUUID: "acct-new", AccountID: "acct-new", ClaudeDeviceID: strings.Repeat("11", 32),
	}
	conn := &fixedConnect{state: "state-apikey", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("apikey transition status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthOAuth || got.Protocol != domain.ProtocolClaudeCode || got.APIKey != "" {
		t.Fatalf("apikey transition not saved: %+v", got)
	}
	if got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "session-access" {
		t.Fatalf("apikey transition lost credentials: %+v", got.OAuthCreds)
	}
}

func TestDashboardTransitionOutOfClaudeReconnect(t *testing.T) {
	h := testHandler(t)
	stored := dashboardClaudeCreds("https://stored.example/token")
	p := dashboardClaudeProvider(stored)
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		Mode: domain.OAuthManual, Preset: "custom", AccessToken: "session-access",
		RefreshToken: "session-refresh", ExpiresAt: 42, TokenURL: "https://new.example/token",
		ClientID: "new-client",
	}
	conn := &fixedConnect{state: "state-out", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolAnthropic, conn.State())))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("reconnect transition out status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolAnthropic || got.OAuthCreds.AccessToken != "session-access" || got.OAuthCreds.RefreshToken != "session-refresh" {
		t.Fatalf("reconnect transition out not saved: %+v creds %+v", got, got.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); ok {
		t.Fatal("successful reconnect kept the session")
	}
}

func TestDashboardLegacyBlankAPIKeyToClaudeReconnect(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "legacy", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolAnthropic,
		AuthMethod: "", AuthScheme: "", APIKey: "static-key",
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
		ClaudeAccountUUID: "acct-new", AccountID: "acct-new", ClaudeDeviceID: strings.Repeat("11", 32),
	}
	conn := &fixedConnect{state: "state-legacy-blank", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("legacy blank transition status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthOAuth || got.Protocol != domain.ProtocolClaudeCode || got.APIKey != "" {
		t.Fatalf("legacy blank transition not saved: %+v", got)
	}
	if got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "session-access" {
		t.Fatalf("legacy blank transition lost credentials: %+v", got.OAuthCreds)
	}
	if _, ok := h.sessions.get(conn.State()); ok {
		t.Fatal("successful legacy blank transition kept the session")
	}
}

func TestDashboardLegacyBlankMetadataTransitionIntoClaude(t *testing.T) {
	h := testHandler(t)
	p := anthropicOAuthProvider()
	p.AuthMethod = ""
	p.AuthScheme = ""
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, "")))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("legacy blank metadata transition status %d body %s", rec.Code, rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthOAuth || got.Protocol != domain.ProtocolClaudeCode || got.Name != "renamed" {
		t.Fatalf("legacy blank metadata transition not saved: %+v", got)
	}
	if got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "anthropic-access" || got.OAuthCreds.RefreshToken != "anthropic-refresh" {
		t.Fatalf("legacy blank metadata transition changed credentials: %+v", got.OAuthCreds)
	}
}

func TestDashboardLegacyBlankReconnectConflictKeepsSession(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "legacy", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolAnthropic,
		AuthMethod: "", AuthScheme: "", APIKey: "static-key",
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
		ClaudeAccountUUID: "acct-new", AccountID: "acct-new", ClaudeDeviceID: strings.Repeat("11", 32),
	}
	conn := &fixedConnect{state: "state-legacy-conflict", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	// The row changes connection between the form read and the save.
	switched := *before.Provider
	switched.Protocol = domain.ProtocolOpenAI
	switched.AuthMethod = domain.AuthOAuth
	switched.AuthScheme = domain.AuthBearer
	switched.APIKey = ""
	switched.OAuthCreds = &domain.OAuthCreds{AccessToken: "winner-access", RefreshToken: "winner-refresh"}
	if err := h.store.UpdateProvider(context.Background(), &switched); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())), before.Provider, domain.ProtocolClaudeCode, before.OAuthBlob)
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("legacy blank conflict body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthOAuth || got.Protocol != domain.ProtocolOpenAI || got.OAuthCreds == nil || got.OAuthCreds.AccessToken != "winner-access" {
		t.Fatalf("legacy blank conflict mutated switched row: %+v", got)
	}
	if _, ok := h.sessions.get(conn.State()); !ok {
		t.Fatal("legacy blank conflict dropped connect session")
	}
	held, _, _ := conn.Result()
	if held.AccessToken != "session-access" || held.RefreshToken != "session-refresh" {
		t.Fatalf("legacy blank conflict mutated session credentials: %+v", held)
	}
}

func webUnsignedIDToken(email, account string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `","account_id":"` + account + `"}`))
	return header + "." + payload + "."
}

func TestDashboardGenericOAuthRefreshUnderClaudeProtocol(t *testing.T) {
	tests := []struct {
		name        string
		jsonWire    bool
		replacement bool
		response    string
	}{
		{
			name:     "form wire",
			response: `{"access_token":"form-access","refresh_token":"form-refresh","expires_in":3600,"id_token":"` + webUnsignedIDToken("form@example.com", "acct-form") + `"}`,
		},
		{
			name:     "JSON wire",
			jsonWire: true,
			response: `{"access_token":"json-access","refresh_token":"json-refresh","expires_in":3600,"id_token":"` + webUnsignedIDToken("json@example.com", "acct-json") + `"}`,
		},
		{
			name:        "replacement unknown grant",
			replacement: true,
			response:    `{"access_token":"replacement-access","refresh_token":"replacement-refresh","expires_in":3600}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := testHandler(t)
			var sawContentType string
			var sawForm url.Values
			var sawJSON map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawContentType = r.Header.Get("Content-Type")
				raw, _ := io.ReadAll(r.Body)
				if tt.jsonWire {
					_ = json.Unmarshal(raw, &sawJSON)
				} else {
					sawForm, _ = url.ParseQuery(string(raw))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.response)
			}))
			t.Cleanup(srv.Close)
			allowed := srv.URL + "/oauth/token"
			withClaudeRefreshTransport(t, allowed)

			stored := &domain.OAuthCreds{
				Mode: domain.OAuthManual, Preset: "custom",
				AccessToken: "stored-access", RefreshToken: "stored-refresh", ExpiresAt: 11,
				Email: "stored@example.com", IDToken: "stored-id-token", AccountID: "acct-stored",
				OrganizationID: "org-stored",
				AuthURL:        "https://stored.example/authorize",
				TokenURL:       allowed, ClientID: "stored-client", ClientSecret: "stored-secret",
				Scopes: "configured-scope", RedirectURI: "http://127.0.0.1:9/stored",
				ExtraAuthParams: map[string]string{"legacy": "1"},
				RefreshJSON:     tt.jsonWire, RefreshURL: "https://stored.example/refresh",
			}
			p := dashboardClaudeProvider(stored)
			if err := h.store.CreateProvider(context.Background(), p); err != nil {
				t.Fatal(err)
			}

			form := dashboardRefreshForm(p.ID, "")
			if tt.replacement {
				form.Set("refresh_token", "replacement-grant")
			}
			rec := httptest.NewRecorder()
			h.oauthRefreshTokens(rec, reqWithForm(form))
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "refresh failed") || strings.Contains(rec.Body.String(), "does not support") {
				t.Fatalf("refresh status %d body %s", rec.Code, rec.Body.String())
			}
			if tt.jsonWire {
				if sawContentType != "application/json" || sawJSON["grant_type"] != "refresh_token" || sawJSON["client_id"] != "stored-client" || sawJSON["refresh_token"] != "stored-refresh" {
					t.Fatalf("JSON wire = %q %#v", sawContentType, sawJSON)
				}
				if _, ok := sawJSON["client_secret"]; ok {
					t.Fatalf("generic JSON refresh sent client_secret: %#v", sawJSON)
				}
			} else {
				if sawContentType != "application/x-www-form-urlencoded" || sawForm.Get("grant_type") != "refresh_token" || sawForm.Get("client_id") != "stored-client" || sawForm.Get("client_secret") != "stored-secret" {
					t.Fatalf("form wire = %q %#v", sawContentType, sawForm)
				}
				wantRefresh := "stored-refresh"
				if tt.replacement {
					wantRefresh = "replacement-grant"
				}
				if sawForm.Get("refresh_token") != wantRefresh {
					t.Fatalf("form refresh_token = %q", sawForm.Get("refresh_token"))
				}
				if _, ok := sawForm["scope"]; ok {
					t.Fatalf("generic refresh sent scope: %#v", sawForm)
				}
			}

			saved, err := h.store.GetProvider(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			c := saved.OAuthCreds
			if c.ClaudeCodeAuth || c.ClaudeDeviceID != "" || c.ClaudeScopes != "" || c.ClaudeRefreshExpiresAt != 0 {
				t.Fatalf("generic refresh invented Claude state: %+v", c)
			}
			if c.TokenURL != stored.TokenURL || c.ClientID != stored.ClientID || c.ClientSecret != stored.ClientSecret || c.Scopes != stored.Scopes || c.RefreshJSON != stored.RefreshJSON || c.RefreshURL != stored.RefreshURL || c.Mode != stored.Mode || c.Preset != stored.Preset || c.AuthURL != stored.AuthURL || c.RedirectURI != stored.RedirectURI {
				t.Fatalf("generic refresh changed config: %+v", c)
			}
			if !reflect.DeepEqual(c.ExtraAuthParams, stored.ExtraAuthParams) {
				t.Fatalf("extra auth params = %#v", c.ExtraAuthParams)
			}
			if c.AccessToken == "" || c.AccessToken == "stored-access" || c.ExpiresAt == 0 {
				t.Fatalf("tokens were not rotated: %+v", c)
			}
			if tt.replacement {
				if c.IDToken != "" || c.Email != "" || c.AccountID != "" || c.OrganizationID != "" {
					t.Fatalf("replacement inherited old identity: %+v", c)
				}
			} else if c.IDToken == "" || c.IDToken == "stored-id-token" {
				t.Fatalf("generic refresh dropped returned id_token: %+v", c)
			}
		})
	}
}

func TestDashboardGenericRefreshAfterMetadataTransitionIntoClaude(t *testing.T) {
	h := testHandler(t)
	var sawRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sawRefresh = r.PostForm.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"transition-access","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	allowed := srv.URL + "/oauth/token"
	withClaudeRefreshTransport(t, allowed)

	p := anthropicOAuthProvider()
	p.OAuthCreds.TokenURL = allowed
	p.OAuthCreds.ExpiresAt = 11
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, "")))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "credentials changed") {
		t.Fatalf("metadata transition status %d body %s", rec.Code, rec.Body.String())
	}
	form := dashboardRefreshForm(p.ID, "")
	rec = httptest.NewRecorder()
	h.oauthRefreshTokens(rec, reqWithForm(form))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "refresh failed") {
		t.Fatalf("transition refresh status %d body %s", rec.Code, rec.Body.String())
	}
	if sawRefresh != "anthropic-refresh" {
		t.Fatalf("transition refresh sent %q", sawRefresh)
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != domain.ProtocolClaudeCode || got.OAuthCreds.ClaudeCodeAuth || got.OAuthCreds.AccessToken != "transition-access" {
		t.Fatalf("transition refresh result: %+v creds %+v", got, got.OAuthCreds)
	}
}

func TestDashboardLegacyBlankRawAuthChangeStillConflicts(t *testing.T) {
	h := testHandler(t)
	p := &domain.Provider{
		Name: "legacy", BaseURL: "https://api.anthropic.com", Protocol: domain.ProtocolAnthropic,
		AuthMethod: "", AuthScheme: "", APIKey: "static-key",
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	before, err := h.store.GetProviderOAuthStored(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionCreds := &domain.OAuthCreds{
		ClaudeCodeAuth: true, AccessToken: "session-access", RefreshToken: "session-refresh",
		ExpiresAt: 99, TokenURL: "https://new.example/token", ClientID: "new-client",
	}
	conn := &fixedConnect{state: "state-raw-auth-conflict", creds: sessionCreds}
	h.sessions.put(conn.State(), &connectSession{conn: conn, created: time.Now()}, time.Now())
	// Effective method stays APIKey, but the raw column changes from empty to
	// explicit. The guarded write must compare the stored raw value, not the
	// normalized effective method.
	switched := *before.Provider
	switched.AuthMethod = domain.AuthAPIKey
	if err := h.store.UpdateProvider(context.Background(), &switched); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.updateOAuthProvider(rec, postForm(p.ID, providerForm(domain.ProtocolClaudeCode, conn.State())), before.Provider, domain.ProtocolClaudeCode, before.OAuthBlob)
	if !strings.Contains(rec.Body.String(), "credentials changed during save") {
		t.Fatalf("raw auth conflict body %s", rec.Body.String())
	}
	got, err := h.store.GetProvider(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthMethod != domain.AuthAPIKey || got.Protocol != domain.ProtocolAnthropic || got.Name != "legacy" {
		t.Fatalf("raw auth conflict wrote partial transition: %+v", got)
	}
	if _, ok := h.sessions.get(conn.State()); !ok {
		t.Fatal("raw auth conflict dropped connect session")
	}
}

func TestDashboardClaudeAPIKeyRefreshDoesNotRotate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		method domain.AuthMethod
	}{
		{name: "explicit apikey", method: domain.AuthAPIKey},
		{name: "legacy blank", method: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			h := testHandler(t)
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"should-not-exist","expires_in":3600}`)
			}))
			t.Cleanup(srv.Close)
			stored := &domain.OAuthCreds{
				TokenURL: srv.URL + "/oauth/token", ClientID: "cid", RefreshToken: "stored-refresh",
				AccessToken: "stored-access",
			}
			p := dashboardClaudeProvider(stored)
			p.AuthMethod = method
			p.AuthScheme = domain.AuthXAPIKey
			p.APIKey = "static-key"
			if err := h.store.CreateProvider(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			form := dashboardRefreshForm(p.ID, "")
			rec := httptest.NewRecorder()
			h.oauthRefreshTokens(rec, reqWithForm(form))
			if !strings.Contains(rec.Body.String(), "does not use OAuth") {
				t.Fatalf("API-key refresh body %s", rec.Body.String())
			}
			if hits != 0 {
				t.Fatalf("API-key refresh hit the token endpoint %d times", hits)
			}
			got, err := h.store.GetProvider(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.AuthMethod != method || got.OAuthCreds.AccessToken != "stored-access" {
				t.Fatalf("API-key refresh changed row: %+v", got)
			}
		})
	}
}
