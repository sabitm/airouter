package opencode

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"airouter/internal/domain"
)

const (
	// ConsoleOrigin is the fixed OpenCode Console origin. Dashboard forms do
	// not configure it. Tests may replace ConsoleOrigin and must restore it.
	// Live acceptance of this origin for device grants is the documented
	// contract; this process does not call it during tests.
	defaultConsoleOrigin = "https://opencode.ai/console"
	// ConsoleClientID is the device client id in the official OpenCode Console
	// plugin. No linked source documents a different id that Console accepts
	// for org-scoped grants, so this value is fixed. Live acceptance is unverified.
	ConsoleClientID = "opencode-cli"
	// ConsoleTokenPath is the device-code and refresh grant path.
	ConsoleTokenPath = "/auth/device/token"
	// ConsoleDevicePath is the device authorization start path.
	ConsoleDevicePath = "/auth/device/code"
	// defaultGoUsageURL is the Console-authenticated Go allowance endpoint from
	// OpenCode usage-limits.ts. That file sends Bearer and x-opencode-org-id
	// and treats HTTP 403 as no Go allowance. 9router's /inference/go/v1/usage
	// is an API-key usage URL, not the Console login contract. Live equivalence
	// of the two paths is unverified.
	defaultGoUsageURL = "https://opencode.ai/zen/go/v1/usage"
)

// ConsoleOrigin is the production Console origin. Tests may replace it.
var ConsoleOrigin = defaultConsoleOrigin

// GoUsageURL is the production Go allowance probe. Tests may replace it.
// It is not read from dashboard forms.
var GoUsageURL = defaultGoUsageURL

// ConsoleTokenURL is the fixed refresh and device-token URL for the current
// ConsoleOrigin. Callers must not substitute a credential TokenURL.
func ConsoleTokenURL() string {
	return strings.TrimRight(ConsoleOrigin, "/") + ConsoleTokenPath
}

// ConsoleDeviceURL is the fixed device-code URL.
func ConsoleDeviceURL() string {
	return strings.TrimRight(ConsoleOrigin, "/") + ConsoleDevicePath
}

// ConsoleUserURL is the fixed account lookup URL.
func ConsoleUserURL() string {
	return strings.TrimRight(ConsoleOrigin, "/") + "/api/user"
}

// ConsoleOrgsURL is the fixed workspace list URL.
func ConsoleOrgsURL() string {
	return strings.TrimRight(ConsoleOrigin, "/") + "/api/orgs"
}

// IsOfficialGoBase reports whether baseURL is an exact official Go alias.
func IsOfficialGoBase(baseURL string) bool {
	tier, ok := officialTier(baseURL)
	return ok && tier == goTier
}

// IsOfficialGoRequestURL reports whether raw is an HTTPS request to opencode.ai
// on an official Go path. Zen, Console, and other same-origin paths are false.
// Query strings are allowed; userinfo, a non-default port, and a fragment are not.
func IsOfficialGoRequestURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return false
	}
	if u.Fragment != "" || !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return false
	}
	if port := u.Port(); port != "" && port != "443" {
		return false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path == "/inference/go" || strings.HasPrefix(path, "/inference/go/") ||
		path == "/zen/go" || strings.HasPrefix(path, "/zen/go/")
}

// ApplyConsoleAuth sets the Console inference credential from one snapshot.
// Caller-supplied workspace identity is removed. The access token is not read
// from a later lookup.
func ApplyConsoleAuth(h http.Header, creds *domain.OAuthCreds) {
	if h == nil || creds == nil {
		return
	}
	h.Del("x-opencode-org-id")
	h.Del("x-api-key")
	h.Set("Authorization", "Bearer "+creds.AccessToken)
	if creds.OpencodeOrgID != "" {
		h.Set("x-opencode-org-id", creds.OpencodeOrgID)
	}
}

// RejectUnsafeSend fails closed when a Console credential, or any OpenCode
// OAuth row, would be sent somewhere other than an official Go HTTPS endpoint.
// API-key and Zen rows are unchanged. The error text does not include tokens.
func RejectUnsafeSend(p *domain.Provider, rawURL string) error {
	marked := p != nil && p.OAuthCreds != nil && p.OAuthCreds.OpencodeConsoleAuth
	oauthOpencode := p != nil && p.Protocol == domain.ProtocolOpencode && p.Method() == domain.AuthOAuth
	if !marked && !oauthOpencode {
		return nil
	}
	if !marked || !oauthOpencode || p.Auth() != domain.AuthBearer {
		return errors.New("opencode: console credentials are not valid for this provider")
	}
	if strings.TrimSpace(p.OAuthCreds.AccessToken) == "" ||
		strings.TrimSpace(p.OAuthCreds.RefreshToken) == "" ||
		strings.TrimSpace(p.OAuthCreds.OpencodeOrgID) == "" {
		return errors.New("opencode: console credentials are incomplete")
	}
	if !IsOfficialGoBase(p.BaseURL) || !IsOfficialGoRequestURL(rawURL) {
		return errors.New("opencode: console credentials are only sent to official Go endpoints")
	}
	return nil
}

// ValidateConsoleImport rejects an imported or config-edited row that would
// store Console credentials for a non-Go destination or a non-Console grant.
// The error text does not include tokens or device codes.
func ValidateConsoleImport(p *domain.Provider) error {
	if p == nil || p.OAuthCreds == nil || !p.OAuthCreds.OpencodeConsoleAuth {
		if p != nil && p.Protocol == domain.ProtocolOpencode && p.Method() == domain.AuthOAuth {
			return errors.New("OpenCode OAuth requires a Console connection")
		}
		return nil
	}
	if p.Protocol != domain.ProtocolOpencode || p.Method() != domain.AuthOAuth || p.Auth() != domain.AuthBearer {
		return errors.New("OpenCode Console credentials require an OAuth Go provider")
	}
	if strings.TrimSpace(p.APIKey) != "" {
		return errors.New("OpenCode Console credentials cannot be combined with an API key")
	}
	if !IsOfficialGoBase(p.BaseURL) {
		return errors.New("OpenCode Console credentials require an official Go base URL")
	}
	c := p.OAuthCreds
	if strings.TrimSpace(c.AccessToken) == "" || strings.TrimSpace(c.RefreshToken) == "" || strings.TrimSpace(c.OpencodeOrgID) == "" {
		return errors.New("OpenCode Console credentials are incomplete")
	}
	if c.ClientID != "" && c.ClientID != ConsoleClientID {
		return errors.New("OpenCode Console client id is not accepted")
	}
	if c.TokenURL != "" && !sameConsoleURL(c.TokenURL, defaultConsoleOrigin+ConsoleTokenPath) {
		return errors.New("OpenCode Console token URL is not the official endpoint")
	}
	// Refresh always posts to the fixed Console URL. A stored TokenURL is not
	// used, but import still rejects any other value so a later reader cannot
	// mistake it for a configurable destination.
	if c.RefreshURL != "" {
		return errors.New("OpenCode Console refresh URL is not configurable")
	}
	if c.AuthURL != "" && !sameConsoleURL(c.AuthURL, defaultConsoleOrigin) {
		return errors.New("OpenCode Console authorization URL is not the official endpoint")
	}
	return nil
}

func sameConsoleURL(got, want string) bool {
	left, err1 := url.Parse(got)
	right, err2 := url.Parse(want)
	if err1 != nil || err2 != nil || left.User != nil || left.RawQuery != "" || left.Fragment != "" {
		return false
	}
	if left.Scheme != "https" || !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	if port := left.Port(); port != "" && port != "443" {
		return false
	}
	return strings.TrimRight(left.EscapedPath(), "/") == strings.TrimRight(right.EscapedPath(), "/")
}

// ClientNoRedirect copies base and refuses redirects so a Console bearer is
// not forwarded to another path or host. A nil base returns nil.
func ClientNoRedirect(base *http.Client) *http.Client {
	if base == nil {
		return nil
	}
	clone := *base
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("opencode: redirect refused")
	}
	return &clone
}
