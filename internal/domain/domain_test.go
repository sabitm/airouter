package domain

import "testing"

func TestProtocolValid(t *testing.T) {
	valid := []Protocol{
		ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAIResponses,
		ProtocolOpenAICodex, ProtocolKiro, ProtocolQoder,
		ProtocolAntigravity, ProtocolCursor,
	}
	for _, p := range valid {
		if !p.Valid() {
			t.Errorf("%q should be valid", p)
		}
	}
	invalid := []Protocol{"", "openai ", "OpenAI", "chat", "claude", "random"}
	for _, p := range invalid {
		if p.Valid() {
			t.Errorf("%q should be invalid", p)
		}
	}
}

func TestAuthSchemeValid(t *testing.T) {
	for _, a := range []AuthScheme{AuthBearer, AuthXAPIKey} {
		if !a.Valid() {
			t.Errorf("%q should be valid", a)
		}
	}
	for _, a := range []AuthScheme{"", "Bearer", "bearer ", "basic", "x-api"} {
		if a.Valid() {
			t.Errorf("%q should be invalid", a)
		}
	}
}

func TestAuthMethodValid(t *testing.T) {
	for _, m := range []AuthMethod{AuthAPIKey, AuthOAuth} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []AuthMethod{"", "apikey ", "token", "oauth "} {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}

func TestComboStrategyValid(t *testing.T) {
	for _, s := range []ComboStrategy{StrategyFailover, StrategyRoundRobin} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []ComboStrategy{"", "random", "round-robin", "Failover"} {
		if s.Valid() {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestProviderMethod(t *testing.T) {
	cases := []struct {
		name string
		in   AuthMethod
		want AuthMethod
	}{
		{"explicit apikey", AuthAPIKey, AuthAPIKey},
		{"explicit oauth", AuthOAuth, AuthOAuth},
		{"empty defaults to apikey", "", AuthAPIKey},
		{"invalid defaults to apikey", "garbage", AuthAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{AuthMethod: tc.in}
			if got := p.Method(); got != tc.want {
				t.Errorf("Method() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProviderAuth(t *testing.T) {
	// Explicit valid scheme always wins, regardless of method/protocol.
	for _, proto := range []Protocol{ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAICodex, ProtocolKiro} {
		p := Provider{Protocol: proto, AuthMethod: AuthAPIKey, AuthScheme: AuthBearer}
		if got := p.Auth(); got != AuthBearer {
			t.Errorf("explicit bearer with %s: Auth() = %q, want bearer", proto, got)
		}
		p = Provider{Protocol: proto, AuthMethod: AuthAPIKey, AuthScheme: AuthXAPIKey}
		if got := p.Auth(); got != AuthXAPIKey {
			t.Errorf("explicit x-api-key with %s: Auth() = %q, want x-api-key", proto, got)
		}
	}

	// Defaulting matrix when no scheme is set.
	cases := []struct {
		name     string
		method   AuthMethod
		protocol Protocol
		want     AuthScheme
	}{
		{"oauth + openai -> bearer", AuthOAuth, ProtocolOpenAI, AuthBearer},
		{"oauth + anthropic -> bearer (oauth wins)", AuthOAuth, ProtocolAnthropic, AuthBearer},
		{"oauth + empty method -> bearer", AuthOAuth, ProtocolAnthropic, AuthBearer},
		{"apikey + anthropic -> x-api-key", AuthAPIKey, ProtocolAnthropic, AuthXAPIKey},
		{"apikey + openai -> bearer", AuthAPIKey, ProtocolOpenAI, AuthBearer},
		{"apikey + openai-responses -> bearer", AuthAPIKey, ProtocolOpenAIResponses, AuthBearer},
		{"apikey + openai-codex -> bearer", AuthAPIKey, ProtocolOpenAICodex, AuthBearer},
		{"apikey + kiro -> bearer", AuthAPIKey, ProtocolKiro, AuthBearer},
		{"apikey + qoder -> bearer", AuthAPIKey, ProtocolQoder, AuthBearer},
		{"apikey + antigravity -> bearer", AuthAPIKey, ProtocolAntigravity, AuthBearer},
		{"apikey + cursor -> bearer", AuthAPIKey, ProtocolCursor, AuthBearer},
		// Legacy row: empty method defaults to apikey, then default by protocol.
		{"empty method + anthropic -> x-api-key", "", ProtocolAnthropic, AuthXAPIKey},
		{"empty method + openai -> bearer", "", ProtocolOpenAI, AuthBearer},
		// Invalid scheme falls through to defaulting.
		{"invalid scheme + oauth -> bearer", AuthOAuth, ProtocolOpenAI, AuthBearer},
		{"invalid scheme + apikey anthropic -> x-api-key", AuthAPIKey, ProtocolAnthropic, AuthXAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{Protocol: tc.protocol, AuthMethod: tc.method, AuthScheme: "invalid-scheme"}
			if got := p.Auth(); got != tc.want {
				t.Errorf("Auth() = %q, want %q", got, tc.want)
			}
		})
	}

	// No explicit scheme and no method: pure protocol defaulting.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{Protocol: tc.protocol, AuthMethod: tc.method}
			if got := p.Auth(); got != tc.want {
				t.Errorf("Auth() (no scheme set) = %q, want %q", got, tc.want)
			}
		})
	}
}
