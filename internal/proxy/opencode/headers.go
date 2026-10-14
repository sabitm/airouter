package opencode

import (
	"net/http"
)

const (
	// DefaultClient is the client identity used by paid Zen, Go, and model probes.
	DefaultClient = "desktop"
	// FreeClientVersion is the free-tier CLI version. It is distinct from
	// UserAgent so paid and probe traffic keep the older default.
	FreeClientVersion = "1.19.0"
	// FreeUserAgent is the free-tier fallback User-Agent. It is the literal CLI
	// version only; runtime and SDK suffixes are not invented.
	FreeUserAgent = "opencode/" + FreeClientVersion
	// FreeClient is the free-tier fallback x-opencode-client.
	FreeClient = "cli"
	// SessionIDHeader mirrors the resolved x-opencode-session on free Zen only.
	SessionIDHeader = "x-opencode-session-id"
)

// FingerprintHeaders fills missing opencode identity headers with the paid and
// probe defaults. A versioned OpenCode UA and native-shaped request/session IDs
// are kept. Other UA values are replaced with UserAgent, generic sessions are
// mapped to native shape, and a missing or non-native request id is generated.
// sessionID is used when x-opencode-session is unset. Dashboard/model probes
// call this without request context. It does not add SessionIDHeader.
func FingerprintHeaders(h http.Header, sessionID string) {
	applyFingerprint(h, sessionID, UserAgent, DefaultClient, false)
}

// FingerprintFreeHeaders fills the same missing identity as FingerprintHeaders,
// but uses the free-tier CLI defaults. After the session is resolved it sets
// SessionIDHeader to that exact value. A caller-supplied mirror cannot replace
// or diverge from x-opencode-session.
func FingerprintFreeHeaders(h http.Header, sessionID string) {
	applyFingerprint(h, sessionID, FreeUserAgent, FreeClient, true)
}

func applyFingerprint(h http.Header, sessionID, fallbackUA, fallbackClient string, mirrorSession bool) {
	if h == nil {
		return
	}
	if !IsVersionedUserAgent(h.Get("User-Agent")) {
		h.Set("User-Agent", fallbackUA)
	}
	if h.Get("x-opencode-client") == "" {
		h.Set("x-opencode-client", fallbackClient)
	}
	if h.Get("x-opencode-project") == "" {
		h.Set("x-opencode-project", "global")
	}
	sid := h.Get("x-opencode-session")
	if sid == "" {
		sid = sessionID
	}
	if mapped := NativeSessionID(sid); mapped != "" {
		h.Set("x-opencode-session", mapped)
		sid = mapped
	}
	if !IsNativeRequestID(h.Get("x-opencode-request")) {
		h.Set("x-opencode-request", NewRequestID())
	}
	if !mirrorSession {
		return
	}
	// The mirror is metadata for the already resolved session. It is not a
	// second session source, so a conflicting or injected value cannot win.
	h.Del(SessionIDHeader)
	if IsNativeSessionID(sid) {
		h.Set(SessionIDHeader, sid)
	}
}
