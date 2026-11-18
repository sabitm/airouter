package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/opencode"
)

func TestOpencodeConsoleInferenceHeaders(t *testing.T) {
	creds := &domain.OAuthCreds{
		OpencodeConsoleAuth: true,
		AccessToken:         "access-secret",
		RefreshToken:        "refresh-secret",
		OpencodeOrgID:       "org-1",
		OpencodeOrgName:     "Workspace",
	}
	provider := &domain.Provider{
		Name: "console", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer, OAuthCreds: creds,
		APIKey: "stale-key",
	}
	clientHeaders := http.Header{}
	clientHeaders.Set("x-opencode-org-id", "caller-org")
	clientHeaders.Set("Authorization", "Bearer caller")
	paths := []struct {
		name string
		path string
		want string
	}{
		{name: "chat", path: opencode.ChatPath, want: opencode.GoOpenAIBaseURL + opencode.ChatPath},
		{name: "responses", path: opencode.ResponsesPath, want: opencode.GoOpenAIBaseURL + opencode.ResponsesPath},
		{name: "messages", path: opencode.MessagesPath, want: opencode.GoAnthropicBaseURL + opencode.MessagesPath},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				var gotURL, gotAuth, gotOrg, gotAPIKey string
				st := newTestStore(t)
				p := cloneConsoleProvider(provider)
				p.APIKey = ""
				if err := st.CreateProvider(t.Context(), p); err != nil {
					t.Fatal(err)
				}
				p.APIKey = "stale-key"
				px := New(st, nil)
				transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					gotURL = r.URL.String()
					gotAuth = r.Header.Get("Authorization")
					gotOrg = r.Header.Get("x-opencode-org-id")
					gotAPIKey = r.Header.Get("x-api-key")
					body := `{"id":"ok"}`
					ctype := "application/json"
					if stream {
						body = "data: [DONE]\n\n"
						ctype = "text/event-stream"
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {ctype}},
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    r,
					}, nil
				})
				px.client.Transport = transport
				px.streamClient.Transport = transport
				body := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
				if tc.path == opencode.MessagesPath {
					body = []byte(`{"model":"minimax-m3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
				}
				if stream {
					resp, err := px.forwardStream(context.Background(), p, tc.path, body, clientHeaders, "")
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
				} else {
					if _, _, err := px.forward(context.Background(), p, tc.path, body, clientHeaders); err != nil {
						t.Fatal(err)
					}
				}
				if gotURL != tc.want || gotAuth != "Bearer access-secret" || gotOrg != "org-1" || gotAPIKey != "" {
					t.Fatalf("stream=%v url=%q auth=%q org=%q api=%q", stream, gotURL, gotAuth, gotOrg, gotAPIKey)
				}
			}
		})
	}
}

func TestOpencodeConsoleSendFailsClosed(t *testing.T) {
	hits := 0
	px := New(newTestStore(t), nil)
	px.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header), Request: r}, nil
	})
	px.streamClient = px.client
	base := &domain.Provider{
		Name: "bad", Protocol: domain.ProtocolOpencode, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		BaseURL: opencode.ZenBaseURL,
		OAuthCreds: &domain.OAuthCreds{
			OpencodeConsoleAuth: true, AccessToken: "access-secret", RefreshToken: "refresh-secret", OpencodeOrgID: "org-1",
		},
	}
	if _, _, err := px.forward(context.Background(), base, opencode.ChatPath, []byte(`{"model":"m"}`), nil); err == nil || hits != 0 {
		t.Fatalf("zen send err=%v hits=%d", err, hits)
	}
	custom := cloneConsoleProvider(base)
	custom.BaseURL = "https://evil.example/inference/go/v1"
	if _, _, err := px.forward(context.Background(), custom, opencode.ChatPath, []byte(`{"model":"m"}`), nil); err == nil || hits != 0 {
		t.Fatalf("custom send err=%v hits=%d", err, hits)
	}
	scheme := cloneConsoleProvider(base)
	scheme.BaseURL = opencode.GoBaseURL
	scheme.AuthScheme = domain.AuthXAPIKey
	if _, _, err := px.forward(context.Background(), scheme, opencode.ChatPath, []byte(`{"model":"m"}`), nil); err == nil || hits != 0 {
		t.Fatalf("scheme send err=%v hits=%d", err, hits)
	}
	malformed := &domain.Provider{
		Protocol: domain.ProtocolOpencode, AuthMethod: domain.AuthOAuth, BaseURL: opencode.GoBaseURL,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "access-secret", TokenURL: "https://evil.example/token"},
	}
	if _, _, err := px.forward(context.Background(), malformed, opencode.ChatPath, []byte(`{"model":"m"}`), nil); err == nil || hits != 0 {
		t.Fatalf("malformed send err=%v hits=%d", err, hits)
	}
}

func TestOpencodeKeyAndZenHeadersUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		key     string
		model   string
		wantURL string
	}{
		{name: "zen", base: opencode.ZenBaseURL, key: opencode.PublicKey, model: "qwen3.8-max", wantURL: opencode.ZenOpenAIBaseURL + opencode.ChatPath},
		{name: "go-key", base: opencode.GoBaseURL, key: "go-secret", model: "deepseek-v4-pro", wantURL: opencode.GoOpenAIBaseURL + opencode.ChatPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotURL, gotAuth, gotOrg string
			px := New(newTestStore(t), nil)
			px.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				gotURL = r.URL.String()
				gotAuth = r.Header.Get("Authorization")
				gotOrg = r.Header.Get("x-opencode-org-id")
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`)), Request: r}, nil
			})
			p := &domain.Provider{
				Name: tc.name, BaseURL: tc.base, APIKey: tc.key, Protocol: domain.ProtocolOpencode,
				AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthBearer,
			}
			headers := http.Header{"x-opencode-org-id": {"caller-org"}}
			if _, _, err := px.forward(context.Background(), p, opencode.ChatPath, []byte(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`), headers); err != nil {
				t.Fatal(err)
			}
			if gotURL != tc.wantURL || gotAuth != "Bearer "+tc.key {
				t.Fatalf("url=%q auth=%q", gotURL, gotAuth)
			}
			if tc.name == "go-key" && gotOrg != "caller-org" {
				t.Fatalf("api-key org header = %q", gotOrg)
			}
		})
	}
}

func TestOpencodeConsoleSendAfterKeySwitch(t *testing.T) {
	st := newTestStore(t)
	p := &domain.Provider{
		Name: "console", BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{OpencodeConsoleAuth: true, AccessToken: "old-access", RefreshToken: "old-refresh", OpencodeOrgID: "org-1"},
	}
	if err := st.CreateProvider(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	switched := *p
	switched.AuthMethod, switched.APIKey, switched.OAuthCreds = domain.AuthAPIKey, "key", nil
	if err := st.UpdateProvider(t.Context(), &switched); err != nil {
		t.Fatal(err)
	}
	px := New(st, nil)
	px.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("stale connection reached the upstream")
		return nil, nil
	})
	px.streamClient = px.client
	body := []byte(`{"model":"deepseek-v4-pro"}`)
	if _, _, err := px.forward(t.Context(), cloneConsoleProvider(p), opencode.ChatPath, body, nil); !oauth.IsCredentialConflict(err) {
		t.Fatalf("unary: %v", err)
	}
	if _, err := px.forwardStream(t.Context(), cloneConsoleProvider(p), opencode.ChatPath, body, nil, ""); !oauth.IsCredentialConflict(err) {
		t.Fatalf("stream: %v", err)
	}
}

func TestOpencodeConsoleRedirectRefused(t *testing.T) {
	hits := 0
	st := newTestStore(t)
	px := New(st, nil)
	px.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"https://opencode.ai/zen/v1/chat/completions"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	p := &domain.Provider{
		BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode, AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{OpencodeConsoleAuth: true, AccessToken: "access-secret", RefreshToken: "refresh-secret", OpencodeOrgID: "org-1"},
	}
	p.Name = "console"
	if err := st.CreateProvider(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	_, _, err := px.forward(context.Background(), p, opencode.ChatPath, []byte(`{"model":"deepseek-v4-pro"}`), nil)
	if err == nil || hits != 1 || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func cloneConsoleProvider(p *domain.Provider) *domain.Provider {
	cp := *p
	if p.OAuthCreds != nil {
		creds := *p.OAuthCreds
		cp.OAuthCreds = &creds
	}
	return &cp
}
