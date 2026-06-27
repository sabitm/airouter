package web

import (
	"context"
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
