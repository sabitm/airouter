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
	"sync"
	"time"
)

const (
	// ZenBaseURL is the free tier: Bearer "public" plus client fingerprint.
	ZenBaseURL = "https://opencode.ai/zen/v1"
	// GoBaseURL is the paid tier: a real opencode.ai API key.
	GoBaseURL = "https://opencode.ai/zen/go/v1"
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

// Tier classifies a provider base URL. Base URLs other than the two known ones
// (custom/self-hosted) are treated as zen-style: the fingerprint headers are
// required on the free tier and harmless elsewhere.
func Tier(baseURL string) string {
	if strings.Contains(baseURL, "/zen/go") {
		return "go"
	}
	return "zen"
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
	now := uint64(time.Now().UnixMilli())
	idMu.Lock()
	if now != lastTimestamp {
		lastTimestamp = now
		idCounter = 0
	}
	idCounter++
	current := lastTimestamp*0x1000 + idCounter
	idMu.Unlock()
	var rb [identifierRandLen]byte
	_, _ = rand.Read(rb[:])
	var randPart [identifierRandLen]byte
	for i := 0; i < identifierRandLen; i++ {
		randPart[i] = identifierAlphabet[int(rb[i])%len(identifierAlphabet)]
	}
	return encodeTimeHex(current) + string(randPart[:])
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
