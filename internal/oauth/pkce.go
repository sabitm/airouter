package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
)

// verifierLen is the PKCE code verifier length in bytes before base64url
// encoding, matching grok-cli / 9router (96 bytes -> 128 chars). RFC 7636
// permits 32-96 bytes; xAI's public client uses the high end.
const verifierLen = 96

// newVerifier returns a high-entropy random code verifier, base64url-encoded
// without padding (RFC 7636 S4.1).
func newVerifier() (string, error) {
	b := make([]byte, verifierLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challengeS256 returns the S256 code_challenge for a verifier (RFC 7636 S4.2):
// base64url(SHA-256(verifier)) without padding.
func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newState returns an opaque state parameter for CSRF protection during connect.
// 32 bytes -> 43-char base64url, mirroring the Claude Code client and 9router.
// claude.ai's consent flow rejected a shorter (16-byte/22-char) state as
// "Invalid request format"; matching the client's state length resolves it. The
// value is opaque and compared by string equality in handleCallback, so a longer
// state only adds entropy and is safe across all providers.
func newState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// loopbackPort extracts the TCP port from a loopback redirect URI. The connect
// flow binds a local server on this port to receive the authorization callback.
// Returns an error if the URI is not a loopback http URL with an explicit port.
func loopbackPort(redirectURI string) (int, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return 0, fmt.Errorf("oauth: parse redirect_uri: %w", err)
	}
	if u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return 0, fmt.Errorf("oauth: loopback redirect required, got %s", redirectURI)
	}
	// An explicit 0 is allowed: net.Listen treats it as "OS-assigned ephemeral
	// port" (used by tests). A missing port (empty string) is an error.
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0, fmt.Errorf("oauth: redirect_uri has no port: %s", redirectURI)
	}
	return port, nil
}

// reservedAuthParams are the standard authorization-code params set by
// AuthorizeURL itself; ExtraAuthParams must not override them (a preset that
// could set response_type=token, say, would break the flow).
var reservedAuthParams = map[string]bool{
	"response_type":         true,
	"client_id":             true,
	"redirect_uri":          true,
	"state":                 true,
	"scope":                 true,
	"code_challenge":        true,
	"code_challenge_method": true,
}

func reservedAuthParam(k string) bool { return reservedAuthParams[k] }
