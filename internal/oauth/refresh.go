package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"airouter/internal/domain"
)

// ErrInvalidGrant signals that the refresh token is no longer valid (revoked or
// expired) and the connection must be re-established via the connect flow. It is
// distinct from a transient refresh failure, which leaves the existing token in
// place to retry on the next request.
var ErrInvalidGrant = errors.New("oauth: refresh token invalid or revoked")

// refreshLead is how long before expiry a token is proactively refreshed. xAI
// uses 5 minutes; applied uniformly since the connect config is inline.
const refreshLead = 5 * time.Minute

// tokenResponse is the subset of an OAuth/OIDC token response we persist. Error
// is RawMessage because providers disagree on its shape: the OAuth2 standard uses
// a flat string ("error":"invalid_grant"), while the ChatGPT/OpenAI backend nests
// an object ("error":{"code":"token_invalidated","message":...}). Typing it as a
// string would fail the whole decode against the nested shape, masking a revoked
// token as a raw parse error instead of a clean reconnect prompt.
type tokenResponse struct {
	AccessToken      string          `json:"access_token"`
	RefreshToken     string          `json:"refresh_token"`
	IDToken          string          `json:"id_token"`
	ExpiresIn        int             `json:"expires_in"`
	Scope            string          `json:"scope"`
	Error            json.RawMessage `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

// parseTokenError extracts a machine code and human description from the two
// error envelope shapes: a flat string (code, no description) or a nested object
// carrying code/message/type. Returns empty code when the token response carries
// no error.
func parseTokenError(raw json.RawMessage) (code, desc string) {
	if len(raw) == 0 {
		return "", ""
	}
	var flat string
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat, ""
	}
	var obj struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		code = obj.Code
		if code == "" {
			code = obj.Type
		}
		return code, obj.Message
	}
	return "", ""
}

// isInvalidGrantCode reports whether an error code from a token endpoint denotes
// a permanently rejected refresh token (revoked, expired, or invalidated) that
// requires re-running the connect flow rather than a retry.
func isInvalidGrantCode(code string) bool {
	switch strings.ToLower(code) {
	case "invalid_grant", "invalid_request", "token_invalidated", "token_expired":
		return true
	}
	return false
}

// shouldRefresh reports whether the access token should be refreshed before use.
// A zero ExpiresAt (unknown expiry) is left to the reactive 401 path.
func shouldRefresh(c *domain.OAuthCreds, now time.Time) bool {
	if c == nil || c.ExpiresAt == 0 {
		return false
	}
	return time.Unix(c.ExpiresAt, 0).Sub(now) < refreshLead
}

// refresh exchanges the refresh token for a new access token, updating creds in
// place. It keeps the old refresh token when the response does not rotate it
// (some providers always issue a new one; others reuse). Returns ErrInvalidGrant
// when the authorization server rejects the refresh token.
func refresh(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c.RefreshToken == "" {
		return errors.New("oauth: no refresh token")
	}
	// Kiro social/OIDC refresh uses JSON bodies with camelCase responses the
	// generic OAuth2 path cannot parse. external_idp is excluded: it refreshes
	// against a standard Microsoft token endpoint via the generic form path below.
	if c.KiroAuth != "" && c.KiroAuth != "external_idp" {
		return refreshKiro(ctx, c, now)
	}
	var req *http.Request
	var err error
	if c.RefreshJSON {
		// Codex/ChatGPT backend requires a JSON body for refresh (not form).
		body := map[string]string{
			"grant_type":    "refresh_token",
			"client_id":     c.ClientID,
			"refresh_token": c.RefreshToken,
		}
		bodyBytes, _ := json.Marshal(body)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("client_id", c.ClientID)
		form.Set("refresh_token", c.RefreshToken)
		if c.ClientSecret != "" {
			form.Set("client_secret", c.ClientSecret)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := readLimited(resp.Body)

	// A non-2xx with no parseable JSON body (e.g. an HTML error page) still means a
	// rejected credential; classify by status rather than surfacing a decode error.
	badStatus := resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		if badStatus {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: refresh: decode %d: %w", resp.StatusCode, err)
	}
	code, desc := parseTokenError(tr.Error)
	if code != "" {
		if isInvalidGrantCode(code) {
			return ErrInvalidGrant
		}
		if desc == "" {
			desc = tr.ErrorDescription
		}
		return fmt.Errorf("oauth: refresh: %s: %s", code, desc)
	}
	if tr.AccessToken == "" {
		// An empty token on a rejection status is a revoked/invalidated credential
		// whose envelope wording we do not recognize; prefer reconnect over a raw error.
		if badStatus {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: refresh: empty access_token (HTTP %d)", resp.StatusCode)
	}

	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		c.IDToken = tr.IDToken
		if claims, ok := claimsFromIDToken(tr.IDToken); ok {
			if claims.Email != "" {
				c.Email = claims.Email
			}
			if claims.AccountID != "" {
				c.AccountID = claims.AccountID
			}
		}
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// idClaims is the subset of a JWT id_token's payload we read. airouter is not the
// token's audience; the claims are used only for display and an upstream account
// header, so an unverified decode is acceptable (matches 9router).
type idClaims struct {
	Email string `json:"email"`
	// Auth is OpenAI's namespaced claim block carrying the ChatGPT account id.
	Auth struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	} `json:"https://api.openai.com/auth"`
	AccountID string `json:"account_id"`
}

// claimsFromIDToken decodes a JWT id_token's payload (signature unverified) and
// returns the email and ChatGPT account id it carries. ok is false when the token
// is malformed.
func claimsFromIDToken(idToken string) (idClaims, bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return idClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idClaims{}, false
	}
	var claims idClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return idClaims{}, false
	}
	if claims.AccountID == "" {
		claims.AccountID = claims.Auth.ChatGPTAccountID
	}
	return claims, true
}

// ClaimsFromToken decodes the display/account claims from a JWT access_token or
// id_token. It does not verify the signature; callers use these only for display
// and provider-specific upstream headers, never for router-side authorization.
func ClaimsFromToken(token string) (email, accountID string, ok bool) {
	claims, ok := claimsFromIDToken(token)
	if !ok {
		return "", "", false
	}
	return claims.Email, claims.AccountID, true
}
