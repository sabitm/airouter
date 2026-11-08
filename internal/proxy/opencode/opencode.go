// Package opencode implements the opencode.ai backend: one provider row whose
// wire format depends on the model. The catalog SDK selects OpenAI Chat
// Completions, OpenAI Responses, or Anthropic Messages. The free tier
// authenticates with the literal key "public" and gates on a client
// fingerprint, so every upstream request carries the opencode identity.
package opencode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ZenBaseURL is the Zen model-discovery base. It is also an accepted legacy
	// inference alias; InferenceBaseURL maps it to the current API-family base.
	ZenBaseURL = "https://opencode.ai/inference/v1"
	// LegacyZenBaseURL is the previous free-tier base. Provider rows saved
	// before the move still store it; official aliases resolve at send time so
	// stored rows and custom URLs are never rewritten.
	LegacyZenBaseURL = "https://opencode.ai/zen/v1"
	// GoBaseURL is the current Go model-discovery base.
	GoBaseURL = "https://opencode.ai/inference/go/v1"
	// LegacyGoBaseURL is the previous Go base.
	LegacyGoBaseURL = "https://opencode.ai/zen/go/v1"

	// Current official inference bases, split by tier and API family. The proxy
	// selects the family from the target model's catalog SDK.
	ZenOpenAIBaseURL    = "https://opencode.ai/inference/openai/v1"
	ZenAnthropicBaseURL = "https://opencode.ai/inference/anthropic/v1"
	GoOpenAIBaseURL     = "https://opencode.ai/inference/go/openai/v1"
	GoAnthropicBaseURL  = "https://opencode.ai/inference/go/anthropic/v1"
	// PublicKey is the zen tier's literal credential.
	PublicKey = "public"
	// UserAgent is the default versioned OpenCode fingerprint. Bare "opencode"
	// is rejected by the free-tier gate; a current x.y.z version is required.
	UserAgent = "opencode/1.18.31"

	ChatPath      = "/chat/completions"
	ResponsesPath = "/responses"
	MessagesPath  = "/messages"

	sessionPrefix = "ses_"
	requestPrefix = "msg_"

	// Official OpenCode IDs are prefix + 26 characters: 12 lowercase hex
	// (timestamp_ms * 0x1000 + counter, complemented when descending) and 14
	// characters from identifierAlphabet. Session IDs are descending; request
	// IDs are ascending. Derived IDs keep this layout without using the clock.
	identifierLen      = 26
	identifierTimeLen  = 12
	identifierRandLen  = 14
	identifierAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

var (
	idMu          sync.Mutex
	lastTimestamp uint64
	idCounter     uint64
)

// IsResponsesModel reports whether the Zen catalog serves model on /responses.
// Tier-specific routing uses Endpoint. Unknown ids stay on chat.
func IsResponsesModel(model string) bool {
	return Endpoint(zenTier, model) == EndpointResponses
}

// Endpoint reports the upstream family for a tier and model. Unknown models
// stay on chat. Google rows also stay on chat; native thinkingConfig is deferred.
func Endpoint(tier, model string) string {
	spec, ok := Lookup(tier, model)
	return EndpointFor(spec, ok)
}

// Tier classifies a provider base URL. Recognized official aliases use their
// exact tier; custom URLs keep the historical Go path-substring behavior and
// otherwise default to zen-style fingerprinting.
func Tier(baseURL string) string {
	if tier, ok := officialTier(baseURL); ok {
		return tier
	}
	if isCustomGoBase(baseURL) {
		return goTier
	}
	return zenTier
}

// isCustomGoBase keeps the historical custom-URL tier guess for both the old
// and current Go path prefixes. Official bases never reach it.
func isCustomGoBase(baseURL string) bool {
	return strings.Contains(baseURL, "/zen/go") || strings.Contains(baseURL, "/inference/go")
}

// InferenceBaseURL returns the current wire base for a stored provider base and
// backend path. Recognized official legacy or family-specific aliases resolve to
// the tier and API family. Any other URL is returned unchanged, so custom
// mirrors, ports, userinfo, and near-match paths are never rewritten.
func InferenceBaseURL(baseURL, path string) string {
	tier, ok := officialTier(baseURL)
	if !ok {
		return baseURL
	}
	family := apiFamily(path)
	if family == "" {
		return baseURL
	}
	if tier == goTier {
		if family == "anthropic" {
			return GoAnthropicBaseURL
		}
		return GoOpenAIBaseURL
	}
	if family == "anthropic" {
		return ZenAnthropicBaseURL
	}
	return ZenOpenAIBaseURL
}

// EffectiveBaseURL returns the current model-discovery base for a stored
// provider base URL. Recognized official aliases, including the retired Zen
// and Go bases, resolve by tier with trailing slashes normalized. Custom
// mirrors, ports, userinfo, and near-match paths are returned unchanged.
func EffectiveBaseURL(baseURL string) string {
	tier, ok := officialTier(baseURL)
	if !ok {
		return baseURL
	}
	if tier == goTier {
		return GoBaseURL
	}
	return ZenBaseURL
}

func apiFamily(path string) string {
	switch path {
	case ChatPath, ResponsesPath:
		return "openai"
	case MessagesPath:
		return "anthropic"
	default:
		return ""
	}
}

// officialTier recognizes exact official bases only: HTTPS, opencode.ai,
// default port, no userinfo, query, fragment, or trailing slash. The path must
// be a documented console base, a known legacy base, or an API-family base.
func officialTier(baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return "", false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return "", false
	}
	if port := u.Port(); port != "" && port != "443" {
		return "", false
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	switch path {
	case "/inference", "/inference/v1", "/zen/v1", "/inference/openai/v1", "/inference/anthropic/v1":
		return zenTier, true
	case "/inference/go", "/inference/go/v1", "/zen/go/v1", "/inference/go/openai/v1", "/inference/go/anthropic/v1":
		return goTier, true
	}
	return "", false
}

// DeriveSessionID hashes seed and transcript into a native-shaped ses_ id.
// The proxy must namespace seed with a per-Proxy nonce and provider identity
// so a shared credential such as "public" cannot collide first-turn sessions.
func DeriveSessionID(hashSeed, transcript string) string {
	sum := sha256.Sum256([]byte("opencode-session\x00" + hashSeed + "\x00" + transcript))
	return sessionPrefix + identifierFromDigest(sum[:])
}

// NativeSessionID returns a wire-ready OpenCode session id. Native-shaped
// values are preserved. Other non-empty candidates are mapped deterministically
// so client affinity stays stable while the value passes the shape gate.
func NativeSessionID(candidate string) string {
	if candidate == "" {
		return ""
	}
	if IsNativeSessionID(candidate) {
		return candidate
	}
	return DeriveSessionID("client-session", candidate)
}

// NewRequestID returns a fresh native-shaped x-opencode-request value.
func NewRequestID() string {
	return requestPrefix + newIdentifier()
}

// IsNativeSessionID reports whether id is a ses_ OpenCode identifier.
func IsNativeSessionID(id string) bool {
	return isNativeID(sessionPrefix, id)
}

// IsNativeRequestID reports whether id is a msg_ OpenCode identifier.
func IsNativeRequestID(id string) bool {
	return isNativeID(requestPrefix, id)
}

func isNativeID(prefix, id string) bool {
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	payload := id[len(prefix):]
	if len(payload) != identifierLen {
		return false
	}
	for i := 0; i < identifierTimeLen; i++ {
		c := payload[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	for i := identifierTimeLen; i < identifierLen; i++ {
		if strings.IndexByte(identifierAlphabet, payload[i]) < 0 {
			return false
		}
	}
	return true
}

func identifierFromDigest(sum []byte) string {
	timePart := hex.EncodeToString(sum[:6])
	var randPart [identifierRandLen]byte
	for i := 0; i < identifierRandLen; i++ {
		randPart[i] = identifierAlphabet[int(sum[6+i])%len(identifierAlphabet)]
	}
	return timePart + string(randPart[:])
}

func newIdentifier() string {
	return newIdentifierAt(uint64(time.Now().UnixMilli()), false)
}

// NewSessionID returns a fresh native-shaped ses_ id using the official
// descending timestamp/counter layout. The leading 12 hex digits are the
// complement of (unix milli << 12) + counter, truncated to 48 bits. A restart
// or later eviction of a mapped id starts a new value; this function does not
// restore affinity.
func NewSessionID() string {
	return sessionPrefix + newIdentifierAt(uint64(time.Now().UnixMilli()), true)
}

func newIdentifierAt(now uint64, descending bool) string {
	current := nextIdentifierValue(now)
	if descending {
		current = ^current
	}
	var rb [identifierRandLen]byte
	_, _ = rand.Read(rb[:])
	return encodeTimeHex(current) + randomIdentifierTail(rb[:])
}

func nextIdentifierValue(now uint64) uint64 {
	idMu.Lock()
	defer idMu.Unlock()
	if now != lastTimestamp {
		lastTimestamp = now
		idCounter = 0
	}
	idCounter++
	return lastTimestamp*0x1000 + idCounter
}

func randomIdentifierTail(raw []byte) string {
	var randPart [identifierRandLen]byte
	for i := 0; i < identifierRandLen && i < len(raw); i++ {
		randPart[i] = identifierAlphabet[int(raw[i])%len(identifierAlphabet)]
	}
	return string(randPart[:])
}

func encodeTimeHex(v uint64) string {
	var b [6]byte
	b[0] = byte(v >> 40)
	b[1] = byte(v >> 32)
	b[2] = byte(v >> 24)
	b[3] = byte(v >> 16)
	b[4] = byte(v >> 8)
	b[5] = byte(v)
	return hex.EncodeToString(b[:])
}
