// Package oauth implements OAuth connect and token refresh for providers whose
// auth_method is oauth. It is provider-agnostic: every connection carries its
// full configuration inline (OAuthCreds), so the connect and refresh flows read
// config from that struct rather than from a registry at runtime. The Presets
// here are convenience prefills applied when a provider is created from the
// dashboard — e.g. choosing "Grok" copies the xAI configuration into the
// provider's OAuthCreds rather than being referenced later.
package oauth

import "airouter/internal/domain"

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
			Mode:         domain.OAuthAuto,
			Preset:       p.Name,
			AuthURL:      p.AuthURL,
			TokenURL:     p.TokenURL,
			ClientID:     p.ClientID,
			ClientSecret: p.ClientSecret,
			Scopes:       p.Scopes,
			RedirectURI:  p.RedirectURI,
			PKCE:         p.PKCE,
		}
}
