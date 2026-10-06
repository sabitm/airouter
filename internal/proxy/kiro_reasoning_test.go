package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/kirocatalog"
	"airouter/internal/oauth"
	"airouter/internal/observability"
)

func TestKiroCapabilitySelectionBoundary(t *testing.T) {
	schema := `{"models":[{"modelId":"claude-sonnet-4.5","additionalModelRequestFieldsSchema":{"properties":{"output_config":{"properties":{"effort":{"enum":["low","medium","high","xhigh"],"default":"medium"}}},"thinking":{"properties":{"type":{"enum":["disabled","adaptive"],"default":"adaptive"}}}}}},{"modelId":"simple-task","additionalModelRequestFieldsSchema":{"properties":{"reasoning":{"properties":{"effort":{"enum":["low","high"],"default":"low"}}}}}}]}`
	var mu sync.Mutex
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, schema)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), raw...))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(buildKiroFrame("reasoningContentEvent", `{"text":"hidden-trace"}`))
		_, _ = w.Write(buildKiroFrame("assistantResponseEvent", `{"content":"answer"}`))
		_, _ = w.Write(buildKiroFrame("metricsEvent", `{"inputTokens":3,"outputTokens":1}`))
		_, _ = w.Write(buildKiroFrame("messageStopEvent", `{}`))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	base, token := setupKiroProxy(t, upstream.URL, "claude-sonnet-4.5", false)
	cases := []struct {
		name string
		path string
		body string
		want string
	}{
		{name: "openai default", path: "/v1/chat/completions", body: `{"model":"default","messages":[{"role":"user","content":"hi"}]}`, want: `"output_config":{"effort":"medium"}`},
		{name: "responses xhigh", path: "/v1/responses", body: `{"model":"default","input":"hi","reasoning":{"effort":"xhigh"}}`, want: `"effort":"xhigh"`},
		{name: "anthropic disabled", path: "/v1/messages", body: `{"model":"default","max_tokens":20,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"xhigh"},"thinking":{"type":"disabled"}}`, want: `"type":"disabled"`},
		{name: "unsupported", path: "/v1/chat/completions", body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"ultra"}`, want: `"effort":"medium"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(snapshotBodies(&mu, &bodies))
			resp, out := post(t, base+tc.path, token, tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			got := snapshotBodies(&mu, &bodies)
			if len(got) <= before {
				t.Fatal("no chat body")
			}
			raw := got[len(got)-1]
			if !bytes.Contains(raw, []byte(tc.want)) || bytes.Contains(raw, []byte("stored-secret")) {
				t.Fatalf("body missing %s: %s", tc.want, raw)
			}
			if !bytes.Contains(out, []byte("hidden-trace")) || !bytes.Contains(out, []byte("answer")) {
				t.Fatalf("reasoning not preserved: %s", out)
			}
		})
	}
}

func TestKiroSuffixDisableSurvivesLevel(t *testing.T) {
	var body []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			_, _ = io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.5","additionalModelRequestFieldsSchema":{"properties":{"output_config":{"properties":{"effort":{"enum":["low","medium","high","xhigh"],"default":"medium"}}},"thinking":{"properties":{"type":{"enum":["disabled","adaptive"],"default":"adaptive"}}}}}}]}`)
			return
		}
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "kiro", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-4.5(xhigh)", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/messages", key.Token, `{"model":"default","max_tokens":20,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(body, []byte(`"effort":"medium"`)) || !bytes.Contains(body, []byte(`"type":"disabled"`)) {
		t.Fatalf("suffix did not keep disable: %s", body)
	}
}

func TestKiroMissingCatalogOmitsControls(t *testing.T) {
	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			http.Error(w, "no", http.StatusBadGateway)
			return
		}
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	base, token := setupKiroProxy(t, upstream.URL, "claude-sonnet-4.5", false)
	resp, body := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if bytes.Contains(captured, []byte("additionalModelRequestFields")) {
		t.Fatalf("controls written without catalog: %s", captured)
	}
}

func TestKiroCatalogDoesNotUseLiveManagementByDefault(t *testing.T) {
	var saw []string
	client := &http.Client{Transport: kiroRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		saw = append(saw, r.URL.Host+" "+r.Header.Get("X-Amz-Target"))
		if strings.Contains(r.URL.Host, "kiro.dev") {
			t.Fatal("live management host")
		}
		body := `{"models":[{"modelId":"m"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
	})}
	previous := kirocatalog.CurrentDefaultHTTPClient()
	kirocatalog.SetDefaultHTTPClient(client)
	t.Cleanup(func() { kirocatalog.SetDefaultHTTPClient(previous) })
	svc := kirocatalog.New(nil)
	p := &domain.Provider{BaseURL: "https://catalog.example", APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	if _, err := svc.Models(context.Background(), nil, p); err != nil {
		t.Fatal(err)
	}
	if len(saw) != 1 || !strings.Contains(saw[0], "catalog.example") || !strings.Contains(saw[0], "AmazonCodeWhispererService.ListAvailableModels") {
		t.Fatalf("saw=%v", saw)
	}
}

type kiroRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kiroRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func snapshotBodies(mu *sync.Mutex, bodies *[][]byte) [][]byte {
	mu.Lock()
	defer mu.Unlock()
	return append([][]byte(nil), (*bodies)...)
}

func setupKiroProxy(t *testing.T, baseURL, model string, runtime bool) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	var prov *domain.Provider
	if runtime {
		prov = runtimeProvider("kiro", baseURL)
	} else {
		prov = &domain.Provider{Name: "kiro", BaseURL: baseURL, APIKey: "stored-secret", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: model, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

func TestKiroOAuthDiscoveryUsesEffectiveToken(t *testing.T) {
	tokenURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-token","refresh_token":"rt-next","expires_in":3600}`)
	}))
	t.Cleanup(tokenURL.Close)
	var catalogAuth atomic.Value
	var chatAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			catalogAuth.Store(r.Header.Get("Authorization"))
			kiroTestCatalog(w, "claude-sonnet-4.5")
			return
		}
		chatAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "kiro", BaseURL: upstream.URL, Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthManual, AccessToken: "", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
			TokenURL: tokenURL.URL, ClientID: "cid", ProfileArn: "arn:aws:codewhisperer:us-east-1:1:profile/A",
		},
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-4.5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if catalogAuth.Load() != "Bearer fresh-token" || chatAuth.Load() != "Bearer fresh-token" {
		t.Fatalf("catalog=%v chat=%v", catalogAuth.Load(), chatAuth.Load())
	}
}

func TestKiroCatalogAuthRefreshThenChat(t *testing.T) {
	var catalogHits atomic.Int32
	var refreshes atomic.Int32
	tokenURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"rotated","refresh_token":"rt-next","expires_in":3600}`)
	}))
	t.Cleanup(tokenURL.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			n := catalogHits.Add(1)
			if n == 1 {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer rotated" {
				t.Errorf("retry auth=%s", r.Header.Get("Authorization"))
			}
			kiroTestCatalog(w, "claude-sonnet-4.5")
			return
		}
		if r.Header.Get("Authorization") != "Bearer rotated" {
			t.Errorf("chat auth=%s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "kiro", BaseURL: upstream.URL, Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			Mode: domain.OAuthManual, AccessToken: "expired", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			TokenURL: tokenURL.URL, ClientID: "cid", AccountID: "acct",
		},
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-4.5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewWithOAuth(st, nil, oauth.New(st)).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if resp.StatusCode != http.StatusOK || catalogHits.Load() != 2 {
		t.Fatalf("status=%d catalog=%d body=%s", resp.StatusCode, catalogHits.Load(), out)
	}
}

func TestKiroCatalogCancelSkipsChatAndHealth(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var chats atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			kiroTestCatalog(w, "claude-sonnet-4.5")
			return
		}
		chats.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		upstream.Close()
	})
	st := newTestStore(t)
	prov := &domain.Provider{Name: "kiro", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-4.5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"default","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	<-done
	if chats.Load() != 0 {
		t.Fatalf("canceled discovery sent chat")
	}
	if backoffSkips(px, prov.ID) != 0 {
		t.Fatalf("canceled discovery penalized provider skips=%d", backoffSkips(px, prov.ID))
	}
}

func TestKiroCatalogFailureDoesNotHideFailover(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			http.Error(w, "no", http.StatusBadGateway)
			return
		}
		http.Error(w, "chat down", http.StatusBadGateway)
	}))
	t.Cleanup(bad.Close)
	var goodBody []byte
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			kiroTestCatalog(w, "claude-sonnet-4.5")
			return
		}
		goodBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
	}))
	t.Cleanup(good.Close)
	st := newTestStore(t)
	ctx := context.Background()
	first := &domain.Provider{Name: "bad", BaseURL: bad.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	second := &domain.Provider{Name: "good", BaseURL: good.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	if err := st.CreateProvider(ctx, first); err != nil || st.CreateProvider(ctx, second) != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: first.ID, UpstreamModel: "claude-sonnet-4.5", Enabled: true},
		{ProviderID: second.ID, UpstreamModel: "claude-sonnet-4.5", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(goodBody, []byte(`"effort":"high"`)) {
		t.Fatalf("status=%d body=%s upstream=%s", resp.StatusCode, out, goodBody)
	}
}

func TestKiroPreciseAutoOmitsControls(t *testing.T) {
	var body []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			t.Fatal("auto requested catalog")
		}
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
	}))
	t.Cleanup(upstream.Close)
	base, token := setupKiroProxy(t, upstream.URL, "auto", false)
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte("additionalModelRequestFields")) {
		t.Fatalf("status=%d body=%s upstream=%s", resp.StatusCode, out, body)
	}
}

func TestKiroMissingSchemaLogsDiagnostic(t *testing.T) {
	var logs bytes.Buffer
	logger := observability.NewLogger(1, &logs)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			_, _ = io.WriteString(w, `{"models":[{"modelId":"plain"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
	}))
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	prov := &domain.Provider{Name: "kiro", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "plain", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, logger).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","input":"hi","reasoning":{"effort":"high"}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(logs.String(), "schema_missing") {
		t.Fatalf("status=%d body=%s log=%s", resp.StatusCode, out, logs.String())
	}
}

func TestKiroRuntimeCapabilityUsesSameSchema(t *testing.T) {
	var body []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kiroTestIsCatalog(r) {
			_, _ = io.WriteString(w, `{"models":[{"modelId":"runtime-model","additionalModelRequestFieldsSchema":{"properties":{"reasoning":{"properties":{"effort":{"enum":["low","high"],"default":"low"}}}}}}]}`)
			return
		}
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroTextStream())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)
	base, token := setupKiroProxy(t, upstream.URL, "runtime-model", true)
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !bytes.Contains(body, []byte(`"reasoning":{"effort":"high"}`)) || bytes.Contains(body, []byte(`"inferenceConfig"`)) {
		t.Fatalf("runtime body = %s", body)
	}
}
