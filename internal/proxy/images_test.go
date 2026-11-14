package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/observability"
	"airouter/internal/proxy/responses"
	"airouter/internal/store"
)

const imagePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a7X8AAAAASUVORK5CYII="
const imageUpstreamBody = `{"created":123,"data":[{"b64_json":"` + imagePNG + `","generation_id":"gen-1"}],"output_format":"png","usage":{"input_tokens":3,"output_tokens":400,"total_tokens":403,"output_tokens_details":{"image_tokens":400}}}`
const imageBody = `{"model":"image-combo","prompt":"draw a lake"}`

func imageTestProxy(t *testing.T, providers ...*domain.Provider) (*Proxy, *store.Store, *domain.Combo) {
	t.Helper()
	st := newTestStore(t)
	combo := &domain.Combo{Name: "image-combo", Strategy: domain.StrategyFailover}
	for _, provider := range providers {
		if err := st.CreateProvider(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
		combo.Targets = append(combo.Targets, domain.ComboTarget{
			ProviderID: provider.ID, UpstreamModel: "gpt-image-2.6-flare", Enabled: true,
		})
	}
	if err := st.CreateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	return New(st, nil), st, combo
}

func imageTestRequest(t *testing.T, p *Proxy, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	before, err := p.store.CountRequestLogsQuery(context.Background(), store.RequestLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p.Mount(mux)
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	waitForLogs(t, p.store, int(before)+1)
	return w
}

func imageTestTransport(status int, body string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": {"application/json"}}, Request: req}, nil
	})
}

func TestImagesNativeRoutesEnvelopeAndUsage(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/images/generations"} {
		t.Run(path, func(t *testing.T) {
			provider := &domain.Provider{Name: "codex", BaseURL: "https://provider.example/codex", Protocol: domain.ProtocolOpenAICodex,
				AuthMethod: domain.AuthAPIKey, AuthScheme: domain.AuthBearer, APIKey: "up-secret",
				OAuthCreds: &domain.OAuthCreds{AccountID: "account-1", AccessToken: "must-not-replace-static-key"}}
			p, st, _ := imageTestProxy(t, provider)
			if p.client.Timeout != 5*time.Minute {
				t.Fatalf("unary timeout = %s", p.client.Timeout)
			}
			var captured http.Header
			var capturedBody map[string]any
			p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "https://provider.example/codex/images/generations" {
					t.Errorf("upstream URL = %s", req.URL)
				}
				captured = req.Header.Clone()
				if err := json.NewDecoder(req.Body).Decode(&capturedBody); err != nil {
					t.Error(err)
				}
				return imageTestTransport(200, imageUpstreamBody).RoundTrip(req)
			})}
			p.streamClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("Images used streaming transport")
				return nil, nil
			})}
			mux := http.NewServeMux()
			p.Mount(mux)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"image-combo","prompt":"draw a lake","user":"private-user","stream":false}`))
			req.Header.Set("Authorization", "Bearer client-secret")
			req.Header.Set("User-Agent", "caller-agent")
			req.Header.Set("X-Codex-Image-Turn-Id", "caller-turn")
			req.Header.Set("X-Private-Header", "must-not-forward")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			want := map[string]any{"model": "gpt-image-2.6-flare", "prompt": "draw a lake", "n": float64(1), "size": "auto", "quality": "auto", "background": "auto", "output_format": "png"}
			if !reflect.DeepEqual(capturedBody, want) {
				t.Fatalf("upstream body = %+v", capturedBody)
			}
			if captured.Get("Authorization") != "Bearer up-secret" || captured.Get("Chatgpt-Account-Id") != "account-1" ||
				captured.Get("User-Agent") != "codex_cli_rs/"+responses.CodexCLIVersion || captured.Get("Originator") != "codex_cli_rs" ||
				captured.Get("Accept") != "application/json" || captured.Get("Content-Type") != "application/json" {
				t.Fatalf("headers=%v", captured)
			}
			if captured.Get("Session_id") == "" || captured.Get("Session_id") != captured.Get("X-Codex-Image-Turn-Id") ||
				captured.Get("X-Codex-Image-Turn-Id") == "caller-turn" || captured.Get("X-Private-Header") != "" {
				t.Fatalf("identity/client header handling=%v", captured)
			}
			var got, expected any
			_ = json.Unmarshal(w.Body.Bytes(), &got)
			_ = json.Unmarshal([]byte(imageUpstreamBody), &expected)
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("response = %s", w.Body.String())
			}
			logs := waitForLogs(t, st, 1)
			if len(logs) != 1 || logs[0].InputTokens != 3 || logs[0].OutputTokens != 400 ||
				logs[0].UsageEstimated || logs[0].Stream || logs[0].Format != "oai-images" {
				t.Fatalf("logs=%+v", logs)
			}
			if imagesCodec.id == codexImagesCodec.id || codexImagesCodec.id == codexCodec.id {
				t.Fatal("image codec IDs must be distinct")
			}
		})
	}
}

func TestImagesAccessKeysAndAuthScheme(t *testing.T) {
	p, st, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex, AuthScheme: domain.AuthXAPIKey, APIKey: "up-key"})
	var calls int
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("X-Api-Key") != "up-key" || req.Header.Get("Authorization") != "" {
			t.Errorf("auth scheme inferred from protocol: %v", req.Header)
		}
		return imageTestTransport(200, imageUpstreamBody).RoundTrip(req)
	})}
	if w := imageTestRequest(t, p, "/images/generations", imageBody, ""); w.Code != 200 {
		t.Fatalf("open mode: %d %s", w.Code, w.Body.String())
	}
	key, err := st.NewAccessKey(context.Background(), "images")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong"} {
		w := imageTestRequest(t, p, "/images/generations", imageBody, token)
		if w.Code != 401 || !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("auth rejection=%d %s", w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized upstream calls=%d", calls)
	}
	if w := imageTestRequest(t, p, "/images/generations", imageBody, key.Token); w.Code != 200 {
		t.Fatalf("bearer client key=%d %s", w.Code, w.Body.String())
	}
	mux := http.NewServeMux()
	p.Mount(mux)
	r := httptest.NewRequest(http.MethodPost, "/images/generations", strings.NewReader(imageBody))
	r.Header.Set("X-Api-Key", key.Token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || calls != 3 {
		t.Fatalf("x-api-key client auth=%d calls=%d", w.Code, calls)
	}
}

func TestImagesLocalRejectionsDoNotSendOrPenalize(t *testing.T) {
	p, st, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
	p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid local request sent upstream")
		return nil, nil
	})}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{`, 400}, {`{"prompt":"draw"}`, 400}, {`{"model":"missing","prompt":"draw"}`, 404},
		{`{"model":"image-combo"}`, 400}, {`{"model":"image-combo","prompt":" "}`, 400},
		{`{"model":"image-combo","prompt":"draw","stream":true}`, 400},
		{`{"model":"image-combo","prompt":"draw","n":2}`, 400},
		{`{"model":"image-combo","prompt":"draw","size":"1024x1024"}`, 400},
		{`{"model":"image-combo","prompt":"draw","output_format":"jpeg"}`, 400},
		{`{"model":"image-combo","prompt":"draw","image":"ref"}`, 400},
	} {
		w := imageTestRequest(t, p, "/images/generations", tc.body, "")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("body=%s status=%d response=%s", tc.body, w.Code, w.Body.String())
		}
	}
	mux := http.NewServeMux()
	p.Mount(mux)
	r := httptest.NewRequest(http.MethodPost, "/images/generations", strings.NewReader(imageBody))
	r.Header.Set("Content-Type", "multipart/form-data")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 400 || len(p.bo) != 0 || len(p.rr) != 0 {
		t.Fatalf("local rejection touched health/order: %d bo=%v rr=%v", w.Code, p.bo, p.rr)
	}
	providers, err := st.ListProviders(context.Background())
	if err != nil || len(providers) != 1 {
		t.Fatalf("providers=%v err=%v", providers, err)
	}
}

func TestImagesTargetCompatibilityOrdering(t *testing.T) {
	p := newBackoffProxy()
	codex := func(id int64) domain.ComboTarget {
		return domain.ComboTarget{ProviderID: id, Enabled: true, UpstreamModel: "gpt-image-2", Provider: &domain.Provider{ID: id, Protocol: domain.ProtocolOpenAICodex}}
	}
	wrong := domain.ComboTarget{ProviderID: 1, Enabled: true, Provider: &domain.Provider{Protocol: domain.ProtocolOpenAI}}
	disabled, archived, empty := codex(4), codex(5), codex(6)
	disabled.Enabled = false
	archived.Provider.Archived = true
	empty.UpstreamModel = ""
	combo := &domain.Combo{ID: 9, Name: "images", Strategy: domain.StrategyRoundRobin,
		Targets: []domain.ComboTarget{wrong, codex(2), codex(3), disabled, archived, empty}}
	for _, id := range []int64{1, 4, 5, 6} {
		p.penalizeProvider(id)
	}
	for i, want := range []int64{2, 3, 2} {
		got := p.imageTargets(context.Background(), combo)
		if len(got) != 2 || got[0].ProviderID != want {
			t.Fatalf("rotation %d: %+v", i, got)
		}
	}
	for _, id := range []int64{1, 4, 5, 6} {
		if p.bo[id].skips != backoffBaseSkips {
			t.Fatalf("incompatible target %d consumed backoff", id)
		}
	}
	combo.Strategy = domain.StrategyFailover
	p.penalizeProvider(2)
	got := p.imageTargets(context.Background(), combo)
	if got[0].ProviderID != 3 || got[1].ProviderID != 2 {
		t.Fatalf("health ordering=%+v", got)
	}
	p.penalizeProvider(3)
	got = p.imageTargets(context.Background(), combo)
	if len(got) != 2 || got[0].ProviderID != 2 || got[1].ProviderID != 3 {
		t.Fatalf("all backed off dropped/reordered=%+v", got)
	}
	combo.Targets = []domain.ComboTarget{codex(2), codex(2), codex(3)}
	p.penalizeProvider(2)
	before := p.bo[2].skips
	p.imageTargets(context.Background(), combo)
	if p.bo[2].skips != before-1 {
		t.Fatal("duplicate provider consumed more than one skip credit")
	}
}

func TestImagesNoCompatibleTarget(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic, domain.ProtocolCursor} {
		p, _, _ := imageTestProxy(t, &domain.Provider{Name: "other", BaseURL: "https://provider.example", Protocol: protocol})
		p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("incompatible target contacted")
			return nil, nil
		})}
		w := imageTestRequest(t, p, "/images/generations", imageBody, "")
		if w.Code != 400 || len(p.bo) != 0 {
			t.Fatalf("protocol %s: %d %s", protocol, w.Code, w.Body.String())
		}
	}
	p, st, combo := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
	combo.Targets[0].Enabled = false
	if err := st.UpdateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	w := imageTestRequest(t, p, "/images/generations", imageBody, "")
	if w.Code != 500 {
		t.Fatalf("no enabled targets=%d %s", w.Code, w.Body.String())
	}
}

func TestImagesFailoverAndMetadataLogs(t *testing.T) {
	first := &domain.Provider{Name: "bad", BaseURL: "https://bad.example", Protocol: domain.ProtocolOpenAICodex}
	second := &domain.Provider{Name: "good", BaseURL: "https://good.example", Protocol: domain.ProtocolOpenAICodex}
	p, st, _ := imageTestProxy(t, first, second)
	p.penalizeProvider(second.ID)
	var logs bytes.Buffer
	p.logger = observability.NewLogger(2, &logs)
	var hosts []string
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if req.URL.Host == "bad.example" {
			return imageTestTransport(400, `{"error":{"message":"private-upstream-error"}}`).RoundTrip(req)
		}
		return imageTestTransport(200, imageUpstreamBody).RoundTrip(req)
	})}
	w := imageTestRequest(t, p, "/images/generations", `{"model":"image-combo","prompt":"private-prompt"}`, "")
	if w.Code != 200 || !reflect.DeepEqual(hosts, []string{"bad.example", "good.example"}) {
		t.Fatalf("status=%d hosts=%v body=%s", w.Code, hosts, w.Body.String())
	}
	if p.bo[first.ID] == nil || p.bo[second.ID] != nil {
		t.Fatalf("backoff=%v", p.bo)
	}
	for _, sentinel := range []string{"private-prompt", "private-upstream-error", imagePNG} {
		if strings.Contains(logs.String(), sentinel) {
			t.Fatalf("payload leaked to logs: %s", logs.String())
		}
	}
	rows := waitForLogs(t, st, 2)
	statuses := map[int]*domain.RequestLog{}
	for _, row := range rows {
		statuses[row.Status] = row
	}
	if len(rows) != 2 || statuses[200] == nil || statuses[400] == nil ||
		statuses[400].ErrMsg != "private-upstream-error" || statuses[200].OutputTokens != 400 || statuses[400].OutputTokens != 0 {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestImagesInvalidOutputFailsOverBeforeCommit(t *testing.T) {
	for _, bad := range []string{`not-json`, `{"data":[]}`, `{"data":[{"b64_json":"broken"}]}`} {
		p, _, _ := imageTestProxy(t,
			&domain.Provider{Name: "bad", BaseURL: "https://bad.example", Protocol: domain.ProtocolOpenAICodex},
			&domain.Provider{Name: "good", BaseURL: "https://good.example", Protocol: domain.ProtocolOpenAICodex})
		var calls int
		p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return imageTestTransport(200, bad).RoundTrip(req)
			}
			return imageTestTransport(200, imageUpstreamBody).RoundTrip(req)
		})}
		w := imageTestRequest(t, p, "/images/generations", imageBody, "")
		if w.Code != 200 || calls != 2 {
			t.Fatalf("invalid output not failed over: %d calls=%d body=%s", w.Code, calls, w.Body.String())
		}
	}
}

func TestImagesInvalidOutputRetainsUsage(t *testing.T) {
	for _, failover := range []bool{false, true} {
		providers := []*domain.Provider{{Name: "bad", BaseURL: "https://bad.example", Protocol: domain.ProtocolOpenAICodex}}
		if failover {
			providers = append(providers, &domain.Provider{Name: "good", BaseURL: "https://good.example", Protocol: domain.ProtocolOpenAICodex})
		}
		p, st, _ := imageTestProxy(t, providers...)
		p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "bad.example" {
				return imageTestTransport(200, `{"data":[],"usage":{"input_tokens":9,"output_tokens":800}}`).RoundTrip(req)
			}
			return imageTestTransport(200, imageUpstreamBody).RoundTrip(req)
		})}
		w := imageTestRequest(t, p, "/images/generations", imageBody, "")
		wantStatus, wantLogs := 502, 1
		if failover {
			wantStatus, wantLogs = 200, 2
		}
		if w.Code != wantStatus {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		for _, row := range waitForLogs(t, st, wantLogs) {
			if row.Provider == "bad" && (row.InputTokens != 9 || row.OutputTokens != 800 || row.UsageEstimated) {
				t.Fatalf("failed attempt usage lost: %+v", row)
			}
			if row.Provider == "good" && (row.InputTokens != 3 || row.OutputTokens != 400) {
				t.Fatalf("previous usage leaked into winning attempt: %+v", row)
			}
		}
	}
}

func TestImagesFinalErrorsPreserveStatus(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500, 503} {
		p, _, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
		p.client = &http.Client{Transport: imageTestTransport(status, `{"error":{"message":"provider refusal"}}`)}
		w := imageTestRequest(t, p, "/images/generations", imageBody, "")
		if w.Code != status || !strings.Contains(w.Body.String(), `"message":"provider refusal"`) {
			t.Fatalf("upstream %d => %d %s", status, w.Code, w.Body.String())
		}
	}
}

func TestImagesOAuthRefreshOnce(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		status               int
		expired, rejectAgain bool
	}{
		{name: "proactive", status: 401, expired: true},
		{name: "reactive401", status: 401},
		{name: "reactive403", status: 403},
		{name: "second401", status: 401, rejectAgain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTokenEndpoint(t, "tok-good")
			expires := time.Now().Add(time.Hour).Unix()
			if tc.expired {
				expires = time.Now().Add(-time.Hour).Unix()
			}
			provider := &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex, AuthMethod: domain.AuthOAuth,
				OAuthCreds: &domain.OAuthCreds{Mode: domain.OAuthAuto, AccessToken: "tok-old", RefreshToken: "rt-old", ExpiresAt: expires, TokenURL: te.server.URL, ClientID: "cid", PKCE: true, AccountID: "account-1"}}
			p, st, _ := imageTestProxy(t, provider)
			var bodies []string
			var identities []string
			p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				bodies = append(bodies, string(body))
				identities = append(identities, req.Header.Get("Session_id"))
				if req.URL.Path != "/images/generations" || req.Header.Get("X-Codex-Image-Turn-Id") != identities[0] {
					t.Errorf("retry endpoint/identity changed: %s %v", req.URL, req.Header)
				}
				if req.Header.Get("Authorization") != "Bearer tok-good" || tc.rejectAgain {
					return imageTestTransport(tc.status, `{"error":{"message":"token rejected"}}`).RoundTrip(req)
				}
				return imageTestTransport(200, imageUpstreamBody).RoundTrip(req)
			})}
			w := imageTestRequest(t, p, "/images/generations", imageBody, "")
			wantStatus, wantCalls := 200, 2
			if tc.expired {
				wantCalls = 1
			}
			if tc.rejectAgain {
				wantStatus = tc.status
			}
			if w.Code != wantStatus || len(bodies) != wantCalls || te.hits.Load() != 1 {
				t.Fatalf("status=%d sends=%d refreshes=%d body=%s", w.Code, len(bodies), te.hits.Load(), w.Body.String())
			}
			if len(bodies) == 2 && (bodies[0] != bodies[1] || identities[0] != identities[1]) {
				t.Fatal("auth retry changed image request")
			}
			stored, err := st.GetProvider(context.Background(), provider.ID)
			if err != nil || stored.OAuthCreds.AccessToken != "tok-good" {
				t.Fatalf("refresh not persisted: %+v %v", stored, err)
			}
		})
	}
}

func TestImagesMissingAndZeroUsageNotEstimated(t *testing.T) {
	for _, usage := range []string{"", `,"usage":{"input_tokens":0,"output_tokens":0}`, `,"usage":{"input_tokens":null,"output_tokens":12}`} {
		p, st, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
		p.client = &http.Client{Transport: imageTestTransport(200, `{"data":[{"b64_json":"`+imagePNG+`"}]`+usage+`}`)}
		w := imageTestRequest(t, p, "/images/generations", imageBody, "")
		rows := waitForLogs(t, st, 1)
		if w.Code != 200 || len(rows) != 1 || rows[0].InputTokens != 0 || rows[0].OutputTokens != 0 || rows[0].UsageEstimated {
			t.Fatalf("usage=%s status=%d rows=%+v", usage, w.Code, rows)
		}
		wantUsage := strings.Contains(usage, `"input_tokens":0`)
		if strings.Contains(w.Body.String(), `"usage"`) != wantUsage {
			t.Fatalf("invented/dropped usage: %s", w.Body.String())
		}
	}
}

func TestImagesCancellationStopsFailover(t *testing.T) {
	for _, cancelBefore := range []bool{true, false} {
		p, _, _ := imageTestProxy(t,
			&domain.Provider{Name: "first", BaseURL: "https://first.example", Protocol: domain.ProtocolOpenAICodex},
			&domain.Provider{Name: "second", BaseURL: "https://second.example", Protocol: domain.ProtocolOpenAICodex})
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			cancel()
			return nil, context.Canceled
		})}
		if cancelBefore {
			cancel()
		}
		mux := http.NewServeMux()
		p.Mount(mux)
		r := httptest.NewRequest(http.MethodPost, "/images/generations", strings.NewReader(imageBody)).WithContext(ctx)
		w := httptest.NewRecorder()
		if cancelBefore {
			p.serveImages(w, r, &reqResult{}, &domain.RequestLog{}, imagesCodec, &domain.Combo{Targets: []domain.ComboTarget{
				{ProviderID: 1, Enabled: true, UpstreamModel: "gpt-image-2", Provider: &domain.Provider{Protocol: domain.ProtocolOpenAICodex}},
			}}, []byte(imageBody))
		} else {
			mux.ServeHTTP(w, r)
			waitForLogs(t, p.store, 1)
		}
		cancel()
		want := 1
		if cancelBefore {
			want = 0
		}
		if calls != want || len(p.bo) != 0 || w.Code != 502 {
			t.Fatalf("cancelBefore=%v calls=%d bo=%v status=%d", cancelBefore, calls, p.bo, w.Code)
		}
	}
}

func TestImagesTransportTimeoutAndResponseLimit(t *testing.T) {
	p, _, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
	p.client = &http.Client{Timeout: 10 * time.Millisecond, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	w := imageTestRequest(t, p, "/images/generations", imageBody, "")
	if w.Code != 502 {
		t.Fatalf("timeout=%d %s", w.Code, w.Body.String())
	}
	p.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(io.LimitReader(zeroReader{}, maxUnaryUpstreamResponseBytes+1))}, nil
	})}
	w = imageTestRequest(t, p, "/images/generations", imageBody, "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("response limit=%d %s", w.Code, w.Body.String())
	}
	p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport-sensitive-error")
	})}
	w = imageTestRequest(t, p, "/images/generations", imageBody, "")
	if w.Code != 502 || strings.Contains(w.Body.String(), "transport-sensitive-error") {
		t.Fatalf("transport error=%d %s", w.Code, w.Body.String())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestImagesRequestBodyLimit(t *testing.T) {
	p, _, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
	p.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized request sent upstream")
		return nil, nil
	})}
	mux := http.NewServeMux()
	p.Mount(mux)
	r := httptest.NewRequest(http.MethodPost, "/images/generations", io.LimitReader(zeroReader{}, maxBodyBytes+1))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("body limit=%d %s", w.Code, w.Body.String())
	}
}

func TestImagesDoNotChangeCodexChatEnvelope(t *testing.T) {
	p, st, combo := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex})
	p.client = &http.Client{Transport: imageTestTransport(200, imageUpstreamBody)}
	if w := imageTestRequest(t, p, "/images/generations", imageBody, ""); w.Code != 200 {
		t.Fatalf("image status=%d", w.Code)
	}
	combo.Targets[0].UpstreamModel = "gpt-5.3-codex-high"
	if err := st.UpdateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	p.streamClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if req.URL.Path != "/responses" || body["model"] != "gpt-5.3-codex" || body["stream"] != true ||
			body["store"] != false || body["instructions"] == "" || body["prompt_cache_key"] != req.Header.Get("Session_id") ||
			req.Header.Get("Session_id") == "" || req.Header.Get("X-Codex-Image-Turn-Id") != "" {
			t.Fatalf("chat request changed: %s %v %+v", req.URL, req.Header, body)
		}
		return imageTestTransport(200, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-chat\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n").RoundTrip(req)
	})}
	w := imageTestRequest(t, p, "/v1/responses", `{"model":"image-combo","input":"hello"}`, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"object":"response"`) {
		t.Fatalf("chat response=%d %s", w.Code, w.Body.String())
	}
}

func TestImagesHARUsesPinnedRecorderAndMasksTerminalPayloads(t *testing.T) {
	p, _, _ := imageTestProxy(t, &domain.Provider{Name: "codex", BaseURL: "https://provider.example", Protocol: domain.ProtocolOpenAICodex, APIKey: "provider-secret"})
	var terminal bytes.Buffer
	p.logger = observability.NewLogger(2, &terminal)
	p.client = &http.Client{Transport: imageTestTransport(200, imageUpstreamBody)}
	recorder := harlog.New("images")
	trace := &TraceInfo{RequestID: "image-request-1", HAR: recorder}
	ctx := WithTraceInfo(observability.WithRequestID(context.Background(), trace.RequestID), trace)
	mux := http.NewServeMux()
	p.Mount(mux)
	r := httptest.NewRequest(http.MethodPost, "/images/generations", strings.NewReader(imageBody)).WithContext(ctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || trace.UpstreamURL != "https://provider.example/images/generations" {
		t.Fatalf("status=%d trace=%+v", w.Code, trace)
	}
	har, err := recorder.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(har, []byte(imagePNG)) || !bytes.Contains(har, []byte("provider-secret")) || !bytes.Contains(har, []byte("page_image-request-1")) {
		t.Fatal("upstream HAR lost image/auth/request correlation")
	}
	for _, sentinel := range []string{imagePNG, "provider-secret", "draw a lake"} {
		if strings.Contains(terminal.String(), sentinel) {
			t.Fatalf("terminal leaked %s", sentinel)
		}
	}
}
