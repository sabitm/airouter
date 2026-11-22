package claudecode

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"airouter/internal/domain"
)

func TestListModelIDsUsesRawSDKProfile(t *testing.T) {
	for _, tc := range []struct {
		name, token, body string
		status            int
		want              []string
		wantErr           bool
	}{
		{"oauth", "sk-ant-oat-synthetic", `{"data":[{"id":"claude-haiku-5-5"},{"id":""},{"id":"claude-sonnet-5"}]}`, 200, []string{"claude-haiku-5-5", "claude-sonnet-5"}, false},
		{"static", "synthetic-key", `{"data":[{"id":"m"}]}`, 200, []string{"m"}, false},
		{"empty", "synthetic-key", `{"data":[]}`, 200, []string{}, false},
		{"rejected", "synthetic-key", `{"error":"denied"}`, 403, nil, true},
		{"malformed", "synthetic-key", `not-json`, 200, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/models?limit=1000" {
					t.Errorf("discovery request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer "+tc.token {
					t.Error("discovery auth changed")
				}
				if _, ok := r.Header["X-Stainless-Helper-Method"]; ok {
					t.Error("discovery claimed an SDK helper")
				}
				for key, value := range StainlessRuntimeHeaders() {
					if r.Header.Get(key) != value {
						t.Errorf("header %s = %q, want %q", key, r.Header.Get(key), value)
					}
				}
				if r.Header.Get("User-Agent") != "claude-cli/2.1.318 (external, cli)" || r.Header.Get("X-App") != "cli" || r.Header.Get("anthropic-version") != AnthropicVersion {
					t.Error("discovery profile changed")
				}
				wantBeta := ""
				if tc.name == "oauth" {
					wantBeta = BetaOAuth
				}
				if r.Header.Get("anthropic-beta") != wantBeta {
					t.Errorf("discovery betas = %q, want %q", r.Header.Get("anthropic-beta"), wantBeta)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			got, err := ListModelIDs(t.Context(), &domain.Provider{BaseURL: srv.URL + "/v1/", APIKey: tc.token, Protocol: domain.ProtocolClaudeCode})
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("models = %v, err = %v, want %v / error=%v", got, err, tc.want, tc.wantErr)
			}
			if calls != 1 {
				t.Fatalf("discovery sent %d requests", calls)
			}
		})
	}
}
