package domain

import "time"

// Protocol identifies the wire format a provider speaks natively.
type Protocol string

const (
	ProtocolOpenAI          Protocol = "openai"           // OpenAI Chat Completions (/chat/completions)
	ProtocolAnthropic       Protocol = "anthropic"        // Anthropic Messages (/messages)
	ProtocolOpenAIResponses Protocol = "openai-responses" // OpenAI Responses (/responses)
)

func (p Protocol) Valid() bool {
	return p == ProtocolOpenAI || p == ProtocolAnthropic || p == ProtocolOpenAIResponses
}

// AuthScheme is the header an upstream uses to carry the provider credential. It
// is independent of Protocol: an Anthropic-format provider may authenticate with
// a bearer token (ANTHROPIC_AUTH_TOKEN) rather than x-api-key.
type AuthScheme string

const (
	AuthBearer  AuthScheme = "bearer"    // Authorization: Bearer <key>
	AuthXAPIKey AuthScheme = "x-api-key" // x-api-key: <key>
)

func (a AuthScheme) Valid() bool {
	return a == AuthBearer || a == AuthXAPIKey
}

// AuthMethod selects how a provider's upstream credential is obtained. It is
// independent of AuthScheme: apikey sends a stored static key; oauth obtains a
// bearer access token (refreshed as needed) from a token endpoint.
type AuthMethod string

const (
	AuthAPIKey AuthMethod = "apikey" // static API key (provider.APIKey)
	AuthOAuth  AuthMethod = "oauth"  // OAuth access token, refreshed from a token endpoint
)

func (m AuthMethod) Valid() bool {
	return m == AuthAPIKey || m == AuthOAuth
}

// OAuthMode distinguishes a built-in preset connect from a manually configured
// connection. Both share one refresh path; the difference is whether the
// preset/config (client_id, endpoints, scopes) comes from a registry or from
// user input, and whether connect runs the PKCE authorization-code flow.
type OAuthMode string

const (
	OAuthManual OAuthMode = "manual" // tokens + refresh config supplied by the user
	OAuthAuto   OAuthMode = "auto"   // preset-driven PKCE connect (e.g. xAI)
)

// OAuthCreds holds an OAuth connection's tokens and refresh/connect
// configuration. The full configuration is stored inline so the connect and
// refresh flows are universal — a built-in preset merely prefills these fields
// at save time rather than being referenced at runtime. Stored encrypted at
// rest; the value carried on this struct is always the decrypted plaintext.
type OAuthCreds struct {
	Mode OAuthMode `json:"mode"`
	// Preset names the built-in configuration that prefilled this connection, for
	// display. Empty for manual (custom) connections.
	Preset string `json:"preset,omitempty"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is the access token's expiry, as a Unix timestamp (seconds). Zero
	// means unknown; the resolver then refreshes only reactively on a 401/403.
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Email     string `json:"email,omitempty"`
	IDToken   string `json:"id_token,omitempty"`

	// Connect/refresh configuration. Populated for both modes: auto copies it from
	// the preset, manual takes it from user input.
	AuthURL      string `json:"auth_url,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
	// PKCE marks a public client that authenticates with a code challenge instead
	// of a client_secret (e.g. xAI). Governs both the authorize request and the
	// connect flow offered by the dashboard.
	PKCE bool `json:"pkce,omitempty"`
}

// Provider is a named upstream connection: a base URL, a credential, and the
// protocol the upstream speaks. For apikey providers the credential is APIKey,
// stored encrypted at rest. For oauth providers APIKey is empty and OAuthCreds
// holds the (encrypted) tokens; the proxy resolves an effective bearer token per
// request. The values carried on this struct are always the decrypted plaintext.
type Provider struct {
	ID       int64
	Name     string
	BaseURL  string
	APIKey   string
	Protocol Protocol
	// AuthMethod may be empty on legacy rows; use Method for the effective value.
	AuthMethod AuthMethod
	OAuthCreds *OAuthCreds
	// AuthScheme may be empty on legacy rows; use Auth for the effective value.
	AuthScheme AuthScheme
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Method resolves the effective auth method, defaulting to apikey when none was
// set explicitly. This keeps pre-AuthMethod providers behaving exactly as before.
func (p *Provider) Method() AuthMethod {
	if p.AuthMethod.Valid() {
		return p.AuthMethod
	}
	return AuthAPIKey
}

// Auth resolves the effective auth scheme, defaulting by method/protocol when
// none was set explicitly: OAuth always uses bearer; apikey Anthropic uses
// x-api-key; apikey OpenAI uses bearer. This keeps pre-AuthScheme providers
// behaving exactly as before.
func (p *Provider) Auth() AuthScheme {
	if p.AuthScheme.Valid() {
		return p.AuthScheme
	}
	if p.Method() == AuthOAuth {
		return AuthBearer
	}
	if p.Protocol == ProtocolAnthropic {
		return AuthXAPIKey
	}
	return AuthBearer
}

// ComboStrategy selects which target a combo resolves to per request.
type ComboStrategy string

const (
	// StrategyFailover always tries targets in position order, advancing to the
	// next only when an upstream attempt fails before any bytes reach the client.
	StrategyFailover ComboStrategy = "failover"
	// StrategyRoundRobin rotates the starting target per request, then continues
	// through the remaining targets so it still fails over past a dead provider.
	StrategyRoundRobin ComboStrategy = "roundrobin"
)

func (s ComboStrategy) Valid() bool {
	return s == StrategyFailover || s == StrategyRoundRobin
}

// Combo is a custom model name (e.g. "default") backed by one or more targets.
// Clients call the combo name in the request `model` field and the router
// resolves it to a provider + upstream model according to the strategy.
type Combo struct {
	ID        int64
	Name      string
	Strategy  ComboStrategy
	CreatedAt time.Time
	UpdatedAt time.Time

	// Targets are ordered by Position. Hydrated for display/resolution.
	Targets []ComboTarget
}

// ComboTarget binds a combo to one provider + upstream model at a position in
// the combo's ordered target list.
type ComboTarget struct {
	ID            int64
	ProviderID    int64
	UpstreamModel string
	Position      int

	// Provider is hydrated for display/resolution. Not a stored column here.
	Provider *Provider
}

// AccessKey is a router-side bearer token clients authenticate with. The raw
// token is shown to the user exactly once at creation; only its SHA-256 hash is
// persisted, alongside a short prefix used for display.
type AccessKey struct {
	ID        int64
	Name      string
	Prefix    string
	Hash      string
	CreatedAt time.Time

	// Token is populated only on creation, never loaded from the store.
	Token string
}

// RequestLog is one proxied inference request, recorded after it completes.
// Provider, combo, and access-key names are denormalized so a log survives
// deletion of the entities it references. Token counts are 0 when the path did
// not decode usage (streaming passthrough always; unary passthrough when the
// upstream body omits a usage object).
type RequestLog struct {
	ID            int64
	CreatedAt     time.Time
	AccessKeyName string
	Combo         string
	Provider      string
	UpstreamModel string
	Format        string // ingress wire format id (oai-chat, anth-msg, oai-responses)
	Stream        bool
	Status        int
	InputTokens   int
	OutputTokens  int
	LatencyMS     int64
	ErrMsg        string
}
