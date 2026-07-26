package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ApplyOAuthCloaking applies the Claude Code anti-ban transform to an encoded
// Anthropic Messages body. It is a no-op unless the resolved token is a Claude
// OAuth token (IsOAuthToken), so a static apikey provider gets identity headers
// but no cloak. Steps, in 9router's order:
//
//  1. Compute the billing header cch over the pre-injection body.
//  2. Inject the billing marker as system[0], preserving existing system blocks.
//  3. Inject metadata.user_id (seed stable per account, sessionID per request).
//  4. CloakTools: suffix client tools, append decoys, rewrite history/choice.
//
// seed feeds the fake device_id/account_uuid and should be stable across access
// token refreshes (refresh token, falling back to the access token). An invalid
// body is a terminal pre-commit error so the attempt fails over cleanly.
func ApplyOAuthCloaking(body []byte, apiKey, sessionID, seed string) ([]byte, error) {
	if !IsOAuthToken(apiKey) {
		return body, nil
	}
	var m messagesBody
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("claude-code: decode for cloak: %w", err)
	}
	injectSystemBilling(&m, GenerateBillingHeader(body))
	if m.Metadata == nil {
		m.Metadata = &wireMetadata{}
	}
	if m.Metadata.UserID == "" {
		m.Metadata.UserID = GenerateFakeUserID(sessionID, seed)
	}
	CloakTools(&m)
	return json.Marshal(m)
}

// injectSystemBilling inserts the billing marker as the first system text block.
// Idempotent: a system[0] that already carries a billing header is left alone.
// A string system is promoted to [billing, {text: <string>}]; an absent system
// becomes [billing]; an array system is prepended.
func injectSystemBilling(m *messagesBody, billingText string) {
	billing := wireBlock{Type: "text", Text: billingText}
	sys := m.System
	switch {
	case len(sys) == 0:
		m.System, _ = json.Marshal([]wireBlock{billing})
	case sys[0] == '[':
		var blocks []wireBlock
		if json.Unmarshal(sys, &blocks) != nil {
			m.System, _ = json.Marshal([]wireBlock{billing})
			return
		}
		if len(blocks) > 0 && blocks[0].Type == "text" && strings.HasPrefix(blocks[0].Text, "x-anthropic-billing-header:") {
			return
		}
		m.System, _ = json.Marshal(append([]wireBlock{billing}, blocks...))
	case sys[0] == '"':
		var s string
		_ = json.Unmarshal(sys, &s)
		m.System, _ = json.Marshal([]wireBlock{billing, {Type: "text", Text: s}})
	default:
		m.System, _ = json.Marshal([]wireBlock{billing})
	}
}
