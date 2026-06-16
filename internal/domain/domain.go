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

// Combo is a custom model name (e.g. "default") bound to a provider and a real
// upstream model id. Clients call the combo name in the request `model` field
// and the router resolves it to the provider + upstream model.
type Combo struct {
	ID            int64
	Name          string
	ProviderID    int64
	UpstreamModel string
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Provider is optionally hydrated for display/resolution. Not persisted here.
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
