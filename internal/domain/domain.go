package domain

import "time"

// Protocol identifies the wire format a provider speaks natively.
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
)

func (p Protocol) Valid() bool {
	return p == ProtocolOpenAI || p == ProtocolAnthropic
}

// Provider is a named upstream connection: a base URL, an API key, and the
// protocol the upstream speaks. The API key is stored encrypted at rest; the
// value carried on this struct is always the decrypted plaintext.
type Provider struct {
	ID        int64
	Name      string
	BaseURL   string
	APIKey    string
	Protocol  Protocol
	CreatedAt time.Time
	UpdatedAt time.Time
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
