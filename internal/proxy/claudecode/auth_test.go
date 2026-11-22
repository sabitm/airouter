package claudecode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"airouter/internal/domain"
)

func TestIsOAuthTokenRemainsLegacyHeuristic(t *testing.T) {
	if !IsOAuthToken("prefix-sk-ant-oat-suffix") || !IsOAuthToken("sk-ant-oat") {
		t.Fatal("legacy marker was not detected")
	}
	for _, token := range []string{"", "sk-ant-api", "opaque-synthetic"} {
		if IsOAuthToken(token) {
			t.Fatalf("legacy heuristic matched %q", token)
		}
	}
}

func TestOAuthBetaTruthTable(t *testing.T) {
	const oat = "sk-ant-oat-synthetic"
	const opaque = "opaque-synthetic"
	const other = "other-synthetic"
	inference := "user:inference"
	profile := "user:profile"
	cases := []struct {
		name string
		p    *domain.Provider
		want bool
	}{
		{name: "nil provider", want: false},
		{name: "empty token ignores grant", p: oauthGrant("", oat, inference), want: false},
		{name: "exact inference opaque", p: oauthGrant(opaque, opaque, inference), want: true},
		{name: "exact inference oat", p: oauthGrant(oat, oat, inference), want: true},
		{name: "padded inference", p: oauthGrant(opaque, opaque, " \tuser:inference\n"), want: true},
		{name: "inference among grants", p: oauthGrant(opaque, opaque, "user:profile\tuser:inference\nuser:sessions:claude_code"), want: true},
		{name: "near suffix", p: oauthGrant(oat, oat, "user:inferences"), want: false},
		{name: "near extra", p: oauthGrant(oat, oat, "user:inference-extra"), want: false},
		{name: "near case", p: oauthGrant(oat, oat, "User:inference"), want: false},
		{name: "near punctuation", p: oauthGrant(oat, oat, "user:inference,"), want: false},
		{name: "near prefix", p: oauthGrant(oat, oat, "xuser:inference"), want: false},
		{name: "split item", p: oauthGrant(oat, oat, "user: inference"), want: false},
		{name: "profile only oat", p: oauthGrant(oat, oat, profile), want: false},
		{name: "profile only opaque", p: oauthGrant(opaque, opaque, profile), want: false},
		{name: "preset without inference", p: oauthGrant(oat, oat, "org:create_api_key user:profile user:sessions:claude_code"), want: false},
		{name: "unknown empty oat", p: oauthGrant(oat, oat, ""), want: true},
		{name: "unknown empty opaque", p: oauthGrant(opaque, opaque, ""), want: false},
		{name: "whitespace grant oat", p: oauthGrant(oat, oat, " \t\n"), want: true},
		{name: "whitespace grant opaque", p: oauthGrant(opaque, opaque, " \t\n"), want: false},
		{name: "nil creds oat", p: &domain.Provider{APIKey: oat, AuthMethod: domain.AuthOAuth}, want: true},
		{name: "nil creds opaque", p: &domain.Provider{APIKey: opaque, AuthMethod: domain.AuthOAuth}, want: false},
		{name: "mismatch scopes not applied to oat", p: oauthGrant(oat, other, profile), want: true},
		{name: "mismatch scopes not applied to opaque", p: oauthGrant(opaque, other, inference), want: false},
		{name: "mismatch other direction oat", p: oauthGrant(oat, opaque, profile), want: true},
		{name: "mismatch other direction opaque", p: oauthGrant(opaque, oat, inference), want: false},
		{name: "api key ignores matching inference grant", p: apiKeyGrant(opaque, opaque, inference), want: false},
		{name: "api key ignores matching profile grant", p: apiKeyGrant(oat, oat, profile), want: true},
		{name: "default method ignores config grant", p: defaultMethodGrant(opaque, opaque, inference), want: false},
		{name: "imported oat without grant", p: oauthGrant(oat, oat, ""), want: true},
		{name: "imported opaque without grant", p: oauthGrant(opaque, opaque, ""), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.p != nil {
				tc.p.Protocol = domain.ProtocolOpenAI
				tc.p.AuthScheme = domain.AuthXAPIKey
			}
			if got := OAuthBeta(tc.p); got != tc.want {
				t.Fatalf("OAuthBeta = %v, want %v", got, tc.want)
			}
			assertOAuthBetaDidNotMutate(t, tc.p)
		})
	}
	paired := oauthGrant(opaque, opaque, inference)
	paired.Protocol = domain.ProtocolClaudeCode
	paired.AuthScheme = domain.AuthBearer
	if !OAuthBeta(paired) {
		t.Fatal("scheme or protocol changed the paired grant result")
	}
}

func TestOAuthBetaConcurrentDoesNotMutate(t *testing.T) {
	p := oauthGrant("opaque-synthetic", "opaque-synthetic", "user:inference")
	before := *p.OAuthCreds
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !OAuthBeta(p) {
				t.Error("paired grant became false")
			}
		}()
	}
	wg.Wait()
	if p.APIKey != "opaque-synthetic" || p.OAuthCreds.AccessToken != before.AccessToken || p.OAuthCreds.ClaudeScopes != before.ClaudeScopes {
		t.Fatal("concurrent classification mutated the provider")
	}
}

func TestListModelIDsClassifiesPairedGrant(t *testing.T) {
	if _, err := ListModelIDs(context.Background(), nil); err == nil {
		t.Fatal("nil provider was accepted")
	}
	const oat = "sk-ant-oat-synthetic"
	const opaque = "opaque-synthetic"
	cases := []struct {
		name      string
		provider  *domain.Provider
		wantOAuth bool
	}{
		{name: "known inference opaque", provider: oauthGrant(opaque, opaque, "user:inference"), wantOAuth: true},
		{name: "known profile oat", provider: oauthGrant(oat, oat, "user:profile"), wantOAuth: false},
		{name: "api key ignores scopes", provider: apiKeyGrant(opaque, opaque, "user:inference"), wantOAuth: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/models?limit=1000" {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer "+tc.provider.APIKey {
					t.Errorf("auth = %q", r.Header.Get("Authorization"))
				}
				if _, ok := r.Header["X-Stainless-Helper-Method"]; ok {
					t.Error("discovery claimed an SDK helper")
				}
				gotOAuth := r.Header.Get("anthropic-beta") == BetaOAuth
				if gotOAuth != tc.wantOAuth {
					t.Errorf("oauth beta = %v header %q", gotOAuth, r.Header.Get("anthropic-beta"))
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":[{"id":"account-model"}]}`)
			}))
			defer srv.Close()
			before := *tc.provider
			var credsBefore domain.OAuthCreds
			credsPtr := tc.provider.OAuthCreds
			if credsPtr != nil {
				credsBefore = *credsPtr
			}
			tc.provider.BaseURL = srv.URL + "/v1/"
			got, err := ListModelIDs(t.Context(), tc.provider)
			if err != nil || len(got) != 1 || got[0] != "account-model" || calls != 1 {
				t.Fatalf("models = %v err = %v calls = %d", got, err, calls)
			}
			if tc.provider.APIKey != before.APIKey || tc.provider.AuthMethod != before.AuthMethod || tc.provider.OAuthCreds != credsPtr {
				t.Fatal("discovery mutated the provider")
			}
			if credsPtr != nil && (tc.provider.OAuthCreds.AccessToken != credsBefore.AccessToken || tc.provider.OAuthCreds.ClaudeScopes != credsBefore.ClaudeScopes) {
				t.Fatal("discovery mutated the credential snapshot")
			}
		})
	}
}

func oauthGrant(effective, access, scopes string) *domain.Provider {
	return &domain.Provider{
		APIKey: effective, AuthMethod: domain.AuthOAuth, Protocol: domain.ProtocolClaudeCode,
		OAuthCreds: &domain.OAuthCreds{AccessToken: access, ClaudeScopes: scopes, ClaudeCodeAuth: true},
	}
}

func apiKeyGrant(effective, access, scopes string) *domain.Provider {
	p := oauthGrant(effective, access, scopes)
	p.AuthMethod = domain.AuthAPIKey
	return p
}

func defaultMethodGrant(effective, access, scopes string) *domain.Provider {
	p := oauthGrant(effective, access, scopes)
	p.AuthMethod = ""
	return p
}

func assertOAuthBetaDidNotMutate(t *testing.T, p *domain.Provider) {
	t.Helper()
	if p == nil {
		return
	}
	apiKey := p.APIKey
	method := p.AuthMethod
	scheme := p.AuthScheme
	protocol := p.Protocol
	creds := p.OAuthCreds
	var access, scopes string
	if creds != nil {
		access, scopes = creds.AccessToken, creds.ClaudeScopes
	}
	_ = OAuthBeta(p)
	if p.APIKey != apiKey || p.AuthMethod != method || p.AuthScheme != scheme || p.Protocol != protocol || p.OAuthCreds != creds {
		t.Fatal("classifier mutated the provider")
	}
	if creds != nil && (creds.AccessToken != access || creds.ClaudeScopes != scopes) {
		t.Fatal("classifier mutated the credential snapshot")
	}
}
