// Package xaigrant identifies the official public xAI OAuth grant and the
// stateless Responses endpoint used by the public Grok API.
package xaigrant

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"airouter/internal/domain"
)

const (
	// Preset is the built-in public Grok (xAI) OAuth recipe name.
	Preset = "xai"
	// ClientID is the public xAI OAuth client id. A different id is not this grant.
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// TokenURL is the official xAI token endpoint.
	TokenURL = "https://auth.x.ai/oauth2/token"
	// APIBase is the only inference origin that may carry opaque xAI replay.
	APIBase = "https://api.x.ai/v1"
	// ResponsesURL is the stateless Responses endpoint formed from APIBase.
	ResponsesURL = APIBase + "/responses"
	// WireModel is the public Grok model whose Responses output carries
	// reasoning.encrypted_content.
	WireModel = "grok-4.7"
)

// ErrGeneration is a sanitized mint failure. It does not include token material.
var ErrGeneration = errors.New("xai: could not mint a grant generation")

// NewGeneration returns a random id that is not derived from tokens or account ids.
func NewGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", ErrGeneration
	}
	return hex.EncodeToString(b[:]), nil
}

// Rotate replaces any imported or previous generation. Tokens stay unchanged.
func Rotate(creds *domain.OAuthCreds) error {
	if creds == nil {
		return nil
	}
	id, err := NewGeneration()
	if err != nil {
		return err
	}
	creds.XAIGrantGeneration = id
	return nil
}

// NormalizeEndpoint accepts only the standard public xAI API origin. Lookalike
// hosts, userinfo, query, fragment, and a non-default port are rejected. The
// result is the canonical origin without a trailing slash.
func NormalizeEndpoint(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "api.x.ai") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	if strings.TrimRight(u.EscapedPath(), "/") != "/v1" {
		return "", false
	}
	return APIBase, true
}

// NormalizeTokenURL accepts only the official xAI token endpoint.
func NormalizeTokenURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "auth.x.ai") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	if u.EscapedPath() != "/oauth2/token" {
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
	if !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "api.x.ai") {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	return u.EscapedPath() == "/v1/responses"
}

// Mixed reports provider markers that do not belong on a public xAI grant.
func Mixed(c *domain.OAuthCreds) bool {
	if c == nil {
		return false
	}
	return c.GrokBuildAuth || c.ClaudeCodeAuth || c.ClineAuth || c.QoderAuth || c.CursorAuth ||
		c.AntigravityAuth || c.OpencodeConsoleAuth || c.KiroAuth != ""
}

// IsGrant reports the official public xAI OAuth grant. An empty client id or
// token URL is not treated as the official value. Mixed markers fail closed.
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

// Connection reports a provider row that can carry this grant, regardless of
// transport. A protocol change does not turn a grant into a different grant.
func Connection(p *domain.Provider) bool {
	return p != nil && p.ID != 0 && !p.Archived && p.Method() == domain.AuthOAuth && IsGrant(p.OAuthCreds)
}

// Official reports a Responses provider whose base URL is the official endpoint.
func Official(p *domain.Provider) bool {
	if !Connection(p) || p.Protocol != domain.ProtocolOpenAIResponses {
		return false
	}
	_, ok := NormalizeEndpoint(p.BaseURL)
	return ok
}

// Eligible reports a provider that may stamp or pin public xAI Chat replay.
// The base URL and credentials must already be the trusted stored values.
func Eligible(p *domain.Provider) bool {
	if !Official(p) {
		return false
	}
	c := p.OAuthCreds
	return strings.TrimSpace(c.XAIGrantGeneration) != "" && c.AccessToken != ""
}

// IsWireModel reports the exact effective model whose Responses output carries
// encrypted reasoning for this bridge.
func IsWireModel(model string) bool {
	return strings.TrimSpace(model) == WireModel
}

type jwtClaims struct {
	Subject          string `json:"sub"`
	PrincipalType    string `json:"principal_type"`
	PrincipalID      string `json:"principal_id"`
	PrincipalTypeAlt string `json:"principalType"`
	PrincipalIDAlt   string `json:"principalId"`
	TeamID           string `json:"team_id"`
	OrganizationID   string `json:"organization_id"`
	AccountID        string `json:"account_id"`
}

func tokenClaims(token string) jwtClaims {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return jwtClaims{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}
	}
	var claims jwtClaims
	if json.Unmarshal(raw, &claims) != nil {
		return jwtClaims{}
	}
	return claims
}

// Principal returns a stable user or team identity for source binding. Email is
// not proof and is never returned. Empty means the token exposed no stable id.
func Principal(c *domain.OAuthCreds) string {
	if c == nil {
		return ""
	}
	claims := tokenClaims(c.AccessToken)
	if claims.Subject == "" && claims.PrincipalID == "" && claims.PrincipalIDAlt == "" {
		claims = tokenClaims(c.IDToken)
	}
	typ := claims.PrincipalType
	if typ == "" {
		typ = claims.PrincipalTypeAlt
	}
	id := claims.PrincipalID
	if id == "" {
		id = claims.PrincipalIDAlt
	}
	switch strings.ToLower(typ) {
	case "team", "organization":
		if id != "" {
			return id
		}
	}
	if claims.Subject != "" {
		return claims.Subject
	}
	if id != "" {
		return id
	}
	if claims.TeamID != "" {
		return claims.TeamID
	}
	if claims.OrganizationID != "" {
		return claims.OrganizationID
	}
	if claims.AccountID != "" {
		return claims.AccountID
	}
	return strings.TrimSpace(c.AccountID)
}
