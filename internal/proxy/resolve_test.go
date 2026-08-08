package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
)

// scriptedUpstream is a mock provider whose status and body are controlled per
// test. It counts hits so distribution across targets can be asserted.
type scriptedUpstream struct {
	server   *httptest.Server
	hits     atomic.Int64
	status   atomic.Int64 // HTTP status to return; 200 by default
	lastBody atomic.Value // []byte
}

func newScriptedUpstream(t *testing.T, protocol domain.Protocol) *scriptedUpstream {
	t.Helper()
	su := &scriptedUpstream{}
	su.status.Store(http.StatusOK)
	body := openaiUpstreamBody
	if protocol == domain.ProtocolAnthropic {
		body = anthropicUpstreamBody
	}
	su.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		su.hits.Add(1)
		requestBody, _ := io.ReadAll(r.Body)
		su.lastBody.Store(requestBody)
		st := int(su.status.Load())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		if st >= 200 && st < 300 {
			_, _ = io.WriteString(w, body)
		} else {
			_, _ = io.WriteString(w, `{"error":{"message":"scripted failure"}}`)
		}
	}))
	t.Cleanup(su.server.Close)
	return su
}

func (su *scriptedUpstream) requestBody(t *testing.T) map[string]any {
	t.Helper()
	body, ok := su.lastBody.Load().([]byte)
	if !ok {
		t.Fatal("upstream did not receive a request body")
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode upstream request: %v; body=%s", err, body)
	}
	return out
}

// setupCombo wires a store + proxy with one combo whose targets reference the
// given upstreams (in order). Returns the proxy base URL and access token.
func setupCombo(t *testing.T, strategy domain.ComboStrategy, targets []*scriptedUpstream, protocols []domain.Protocol) (string, string) {
	t.Helper()
	base, token, _ := setupComboProxy(t, strategy, targets, protocols)
	return base, token
}

// setupComboProxy is setupCombo but also returns the Proxy for backoff inspection.
func setupComboProxy(t *testing.T, strategy domain.ComboStrategy, targets []*scriptedUpstream, protocols []domain.Protocol) (string, string, *Proxy) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	combo := &domain.Combo{Name: "default", Strategy: strategy}
	for i, su := range targets {
		prov := &domain.Provider{
			Name:     "p" + string(rune('0'+i)),
			BaseURL:  su.server.URL,
			APIKey:   "up-key",
			Protocol: protocols[i],
		}
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		combo.Targets = append(combo.Targets, domain.ComboTarget{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true})
	}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, px
}

const oaiReq = `{"model":"default","messages":[{"role":"user","content":"hi"}]}`

// TestFailoverAdvancesOnFailure: first target returns 500, second returns 200.
// The client must see a 200 served by the second target.
func TestFailoverAdvancesOnFailure(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	a.status.Store(http.StatusInternalServerError)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{a, b}, []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAI})

	resp, out := post(t, base+"/v1/chat/completions", token, oaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 1 {
		t.Errorf("hits a=%d b=%d, want 1/1 (tried a, failed over to b)", a.hits.Load(), b.hits.Load())
	}
	if got := extractText(t, "/v1/chat/completions", out); got != "hello from openai" {
		t.Errorf("text = %q", got)
	}
}

func TestFailoverDropsReasoningForNonReasoningTarget(t *testing.T) {
	primary := newScriptedUpstream(t, domain.ProtocolOpenAI)
	fallback := newScriptedUpstream(t, domain.ProtocolOpenAI)
	primary.status.Store(http.StatusInternalServerError)

	st := newTestStore(t)
	ctx := context.Background()
	primaryProvider := &domain.Provider{
		Name:             "reasoning-primary",
		BaseURL:          primary.server.URL,
		APIKey:           "up-key",
		Protocol:         domain.ProtocolOpenAI,
		ReasoningDialect: domain.ReasoningOpenAI,
	}
	fallbackProvider := &domain.Provider{
		Name:             "non-reasoning-fallback",
		BaseURL:          fallback.server.URL,
		APIKey:           "up-key",
		Protocol:         domain.ProtocolOpenAI,
		ReasoningDialect: domain.ReasoningOpenAI,
	}
	if err := st.CreateProvider(ctx, primaryProvider); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, fallbackProvider); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: primaryProvider.ID, UpstreamModel: "gpt-5(high)", Enabled: true},
		{ProviderID: fallbackProvider.ID, UpstreamModel: "gpt-4o", Enabled: true},
	}}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	body := `{"model":"default","reasoning_effort":"low","reasoning":{"summary":"detailed"},"messages":[{"role":"user","content":"hi"}]}`
	resp, out := post(t, server.URL+"/v1/chat/completions", key.Token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if primary.hits.Load() != 1 || fallback.hits.Load() != 1 {
		t.Fatalf("hits primary=%d fallback=%d, want 1/1", primary.hits.Load(), fallback.hits.Load())
	}
	if got := extractText(t, "/v1/chat/completions", out); got != "hello from openai" {
		t.Fatalf("fallback response text = %q", got)
	}

	primaryBody := primary.requestBody(t)
	if primaryBody["model"] != "gpt-5" || primaryBody["reasoning_effort"] != "high" {
		t.Fatalf("primary reasoning was not finalized from suffix: %v", primaryBody)
	}
	if reasoning, ok := primaryBody["reasoning"].(map[string]any); !ok || reasoning["summary"] != "detailed" {
		t.Fatalf("primary reasoning summary was not preserved: %v", primaryBody)
	}

	fallbackBody := fallback.requestBody(t)
	if fallbackBody["model"] != "gpt-4o" {
		t.Fatalf("fallback model = %v, want gpt-4o", fallbackBody["model"])
	}
	for _, field := range []string{"reasoning_effort", "thinking", "enable_thinking", "thinking_budget"} {
		if _, ok := fallbackBody[field]; ok {
			t.Fatalf("fallback retained unsupported %s: %v", field, fallbackBody)
		}
	}
	if reasoning, ok := fallbackBody["reasoning"].(map[string]any); !ok || reasoning["summary"] != "detailed" {
		t.Fatalf("fallback reasoning summary was not preserved: %v", fallbackBody)
	}
}

// TestDisabledTargetSkipped: a combo whose first (healthy) target is disabled
// must route straight to the second target; the disabled one is never contacted.
func TestDisabledTargetSkipped(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	st := newTestStore(t)
	ctx := context.Background()
	pa := &domain.Provider{Name: "pa", BaseURL: a.server.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	pb := &domain.Provider{Name: "pb", BaseURL: b.server.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, pa); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, pb); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: pa.ID, UpstreamModel: "real-model", Enabled: false},
		{ProviderID: pb.ID, UpstreamModel: "real-model", Enabled: true},
	}}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, oaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if a.hits.Load() != 0 {
		t.Errorf("disabled target hits = %d, want 0", a.hits.Load())
	}
	if b.hits.Load() != 1 {
		t.Errorf("enabled target hits = %d, want 1", b.hits.Load())
	}
}

// TestArchivedProviderSkipped: an enabled target whose provider is archived must
// not be contacted; resolution uses the other enabled target.
func TestArchivedProviderSkipped(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	st := newTestStore(t)
	ctx := context.Background()
	pa := &domain.Provider{Name: "pa", BaseURL: a.server.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	pb := &domain.Provider{Name: "pb", BaseURL: b.server.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, pa); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, pb); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProviderArchived(ctx, pa.ID, true); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: pa.ID, UpstreamModel: "real-model", Enabled: true},
		{ProviderID: pb.ID, UpstreamModel: "real-model", Enabled: true},
	}}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, oaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if a.hits.Load() != 0 {
		t.Errorf("archived provider hits = %d, want 0", a.hits.Load())
	}
	if b.hits.Load() != 1 {
		t.Errorf("active target hits = %d, want 1", b.hits.Load())
	}
}

// TestFailoverAllFail: every target fails; the client gets the last failure.
func TestFailoverAllFail(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	a.status.Store(http.StatusInternalServerError)
	b.status.Store(http.StatusBadGateway)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{a, b}, []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAI})

	resp, _ := post(t, base+"/v1/chat/completions", token, oaiReq)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (last target's status)", resp.StatusCode)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 1 {
		t.Errorf("hits a=%d b=%d, want both tried once", a.hits.Load(), b.hits.Load())
	}
}

// TestRoundRobinSpreadsLoad: two healthy targets, four requests; each target
// should serve two. Rotation starts at index 0 since the counter is fresh.
func TestRoundRobinSpreadsLoad(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	base, token := setupCombo(t, domain.StrategyRoundRobin,
		[]*scriptedUpstream{a, b}, []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAI})

	for i := 0; i < 4; i++ {
		resp, out := post(t, base+"/v1/chat/completions", token, oaiReq)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d: status %d, body %s", i, resp.StatusCode, out)
		}
	}
	if a.hits.Load() != 2 || b.hits.Load() != 2 {
		t.Errorf("hits a=%d b=%d, want 2/2", a.hits.Load(), b.hits.Load())
	}
}

// TestRoundRobinStillFailsOver: target picked first is down; the request must
// still succeed on the other target.
func TestRoundRobinStillFailsOver(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	a.status.Store(http.StatusInternalServerError)
	base, token := setupCombo(t, domain.StrategyRoundRobin,
		[]*scriptedUpstream{a, b}, []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAI})

	resp, out := post(t, base+"/v1/chat/completions", token, oaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	if b.hits.Load() != 1 {
		t.Errorf("healthy target hits = %d, want 1", b.hits.Load())
	}
}

// TestFailoverBacksOffRepeatedFailure: a failover combo whose first target is
// persistently down. After the first request pays the failover cost, subsequent
// requests must skip the dead target and hit the healthy one directly, proving
// the backoff defers it rather than retrying it first every time.
func TestFailoverBacksOffRepeatedFailure(t *testing.T) {
	a := newScriptedUpstream(t, domain.ProtocolOpenAI)
	b := newScriptedUpstream(t, domain.ProtocolOpenAI)
	a.status.Store(http.StatusInternalServerError)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{a, b}, []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolOpenAI})

	for i := 0; i < 3; i++ {
		resp, out := post(t, base+"/v1/chat/completions", token, oaiReq)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d: status %d, body %s", i, resp.StatusCode, out)
		}
	}
	// The dead target is tried only on the first request; the backoff window (>=1s)
	// keeps it deferred for the next two, which reach the healthy target directly.
	if a.hits.Load() != 1 {
		t.Errorf("dead target hits = %d, want 1 (deferred after first failure)", a.hits.Load())
	}
	if b.hits.Load() != 3 {
		t.Errorf("healthy target hits = %d, want 3", b.hits.Load())
	}
}

// TestMixedProtocolCombo: an OpenAI ingress request fails over from a dead
// OpenAI target to a healthy Anthropic target, translating on the second leg.
func TestMixedProtocolCombo(t *testing.T) {
	oai := newScriptedUpstream(t, domain.ProtocolOpenAI)
	anth := newScriptedUpstream(t, domain.ProtocolAnthropic)
	oai.status.Store(http.StatusInternalServerError)
	base, token := setupCombo(t, domain.StrategyFailover,
		[]*scriptedUpstream{oai, anth}, []domain.Protocol{domain.ProtocolOpenAI, domain.ProtocolAnthropic})

	resp, out := post(t, base+"/v1/chat/completions", token, oaiReq)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	// Response is in the OpenAI ingress format but the text came from the
	// Anthropic upstream, proving the failover leg translated.
	if got := extractText(t, "/v1/chat/completions", out); got != "hello from anthropic" {
		t.Errorf("text = %q, want anthropic upstream text via translation", got)
	}
}
