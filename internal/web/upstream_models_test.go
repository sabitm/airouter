package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/observability"
	"airouter/internal/proxy/opencode"
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
	models, err := fetchUpstreamModels(context.Background(), nil, &domain.Provider{
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

	ok, msg := checkUpstream(context.Background(), nil, &domain.Provider{
		BaseURL:  up.URL,
		APIKey:   "tok",
		Protocol: domain.ProtocolOpenAICodex,
		OAuthCreds: &domain.OAuthCreds{
			AccountID: "acct-1",
		},
	})
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

func TestCheckCodexUpstreamTraceMetadataOnly(t *testing.T) {
	const sentinel = "codex-model-id-sentinel-should-not-log"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[{"id":"` + sentinel + `"}]}`))
	}))
	t.Cleanup(up.Close)

	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)

	ok, msg := checkUpstream(context.Background(), logger, &domain.Provider{
		BaseURL:  up.URL,
		APIKey:   "tok",
		Protocol: domain.ProtocolOpenAICodex,
	})
	if !ok {
		t.Fatalf("check failed: %s", msg)
	}

	out := buf.String()
	if !strings.Contains(out, "event=probe_request") || !strings.Contains(out, "event=probe_response") {
		t.Fatalf("missing probe TRACE events: %s", out)
	}
	if strings.Contains(out, sentinel) || strings.Contains(out, " body=") {
		t.Fatalf("probe body leaked into TRACE: %s", out)
	}
	if strings.Contains(out, "tok") && strings.Contains(out, "Authorization") {
		t.Fatalf("auth leaked: %s", out)
	}
}

func TestOpencodeModelQueriesShareRequestAndParsing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		discovery bool
	}{
		{name: "check", operation: "check_opencode"},
		{name: "discovery", operation: "fetch_opencode_models", discovery: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *http.Request
			withOpencodeProbeTransport(t, func(req *http.Request) (*http.Response, error) {
				got = req.Clone(req.Context())
				return opencodeModelsHTTPResponse(http.StatusOK, `{"data":[{"id":"model-a"},{"id":""},{"id":"model-b"}]}`), nil
			})
			var logs bytes.Buffer
			logger := observability.NewLogger(2, &logs)
			p := &domain.Provider{BaseURL: "https://models.example/v1/", APIKey: "secret", Protocol: domain.ProtocolOpencode}
			var models []string
			var err error
			if tc.discovery {
				models, err = fetchOpencodeModels(context.Background(), logger, p)
				if strings.Join(models, ",") != "model-a,model-b" {
					t.Fatalf("models = %v", models)
				}
			} else {
				ok, msg := checkOpencodeUpstream(context.Background(), logger, p)
				if !ok || !strings.Contains(msg, "(2 models)") {
					t.Fatalf("check ok=%v msg=%q", ok, msg)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.URL.String() != "https://models.example/v1/models" {
				t.Fatalf("request URL = %v", got)
			}
			if got.Header.Get("Authorization") != "Bearer secret" || got.Header.Get("Accept") != "application/json" {
				t.Fatalf("auth/accept headers = %v", got.Header)
			}
			if got.Header.Get("User-Agent") != opencode.UserAgent || got.Header.Get("x-opencode-client") != "desktop" ||
				got.Header.Get("x-opencode-project") != "global" || !strings.HasPrefix(got.Header.Get("x-opencode-request"), "msg_") {
				t.Fatalf("fingerprint headers = %v", got.Header)
			}
			if !strings.Contains(logs.String(), "operation="+tc.operation) {
				t.Fatalf("probe operation missing from logs: %s", logs.String())
			}
		})
	}
}

func TestOpencodeModelQueriesResponseSemantics(t *testing.T) {
	p := &domain.Provider{BaseURL: opencode.ZenBaseURL, APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}

	t.Run("empty data is valid", func(t *testing.T) {
		withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
			return opencodeModelsHTTPResponse(http.StatusOK, `{"data":[]}`), nil
		})
		models, err := fetchOpencodeModels(context.Background(), nil, p)
		if err != nil || len(models) != 0 {
			t.Fatalf("models=%v err=%v", models, err)
		}
		ok, msg := checkOpencodeUpstream(context.Background(), nil, p)
		if !ok || !strings.Contains(msg, "(0 models)") {
			t.Fatalf("check ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("malformed success is unexpected shape", func(t *testing.T) {
		withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
			return opencodeModelsHTTPResponse(http.StatusOK, `{"models":[]}`), nil
		})
		if _, err := fetchOpencodeModels(context.Background(), nil, p); !errors.Is(err, errOpencodeResponseShape) {
			t.Fatalf("discovery err=%v", err)
		}
		ok, msg := checkOpencodeUpstream(context.Background(), nil, p)
		if ok || msg != "reachable, but response shape unexpected" {
			t.Fatalf("check ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("invalid base URL", func(t *testing.T) {
		invalid := &domain.Provider{BaseURL: "https://bad\nhost", APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
		if _, err := fetchOpencodeModels(context.Background(), nil, invalid); !errors.Is(err, errOpencodeInvalidBaseURL) {
			t.Fatalf("discovery err=%v", err)
		}
		ok, msg := checkOpencodeUpstream(context.Background(), nil, invalid)
		if ok || msg != "invalid base URL" {
			t.Fatalf("check ok=%v msg=%q", ok, msg)
		}
	})

	t.Run("redirect status preserves caller policy", func(t *testing.T) {
		withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
			return opencodeModelsHTTPResponse(http.StatusFound, `{"data":[]}`), nil
		})
		ok, msg := checkOpencodeUpstream(context.Background(), nil, p)
		if !ok || !strings.Contains(msg, "(0 models)") {
			t.Fatalf("check ok=%v msg=%q", ok, msg)
		}
		if _, err := fetchOpencodeModels(context.Background(), nil, p); err == nil || err.Error() != "HTTP 302" {
			t.Fatalf("discovery err=%v", err)
		}
	})

	for _, tc := range []struct {
		status int
		msg    string
	}{
		{http.StatusUnauthorized, "API key rejected (HTTP 401)"},
		{http.StatusForbidden, "API key rejected (HTTP 403)"},
		{http.StatusNotFound, "not found (HTTP 404) - check base URL and tier"},
		{http.StatusInternalServerError, "upstream returned HTTP 500"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
				return opencodeModelsHTTPResponse(tc.status, `{}`), nil
			})
			ok, msg := checkOpencodeUpstream(context.Background(), nil, p)
			if ok || msg != tc.msg {
				t.Fatalf("check ok=%v msg=%q", ok, msg)
			}
			if _, err := fetchOpencodeModels(context.Background(), nil, p); err == nil || err.Error() != "HTTP "+strconv.Itoa(tc.status) {
				t.Fatalf("discovery err=%v", err)
			}
		})
	}
}

func TestOpencodeModelQueriesTransportErrors(t *testing.T) {
	want := errors.New("network down")
	withOpencodeProbeTransport(t, func(*http.Request) (*http.Response, error) {
		return nil, want
	})
	p := &domain.Provider{BaseURL: opencode.ZenBaseURL, APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	if _, err := fetchOpencodeModels(context.Background(), nil, p); !errors.Is(err, want) {
		t.Fatalf("discovery err=%v", err)
	}
	ok, msg := checkOpencodeUpstream(context.Background(), nil, p)
	if ok || !strings.Contains(msg, "could not reach URL") || !strings.Contains(msg, "network down") {
		t.Fatalf("check ok=%v msg=%q", ok, msg)
	}
}

func opencodeModelsHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
