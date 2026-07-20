package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"airouter/internal/domain"
)

// ClientVersion is the product identity sent in Cline upstream headers. Main may
// overwrite it at startup; empty falls back to "dev".
var ClientVersion = "dev"

// EnsureWorkosPrefix returns a Cline access token with the WorkOS prefix the
// upstream requires. Idempotent: already-prefixed and empty tokens pass through.
func EnsureWorkosPrefix(token string) string {
	token = strings.TrimSpace(token)
	if token == "" || strings.HasPrefix(token, "workos:") {
		return token
	}
	return "workos:" + token
}

// ClineIdentityHeaders returns the Authorization + Cline client-identity headers
// the upstream expects on every request. Callers apply the map onto an *http.Request.
// version may be empty (falls back to ClientVersion / "dev").
func ClineIdentityHeaders(version, token string) map[string]string {
	if version == "" {
		version = ClientVersion
	}
	if version == "" {
		version = "dev"
	}
	ua := "airouter/" + version
	h := map[string]string{
		"HTTP-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"User-Agent":         ua,
		"X-PLATFORM":         runtime.GOOS,
		"X-PLATFORM-VERSION": runtime.Version(),
		"X-CLIENT-TYPE":      "airouter",
		"X-CLIENT-VERSION":   version,
		"X-CORE-VERSION":     version,
		"X-IS-MULTIROOT":     "false",
	}
	if tok := EnsureWorkosPrefix(token); tok != "" {
		h["Authorization"] = "Bearer " + tok
	}
	return h
}

// clineTokenPayload is the camelCase token envelope Cline returns from base64
// codes, the token endpoint, and refresh. Nested under data or top-level.
type clineTokenPayload struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Email        string `json:"email"`
	ExpiresAt    string `json:"expiresAt"`
	UserInfo     *struct {
		Email string `json:"email"`
	} `json:"userInfo"`
	Data *clineTokenPayload `json:"data"`
}

// unwrap returns the inner payload when nested under data, else p itself.
func (p *clineTokenPayload) unwrap() *clineTokenPayload {
	if p != nil && p.Data != nil {
		return p.Data
	}
	return p
}

// exchangeClineCode completes a Cline authorization-code grant. Primary path:
// the redirect code is itself base64(JSON tokens). Fallback: POST JSON to the
// token endpoint with client_type=extension (no client_id/PKCE).
func exchangeClineCode(ctx context.Context, c *domain.OAuthCreds, code, baseURL string) error {
	if payload, ok := decodeClineBase64Code(code); ok {
		return applyClineTokenPayload(c, payload, time.Now())
	}

	tokenURL := c.TokenURL
	if baseURL != "" {
		tokenURL = baseURL + "/token"
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"client_type":  "extension",
		"redirect_uri": c.RedirectURI,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: cline exchange request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	var payload clineTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("oauth: cline exchange: decode %d: %w", resp.StatusCode, err)
	}
	if err := applyClineTokenPayload(c, &payload, time.Now()); err != nil {
		return fmt.Errorf("oauth: cline exchange: %w (HTTP %d)", err, resp.StatusCode)
	}
	return nil
}

// decodeClineBase64Code tries to treat code as base64-encoded JSON tokens, the
// shape Cline's app redirect commonly embeds. ok is false when the code is not
// that shape so the caller can fall back to the token endpoint.
func decodeClineBase64Code(code string) (*clineTokenPayload, bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, false
	}
	// Pad to a multiple of 4; Cline codes are often unpadded.
	if m := len(code) % 4; m != 0 {
		code += strings.Repeat("=", 4-m)
	}
	decoded, err := base64.StdEncoding.DecodeString(code)
	if err != nil {
		// Some clients emit URL-safe base64; try once before giving up.
		decoded, err = base64.URLEncoding.DecodeString(code)
		if err != nil {
			return nil, false
		}
	}
	s := string(decoded)
	// Truncate trailing junk after the JSON object (9router lastIndexOf('}')).
	last := strings.LastIndex(s, "}")
	if last == -1 {
		return nil, false
	}
	var payload clineTokenPayload
	if err := json.Unmarshal([]byte(s[:last+1]), &payload); err != nil {
		return nil, false
	}
	if payload.unwrap().AccessToken == "" {
		return nil, false
	}
	return &payload, true
}

// refreshCline exchanges a Cline refresh token. Body and response are camelCase
// JSON (grantType/clientType), optionally nested under data; expiresAt is ISO.
func refreshCline(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c.RefreshToken == "" {
		return fmt.Errorf("oauth: cline refresh: no refresh token")
	}
	url := c.RefreshURL
	if url == "" {
		url = c.TokenURL
	}
	if url == "" {
		return fmt.Errorf("oauth: cline refresh: no refresh URL")
	}
	body, _ := json.Marshal(map[string]string{
		"refreshToken": c.RefreshToken,
		"grantType":    "refresh_token",
		"clientType":   "extension",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: cline refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	badStatus := resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden

	var payload clineTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		if badStatus {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: cline refresh: decode %d: %w", resp.StatusCode, err)
	}
	inner := payload.unwrap()
	if badStatus && (inner == nil || inner.AccessToken == "") {
		return ErrInvalidGrant
	}
	if err := applyClineTokenPayload(c, &payload, now); err != nil {
		if badStatus {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: cline refresh: %w (HTTP %d)", err, resp.StatusCode)
	}
	return nil
}

// applyClineTokenPayload writes access/refresh/email/expiry from a Cline token
// envelope into c. Access tokens are stored with the workos: prefix.
func applyClineTokenPayload(c *domain.OAuthCreds, payload *clineTokenPayload, now time.Time) error {
	if payload == nil {
		return fmt.Errorf("empty token payload")
	}
	p := payload.unwrap()
	if p == nil || p.AccessToken == "" {
		return fmt.Errorf("empty accessToken")
	}
	c.AccessToken = EnsureWorkosPrefix(p.AccessToken)
	if p.RefreshToken != "" {
		c.RefreshToken = p.RefreshToken
	}
	email := p.Email
	if email == "" && p.UserInfo != nil {
		email = p.UserInfo.Email
	}
	if email != "" {
		c.Email = email
	}
	if exp := parseClineExpiresAt(p.ExpiresAt, now); exp > 0 {
		c.ExpiresAt = exp
	}
	return nil
}

// parseClineExpiresAt converts a Cline ISO expiresAt to unix seconds. Zero means
// unknown (leave proactive refresh off) rather than inventing a default lifetime.
func parseClineExpiresAt(s string, _ time.Time) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}
