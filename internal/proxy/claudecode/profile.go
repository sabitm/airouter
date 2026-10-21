package claudecode

import (
	"strings"

	"airouter/internal/proxy/thinking"
)

// Profile is the pinned Claude Code 2.1.303 request profile.
// Runtime fingerprints are not invented. The process reports its own OS and
// architecture, and the runtime name stays unknown because this process is not Node.
type Profile struct {
	Version    string
	Entrypoint string
	// SDK is true only when the caller is a validated Agent SDK entrypoint
	// (sdk-cli, sdk-ts, sdk-py) and an Agent SDK version was supplied.
	SDK bool
	// NonInteractive is the CKn isNonInteractive flag. Without an appended
	// system prompt it selects the agent identity line, not the SDK line.
	NonInteractive bool
	// HasAppendSystemPrompt is CKn's second flag. With NonInteractive it
	// selects the SDK identity line.
	HasAppendSystemPrompt bool
	// AgentSDKVersion is the validated CLAUDE_AGENT_SDK_VERSION. It is not the
	// CLI version and not the Anthropic SDK package version.
	AgentSDKVersion string
	// Workload, when set and validated, is appended inside the User-Agent
	// parentheses and may be copied to the billing header.
	Workload string
	// ClientApp, when set and validated, is appended inside the parentheses.
	ClientApp string
}

// DefaultProfile is the interactive CLI profile used when the caller does not
// supply a validated entrypoint.
func DefaultProfile() Profile {
	return Profile{Version: CLIVersion, Entrypoint: DefaultEntrypoint}
}

// ProfileUserAgent is the short profile User-Agent used by the profile fetch.
// BXe uses hs(), which is claude-code/<version>, not the inference User-Agent.
func ProfileUserAgent() string {
	return "claude-code/" + CLIVersion
}

// UserAgent matches V0: claude-cli/<version> (external, <entrypoint>[, agent-sdk/<ver>][, client-app/<app>][, workload/<w>]).
// Optional suffixes stay inside the parentheses. An SDK flag without a validated
// Agent SDK version does not invent a suffix.
func (p Profile) UserAgent() string {
	entry := p.Entrypoint
	if entry == "" {
		entry = DefaultEntrypoint
	}
	var suffixes []string
	suffixes = append(suffixes, entry)
	if p.SDK && validSDKVersion(p.AgentSDKVersion) {
		suffixes = append(suffixes, "agent-sdk/"+p.AgentSDKVersion)
	}
	if validSuffix(p.ClientApp) {
		suffixes = append(suffixes, "client-app/"+p.ClientApp)
	}
	if validSuffix(p.Workload) {
		suffixes = append(suffixes, "workload/"+p.Workload)
	}
	return UserAgentPrefix + " (external, " + strings.Join(suffixes, ", ") + ")"
}

// IdentityLine is CKn for a non-vertex request. A recorded override is not
// reconstructed. Non-interactive without an append uses the agent line.
// Non-interactive with an append uses the SDK line. Otherwise the CLI line.
func (p Profile) IdentityLine() string {
	if p.NonInteractive {
		if p.HasAppendSystemPrompt {
			return IdentitySDK
		}
		return IdentityAgent
	}
	return IdentityCLI
}

func validSDKVersion(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

func validSuffix(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// BetaRequest is the parsed feature set used to select known betas.
// Flags are taken from structured fields, never from a substring of the body.
type BetaRequest struct {
	Model              string
	OAuth              bool
	ContextManagement  bool
	StructuredOutput   bool
	Effort             bool
	CacheScope         bool
	ExtendedTTL        bool
	CacheEvict         bool
	AdvancedTools      bool
	TaskBudget         bool
	ThinkingActive     bool
	ThinkingSummarized bool
	FastMode           bool
	OneMillion         bool
	// ContextOneMillion carries the request-local target [1m] selector. The
	// prepared wire model no longer contains that selector.
	ContextOneMillion bool
	// CallerBetas are validated anthropic-beta flags captured from the client
	// header. Unknown names are already dropped. Model support is checked here.
	CallerBetas []string
}

// BetaHeaders returns the feature- and model-gated flags for one request.
// The 2.1.303 registry is not sent wholesale. token-efficient-tools and
// between_tools are absent from that CLI and are never added.
// A summarized thinking display does not receive the redact-thinking beta.
func BetaHeaders(req BetaRequest) string {
	var out []string
	add := func(flag string) {
		for _, existing := range out {
			if existing == flag {
				return
			}
		}
		out = append(out, flag)
	}
	model := canonicalModel(req.Model)
	if model != "" && !strings.Contains(model, "haiku") {
		add(BetaClaudeCode)
	}
	if req.OAuth {
		add(BetaOAuth)
	}
	if req.OneMillion && (req.ContextOneMillion || thinking.HasOneMillionContextSelector(req.Model) || modelSupportsOneMillion(model)) {
		add(BetaLongContext)
	}
	if req.ThinkingActive && modelSupportsInterleavedThinking(model) {
		add(BetaInterleaved)
		if !req.ThinkingSummarized {
			add(BetaRedactThinking)
		}
	}
	if req.ContextManagement && modelSupportsContextManagement(model) {
		add(BetaContextMgmt)
	}
	if req.StructuredOutput && modelSupportsStructuredOutput(model) {
		add(BetaStructured)
	}
	if req.Effort && modelSupportsEffort(model) {
		add(BetaEffort)
	}
	if req.CacheScope {
		add(BetaPromptCache)
	}
	if req.ExtendedTTL {
		add(BetaExtendedTTL)
	}
	if req.CacheEvict {
		add(BetaCacheEvict)
	}
	if req.AdvancedTools {
		add(BetaAdvancedToolUse)
	}
	// The pinned first-party gate is the backend plus a validated task_budget
	// field, not a per-model capability table.
	if req.TaskBudget {
		add(BetaTaskBudget)
	}
	if req.FastMode && modelSupportsFastMode(model) {
		add(BetaFastMode)
	}
	for _, flag := range req.CallerBetas {
		if callerBetaSupported(flag, model, req) {
			add(flag)
		}
	}
	return strings.Join(out, ",")
}

func callerBeta(req BetaRequest, flag string) bool {
	for _, got := range req.CallerBetas {
		if got == flag {
			return true
		}
	}
	return false
}

// callerBetaSupported admits a caller flag only when this model can use it.
// The check reuses the same feature gates as a body-derived flag. It does not
// accept an unknown name or a flag the catalog does not support.
func callerBetaSupported(flag, model string, req BetaRequest) bool {
	switch flag {
	case BetaClaudeCode:
		return model != "" && !strings.Contains(model, "haiku")
	case BetaOAuth:
		return req.OAuth
	case BetaLongContext:
		// A caller flag is a request for the beta. Model support is the gate.
		// Do not also require the body to repeat the same flag.
		return modelSupportsOneMillion(model)
	case BetaInterleaved:
		return req.ThinkingActive && modelSupportsInterleavedThinking(model)
	case BetaRedactThinking:
		return req.ThinkingActive && !req.ThinkingSummarized && modelSupportsInterleavedThinking(model)
	case BetaContextMgmt:
		return modelSupportsContextManagement(model)
	case BetaStructured:
		return modelSupportsStructuredOutput(model)
	case BetaEffort:
		return modelSupportsEffort(model)
	case BetaPromptCache, BetaExtendedTTL, BetaCacheEvict, BetaAdvancedToolUse:
		return true
	case BetaTaskBudget:
		return true
	case BetaFastMode:
		return modelSupportsFastMode(model)
	default:
		return false
	}
}

// ParseCallerBetas splits an anthropic-beta header into known registry flags.
// Unknown names, empty items, and duplicates are dropped. The parse does not
// read arbitrary body strings or schema keys.
func ParseCallerBetas(header string) []string {
	if header == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(header, ",") {
		flag := strings.TrimSpace(part)
		if flag == "" || !knownBeta(flag) {
			continue
		}
		dup := false
		for _, got := range out {
			if got == flag {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, flag)
		}
	}
	return out
}

func knownBeta(flag string) bool {
	switch flag {
	case BetaClaudeCode, BetaOAuth, BetaInterleaved, BetaLongContext, BetaContextMgmt,
		BetaPromptCache, BetaStructured, BetaEffort, BetaRedactThinking, BetaExtendedTTL,
		BetaCacheEvict, BetaAdvancedToolUse, BetaFastMode, BetaTaskBudget:
		return true
	default:
		return false
	}
}

// modelSupportsInterleavedThinking matches DYn for a first-party model:
// claude-3 and claude-haiku-4-5 do not receive the interleaved beta. Other
// known and unknown non-haiku-4.5 names do. This is the beta gate, not the
// thinking-capability table.
func modelSupportsInterleavedThinking(model string) bool {
	m := canonicalModel(model)
	if m == "" || m == "claude-haiku-4-5" || strings.Contains(m, "claude-3-") || strings.HasPrefix(m, "claude-3") {
		return false
	}
	return true
}

func modelSupportsContextManagement(model string) bool {
	switch canonicalModel(model) {
	case "claude-haiku-4-5", "claude-haiku-5-5",
		"claude-sonnet-4-0", "claude-sonnet-4-5", "claude-sonnet-4-6", "claude-sonnet-5", "claude-sonnet-5-5",
		"claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-5", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
		"claude-opus-5", "claude-opus-5-5",
		"claude-fable-5", "claude-fable-5-1",
		"claude-mythos-5-1":
		return true
	default:
		return false
	}
}

func modelSupportsStructuredOutput(model string) bool {
	m := canonicalModel(model)
	if m == "" || strings.Contains(m, "claude-3") || m == "claude-opus-4-1" {
		return false
	}
	return strings.HasPrefix(m, "claude-")
}

func modelSupportsEffort(model string) bool {
	switch canonicalModel(model) {
	case "claude-haiku-5-5",
		"claude-sonnet-4-6", "claude-sonnet-5", "claude-sonnet-5-5",
		"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-opus-5", "claude-opus-5-5",
		"claude-fable-5", "claude-fable-5-1",
		"claude-mythos-5-1":
		return true
	default:
		return false
	}
}

func modelSupportsFastMode(model string) bool {
	switch canonicalModel(model) {
	case "claude-opus-4-8", "claude-opus-5", "claude-opus-5-5":
		return true
	default:
		return false
	}
}

func modelSupportsOneMillion(model string) bool {
	row, ok := LookupModel(model)
	return ok && row.OneMillion
}

func canonicalModel(model string) string {
	m := thinking.StripContextSelectors(strings.ToLower(strings.TrimSpace(model)))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.ReplaceAll(m, ".", "-")
	if i := strings.Index(m, "@"); i >= 0 {
		m = m[:i]
	}
	return m
}
