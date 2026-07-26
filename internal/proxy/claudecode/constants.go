// Package claudecode implements the Claude Code backend: an Anthropic Messages
// wire format spoken with the Claude Code CLI client identity, claude.ai OAuth,
// and an anti-ban tool cloak/decoy transform. It is backend-only and reuses the
// anthropic codec's encoder for the base body; cloak and identity are applied in
// the proxy prepare/header seams where the resolved token and per-request
// session id are visible. The codec id is distinct from anth-msg so every
// ingress translates through this prepare step rather than passing through.
package claudecode

// DefaultBaseURL is the Anthropic Messages API root the Claude Code CLI targets.
const DefaultBaseURL = "https://api.anthropic.com/v1"

// UpstreamPath is appended to the provider base URL. The ?beta=true suffix
// matches the Claude Code CLI request path; query strings are already supported
// by the forward path (Antigravity uses ?alt=sse).
const UpstreamPath = "/messages?beta=true"

const (
	// CLIVersion is the Claude Code client version impersonated upstream.
	CLIVersion = "2.1.92"
	// CCEntrypoint is the cc_entrypoint value in the billing header.
	CCEntrypoint = "sdk-cli"
	// UserAgent is the Claude Code CLI fingerprint sent as User-Agent.
	UserAgent = "claude-cli/2.1.92 (external, sdk-cli)"

	// AnthropicVersion is the anthropic-version header value.
	AnthropicVersion = "2023-06-01"
	// AnthropicBeta is the full beta-flag list copied verbatim from the Claude
	// Code CLI profile. Some flags are forward-dated; keeping them faithfully is
	// part of the client fingerprint.
	AnthropicBeta = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,advanced-tool-use-2025-11-20,effort-2025-11-24,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28"

	// ToolSuffix is appended to non-native client tools when cloaking. Matches
	// the Antigravity cloak suffix; the decloak strips exactly one occurrence.
	ToolSuffix = "_ide"

	// OAuthTokenMarker identifies a Claude OAuth access token. Cloaking is gated
	// on the resolved upstream token containing this marker, mirroring 9router's
	// sk-ant-oat check. A static apikey provider gets identity headers but no
	// cloak.
	OAuthTokenMarker = "sk-ant-oat"

	// SessionIDHeader carries the per-request session id and must match
	// metadata.user_id.session_id in the request body.
	SessionIDHeader = "X-Claude-Code-Session-Id"
)

// IdentityHeaders returns the Claude Code CLI header fingerprint applied on top
// of the auth header. OS/Arch are static (MacOS/arm64) like the Antigravity IDE
// profile: the fingerprint matches the impersonated client, not the proxy host.
// Callers set these after copying client headers so they overwrite forwarded
// values the Anthropic backend would reject.
func IdentityHeaders() map[string]string {
	return map[string]string{
		"anthropic-version":                         AnthropicVersion,
		"anthropic-beta":                            AnthropicBeta,
		"anthropic-dangerous-direct-browser-access": "true",
		"User-Agent":                                UserAgent,
		"X-App":                                     "cli",
		"X-Stainless-Helper-Method":                 "stream",
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Runtime-Version":               "v24.14.0",
		"X-Stainless-Package-Version":               "0.80.0",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Arch":                          "arm64",
		"X-Stainless-Os":                            "MacOS",
		"X-Stainless-Timeout":                       "600",
	}
}

// StaticModels is the fallback model catalog for dashboard autocomplete when the
// live /models probe fails. Copied verbatim from the Claude Code provider
// registry.
var StaticModels = []string{
	"claude-fable-5",
	"claude-sonnet-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-haiku-4-5-20251001",
}
