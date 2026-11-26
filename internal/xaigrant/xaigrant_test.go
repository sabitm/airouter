package xaigrant

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/domain"
)

func jwtWith(payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func grant(generation string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Preset: Preset, ClientID: ClientID, TokenURL: TokenURL,
		AccessToken: "access", RefreshToken: "refresh",
		XAIGrantGeneration: generation,
	}
}

func TestIsGrantAndEligible(t *testing.T) {
	c := grant("gen")
	if !IsGrant(c) {
		t.Fatal("official grant rejected")
	}
	p := &domain.Provider{ID: 1, BaseURL: APIBase, Protocol: domain.ProtocolOpenAIResponses, AuthMethod: domain.AuthOAuth, OAuthCreds: c}
	if !Eligible(p) {
		t.Fatal("eligible provider rejected")
	}
	c.ClientID = "other"
	if IsGrant(c) || Eligible(p) {
		t.Fatal("foreign client accepted")
	}
	c = grant("gen")
	c.GrokBuildAuth = true
	if IsGrant(c) {
		t.Fatal("mixed marker accepted")
	}
	p.BaseURL = "https://api.x.ai.evil/v1"
	if Eligible(p) {
		t.Fatal("lookalike endpoint accepted")
	}
}

func TestEndpointChecks(t *testing.T) {
	for _, raw := range []string{APIBase, APIBase + "/", "https://api.x.ai:443/v1"} {
		if got, ok := NormalizeEndpoint(raw); !ok || got != APIBase {
			t.Errorf("endpoint %q = %q %v", raw, got, ok)
		}
	}
	for _, raw := range []string{
		"http://api.x.ai/v1", "https://api.x.ai/v1?x=1", "https://api.x.ai/v2",
		"https://api.x.ai:8443/v1", "https://user@api.x.ai/v1", "https://api.x.ai/v1#x",
	} {
		if _, ok := NormalizeEndpoint(raw); ok {
			t.Errorf("endpoint %q accepted", raw)
		}
	}
	if !IsStandardResponsesURL(ResponsesURL) || IsStandardResponsesURL(APIBase+"/chat/completions") {
		t.Fatal("responses URL check failed")
	}
	if _, ok := NormalizeTokenURL(TokenURL); !ok {
		t.Fatal("official token URL rejected")
	}
	if _, ok := NormalizeTokenURL("https://auth.x.ai/oauth2/token?x=1"); ok {
		t.Fatal("token URL with query accepted")
	}
}

func TestRotateAndPrincipal(t *testing.T) {
	c := grant("old")
	if err := Rotate(c); err != nil || c.XAIGrantGeneration == "" || c.XAIGrantGeneration == "old" || c.AccessToken != "access" {
		t.Fatalf("rotate = %+v %v", c, err)
	}
	c.AccessToken = jwtWith(map[string]any{"sub": "user-1", "email": "not-proof@example.com"})
	if got := Principal(c); got != "user-1" {
		t.Fatalf("principal = %q", got)
	}
	c.AccessToken = jwtWith(map[string]any{"principal_type": "Team", "principal_id": "team-1", "sub": "user-1"})
	if got := Principal(c); got != "team-1" {
		t.Fatalf("team principal = %q", got)
	}
	c.AccessToken = "opaque"
	c.AccountID = "acct"
	if got := Principal(c); got != "acct" {
		t.Fatalf("account fallback = %q", got)
	}
	if strings.Contains(Principal(&domain.OAuthCreds{Email: "x@y"}), "x@y") {
		t.Fatal("email became proof")
	}
}
