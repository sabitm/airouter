package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
