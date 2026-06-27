package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
)

// fakeTokenServer records the exchange form values and returns a configurable
// token response. The token URL in creds is pointed at this server.
func fakeTokenServer(t *testing.T, fn func(form url.Values) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		status, body := fn(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testCreds(t *testing.T, tokenSrv *httptest.Server) *domain.OAuthCreds {
	t.Helper()
	return &domain.OAuthCreds{
		Mode:        domain.OAuthAuto,
		Preset:      "xai",
		AuthURL:     "https://auth.example/authorize",
		TokenURL:    tokenSrv.URL,
		ClientID:    "cid",
		Scopes:      "openid offline_access",
		RedirectURI: "http://127.0.0.1:56121/callback",
		PKCE:        true,
	}
}

func TestNewConnectValidates(t *testing.T) {
	cases := []struct {
		name  string
		creds *domain.OAuthCreds
	}{
		{"nil", nil},
		{"no auth url", &domain.OAuthCreds{TokenURL: "u", ClientID: "c"}},
		{"no token url", &domain.OAuthCreds{AuthURL: "u", ClientID: "c"}},
		{"no client id", &domain.OAuthCreds{AuthURL: "u", TokenURL: "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewConnect(tc.creds); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestAuthorizeURLIncludesPKCEChallenge(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) { return 200, `{}` })
	defer srv.Close()
	c, err := NewConnect(testCreds(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	u, err := c.AuthorizeURL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://auth.example/authorize?") {
		t.Errorf("url = %q", u)
	}
	for _, want := range []string{"response_type=code", "code_challenge=", "code_challenge_method=S256", "client_id=cid"} {
		if !strings.Contains(u, want) {
			t.Errorf("authorize url missing %q: %s", want, u)
		}
	}
}

func TestAuthorizeURLNoPKCEForConfidentialClient(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) { return 200, `{}` })
	defer srv.Close()
	creds := testCreds(t, srv)
	creds.PKCE = false
	creds.ClientSecret = "secret"
	c, _ := NewConnect(creds)
	u, _ := c.AuthorizeURL()
	if strings.Contains(u, "code_challenge") {
		t.Errorf("confidential client should not send code_challenge: %s", u)
	}
}

// TestExchangeCodeManualPaste exchanges a raw code pasted by a remote operator.
func TestExchangeCodeManualPaste(t *testing.T) {
	var seen url.Values
	srv := fakeTokenServer(t, func(form url.Values) (int, string) {
		seen = form
		return 200, `{"access_token":"tok","refresh_token":"rt","expires_in":3600,"id_token":"` + idToken(t, "u@x.com") + `"}`
	})
	c, err := NewConnect(testCreds(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := c.ExchangeCode(context.Background(), "the-code")
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "tok" || creds.RefreshToken != "rt" {
		t.Errorf("creds = %+v", creds)
	}
	if creds.Email != "u@x.com" {
		t.Errorf("email = %q", creds.Email)
	}
	// PKCE client must send the verifier matching the challenge it issued.
	if seen.Get("code_verifier") != c.verifier {
		t.Errorf("verifier mismatch")
	}
	if seen.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", seen.Get("grant_type"))
	}
	if seen.Get("code") != "the-code" {
		t.Errorf("code = %q", seen.Get("code"))
	}
	if seen.Get("client_secret") != "" {
		t.Errorf("PKCE client should not send client_secret")
	}
}

// TestExchangeCodeFromPastedURL extracts the code from a full redirect URL and
// validates state.
func TestExchangeCodeFromPastedURL(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok","expires_in":60}`
	})
	c, err := NewConnect(testCreds(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	redirect := "http://127.0.0.1:56121/callback?code=abc&state=" + c.state
	creds, err := c.ExchangeCode(context.Background(), redirect)
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "tok" {
		t.Errorf("token = %q", creds.AccessToken)
	}
}

func TestExchangeCodeStateMismatch(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) { return 200, `{}` })
	c, _ := NewConnect(testCreds(t, srv))
	redirect := "http://127.0.0.1:56121/callback?code=abc&state=wrong"
	if _, err := c.ExchangeCode(context.Background(), redirect); err == nil {
		t.Fatal("want state mismatch error")
	}
}

func TestExchangeCodeEmpty(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) { return 200, `{}` })
	c, _ := NewConnect(testCreds(t, srv))
	if _, err := c.ExchangeCode(context.Background(), ""); err == nil {
		t.Fatal("want error for empty code")
	}
}

// TestExchangeCodeDoubleExchangeNoDuplicate verifies a second exchange (e.g. a
// late loopback callback after a manual paste) reuses the first result instead
// of hitting the token endpoint again.
func TestExchangeCodeDoubleExchangeNoDuplicate(t *testing.T) {
	var hits int
	srv := fakeTokenServer(t, func(url.Values) (int, string) {
		hits++
		return 200, `{"access_token":"tok","refresh_token":"rt","expires_in":60}`
	})
	c, _ := NewConnect(testCreds(t, srv))
	if _, err := c.ExchangeCode(context.Background(), "code1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExchangeCode(context.Background(), "code2"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("token endpoint hits = %d, want 1 (memoized)", hits)
	}
}

func TestExchangeCodeUpstreamError(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) {
		return 400, `{"error":"invalid_grant","error_description":"bad code"}`
	})
	c, _ := NewConnect(testCreds(t, srv))
	_, err := c.ExchangeCode(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err = %v, want invalid_grant", err)
	}
}

// TestWaitTimesOut confirms Wait honors context cancellation when no exchange
// has occurred.
func TestWaitTimesOut(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) { return 200, `{}` })
	c, _ := NewConnect(testCreds(t, srv))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Wait(ctx)
	if err == nil {
		t.Fatal("want timeout error")
	}
}

// TestLoopbackAutoExchange starts the loopback callback server, simulates the
// provider's authorize redirect (code + state), and asserts Wait returns the
// exchanged token. This is the local-browser happy path. An ephemeral port (0)
// avoids colliding with the fixed 56121 a real xAI connect would use.
func TestLoopbackAutoExchange(t *testing.T) {
	srv := fakeTokenServer(t, func(url.Values) (int, string) {
		return 200, `{"access_token":"tok-lb","refresh_token":"rt-lb","expires_in":3600}`
	})
	creds := &domain.OAuthCreds{
		Mode: domain.OAuthAuto, AuthURL: "https://auth.example/authorize", TokenURL: srv.URL,
		ClientID: "cid", Scopes: "openid", RedirectURI: "http://127.0.0.1:0/callback", PKCE: true,
	}
	c, err := NewConnect(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	// Simulate the provider redirecting the browser to the bound callback.
	cbURL := "http://" + c.Addr() + "/callback?code=lbc&state=" + c.state
	resp, err := http.Get(cbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback status = %d, want 200", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := c.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got.AccessToken != "tok-lb" {
		t.Errorf("token = %q, want tok-lb", got.AccessToken)
	}
}
