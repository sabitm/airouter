package oauth

import (
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

// tokenResponse is the subset of an OAuth/OIDC token response we persist.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
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
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", c.ClientID)
	form.Set("refresh_token", c.RefreshToken)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: refresh request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := readLimited(resp.Body)

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("oauth: refresh: decode %d: %w", resp.StatusCode, err)
	}
	if tr.Error != "" {
		if tr.Error == "invalid_grant" || tr.Error == "invalid_request" {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: refresh: %s: %s", tr.Error, tr.ErrorDescription)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("oauth: refresh: empty access_token (HTTP %d)", resp.StatusCode)
	}

	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		c.IDToken = tr.IDToken
		if email, ok := emailFromIDToken(tr.IDToken); ok {
			c.Email = email
		}
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// emailFromIDToken extracts the email claim from a JWT id_token's payload without
// verifying its signature. airouter is not the token's audience; the email is
// used only for display, so an unverified claim is acceptable (matches 9router).
func emailFromIDToken(idToken string) (string, bool) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	return claims.Email, claims.Email != ""
}
