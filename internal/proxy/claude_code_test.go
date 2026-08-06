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

	"airouter/internal/crypto"
	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
	"airouter/internal/store"
)

// TestClaudeCodeCloakAndHeaders verifies the full OAuth path: an OpenAI ingress
// request to a claude-code OAuth backend is translated, cloaked (billing header,
// fake user_id, suffixed tools + decoys), sent with the CLI identity headers and
// a per-request session id that matches metadata.user_id.session_id, and the
// upstream's cloaked tool_use response is decloaked back to the client.
func TestClaudeCodeCloakAndHeaders(t *testing.T) {
	var cap struct {
		path string
		auth string
		ua   string
		beta string
		sess string
		body []byte
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.path = r.URL.Path
		cap.auth = r.Header.Get("Authorization")
		cap.ua = r.Header.Get("User-Agent")
		cap.beta = r.Header.Get("anthropic-beta")
		cap.sess = r.Header.Get(claudecode.SessionIDHeader)
		cap.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_x","type":"message","role":"assistant","model":"up","content":[{"type":"tool_use","id":"t1","name":"search_ide","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	t.Cleanup(upstream.Close)

	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	prov := &domain.Provider{
		Name: "cc", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthAuto, Preset: "claude", ClaudeCodeAuth: true,
			AccessToken: "sk-ant-oat-test", RefreshToken: "rt-stable", ExpiresAt: 0,
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	// The server middleware attaches a TraceInfo so the prepare and header seams
	// share the per-request session id; replicate that one line here (the proxy
	// package's tests cannot import the server package without an import cycle).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithTraceInfo(r.Context(), &TraceInfo{}))
		mux.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	body := `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"search","description":"d","parameters":{"type":"object"}}}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}

	// Upstream path carries the beta suffix.
	if cap.path != "/messages" {
		t.Errorf("upstream path = %q, want /messages", cap.path)
	}
	// Resolved OAuth token sent as bearer.
	if cap.auth != "Bearer sk-ant-oat-test" {
		t.Errorf("auth = %q, want bearer oat token", cap.auth)
	}
	// CLI identity fingerprint overwrote forwarded client values.
	if !strings.HasPrefix(cap.ua, "claude-cli/2.1.92") {
		t.Errorf("User-Agent = %q, want claude-cli fingerprint", cap.ua)
	}
	if !strings.Contains(cap.beta, "claude-code-20250219") {
		t.Errorf("anthropic-beta = %q, missing claude-code flag", cap.beta)
	}
	if cap.sess == "" {
		t.Error("X-Claude-Code-Session-Id not set")
	}

	// The upstream body was cloaked.
	var m map[string]any
	if err := json.Unmarshal(cap.body, &m); err != nil {
		t.Fatalf("upstream body invalid: %v", err)
	}
	sys, _ := m["system"].([]any)
	if len(sys) == 0 {
		t.Fatal("system blocks missing")
	}
	first, _ := sys[0].(map[string]any)
	if !strings.HasPrefix(first["text"].(string), "x-anthropic-billing-header:") {
		t.Errorf("billing header not system[0]: %v", sys[0])
	}
	meta, _ := m["metadata"].(map[string]any)
	uid, _ := meta["user_id"].(string)
	if !strings.Contains(uid, cap.sess) {
		t.Errorf("metadata.user_id (%s) must contain the session id header (%s)", uid, cap.sess)
	}
	tools, _ := m["tools"].([]any)
	var toolNames []string
	for _, tt := range tools {
		tm, _ := tt.(map[string]any)
		toolNames = append(toolNames, tm["name"].(string))
	}
	if !contains(toolNames, "search_ide") {
		t.Errorf("client tool not suffixed in upstream tools: %v", toolNames)
	}
	if !contains(toolNames, "Bash") {
		t.Errorf("decoy tools not appended: %v", toolNames)
	}

	// The response was decloaked: the client sees the original tool name "search".
	var oai struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &oai); err != nil {
		t.Fatalf("openai response invalid: %v (%s)", err, out)
	}
	if len(oai.Choices) == 0 || len(oai.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("no tool call in response: %s", out)
	}
	if name := oai.Choices[0].Message.ToolCalls[0].Function.Name; name != "search" {
		t.Errorf("response tool name = %q, want search (decloaked)", name)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
