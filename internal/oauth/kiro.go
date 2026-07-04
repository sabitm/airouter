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

// Kiro token endpoints. The social endpoint is fixed; the OIDC host is
// regionalized from the connection's Region.
const kiroSocialUserAgent = "kiro-cli/1.0.0"

// Endpoint builders are vars so tests can point them at a local server; the
// production values are the real Kiro/AWS hosts.
var (
	kiroSocialRefreshURL = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"

	// kiroOIDCTokenURL returns the AWS SSO OIDC token endpoint for a region,
	// defaulting to us-east-1 when unset.
	kiroOIDCTokenURL = func(region string) string {
		if region == "" {
			region = "us-east-1"
		}
		return fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)
	}
)

// kiroTokenResponse is a Kiro refresh response. Kiro's social and OIDC endpoints
// return camelCase fields (unlike the snake_case standard OAuth2 response), and
// may echo a profileArn.
type kiroTokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
	ProfileArn   string `json:"profileArn"`
	Error        string `json:"error"`
	Message      string `json:"message"`
}

// refreshKiro refreshes a Kiro OAuth connection. It selects the branch by the
// connection's config, matching KIRO.md 8:
//
//   - clientId + clientSecret present -> AWS SSO OIDC token endpoint (JSON body)
//   - otherwise                       -> Kiro social refresh endpoint (JSON body)
//
// external_idp is not handled here: it uses a standard Microsoft form refresh,
// so refresh() routes it to the generic path. On success the access token,
// (rotated) refresh token, expiry, and any returned profileArn are written into
// c. A rejected refresh token surfaces as ErrInvalidGrant.
func refreshKiro(ctx context.Context, c *domain.OAuthCreds, now time.Time) error {
	if c.RefreshToken == "" {
		return fmt.Errorf("oauth: kiro refresh: no refresh token")
	}

	var url string
	var body []byte
	headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json"}

	if c.ClientID != "" && c.ClientSecret != "" {
		url = kiroOIDCTokenURL(c.Region)
		body, _ = json.Marshal(map[string]string{
			"clientId":     c.ClientID,
			"clientSecret": c.ClientSecret,
			"refreshToken": c.RefreshToken,
			"grantType":    "refresh_token",
		})
	} else {
		url = kiroSocialRefreshURL
		body, _ = json.Marshal(map[string]string{"refreshToken": c.RefreshToken})
		headers["User-Agent"] = kiroSocialUserAgent
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("oauth: kiro refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := readLimited(resp.Body)

	var tr kiroTokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return fmt.Errorf("oauth: kiro refresh: decode %d: %w", resp.StatusCode, err)
	}
	// The OIDC endpoint reports an invalid refresh token as an error field; the
	// social endpoint may use a non-2xx status with a message. Treat either as a
	// hard invalid-grant so the connection is flagged for reconnect.
	if isKiroInvalidGrant(tr.Error) || (resp.StatusCode == http.StatusBadRequest && tr.AccessToken == "") {
		return ErrInvalidGrant
	}
	if tr.Error != "" {
		return fmt.Errorf("oauth: kiro refresh: %s: %s", tr.Error, tr.Message)
	}
	if tr.AccessToken == "" {
		return fmt.Errorf("oauth: kiro refresh: empty accessToken (HTTP %d)", resp.StatusCode)
	}

	c.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		c.RefreshToken = tr.RefreshToken
	}
	if tr.ExpiresIn > 0 {
		c.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	// Patch the profile ARN from the refresh response only when we do not already
	// have one, so a user-configured account-specific ARN is never overwritten.
	if c.ProfileArn == "" && tr.ProfileArn != "" {
		c.ProfileArn = tr.ProfileArn
	}
	return nil
}

func isKiroInvalidGrant(errCode string) bool {
	switch strings.ToLower(errCode) {
	case "invalid_grant", "invalidgrantexception", "accessdeniedexception":
		return true
	}
	return false
}
