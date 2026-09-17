package opencode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const maxIdentityBytes = 256

// Identity is the sanitized OpenCode-relevant client identity captured from
// the original ingress request. It never includes Authorization, API keys,
// cookies, or arbitrary headers/body metadata.
type Identity struct {
	UserAgent string
	Client    string
	Project   string
	Request   string
	// Session is the first valid client-provided session (explicit OpenCode
	// header, then generic session headers, then original-body fields). Empty
	// means the proxy must derive a fallback. Native-shaped values are kept;
	// other candidates are mapped to a native ses_ id in ResolveSession.
	Session string
}

var (
	sessionHeaderKeys = []string{
		"x-session-id",
		"session-id",
		"session_id",
		"x-amp-thread-id",
		"x-client-request-id",
	}
	bodySessionKeys = []string{
		"prompt_cache_key",
		"session_id",
		"conversation_id",
	}
)

// CaptureIdentity extracts OpenCode identity from the original ingress headers
// and JSON body. Call this before translation drops unknown fields. Invalid
// candidates are ignored; nothing is hashed when a value is valid.
func CaptureIdentity(h http.Header, body []byte) Identity {
	var id Identity
	if ua := headerValue(h, "User-Agent"); IsVersionedUserAgent(ua) {
		id.UserAgent = ua
	}
	id.Client = headerValue(h, "x-opencode-client")
	id.Project = headerValue(h, "x-opencode-project")
	if req := headerValue(h, "x-opencode-request"); IsNativeRequestID(req) {
		id.Request = req
	}
	id.Session = resolveClientSession(h, body)
	return id
}

func resolveClientSession(h http.Header, body []byte) string {
	if s := headerValue(h, "x-opencode-session"); s != "" {
		return s
	}
	for _, key := range sessionHeaderKeys {
		if s := headerValue(h, key); s != "" {
			return s
		}
	}
	return bodySession(body)
}

func bodySession(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	for _, key := range bodySessionKeys {
		if s := stringField(envelope[key]); s != "" {
			return s
		}
	}
	raw, ok := envelope["metadata"]
	if !ok {
		return ""
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	return stringField(meta["user_id"])
}

func stringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return NormalizeIdentity(s)
}

func headerValue(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	values := h.Values(key)
	if len(values) != 1 {
		return ""
	}
	return NormalizeIdentity(values[0])
}

// NormalizeIdentity trims whitespace, rejects empty/oversized values, and
// rejects ASCII controls (including CR/LF) so a client cannot inject headers.
// Valid values are returned byte-for-byte after trimming.
func NormalizeIdentity(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxIdentityBytes {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c == 0x7F {
			return ""
		}
	}
	return v
}

// IsVersionedUserAgent reports whether ua is a syntactically valid versioned
// OpenCode User-Agent (opencode/<semver> with optional product suffixes).
// Bare "opencode" and strings that merely contain "opencode" are rejected.
func IsVersionedUserAgent(ua string) bool {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return false
	}
	const prefix = "opencode/"
	if !strings.HasPrefix(strings.ToLower(ua), prefix) {
		return false
	}
	rest := ua[len(prefix):]
	if rest == "" {
		return false
	}
	ver, _, _ := strings.Cut(rest, " ")
	return isOpenCodeVersion(ver)
}

func isOpenCodeVersion(ver string) bool {
	if ver == "" || strings.HasPrefix(ver, ".") || strings.HasSuffix(ver, ".") {
		return false
	}
	parts := strings.Split(ver, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// SanitizeIdentityHeaders drops invalid OpenCode identity values that may have
// been copied onto an upstream request so they cannot be forwarded.
func SanitizeIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	if values := h.Values("User-Agent"); len(values) > 0 {
		if len(values) != 1 {
			h.Del("User-Agent")
		} else {
			n := NormalizeIdentity(values[0])
			if !IsVersionedUserAgent(n) {
				h.Del("User-Agent")
			} else {
				h.Set("User-Agent", n)
			}
		}
	}
	for _, key := range []string{"x-opencode-client", "x-opencode-project", "x-opencode-request", "x-opencode-session"} {
		values := h.Values(key)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 {
			h.Del(key)
			continue
		}
		n := NormalizeIdentity(values[0])
		if n == "" {
			h.Del(key)
			continue
		}
		switch key {
		case "x-opencode-request":
			if !IsNativeRequestID(n) {
				h.Del(key)
				continue
			}
		case "x-opencode-session":
			if !IsNativeSessionID(n) {
				h.Del(key)
				continue
			}
		}
		h.Set(key, n)
	}
}

// ApplyIdentity writes captured OpenCode identity fields onto h. Empty fields
// are left unset so FingerprintHeaders can fill defaults / a derived session.
func ApplyIdentity(h http.Header, id Identity) {
	if id.UserAgent != "" {
		h.Set("User-Agent", id.UserAgent)
	}
	if id.Client != "" {
		h.Set("x-opencode-client", id.Client)
	}
	if id.Project != "" {
		h.Set("x-opencode-project", id.Project)
	}
	if IsNativeRequestID(id.Request) {
		h.Set("x-opencode-request", id.Request)
	}
	if sid := NativeSessionID(id.Session); sid != "" {
		h.Set("x-opencode-session", sid)
	}
}

// ResolveSession picks x-opencode-session: a valid client session wins and is
// mapped to native shape when needed; otherwise a namespaced transcript
// fallback (empty transcript on first turn).
func ResolveSession(id Identity, nonce string, providerID int64, baseURL, apiKey, transcript string) string {
	if sid := NativeSessionID(id.Session); sid != "" {
		return sid
	}
	return FallbackSessionID(nonce, providerID, baseURL, apiKey, transcript)
}

// FallbackSessionID derives an opaque ses_ id from the per-Proxy nonce,
// provider identity, and assistant transcript. Same inputs are stable; the
// nonce changes across Proxy instances so Zen/public first turns do not collide.
func FallbackSessionID(nonce string, providerID int64, baseURL, apiKey, transcript string) string {
	return DeriveSessionID(fallbackSeed(nonce, providerID, baseURL, apiKey), transcript)
}

func fallbackSeed(nonce string, providerID int64, baseURL, apiKey string) string {
	sum := sha256.Sum256([]byte(
		nonce + "\x00" +
			strconv.FormatInt(providerID, 10) + "\x00" +
			strings.TrimSpace(baseURL) + "\x00" +
			strings.TrimSpace(apiKey),
	))
	return hex.EncodeToString(sum[:])
}
