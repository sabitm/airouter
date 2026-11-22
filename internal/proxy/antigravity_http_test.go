package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/antigravity"
)

func TestAntigravityChatWireCompatibility(t *testing.T) {
	const model = "gemini-3.8-flash-high"
	const piOpener = "You are a coding assistant running inside pi, a coding agent harness. Help users inspect files, run commands, and modify code."
	const docsBullet = "- When asked about: extensions (docs/extensions.md, examples/extensions/), themes (docs/themes.md), skills (docs/skills.md), prompt templates (docs/prompt-templates.md), TUI components (docs/tui.md), keybindings (docs/keybindings.md), SDK integrations (docs/sdk.md), custom providers (docs/custom-provider.md), adding models (docs/models.md), packages (docs/packages.md), environment variables (docs/environment-variables.md), MCP servers (docs/mcp.md), codemode scripts and non-LLM models such as classifiers and image models (docs/codemode.md)"
	const system = piOpener + "\n<docs>\nConsult Pi documentation.\n" + docsBullet + "\n</docs>"
	const wantSystem = "You are a coding assistant. Help users inspect files, run commands, and modify code.\n<docs>\nConsult Pi documentation.\n</docs>"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RequestURI() != antigravity.UpstreamPathStream || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("unexpected upstream request: %s %s, Accept=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Accept"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer up-key" {
			t.Errorf("upstream Authorization = %q", got)
		}
		var body struct {
			Project     string          `json:"project"`
			Model       string          `json:"model"`
			RequestID   string          `json:"requestId"`
			RequestType json.RawMessage `json:"requestType"`
			Request     struct {
				SystemInstruction struct {
					Parts []struct{ Text string } `json:"parts"`
				} `json:"systemInstruction"`
			} `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Project != "proj-1" || body.Model != model {
			t.Errorf("project = %q, model = %q", body.Project, body.Model)
		}
		if !strings.HasPrefix(body.RequestID, "agent/") {
			t.Errorf("upstream requestId = %q, want agent/ prefix", body.RequestID)
		}
		parts := body.Request.SystemInstruction.Parts
		systemOK := len(parts) == 1 && parts[0].Text == wantSystem
		if !systemOK {
			t.Errorf("upstream system parts = %+v, want %q", parts, wantSystem)
		}
		if body.RequestType != nil {
			t.Errorf("upstream JSON contains requestType: %s", body.RequestType)
		}
		if body.RequestType != nil || !systemOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"antigravity-ok"}]},"finishReason":"STOP"}]}}`+"\n\n")
	}))
	t.Cleanup(up.Close)

	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "ag", BaseURL: up.URL, APIKey: "up-key",
		Protocol:   domain.ProtocolAntigravity,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{AntigravityAuth: true, ProjectID: "proj-1"},
	}
	if err := st.CreateProvider(t.Context(), prov); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "ag-chat", Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: model, Enabled: true}}}
	if err := st.CreateCombo(t.Context(), combo); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)

	systemJSON, err := json.Marshal(system)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, stream, contentType string
	}{
		{"unary", "false", "application/json"},
		{"streaming", "true", "text/event-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"ag-chat","messages":[{"role":"system","content":` + string(systemJSON) + `},{"role":"user","content":"hi"}],"stream":` + tc.stream + `}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			waitForLogs(t, st, i+1)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "antigravity-ok") {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
				t.Errorf("Content-Type = %q, want %q", got, tc.contentType)
			}
		})
	}
}
