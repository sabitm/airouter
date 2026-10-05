package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/kiro"
)

func managementKiroProvider() *domain.Provider {
	p := kiroProbeProvider()
	p.OAuthCreds = &domain.OAuthCreds{
		KiroDiscovery: "management", KiroAuth: "builder-id", KiroIDP: "BuilderId",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:1:profile/account", Region: "us-east-1",
	}
	return p
}

func TestKiroManagementPaginationWire(t *testing.T) {
	calls := 0
	var deadline time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/" || r.URL.RawQuery != "" {
			t.Errorf("method=%s path=%s query=%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if body["origin"] != "AI_EDITOR" || body["profileArn"] != managementKiroProvider().OAuthCreds.ProfileArn {
			t.Errorf("body=%v", body)
		}
		for name, want := range map[string]string{
			"TokenType": "SSO_OIDC", "Authorization": "Bearer tok", "X-Kiro-Idp": "BuilderId",
			"X-Kiro-Profile-Arn": body["profileArn"], "Content-Type": kiro.JSONContentType, "X-Amz-Target": kiroManagementTarget,
		} {
			if values := r.Header.Values(name); len(values) != 1 || values[0] != want {
				t.Errorf("received %s=%v", name, values)
			}
		}
		if calls == 1 {
			if body["nextToken"] != "" {
				t.Error("first page had token")
			}
			_, _ = io.WriteString(w, `{"models":[{"modelId":"  Exact-ID  "}],"nextToken":"next /+= token"}`)
		} else {
			if calls != 2 || body["nextToken"] != "next /+= token" {
				t.Errorf("page=%d token=%q", calls, body["nextToken"])
			}
			_, _ = io.WriteString(w, `{"models":[{"modelId":"  Exact-ID  "},{"modelId":"second"}]}`)
		}
	}))
	defer server.Close()
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://management.eu-central-1.kiro.dev/" {
			t.Fatalf("regional endpoint=%s", req.URL)
		}
		d, ok := req.Context().Deadline()
		if !ok || time.Until(d) > kiroCatalogDeadline+time.Second {
			t.Fatal("catalog deadline missing or too long")
		}
		if deadline.IsZero() {
			deadline = d
		} else if !deadline.Equal(d) {
			t.Fatal("pagination reset deadline")
		}
		local := req.Clone(req.Context())
		endpoint, _ := http.NewRequest(http.MethodPost, server.URL+"/", nil)
		local.URL = endpoint.URL
		local.Host = ""
		return server.Client().Transport.RoundTrip(local)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	models, err := queryKiroModels(ctx, nil, managementKiroProvider())
	if err != nil || calls != 2 || !reflect.DeepEqual(models, []string{"  Exact-ID  ", "second"}) {
		t.Fatalf("models=%q calls=%d err=%v", models, calls, err)
	}
}

func TestKiroManagementPaginationFailures(t *testing.T) {
	cases := []struct {
		name string
		body func(int) string
		want error
		hits int
	}{
		{"repeat", func(int) string { return `{"models":[{"modelId":"m"}],"nextToken":"repeat"}` }, errKiroCatalogTokenRepeat, 2},
		{"cap", func(n int) string { return fmt.Sprintf(`{"models":[{"modelId":"m"}],"nextToken":"page-%d"}`, n) }, errKiroCatalogPageLimit, kiroCatalogMaxPages},
		{"empty", func(int) string { return `{"models":[]}` }, errKiroCatalogEmpty, 1},
		{"invalid JSON", func(int) string { return `{"models":` }, errKiroCatalogShape, 1},
		{"missing models", func(int) string { return `{}` }, errKiroCatalogShape, 1},
		{"invalid token", func(int) string { return `{"models":[{"modelId":"m"}],"nextToken":23}` }, errKiroCatalogShape, 1},
		{"trailing JSON", func(int) string { return `{"models":[]} {}` }, errKiroCatalogShape, 1},
		{"truncated", func(int) string { return `{"models":[{"modelId":"` + strings.Repeat("m", probeCaptureMax) + `"}]}` }, errKiroCatalogTruncated, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				hits++
				if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
					t.Fatal("unexpected fallback")
				}
				return kiroJSONResponse(http.StatusOK, tc.body(hits)), nil
			})
			p := managementKiroProvider()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			models, err := queryKiroManagementModels(ctx, nil, p, kiro.IdentityFromProvider(p))
			if models != nil || !errors.Is(err, tc.want) || hits != tc.hits {
				t.Fatalf("models=%v hits=%d err=%v want=%v", models, hits, err, tc.want)
			}
		})
	}
}

func TestKiroManagementRefreshAndFallbackShareDeadline(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, refreshes := 0, 0
			var deadline time.Time
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				d, ok := req.Context().Deadline()
				if !ok {
					t.Fatal("no deadline")
				}
				if calls == 1 {
					deadline = d
				} else if !deadline.Equal(d) {
					t.Fatal("retry or fallback reset deadline")
				}
				var body map[string]string
				_ = json.NewDecoder(req.Body).Decode(&body)
				switch calls {
				case 1:
					return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"old"}],"nextToken":"old-next"}`), nil
				case 2:
					if body["nextToken"] != "old-next" {
						t.Fatal("pagination missing token")
					}
					return kiroJSONResponse(status, `{}`), nil
				case 3:
					if body["nextToken"] != "" || req.Header.Get("Authorization") != "Bearer fresh" || req.Header.Get("X-Amz-Target") != kiroManagementTarget {
						t.Fatal("refresh did not restart management")
					}
					return kiroJSONResponse(http.StatusNotFound, `{}`), nil
				default:
					if calls != 4 || req.Header.Get("X-Amz-Target") != kiroCatalogTarget || req.Header.Get("Authorization") != "Bearer fresh" {
						t.Fatal("fallback used incorrect request")
					}
					return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"fallback"}]}`), nil
				}
			})
			models, err := queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(ctx context.Context) (string, error) {
				refreshes++
				d, ok := ctx.Deadline()
				if !ok || !d.Equal(deadline) {
					t.Fatal("refresh changed deadline")
				}
				return "fresh", nil
			})
			if err != nil || calls != 4 || refreshes != 1 || !reflect.DeepEqual(models, []string{"fallback"}) {
				t.Fatalf("models=%v calls=%d refreshes=%d err=%v", models, calls, refreshes, err)
			}
		})
	}
}

func TestKiroManagementSingleRefreshAndTransportFallback(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, refreshes := 0, 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
					t.Fatal("auth failure fell back to legacy")
				}
				return kiroJSONResponse(status, `{}`), nil
			})
			_, err := queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(context.Context) (string, error) {
				refreshes++
				return "fresh", nil
			})
			if kiroCatalogAuthStatus(err) != status || calls != 2 || refreshes != 1 {
				t.Fatalf("calls=%d refreshes=%d err=%v", calls, refreshes, err)
			}
		})
	}
	calls := 0
	var deadline time.Time
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		d, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("no deadline")
		}
		if calls == 1 {
			deadline = d
			return nil, errors.New("transport unavailable")
		}
		if calls != 2 || !d.Equal(deadline) || req.Header.Get("X-Amz-Target") != kiroCatalogTarget {
			t.Fatal("unbounded transport fallback")
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"fallback"}]}`), nil
	})
	models, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
	if err != nil || calls != 2 || !reflect.DeepEqual(models, []string{"fallback"}) {
		t.Fatalf("models=%v calls=%d err=%v", models, calls, err)
	}
}

func TestKiroManagementOuterSemanticFailuresDoNotFallBack(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "malformed", status: http.StatusOK, body: `{"models":`, want: errKiroCatalogShape},
		{name: "missing models", status: http.StatusOK, body: `{}`, want: errKiroCatalogShape},
		{name: "empty", status: http.StatusOK, body: `{"models":[]}`, want: errKiroCatalogEmpty},
		{name: "truncated", status: http.StatusOK, body: `{"models":[{"modelId":"` + strings.Repeat("m", probeCaptureMax) + `"}]}`, want: errKiroCatalogTruncated},
		{name: "bad request", status: http.StatusBadRequest, body: `{"models":[{"modelId":"legacy"}]}`},
		{name: "conflict", status: http.StatusConflict, body: `{"models":[{"modelId":"legacy"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				hits++
				if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
					t.Fatal("semantic failure fell back")
				}
				return kiroJSONResponse(tc.status, tc.body), nil
			})
			_, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
			if hits != 1 || !managementOuterError(err, tc.status, tc.want) {
				t.Fatalf("hits=%d err=%v want=%v", hits, err, tc.want)
			}
		})
	}
}

func managementOuterError(err error, status int, want error) bool {
	if want != nil {
		return errors.Is(err, want)
	}
	var got *kiroCatalogStatusError
	return errors.As(err, &got) && got.Status == status
}

func TestKiroManagementOuterPaginationAndAuthStayBounded(t *testing.T) {
	hits := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		hits++
		if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
			t.Fatal("pagination failure fell back")
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"m"}],"nextToken":"repeat"}`), nil
	})
	_, err := queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(context.Context) (string, error) {
		t.Fatal("pagination must not refresh")
		return "", nil
	})
	if hits != 2 || !errors.Is(err, errKiroCatalogTokenRepeat) {
		t.Fatalf("hits=%d err=%v", hits, err)
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, refreshes := 0, 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
					t.Fatal("status failure fell back")
				}
				return kiroJSONResponse(status, `{}`), nil
			})
			_, err := queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(context.Context) (string, error) {
				refreshes++
				return "fresh", nil
			})
			wantCalls, wantRefresh := 1, 0
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				wantCalls, wantRefresh = 2, 1
			}
			if calls != wantCalls || refreshes != wantRefresh {
				t.Fatalf("calls=%d refreshes=%d err=%v", calls, refreshes, err)
			}
		})
	}
}

func TestKiroManagementStatusAndReadFallback(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
						t.Fatal("first request was not management")
					}
					return kiroJSONResponse(status, `{}`), nil
				}
				if calls != 2 || req.Header.Get("X-Amz-Target") != kiroCatalogTarget {
					t.Fatal("status fallback was not one legacy request")
				}
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"legacy"}]}`), nil
			})
			models, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
			if err != nil || calls != 2 || !reflect.DeepEqual(models, []string{"legacy"}) {
				t.Fatalf("models=%v calls=%d err=%v", models, calls, err)
			}
		})
	}

	calls := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(errReader{}), Request: req,
			}, nil
		}
		if req.Header.Get("X-Amz-Target") != kiroCatalogTarget {
			t.Fatal("body-read failure did not fall back once")
		}
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"legacy"}]}`), nil
	})
	models, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
	if err != nil || calls != 2 || !reflect.DeepEqual(models, []string{"legacy"}) {
		t.Fatalf("models=%v calls=%d err=%v", models, calls, err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type fixedErrReader struct{ err error }

func (r fixedErrReader) Read([]byte) (int, error) { return 0, r.err }

func TestKiroManagementStatusBeforeBodyReadError(t *testing.T) {
	legacy := func(req *http.Request) (*http.Response, error) {
		t.Errorf("legacy fallback for %s", req.Header.Get("X-Amz-Target"))
		return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"legacy"}]}`), nil
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls, refreshes := 0, 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
					return legacy(req)
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(errReader{}), Request: req}, nil
			})
			_, err := queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(context.Context) (string, error) {
				refreshes++
				return "fresh", nil
			})
			wantCalls, wantRefresh := 1, 0
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				wantCalls, wantRefresh = 2, 1
			}
			if !managementOuterError(err, status, nil) || calls != wantCalls || refreshes != wantRefresh {
				t.Fatalf("calls=%d refreshes=%d err=%v", calls, refreshes, err)
			}
		})
	}

	calls := 0
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
			return legacy(req)
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(errReader{}), Request: req}, nil
	})
	_, err := queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), nil)
	if !managementOuterError(err, http.StatusBadRequest, nil) || calls != 1 {
		t.Fatalf("nil refresh calls=%d err=%v", calls, err)
	}
}

func TestKiroManagementTruncationBeforeLaterReadError(t *testing.T) {
	calls, refreshes := 0, 0
	overLimit := func() io.Reader {
		return io.MultiReader(strings.NewReader(strings.Repeat("x", probeCaptureMax+1)), errReader{})
	}
	withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
			t.Fatal("truncation fell back")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(overLimit()), Request: req}, nil
	})
	_, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
	if !errors.Is(err, errKiroCatalogTruncated) || calls != 1 {
		t.Fatalf("query calls=%d err=%v", calls, err)
	}
	calls = 0
	_, err = queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(context.Context) (string, error) {
		refreshes++
		return "fresh", nil
	})
	if !errors.Is(err, errKiroCatalogTruncated) || calls != 1 || refreshes != 0 {
		t.Fatalf("refresh calls=%d refreshes=%d err=%v", calls, refreshes, err)
	}
}

func TestKiroManagementAllowedReadErrorsStillFallBack(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			var deadline time.Time
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				d, ok := req.Context().Deadline()
				if !ok {
					t.Fatal("no deadline")
				}
				if calls == 1 {
					deadline = d
					if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
						t.Fatal("first request was not management")
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(errReader{}), Request: req}, nil
				}
				if calls != 2 || !d.Equal(deadline) || req.Header.Get("X-Amz-Target") != kiroCatalogTarget {
					t.Fatal("read-error fallback was not one bounded legacy request")
				}
				return kiroJSONResponse(http.StatusOK, `{"models":[{"modelId":"legacy"}]}`), nil
			})
			models, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
			if err != nil || calls != 2 || !reflect.DeepEqual(models, []string{"legacy"}) {
				t.Fatalf("models=%v calls=%d err=%v", models, calls, err)
			}
		})
	}
}

func TestKiroManagementCanceledReadAfterStatusDoesNotFallBack(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			calls, refreshes := 0, 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("X-Amz-Target") != kiroManagementTarget {
					t.Fatal("context read error fell back")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{},
					Body:       io.NopCloser(fixedErrReader{err: want}),
					Request:    req,
				}, nil
			})
			_, err := queryKiroModels(context.Background(), nil, managementKiroProvider())
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("query calls=%d err=%v", calls, err)
			}
			calls = 0
			_, err = queryKiroModelsWithRefresh(context.Background(), nil, managementKiroProvider(), func(context.Context) (string, error) {
				refreshes++
				return "fresh", nil
			})
			if !errors.Is(err, want) || calls != 1 || refreshes != 0 {
				t.Fatalf("refresh calls=%d refreshes=%d err=%v", calls, refreshes, err)
			}
		})
	}
}

func TestKiroManagementCanceledDeadlineDoesNotFallBack(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			hits := 0
			withKiroCatalogTransport(t, func(req *http.Request) (*http.Response, error) {
				hits++
				return nil, req.Context().Err()
			})
			_, err := queryKiroModels(ctx, nil, managementKiroProvider())
			if !errors.Is(err, want) || hits > 1 {
				t.Fatalf("hits=%d err=%v", hits, err)
			}
		})
	}
}
