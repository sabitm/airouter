package web

import (
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/qoder"
)

type recipeKind string

const (
	kindInteractiveOAuth recipeKind = "interactive-oauth"
	kindKiro             recipeKind = "kiro"
	kindQoder            recipeKind = "qoder"
	kindCursor           recipeKind = "cursor"
	kindGenericAPIKey    recipeKind = "generic-apikey"
	kindOpencode         recipeKind = "opencode"
)

// recipe is a provider-creation template: a card in the gallery that, when
// chosen, renders a focused form exposing only the fields that provider needs.
type recipe struct {
	ID               string
	Label            string
	Sublabel         string
	Tag              string
	Kind             recipeKind
	Protocol         domain.Protocol
	Method           domain.AuthMethod
	Preset           string
	BaseURL          string
	ReasoningDialect domain.ReasoningDialect
}

var recipes = []recipe{
	{ID: "xai", Label: "Grok", Sublabel: "xAI", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAI, Method: domain.AuthOAuth, Preset: "xai", BaseURL: "https://api.x.ai/v1", ReasoningDialect: domain.ReasoningGrok},
	{ID: "codex", Label: "OpenAI Codex", Sublabel: "ChatGPT", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAICodex, Method: domain.AuthOAuth, Preset: "codex", BaseURL: "https://chatgpt.com/backend-api/codex"},
	{ID: "cline", Label: "Cline", Sublabel: "cline.bot", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAI, Method: domain.AuthOAuth, Preset: "cline", BaseURL: "https://api.cline.bot/api/v1", ReasoningDialect: domain.ReasoningCline},
	{ID: "clinepass", Label: "ClinePass", Sublabel: "cline.bot", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAI, Method: domain.AuthOAuth, Preset: "clinepass", BaseURL: "https://api.cline.bot/api/v1", ReasoningDialect: domain.ReasoningCline},
	{ID: "kiro", Label: "Kiro", Sublabel: "AWS CodeWhisperer", Tag: "API key / OAuth", Kind: kindKiro, Protocol: domain.ProtocolKiro, BaseURL: kiro.DefaultBaseURL},
	{ID: "qoder", Label: "Qoder", Sublabel: "qoder.com", Tag: "OAuth device", Kind: kindQoder, Protocol: domain.ProtocolQoder, Method: domain.AuthOAuth, Preset: "qoder", BaseURL: qoder.DefaultBaseURL},
	{ID: "antigravity", Label: "Antigravity", Sublabel: "Google Cloud Code (unofficial)", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolAntigravity, Method: domain.AuthOAuth, Preset: "antigravity", BaseURL: antigravity.DefaultBaseURL},
	{ID: "cursor", Label: "Cursor", Sublabel: "cursor.com (unofficial)", Tag: "OAuth", Kind: kindCursor, Protocol: domain.ProtocolCursor, Method: domain.AuthOAuth, Preset: "cursor", BaseURL: cursor.DefaultBaseURL},
	{ID: "claude", Label: "Claude Code", Sublabel: "claude.ai (unofficial)", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolClaudeCode, Method: domain.AuthOAuth, Preset: "claude", BaseURL: claudecode.DefaultBaseURL},
	{ID: "opencode", Label: "OpenCode", Sublabel: "opencode.ai free tier", Tag: "API key", Kind: kindOpencode, Protocol: domain.ProtocolOpencode, Method: domain.AuthAPIKey, BaseURL: opencode.ZenBaseURL},
	{ID: "openai", Label: "OpenAI-compatible", Sublabel: "OpenRouter, OpenAI, vLLM...", Tag: "API key", Kind: kindGenericAPIKey, Protocol: domain.ProtocolOpenAI, Method: domain.AuthAPIKey},
	{ID: "openai-responses", Label: "OpenAI Responses", Sublabel: "Responses API upstreams", Tag: "API key", Kind: kindGenericAPIKey, Protocol: domain.ProtocolOpenAIResponses, Method: domain.AuthAPIKey},
	{ID: "anthropic", Label: "Anthropic-compatible", Sublabel: "Claude API...", Tag: "API key", Kind: kindGenericAPIKey, Protocol: domain.ProtocolAnthropic, Method: domain.AuthAPIKey},
}

func recipeByID(id string) (recipe, bool) {
	for _, r := range recipes {
		if r.ID == id {
			return r, true
		}
	}
	return recipe{}, false
}

// genericProtocolEditable reports whether a provider's protocol can be switched
// from the generic API-key edit row. Only the three wire-format-equivalent
// protocols are interchangeable; specific providers keep a locked field.
func genericProtocolEditable(p domain.Protocol) bool {
	return p == domain.ProtocolOpenAI || p == domain.ProtocolOpenAIResponses || p == domain.ProtocolAnthropic
}

// reasoningDialectEditable reports whether the dashboard should expose a
// reasoning-dialect selector. Generic OpenAI/Anthropic-compatible providers can
// choose; fixed backends lock to their effective dialect. Cline/ClinePass
// recipes and existing Cline OAuth rows are locked separately.
func reasoningDialectEditable(p domain.Protocol) bool {
	return p == domain.ProtocolOpenAI || p == domain.ProtocolOpenAIResponses || p == domain.ProtocolAnthropic
}

// lockedReasoningDialect is the effective dialect for fixed backends (hidden input).
func lockedReasoningDialect(p domain.Protocol) domain.ReasoningDialect {
	return domain.DefaultReasoningDialect(p)
}

func recipeReasoningLocked(r recipe) bool {
	return r.ReasoningDialect == domain.ReasoningCline
}

func recipeLockedReasoningDialect(r recipe) domain.ReasoningDialect {
	if r.ReasoningDialect != "" {
		return r.ReasoningDialect
	}
	return domain.DefaultReasoningDialect(r.Protocol)
}

// providerReasoningLocked locks existing Cline/ClinePass OAuth rows to the
// effective Cline dialect. Explicit stored non-Cline values stay editable.
func providerReasoningLocked(p *domain.Provider) bool {
	if p == nil {
		return false
	}
	if !reasoningDialectEditable(p.Protocol) {
		return true
	}
	if p.ReasoningDialect != "" {
		d, ok := domain.ParseReasoningDialect(string(p.ReasoningDialect))
		if !ok || d != domain.ReasoningCline {
			return false
		}
	}
	return isClineOAuthProvider(p)
}

func isClineOAuthProvider(p *domain.Provider) bool {
	if p == nil || p.OAuthCreds == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(p.OAuthCreds.Preset)) {
	case "cline", "clinepass":
		return true
	}
	return p.OAuthCreds.ClineAuth
}

func providerLockedReasoningDialect(p *domain.Provider) domain.ReasoningDialect {
	if p == nil {
		return domain.ReasoningNone
	}
	if d := p.Reasoning(); d != "" {
		return d
	}
	return domain.DefaultReasoningDialect(p.Protocol)
}

// openaiDialectOptions are selectable dialects for OpenAI Chat Completions.
var openaiDialectOptions = []domain.ReasoningDialect{
	domain.ReasoningNone,
	domain.ReasoningOpenAI,
	domain.ReasoningKimi,
	domain.ReasoningQwen,
	domain.ReasoningDeepSeek,
	domain.ReasoningZAI,
	domain.ReasoningGrok,
	domain.ReasoningCline,
}

// responsesDialectOptions omit Cline, which is a Chat Completions gateway.
var responsesDialectOptions = []domain.ReasoningDialect{
	domain.ReasoningNone,
	domain.ReasoningOpenAI,
	domain.ReasoningKimi,
	domain.ReasoningQwen,
	domain.ReasoningDeepSeek,
	domain.ReasoningZAI,
	domain.ReasoningGrok,
}

// anthropicDialectOptions are selectable dialects for Anthropic-compatible transports.
var anthropicDialectOptions = []domain.ReasoningDialect{
	domain.ReasoningNone,
	domain.ReasoningClaude,
}

func dialectOptionsFor(proto domain.Protocol) []domain.ReasoningDialect {
	switch proto {
	case domain.ProtocolAnthropic:
		return anthropicDialectOptions
	case domain.ProtocolOpenAIResponses:
		return responsesDialectOptions
	default:
		return openaiDialectOptions
	}
}

// parseReasoningDialectForm reads reasoning_dialect from a form. Empty keeps the
// protocol default (stored as ""). Invalid values return false.
func parseReasoningDialectForm(raw string, proto domain.Protocol) (domain.ReasoningDialect, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "default" {
		return "", true
	}
	d, ok := domain.ParseReasoningDialect(raw)
	if !ok {
		return "", false
	}
	// Fixed backends always store their locked effective dialect explicitly when submitted.
	if !reasoningDialectEditable(proto) {
		return domain.DefaultReasoningDialect(proto), true
	}
	if d == domain.ReasoningCline && proto != domain.ProtocolOpenAI {
		return "", false
	}
	// For editable providers, store canonical non-default values. Empty remains
	// the protocol-default sentinel unless an update needs to override Cline compatibility.
	if d == domain.DefaultReasoningDialect(proto) && raw != "none" && d != domain.ReasoningNone {
		return "", true
	}
	return d, true
}

// parseProviderReasoningDialectForm preserves an explicit protocol default when
// it replaces Cline semantics. Empty must not reactivate the legacy Cline fallback.
func parseProviderReasoningDialectForm(raw string, proto domain.Protocol, current *domain.Provider) (domain.ReasoningDialect, bool) {
	d, ok := parseReasoningDialectForm(raw, proto)
	if !ok || d != "" || current == nil || !isClineOAuthProvider(current) {
		return d, ok
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "default" {
		return domain.DefaultReasoningDialect(proto), true
	}
	explicit, parsed := domain.ParseReasoningDialect(raw)
	if parsed && explicit == domain.DefaultReasoningDialect(proto) {
		return explicit, true
	}
	return d, true
}
