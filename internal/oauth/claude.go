package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"airouter/internal/domain"
)

// exchangeClaudeCode posts the Claude authorization-code grant as a JSON body
// (the claude.ai token endpoint requires JSON, not form-urlencoded). The code
// may carry an echoed state after '#'; that takes precedence over the connect
// state in the request body, matching the official CLI. verifier is the PKCE
// code_verifier. baseURL overrides the token URL in tests (empty in production).
func exchangeClaudeCode(ctx context.Context, c *domain.OAuthCreds, code, verifier, state, baseURL string) error {
	authCode := code
	codeState := ""
	if i := strings.Index(code, "#"); i >= 0 {
		authCode = code[:i]
		codeState = code[i+1:]
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          authCode,
		"state":         orDefault(codeState, state),
		"client_id":     c.ClientID,
		"redirect_uri":  c.RedirectURI,
		"code_verifier": verifier,
	})
	tokenURL := c.TokenURL
	if baseURL != "" {
		tokenURL = baseURL + "/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: claude exchange request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("oauth: claude exchange: decode %d: %w", resp.StatusCode, err)
	}
	if code, desc := parseTokenError(tr.Error); code != "" {
		if desc == "" {
			desc = tr.ErrorDescription
		}
		return fmt.Errorf("oauth: claude exchange: %s: %s", code, desc)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("oauth: claude exchange: empty access_token (HTTP %d)", resp.StatusCode)
	}
	applyExchangeToken(c, tr, time.Now())
	return nil
}

func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// applyExchangeToken populates creds from a successful authorization-code token
// response. Shared by the generic form exchange and the Claude JSON exchange so
// the id_token claim extraction and expiry math stay in one place.
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
