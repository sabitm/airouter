package domain

import (
	"strings"
	"time"
)

// Protocol identifies the wire format a provider speaks natively.
type Protocol string

const (
	ProtocolOpenAI          Protocol = "openai"           // OpenAI Chat Completions (/chat/completions)
	ProtocolAnthropic       Protocol = "anthropic"        // Anthropic Messages (/messages)
	ProtocolOpenAIResponses Protocol = "openai-responses" // OpenAI Responses (/responses)
	// ProtocolOpenAICodex is the ChatGPT-backend Codex variant of the Responses
	// API: same wire shape, but the upstream enforces store=false, requires the
	// Codex-CLI identity headers, and speaks only at .../codex/responses.
	ProtocolOpenAICodex Protocol = "openai-codex"
	// ProtocolKiro is the AWS CodeWhisperer-backed Kiro provider. It is backend
	// only (never an ingress format): it speaks CodeWhisperer conversationState
	// JSON on request and a binary AWS EventStream (not SSE) on response, so every
	// request to it translates through the IR and the upstream is stream-only.
	ProtocolKiro Protocol = "kiro"
	// ProtocolQoder is the Qoder (qoder.com) backend. It is backend only: device-flow
	// OAuth, COSY-signed WAF-encoded chat against api3.qoder.sh, SSE-only responses.
	// Every request translates through the IR; device tokens do not refresh.
	ProtocolQoder Protocol = "qoder"
	// ProtocolAntigravity is Google Antigravity / Cloud Code. Backend only: Gemini
	// generateContent-like body in a Cloud Code envelope, SSE-only chat, Google OAuth
	// with loadCodeAssist/onboardUser project bootstrap. Every request translates
	// through the IR.
	ProtocolAntigravity Protocol = "antigravity"
	// ProtocolCursor is the Cursor backend. Backend only: Connect-RPC protobuf
	// chat (ChatService StreamUnifiedChatWithTools), stream-only. Auth is a Cursor
	// browser/CLI OAuth session or a pasted IDE token plus a stable machine id;
	// every request translates through the IR. Refresh uses exchange_user_api_key
	// when a refresh token is present; access-only imports stay non-refreshable.
	ProtocolCursor Protocol = "cursor"
	// ProtocolClaudeCode is the Claude Code CLI-backed Anthropic Messages backend.
	// Backend only: same Messages wire format as ProtocolAnthropic, but it
	// impersonates the Claude Code CLI (identity headers, claude.ai OAuth with a
	// JSON token exchange) and applies an OAuth-only anti-ban tool cloak/decoy
	// transform. Its codec id is distinct from anth-msg so an Anthropic ingress
	// request never passes through and always translates through the cloak
	// prepare step rather than forwarding the raw body.
	ProtocolClaudeCode Protocol = "claude-code"
)

func (p Protocol) Valid() bool {
	return p == ProtocolOpenAI || p == ProtocolAnthropic || p == ProtocolOpenAIResponses || p == ProtocolOpenAICodex || p == ProtocolKiro || p == ProtocolQoder || p == ProtocolAntigravity || p == ProtocolCursor || p == ProtocolClaudeCode
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

// ReasoningDialect selects provider-native reasoning/thinking wire semantics.
// It is independent of Protocol (transport) and AuthScheme/AuthMethod.
// Empty on a provider means "use the protocol default"; explicit none disables
// the generic reasoning writer (special protocols keep their own behavior).
type ReasoningDialect string

const (
	ReasoningNone     ReasoningDialect = "none"
	ReasoningOpenAI   ReasoningDialect = "openai"
	ReasoningClaude   ReasoningDialect = "claude"
	ReasoningCodex    ReasoningDialect = "codex"
	ReasoningKimi     ReasoningDialect = "kimi"
	ReasoningQwen     ReasoningDialect = "qwen"
	ReasoningDeepSeek ReasoningDialect = "deepseek"
	ReasoningZAI      ReasoningDialect = "zai"
	ReasoningGrok     ReasoningDialect = "grok"
)

// ParseReasoningDialect canonicalizes a user/import value. Empty input yields
// ("", true) so callers can store the protocol-default sentinel. Unknown values
// return ("", false).
func ParseReasoningDialect(s string) (ReasoningDialect, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", true
	case "none", "off":
		return ReasoningNone, true
	case "openai", "gpt":
		return ReasoningOpenAI, true
	case "claude", "anthropic":
		return ReasoningClaude, true
	case "codex":
		return ReasoningCodex, true
	case "kimi":
		return ReasoningKimi, true
	case "qwen":
		return ReasoningQwen, true
	case "deepseek":
		return ReasoningDeepSeek, true
	case "zai", "zhipu", "glm":
		return ReasoningZAI, true
	case "grok", "xai":
		return ReasoningGrok, true
	default:
		return "", false
	}
}

func (d ReasoningDialect) Valid() bool {
	switch d {
	case "", ReasoningNone, ReasoningOpenAI, ReasoningClaude, ReasoningCodex,
		ReasoningKimi, ReasoningQwen, ReasoningDeepSeek, ReasoningZAI, ReasoningGrok:
		return true
	default:
		return false
	}
}

// DefaultReasoningDialect is the legacy protocol default when the provider
// stores an empty dialect. Kiro/Qoder/Antigravity/Cursor are protocol-managed
// (no generic writer), so their default is none.
func DefaultReasoningDialect(p Protocol) ReasoningDialect {
	switch p {
	case ProtocolOpenAI, ProtocolOpenAIResponses:
		return ReasoningOpenAI
	case ProtocolOpenAICodex:
		return ReasoningCodex
	case ProtocolAnthropic, ProtocolClaudeCode:
		return ReasoningClaude
	default:
		// Kiro, Qoder, Antigravity, Cursor: protocol-managed, no generic writer.
		return ReasoningNone
	}
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
	// AccountID is a provider-specific account identifier extracted from the
	// id_token (e.g. ChatGPT's chatgpt_account_id), sent as an upstream header for
	// account binding. Empty when the token carries no such claim.
	AccountID string `json:"account_id,omitempty"`

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
	// ExtraAuthParams are additional query parameters appended to the authorize
	// URL (e.g. Codex's simplified-flow flags). Nil for providers that need none.
	ExtraAuthParams map[string]string `json:"extra_auth_params,omitempty"`
	// RefreshJSON sends the token-refresh body as application/json rather than the
	// default application/x-www-form-urlencoded (required by the ChatGPT backend).
	RefreshJSON bool `json:"refresh_json,omitempty"`
	// RefreshURL is an optional dedicated refresh endpoint. When set, refresh posts
	// here instead of TokenURL (Cline's exchange and refresh hosts differ). Empty keeps
	// the historical behavior of refreshing against TokenURL.
	RefreshURL string `json:"refresh_url,omitempty"`

	// ClineAuth marks a Cline/ClinePass OAuth connection. When true, authorize,
	// exchange, and refresh leave the generic OAuth2 path: no client_id/PKCE, a
	// base64-embedded code (with JSON token-endpoint fallback), camelCase refresh, and
	// the workos: access-token prefix plus Cline identity headers upstream.
	ClineAuth bool `json:"cline_auth,omitempty"`

	// Kiro-specific config. ProfileArn is the CodeWhisperer profile ARN injected
	// into every Kiro request body; Region selects the OIDC refresh endpoint host.
	// These live here (rather than in a dedicated column) because OAuthCreds is
	// already an encrypted JSON blob, so adding fields needs no schema migration.
	// A Kiro apikey provider carries an OAuthCreds holding only these fields (no
	// tokens), so the request encoder reads profile config from one place
	// regardless of auth method.
	ProfileArn string `json:"profile_arn,omitempty"`
	Region     string `json:"region,omitempty"`
	// KiroAuth marks a Kiro OAuth connection and names its flavor (e.g. "social",
	// "idc", "builder-id", "external_idp", "imported"). Non-empty routes token
	// refresh to Kiro's flow instead of the generic one: the social/OIDC branches
	// use JSON bodies and camelCase responses that the standard OAuth2 refresh
	// does not. "external_idp" is left to the generic form refresh (it targets a
	// standard Microsoft token endpoint).
	KiroAuth string `json:"kiro_auth,omitempty"`

	// QoderAuth marks a Qoder device-flow OAuth connection. When true, proactive
	// and reactive refresh are no-ops that surface reconnect (device tokens cannot
	// refresh against center.qoder.sh). UserID and MachineID feed COSY signing.
	QoderAuth bool `json:"qoder_auth,omitempty"`
	// UserID is the stable Qoder user id from the device token poll (COSY uid).
	UserID string `json:"user_id,omitempty"`
	// MachineID is the stable provider identity generated at connect time. Qoder
	// uses it for COSY signing; Cursor appends it to x-cursor-checksum.
	MachineID string `json:"machine_id,omitempty"`
	// OrganizationID is optional org scope from userinfo.
	OrganizationID string `json:"organization_id,omitempty"`
	// DisplayName is the profile name from userinfo, used in COSY user-info payload.
	DisplayName string `json:"display_name,omitempty"`

	// AntigravityAuth marks a Google Antigravity OAuth connection. When true,
	// post-exchange runs loadCodeAssist/onboardUser and chat requires ProjectID
	// on every request envelope.
	AntigravityAuth bool `json:"antigravity_auth,omitempty"`
	// ProjectID is the GCP / cloudaicompanion project id required on Antigravity
	// chat bodies. Empty means the connection is incomplete (fail closed).
	ProjectID string `json:"project_id,omitempty"`

	// CursorAuth marks a Cursor OAuth connection. When true, refresh uses
	// POST /auth/exchange_user_api_key only when RefreshToken is a user API key.
	// Browser poll tokens are session JWTs and cannot rotate; access-only
	// imports stay usable until they expire.
	// MachineID feeds the x-cursor-checksum header and must stay stable across
	// refresh; AccessToken (with any "::" prefix stripped at header-build time)
	// is sent as the bearer credential.
	CursorAuth bool `json:"cursor_auth,omitempty"`

	// ClaudeCodeAuth marks a Claude Code (claude.ai) OAuth connection. When true,
	// the authorization-code exchange posts a JSON body (not form-urlencoded) and
	// the proxy applies the Claude Code CLI identity headers plus the OAuth-only
	// cloak/decoy transform. The cloak gate itself is the sk-ant-oat marker on the
	// resolved access token, not this flag; refresh reuses the generic JSON path.
	ClaudeCodeAuth bool `json:"claude_code_auth,omitempty"`
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
	// ReasoningDialect may be empty on legacy rows; use Reasoning for the
	// effective dialect. Explicit none disables the generic reasoning writer.
	ReasoningDialect ReasoningDialect
	// Archived providers are soft-disabled: hidden from the combo builder and
	// skipped during resolution, but kept so they can be restored or deleted.
	Archived  bool
	CreatedAt time.Time
	UpdatedAt time.Time
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

// Reasoning resolves the effective reasoning dialect. Empty stored value falls
// back to the protocol default; an explicit none is preserved so callers can
// disable the generic writer.
func (p *Provider) Reasoning() ReasoningDialect {
	if p.ReasoningDialect == "" {
		return DefaultReasoningDialect(p.Protocol)
	}
	if d, ok := ParseReasoningDialect(string(p.ReasoningDialect)); ok {
		return d
	}
	return DefaultReasoningDialect(p.Protocol)
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
	// Enabled targets participate in resolution; disabled ones are skipped by the
	// router but preserved. Zero value is false, so all store/parse paths must set
	// this explicitly (existing rows default to enabled via the store column).
	Enabled bool

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
