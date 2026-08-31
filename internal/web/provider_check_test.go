package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"airouter/internal/domain"
)

func TestCheckProviderRejectsBlankBaseURLBeforeCredentialsOrProbe(t *testing.T) {
	protocols := []domain.Protocol{
		domain.ProtocolOpenAI,
		domain.ProtocolAnthropic,
		domain.ProtocolOpenAIResponses,
		domain.ProtocolOpenAICodex,
		domain.ProtocolKiro,
		domain.ProtocolQoder,
		domain.ProtocolAntigravity,
		domain.ProtocolCursor,
		domain.ProtocolClaudeCode,
	}
	for _, proto := range protocols {
		t.Run(string(proto), func(t *testing.T) {
			h := testHandler(t)
			hits := 0
			withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
				hits++
				return opencodeModelsResponse(), nil
			})
			form := url.Values{
				"protocol": {string(proto)},
				"base_url": {" \t\n "},
				"api_key":  {"secret"},
			}
			rec := httptest.NewRecorder()
			h.checkProvider(rec, reqWithForm(form))
			if !strings.Contains(rec.Body.String(), "enter a base URL") {
				t.Fatalf("check result = %s", rec.Body.String())
			}
			if hits != 0 {
				t.Fatalf("blank URL made %d upstream request(s)", hits)
			}
		})
	}
}

func TestCheckOAuthProviderRejectsBlankBaseURLBeforeConnectionLookup(t *testing.T) {
	h := testHandler(t)
	hits := 0
	withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
		hits++
		return opencodeModelsResponse(), nil
	})
	form := url.Values{
		"protocol":    {string(domain.ProtocolOpenAI)},
		"auth_method": {string(domain.AuthOAuth)},
		"base_url":    {""},
	}
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "enter a base URL") {
		t.Fatalf("check result = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "not connected") {
		t.Fatalf("credential lookup ran before URL validation: %s", rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("blank URL made %d upstream request(s)", hits)
	}
}
