package web

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"airouter/internal/domain"

	"github.com/a-h/templ"
)

func renderComponent(t *testing.T, comp templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := comp.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestProviderEditRowGenericAPIKeyProtocolEditable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proto   domain.Protocol
		wantSel []string
	}{
		{"openai", domain.ProtocolOpenAI, []string{"openai", "openai-responses", "anthropic"}},
		{"openai-responses", domain.ProtocolOpenAIResponses, []string{"openai", "openai-responses", "anthropic"}},
		{"anthropic", domain.ProtocolAnthropic, []string{"openai", "openai-responses", "anthropic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Provider{ID: 1, Name: "p", BaseURL: "https://x", Protocol: tc.proto}
			html := renderComponent(t, providerEditRowGenericAPIKey(p))
			if !strings.Contains(html, `<select name="protocol"`) {
				t.Fatalf("want protocol select; html=%s", html)
			}
			if strings.Contains(html, `type="hidden" name="protocol"`) {
				t.Fatalf("want no hidden protocol input for editable proto; html=%s", html)
			}
			for _, opt := range tc.wantSel {
				if !strings.Contains(html, `value="`+opt+`"`) {
					t.Fatalf("missing option %q; html=%s", opt, html)
				}
			}
			// selected attribute is rendered via templ boolean attribute as ` selected`
			if !strings.Contains(html, `value="`+tc.name+`" selected`) {
				t.Fatalf("want %q selected; html=%s", tc.name, html)
			}
		})
	}
}

func TestProviderEditRowGenericAPIKeyProtocolLockedForSpecific(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto domain.Protocol
	}{
		{"kiro", domain.ProtocolKiro},
		{"qoder", domain.ProtocolQoder},
		{"cursor", domain.ProtocolCursor},
		{"antigravity", domain.ProtocolAntigravity},
		{"codex", domain.ProtocolOpenAICodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &domain.Provider{ID: 2, Name: "p", BaseURL: "https://x", Protocol: tc.proto, AuthMethod: domain.AuthAPIKey}
			html := renderComponent(t, providerEditRowGenericAPIKey(p))
			if !strings.Contains(html, `type="hidden" name="protocol" value="`+string(tc.proto)+`"`) {
				t.Fatalf("want hidden locked protocol=%q; html=%s", tc.proto, html)
			}
			if strings.Contains(html, `<select name="protocol"`) {
				t.Fatalf("want no protocol select for %q; html=%s", tc.proto, html)
			}
		})
	}
}
