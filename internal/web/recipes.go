package web

import (
	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/qoder"
)

type recipeKind string

const (
	kindInteractiveOAuth recipeKind = "interactive-oauth"
	kindKiro             recipeKind = "kiro"
	kindQoder            recipeKind = "qoder"
	kindCursor           recipeKind = "cursor"
	kindGenericAPIKey    recipeKind = "generic-apikey"
)

// recipe is a provider-creation template: a card in the gallery that, when
// chosen, renders a focused form exposing only the fields that provider needs.
type recipe struct {
	ID       string
	Label    string
	Sublabel string
	Tag      string
	Kind     recipeKind
	Protocol domain.Protocol
	Method   domain.AuthMethod
	Preset   string
	BaseURL  string
}

var recipes = []recipe{
	{ID: "xai", Label: "Grok", Sublabel: "xAI", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAI, Method: domain.AuthOAuth, Preset: "xai", BaseURL: "https://api.x.ai/v1"},
	{ID: "codex", Label: "OpenAI Codex", Sublabel: "ChatGPT", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAICodex, Method: domain.AuthOAuth, Preset: "codex", BaseURL: "https://chatgpt.com/backend-api/codex"},
	{ID: "cline", Label: "Cline", Sublabel: "cline.bot", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAI, Method: domain.AuthOAuth, Preset: "cline", BaseURL: "https://api.cline.bot/api/v1"},
	{ID: "clinepass", Label: "ClinePass", Sublabel: "cline.bot", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolOpenAI, Method: domain.AuthOAuth, Preset: "clinepass", BaseURL: "https://api.cline.bot/api/v1"},
	{ID: "kiro", Label: "Kiro", Sublabel: "AWS CodeWhisperer", Tag: "API key / OAuth", Kind: kindKiro, Protocol: domain.ProtocolKiro, BaseURL: kiro.DefaultBaseURL},
	{ID: "qoder", Label: "Qoder", Sublabel: "qoder.com", Tag: "OAuth device", Kind: kindQoder, Protocol: domain.ProtocolQoder, Method: domain.AuthOAuth, Preset: "qoder", BaseURL: qoder.DefaultBaseURL},
	{ID: "antigravity", Label: "Antigravity", Sublabel: "Google Cloud Code (unofficial)", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolAntigravity, Method: domain.AuthOAuth, Preset: "antigravity", BaseURL: antigravity.DefaultBaseURL},
	{ID: "cursor", Label: "Cursor IDE", Sublabel: "cursor.com (unofficial)", Tag: "Import token", Kind: kindCursor, Protocol: domain.ProtocolCursor, Method: domain.AuthOAuth, Preset: "cursor", BaseURL: cursor.DefaultBaseURL},
	{ID: "claude", Label: "Claude Code", Sublabel: "claude.ai (unofficial)", Tag: "OAuth", Kind: kindInteractiveOAuth, Protocol: domain.ProtocolClaudeCode, Method: domain.AuthOAuth, Preset: "claude", BaseURL: claudecode.DefaultBaseURL},
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
