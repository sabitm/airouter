package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
)

func TestEnsureWorkosPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"raw-token", "workos:raw-token"},
		{"workos:already", "workos:already"},
		{"  workos:pad  ", "workos:pad"},
	}
	for _, tc := range cases {
		if got := EnsureWorkosPrefix(tc.in); got != tc.want {
			t.Errorf("EnsureWorkosPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewConnectAllowsEmptyClientIDForCline(t *testing.T) {
	creds := &domain.OAuthCreds{
		AuthURL:   "https://example.test/authorize",
		TokenURL:  "https://example.test/token",
		ClineAuth: true,
	}
	c, err := NewConnect(creds)
	if err != nil {
		t.Fatalf("NewConnect Cline: %v", err)
	}
	if c == nil {
		t.Fatal("nil connect")
	}
	// Non-Cline still requires client_id.
	if _, err := NewConnect(&domain.OAuthCreds{AuthURL: "a", TokenURL: "t"}); err == nil {
		t.Fatal("want error without client_id for non-Cline")
	}
}

func TestAuthorizeURLForCline(t *testing.T) {
	creds := &domain.OAuthCreds{
		AuthURL:     "https://api.cline.bot/api/v1/auth/authorize",
		TokenURL:    "https://api.cline.bot/api/v1/auth/token",
		RedirectURI: "http://127.0.0.1:56122/callback",
		ClineAuth:   true,
	}
	c, err := NewConnect(creds)
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.AuthorizeURL()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("client_type") != "extension" {
		t.Errorf("client_type = %q", q.Get("client_type"))
	}
	if q.Get("callback_url") != creds.RedirectURI {
		t.Errorf("callback_url = %q", q.Get("callback_url"))
	}
	if q.Get("redirect_uri") != creds.RedirectURI {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != c.State() {
		t.Errorf("state = %q, want %q", q.Get("state"), c.State())
	}
	for _, banned := range []string{"response_type", "client_id", "code_challenge", "scope"} {
		if q.Get(banned) != "" {
			t.Errorf("unexpected %s=%q", banned, q.Get(banned))
		}
	}
}

func TestExchangeClineCodeBase64(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	raw := `{"accessToken":"at-1","refreshToken":"rt-1","email":"a@b.c","expiresAt":"` + exp + `"}`
	// Unpadded base64 plus trailing junk after the JSON object.
	code := base64.StdEncoding.EncodeToString([]byte(raw + "TRAILING"))
	code = strings.TrimRight(code, "=")

	creds := &domain.OAuthCreds{ClineAuth: true, RedirectURI: "http://127.0.0.1/cb"}
	if err := exchangeClineCode(context.Background(), creds, code, ""); err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "workos:at-1" {
		t.Errorf("access = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "rt-1" {
		t.Errorf("refresh = %q", creds.RefreshToken)
	}
	if creds.Email != "a@b.c" {
		t.Errorf("email = %q", creds.Email)
	}
	if creds.ExpiresAt == 0 {
		t.Error("expected non-zero expires_at")
	}
}

func TestExchangeClineCodeJSONFallback(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	var sawBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sawBody)
		_, _ = w.Write([]byte(`{"data":{"accessToken":"at-2","refreshToken":"rt-2","expiresAt":"` + exp + `","userInfo":{"email":"u@c.d"}}}`))
	}))
	defer srv.Close()

	creds := &domain.OAuthCreds{
		ClineAuth:   true,
		TokenURL:    srv.URL,
		RedirectURI: "http://127.0.0.1:56122/callback",
	}
	// Not valid base64-JSON, so exchange must hit the token endpoint.
	if err := exchangeClineCode(context.Background(), creds, "not-base64-json-tokens", ""); err != nil {
		t.Fatal(err)
	}
	if sawBody["grant_type"] != "authorization_code" || sawBody["client_type"] != "extension" {
		t.Errorf("body = %#v", sawBody)
	}
	if sawBody["redirect_uri"] != creds.RedirectURI || sawBody["code"] != "not-base64-json-tokens" {
		t.Errorf("body = %#v", sawBody)
	}
	if creds.AccessToken != "workos:at-2" || creds.RefreshToken != "rt-2" || creds.Email != "u@c.d" {
		t.Errorf("creds = %+v", creds)
	}
	if creds.ExpiresAt == 0 {
		t.Error("expected expires_at from ISO")
	}
}

func TestRefreshCline(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	var sawBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sawBody)
		_, _ = w.Write([]byte(`{"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresAt":"` + exp + `"}}`))
	}))
	defer srv.Close()

	creds := &domain.OAuthCreds{
		ClineAuth:    true,
		RefreshURL:   srv.URL,
		RefreshToken: "old-rt",
		AccessToken:  "workos:old",
	}
	if err := refresh(context.Background(), creds, time.Now()); err != nil {
		t.Fatal(err)
	}
	if sawBody["grantType"] != "refresh_token" || sawBody["clientType"] != "extension" || sawBody["refreshToken"] != "old-rt" {
		t.Errorf("body = %#v", sawBody)
	}
	if creds.AccessToken != "workos:new-at" {
		t.Errorf("access = %q", creds.AccessToken)
	}
	if creds.RefreshToken != "new-rt" {
		t.Errorf("refresh = %q", creds.RefreshToken)
	}
	if creds.ExpiresAt == 0 {
		t.Error("expected expires_at")
	}
}

func TestRefreshClineFallsBackToTokenURL(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"accessToken":"a","refreshToken":"r"}`))
	}))
	defer srv.Close()
	creds := &domain.OAuthCreds{
		ClineAuth:    true,
		TokenURL:     srv.URL,
		RefreshToken: "rt",
	}
	if err := refreshCline(context.Background(), creds, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("expected POST to TokenURL when RefreshURL empty")
	}
}

func TestRefreshClineInvalidGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	creds := &domain.OAuthCreds{
		ClineAuth:    true,
		RefreshURL:   srv.URL,
		RefreshToken: "dead",
	}
	if err := refresh(context.Background(), creds, time.Now()); err != ErrInvalidGrant {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
}

func TestClineIdentityHeaders(t *testing.T) {
	h := ClineIdentityHeaders("1.2.3", "plain")
	if h["Authorization"] != "Bearer workos:plain" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
	if h["HTTP-Referer"] != "https://cline.bot" || h["X-Title"] != "Cline" {
		t.Errorf("identity headers = %#v", h)
	}
	if h["X-CLIENT-TYPE"] != "airouter" || h["User-Agent"] != "airouter/1.2.3" {
		t.Errorf("client headers = %#v", h)
	}
	// Already-prefixed token must not double-prefix.
	h2 := ClineIdentityHeaders("", "workos:x")
	if h2["Authorization"] != "Bearer workos:x" {
		t.Errorf("prefixed Authorization = %q", h2["Authorization"])
	}
}

func TestApplyPresetCline(t *testing.T) {
	p, ok := PresetByName("cline")
	if !ok {
		t.Fatal("missing cline preset")
	}
	prov, creds := Apply(p)
	if !creds.ClineAuth {
		t.Error("ClineAuth not set")
	}
	if creds.RefreshURL == "" || creds.TokenURL == "" || creds.AuthURL == "" {
		t.Errorf("urls incomplete: %+v", creds)
	}
	if creds.ClientID != "" || creds.PKCE {
		t.Errorf("unexpected client_id/pkce: %+v", creds)
	}
	if prov.Protocol != domain.ProtocolOpenAI || prov.BaseURL != "https://api.cline.bot/api/v1" {
		t.Errorf("provider = %+v", prov)
	}
	if _, ok := PresetByName("clinepass"); !ok {
		t.Fatal("missing clinepass preset")
	}
}

func TestHandleCallbackSkipsMissingStateForCline(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	raw := `{"accessToken":"cb-at","refreshToken":"cb-rt","email":"c@d.e","expiresAt":"` + exp + `"}`
	code := base64.StdEncoding.EncodeToString([]byte(raw))

	creds := &domain.OAuthCreds{
		AuthURL:     "https://example.test/authorize",
		TokenURL:    "https://example.test/token",
		RedirectURI: "http://127.0.0.1:0/callback",
		ClineAuth:   true,
	}
	c, err := NewConnect(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// No state query param — Cline AS may omit it.
	resp, err := http.Get("http://" + c.Addr() + "/callback?code=" + url.QueryEscape(code))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, err := c.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "workos:cb-at" {
		t.Errorf("access = %q", got.AccessToken)
	}
}
