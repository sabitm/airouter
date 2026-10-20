package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
)

func TestClaudeExchangeRejectsStateOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("token endpoint must not be called on state mismatch")
	}))
	defer srv.Close()
	c := &domain.OAuthCreds{ClientID: "cid", TokenURL: srv.URL + "/token", RedirectURI: "http://localhost:1/callback", ClaudeCodeAuth: true}
	err := exchangeClaudeCode(context.Background(), c, "code#other-state", "verifier", "expected-state", srv.URL)
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeExchangePersistsAccountAndDevice(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"sk-ant-oat","refresh_token":"rt","expires_in":3600,"refresh_token_expires_in":7200,"scope":"user:inference user:profile","account":{"uuid":"acct-1","email_address":"a@example.com"},"organization":{"uuid":"org-1"}}`)
	}))
	defer srv.Close()
	c := &domain.OAuthCreds{ClientID: "cid", RedirectURI: "http://localhost:9/callback", ClaudeCodeAuth: true, TokenURL: srv.URL + "/token"}
	if err := exchangeClaudeCode(context.Background(), c, "code#expected", "ver", "expected", srv.URL); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "expected" || got["code"] != "code" || got["code_verifier"] != "ver" {
		t.Fatalf("body = %#v", got)
	}
	if c.ClaudeAccountUUID != "acct-1" || c.AccountID != "acct-1" || c.ClaudeOrgUUID != "org-1" {
		t.Fatalf("identity = %+v", c)
	}
	if c.Email != "a@example.com" || c.ClaudeScopes != "user:inference user:profile" {
		t.Fatalf("profile fields = %+v", c)
	}
	if c.ClaudeRefreshExpiresAt == 0 || !isDeviceID(c.ClaudeDeviceID) {
		t.Fatalf("expiry/device = %d %q", c.ClaudeRefreshExpiresAt, c.ClaudeDeviceID)
	}
	device := c.ClaudeDeviceID
	if err := refreshClaude(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.ClaudeDeviceID != device {
		t.Fatal("device id changed across refresh")
	}
}

func TestClaudeRefreshDoesNotExpandMissingScopes(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"access_token":"new","refresh_token":"rt2","expires_in":10}`)
	}))
	defer srv.Close()
	c := &domain.OAuthCreds{
		ClaudeCodeAuth: true, ClientID: "cid", TokenURL: srv.URL, RefreshToken: "rt",
		Scopes: "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins",
	}
	if err := refreshClaude(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["scope"]; ok {
		t.Fatalf("missing granted scopes were expanded: %#v", got)
	}
	if c.RefreshToken != "rt2" || c.AccessToken != "new" {
		t.Fatalf("rotation = %+v", c)
	}
}
