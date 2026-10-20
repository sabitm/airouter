package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"
)

// projectIDPattern is the CLI tagged project id check. A value that does not
// match is omitted. The proxy does not synthesize one.
var projectIDPattern = regexp.MustCompile(`^claude_proj_(?:staging_|local_)?01[1-9A-HJ-NP-Za-km-z]{22}$`)

// BillingInput is the verified attribution set for one CLI request.
// Optional fields are omitted unless they pass the CLI's own validators.
// OfficialHost is the actual first-party or Vertex transport, not a hostname guess.
type BillingInput struct {
	// Fingerprint is Qm(first user text, CLI version). It is appended only to
	// cc_version. It is never written into cch.
	Fingerprint string
	Entrypoint  string
	Workload    string
	// IsSubagent is sent only when a validated agent context says this is not
	// the main session. The proxy has no agent runtime, so callers leave it
	// false unless that context is present.
	IsSubagent bool
	// PreviousRequestID must match req_[A-Za-z0-9_-]{1,36}.
	PreviousRequestID string
	// PromptID must be a UUID. The CLI accepts any hex case.
	PromptID string
	// TurnOrigin must match [a-z][a-z_]{0,31}.
	TurnOrigin string
	// PromptIndex and TurnIndex are sent only when both are in range.
	// TurnIndex's minimum is 1. A nil pointer means absent.
	PromptIndex *int
	TurnIndex   *int
	// OfficialHost is true only for the verified first-party host or an actual
	// Vertex transport. A hostname that merely contains a Vertex marker is not
	// enough: the caller must set VertexTransport.
	OfficialHost    bool
	VertexTransport bool
	// FirstUserText is the first non-meta user text. Qm reads UTF-16 indexes
	// 4, 7, and 20.
	FirstUserText string
	// AttributionProject is sent only when both fields pass the CLI validators.
	// The proxy does not synthesize a project id.
	AttributionProjectID     string
	AttributionProjectSource string
	// Gateway is "1" or "env" only when the caller verified a gateway transport
	// whose host is not vouched. An empty value omits cc_gateway.
	Gateway string
}

// cchLiteral is the value GPo writes. It is not a fingerprint and is not patched.
const cchLiteral = "00000"

// BillingHeader builds the x-anthropic-billing-header system text.
// cch is the literal 00000 on a verified official host. Qm's 3-hex digest is
// appended only to cc_version.
func BillingHeader(in BillingInput) string {
	fp := in.Fingerprint
	if fp == "" {
		fp = Fingerprint(in.FirstUserText)
	}
	version := CLIVersion + "." + fp
	entry := in.Entrypoint
	if entry == "" {
		entry = "unknown"
	}
	official := in.OfficialHost || in.VertexTransport
	var b strings.Builder
	b.WriteString(BillingHeaderPrefix)
	b.WriteString(" cc_version=")
	b.WriteString(version)
	b.WriteString("; cc_entrypoint=")
	b.WriteString(entry)
	b.WriteByte(';')
	if official {
		b.WriteString(" cch=")
		b.WriteString(cchLiteral)
		b.WriteByte(';')
	}
	if in.Workload != "" {
		fmt.Fprintf(&b, " cc_workload=%s;", in.Workload)
	}
	if in.IsSubagent {
		b.WriteString(" cc_is_subagent=true;")
	}
	if validPrevReq(in.PreviousRequestID) && official && !in.VertexTransport {
		fmt.Fprintf(&b, " cc_prev_req=%s;", in.PreviousRequestID)
	}
	if validUUID(in.PromptID) && official && !in.VertexTransport {
		fmt.Fprintf(&b, " cc_prompt_id=%s;", in.PromptID)
	}
	if validTurnOrigin(in.TurnOrigin) && official && !in.VertexTransport {
		fmt.Fprintf(&b, " cc_turn_origin=%s;", in.TurnOrigin)
	}
	if official && !in.VertexTransport && inRange(in.PromptIndex, 0) && inRange(in.TurnIndex, 1) {
		fmt.Fprintf(&b, " cc_prompt_index=%d; cc_turn_index=%d;", *in.PromptIndex, *in.TurnIndex)
	}
	if in.Gateway == "1" || in.Gateway == "env" {
		fmt.Fprintf(&b, " cc_gateway=%s;", in.Gateway)
	}
	if validProject(in.AttributionProjectID, in.AttributionProjectSource) {
		fmt.Fprintf(&b, " cc_project=%s; cc_project_src=%s;", in.AttributionProjectID, in.AttributionProjectSource)
	}
	return b.String()
}

// Fingerprint matches Qm in Claude Code 2.1.303: take UTF-16 units at indexes
// 4, 7, and 20, replace a missing unit with "0", join those units into one
// UTF-16 string, then UTF-8 encode that string once. sha256(salt + chars +
// version) hex[:3] is the result. Joining before decoding is required: a
// selected high surrogate followed by a selected low surrogate is one scalar,
// while a surrogate whose neighbor was not selected becomes U+FFFD. A selected
// NUL unit stays NUL; a missing unit alone becomes the ASCII "0".
func Fingerprint(text string) string {
	units := utf16.Encode([]rune(text))
	chars := selectedUnits(units, 4, 7, 20)
	sum := sha256.Sum256([]byte(BillingFingerprintSalt + chars + CLIVersion))
	return hex.EncodeToString(sum[:])[:3]
}

func selectedUnits(units []uint16, indexes ...int) string {
	selected := make([]uint16, 0, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= len(units) {
			selected = append(selected, '0')
			continue
		}
		selected = append(selected, units[index])
	}
	return string(utf16.Decode(selected))
}

func validPrevReq(s string) bool {
	if len(s) < 5 || !strings.HasPrefix(s, "req_") || len(s) > 4+36 {
		return false
	}
	for _, c := range s[4:] {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func validTurnOrigin(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, c := range s {
		if i == 0 {
			if c < 'a' || c > 'z' {
				return false
			}
			continue
		}
		if (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

func inRange(n *int, min int) bool {
	return n != nil && *n >= min && *n <= 10000000
}

func validProject(id, source string) bool {
	if !projectIDPattern.MatchString(id) {
		return false
	}
	switch source {
	case "flag", "managed", "admin_default":
		return true
	default:
		return false
	}
}

// IsOfficialHost reports whether the base URL host is exactly api.anthropic.com.
// A substring, path, or userinfo trick does not match. An unparseable value is
// not official. Vertex is not inferred from the hostname; the caller sets
// BillingInput.VertexTransport only for an actual Vertex backend.
func IsOfficialHost(baseURL string) bool {
	host, ok := requestHost(baseURL)
	return ok && host == "api.anthropic.com"
}

func requestHost(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", false
	}
	return strings.ToLower(u.Hostname()), true
}
