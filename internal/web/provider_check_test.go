package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

func TestCheckClaudeCodeProfileAndModelLimit(t *testing.T) {
	calls := 0
	withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.String() != "https://claude-check.invalid/v1/models?limit=1000" {
			t.Fatalf("check request = %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer sk-ant-oat-synthetic" || req.Header.Get("User-Agent") != "claude-cli/2.1.318 (external, cli)" {
			t.Fatal("check auth/profile changed")
		}
		if req.Header.Get("anthropic-beta") != claudecode.BetaOAuth {
			t.Fatalf("check betas = %q", req.Header.Get("anthropic-beta"))
		}
		if _, ok := req.Header["X-Stainless-Helper-Method"]; ok {
			t.Fatal("check claimed an SDK helper")
		}
		for key, value := range claudecode.StainlessRuntimeHeaders() {
			if req.Header.Get(key) != value {
				t.Errorf("check header %s = %q, want %q", key, req.Header.Get(key), value)
			}
		}
		return opencodeModelsResponse(), nil
	})
	p := &domain.Provider{Protocol: domain.ProtocolClaudeCode, BaseURL: "https://claude-check.invalid/v1/", APIKey: "sk-ant-oat-synthetic"}
	ok, result := checkClaudeCodeUpstream(t.Context(), slog.New(slog.NewTextHandler(io.Discard, nil)), p)
	if !ok || !strings.Contains(result, "does not prove Messages") || calls != 1 {
		t.Fatalf("check = %v / %q / calls %d", ok, result, calls)
	}
}

func TestClaudeModelDiscoveryFallbackIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"live", 200, `{"data":[{"id":"account-model"}]}`, []string{"account-model"}},
		{"empty", 200, `{"data":[]}`, claudecode.StaticModels},
		{"rejected", 403, `{"error":"denied"}`, claudecode.StaticModels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/models?limit=1000" {
					t.Errorf("autocomplete request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("X-Stainless-Helper-Method") != "" {
					t.Error("autocomplete claimed an SDK helper")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			p := &domain.Provider{Protocol: domain.ProtocolClaudeCode, BaseURL: srv.URL, APIKey: "synthetic-key"}
			got, err := fetchUpstreamModels(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), p)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("models = %v / %v, want %v", got, err, tc.want)
			}
			if tc.name != "live" && !slices.Contains(got, "claude-haiku-5-5") {
				t.Fatal("fallback omits Haiku 5.5")
			}
			if tc.name != "live" {
				got[0] = "mutated-test-result"
				if claudecode.StaticModels[0] == "mutated-test-result" {
					t.Fatal("fallback returned shared catalog storage")
				}
			}
		})
	}
}

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
