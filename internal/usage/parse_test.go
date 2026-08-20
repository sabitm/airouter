package usage

import (
	"testing"
	"time"

	"airouter/internal/domain"
)

func TestParseResetTimeVariants(t *testing.T) {
	sec := int64(1_700_000_000)
	want := time.Unix(sec, 0).UTC()

	cases := []struct {
		name string
		in   any
		want *time.Time
	}{
		{"nil", nil, nil},
		{"empty string", "", nil},
		{"unix seconds int", int(sec), &want},
		{"unix seconds float", float64(sec), &want},
		{"unix seconds string", "1700000000", &want},
		{"unix ms float", float64(sec * 1000), &want},
		{"unix ms string", "1700000000000", &want},
		{"rfc3339", "2023-11-14T22:13:20Z", ptrTime(time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC))},
		{"rfc3339 nano", "2023-11-14T22:13:20.5Z", ptrTime(time.Date(2023, 11, 14, 22, 13, 20, 500000000, time.UTC))},
		{"garbage", "not-a-date", nil},
		{"zero", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResetTime(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil")
			}
			if !got.Equal(*tc.want) {
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestToFinite(t *testing.T) {
	if got := toFinite("12.5", 0); got != 12.5 {
		t.Fatalf("string = %v", got)
	}
	if got := toFinite("x", 3); got != 3 {
		t.Fatalf("fallback = %v", got)
	}
	if got := toFinite(nil, 9); got != 9 {
		t.Fatalf("nil = %v", got)
	}
}

func TestSupported(t *testing.T) {
	if Supported(nil) {
		t.Fatal("nil")
	}
	want := map[domain.Protocol]bool{
		domain.ProtocolOpenAICodex: true,
		domain.ProtocolClaudeCode:  true,
		domain.ProtocolKiro:        true,
		domain.ProtocolQoder:       true,
		domain.ProtocolAntigravity: true,
		domain.ProtocolOpenAI:      false,
		domain.ProtocolAnthropic:   false,
		domain.ProtocolCursor:      false,
	}
	for proto, ok := range want {
		p := &domain.Provider{Protocol: proto}
		if got := Supported(p); got != ok {
			t.Errorf("%s: Supported=%v want %v", proto, got, ok)
		}
	}
}

func TestSupportedGrokXaiOAuthOnly(t *testing.T) {
	// xAI OAuth preset exposes Grok usage through ProtocolOpenAI.
	grok := &domain.Provider{
		Protocol:   domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{Preset: "xai"},
	}
	if !Supported(grok) {
		t.Fatal("xAI OAuth Grok must be supported")
	}

	// xAI over API key, unrelated OAuth OpenAI, and Cursor are all excluded.
	for name, p := range map[string]*domain.Provider{
		"xai apikey": {
			Protocol:   domain.ProtocolOpenAI,
			AuthMethod: domain.AuthAPIKey,
			APIKey:     "k",
			OAuthCreds: &domain.OAuthCreds{Preset: "xai"},
		},
		"unrelated oauth": {
			Protocol:   domain.ProtocolOpenAI,
			AuthMethod: domain.AuthOAuth,
			OAuthCreds: &domain.OAuthCreds{Preset: "cline"},
		},
		"cursor": {
			Protocol:   domain.ProtocolCursor,
			AuthMethod: domain.AuthOAuth,
			OAuthCreds: &domain.OAuthCreds{Preset: "xai", CursorAuth: true},
		},
	} {
		if Supported(p) {
			t.Fatalf("%s must not be supported", name)
		}
	}
}
