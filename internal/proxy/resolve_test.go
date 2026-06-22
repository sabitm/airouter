package proxy

import (
	"context"
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
	server *httptest.Server
	hits   atomic.Int64
	status atomic.Int64 // HTTP status to return; 200 by default
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
		_, _ = io.ReadAll(r.Body)
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

// setupCombo wires a store + proxy with one combo whose targets reference the
// given upstreams (in order). Returns the proxy base URL and access token.
func setupCombo(t *testing.T, strategy domain.ComboStrategy, targets []*scriptedUpstream, protocols []domain.Protocol) (string, string) {
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
		combo.Targets = append(combo.Targets, domain.ComboTarget{ProviderID: prov.ID, UpstreamModel: "real-model"})
	}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "client")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	New(st, false, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
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
