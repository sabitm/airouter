package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/kirocatalog"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/thinking"
)

func TestKiroReasoningSelectionMatrix(t *testing.T) {
	cases := []struct{ name, path, body, effort, toggle string }{
		{"chat default", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}]}`, "medium", "adaptive"},
		{"chat explicit", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`, "xhigh", "adaptive"},
		{"chat unsupported", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"ultra"}`, "medium", "adaptive"},
		{"chat none", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`, "medium", "disabled"},
		{"chat disabled", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh","thinking":{"type":"disabled"}}`, "medium", "disabled"},
		{"responses disabled", "/v1/responses", `{"model":"default","input":"hi","reasoning":{"effort":"xhigh"},"thinking":{"type":"disabled"}}`, "medium", "disabled"},
		{"anthropic disabled", "/v1/messages", `{"model":"default","max_tokens":20,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"xhigh"},"thinking":{"type":"disabled"}}`, "medium", "disabled"},
	}
	for _, runtime := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(tc.name+map[bool]string{false: " legacy", true: " runtime"}[runtime]+map[bool]string{false: " unary", true: " stream"}[stream], func(t *testing.T) {
					captured := make(chan []byte, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if kiroTestIsCatalog(r) {
							kiroTestCatalog(w, "m")
							return
						}
						raw, _ := io.ReadAll(r.Body)
						captured <- raw
						w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
						_, _ = w.Write(buildKiroFrame("reasoningContentEvent", `{"text":"trace"}`))
						_, _ = w.Write(buildKiroFrame("assistantResponseEvent", `{"content":"answer"}`))
						_, _ = w.Write(buildKiroFrame("metadataEvent", `{"stopReason":"END_TURN","tokenUsage":{"uncachedInputTokens":3,"outputTokens":1}}`))
					}))
					defer upstream.Close()
					base, key := setupKiroProxy(t, upstream.URL, "m", runtime)
					var ingress map[string]any
					if err := json.Unmarshal([]byte(tc.body), &ingress); err != nil {
						t.Fatal(err)
					}
					ingress["stream"] = stream
					body, _ := json.Marshal(ingress)
					resp, output := post(t, base+tc.path, key, string(body))
					if resp.StatusCode != http.StatusOK || !bytes.Contains(output, []byte("trace")) || !bytes.Contains(output, []byte("answer")) {
						t.Fatalf("status=%d output=%s", resp.StatusCode, output)
					}
					var wire struct {
						Fields struct {
							Output struct {
								Effort string `json:"effort"`
							} `json:"output_config"`
							Thinking struct {
								Type string `json:"type"`
							} `json:"thinking"`
						} `json:"additionalModelRequestFields"`
					}
					if err := json.Unmarshal(<-captured, &wire); err != nil {
						t.Fatal(err)
					}
					if wire.Fields.Output.Effort != tc.effort || wire.Fields.Thinking.Type != tc.toggle {
						t.Fatalf("effort=%s toggle=%s", wire.Fields.Output.Effort, wire.Fields.Thinking.Type)
					}
				})
			}
		}
	}
}

func TestKiroBudgetMetadataSurvivesSuffix(t *testing.T) {
	for _, body := range []string{
		`{"thinking":{"type":"enabled","budget_tokens":4096}}`,
		`{"output_config":{"effort":"high"},"thinking":{"type":"enabled","budget_tokens":4096}}`,
		`{"reasoning_effort":"high","enable_thinking":true,"thinking_budget":4096}`,
	} {
		for _, suffix := range []string{"m(high)", "m(none)", "m(auto)"} {
			req := &ir.Request{Thinking: thinking.ToIR(thinking.Capture([]byte(body)))}
			applyUpstreamModel(req, suffix)
			_, _, _, budget := kiroIntent(req.Thinking)
			if !budget {
				t.Errorf("suffix=%s discarded budget metadata: %+v", suffix, req.Thinking)
			}
		}
	}
}

func TestKiroCatalogRejectsAccountReplacement(t *testing.T) {
	st := newTestStore(t)
	old := &domain.Provider{Name: "kiro", BaseURL: "https://catalog.example", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{AccessToken: "old-token", RefreshToken: "old-refresh", AccountID: "old-account", ProfileArn: "arn:old-profile", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	if err := st.CreateProvider(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	snapshot := kiroCatalogProvider(old)
	current := kiroCatalogProvider(old)
	current.OAuthCreds = &domain.OAuthCreds{AccessToken: "new-token", RefreshToken: "new-refresh", AccountID: "new-account", ProfileArn: "arn:new-profile", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if err := st.UpdateProvider(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	catalog := kirocatalog.New(&kirocatalog.Client{HTTP: &http.Client{Transport: kiroRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("superseded account attempted catalog HTTP")
		return nil, context.Canceled
	})}})
	px := NewWithDeps(st, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, catalog)
	body := []byte(`{"conversationState":{}}`)
	got, err := px.applyKiroCapability(context.Background(), snapshot, &ir.Request{Model: "m"}, body)
	if err != nil || string(got) != string(body) || snapshot.APIKey != "" || snapshot.OAuthCreds.ProfileArn != "arn:old-profile" {
		t.Fatalf("snapshot changed or discovery failed chat: err=%v", err)
	}
}

func TestKiroBudgetDiagnosticWithoutBudgetWire(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	catalog := kirocatalog.New(&kirocatalog.Client{HTTP: &http.Client{Transport: kiroRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"m","additionalModelRequestFieldsSchema":{"properties":{"reasoning":{"properties":{"effort":{"enum":["low","high"],"default":"low"}}}}}}]}`))}, nil
	})}})
	px := NewWithDeps(newTestStore(t), logger, nil, catalog)
	provider := &domain.Provider{BaseURL: "https://catalog.example", APIKey: "key", Protocol: domain.ProtocolKiro, AuthMethod: domain.AuthAPIKey}
	req := &ir.Request{Thinking: thinking.ToIR(thinking.Capture([]byte(`{"thinking":{"type":"enabled","budget_tokens":4096}}`)))}
	applyUpstreamModel(req, "m(high)")
	body, err := px.applyKiroCapability(context.Background(), provider, req, []byte(`{"conversationState":{}}`))
	if err != nil || bytes.Contains(body, []byte("budget_tokens")) || !bytes.Contains(body, []byte(`"effort":"high"`)) || !strings.Contains(logs.String(), "kiro_budget_not_applied") {
		t.Fatalf("body=%s err=%v logs=%s", body, err, logs.String())
	}
}
