package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/kirocatalog"
	"airouter/internal/observability"
	"airouter/internal/proxy/kiro"
)

type kiroRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kiroRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withKiroCatalogTransport(t *testing.T, fn kiroRoundTripFunc) {
	t.Helper()
	previous := kirocatalog.CurrentDefaultHTTPClient()
	fake := &http.Client{Transport: fn}
	kirocatalog.SetDefaultHTTPClient(fake)
	t.Cleanup(func() { kirocatalog.SetDefaultHTTPClient(previous) })
}

// OAuth's default client resolves its transport at send time. These tests run
// serially and intercept only the fixture's token endpoint, never real auth.
func withKiroTokenTransport(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := http.DefaultTransport
	endpoint := server.URL
	http.DefaultTransport = kiroRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://oidc.us-east-1.amazonaws.com/token" {
			t.Errorf("unexpected token request to %s", req.URL.Redacted())
			return nil, errors.New("unexpected test token endpoint")
		}
		local := req.Clone(req.Context())
		parsed, err := http.NewRequest(req.Method, endpoint, nil)
		if err != nil {
			return nil, err
		}
		local.URL = parsed.URL
		local.Host = ""
		return previous.RoundTrip(local)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func kiroJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func TestDashboardDiscoveryPopulatesSharedService(t *testing.T) {
	var hits atomic.Int32
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		hits.Add(1)
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"shared","additionalModelRequestFieldsSchema":{"properties":{"output_config":{"properties":{"effort":{"enum":["low","high"],"default":"low"}}}}}}]}`), nil
	})
	st := newWebTestStore(t)
	p := &domain.Provider{Name: "k", BaseURL: "https://catalog.example", APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	catalog := kirocatalog.New(nil)
	h := NewHandlerWithDeps(st, nil, nil, nil, catalog)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/providers/models?provider_id=1", nil)
	rec := httptest.NewRecorder()
	h.providerModels(rec, req)
	if rec.Code != http.StatusOK || hits.Load() != 1 || !strings.Contains(rec.Body.String(), "shared") {
		t.Fatalf("status=%d hits=%d body=%s", rec.Code, hits.Load(), rec.Body.String())
	}
	models, err := catalog.Models(context.Background(), nil, p)
	if err != nil || hits.Load() != 1 || len(models) != 1 || models[0].Capability == nil {
		t.Fatalf("shared cache not populated models=%v hits=%d err=%v", models, hits.Load(), err)
	}
	ok, msg := h.checkKiroUpstream(context.Background(), p, nil)
	if !ok || hits.Load() != 2 || !strings.Contains(msg, "catalog access confirmed") {
		t.Fatalf("check used cache ok=%v hits=%d msg=%s", ok, hits.Load(), msg)
	}
}

func TestKiroCatalogQuerySharedByCheckAndDiscovery(t *testing.T) {
	const model = "Claude-Sonnet-4.5"
	for _, discovery := range []bool{false, true} {
		name := "check"
		if discovery {
			name = "discovery"
		}
		t.Run(name, func(t *testing.T) {
			var got *http.Request
			var raw []byte
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				got = req.Clone(req.Context())
				raw, _ = io.ReadAll(req.Body)
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"`+model+`"},{"modelId":"  "},{"modelName":"skip"}]}`), nil
			})
			p := &domain.Provider{
				BaseURL:    "https://codewhisperer.example/custom/",
				APIKey:     "catalog-token",
				Protocol:   domain.ProtocolKiro,
				AuthMethod: domain.AuthOAuth,
				OAuthCreds: &domain.OAuthCreds{KiroAuth: "idc", ProfileArn: "arn:configured", Region: "eu-west-1"},
			}
			var models []string
			var err error
			if discovery {
				models, err = fetchUpstreamModels(context.Background(), nil, p)
			} else {
				ok, msg := checkKiroUpstream(context.Background(), nil, p, nil)
				if !ok || !strings.Contains(msg, "(1 models)") || !strings.Contains(msg, "does not confirm chat readiness") {
					t.Fatalf("check ok=%v msg=%q", ok, msg)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if discovery && strings.Join(models, ",") != model {
				t.Fatalf("models = %v", models)
			}
			if got == nil || got.URL.String() != "https://codewhisperer.example/custom/" {
				t.Fatalf("URL = %v", got)
			}
			if got.Header.Get("X-Amz-Target") != kiroCatalogTarget || got.Header.Get("Content-Type") != kiroCatalogJSONType ||
				got.Header.Get("Accept") != kiroCatalogJSONAccept || got.Header.Get("User-Agent") != kiro.UserAgent ||
				got.Header.Get("X-Amz-User-Agent") != kiro.XAmzUserAgent || got.Header.Get("Amz-Sdk-Request") != kiro.AmzSdkRequest {
				t.Fatalf("headers = %v", got.Header)
			}
			if !isUUID(got.Header.Get("Amz-Sdk-Invocation-Id")) {
				t.Fatalf("invocation id = %q", got.Header.Get("Amz-Sdk-Invocation-Id"))
			}
			if got.Header.Get("Authorization") != "Bearer catalog-token" || len(got.Header.Values("TokenType")) != 1 || got.Header.Get("TokenType") != "SSO_OIDC" {
				t.Fatalf("auth headers = %v", got.Header)
			}
			if string(raw) != `{"origin":"AI_EDITOR","profileArn":"arn:configured"}` {
				t.Fatalf("body = %s", raw)
			}
			if strings.Contains(got.URL.String(), "eu-west-1") || strings.Contains(string(raw), "eu-west-1") {
				t.Fatalf("region became a request destination: url=%s body=%s", got.URL, raw)
			}
		})
	}
}

func TestKiroCatalogBuilderIDKeepsAuthProfile(t *testing.T) {
	for _, auth := range []string{"builder-id", "Builder_ID", "Builder ID", "builderid"} {
		t.Run(auth, func(t *testing.T) {
			var raw []byte
			var target string
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				target = req.Header.Get("X-Amz-Target")
				raw, _ = io.ReadAll(req.Body)
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"auto"}]}`), nil
			})
			p := &domain.Provider{
				BaseURL: "https://custom.example", APIKey: "tok", Protocol: domain.ProtocolKiro,
				AuthMethod: domain.AuthOAuth,
				OAuthCreds: &domain.OAuthCreds{KiroAuth: auth, ProfileArn: "arn:from-auth"},
			}
			if _, err := queryKiroModels(context.Background(), nil, p); err != nil {
				t.Fatal(err)
			}
			if target != kiroCatalogTarget || strings.Contains(target, "ListAvailableProfiles") {
				t.Fatalf("target = %q", target)
			}
			if !strings.Contains(string(raw), "arn:from-auth") || strings.Contains(string(raw), "placeholder") {
				t.Fatalf("body = %s", raw)
			}
		})
	}
}

func TestKiroCatalogCredentialHeaders(t *testing.T) {
	cases := []struct {
		name       string
		provider   domain.Provider
		authHeader string
		authValue  string
		tokenType  string
	}{
		{
			name: "api key bearer",
			provider: domain.Provider{
				APIKey: "key-1", AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthBearer,
				OAuthCreds: &domain.OAuthCreds{ProfileArn: "arn:api"},
			},
			authHeader: "Authorization", authValue: "Bearer key-1", tokenType: "API_KEY",
		},
		{
			name: "api key x-api-key",
			provider: domain.Provider{
				APIKey: "key-2", AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthXAPIKey,
			},
			authHeader: "x-api-key", authValue: "key-2", tokenType: "API_KEY",
		},
		{
			name: "external idp",
			provider: domain.Provider{
				APIKey: "idp-tok", AuthMethod: domain.AuthOAuth,
				OAuthCreds: &domain.OAuthCreds{KiroAuth: "external_idp", ProfileArn: "arn:idp"},
			},
			authHeader: "Authorization", authValue: "Bearer idp-tok", tokenType: "EXTERNAL_IDP",
		},
		{
			name: "social keeps profile",
			provider: domain.Provider{
				APIKey: "social-tok", AuthMethod: domain.AuthOAuth,
				OAuthCreds: &domain.OAuthCreds{KiroAuth: "social", ProfileArn: "arn:social"},
			},
			authHeader: "Authorization", authValue: "Bearer social-tok",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *http.Request
			var raw []byte
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				got = req.Clone(req.Context())
				raw, _ = io.ReadAll(req.Body)
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}]}`), nil
			})
			tc.provider.BaseURL = "https://configured.example/root"
			tc.provider.Protocol = domain.ProtocolKiro
			if _, err := queryKiroModels(context.Background(), nil, &tc.provider); err != nil {
				t.Fatal(err)
			}
			if got.URL.String() != "https://configured.example/root/" || got.Header.Get(tc.authHeader) != tc.authValue {
				t.Fatalf("url=%s headers=%v", got.URL, got.Header)
			}
			if tc.tokenType == "" {
				if got.Header.Get("TokenType") != "" || len(got.Header.Values("TokenType")) != 0 {
					t.Fatalf("unexpected token marker: %v", got.Header)
				}
			} else if len(got.Header.Values("TokenType")) != 1 || got.Header.Get("TokenType") != tc.tokenType {
				t.Fatalf("TokenType = %#v", got.Header.Values("TokenType"))
			}
			if tc.provider.OAuthCreds != nil && tc.provider.OAuthCreds.ProfileArn != "" && !strings.Contains(string(raw), tc.provider.OAuthCreds.ProfileArn) {
				t.Fatalf("profile missing from body %s", raw)
			}
		})
	}
}

func TestKiroCatalogPaginationAndFailures(t *testing.T) {
	t.Run("combine pages and dedup", func(t *testing.T) {
		var tokens []string
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			tokens = append(tokens, string(raw))
			if !strings.Contains(string(raw), "nextToken") {
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"A"},{"modelId":"B"}],"nextToken":"page-2"}`), nil
			}
			return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"B"},{"modelId":"c"}],"nextToken":""}`), nil
		})
		models, err := queryKiroModels(context.Background(), nil, kiroProbeProvider())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(models, ",") != "A,B,c" {
			t.Fatalf("models = %v", models)
		}
		if len(tokens) != 2 || strings.Contains(tokens[1], "page-2") == false {
			t.Fatalf("page bodies = %v", tokens)
		}
	})

	t.Run("second page failure refuses partial", func(t *testing.T) {
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(raw), "nextToken") {
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"only-page-1"}],"nextToken":"more"}`), nil
			}
			return kiroJSONResponse(http.StatusBadGateway, `{"message":"down"}`), nil
		})
		models, err := queryKiroModels(context.Background(), nil, kiroProbeProvider())
		if models != nil || !errors.As(err, new(*kiroCatalogStatusError)) {
			t.Fatalf("models=%v err=%v", models, err)
		}
	})

	t.Run("page limit", func(t *testing.T) {
		n := 0
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			n++
			return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}],"nextToken":"page-`+strconv.Itoa(n)+`"}`), nil
		})
		if _, err := queryKiroModels(context.Background(), nil, kiroProbeProvider()); !errors.Is(err, errKiroCatalogPageLimit) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("repeated token", func(t *testing.T) {
		n := 0
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			n++
			token := "same"
			if n == 1 {
				token = "next"
			}
			return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}],"nextToken":"`+token+`"}`), nil
		})
		if _, err := queryKiroModels(context.Background(), nil, kiroProbeProvider()); !errors.Is(err, errKiroCatalogTokenRepeat) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestKiroCatalogParseFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "malformed", body: `{"models":`, want: errKiroCatalogShape},
		{name: "wrong shape", body: `{"data":[{"modelId":"m"}]}`, want: errKiroCatalogShape},
		{name: "models null", body: `{"models":null}`, want: errKiroCatalogShape},
		{name: "modelId wrong type", body: `{"models":[{"modelId":1}]}`, want: errKiroCatalogShape},
		{name: "nextToken wrong type", body: `{"models":[{"modelId":"m"}],"nextToken":1}`, want: errKiroCatalogShape},
		{name: "empty usable", body: `{"models":[{"modelId":""},{"modelName":"x"}]}`, want: errKiroCatalogEmpty},
		{name: "empty array", body: `{"models":[]}`, want: errKiroCatalogEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				return kiroJSONResponse(http.StatusOK, tc.body), nil
			})
			models, err := queryKiroModels(context.Background(), nil, kiroProbeProvider())
			if models != nil || !errors.Is(err, tc.want) {
				t.Fatalf("models=%v err=%v", models, err)
			}
			ok, msg := checkKiroUpstream(context.Background(), nil, kiroProbeProvider(), nil)
			if ok || msg == "" || strings.Contains(msg, "OK") {
				t.Fatalf("check ok=%v msg=%q", ok, msg)
			}
		})
	}
}

func TestKiroCatalogStatusAndTransport(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		want   string
	}{
		{name: "401", status: http.StatusUnauthorized, want: "credential rejected (HTTP 401)"},
		{name: "403", status: http.StatusForbidden, want: "access denied (HTTP 403)"},
		{name: "404", status: http.StatusNotFound, want: "not found (HTTP 404)"},
		{name: "500", status: http.StatusInternalServerError, want: "upstream unavailable (HTTP 500)"},
		{name: "network", err: errors.New("dial failed"), want: "could not reach Kiro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return kiroJSONResponse(tc.status, `{"message":"secret-upstream"}`), nil
			})
			ok, msg := checkKiroUpstream(context.Background(), nil, kiroProbeProvider(), nil)
			if ok || !strings.Contains(msg, tc.want) || strings.Contains(msg, "secret-upstream") {
				t.Fatalf("ok=%v msg=%q", ok, msg)
			}
		})
	}
}

func TestKiroCatalogTruncatedAndCanceled(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			body := `{"models":[{"modelId":"` + strings.Repeat("m", probeCaptureMax) + `"}]}`
			return kiroJSONResponse(http.StatusOK, body), nil
		})
		if _, err := queryKiroModels(context.Background(), nil, kiroProbeProvider()); !errors.Is(err, errKiroCatalogTruncated) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			return nil, req.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := queryKiroModels(ctx, nil, kiroProbeProvider())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
			return nil, req.Context().Err()
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond)
		_, err := queryKiroModels(ctx, nil, kiroProbeProvider())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestKiroCatalogTraceOmitsSecrets(t *testing.T) {
	const model = "model-id-sentinel-not-for-logs"
	const token = "next-token-sentinel-not-for-logs"
	const cred = "credential-sentinel-not-for-logs"
	var buf bytes.Buffer
	logger := observability.NewLogger(2, &buf)
	n := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		n++
		if n == 1 {
			return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"`+model+`"}],"nextToken":"`+token+`"}`), nil
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"second"}]}`), nil
	})
	ctx := observability.WithRequestID(context.Background(), "req-kiro-1")
	models, err := queryKiroModels(ctx, logger, &domain.Provider{
		BaseURL: "https://catalog.example", APIKey: cred, Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthAPIKey, OAuthCreds: &domain.OAuthCreds{ProfileArn: "arn:trace"},
	})
	if err != nil || strings.Join(models, ",") != model+",second" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	out := buf.String()
	if !strings.Contains(out, "request_id=req-kiro-1") || !strings.Contains(out, "operation=kiro_models") {
		t.Fatalf("missing trace metadata: %s", out)
	}
	for _, secret := range []string{model, token, cred, "arn:trace", "Authorization", "tokentype"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaked %q: %s", secret, out)
		}
	}
}

func TestKiroCatalogDoesNotMutateProvider(t *testing.T) {
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}]}`), nil
	})
	creds := &domain.OAuthCreds{KiroAuth: "idc", ProfileArn: "arn:orig", Region: "us-west-2"}
	p := &domain.Provider{BaseURL: "https://catalog.example", APIKey: "orig", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth, OAuthCreds: creds}
	if _, err := queryKiroModels(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	if p.APIKey != "orig" || creds.ProfileArn != "arn:orig" || creds.Region != "us-west-2" {
		t.Fatalf("provider mutated: %+v %+v", p, creds)
	}
}

func TestKiroCatalogFreshInvocationID(t *testing.T) {
	var ids []string
	var mu sync.Mutex
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		ids = append(ids, req.Header.Get("Amz-Sdk-Invocation-Id"))
		mu.Unlock()
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}]}`), nil
	})
	p := kiroProbeProvider()
	if _, err := queryKiroModels(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	if _, err := queryKiroModels(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("invocation ids = %v", ids)
	}
}

func TestKiroCheckRefreshOnce(t *testing.T) {
	var saw []string
	var mu sync.Mutex
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		saw = append(saw, req.Header.Get("Authorization"))
		n := len(saw)
		mu.Unlock()
		if n == 1 {
			return kiroJSONResponse(http.StatusUnauthorized, `{}`), nil
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"fresh-model"}]}`), nil
	})
	calls := 0
	ok, msg := checkKiroUpstream(context.Background(), nil, kiroProbeProvider(), func(context.Context) (string, error) {
		calls++
		return "fresh-token", nil
	})
	if !ok || calls != 1 || strings.Join(saw, ",") != "Bearer tok,Bearer fresh-token" {
		t.Fatalf("ok=%v msg=%q calls=%d auth=%v", ok, msg, calls, saw)
	}
}

func TestKiroCheckRefreshFailureNoPartial(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			hits := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				hits++
				return kiroJSONResponse(status, `{}`), nil
			})
			ok, msg := checkKiroUpstream(context.Background(), nil, kiroProbeProvider(), func(context.Context) (string, error) {
				return "", errors.New("refresh down")
			})
			if ok || hits != 1 || !strings.Contains(msg, "token refresh failed") {
				t.Fatalf("ok=%v hits=%d msg=%q", ok, hits, msg)
			}
		})
	}
}

func TestKiroCheckNoRefreshWithoutCallback(t *testing.T) {
	hits := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		hits++
		return kiroJSONResponse(http.StatusForbidden, `{}`), nil
	})
	ok, msg := checkKiroUpstream(context.Background(), nil, kiroProbeProvider(), nil)
	if ok || hits != 1 || !strings.Contains(msg, "access denied") {
		t.Fatalf("ok=%v hits=%d msg=%q", ok, hits, msg)
	}
}

func TestKiroCatalogPreservesExactIDsAndTokens(t *testing.T) {
	const token = " \tOpaque+/=\n "
	wantModels := []string{"  Case-Sensitive  ", "Case-Sensitive", "second-model"}
	calls := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]string
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			body, _ := json.Marshal(map[string]any{
				"models": []map[string]string{
					{"modelId": wantModels[0]}, {"modelId": wantModels[1]}, {"modelId": " \t\n "},
				},
				"nextToken": token,
			})
			return kiroJSONResponse(http.StatusOK, string(body)), nil
		}
		if calls != 2 || payload["nextToken"] != token {
			t.Fatalf("page %d nextToken = %q, want %q", calls, payload["nextToken"], token)
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"  Case-Sensitive  "},{"modelId":"second-model"}]}`), nil
	})
	models, err := queryKiroModels(context.Background(), nil, kiroProbeProvider())
	if err != nil || calls != 2 || !reflect.DeepEqual(models, wantModels) {
		t.Fatalf("models=%q calls=%d err=%v", models, calls, err)
	}
}

func TestKiroCatalogSingleDeadlineAcrossRefresh(t *testing.T) {
	var deadline time.Time
	calls := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		got, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("catalog request has no deadline")
		}
		if calls == 1 {
			deadline = got
			time.Sleep(5 * time.Millisecond)
			return kiroJSONResponse(http.StatusUnauthorized, `{}`), nil
		}
		if !got.Equal(deadline) {
			t.Fatalf("retry deadline = %s, initial deadline = %s", got, deadline)
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"fresh"}]}`), nil
	})
	refreshes := 0
	models, err := queryKiroModelsWithRefresh(context.Background(), nil, kiroProbeProvider(), func(ctx context.Context) (string, error) {
		refreshes++
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(deadline) {
			t.Fatalf("refresh deadline = %s, initial deadline = %s", got, deadline)
		}
		return "fresh-token", nil
	})
	if err != nil || calls != 2 || refreshes != 1 || !reflect.DeepEqual(models, []string{"fresh"}) {
		t.Fatalf("models=%v calls=%d refreshes=%d err=%v", models, calls, refreshes, err)
	}
}

func TestKiroCatalogRefreshRestartsPagination(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				var payload map[string]string
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				switch calls {
				case 1:
					return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"old-page"}],"nextToken":"old-next"}`), nil
				case 2:
					if payload["nextToken"] != "old-next" {
						t.Fatalf("initial pagination token = %q", payload["nextToken"])
					}
					return kiroJSONResponse(status, `{}`), nil
				case 3:
					if payload["nextToken"] != "" || req.Header.Get("Authorization") != "Bearer fresh-token" {
						t.Fatal("refreshed catalog did not restart at the first page")
					}
					return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"fresh-page"}],"nextToken":"fresh-next"}`), nil
				default:
					if calls != 4 || payload["nextToken"] != "fresh-next" {
						t.Fatalf("retry pagination: call=%d token=%q", calls, payload["nextToken"])
					}
					return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"fresh-last"}]}`), nil
				}
			})
			refreshes := 0
			models, err := queryKiroModelsWithRefresh(context.Background(), nil, kiroProbeProvider(), func(context.Context) (string, error) {
				refreshes++
				return "fresh-token", nil
			})
			if err != nil || calls != 4 || refreshes != 1 || !reflect.DeepEqual(models, []string{"fresh-page", "fresh-last"}) {
				t.Fatalf("models=%v calls=%d refreshes=%d err=%v", models, calls, refreshes, err)
			}
		})
	}
}

func TestKiroCatalogRefreshDoesNotRetryTwice(t *testing.T) {
	calls, refreshes := 0, 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return kiroJSONResponse(http.StatusForbidden, `{}`), nil
	})
	models, err := queryKiroModelsWithRefresh(context.Background(), nil, kiroProbeProvider(), func(context.Context) (string, error) {
		refreshes++
		return "fresh-token", nil
	})
	if models != nil || kiroCatalogAuthStatus(err) != http.StatusForbidden || calls != 2 || refreshes != 1 {
		t.Fatalf("models=%v calls=%d refreshes=%d err=%v", models, calls, refreshes, err)
	}
}

func TestKiroCatalogCanceledRefreshHasNoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return kiroJSONResponse(http.StatusUnauthorized, `{}`), nil
	})
	models, err := queryKiroModelsWithRefresh(ctx, nil, kiroProbeProvider(), func(context.Context) (string, error) {
		cancel()
		return "fresh-token", nil
	})
	if models != nil || !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("models=%v calls=%d err=%v", models, calls, err)
	}
}

func TestKiroManagementDiscoveryFallback(t *testing.T) {
	var urls []string
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		urls = append(urls, req.URL.String())
		raw, _ := io.ReadAll(req.Body)
		if strings.Contains(req.URL.Host, "management.") {
			if req.Method != http.MethodPost || req.URL.Path != "/" || req.URL.RawQuery != "" {
				t.Fatalf("management method/path/query = %s %s %q", req.Method, req.URL.Path, req.URL.RawQuery)
			}
			if !strings.Contains(string(raw), `"origin":"AI_EDITOR"`) || !strings.Contains(string(raw), `"profileArn":"arn:from-auth"`) {
				t.Fatalf("management body = %s", raw)
			}
			if req.Header.Get("X-Amz-Target") != kiroManagementTarget || req.Header.Get("Content-Type") != kiroCatalogJSONType {
				t.Fatalf("management headers = %v", req.Header)
			}
			if req.Header.Get("TokenType") != "SSO_OIDC" || len(req.Header.Values("TokenType")) != 1 {
				t.Fatalf("token type = %#v", req.Header.Values("TokenType"))
			}
			return kiroJSONResponse(http.StatusNotFound, `{}`), nil
		}
		if req.Method != http.MethodPost || req.URL.String() != "https://catalog.example/" || !strings.Contains(string(raw), "arn:from-auth") {
			t.Fatalf("legacy fallback = %s %s body=%s", req.Method, req.URL, raw)
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"legacy-model"}]}`), nil
	})
	p := kiroProbeProvider()
	p.OAuthCreds = &domain.OAuthCreds{KiroAuth: "builder-id", KiroIDP: "BuilderId", KiroDiscovery: "management", ProfileArn: "arn:from-auth", Region: "eu-central-1"}
	models, err := queryKiroModels(context.Background(), nil, p)
	if err != nil || len(models) != 1 || models[0] != "legacy-model" {
		t.Fatalf("models=%v err=%v urls=%v", models, err, urls)
	}
	if len(urls) != 2 || urls[0] != "https://management.eu-central-1.kiro.dev/" {
		t.Fatalf("urls = %v", urls)
	}
}

func TestKiroManagementAuthDoesNotFallBack(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			hits := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				hits++
				return kiroJSONResponse(status, `{}`), nil
			})
			p := kiroProbeProvider()
			p.OAuthCreds = &domain.OAuthCreds{KiroDiscovery: "management", Region: "us-east-1"}
			_, err := queryKiroModels(context.Background(), nil, p)
			if hits != 1 || kiroCatalogAuthStatus(err) == 0 && status != http.StatusBadRequest {
				t.Fatalf("hits=%d err=%v", hits, err)
			}
			if status == http.StatusBadRequest && !errors.As(err, new(*kiroCatalogStatusError)) {
				t.Fatalf("400 was swallowed: %v", err)
			}
		})
	}
}

func kiroProbeProvider() *domain.Provider {
	return &domain.Provider{
		BaseURL: "https://catalog.example", APIKey: "tok", Protocol: domain.ProtocolKiro,
		AuthMethod: domain.AuthOAuth,
	}
}

func isUUID(v string) bool {
	parts := strings.Split(v, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 16, 64); err != nil {
			return false
		}
	}
	return true
}
