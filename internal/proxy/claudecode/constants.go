// Package claudecode implements the Claude Code backend: an Anthropic Messages
// wire format spoken with the Claude Code CLI client identity and claude.ai
// OAuth. It is backend-only and reuses the anthropic codec for the base body.
// Identity, billing, and cache anchors are applied in the proxy prepare/header
// seams where the resolved token and session id are visible. The codec id is
// distinct from anth-msg so every ingress translates through this prepare step
// rather than passing through.
//
// The profile is pinned to Claude Code 2.1.303 (SDK 0.128.0). Official wire
// fidelity does not mean Anthropic supports this proxy. Agent runtime, cloud
// gateway, remote workers, telemetry, and experimental feature flags are out
// of scope.
package claudecode

import "runtime"

// DefaultBaseURL is the Anthropic Messages API root the Claude Code CLI targets.
const DefaultBaseURL = "https://api.anthropic.com/v1"

// UpstreamPath is appended to the provider base URL. The ?beta=true suffix
// matches the Claude Code CLI request path.
const UpstreamPath = "/messages?beta=true"

const (
	// CLIVersion is the pinned Claude Code client version.
	CLIVersion = "2.1.303"
	// SDKPackageVersion is the Anthropic SDK package version embedded in that CLI.
	SDKPackageVersion = "0.128.0"
	// DefaultEntrypoint is the V0 fallback when CLAUDE_CODE_ENTRYPOINT is unset.
	// The billing header uses "unknown" in that same case. The two defaults differ.
	DefaultEntrypoint = "cli"
	// UserAgentPrefix is the fixed CLI product token. Optional agent-sdk,
	// client-app, and workload suffixes stay inside the parentheses.
	UserAgentPrefix = "claude-cli/" + CLIVersion

	// AnthropicVersion is the anthropic-version header value.
	AnthropicVersion = "2023-06-01"

	// SessionIDHeader must match metadata.user_id.session_id.
	SessionIDHeader = "X-Claude-Code-Session-Id"

	// BillingHeaderPrefix is the system-text marker the CLI prepends.
	BillingHeaderPrefix = "x-anthropic-billing-header:"
	// BillingFingerprintSalt is the CLI salt (var oj in 2.1.303). Qm uses it
	// only for the cc_version suffix. cch stays the literal 00000.
	BillingFingerprintSalt = "59cf53e54c78"

	// IdentityCLI is the interactive CLI system line (nl).
	IdentityCLI = "You are Claude Code, Anthropic's official CLI for Claude."
	// IdentitySDK is the non-interactive line used when an append-system prompt
	// is present (zd). It is not implied by an SDK User-Agent suffix.
	IdentitySDK = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	// IdentityAgent is the non-interactive line without an append (qd).
	IdentityAgent = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

	// Official OAuth endpoints and the public CLI client id (2.1.303).
	AuthorizeURL        = "https://claude.com/cai/oauth/authorize"
	ConsoleAuthorizeURL = "https://platform.claude.com/oauth/authorize"
	TokenURL            = "https://platform.claude.com/v1/oauth/token"
	ManualRedirectURL   = "https://platform.claude.com/oauth/code/callback"
	ClientID            = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// DefaultScopes is iWo(): org:create_api_key and user:profile, unioned with
	// Dbt (profile, inference, sessions, mcp_servers, file_upload, and plugins
	// because PLUGINS_SCOPE_REGISTERED is true). A stored grant is not expanded
	// to this list. inferenceOnly and a custom OAuth client use narrower lists.
	DefaultScopes = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins"
	// InferenceOnlyScopes is pDr's inferenceOnly list: user:inference alone.
	InferenceOnlyScopes = "user:inference"

	// ProfileURL is the account/org profile endpoint used when a token response
	// omits account.uuid. A failed fetch does not invent an account id.
	ProfileURL = "https://api.anthropic.com/api/oauth/profile"

	// StainlessTimeoutSeconds matches the SDK default of 600000 ms, truncated.
	StainlessTimeoutSeconds = "600"
)

// Beta flag names from the 2.1.303 registry. Registry presence is not evidence
// that a flag is always sent. Selection is feature- and model-gated.
const (
	BetaClaudeCode      = "claude-code-20250219"
	BetaOAuth           = "oauth-2025-04-20"
	BetaInterleaved     = "interleaved-thinking-2025-05-14"
	BetaLongContext     = "context-1m-2025-08-07"
	BetaContextMgmt     = "context-management-2025-06-27"
	BetaPromptCache     = "prompt-caching-scope-2026-01-05"
	BetaStructured      = "structured-outputs-2025-12-15"
	BetaEffort          = "effort-2025-11-24"
	BetaRedactThinking  = "redact-thinking-2026-02-12"
	BetaExtendedTTL     = "extended-cache-ttl-2025-04-11"
	BetaCacheEvict      = "prompt-caching-evict-2026-05-12"
	BetaAdvancedToolUse = "advanced-tool-use-2025-11-20"
	BetaFastMode        = "fast-mode-2026-02-01"
	// BetaTaskBudget is the output_config.task_budget header. The body field
	// alone is not enough; the request must opt in with this exact flag.
	BetaTaskBudget = "task-budgets-2026-03-13"
)

// StainlessRuntimeHeaders returns the SDK fingerprint for this process.
// The CLI SDK reports a Node-compatible process as "node". The Go runtime is
// not Node, so the runtime is "unknown" and OS/arch come from the real process.
// Callers overwrite forwarded client values after copying client headers.
func StainlessRuntimeHeaders() map[string]string {
	h := StainlessRuntimeHeadersFor("stream")
	return h
}

// StainlessRuntimeHeadersFor returns the process fingerprint with the helper
// method for that endpoint. A model list is not a stream.
func StainlessRuntimeHeadersFor(method string) map[string]string {
	if method == "" {
		method = "stream"
	}
	return map[string]string{
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": SDKPackageVersion,
		"X-Stainless-OS":              stainlessOS(runtime.GOOS),
		"X-Stainless-Arch":            stainlessArch(runtime.GOARCH),
		"X-Stainless-Runtime":         "unknown",
		"X-Stainless-Runtime-Version": "unknown",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         StainlessTimeoutSeconds,
		"X-Stainless-Helper-Method":   method,
	}
}

func stainlessOS(goos string) string {
	switch goos {
	case "darwin":
		return "MacOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	case "android":
		return "Android"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	default:
		return "Unknown"
	}
}

func stainlessArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		if goarch == "" {
			return "unknown"
		}
		return "other:" + goarch
	}
}

// StaticModels is the fallback catalog for dashboard autocomplete when the
// live /models probe fails. Names match the 2.1.303 first-party catalog.
var StaticModels = []string{
	"claude-fable-5",
	"claude-fable-5-1",
	"claude-sonnet-5",
	"claude-sonnet-5-5",
	"claude-opus-5",
	"claude-opus-5-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-haiku-4-5-20251001",
}
