package codexgrant

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"airouter/internal/domain"
)

const (
	// Preset is the built-in Codex OAuth recipe name.
	Preset = "codex"
	// ClientID is the public Codex CLI client id. A different id is not this grant.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// TokenURL is the official ChatGPT token endpoint.
	TokenURL = "https://auth.openai.com/oauth/token"
	// APIBase is the only inference origin that may carry opaque Chat replay.
	APIBase = "https://chatgpt.com/backend-api/codex"
	// ResponsesURL is the inference URL formed from APIBase and the Codex path.
	ResponsesURL = APIBase + "/responses"
)

// ErrGeneration is a sanitized mint failure. It does not include token material.
var ErrGeneration = errors.New("codex: could not mint a grant generation")

// NewGeneration returns a random id that is not derived from tokens or account ids.
func NewGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrGeneration
	}
	return hex.EncodeToString(b[:]), nil
}

// Rotate replaces any imported or previous generation. A nil creds value is ignored.
func Rotate(creds *domain.OAuthCreds) error {
	if creds == nil {
		return nil
	}
	id, err := NewGeneration()
	if err != nil {
		return err
	}
	creds.CodexGrantGeneration = id
	return nil
}

// NormalizeEndpoint accepts only the standard Codex API origin. Lookalike hosts,
// userinfo, query, fragment, and a non-default port are rejected. The result is
// the canonical origin without a trailing slash.
func NormalizeEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", false
	}
	if !strings.EqualFold(u.Hostname(), "chatgpt.com") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path != "/backend-api/codex" {
		return "", false
	}
	return APIBase, true
}

// NormalizeTokenURL accepts only the official token endpoint.
func NormalizeTokenURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "auth.openai.com") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	if u.EscapedPath() != "/oauth/token" {
		return "", false
	}
	return TokenURL, true
}

// IsStandardResponsesURL reports whether raw is the standard inference URL.
func IsStandardResponsesURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "chatgpt.com") {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return u.EscapedPath() == "/backend-api/codex/responses"
}

// Mixed reports provider markers that do not belong on a Codex grant.
func Mixed(c *domain.OAuthCreds) bool {
	if c == nil {
		return false
	}
	return c.GrokBuildAuth || c.ClaudeCodeAuth || c.ClineAuth || c.QoderAuth || c.CursorAuth ||
		c.AntigravityAuth || c.OpencodeConsoleAuth || c.KiroAuth != ""
}

// IsGrant reports a Codex OAuth grant. An empty client id or token URL is not
// treated as the official value. Mixed markers fail closed.
func IsGrant(c *domain.OAuthCreds) bool {
	if c == nil || c.Preset != Preset || Mixed(c) {
		return false
	}
	if c.ClientID != ClientID {
		return false
	}
	_, ok := NormalizeTokenURL(c.TokenURL)
	return ok
}

// Eligible reports a provider that may stamp or pin Codex Chat replay.
// The base URL and credentials must already be the trusted stored values.
func Eligible(p *domain.Provider) bool {
	if p == nil || p.Archived || p.ID == 0 || p.Protocol != domain.ProtocolOpenAICodex || p.Method() != domain.AuthOAuth {
		return false
	}
	if _, ok := NormalizeEndpoint(p.BaseURL); !ok || !IsGrant(p.OAuthCreds) {
		return false
	}
	c := p.OAuthCreds
	return strings.TrimSpace(c.AccountID) != "" && strings.TrimSpace(c.CodexGrantGeneration) != "" && c.AccessToken != ""
}
