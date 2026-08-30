// Package opencode implements the opencode.ai Zen backend: one provider row
// whose wire format depends on the model. Most models speak OpenAI Chat
// Completions; muse-spark models are OpenAI Responses-only. The free tier
// authenticates with the literal key "public" and gates on a client
// fingerprint, so every upstream request carries the opencode identity.
package opencode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// ZenBaseURL is the free tier: Bearer "public" plus client fingerprint.
	ZenBaseURL = "https://opencode.ai/zen/v1"
	// GoBaseURL is the paid tier: a real opencode.ai API key.
	GoBaseURL = "https://opencode.ai/zen/go/v1"
	// PublicKey is the zen tier's literal credential.
	PublicKey = "public"
	// UserAgent is the fingerprint gate: the zen console classifies any other
	// UA as unidentified traffic and rate-limits the free tier immediately.
	UserAgent = "opencode"

	ChatPath      = "/chat/completions"
	ResponsesPath = "/responses"
)

// IsResponsesModel reports whether the model is served by the Responses
// endpoint. muse-spark variants 500 on /chat/completions; every other model
// 500s on /responses, so the split is hard.
func IsResponsesModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "muse-spark")
}

// Tier classifies a provider base URL. Base URLs other than the two known ones
// (custom/self-hosted) are treated as zen-style: the fingerprint headers are
// required on the free tier and harmless elsewhere.
func Tier(baseURL string) string {
	if strings.Contains(baseURL, "/zen/go") {
		return "go"
	}
	return "zen"
}

// DeriveSessionID computes the x-opencode-session value for one conversation:
// stable across requests and restarts so prompt caching survives, but bound to
// the conversation content. The upstream keys cache reuse on this header, so a
// fresh id per request would forfeit caching while an account-stable id would
// collide unrelated conversations. hashSeed is a per-provider secret (the
// stored API key); transcript is the accumulated assistant text of the request
// body (empty on the first turn, which then derives a stable per-account id).
func DeriveSessionID(hashSeed, transcript string) string {
	sum := sha256.Sum256([]byte("opencode-session\x00" + hashSeed + "\x00" + transcript))
	return "ses_" + hex.EncodeToString(sum[:])
}

// NewRequestID returns a fresh x-opencode-request value; the upstream expects a
// unique id per request.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "msg_" + hex.EncodeToString(b[:])
}

// SessionSeedFromCreds picks the hash seed: a real API key when present, else
// the base URL. "public" is shared by every zen user, so transcript becomes
// the distinguishing input there.
func SessionSeedFromCreds(apiKey, baseURL string) string {
	if s := strings.TrimSpace(apiKey); s != "" {
		return s
	}
	return baseURL
}
