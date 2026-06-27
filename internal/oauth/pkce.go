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
func newState() (string, error) {
	b := make([]byte, 16)
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
