package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"airouter/internal/crypto"
	"airouter/internal/domain"
	"airouter/internal/store"
)

const (
	openaiUpstreamBody    = `{"id":"chatcmpl-x","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hello from openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`
	anthropicUpstreamBody = `{"id":"msg_x","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"hello from anthropic"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`
)

type capturedUpstream struct {
	path      string
	auth      string
	xkey      string
	model     string
	userAgent string
	beta      string
}

// newUpstream returns a mock provider that answers in the format matching the
// requested path and records what it received.
func newUpstream(t *testing.T, cap *capturedUpstream) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &m)
		cap.path = r.URL.Path
		cap.auth = r.Header.Get("Authorization")
		cap.xkey = r.Header.Get("x-api-key")
		cap.model = m.Model
		cap.userAgent = r.Header.Get("User-Agent")
		cap.beta = r.Header.Get("anthropic-beta")

		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			_, _ = io.WriteString(w, anthropicUpstreamBody)
		} else {
			_, _ = io.WriteString(w, openaiUpstreamBody)
		}
	}))
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// setup wires a store with one provider + combo + access key, and a proxy
// server in front of the mock upstream. Returns the proxy base URL and token.
func setup(t *testing.T, backend domain.Protocol, cap *capturedUpstream) (string, string) {
	t.Helper()
	base, token, _ := setupWithStore(t, backend, cap)
	return base, token
}

func setupWithStore(t *testing.T, backend domain.Protocol, cap *capturedUpstream) (string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	upstream := newUpstream(t, cap)
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: backend}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", ProviderID: prov.ID, UpstreamModel: "real-model"}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	New(st, false).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, st
}

func post(t *testing.T, url, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

func TestMatrix(t *testing.T) {
	cases := []struct {
		name     string
		backend  domain.Protocol
		ingress  string // path
		body     string
		wantText string
		wantPath string
	}{
		{"openai->openai passthrough", domain.ProtocolOpenAI, "/v1/chat/completions",
			`{"model":"default","messages":[{"role":"user","content":"hi"}]}`, "hello from openai", "/chat/completions"},
		{"openai->anthropic translate", domain.ProtocolAnthropic, "/v1/chat/completions",
			`{"model":"default","messages":[{"role":"user","content":"hi"}]}`, "hello from anthropic", "/messages"},
		{"anthropic->anthropic passthrough", domain.ProtocolAnthropic, "/v1/messages",
			`{"model":"default","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, "hello from anthropic", "/messages"},
		{"anthropic->openai translate", domain.ProtocolOpenAI, "/v1/messages",
			`{"model":"default","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, "hello from openai", "/chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap capturedUpstream
			base, token := setup(t, tc.backend, &cap)
			resp, out := post(t, base+tc.ingress, token, tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
			}
			if cap.path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", cap.path, tc.wantPath)
			}
			if cap.model != "real-model" {
				t.Errorf("upstream model = %q, want real-model", cap.model)
			}
			// Auth header must match the backend protocol.
			if tc.backend == domain.ProtocolAnthropic && cap.xkey != "up-key" {
				t.Errorf("expected x-api-key, got auth=%q xkey=%q", cap.auth, cap.xkey)
			}
			if tc.backend == domain.ProtocolOpenAI && cap.auth != "Bearer up-key" {
				t.Errorf("expected bearer auth, got %q", cap.auth)
			}
			// Response must be in the ingress format and carry the upstream text.
			text := extractText(t, tc.ingress, out)
			if text != tc.wantText {
				t.Errorf("response text = %q, want %q", text, tc.wantText)
			}
		})
	}
}

func extractText(t *testing.T, ingress string, body []byte) string {
	t.Helper()
	if strings.HasSuffix(ingress, "/messages") {
		var r struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &r); err != nil || len(r.Content) == 0 {
			t.Fatalf("bad anthropic response: %s", body)
		}
		return r.Content[0].Text
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil || len(r.Choices) == 0 {
		t.Fatalf("bad openai response: %s", body)
	}
	return r.Choices[0].Message.Content
}

func TestAuthRequired(t *testing.T) {
	var cap capturedUpstream
	base, _ := setup(t, domain.ProtocolOpenAI, &cap)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"default","messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestUnknownCombo(t *testing.T) {
	var cap capturedUpstream
	base, token := setup(t, domain.ProtocolOpenAI, &cap)
	resp, _ := post(t, base+"/v1/chat/completions", token, `{"model":"nope","messages":[]}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestPassthroughForwardsClientHeaders verifies passthrough relays caller
// identity headers (User-Agent, anthropic-beta) to the upstream while still
// substituting the provider's credential for the client's.
func TestPassthroughForwardsClientHeaders(t *testing.T) {
	var cap capturedUpstream
	base, token := setup(t, domain.ProtocolAnthropic, &cap)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages",
		strings.NewReader(`{"model":"default","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", token)
	req.Header.Set("User-Agent", "claude-cli/1.2.3")
	req.Header.Set("anthropic-beta", "some-beta")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cap.userAgent != "claude-cli/1.2.3" {
		t.Errorf("upstream User-Agent = %q, want forwarded client value", cap.userAgent)
	}
	if cap.beta != "some-beta" {
		t.Errorf("upstream anthropic-beta = %q, want forwarded", cap.beta)
	}
	// Client credential must be replaced by the provider's, not forwarded.
	if cap.xkey != "up-key" {
		t.Errorf("upstream x-api-key = %q, want provider key up-key", cap.xkey)
	}
}

// TestOpenModeNoKeys verifies the proxy accepts unauthenticated requests when
// no access keys exist, and locks down once one is created.
func TestOpenModeNoKeys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	var cap capturedUpstream
	upstream := newUpstream(t, &cap)
	t.Cleanup(upstream.Close)
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", ProviderID: prov.ID, UpstreamModel: "real-model"}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, false).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body := `{"model":"default","messages":[{"role":"user","content":"hi"}]}`

	// No keys: request with no Authorization header succeeds.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open mode: status = %d, want 200", resp.StatusCode)
	}

	// Create a key: now an unauthenticated request is rejected.
	if _, err := st.NewAccessKey(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("locked mode: status = %d, want 401", resp2.StatusCode)
	}
}

// waitForLogs polls the store for the expected number of request logs, since
// recording is fire-and-forget in a background goroutine.
func waitForLogs(t *testing.T, st *store.Store, want int) []*domain.RequestLog {
	t.Helper()
	for i := 0; i < 100; i++ {
		logs, err := st.ListRequestLogs(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) >= want {
			return logs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d request logs", want)
	return nil
}

func TestRequestLogTranslated(t *testing.T) {
	var cap capturedUpstream
	base, token, st := setupWithStore(t, domain.ProtocolAnthropic, &cap)
	resp, out := post(t, base+"/v1/chat/completions", token,
		`{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	logs := waitForLogs(t, st, 1)
	l := logs[0]
	if l.Format != "oai-chat" || l.Combo != "default" || l.Provider != "p" || l.UpstreamModel != "real-model" {
		t.Errorf("log metadata mismatch: %+v", l)
	}
	if l.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", l.Status)
	}
	// Translated path decodes usage from the upstream body.
	if l.InputTokens != 3 || l.OutputTokens != 4 {
		t.Errorf("tokens = %d/%d, want 3/4", l.InputTokens, l.OutputTokens)
	}
	if l.AccessKeyName != "test" {
		t.Errorf("access key name = %q, want test", l.AccessKeyName)
	}
}

func TestRequestLogPassthroughUsage(t *testing.T) {
	var cap capturedUpstream
	base, token, st := setupWithStore(t, domain.ProtocolOpenAI, &cap)
	resp, _ := post(t, base+"/v1/chat/completions", token,
		`{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	l := waitForLogs(t, st, 1)[0]
	// Passthrough recovers usage from the raw OpenAI body.
	if l.InputTokens != 3 || l.OutputTokens != 4 {
		t.Errorf("tokens = %d/%d, want 3/4", l.InputTokens, l.OutputTokens)
	}
}

func TestRequestLogUnknownCombo(t *testing.T) {
	var cap capturedUpstream
	base, token, st := setupWithStore(t, domain.ProtocolOpenAI, &cap)
	post(t, base+"/v1/chat/completions", token, `{"model":"nope","messages":[]}`)
	l := waitForLogs(t, st, 1)[0]
	if l.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", l.Status)
	}
	if l.Combo != "nope" || l.ErrMsg == "" {
		t.Errorf("expected failed log for unknown combo, got %+v", l)
	}
}

