package web

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/domain"
)

// TestProviderModelsOAuth: the combo form's model-list fetch resolves a saved
// oauth provider's access token onto the upstream request and renders the
// returned model ids into the datalist. Guards the carry-through of the resolved
// token, which a discarded Resolve return value once silently dropped.
func TestProviderModelsOAuth(t *testing.T) {
	h := testHandler(t)

	var sawAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if sawAuth != "Bearer stored-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"grok-4"},{"id":"grok-3"}]}`))
	}))
	t.Cleanup(up.Close)

	p := &domain.Provider{
		Name: "grok", BaseURL: up.URL, Protocol: domain.ProtocolOpenAI,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{Mode: domain.OAuthAuto, AccessToken: "stored-tok"},
	}
	if err := h.store.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet,
		"/dashboard/providers/models?provider_id="+strconv.FormatInt(p.ID, 10), nil)
	rec := httptest.NewRecorder()
	h.providerModels(rec, req)

	if sawAuth != "Bearer stored-tok" {
		t.Errorf("upstream saw auth = %q, want Bearer stored-tok", sawAuth)
	}
	body := rec.Body.String()
	for _, id := range []string{"grok-4", "grok-3"} {
		if !strings.Contains(body, `value="`+id+`"`) {
			t.Errorf("datalist missing option %q: %s", id, body)
		}
	}
}

func TestFetchCodexModelsFallsBackToStatic(t *testing.T) {
	models, err := fetchUpstreamModels(context.Background(), &domain.Provider{
		Protocol: domain.ProtocolOpenAICodex,
		BaseURL:  "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "gpt-5.3-codex-high"
	for _, m := range models {
		if m == want {
			return
		}
	}
	t.Fatalf("codex fallback models missing %q: %v", want, models)
}

func TestCheckCodexUpstreamUsesModelsEndpoint(t *testing.T) {
	var method, path, clientVersion, auth, ua, originator, sessionID, accountID string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		clientVersion = r.URL.Query().Get("client_version")
		auth = r.Header.Get("Authorization")
		ua = r.Header.Get("User-Agent")
		originator = r.Header.Get("originator")
		sessionID = r.Header.Get("session_id")
		accountID = r.Header.Get("chatgpt-account-id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[{"id":"acct-model-1"},{"slug":"acct-model-2"}]}`))
	}))
	t.Cleanup(up.Close)

	ok, msg := checkUpstream(context.Background(), &domain.Provider{
		BaseURL:  up.URL,
		APIKey:   "tok",
		Protocol: domain.ProtocolOpenAICodex,
		OAuthCreds: &domain.OAuthCreds{
			AccountID: "acct-1",
		},
	}, false, nil, nil)
	if !ok {
		t.Fatalf("check failed: %s", msg)
	}
	if method != http.MethodGet || path != "/models" || clientVersion == "" {
		t.Fatalf("request = %s %s?client_version=%s, want GET /models with client_version", method, path, clientVersion)
	}
	if auth != "Bearer tok" || originator != "codex_cli_rs" || sessionID == "" || accountID != "acct-1" {
		t.Errorf("headers auth=%q originator=%q session=%q account=%q", auth, originator, sessionID, accountID)
	}
	if !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Errorf("user-agent = %q", ua)
	}
}

func TestCheckCodexUpstreamTraceSplitsFileAndStderr(t *testing.T) {
	longID := strings.Repeat("x", traceMaxBody+64)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[{"id":"` + longID + `"}]}`))
	}))
	t.Cleanup(up.Close)

	var fileBuf, stderrBuf bytes.Buffer
	fileTrace := log.New(&fileBuf, "", 0)
	stderrTrace := log.New(&stderrBuf, "", 0)
	ok, msg := checkUpstream(context.Background(), &domain.Provider{
		BaseURL:  up.URL,
		APIKey:   "tok",
		Protocol: domain.ProtocolOpenAICodex,
	}, true, fileTrace, stderrTrace)
	if !ok {
		t.Fatalf("check failed: %s", msg)
	}

	fileLog := fileBuf.String()
	stderrLog := stderrBuf.String()
	if !strings.Contains(fileLog, longID) || strings.Contains(fileLog, "truncated") {
		t.Fatalf("file trace was not full: %s", fileLog)
	}
	if strings.Contains(stderrLog, longID) || !strings.Contains(stderrLog, "truncated") {
		t.Fatalf("stderr trace was not truncated: %s", stderrLog)
	}
}
