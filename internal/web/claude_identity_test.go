package web

import (
	"context"
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
