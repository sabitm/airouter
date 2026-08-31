package opencode

import (
	"net/http"
	"strings"
)

// FingerprintHeaders fills missing opencode identity headers. Existing values
// (a real client's UA/fingerprint, or sanitized fields applied by the proxy)
// are left in place. sessionID is used only when x-opencode-session is unset.
// Dashboard/model probes call this without request context.
func FingerprintHeaders(h http.Header, sessionID string) {
	ua := h.Get("User-Agent")
	if ua == "" || !strings.Contains(strings.ToLower(ua), UserAgent) {
		h.Set("User-Agent", UserAgent)
	}
	if h.Get("x-opencode-client") == "" {
		h.Set("x-opencode-client", "desktop")
	}
	if h.Get("x-opencode-project") == "" {
		h.Set("x-opencode-project", "global")
	}
	if h.Get("x-opencode-session") == "" && sessionID != "" {
		h.Set("x-opencode-session", sessionID)
	}
	// x-opencode-request is unique per send; the caller's fresh id is kept.
	if h.Get("x-opencode-request") == "" {
		h.Set("x-opencode-request", NewRequestID())
	}
}
