package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

// claudeTokenResponse is the Claude token envelope. account and organization
// are the only identity fields persisted. A missing account is not invented.
type claudeTokenResponse struct {
	AccessToken           string          `json:"access_token"`
	RefreshToken          string          `json:"refresh_token"`
	ExpiresIn             int             `json:"expires_in"`
	RefreshTokenExpiresIn int             `json:"refresh_token_expires_in"`
	Scope                 string          `json:"scope"`
	Error                 json.RawMessage `json:"error"`
	ErrorDescription      string          `json:"error_description"`
	Account               *struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

// exchangeClaudeCode posts the Claude authorization-code grant as JSON.
// The code may carry an echoed state after '#'. That state must match the
// connect state. A different echoed state is rejected. An absent echo uses the
// connect state. It does not accept an arbitrary override.
func exchangeClaudeCode(ctx context.Context, c *domain.OAuthCreds, code, verifier, state, baseURL string) error {
	authCode := code
	codeState := ""
	if i := strings.Index(code, "#"); i >= 0 {
		authCode = code[:i]
		codeState = code[i+1:]
	}
	if codeState != "" && state != "" && codeState != state {
		return fmt.Errorf("oauth: claude exchange: state mismatch")
	}
	bodyState := state
	if codeState != "" {
		bodyState = codeState
	}
	redirect := c.RedirectURI
	if redirect == "" {
		redirect = claudecode.ManualRedirectURL
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          authCode,
		"state":         bodyState,
		"client_id":     c.ClientID,
		"redirect_uri":  redirect,
		"code_verifier": verifier,
	})
	tokenURL := c.TokenURL
	if baseURL != "" {
		tokenURL = baseURL + "/token"
	}
	raw, status, err := postClaudeJSON(ctx, tokenURL, body)
	if err != nil {
		return fmt.Errorf("oauth: claude exchange request: %w", err)
	}
	var tr claudeTokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("oauth: claude exchange: decode %d: %w", status, err)
	}
	if code, desc := parseTokenError(tr.Error); code != "" {
		if desc == "" {
			desc = tr.ErrorDescription
		}
		return fmt.Errorf("oauth: claude exchange: %s: %s", code, desc)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("oauth: claude exchange: HTTP %d", status)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("oauth: claude exchange: empty access_token (HTTP %d)", status)
	}
	applyClaudeToken(c, tr, time.Now())
	if err := ensureClaudeDeviceID(c); err != nil {
		return err
	}
	if c.ClaudeAccountUUID == "" {
		discoverClaudeProfile(ctx, c)
	}
	return nil
}

// refreshClaude posts the Claude refresh grant. Scope is the granted scope
// list when one was stored. A connection with no stored grant is not expanded
// to the current preset scopes. The device id is not derived from the refresh
// token and is not rotated.
func refreshClaude(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c.RefreshToken == "" {
		return fmt.Errorf("oauth: claude refresh: no refresh token")
	}
	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": c.RefreshToken,
		"client_id":     c.ClientID,
	}
	if scopes := strings.TrimSpace(c.ClaudeScopes); scopes != "" {
		body["scope"] = scopes
	}
	rawBody, _ := json.Marshal(body)
	respBody, status, err := postClaudeJSON(ctx, c.TokenURL, rawBody)
	if err != nil {
		return fmt.Errorf("oauth: claude refresh request: %w", err)
	}
	badStatus := status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden
	var tr claudeTokenResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		if badStatus {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: claude refresh: decode %d: %w", status, err)
	}
	code, desc := parseTokenError(tr.Error)
	if code != "" {
		if isInvalidGrantCode(code) {
			return ErrInvalidGrant
		}
		if desc == "" {
			desc = tr.ErrorDescription
		}
		return fmt.Errorf("oauth: claude refresh: %s: %s", code, desc)
	}
	if status < 200 || status >= 300 {
		if badStatus {
			return ErrInvalidGrant
		}
		return fmt.Errorf("oauth: claude refresh: HTTP %d", status)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("oauth: claude refresh: empty access_token (HTTP %d)", status)
	}
	applyClaudeToken(c, tr, now)
	return ensureClaudeDeviceID(c)
}

// applyExchangeToken populates creds from a successful authorization-code token
// response. Shared by the generic form exchange and older callers so the
// id_token claim extraction and expiry math stay in one place.
func applyExchangeToken(c *domain.OAuthCreds, tr tokenResponse, now time.Time) {
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
}

func applyClaudeToken(c *domain.OAuthCreds, tr claudeTokenResponse, now time.Time) {
	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	if tr.RefreshTokenExpiresIn > 0 {
		c.ClaudeRefreshExpiresAt = now.Add(time.Duration(tr.RefreshTokenExpiresIn) * time.Second).Unix()
	}
	if tr.Scope != "" {
		c.ClaudeScopes = strings.Join(strings.Fields(tr.Scope), " ")
	}
	if tr.Account != nil {
		if tr.Account.UUID != "" {
			c.ClaudeAccountUUID = tr.Account.UUID
			c.AccountID = tr.Account.UUID
		}
		if tr.Account.EmailAddress != "" {
			c.Email = tr.Account.EmailAddress
		}
	}
	if tr.Organization != nil && tr.Organization.UUID != "" {
		c.ClaudeOrgUUID = tr.Organization.UUID
		c.OrganizationID = tr.Organization.UUID
	}
}

// randRead is the entropy source for a new device id. Tests replace it to
// prove a failure is returned instead of an invented id.
var randRead = rand.Read

func ensureClaudeDeviceID(c *domain.OAuthCreds) error {
	if c == nil || isDeviceID(c.ClaudeDeviceID) {
		return nil
	}
	var b [32]byte
	if _, err := randRead(b[:]); err != nil {
		return fmt.Errorf("oauth: claude device id: %w", err)
	}
	c.ClaudeDeviceID = hex.EncodeToString(b[:])
	return nil
}

func isDeviceID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// discoverClaudeProfile fills a missing account UUID from the profile endpoint.
// Failure leaves the account empty. It does not change authorize endpoints and
// does not send a live inference request.
func discoverClaudeProfile(ctx context.Context, c *domain.OAuthCreds) {
	if c == nil || c.AccessToken == "" || c.ClaudeAccountUUID != "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudecode.ProfileURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("User-Agent", claudecode.ProfileUserAgent())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return
	}
	var profile struct {
		Account *struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
		Organization *struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if json.Unmarshal(raw, &profile) != nil || profile.Account == nil || profile.Account.UUID == "" {
		return
	}
	c.ClaudeAccountUUID = profile.Account.UUID
	c.AccountID = profile.Account.UUID
	if profile.Account.EmailAddress != "" && c.Email == "" {
		c.Email = profile.Account.EmailAddress
	}
	if profile.Organization != nil && profile.Organization.UUID != "" {
		c.ClaudeOrgUUID = profile.Organization.UUID
		c.OrganizationID = profile.Organization.UUID
	}
}

func postClaudeJSON(ctx context.Context, tokenURL string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)
	return raw, resp.StatusCode, nil
}
