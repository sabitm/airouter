package claudecode

import (
	"encoding/json"
	"strings"
)

// UserID is the metadata.user_id object the CLI serializes as a JSON string.
// AccountUUID is the provider account id, never a client-claimed value and
// never derived from a token. An empty account is serialized as "" because the
// CLI writes that field even when no account is known. DeviceID is the
// install-stable 32-byte hex id. SessionID is the conversation session, not a
// per-request id.
type UserID struct {
	DeviceID        string `json:"device_id"`
	AccountUUID     string `json:"account_uuid"`
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
}

// MarshalUserID returns the JSON string placed in metadata.user_id.
// DeviceID and SessionID are required. AccountUUID may be empty and is still
// written, matching the CLI fallback.
func MarshalUserID(id UserID) (string, error) {
	if !IsDeviceID(id.DeviceID) {
		return "", errIdentity("device_id")
	}
	if !validUUID(id.SessionID) {
		return "", errIdentity("session_id")
	}
	if id.ParentSessionID != "" && !validUUID(id.ParentSessionID) {
		return "", errIdentity("parent_session_id")
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type identityError string

func (e identityError) Error() string { return "claude-code: invalid " + string(e) }

func errIdentity(field string) error { return identityError(field) }

// SystemBlocks returns the prepared system array: billing (no cache), identity
// (default cache when caching is on), then caller instructions. Caller blocks
// keep their boundaries. Explicit caller markers and scopes are not rewritten
// and are not copied onto billing.
//
// Anthropic accepts at most four cache_control markers. When the caller already
// set a policy, this function adds none: identity and unmarked caller blocks
// stay unmarked. When the caller set no policy, it synthesizes the minimum
// bounded anchors: identity, and the last caller block when one exists. It does
// not mark every unmarked block.
//
// The Anthropic encoder owns unmarked system-array boundaries. This function
// must not flatten those blocks into one string, and it must not add markers
// that the encoder already counted as caller policy.
//
// cacheTTL is "" (ephemeral, no ttl), "5m", or "1h". Synthesized anchors omit
// scope; the pinned CLI emits scope only for an explicit global policy. Caching
// off omits synthesized cache_control. It does not remove a marker the caller
// already set.
func SystemBlocks(billing, identity string, caller []SystemBlock, cache bool, cacheTTL string) []map[string]any {
	var out []map[string]any
	if billing != "" {
		out = append(out, map[string]any{"type": "text", "text": billing})
	}
	identityIndex := -1
	if identity != "" {
		identityIndex = len(out)
		out = append(out, map[string]any{"type": "text", "text": identity})
	}
	firstCaller := len(out)
	for _, b := range caller {
		text := b.Text
		if strings.TrimSpace(text) == "" && b.CacheControl == nil {
			continue
		}
		block := map[string]any{"type": "text", "text": text}
		if b.CacheControl != nil {
			block["cache_control"] = cacheControlMap(b.CacheControl)
		}
		out = append(out, block)
	}
	if cache && !callerHasCache(caller) {
		anchorDefaultCache(out, identityIndex, firstCaller, cacheTTL)
	}
	return out
}

func callerHasCache(caller []SystemBlock) bool {
	for _, b := range caller {
		if b.CacheControl != nil {
			return true
		}
	}
	return false
}

// anchorDefaultCache adds at most two markers when the caller set none:
// the identity block, and the last caller block. Four is the Anthropic limit.
// Two leaves room for a later explicit marker without exceeding it here.
func anchorDefaultCache(blocks []map[string]any, identityIndex, firstCaller int, ttl string) {
	if identityIndex >= 0 && identityIndex < len(blocks) {
		if _, ok := blocks[identityIndex]["cache_control"]; !ok {
			blocks[identityIndex]["cache_control"] = cacheControl(ttl)
		}
	}
	if firstCaller < len(blocks) {
		last := len(blocks) - 1
		if _, ok := blocks[last]["cache_control"]; !ok {
			blocks[last]["cache_control"] = cacheControl(ttl)
		}
	}
}

// SystemBlock is one caller system text block after billing and identity
// removal. CacheControl nil means the caller set no marker on that block.
type SystemBlock struct {
	Text         string
	CacheControl *CacheMark
}

// CacheMark is one validated ephemeral cache marker.
type CacheMark struct {
	TTL   string
	Scope string
}

func cacheControl(ttl string) map[string]any {
	return cacheControlMap(&CacheMark{TTL: ttl})
}

func cacheControlMap(cc *CacheMark) map[string]any {
	if cc == nil {
		return nil
	}
	out := map[string]any{"type": "ephemeral"}
	if cc.TTL != "" {
		out["ttl"] = cc.TTL
	}
	if cc.Scope == "global" || cc.Scope == "org" {
		out["scope"] = cc.Scope
	}
	return out
}

// IsOAuthToken reports whether the effective resolved token is a Claude OAuth
// access token. Static API keys do not get the oauth beta solely from a
// stored credential flag.
func IsOAuthToken(token string) bool {
	return strings.Contains(token, "sk-ant-oat")
}

// IsDeviceID reports a CLI userID: 64 lowercase hex characters, 32 random bytes.
func IsDeviceID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
