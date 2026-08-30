package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/opencode"
)

func opencodeProviderForm(tier, key string) url.Values {
	form := url.Values{}
	form.Set("auth_method", "apikey")
	form.Set("auth_scheme", "bearer")
	form.Set("name", "opencode")
	form.Set("protocol", string(domain.ProtocolOpencode))
	form.Set("reasoning_dialect", string(domain.ReasoningOpencode))
	form.Set("opencode_tier", tier)
	form.Set("api_key", key)
	return form
}

func createOpencodeProvider(t *testing.T, h *Handler, tier, key string) (*domain.Provider, *httptest.ResponseRecorder) {
	t.Helper()
	form := opencodeProviderForm(tier, key)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/providers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.createProvider(rec, req)
	providers, err := h.store.ListProviders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) == 0 {
		return nil, rec
	}
	return providers[len(providers)-1], rec
}

func updateOpencodeProvider(t *testing.T, h *Handler, id int64, tier, key string) *httptest.ResponseRecorder {
	t.Helper()
	form := opencodeProviderForm(tier, key)
	path := "/dashboard/providers/" + strconv.FormatInt(id, 10)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.updateProvider(rec, req)
	return rec
}

func TestCreateOpencodeCredentialPolicy(t *testing.T) {
	t.Run("zen always stores public", func(t *testing.T) {
		for _, submitted := range []string{"", "paid-secret"} {
			h := testHandler(t)
			p, rec := createOpencodeProvider(t, h, "zen", submitted)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if p == nil || p.BaseURL != opencode.ZenBaseURL || p.APIKey != opencode.PublicKey {
				t.Fatalf("provider = %+v", p)
			}
		}
	})

	t.Run("go requires a real key", func(t *testing.T) {
		for _, submitted := range []string{"", opencode.PublicKey} {
			h := testHandler(t)
			p, rec := createOpencodeProvider(t, h, "go", submitted)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "OpenCode Go API key is required") {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if p != nil {
				t.Fatalf("unexpected provider = %+v", p)
			}
		}
	})

	t.Run("go stores submitted key", func(t *testing.T) {
		h := testHandler(t)
		p, rec := createOpencodeProvider(t, h, "go", "go-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if p == nil || p.BaseURL != opencode.GoBaseURL || p.APIKey != "go-secret" {
			t.Fatalf("provider = %+v", p)
		}
	})
}

func TestUpdateOpencodeCredentialTransitions(t *testing.T) {
	t.Run("zen to go blank is rejected without mutation", func(t *testing.T) {
		h := testHandler(t)
		p, rec := createOpencodeProvider(t, h, "zen", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("create: %s", rec.Body.String())
		}
		rec = updateOpencodeProvider(t, h, p.ID, "go", "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "OpenCode Go API key is required") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		got, err := h.store.GetProvider(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseURL != opencode.ZenBaseURL || got.APIKey != opencode.PublicKey {
			t.Fatalf("provider mutated = %+v", got)
		}
	})

	t.Run("zen to go accepts new key", func(t *testing.T) {
		h := testHandler(t)
		p, _ := createOpencodeProvider(t, h, "zen", "")
		rec := updateOpencodeProvider(t, h, p.ID, "go", "go-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		got, err := h.store.GetProvider(context.Background(), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.BaseURL != opencode.GoBaseURL || got.APIKey != "go-secret" {
			t.Fatalf("provider = %+v", got)
		}
	})

	t.Run("go to go blank preserves stored key", func(t *testing.T) {
		h := testHandler(t)
		p, _ := createOpencodeProvider(t, h, "go", "old-secret")
		rec := updateOpencodeProvider(t, h, p.ID, "go", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		got, _ := h.store.GetProvider(context.Background(), p.ID)
		if got.BaseURL != opencode.GoBaseURL || got.APIKey != "old-secret" {
			t.Fatalf("provider = %+v", got)
		}
	})

	t.Run("go to go replaces key", func(t *testing.T) {
		h := testHandler(t)
		p, _ := createOpencodeProvider(t, h, "go", "old-secret")
		rec := updateOpencodeProvider(t, h, p.ID, "go", "new-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		got, _ := h.store.GetProvider(context.Background(), p.ID)
		if got.APIKey != "new-secret" {
			t.Fatalf("provider = %+v", got)
		}
	})

	t.Run("go to zen discards paid key", func(t *testing.T) {
		for _, submitted := range []string{"", "replacement-secret"} {
			h := testHandler(t)
			p, _ := createOpencodeProvider(t, h, "go", "old-secret")
			rec := updateOpencodeProvider(t, h, p.ID, "zen", submitted)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			got, _ := h.store.GetProvider(context.Background(), p.ID)
			if got.BaseURL != opencode.ZenBaseURL || got.APIKey != opencode.PublicKey {
				t.Fatalf("provider = %+v", got)
			}
		}
	})
}

func TestCheckOpencodeZenToGoBlankRejectsBeforeProbe(t *testing.T) {
	h := testHandler(t)
	p, _ := createOpencodeProvider(t, h, "zen", "")
	hits := 0
	withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
		hits++
		return opencodeModelsResponse(), nil
	})
	form := opencodeProviderForm("go", "")
	form.Set("id", strconv.FormatInt(p.ID, 10))
	rec := httptest.NewRecorder()
	h.checkProvider(rec, reqWithForm(form))
	if !strings.Contains(rec.Body.String(), "OpenCode Go API key is required") {
		t.Fatalf("check result = %s", rec.Body.String())
	}
	if hits != 0 {
		t.Fatalf("invalid transition made %d upstream request(s)", hits)
	}
}

func TestCheckOpencodeCredentialTransitions(t *testing.T) {
	t.Run("go to go blank uses stored key", func(t *testing.T) {
		h := testHandler(t)
		p, _ := createOpencodeProvider(t, h, "go", "go-secret")
		var gotURL, gotAuth string
		withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
			gotURL = req.URL.String()
			gotAuth = req.Header.Get("Authorization")
			return opencodeModelsResponse(), nil
		})
		form := opencodeProviderForm("go", "")
		form.Set("id", strconv.FormatInt(p.ID, 10))
		rec := httptest.NewRecorder()
		h.checkProvider(rec, reqWithForm(form))
		if !strings.Contains(rec.Body.String(), "OK") {
			t.Fatalf("check result = %s", rec.Body.String())
		}
		if gotURL != opencode.GoBaseURL+"/models" || gotAuth != "Bearer go-secret" {
			t.Fatalf("request URL/auth = %q / %q", gotURL, gotAuth)
		}
	})

	t.Run("go to zen uses public", func(t *testing.T) {
		h := testHandler(t)
		p, _ := createOpencodeProvider(t, h, "go", "go-secret")
		var gotURL, gotAuth string
		withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
			gotURL = req.URL.String()
			gotAuth = req.Header.Get("Authorization")
			return opencodeModelsResponse(), nil
		})
		form := opencodeProviderForm("zen", "replacement-secret")
		form.Set("id", strconv.FormatInt(p.ID, 10))
		rec := httptest.NewRecorder()
		h.checkProvider(rec, reqWithForm(form))
		if !strings.Contains(rec.Body.String(), "OK") {
			t.Fatalf("check result = %s", rec.Body.String())
		}
		if gotURL != opencode.ZenBaseURL+"/models" || gotAuth != "Bearer "+opencode.PublicKey {
			t.Fatalf("request URL/auth = %q / %q", gotURL, gotAuth)
		}
	})
}

type opencodeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f opencodeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withOpencodeProbeTransport(t *testing.T, fn opencodeRoundTripFunc) {
	t.Helper()
	previous := upstreamClient
	upstreamClient = &http.Client{Transport: fn}
	t.Cleanup(func() { upstreamClient = previous })
}

func opencodeModelsResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"model"}]}`)),
	}
}
