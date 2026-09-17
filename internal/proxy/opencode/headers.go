package opencode

import (
	"net/http"
)

// FingerprintHeaders fills missing opencode identity headers. A versioned
// OpenCode UA and native-shaped request/session IDs are kept. Other UA values
// are replaced with UserAgent, generic sessions are mapped to native shape,
// and a missing or non-native request id is generated. sessionID is used when
// x-opencode-session is unset. Dashboard/model probes call this without
// request context.
func FingerprintHeaders(h http.Header, sessionID string) {
	ua := h.Get("User-Agent")
	if !IsVersionedUserAgent(ua) {
		h.Set("User-Agent", UserAgent)
	}
	if h.Get("x-opencode-client") == "" {
		h.Set("x-opencode-client", "desktop")
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
	}
	if !IsNativeRequestID(h.Get("x-opencode-request")) {
		h.Set("x-opencode-request", NewRequestID())
	}
}
