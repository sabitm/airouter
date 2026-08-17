// Package oauth implements OAuth connect and token refresh for providers whose
// auth_method is oauth. It is provider-agnostic: every connection carries its
// full configuration inline (OAuthCreds), so the connect and refresh flows read
// config from that struct rather than from a registry at runtime. The Presets
// here are convenience prefills applied when a provider is created from the
// dashboard — e.g. choosing "Grok" copies the xAI configuration into the
// provider's OAuthCreds rather than being referenced later.
package oauth

import (
	"strings"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/proxy/cursor"
	"airouter/internal/proxy/kiro"
	"airouter/internal/proxy/qoder"
)

// Preset is a built-in OAuth configuration used to prefill a provider's
// OAuthCreds (and the provider's base URL/protocol) at creation. It is the only
// place provider-specific constants live; once applied, the connection is
// self-contained and the preset is not consulted again.
type Preset struct {
	Name  string // stable id, referenced by OAuthCreds.Preset for display
	Label string // human label shown in the dashboard dropdown

	AuthURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string // empty for public (PKCE) clients
	Scopes       string
	RedirectURI  string // loopback URL the connect flow binds for the callback
	PKCE         bool   // public client: code_challenge instead of client_secret

	// APIBase and Protocol prefill the provider row when created from this
	// preset (e.g. xAI speaks OpenAI Chat Completions at https://api.x.ai/v1).
	APIBase  string
	Protocol domain.Protocol
	// ExtraAuthParams are appended to the authorize URL (e.g. Codex's
	// simplified-flow flags). Reserved standard params are filtered out.
	ExtraAuthParams map[string]string
	// RefreshJSON sends the refresh body as application/json rather than form
	// (the ChatGPT backend requires JSON).
	RefreshJSON bool
	// RefreshURL is an optional dedicated refresh endpoint copied into OAuthCreds
	// (Cline's exchange and refresh hosts differ). Empty means refresh uses TokenURL.
	RefreshURL string
	// ClineAuth marks a Cline/ClinePass connection (non-standard OAuth + headers).
	ClineAuth bool
	// QoderAuth marks a Qoder device-flow connection (no refresh; COSY identity).
	QoderAuth bool
	// AntigravityAuth marks a Google Antigravity connection (project bootstrap).
	AntigravityAuth bool
	// CursorAuth marks a Cursor IDE connection (browser poll or imported tokens).
	CursorAuth bool
	// ClaudeCodeAuth marks a Claude Code (claude.ai) connection: JSON token
	// exchange and the CLI identity/cloak profile. Refresh reuses the generic
	// JSON path (RefreshJSON).
	ClaudeCodeAuth bool
}

// Presets is the set of built-in OAuth configurations. Add an entry here to
// support a new provider-specific connect; its data is copied into each
// provider created from it, so editing an entry does not retroactively change
// existing connections.
var Presets = []Preset{
	{
		Name:        "xai",
		Label:       "Grok (xAI)",
		AuthURL:     "https://auth.x.ai/oauth2/authorize",
		TokenURL:    "https://auth.x.ai/oauth2/token",
		ClientID:    "b1a00492-073a-47ea-816f-4c329264a828",
		Scopes:      "openid profile email offline_access grok-cli:access api:access",
		RedirectURI: "http://127.0.0.1:56121/callback",
		PKCE:        true, // xAI is a public client; no client_secret
		APIBase:     "https://api.x.ai/v1",
		Protocol:    domain.ProtocolOpenAI,
	},
	// Codex is the ChatGPT-subscription-backed coding agent API. The client id is
	// public (embedded in the official Codex CLI); the ChatGPT token endpoint
	// requires a JSON refresh body, and the authorize URL carries the Codex-CLI
	// simplified-flow flags. Upstream is the Responses API under chatgpt.com.
	{
		Name:     "codex",
		Label:    "OpenAI Codex (ChatGPT)",
		AuthURL:  "https://auth.openai.com/oauth/authorize",
		TokenURL: "https://auth.openai.com/oauth/token",
		ClientID: "app_EMoamEEZ73f0CkXaXp7hrann",
		Scopes:   "openid profile email offline_access",
		// Fixed port 1455 and /auth/callback path, matching the official CLI so the
		// authorize redirect targets the loopback server this flow binds there.
		RedirectURI: "http://localhost:1455/auth/callback",
		PKCE:        true,
		ExtraAuthParams: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "codex_cli_rs",
		},
		RefreshJSON: true,
		APIBase:     "https://chatgpt.com/backend-api/codex",
		Protocol:    domain.ProtocolOpenAICodex,
	},
	{
		Name:     "kiro",
		Label:    "Kiro (import tokens)",
		APIBase:  kiro.DefaultBaseURL,
		Protocol: domain.ProtocolKiro,
	},
	// Qoder uses a custom device flow (not auth-code). Tokens and COSY identity
	// fields are filled by QoderDeviceConnect; this preset only prefills base URL.
	{
		Name:      "qoder",
		Label:     "Qoder",
		APIBase:   qoder.DefaultBaseURL,
		Protocol:  domain.ProtocolQoder,
		QoderAuth: true,
	},
	// Cline is the cline.bot OAuth provider. It speaks OpenAI Chat Completions but
	// uses a non-standard authorization-code flow (no client_id/PKCE; tokens often
	// embedded as base64 in the redirect code) and requires a workos:-prefixed bearer
	// plus Cline identity headers upstream. Exchange and refresh hit different URLs.
	{
		Name:        "cline",
		Label:       "Cline",
		AuthURL:     "https://api.cline.bot/api/v1/auth/authorize",
		TokenURL:    "https://api.cline.bot/api/v1/auth/token",
		RefreshURL:  "https://api.cline.bot/api/v1/auth/refresh",
		RedirectURI: "http://127.0.0.1:56122/callback",
		ClineAuth:   true,
		APIBase:     "https://api.cline.bot/api/v1",
		Protocol:    domain.ProtocolOpenAI,
	},
	// ClinePass shares Cline's OAuth endpoints and header rules; only the product
	// label differs. API-key ClinePass is the generic OpenAI-compatible recipe.
	{
		Name:        "clinepass",
		Label:       "ClinePass",
		AuthURL:     "https://api.cline.bot/api/v1/auth/authorize",
		TokenURL:    "https://api.cline.bot/api/v1/auth/token",
		RefreshURL:  "https://api.cline.bot/api/v1/auth/refresh",
		RedirectURI: "http://127.0.0.1:56122/callback",
		ClineAuth:   true,
		APIBase:     "https://api.cline.bot/api/v1",
		Protocol:    domain.ProtocolOpenAI,
	},
	// Antigravity is Google Cloud Code via the public IDE OAuth client. Chat is
	// SSE-only at daily-cloudcode-pa; discovery stays on prod cloudcode-pa.
	// Connect finalizes a ProjectID via loadCodeAssist.
	{
		Name:         "antigravity",
		Label:        "Antigravity (Google)",
		AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:     "https://oauth2.googleapis.com/token",
		ClientID:     "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com",
		ClientSecret: "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",
		Scopes: strings.Join([]string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
			"https://www.googleapis.com/auth/cclog",
			"https://www.googleapis.com/auth/experimentsandconfigs",
		}, " "),
		RedirectURI: "http://127.0.0.1:56123/callback",
		PKCE:        false,
		ExtraAuthParams: map[string]string{
			"access_type": "offline",
			"prompt":      "consent",
		},
		APIBase:         antigravity.DefaultBaseURL,
		Protocol:        domain.ProtocolAntigravity,
		AntigravityAuth: true,
	},
	// Cursor IDE: browser-and-poll login (loginDeepControl + auth/poll) or a
	// pasted CLI/IDE token + machine id. Refresh uses exchange_user_api_key when
	// a refresh token is present. This preset only prefills the base URL and marker.
	{
		Name:       "cursor",
		Label:      "Cursor IDE",
		APIBase:    cursor.DefaultBaseURL,
		Protocol:   domain.ProtocolCursor,
		CursorAuth: true,
	},
	// Claude Code is the Anthropic Messages API spoken with the Claude Code CLI
	// identity. The client id is public (embedded in the CLI); authorize carries
	// code=true, the token exchange posts JSON, and refresh reuses the generic
	// JSON path. We mirror 9router's proven config: the claude.ai/oauth/authorize
	// consent endpoint with the 3-scope inference grant (org:create_api_key
	// user:profile user:inference), which issues a valid sk-ant-oat token the
	// cloak gates on and the Messages API accepts. The real CLI's newer
	// claude.com/cai gateway + 6-scope flow needs first-party gateway session
	// context (anti-bot/attestation state) that a direct browser hit from the
	// connect page cannot establish, so it rejects third-party authorize hits; we
	// do not use it. The proxy applies the CLI fingerprint and the OAuth-only tool
	// cloak/decoy transform on upstream requests.
	{
		Name:            "claude",
		Label:           "Claude Code",
		AuthURL:         "https://claude.ai/oauth/authorize",
		TokenURL:        "https://api.anthropic.com/v1/oauth/token",
		ClientID:        "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		Scopes:          "org:create_api_key user:profile user:inference",
		RedirectURI:     "http://localhost:56124/callback",
		PKCE:            true,
		ExtraAuthParams: map[string]string{"code": "true"},
		RefreshJSON:     true,
		APIBase:         claudecode.DefaultBaseURL,
		Protocol:        domain.ProtocolClaudeCode,
		ClaudeCodeAuth:  true,
	},
}

// PresetByName returns the preset with the given name, or false.
func PresetByName(name string) (Preset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// Apply fills a provider and its OAuthCreds from a preset, auto mode. The tokens
// remain empty until the connect flow completes.
func Apply(p Preset) (provider *domain.Provider, creds *domain.OAuthCreds) {
	return &domain.Provider{
			BaseURL:    p.APIBase,
			Protocol:   p.Protocol,
			AuthMethod: domain.AuthOAuth,
		},
		&domain.OAuthCreds{
			Mode:            domain.OAuthAuto,
			Preset:          p.Name,
			AuthURL:         p.AuthURL,
			TokenURL:        p.TokenURL,
			ClientID:        p.ClientID,
			ClientSecret:    p.ClientSecret,
			Scopes:          p.Scopes,
			RedirectURI:     p.RedirectURI,
			PKCE:            p.PKCE,
			ExtraAuthParams: p.ExtraAuthParams,
			RefreshJSON:     p.RefreshJSON,
			RefreshURL:      p.RefreshURL,
			ClineAuth:       p.ClineAuth,
			QoderAuth:       p.QoderAuth,
			AntigravityAuth: p.AntigravityAuth,
			CursorAuth:      p.CursorAuth,
			ClaudeCodeAuth:  p.ClaudeCodeAuth,
		}
}
