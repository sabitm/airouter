package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/qoder"
)

// End-to-end coverage for catalog-aware Qoder reasoning controls. The harness
// reuses the queue harness's scripted upstream so the prepared wire body can be
// decoded and its native controls asserted per ingress format.

var qoderReasoningUserSeq atomic.Int64

// qoderReasoningHarness wires a Qoder provider and one combo per named target
// against a scripted upstream, seeding the live catalog with the given
// per-model-key configs. Combo names are usable as request models.
func qoderReasoningHarness(t *testing.T, u *qoderUpstream, combos map[string]string, catalog map[string]string) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "/sse/agent_chat_generation"):
			resp := u.recordChat(r, body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(resp.status)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = io.WriteString(w, resp.sse)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case strings.Contains(r.URL.Path, "/ask/queue/status"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(u.pollStatus)
			_, _ = io.WriteString(w, u.pollBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(up.Close)

	prov := &domain.Provider{
		Name:       "q",
		BaseURL:    up.URL + "/algo",
		Protocol:   domain.ProtocolQoder,
		AuthMethod: domain.AuthAPIKey,
		APIKey:     "dt-token",
		OAuthCreds: &domain.OAuthCreds{
			UserID:      fmt.Sprintf("reasoning-user-%d", qoderReasoningUserSeq.Add(1)),
			AccessToken: "dt-token",
			MachineID:   "machine-1",
		},
	}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(combos))
	for name := range combos {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		combo := &domain.Combo{
			Name:     name,
			Strategy: domain.StrategyFailover,
			Targets:  []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: combos[name], Enabled: true}},
		}
		if err := st.CreateCombo(ctx, combo); err != nil {
			t.Fatal(err)
		}
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}

	stored, err := st.GetProvider(ctx, prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	configs := make(map[string]json.RawMessage, len(catalog))
	keys := make([]string, 0, len(catalog))
	for k, v := range catalog {
		configs[k] = json.RawMessage(v)
		keys = append(keys, k)
	}
	qoder.SeedCatalogForTest(stored, configs, keys)
	t.Cleanup(qoder.ClearCatalogForTest)

	mux := http.NewServeMux()
	p := New(st, nil)
	p.qoderQueuePolicy = qoderQueuePolicy{
		maxWait:      300 * time.Millisecond,
		minDelay:     time.Millisecond,
		maxDelay:     2 * time.Millisecond,
		defaultDelay: time.Millisecond,
		statusFloor:  time.Millisecond,
		pollTimeout:  200 * time.Millisecond,
	}
	p.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

// qoderChatBodyAt returns the raw recorded wire body at idx.
func qoderChatBodyAt(t *testing.T, u *qoderUpstream, idx int) []byte {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if idx >= len(u.chats) {
		t.Fatalf("chat %d not recorded", idx)
	}
	return []byte(u.chats[idx].body)
}

// qoderReasoningWire decodes the recorded chat at idx and returns the native
// reasoning controls plus the record identity.
func qoderReasoningWire(t *testing.T, u *qoderUpstream, idx int) (params map[string]json.RawMessage, top, mirror bool, setID string) {
	t.Helper()
	plain, err := qoder.DecodeWireBody(qoderChatBodyAt(t, u, idx))
	if err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	var m struct {
		Parameters  map[string]json.RawMessage `json:"parameters"`
		ModelConfig struct {
			IsReasoning bool `json:"is_reasoning"`
		} `json:"model_config"`
		ChatContext struct {
			Extra struct {
				ModelConfig struct {
					IsReasoning bool `json:"is_reasoning"`
				} `json:"modelConfig"`
			} `json:"extra"`
		} `json:"chat_context"`
		RequestSetID string `json:"request_set_id"`
	}
	if err := json.Unmarshal(plain, &m); err != nil {
		t.Fatalf("decode plaintext: %v", err)
	}
	return m.Parameters, m.ModelConfig.IsReasoning, m.ChatContext.Extra.ModelConfig.IsReasoning, m.RequestSetID
}

// qoderReasoningCatalog is the verified qfmodel shape: low/medium/xhigh with
// medium as the advertised default.
const qoderReasoningCatalog = `{
	"key":"qfmodel",
	"is_reasoning":true,
	"max_output_tokens":32768,
	"thinking_config":{
		"disabled":{},
		"enabled":{
			"efforts":{"xhigh":{},"low":{},"medium":{"is_default":true}},
			"is_default":true
		},
		"is_default":true
	}
}`

func qoderChatBody(model, effort string) string {
	if effort == "" {
		return fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model)
	}
	return fmt.Sprintf(`{"model":%q,"stream":true,"reasoning_effort":%q,"messages":[{"role":"user","content":"hi"}]}`, model, effort)
}

// TestQoderReasoningSuffixEffortOpenAIStream covers the primary native path:
// a target suffix effort above the advertised enum remaps to the highest
// advertised level and the flags stay consistent.
func TestQoderReasoningSuffixEffortOpenAIStream(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(max)"},
		map[string]string{"qfmodel": qoderReasoningCatalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "hello" || !finished {
		t.Fatalf("text = %q finished = %v", text, finished)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("reasoning_effort = %s, want xhigh", params["reasoning_effort"])
	}
	if string(params["enable_thinking"]) != "true" {
		t.Fatalf("enable_thinking = %s", params["enable_thinking"])
	}
	if string(params["max_tokens"]) != "32768" {
		t.Fatalf("max_tokens = %s", params["max_tokens"])
	}
	if !top || !mirror {
		t.Fatalf("reasoning flags: model_config=%v mirror=%v", top, mirror)
	}
	// Other raw catalog fields keep their exact values.
	var m struct {
		ModelConfig struct {
			MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
			Key             string          `json:"key"`
		} `json:"model_config"`
	}
	plain, err := qoder.DecodeWireBody(qoderChatBodyAt(t, u, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plain, &m); err != nil {
		t.Fatal(err)
	}
	if string(m.ModelConfig.MaxOutputTokens) != "32768" || m.ModelConfig.Key != "qfmodel" {
		t.Fatalf("catalog fields not preserved: %+v", m.ModelConfig)
	}
}

// The same effective selection must be produced from every ingress format's
// reasoning intent.
func TestQoderReasoningBodyEffortAcrossIngresses(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"},
		map[string]string{"qfmodel": qoderReasoningCatalog})

	openaiResp, openaiBody := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)
	if openaiResp.StatusCode != http.StatusOK {
		t.Fatalf("openai status = %d, body = %s", openaiResp.StatusCode, openaiBody)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("openai reasoning_effort = %s", params["reasoning_effort"])
	}
	if string(params["enable_thinking"]) != "true" || !top || !mirror {
		t.Fatalf("openai controls: %s top=%v mirror=%v", params["enable_thinking"], top, mirror)
	}

	// Equal output limits and content, so the identities must match once the
	// effective reasoning controls are equal.
	responsesResp, responsesBody := postStream(t, base+"/v1/responses", token,
		`{"model":"default","stream":true,"max_output_tokens":1024,"reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if responsesResp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d, body = %s", responsesResp.StatusCode, responsesBody)
	}
	params, _, _, setResponses := qoderReasoningWire(t, u, 1)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("responses reasoning_effort = %s", params["reasoning_effort"])
	}

	anthropicResp, anthropicBody := postStream(t, base+"/v1/messages", token,
		`{"model":"default","stream":true,"max_tokens":1024,"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`)
	if anthropicResp.StatusCode != http.StatusOK {
		t.Fatalf("anthropic status = %d, body = %s", anthropicResp.StatusCode, anthropicBody)
	}
	text, finished := collectStreamText(t, "/v1/messages", anthropicBody)
	if text != "hello" || !finished {
		t.Fatalf("anthropic text = %q finished = %v", text, finished)
	}
	params, _, _, setAnthropic := qoderReasoningWire(t, u, 2)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("anthropic reasoning_effort = %s", params["reasoning_effort"])
	}
	if setResponses != setAnthropic {
		t.Fatalf("equal effective effort produced different set ids: %q vs %q", setResponses, setAnthropic)
	}
}

// The suffix must win over body intensity: body medium would stay medium, the
// suffix max remaps to xhigh.
func TestQoderReasoningSuffixOverridesBody(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(max)"},
		map[string]string{"qfmodel": qoderReasoningCatalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", "medium"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, _, _, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("reasoning_effort = %s, want suffix max to win as xhigh", params["reasoning_effort"])
	}
}

func TestQoderReasoningDisable(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(none)"},
		map[string]string{"qfmodel": qoderReasoningCatalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" {
		t.Fatalf("disable controls: %s %s", params["reasoning_effort"], params["enable_thinking"])
	}
	if top || mirror {
		t.Fatalf("disable flags: model_config=%v mirror=%v", top, mirror)
	}
	// The Anthropic disable shape lands on the same wire state.
	resp, body = postStream(t, base+"/v1/messages", token,
		`{"model":"default","stream":true,"max_tokens":1024,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anthropic disable status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ = qoderReasoningWire(t, u, 1)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
		t.Fatalf("anthropic disable controls: %s %s top=%v mirror=%v", params["reasoning_effort"], params["enable_thinking"], top, mirror)
	}
}

// Explicit controls the catalog cannot honor must fail before inference with a
// clear client error.
func TestQoderReasoningUnsupportedControlsError(t *testing.T) {
	cases := []struct {
		name    string
		combos  map[string]string
		catalog string
		request string
		wantErr string
	}{
		{
			name:    "disable without disable support",
			combos:  map[string]string{"default": "qoder/qfmodel(none)"},
			catalog: `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"low":{},"medium":{"is_default":true}}}}}`,
			request: qoderChatBody("default", ""),
			wantErr: "disabling",
		},
		{
			name:    "numeric budget suffix",
			combos:  map[string]string{"default": "qoder/qfmodel(8192)"},
			catalog: qoderReasoningCatalog,
			request: qoderChatBody("default", ""),
			wantErr: "budget",
		},
		{
			name:    "anthropic numeric budget body",
			combos:  map[string]string{"default": "qoder/qfmodel"},
			catalog: qoderReasoningCatalog,
			request: `{"model":"default","stream":true,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`,
			wantErr: "budget",
		},
		{
			name:    "unknown effort string",
			combos:  map[string]string{"default": "qoder/qfmodel"},
			catalog: qoderReasoningCatalog,
			request: qoderChatBody("default", "super"),
			wantErr: "unknown reasoning effort",
		},
		{
			name:    "effort on toggle-only catalog",
			combos:  map[string]string{"default": "qoder/qfmodel(high)"},
			catalog: `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{},"enabled":{}}}`,
			request: qoderChatBody("default", ""),
			wantErr: "does not advertise",
		},
		{
			name:    "effort on nonreasoning model",
			combos:  map[string]string{"default": "qoder/qfmodel(high)"},
			catalog: `{"key":"qfmodel","is_reasoning":false}`,
			request: qoderChatBody("default", ""),
			wantErr: "reasoning model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
			base, token := qoderReasoningHarness(t, u, tc.combos, map[string]string{"qfmodel": tc.catalog})
			resp, body := postStream(t, base+"/v1/chat/completions", token, tc.request)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, tc.wantErr) {
				t.Fatalf("error body %q does not contain %q", body, tc.wantErr)
			}
			if u.chatCount() != 0 {
				t.Fatal("rejected request reached the upstream")
			}
			if !strings.Contains(body, "invalid_request_error") {
				t.Fatalf("error envelope lost its type: %s", body)
			}
		})
	}
}

// Omitted and auto intent take the advertised default; equal effective
// settings share one record identity.
func TestQoderReasoningDefaultAndIdentity(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{
		"default": "qoder/qfmodel",
		"auto":    "qoder/qfmodel(auto)",
		"med":     "qoder/qfmodel(medium)",
		"xh":      "qoder/qfmodel(xhigh)",
	}, map[string]string{"qfmodel": qoderReasoningCatalog})

	for _, model := range []string{"default", "auto", "med"} {
		resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody(model, ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", model, resp.StatusCode, body)
		}
	}
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		params, top, mirror, setID := qoderReasoningWire(t, u, i)
		if string(params["reasoning_effort"]) != `"medium"` || string(params["enable_thinking"]) != "true" {
			t.Fatalf("chat %d controls: %s %s", i, params["reasoning_effort"], params["enable_thinking"])
		}
		if !top || !mirror {
			t.Fatalf("chat %d flags: top=%v mirror=%v", i, top, mirror)
		}
		ids = append(ids, setID)
	}
	if ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("omitted/auto/default must share one identity: %v", ids)
	}
	resp, _ := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("xh", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	_, _, _, setXhigh := qoderReasoningWire(t, u, 3)
	if setXhigh == ids[0] {
		t.Fatal("xhigh must differ from the medium identities")
	}
	// Body medium equals the combo-default identity too.
	resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", "medium"))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	_, _, _, setBodyMedium := qoderReasoningWire(t, u, 4)
	if setBodyMedium != ids[0] {
		t.Fatal("body medium must share the default identity")
	}
}

// A disabled catalog default applies to omitted intent and equals explicit
// disable on the wire and in identity.
func TestQoderReasoningDisabledDefault(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{
		"default": "qoder/qfmodel",
		"off":     "qoder/qfmodel(none)",
	}, map[string]string{"qfmodel": catalog})

	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, defID := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
		t.Fatalf("disabled default not applied: %s %s top=%v mirror=%v", params["reasoning_effort"], params["enable_thinking"], top, mirror)
	}
	resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("off", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	_, _, _, offID := qoderReasoningWire(t, u, 1)
	if defID != offID {
		t.Fatalf("disabled default and explicit none must share identity: %q vs %q", defID, offID)
	}
}

// A supported max stays max; a toggle-only catalog honors the disable toggle
// and stays silent for omitted intent.
func TestQoderReasoningToggleAndMaxEdgeCatalogs(t *testing.T) {
	t.Run("supported max unchanged", func(t *testing.T) {
		catalog := `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"enabled":{"efforts":{"low":{},"medium":{},"xhigh":{},"max":{}},"default_effort":"medium"}}}`
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(max)"}, map[string]string{"qfmodel": catalog})
		resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		params, _, _, _ := qoderReasoningWire(t, u, 0)
		if string(params["reasoning_effort"]) != `"max"` {
			t.Fatalf("reasoning_effort = %s, want max preserved", params["reasoning_effort"])
		}
	})
	t.Run("toggle-only omitted stays silent", func(t *testing.T) {
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"},
			map[string]string{"qfmodel": `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{},"enabled":{}}}`})
		resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		params, top, mirror, _ := qoderReasoningWire(t, u, 0)
		if _, ok := params["reasoning_effort"]; ok {
			t.Fatalf("toggle-only catalog invented reasoning_effort: %s", params["reasoning_effort"])
		}
		if _, ok := params["enable_thinking"]; ok {
			t.Fatalf("toggle-only catalog invented enable_thinking: %s", params["enable_thinking"])
		}
		if !top || !mirror {
			t.Fatalf("catalog is_reasoning flag lost: top=%v mirror=%v", top, mirror)
		}
	})
	t.Run("toggle-only disable honored", func(t *testing.T) {
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(none)"},
			map[string]string{"qfmodel": `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{},"enabled":{}}}`})
		resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		params, top, mirror, _ := qoderReasoningWire(t, u, 0)
		if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
			t.Fatalf("toggle-only disable: %s %s top=%v mirror=%v", params["reasoning_effort"], params["enable_thinking"], top, mirror)
		}
	})
}

// Queue recovery must replay the identical prepared bytes and identity with
// fresh signing, even with reasoning controls selected from the catalog.
func TestQoderReasoningQueueReplayStableIdentity(t *testing.T) {
	u := &qoderUpstream{
		chatResponses: []qoderResponse{
			{status: 200, sse: qoderRefusalSSE(t, 5)},
			{status: 200, sse: qoderSuccessSSE},
		},
		pollStatus: 200,
		pollBody:   `{"isQueued":false,"queueType":"p3"}`,
	}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(max)"},
		map[string]string{"qfmodel": qoderReasoningCatalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "hello" || !finished {
		t.Fatalf("text = %q finished = %v", text, finished)
	}
	if u.chatCount() != 2 {
		t.Fatalf("chat sends = %d, want 2", u.chatCount())
	}
	first := qoderChatBodyAt(t, u, 0)
	second := qoderChatBodyAt(t, u, 1)
	if len(first) == 0 || string(first) != string(second) {
		t.Fatal("queue replay did not reuse the exact prepared body")
	}
	if u.chats[0].auth == "" || u.chats[0].auth == u.chats[1].auth {
		t.Fatal("queue replay reused the COSY signature")
	}
	params, top, mirror, setID := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("replayed reasoning_effort = %s", params["reasoning_effort"])
	}
	if !top || !mirror {
		t.Fatalf("replayed flags: top=%v mirror=%v", top, mirror)
	}
	plain, err := qoder.DecodeWireBody(first)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Set string `json:"request_set_id"`
	}
	if err := json.Unmarshal(plain, &f); err != nil {
		t.Fatal(err)
	}
	if f.Set == "" || f.Set != setID {
		t.Fatalf("identity fields inconsistent: %q vs %q", f.Set, setID)
	}
}

// Probe A: an explicitly nonreasoning catalog entry is never promoted. Stale
// enabled metadata is suppressed for omitted and auto intent, and explicit
// positive effort or enabling gets a pre-inference client error.
func TestQoderReasoningNonReasoningCatalogStaleMetadata(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":false,"thinking_config":{"enabled":{"efforts":{"high":{"is_default":true}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{
		"default": "qoder/qfmodel",
		"high":    "qoder/qfmodel(high)",
		"auto":    "qoder/qfmodel(auto)",
	}, map[string]string{"qfmodel": catalog})

	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("high", ""))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("suffix high status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "reasoning model") {
		t.Fatalf("error body %q does not name the nonreasoning veto", body)
	}
	if u.chatCount() != 0 {
		t.Fatal("rejected request reached the upstream")
	}

	resp, body = postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"enable_thinking":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("explicit enable status = %d, body = %s", resp.StatusCode, body)
	}
	if u.chatCount() != 0 {
		t.Fatal("rejected enable reached the upstream")
	}

	resp, body = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("omitted status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("stale default invented reasoning_effort: %v", params)
	}
	if _, ok := params["enable_thinking"]; ok {
		t.Fatalf("stale default invented enable_thinking: %v", params)
	}
	if top || mirror {
		t.Fatalf("nonreasoning flags were promoted: top=%v mirror=%v", top, mirror)
	}
	resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("auto", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	params, top, mirror, _ = qoderReasoningWire(t, u, 1)
	if len(params) != 1 || string(params["max_tokens"]) == "" {
		t.Fatalf("auto intent invented controls: %v", params)
	}
	if top || mirror {
		t.Fatalf("auto intent promoted flags: top=%v mirror=%v", top, mirror)
	}
}

// Probe B: an explicit enable on a toggle-only catalog with a disabled default
// enables the toggle alone; plain omitted or auto intent still takes the
// disabled default.
func TestQoderReasoningExplicitEnableToggleOnly(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{"is_default":true},"enabled":{}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{
		"default": "qoder/qfmodel",
		"on":      "qoder/qfmodel",
	}, map[string]string{"qfmodel": catalog})

	resp, body := postStream(t, base+"/v1/messages", token,
		`{"model":"on","stream":true,"max_tokens":1024,"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explicit enable status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, onID := qoderReasoningWire(t, u, 0)
	if string(params["enable_thinking"]) != "true" {
		t.Fatalf("enable_thinking = %s", params["enable_thinking"])
	}
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("toggle-only enable invented reasoning_effort: %v", params)
	}
	if !top || !mirror {
		t.Fatalf("enable flags: top=%v mirror=%v", top, mirror)
	}
	resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	params, top, mirror, offID := qoderReasoningWire(t, u, 1)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
		t.Fatalf("plain auto did not take the disabled default: %v top=%v mirror=%v", params, top, mirror)
	}
	if onID == offID {
		t.Fatal("explicit enable and the disabled default must not share an identity")
	}
}

// Probe C: explicit enabling on a catalog with no reasoning advertisement is a
// client error before inference, not a silent no-op.
func TestQoderReasoningEnableWithoutCapability(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"},
		map[string]string{"qfmodel": `{"key":"qfmodel","is_reasoning":false}`})
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"enable_thinking":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "reasoning model") {
		t.Fatalf("error body %q does not name the nonreasoning veto", body)
	}
	if u.chatCount() != 0 {
		t.Fatal("rejected request reached the upstream")
	}
}

// Probe D: a root-level default_effort counts as an advertised default.
func TestQoderReasoningRootDefaultEffort(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"default_effort":"high","thinking_config":{"enabled":{"efforts":{"low":{},"high":{}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"}, map[string]string{"qfmodel": catalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"high"` || string(params["enable_thinking"]) != "true" {
		t.Fatalf("root default not applied: %v", params)
	}
	if !top || !mirror {
		t.Fatalf("flags: top=%v mirror=%v", top, mirror)
	}
}

// Probe E: a root-level supports_disabled flag advertises disable support.
func TestQoderReasoningRootSupportsDisabled(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"supports_disabled":true,"thinking_config":{"enabled":{"efforts":{"low":{}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(none)"}, map[string]string{"qfmodel": catalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
		t.Fatalf("root disable support not honored: %v top=%v mirror=%v", params, top, mirror)
	}
}

// Probe F: a disabled default and a positive default conflict, so omitted
// intent stays silent instead of letting the disabled state win by precedence.
func TestQoderReasoningConflictingDefaultsUnset(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{"is_default":true}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"}, map[string]string{"qfmodel": catalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if _, ok := params["reasoning_effort"]; ok {
		t.Fatalf("conflicting defaults invented reasoning_effort: %v", params)
	}
	if _, ok := params["enable_thinking"]; ok {
		t.Fatalf("conflicting defaults invented enable_thinking: %v", params)
	}
	if !top || !mirror {
		t.Fatalf("catalog flags lost: top=%v mirror=%v", top, mirror)
	}
}

// Probe G: default_effort "none" with advertised disable support selects the
// disabled default and shares the explicit-disable identity.
func TestQoderReasoningNoneDefaultWithDisableSupport(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"thinking_config":{"disabled":{},"default_effort":"none","enabled":{"efforts":{"low":{}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{
		"default": "qoder/qfmodel",
		"off":     "qoder/qfmodel(none)",
	}, map[string]string{"qfmodel": catalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, defID := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
		t.Fatalf("none default not applied: %v top=%v mirror=%v", params, top, mirror)
	}
	resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("off", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	_, _, _, offID := qoderReasoningWire(t, u, 1)
	if defID != offID {
		t.Fatalf("none default and explicit none must share identity: %q vs %q", defID, offID)
	}
}

// Real unary and streaming requests across all three ingress formats must
// select the same effective controls from the same explicit body intent.
func TestQoderReasoningIngressUnaryStreamingMatrix(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		request string
	}{
		{"openai chat stream", "/v1/chat/completions",
			`{"model":"default","stream":true,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`},
		{"openai chat unary", "/v1/chat/completions",
			`{"model":"default","stream":false,"reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`},
		{"openai responses stream", "/v1/responses",
			`{"model":"default","stream":true,"max_output_tokens":1024,"reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{"openai responses unary", "/v1/responses",
			`{"model":"default","max_output_tokens":1024,"reasoning":{"effort":"max"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{"anthropic messages stream", "/v1/messages",
			`{"model":"default","stream":true,"max_tokens":1024,"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`},
		{"anthropic messages unary", "/v1/messages",
			`{"model":"default","max_tokens":1024,"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`},
	}
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"},
		map[string]string{"qfmodel": qoderReasoningCatalog})
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postStream(t, base+tc.path, token, tc.request)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if text := qoderIngressText(t, tc.path, body); text != "hello" {
				t.Fatalf("text = %q", text)
			}
			params, top, mirror, _ := qoderReasoningWire(t, u, i)
			if string(params["reasoning_effort"]) != `"xhigh"` {
				t.Fatalf("reasoning_effort = %s, want remapped xhigh", params["reasoning_effort"])
			}
			if string(params["enable_thinking"]) != "true" || !top || !mirror {
				t.Fatalf("controls: %s top=%v mirror=%v", params["enable_thinking"], top, mirror)
			}
		})
	}
	if u.chatCount() != len(cases) {
		t.Fatalf("chat sends = %d, want %d", u.chatCount(), len(cases))
	}
}

// Rejected reasoning intent must fail before inference with each ingress
// format's own error envelope.
func TestQoderReasoningPerIngressErrorEnvelopes(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		request string
	}{
		{"openai chat", "/v1/chat/completions",
			`{"model":"default","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`},
		{"openai responses", "/v1/responses",
			`{"model":"default","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{"anthropic messages", "/v1/messages",
			`{"model":"default","max_tokens":1024,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
			base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"},
				map[string]string{"qfmodel": `{"key":"qfmodel","is_reasoning":false}`})
			resp, body := postStream(t, base+tc.path, token, tc.request)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			assertIngressErrorEnvelope(t, tc.path, []byte(body), "invalid_request_error")
			if !strings.Contains(body, "reasoning model") {
				t.Fatalf("error body %q does not name the nonreasoning veto", body)
			}
			if u.chatCount() != 0 {
				t.Fatal("rejected request reached the upstream")
			}
		})
	}
}

// The Anthropic budget intensity is replaced by the suffix effort, so the
// request succeeds with the remapped highest level instead of a budget
// rejection, even though BudgetSet survives the merge.
func TestQoderReasoningSuffixOverridesBudget(t *testing.T) {
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel(max)"},
		map[string]string{"qfmodel": qoderReasoningCatalog})
	resp, body := postStream(t, base+"/v1/messages", token,
		`{"model":"default","stream":true,"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, ok := collectStreamText(t, "/v1/messages", body); !ok || text != "hello" {
		t.Fatalf("text = %q finished = %v", text, ok)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"xhigh"` {
		t.Fatalf("reasoning_effort = %s, want suffix max to replace the budget", params["reasoning_effort"])
	}
	if string(params["enable_thinking"]) != "true" || !top || !mirror {
		t.Fatalf("controls: %s top=%v mirror=%v", params["enable_thinking"], top, mirror)
	}
}

// Probe A: an effort enum that explicitly lists none advertises disable
// support by itself, so a none suffix disables instead of erroring.
func TestQoderReasoningEnumNoneSuffixDisables(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":true,"efforts":{"none":{},"low":{}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{"none": "qoder/qfmodel(none)"}, map[string]string{"qfmodel": catalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("none", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if text, ok := collectStreamText(t, "/v1/chat/completions", body); !ok || text != "hello" {
		t.Fatalf("text = %q finished = %v", text, ok)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" {
		t.Fatalf("disable controls: %s %s", params["reasoning_effort"], params["enable_thinking"])
	}
	if top || mirror {
		t.Fatalf("disable flags: model_config=%v mirror=%v", top, mirror)
	}
}

// Probe B: an advertised none/off enum entry marked is_default is a valid
// disabled default and shares the explicit-disable identity; a conflict with
// a positive default stays unset.
func TestQoderReasoningEnumDisabledDefault(t *testing.T) {
	cases := []struct{ name, catalog string }{
		{"none is_default", `{"key":"qfmodel","is_reasoning":true,"efforts":{"none":{"is_default":true},"low":{}}}`},
		{"off isDefault", `{"key":"qfmodel","is_reasoning":true,"efforts":{"off":{"isDefault":true},"low":{}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
			base, token := qoderReasoningHarness(t, u, map[string]string{
				"default": "qoder/qfmodel",
				"off":     "qoder/qfmodel(none)",
			}, map[string]string{"qfmodel": tc.catalog})
			resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			params, top, mirror, defID := qoderReasoningWire(t, u, 0)
			if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
				t.Fatalf("disabled default not applied: %v top=%v mirror=%v", params, top, mirror)
			}
			resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("off", ""))
			if resp.StatusCode != http.StatusOK {
				t.Fatal(resp.StatusCode)
			}
			_, _, _, offID := qoderReasoningWire(t, u, 1)
			if defID != offID {
				t.Fatalf("enum disabled default and explicit none must share identity: %q vs %q", defID, offID)
			}
		})
	}
	t.Run("conflict with positive default stays unset", func(t *testing.T) {
		catalog := `{"key":"qfmodel","is_reasoning":true,"efforts":{"none":{"is_default":true},"low":{"is_default":true}}}`
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"}, map[string]string{"qfmodel": catalog})
		resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		params, top, mirror, _ := qoderReasoningWire(t, u, 0)
		if _, ok := params["reasoning_effort"]; ok {
			t.Fatalf("conflicting enum defaults invented reasoning_effort: %v", params)
		}
		if _, ok := params["enable_thinking"]; ok {
			t.Fatalf("conflicting enum defaults invented enable_thinking: %v", params)
		}
		if !top || !mirror {
			t.Fatalf("catalog flags lost: top=%v mirror=%v", top, mirror)
		}
	})
}

// Probe C: a malformed catalog is_reasoning flag must never produce a
// contradictory native request. Explicit controls are rejected before
// inference with each ingress format's own envelope; omitted and plain auto
// intent infer normally without automatic controls, the raw catalog field
// stays untouched, and the mirror keeps its safe false.
func TestQoderReasoningMalformedFlagPolicy(t *testing.T) {
	catalog := `{"key":"qfmodel","is_reasoning":"yes","thinking_config":{"enabled":{"efforts":{"low":{"is_default":true}}}}}`
	t.Run("explicit effort rejected across ingresses", func(t *testing.T) {
		cases := []struct{ name, path, request string }{
			{"openai chat", "/v1/chat/completions", `{"model":"default","stream":true,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`},
			{"openai responses", "/v1/responses", `{"model":"default","max_output_tokens":1024,"reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
			{"anthropic messages", "/v1/messages", `{"model":"default","max_tokens":1024,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
				base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"}, map[string]string{"qfmodel": catalog})
				resp, body := postStream(t, base+tc.path, token, tc.request)
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
				}
				assertIngressErrorEnvelope(t, tc.path, []byte(body), "invalid_request_error")
				if !strings.Contains(body, "malformed") {
					t.Fatalf("error body %q does not name the malformed flag", body)
				}
				if u.chatCount() != 0 {
					t.Fatal("rejected request reached the upstream")
				}
			})
		}
	})
	t.Run("explicit disable rejected despite disabled block", func(t *testing.T) {
		withDisabled := `{"key":"qfmodel","is_reasoning":"yes","thinking_config":{"disabled":{},"enabled":{"efforts":{"low":{}}}}}`
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{
			"default": "qoder/qfmodel",
			"none":    "qoder/qfmodel(none)",
		}, map[string]string{"qfmodel": withDisabled})
		resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("none", ""))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("suffix none status = %d, body = %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "malformed") {
			t.Fatalf("error body %q does not name the malformed flag", body)
		}
		resp, body = postStream(t, base+"/v1/chat/completions", token,
			`{"model":"default","stream":true,"enable_thinking":false,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("enable_thinking false status = %d, body = %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "malformed") {
			t.Fatalf("error body %q does not name the malformed flag", body)
		}
		if u.chatCount() != 0 {
			t.Fatal("rejected disable reached the upstream")
		}
	})
	t.Run("explicit enable rejected", func(t *testing.T) {
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{"default": "qoder/qfmodel"}, map[string]string{"qfmodel": catalog})
		resp, body := postStream(t, base+"/v1/chat/completions", token,
			`{"model":"default","stream":true,"enable_thinking":true,"messages":[{"role":"user","content":"hi"}]}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if !strings.Contains(body, "malformed") {
			t.Fatalf("error body %q does not name the malformed flag", body)
		}
		if u.chatCount() != 0 {
			t.Fatal("rejected enable reached the upstream")
		}
	})
	t.Run("plain auto and nil stay silent", func(t *testing.T) {
		u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
		base, token := qoderReasoningHarness(t, u, map[string]string{
			"default": "qoder/qfmodel",
			"auto":    "qoder/qfmodel(auto)",
		}, map[string]string{"qfmodel": catalog})
		for i, model := range []string{"default", "auto"} {
			resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody(model, ""))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s status = %d, body = %s", model, resp.StatusCode, body)
			}
			if text, ok := collectStreamText(t, "/v1/chat/completions", body); !ok || text != "hello" {
				t.Fatalf("%s text = %q finished = %v", model, text, ok)
			}
			plain, err := qoder.DecodeWireBody(qoderChatBodyAt(t, u, i))
			if err != nil {
				t.Fatal(err)
			}
			var raw struct {
				Parameters  map[string]json.RawMessage `json:"parameters"`
				ModelConfig map[string]json.RawMessage `json:"model_config"`
				ChatContext struct {
					Extra struct {
						ModelConfig map[string]json.RawMessage `json:"modelConfig"`
					} `json:"extra"`
				} `json:"chat_context"`
			}
			if err := json.Unmarshal(plain, &raw); err != nil {
				t.Fatal(err)
			}
			if _, ok := raw.Parameters["reasoning_effort"]; ok {
				t.Fatalf("%s invented reasoning_effort: %v", model, raw.Parameters)
			}
			if _, ok := raw.Parameters["enable_thinking"]; ok {
				t.Fatalf("%s invented enable_thinking: %v", model, raw.Parameters)
			}
			if string(raw.ModelConfig["is_reasoning"]) != `"yes"` {
				t.Fatalf("%s coerced the malformed catalog flag: %s", model, raw.ModelConfig["is_reasoning"])
			}
			if string(raw.ChatContext.Extra.ModelConfig["is_reasoning"]) != "false" {
				t.Fatalf("%s mirror flag not at safe false: %s", model, raw.ChatContext.Extra.ModelConfig["is_reasoning"])
			}
		}
	})
}

// A catalog entry without an is_reasoning member keeps the default-resolution
// behavior, and both flags follow the effective state.
func TestQoderReasoningMissingFlagFollowsEffectiveState(t *testing.T) {
	catalog := `{"key":"qfmodel","supports_disabled":true,"thinking_config":{"enabled":{"efforts":{"low":{"is_default":true}}}}}`
	u := &qoderUpstream{chatResponses: []qoderResponse{{status: 200, sse: qoderSuccessSSE}}}
	base, token := qoderReasoningHarness(t, u, map[string]string{
		"default": "qoder/qfmodel",
		"none":    "qoder/qfmodel(none)",
	}, map[string]string{"qfmodel": catalog})
	resp, body := postStream(t, base+"/v1/chat/completions", token, qoderChatBody("default", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	params, top, mirror, _ := qoderReasoningWire(t, u, 0)
	if string(params["reasoning_effort"]) != `"low"` || string(params["enable_thinking"]) != "true" || !top || !mirror {
		t.Fatalf("default not applied with missing flag: %v top=%v mirror=%v", params, top, mirror)
	}
	resp, _ = postStream(t, base+"/v1/chat/completions", token, qoderChatBody("none", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	params, top, mirror, _ = qoderReasoningWire(t, u, 1)
	if string(params["reasoning_effort"]) != `"none"` || string(params["enable_thinking"]) != "false" || top || mirror {
		t.Fatalf("disable not applied with missing flag: %v top=%v mirror=%v", params, top, mirror)
	}
}
