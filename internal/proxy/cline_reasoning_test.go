package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/thinking"
)

func clineProvider() *domain.Provider {
	return &domain.Provider{
		Protocol:         domain.ProtocolOpenAI,
		ReasoningDialect: domain.ReasoningCline,
	}
}

func openaiProvider() *domain.Provider {
	return &domain.Provider{
		Protocol:         domain.ProtocolOpenAI,
		ReasoningDialect: domain.ReasoningOpenAI,
	}
}

func decodeJSONMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	return m
}

func finalizeClinePassthrough(t *testing.T, body []byte, model string) map[string]any {
	t.Helper()
	out, err := finalizeRequestBody(body, model, openaiCodec, clineProvider())
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSONMap(t, out)
}

func finalizeClineTranslated(t *testing.T, body []byte, model string) map[string]any {
	t.Helper()
	req, err := openaiCodec.decodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if captured := thinking.Capture(body); captured != nil {
		req.Thinking = thinking.ToIR(captured)
	}
	applyUpstreamModel(req, model)
	encoded, err := openaiCodec.encodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := finalizeEncodedBody(encoded, req, openaiCodec, clineProvider())
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSONMap(t, out)
}

func TestClineRequestPathPassthroughAndTranslated(t *testing.T) {
	const chat = `{"role":"user","content":"hi"}`
	cases := []struct {
		name  string
		body  string
		model string
		check func(t *testing.T, m map[string]any)
	}{
		{
			name:  "body auto",
			body:  `{"model":"combo","reasoning_effort":"auto","messages":[` + chat + `]}`,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				if _, ok := m["reasoning_effort"]; ok {
					t.Fatalf("auto leaked reasoning_effort: %v", m["reasoning_effort"])
				}
			},
		},
		{
			name:  "suffix auto",
			body:  `{"model":"combo","reasoning_effort":"high","messages":[` + chat + `]}`,
			model: "cline-pass/glm-5.2(auto)",
			check: func(t *testing.T, m map[string]any) {
				if m["model"] != "cline-pass/glm-5.2" {
					t.Fatalf("model=%v", m["model"])
				}
				if _, ok := m["reasoning_effort"]; ok {
					t.Fatalf("suffix auto leaked reasoning_effort: %v", m["reasoning_effort"])
				}
			},
		},
		{
			name:  "none",
			body:  `{"model":"combo","reasoning_effort":"none","messages":[` + chat + `]}`,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				r, _ := m["reasoning"].(map[string]any)
				if r == nil || r["enabled"] != false {
					t.Fatalf("none reasoning=%v", r)
				}
				if _, ok := m["reasoning_effort"]; ok {
					t.Fatalf("none leaked reasoning_effort: %v", m["reasoning_effort"])
				}
			},
		},
		{
			name:  "off",
			body:  `{"model":"combo","reasoning_effort":"off","messages":[` + chat + `]}`,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				r, _ := m["reasoning"].(map[string]any)
				if r == nil || r["enabled"] != false {
					t.Fatalf("off reasoning=%v", r)
				}
			},
		},
		{
			name:  "max",
			body:  `{"model":"combo","reasoning_effort":"max","messages":[` + chat + `]}`,
			model: "cline-pass/glm-5.2",
			check: func(t *testing.T, m map[string]any) {
				if m["reasoning_effort"] != "max" {
					t.Fatalf("max effort=%v", m["reasoning_effort"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/passthrough", func(t *testing.T) {
			tc.check(t, finalizeClinePassthrough(t, []byte(tc.body), tc.model))
		})
		t.Run(tc.name+"/translated", func(t *testing.T) {
			tc.check(t, finalizeClineTranslated(t, []byte(tc.body), tc.model))
		})
		t.Run(tc.name+"/stream-body", func(t *testing.T) {
			streamBody := []byte(`{"model":"combo","stream":true,"reasoning_effort":"auto","messages":[{"role":"user","content":"hi"}]}`)
			if tc.name != "body auto" {
				streamBody = []byte(tc.body)
			}
			pass := finalizeClinePassthrough(t, streamBody, tc.model)
			trans := finalizeClineTranslated(t, streamBody, tc.model)
			tc.check(t, pass)
			tc.check(t, trans)
		})
	}
}

func setupClineTranslatedModel(t *testing.T, upstreamModel string, handler http.HandlerFunc) (base, token string) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{
		Name: "cline", BaseURL: upstream.URL, APIKey: "key", Protocol: domain.ProtocolOpenAI,
		ReasoningDialect: domain.ReasoningCline,
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{
		Name: "default", Strategy: domain.StrategyFailover,
		Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: upstreamModel, Enabled: true}},
	}); err != nil {
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

func TestClineFinalUpstreamBytesUnaryAndStream(t *testing.T) {
	upstreamBodies := make(chan []byte, 2)
	base, token := setupClineTranslatedModel(t, "cline-pass/glm-5.2(none)", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBodies <- body
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, openaiSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	})

	for _, stream := range []bool{false, true} {
		body := `{"model":"default","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"default","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
		}
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v status=%d body=%s", stream, resp.StatusCode, out)
		}

		got := decodeJSONMap(t, <-upstreamBodies)
		r, _ := got["reasoning"].(map[string]any)
		if got["model"] != "cline-pass/glm-5.2" || r == nil || r["enabled"] != false {
			t.Fatalf("stream=%v upstream=%v", stream, got)
		}
		if _, ok := got["reasoning_effort"]; ok {
			t.Fatalf("stream=%v effort leaked: %v", stream, got)
		}
	}
}

func TestClineVsGenericFailoverIsolation(t *testing.T) {
	body := []byte(`{"model":"combo","reasoning_effort":"none","enable_thinking":true,"thinking_budget":1024,"messages":[{"role":"user","content":"hi"}]}`)
	clineOut, err := finalizeRequestBody(body, "cline-pass/glm-5.2", openaiCodec, clineProvider())
	if err != nil {
		t.Fatal(err)
	}
	cm := decodeJSONMap(t, clineOut)
	r, _ := cm["reasoning"].(map[string]any)
	if r == nil || r["enabled"] != false {
		t.Fatalf("cline disable = %s", clineOut)
	}
	if _, ok := cm["reasoning_effort"]; ok {
		t.Fatalf("cline leaked effort: %s", clineOut)
	}

	oaiOut, err := finalizeRequestBody(body, "gpt-5", openaiCodec, openaiProvider())
	if err != nil {
		t.Fatal(err)
	}
	om := decodeJSONMap(t, oaiOut)
	if om["reasoning_effort"] != "none" {
		t.Fatalf("openai none = %s", oaiOut)
	}
	if _, ok := om["enable_thinking"]; ok {
		t.Fatalf("qwen/cline field leaked to openai: %s", oaiOut)
	}
	if r, ok := om["reasoning"].(map[string]any); ok {
		if _, ok := r["enabled"]; ok {
			t.Fatalf("cline enabled leaked to openai: %s", oaiOut)
		}
	}
}
