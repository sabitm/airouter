package codexgrant

import (
	"strings"
	"testing"

	"airouter/internal/domain"
)

func TestNormalizeEndpointRejectsLookalikes(t *testing.T) {
	ok := []string{
		APIBase,
		APIBase + "/",
		"https://chatgpt.com/backend-api/codex",
		"https://CHATGPT.COM/backend-api/codex/",
	}
	for _, raw := range ok {
		got, yes := NormalizeEndpoint(raw)
		if !yes || got != APIBase {
			t.Fatalf("%q -> %q %v", raw, got, yes)
		}
	}
	bad := []string{
		"http://chatgpt.com/backend-api/codex",
		"https://chatgpt.com.evil/backend-api/codex",
		"https://evilchatgpt.com/backend-api/codex",
		"https://chatgpt.com/backend-api/codex/extra",
		"https://chatgpt.com/backend-api/codex?x=1",
		"https://user:pass@chatgpt.com/backend-api/codex",
		"https://chatgpt.com:8443/backend-api/codex",
		"https://chatgpt.com/backend-api/codex#frag",
		"https://api.openai.com/v1",
	}
	for _, raw := range bad {
		if _, yes := NormalizeEndpoint(raw); yes {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestRotateReplacesImportedGeneration(t *testing.T) {
	c := &domain.OAuthCreds{CodexGrantGeneration: "imported", AccessToken: "access", RefreshToken: "refresh"}
	if err := Rotate(c); err != nil {
		t.Fatal(err)
	}
	if c.CodexGrantGeneration == "" || c.CodexGrantGeneration == "imported" || c.AccessToken != "access" || c.RefreshToken != "refresh" {
		t.Fatalf("rotated = %+v", c)
	}
	other := &domain.OAuthCreds{}
	if err := Rotate(other); err != nil {
		t.Fatal(err)
	}
	if other.CodexGrantGeneration == c.CodexGrantGeneration || len(other.CodexGrantGeneration) != 32 {
		t.Fatalf("generations = %q %q", c.CodexGrantGeneration, other.CodexGrantGeneration)
	}
	if strings.Contains(c.CodexGrantGeneration, "access") {
		t.Fatal("generation derived from token")
	}
}

func TestEligibleRequiresStandardGrant(t *testing.T) {
	p := &domain.Provider{
		ID: 3, BaseURL: APIBase, Protocol: domain.ProtocolOpenAICodex, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			Preset: Preset, ClientID: ClientID, TokenURL: TokenURL,
			AccountID: "acct", CodexGrantGeneration: "gen", AccessToken: "tok",
		},
	}
	if !Eligible(p) {
		t.Fatal("standard grant rejected")
	}
	p.BaseURL = "https://chatgpt.com.evil/backend-api/codex"
	if Eligible(p) {
		t.Fatal("lookalike endpoint accepted")
	}
	p.BaseURL = APIBase
	p.OAuthCreds.GrokBuildAuth = true
	if Eligible(p) || IsGrant(p.OAuthCreds) {
		t.Fatal("mixed grant accepted")
	}
}
