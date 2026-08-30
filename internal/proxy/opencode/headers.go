package opencode

import (
	"net/http"
	"strings"
)

// FingerprintHeaders sets the opencode client identity on an upstream request.
// The zen tier classifies requests without this fingerprint as unidentified
// traffic and rate-limits the free tier, so it is applied to every opencode
// request (harmless on the go tier). A client User-Agent already containing
// "opencode" is preserved so a real opencode client's own version string wins.
// sessionID is the conversation-stable x-opencode-session value.
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
