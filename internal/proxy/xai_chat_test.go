package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/store"
	"airouter/internal/xaigrant"
)

type xaiCapture struct {
	mu         sync.Mutex
	bodies     [][]byte
	auth       []string
	n          int
	failN      int
	failStatus int
	status     int
	sse        string
	jsonBody   string
	redirect   string
}

func (c *xaiCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, append([]byte(nil), body...))
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	n := c.n
	c.n++
	fail := n < c.failN
	failStatus := c.failStatus
	if failStatus == 0 {
		failStatus = http.StatusBadGateway
	}
	redirect := c.redirect
	status := c.status
	sse := c.sse
	jsonBody := c.jsonBody
	c.mu.Unlock()
	if fail {
		http.Error(w, "upstream failure", failStatus)
		return
	}
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	if status != 0 {
		http.Error(w, "upstream failure", status)
		return
	}
	if jsonBody != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonBody)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sse)
}

func (c *xaiCapture) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return string(c.bodies[len(c.bodies)-1])
}

func (c *xaiCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *xaiCapture) bodyAt(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.bodies) {
		return ""
	}
	return string(c.bodies[i])
}

type xaiRouteTransport struct{ base string }

func (t xaiRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !xaigrant.IsStandardResponsesURL(r.URL.String()) {
		return nil, io.ErrUnexpectedEOF
	}
	u, err := url.Parse(t.base + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func xaiJWTAccess(sub string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": sub})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func xaiTestCreds(generation, sub string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: xaigrant.Preset, ClientID: xaigrant.ClientID,
		TokenURL: xaigrant.TokenURL, AccessToken: xaiJWTAccess(sub), RefreshToken: "refresh-" + generation,
		XAIGrantGeneration: generation, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
}

func addXAIChatProvider(t *testing.T, st *store.Store, name, generation, sub string) *domain.Provider {
	t.Helper()
	p := &domain.Provider{
		Name: name, BaseURL: xaigrant.APIBase, Protocol: domain.ProtocolOpenAIResponses,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: xaiTestCreds(generation, sub),
	}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func xaiCombo(t *testing.T, st *store.Store, targets ...domain.ComboTarget) {
	t.Helper()
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: targets}); err != nil {
		t.Fatal(err)
	}
}

func xaiAccessKey(t *testing.T, st *store.Store) string {
	t.Helper()
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	return key.Token
}

func xaiPlainSSE(text string) string {
	return "data: " + `{"type":"response.output_text.delta","output_index":0,"delta":` + grokJSONString(text) + `}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_plain","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func xaiUnaryResponse() string {
	return `{"id":"resp_unary","model":"grok-4.7","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_unary","status":"completed","encrypted_content":"cipher-unary"},` +
		`{"type":"message","id":"msg_unary","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
		`{"type":"function_call","id":"fc_native","call_id":"call_x","name":"read","arguments":"{}"}],` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
}

func TestXAIChatStreamBridgeAndReplay(t *testing.T) {
	cap := &xaiCapture{sse: grokChatSSE("hello", "cipher-x", "think", false, "fc_a|call_a|read|{}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})

	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) {
		t.Fatalf("first status %d body %s", resp.StatusCode, out)
	}
	first := cap.lastBody()
	if !strings.Contains(first, `"store":false`) || strings.Count(first, "reasoning.encrypted_content") != 1 || strings.Contains(first, "prompt_cache_key") {
		t.Fatalf("first upstream body = %s", first)
	}
	details := extractChatDetails(t, out)
	raw := string(mustChatJSON(details))
	if !strings.Contains(raw, "cipher-x") || !strings.Contains(raw, `"item":"fc_a"`) || strings.Contains(raw, `"call_id"`) {
		t.Fatalf("details = %s", raw)
	}

	calls := `[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]`
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, replayBody("combo", details, "hello", calls, true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status %d body %s", resp.StatusCode, out)
	}
	sent := cap.lastBody()
	for _, want := range []string{"cipher-x", "fc_a", "call_a", `"store":false`, "reasoning.encrypted_content"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("replay missing %s: %s", want, sent)
		}
	}
	if strings.Contains(sent, grokbuild.XAIReplayFormat) || strings.Count(sent, "reasoning.encrypted_content") != 1 {
		t.Fatalf("bridge leaked or include duplicated: %s", sent)
	}
}

func TestXAIChatUnaryPreservesNativeItemID(t *testing.T) {
	cap := &xaiCapture{jsonBody: xaiUnaryResponse()}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})

	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unary status %d body %s", resp.StatusCode, out)
	}
	body := string(out)
	if !strings.Contains(body, "hello") || !strings.Contains(body, `"id":"call_x"`) {
		t.Fatalf("unary chat body = %s", body)
	}
	details := extractChatDetails(t, out)
	raw := string(mustChatJSON(details))
	if !strings.Contains(raw, "cipher-unary") || !strings.Contains(raw, `"item":"fc_native"`) {
		t.Fatalf("marker did not preserve native item id: %s", raw)
	}
	calls := `[{"id":"call_x","type":"function","function":{"name":"read","arguments":"{}"}}]`
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, replayBody("combo", details, "hello", calls, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status %d body %s", resp.StatusCode, out)
	}
	sent := cap.lastBody()
	if !strings.Contains(sent, `"id":"fc_native"`) || !strings.Contains(sent, `"call_id":"call_x"`) || !strings.Contains(sent, "cipher-unary") {
		t.Fatalf("native function item id was not replayed separately: %s", sent)
	}
}

func TestXAIChatRejectsProvisionalCiphertext(t *testing.T) {
	sse := "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_t","status":"in_progress","encrypted_content":"cipher-provisional","summary":[]}}` + "\n\n" +
		"data: " + `{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_t","summary_index":0,"delta":"think"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_t","status":"completed","encrypted_content":"cipher-authoritative","summary":[{"type":"summary_text","text":"think"}]}}` + "\n\n" +
		"data: " + `{"type":"response.output_text.delta","output_index":1,"delta":"hello"}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_t","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"data: [DONE]\n\n"
	cap := &xaiCapture{sse: sse}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})

	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", resp.StatusCode, out)
	}
	raw := string(mustChatJSON(extractChatDetails(t, out)))
	if !strings.Contains(raw, "cipher-authoritative") || strings.Contains(raw, "cipher-provisional") {
		t.Fatalf("provisional ciphertext became authoritative: %s", raw)
	}
}

func TestXAIChatOlderSourceReplaysFirstTurnOnly(t *testing.T) {
	cap := &xaiCapture{sse: grokChatSSE("one", "cipher-1", "sum-1", false, "fc_1|call_1|read|{}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})

	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	first := extractChatDetails(t, out)
	cap.mu.Lock()
	cap.sse = xaiPlainSSE("two")
	cap.mu.Unlock()
	secondRequest := map[string]any{"model": "combo", "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "one", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}, "reasoning_details": first},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result-1"},
		map[string]any{"role": "user", "content": "second"},
	}}
	raw, _ := json.Marshal(secondRequest)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	cap.mu.Lock()
	cap.sse = grokChatSSE("three", "cipher-3", "sum-3", false, "fc_3|call_3|list|{}")
	cap.mu.Unlock()
	secondRequest["messages"] = append(secondRequest["messages"].([]any),
		map[string]any{"role": "assistant", "content": "two", "tool_calls": []any{map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "list", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "result-2"},
		map[string]any{"role": "user", "content": "third"},
	)
	raw, _ = json.Marshal(secondRequest)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	sent := cap.lastBody()
	for _, want := range []string{"cipher-1", "fc_1", "call_1", "result-1", "call_2", "result-2"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("older replay lost %s: %s", want, sent)
		}
	}
	if strings.Contains(sent, grokbuild.XAIReplayFormat) || strings.Contains(sent, "cipher-3") {
		t.Fatalf("unmarked newer turn or marker leaked: %s", sent)
	}
}

func TestXAIChatCacheAffinityIsScopedAndStable(t *testing.T) {
	cap := &xaiCapture{sse: xaiPlainSSE("ok")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})

	request := `{"model":"combo","stream":true,"prompt_cache_key":"explicit-key","messages":[{"role":"user","content":"first"}]}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(request))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Grok-Conv-Id", "header-key")
	req.Header.Set("X-Client-Request-Id", "volatile-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	first := cap.lastBody()
	var firstDoc map[string]any
	if json.Unmarshal([]byte(first), &firstDoc) != nil {
		t.Fatalf("body not JSON: %s", first)
	}
	firstKey, _ := firstDoc["prompt_cache_key"].(string)
	if firstKey == "" || firstKey == "explicit-key" || firstKey == "header-key" || strings.Contains(first, "volatile-request") {
		t.Fatalf("affinity forwarded or unscoped: %s", first)
	}
	if len(firstKey) != 64 {
		t.Fatalf("cache key length = %d", len(firstKey))
	}
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, request)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	var secondDoc map[string]any
	if json.Unmarshal([]byte(cap.lastBody()), &secondDoc) != nil {
		t.Fatal("second body not JSON")
	}
	if secondDoc["prompt_cache_key"] != firstKey {
		t.Fatalf("cache key changed: %v %v", firstKey, secondDoc["prompt_cache_key"])
	}
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"none"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	if strings.Contains(cap.lastBody(), "prompt_cache_key") {
		t.Fatalf("absent affinity invented a key: %s", cap.lastBody())
	}
}

func TestXAIChatRejectsTamperedMarkerBeforeUpstream(t *testing.T) {
	cap := &xaiCapture{sse: grokChatSSE("hello", "cipher-x", "think", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	details[0] = json.RawMessage(strings.Replace(string(details[0]), "cipher-x", "cipher-tampered", 1))
	before := cap.count()
	body := replayBody("combo", details, "hello", "", true)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, body)
	if resp.StatusCode != http.StatusBadRequest || cap.count() != before {
		t.Fatalf("tamper status %d calls %d body %s", resp.StatusCode, cap.count()-before, out)
	}
}

func TestXAIChatWithoutReplayKeyKeepsOrdinaryTranslation(t *testing.T) {
	cap := &xaiCapture{sse: grokChatSSE("hello", "cipher-x", "think", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := openTestStore(t, false)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) {
		t.Fatalf("no-key status %d body %s", resp.StatusCode, out)
	}
	sent := cap.lastBody()
	if strings.Contains(sent, `"store":false`) || strings.Contains(sent, "reasoning.encrypted_content") || strings.Contains(sent, "prompt_cache_key") {
		t.Fatalf("no-key request changed the transport: %s", sent)
	}
}

func TestXAIChatIneligibleEndpointAndModelDoNotBridge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		base  string
		model string
	}{
		{"foreign endpoint", "https://api.x.ai.evil/v1", "grok-4.7"},
		{"other model", xaigrant.APIBase, "grok-4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &xaiCapture{sse: grokChatSSE("hello", "cipher-x", "think", false)}
			up := httptest.NewServer(cap)
			t.Cleanup(up.Close)
			st := newTestStore(t)
			p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
			p.BaseURL = tc.base
			if err := st.UpdateProvider(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: tc.model, Enabled: true})
			key := xaiAccessKey(t, st)
			ts := startChatProxyTransport(t, st, xaiLooseRouteTransport{base: up.URL})
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d body %s", resp.StatusCode, out)
			}
			if sent := cap.lastBody(); strings.Contains(sent, `"store":false`) || strings.Contains(sent, "reasoning.encrypted_content") {
				t.Fatalf("ineligible request changed transport: %s", sent)
			}
			if bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) {
				t.Fatalf("ineligible response was stamped: %s", out)
			}
		})
	}
}

func TestXAIChatRedirectAndFailoverStripBoundary(t *testing.T) {
	redirectHits := &xaiCapture{sse: xaiPlainSSE("redirected")}
	redirectSrv := httptest.NewServer(redirectHits)
	t.Cleanup(redirectSrv.Close)
	origin := &xaiCapture{sse: grokChatSSE("one", "cipher-1", "sum-1", false)}
	originSrv := httptest.NewServer(origin)
	t.Cleanup(originSrv.Close)
	other := &xaiCapture{sse: xaiPlainSSE("fallback")}
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)
	st := newTestStore(t)
	a := addXAIChatProvider(t, st, "a", "gen-a", "user-a")
	b := addXAIChatProvider(t, st, "b", "gen-b", "user-b")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}, domain.ComboTarget{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: originSrv.URL})
	// The transport always targets origin; use per-call routing by body auth is
	// unnecessary here because the pinned target is A and failover B must be sent
	// to B without replay bytes.
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	origin.mu.Lock()
	origin.redirect = redirectSrv.URL + "/steal"
	origin.mu.Unlock()
	other.mu.Lock()
	other.sse = xaiPlainSSE("fallback")
	other.mu.Unlock()
	// Switch the proxy transport to route provider B to otherSrv while A still
	// reaches origin. The transport selects by authorization header.
	ts2 := startChatProxyTransport(t, st, xaiAuthRouteTransport{
		"Bearer " + xaiTestCreds("gen-a", "user-a").AccessToken: originSrv.URL,
		"Bearer " + xaiTestCreds("gen-b", "user-b").AccessToken: otherSrv.URL,
	})
	resp, out = chatPost(t, ts2.URL+"/v1/chat/completions", key, replayBody("combo", details, "one", "", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failover status %d body %s", resp.StatusCode, out)
	}
	if redirectHits.count() != 0 || origin.count() != 2 || other.count() != 1 {
		t.Fatalf("redirect/failover counts redirect=%d origin=%d other=%d", redirectHits.count(), origin.count(), other.count())
	}
	if sent := other.lastBody(); strings.Contains(sent, "cipher-1") || strings.Contains(sent, grokbuild.XAIReplayFormat) || strings.Contains(sent, "fc_") {
		t.Fatalf("failover forwarded bridge state: %s", sent)
	}
}

type xaiRedirectRouteTransport struct {
	api     map[string]string
	foreign string
}

func (t xaiRedirectRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var base string
	switch strings.ToLower(r.URL.Hostname()) {
	case "api.x.ai":
		base = t.api[r.Header.Get("Authorization")]
	case "foreign.invalid":
		base = t.foreign
	}
	if base == "" {
		return nil, io.ErrUnexpectedEOF
	}
	u, err := url.Parse(base + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func xaiPlainUnary(text string) string {
	return `{"id":"resp_plain","model":"grok-4.7","status":"completed","output":[` +
		`{"type":"message","id":"msg_plain","role":"assistant","content":[{"type":"output_text","text":` + grokJSONString(text) + `}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
}

func TestXAIChatFirstTurnRedirectIsNotFollowed(t *testing.T) {
	cases := []struct {
		name     string
		stream   bool
		failover bool
		redirect string
	}{
		{"unary foreign", false, false, "https://foreign.invalid/steal"},
		{"stream foreign", true, false, "https://foreign.invalid/steal"},
		{"stream failover", true, true, "https://foreign.invalid/steal"},
		{"unary same host", false, false, "https://api.x.ai/v1/loop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			foreign := &xaiCapture{}
			if tc.stream {
				foreign.sse = grokChatSSE("foreign", "cipher-foreign", "sum-foreign", false)
			} else {
				foreign.jsonBody = xaiPlainUnary("foreign")
			}
			foreignSrv := httptest.NewServer(foreign)
			t.Cleanup(foreignSrv.Close)
			origin := &xaiCapture{redirect: tc.redirect}
			originSrv := httptest.NewServer(origin)
			t.Cleanup(originSrv.Close)

			st := newTestStore(t)
			a := addXAIChatProvider(t, st, "a", "gen-a", "user-a")
			targets := []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}
			routes := map[string]string{"Bearer " + a.OAuthCreds.AccessToken: originSrv.URL}
			var fallback *xaiCapture
			if tc.failover {
				fallback = &xaiCapture{sse: xaiPlainSSE("fallback")}
				fallbackSrv := httptest.NewServer(fallback)
				t.Cleanup(fallbackSrv.Close)
				b := addXAIChatProvider(t, st, "b", "gen-b", "user-b")
				targets = append(targets, domain.ComboTarget{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true})
				routes["Bearer "+b.OAuthCreds.AccessToken] = fallbackSrv.URL
			}
			xaiCombo(t, st, targets...)
			key := xaiAccessKey(t, st)
			ts := startChatProxyTransport(t, st, xaiRedirectRouteTransport{api: routes, foreign: foreignSrv.URL})
			body := `{"model":"combo","messages":[{"role":"user","content":"first"}]}`
			if tc.stream {
				body = `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`
			}
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, body)
			if foreign.count() != 0 {
				t.Fatalf("foreign endpoint received %d request(s)", foreign.count())
			}
			if origin.count() != 1 {
				t.Fatalf("origin redirect count = %d, want 1", origin.count())
			}
			if bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) || bytes.Contains(out, []byte("cipher-foreign")) {
				t.Fatalf("redirected response was signed: %s", out)
			}
			if tc.failover {
				if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("fallback")) {
					t.Fatalf("failover status %d body %s", resp.StatusCode, out)
				}
				if fallback.count() != 1 || strings.Contains(fallback.lastBody(), grokbuild.XAIReplayFormat) {
					t.Fatalf("failover count=%d body=%s", fallback.count(), fallback.lastBody())
				}
				return
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("local redirect status = %d, body %s", resp.StatusCode, out)
			}
		})
	}
}

func TestXAIChatFirstTurnRetryRedirectIsNotFollowed(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "unary"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			foreign := &xaiCapture{}
			if stream {
				foreign.sse = grokChatSSE("foreign", "cipher-foreign", "sum-foreign", false)
			} else {
				foreign.jsonBody = xaiPlainUnary("foreign")
			}
			foreignSrv := httptest.NewServer(foreign)
			t.Cleanup(foreignSrv.Close)
			origin := &xaiCapture{
				redirect:   "https://foreign.invalid/steal",
				failN:      1,
				failStatus: http.StatusUnauthorized,
			}
			originSrv := httptest.NewServer(origin)
			t.Cleanup(originSrv.Close)
			st := newTestStore(t)
			p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
			xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
			key := xaiAccessKey(t, st)
			newAccess := xaiJWTAccess("user-1") + "x"
			ts := startChatProxyTransport(t, st, xaiRedirectRouteTransport{
				api: map[string]string{
					"Bearer " + p.OAuthCreds.AccessToken: originSrv.URL,
					"Bearer " + newAccess:                originSrv.URL,
				},
				foreign: foreignSrv.URL,
			})
			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "auth.x.ai" {
					return originalTransport.RoundTrip(r)
				}
				raw, _ := json.Marshal(map[string]any{
					"access_token": newAccess, "refresh_token": "refresh-rotated", "expires_in": 600,
				})
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(raw)),
					Header:     make(http.Header),
				}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			body := `{"model":"combo","messages":[{"role":"user","content":"first"}]}`
			if stream {
				body = `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`
			}
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, body)
			if foreign.count() != 0 {
				t.Fatalf("foreign endpoint received %d request(s)", foreign.count())
			}
			if origin.count() != 2 {
				t.Fatalf("origin count = %d, want initial and refreshed retry", origin.count())
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("retry redirect status = %d, body %s", resp.StatusCode, out)
			}
			if bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) || bytes.Contains(out, []byte("cipher-foreign")) {
				t.Fatalf("redirected retry was signed: %s", out)
			}
		})
	}
}

func TestXAIChatRedirectProtectionScope(t *testing.T) {
	t.Run("chat without signer key follows", func(t *testing.T) {
		foreign := &xaiCapture{sse: xaiPlainSSE("foreign")}
		foreignSrv := httptest.NewServer(foreign)
		t.Cleanup(foreignSrv.Close)
		origin := &xaiCapture{redirect: "https://foreign.invalid/steal"}
		originSrv := httptest.NewServer(origin)
		t.Cleanup(originSrv.Close)
		st := openTestStore(t, false)
		p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
		xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
		key := xaiAccessKey(t, st)
		ts := startChatProxyTransport(t, st, xaiRedirectRouteTransport{
			api:     map[string]string{"Bearer " + p.OAuthCreds.AccessToken: originSrv.URL},
			foreign: foreignSrv.URL,
		})
		resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
		if resp.StatusCode != http.StatusOK || foreign.count() != 1 || !bytes.Contains(out, []byte("foreign")) {
			t.Fatalf("unsigned redirect status=%d foreign=%d body=%s", resp.StatusCode, foreign.count(), out)
		}
		if bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) {
			t.Fatalf("unsigned response was signed: %s", out)
		}
	})

	t.Run("native responses passthrough follows", func(t *testing.T) {
		foreign := &xaiCapture{jsonBody: xaiPlainUnary("foreign")}
		foreignSrv := httptest.NewServer(foreign)
		t.Cleanup(foreignSrv.Close)
		origin := &xaiCapture{redirect: "https://foreign.invalid/steal"}
		originSrv := httptest.NewServer(origin)
		t.Cleanup(originSrv.Close)
		st := newTestStore(t)
		p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
		xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
		key := xaiAccessKey(t, st)
		ts := startChatProxyTransport(t, st, xaiRedirectRouteTransport{
			api:     map[string]string{"Bearer " + p.OAuthCreds.AccessToken: originSrv.URL},
			foreign: foreignSrv.URL,
		})
		resp, out := chatPost(t, ts.URL+"/v1/responses", key, `{"model":"combo","input":"hi"}`)
		if resp.StatusCode != http.StatusOK || foreign.count() != 1 || !bytes.Contains(out, []byte("foreign")) {
			t.Fatalf("native redirect status=%d foreign=%d body=%s", resp.StatusCode, foreign.count(), out)
		}
		if bytes.Contains(out, []byte(grokbuild.XAIReplayFormat)) {
			t.Fatalf("native response was signed: %s", out)
		}
	})
}

type xaiLooseRouteTransport struct{ base string }

func (t xaiLooseRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.base + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

type xaiAuthRouteTransport map[string]string

func (t xaiAuthRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !xaigrant.IsStandardResponsesURL(r.URL.String()) {
		return nil, io.ErrUnexpectedEOF
	}
	base := t[r.Header.Get("Authorization")]
	if base == "" {
		return nil, io.ErrUnexpectedEOF
	}
	u, err := url.Parse(base + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func TestXAIChatPiAdapterLoop(t *testing.T) {
	adapter := os.Getenv("AIROUTER_TEST_PI_CHAT_ADAPTER")
	if adapter == "" {
		t.Skip("AIROUTER_TEST_PI_CHAT_ADAPTER is not set")
	}
	if _, err := os.Stat(adapter); err != nil {
		t.Skip("Pi chat adapter is not available")
	}
	cap := &xaiCapture{sse: grokChatSSE("", "cipher-live", "", false, "fc_a|call_a|read|{}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	script := filepath.Join(t.TempDir(), "pi-xai-loop.mjs")
	if err := os.WriteFile(script, []byte(piXAIChatLoopScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script, ts.URL, key, adapter)
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("pi-xai-loop-ok")) {
		t.Fatalf("pi loop: %v %s calls=%d body=%s", err, out, cap.count(), cap.lastBody())
	}
	if cap.count() < 2 || !strings.Contains(cap.lastBody(), "cipher-live") || !strings.Contains(cap.lastBody(), "call_a") {
		t.Fatalf("proxy did not replay xAI state: calls=%d body=%s", cap.count(), cap.lastBody())
	}
}

const piXAIChatLoopScript = `import { pathToFileURL } from "node:url";
const { stream } = await import(pathToFileURL(process.argv[4]).href);
const base = process.argv[2];
const token = process.argv[3];
const fetchImpl = async (url, init) => fetch(url, init);
const model = { id: "combo", api: "openai-completions", provider: "custom-airouter", baseUrl: base + "/v1", reasoning: true, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 128000, maxTokens: 4096 };
const context = { systemPrompt: "be brief", messages: [{ role: "user", content: "first", timestamp: 1 }], tools: [{ name: "read", description: "read", parameters: { type: "object", properties: { path: { type: "string" } } } }] };
const first = stream(model, context, { apiKey: token, fetch: fetchImpl });
let message;
for await (const ev of first) {
  if (ev.type === "done") message = ev.message;
  if (ev.type === "error") throw new Error(ev.error.errorMessage || "stream error");
}
const signature = message.content.find((b) => b.type === "thinking")?.thinkingSignature || "";
if (!signature.includes("cipher-live") || !signature.includes("airouter-xai-v1")) throw new Error("signature missing bridge");
context.messages.push(message);
context.messages.push({ role: "toolResult", toolCallId: "call_a", toolName: "read", content: [{ type: "text", text: "file text" }], timestamp: 2 });
const second = stream(model, context, { apiKey: token, fetch: fetchImpl });
for await (const ev of second) {
  if (ev.type === "error") throw new Error(ev.error?.errorMessage || "second stream error");
}
console.log("pi-xai-loop-ok");
`

// xaiSSEUsage rewrites the fixed usage object emitted by grokChatSSE.
func xaiSSEUsage(s string) string {
	return strings.Replace(s,
		`"usage":{"input_tokens":4,"output_tokens":5}`,
		`"usage":{"input_tokens":14,"output_tokens":7,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":3}}`,
		1)
}

// xaiChatUsage returns cached and cache-write counts from a Chat unary body or
// the last usage-bearing SSE chunk.
func xaiChatUsage(t *testing.T, body []byte) (int, int) {
	t.Helper()
	parse := func(raw []byte) (int, int, bool) {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil {
			return 0, 0, false
		}
		usage, _ := doc["usage"].(map[string]any)
		if usage == nil {
			return 0, 0, false
		}
		details, _ := usage["prompt_tokens_details"].(map[string]any)
		if details == nil {
			return 0, 0, true
		}
		read, _ := details["cached_tokens"].(float64)
		write, _ := details["cache_write_tokens"].(float64)
		return int(read), int(write), true
	}
	if !bytes.Contains(body, []byte("data:")) {
		read, write, ok := parse(body)
		if !ok {
			t.Fatalf("no usage in %s", body)
		}
		return read, write
	}
	read, write := 0, 0
	found := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		if r, w, ok := parse([]byte(line)); ok {
			read, write, found = r, w, true
		}
	}
	if !found {
		t.Fatalf("no usage in stream %s", body)
	}
	return read, write
}

func xaiUpstreamInput(t *testing.T, body string) []any {
	t.Helper()
	var doc struct {
		Input []any `json:"input"`
	}
	if json.Unmarshal([]byte(body), &doc) != nil {
		t.Fatalf("upstream body: %s", body)
	}
	return doc.Input
}

func xaiUpstreamConfig(t *testing.T, body string) map[string]any {
	t.Helper()
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) != nil {
		t.Fatalf("upstream body: %s", body)
	}
	delete(doc, "input")
	return doc
}

func xaiUnaryBridgeResponse() string {
	return `{"id":"resp_1","model":"grok-4.7","status":"completed","output":[` +
		`{"type":"reasoning","id":"rs_1","status":"completed","encrypted_content":"cipher-1","summary":[{"type":"summary_text","text":"sum-1"}]},` +
		`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"one"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"a\"}"}],` +
		`"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":3}}}`
}

func xaiUnaryToolResponse() string {
	return `{"id":"resp_2","model":"grok-4.7","status":"completed","output":[` +
		`{"type":"message","id":"msg_2","status":"completed","role":"assistant","content":[{"type":"output_text","text":"two"}]},` +
		`{"type":"function_call","id":"fc_2","call_id":"call_2","name":"list","arguments":"{\"q\":\"b\"}"}],` +
		`"usage":{"input_tokens":20,"output_tokens":6,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":3}}}`
}

func TestXAIChatLiveRowChangeDeclinesPinnedReplay(t *testing.T) {
	origin := &xaiCapture{sse: grokChatSSE("one", "cipher-1", "sum-1", false, "fc_1|call_1|read|{}")}
	originSrv := httptest.NewServer(origin)
	t.Cleanup(originSrv.Close)
	other := &xaiCapture{sse: xaiPlainSSE("fallback")}
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)
	st := newTestStore(t)
	a := addXAIChatProvider(t, st, "a", "gen-a", "user-a")
	b := addXAIChatProvider(t, st, "b", "gen-b", "user-b")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}, domain.ComboTarget{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiAuthRouteTransport{
		"Bearer " + a.OAuthCreds.AccessToken: originSrv.URL,
		"Bearer " + b.OAuthCreds.AccessToken: otherSrv.URL,
	})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	var once sync.Once
	testXAIReplayResolved = func(*domain.Provider) {
		once.Do(func() {
			live, err := st.GetProvider(context.Background(), a.ID)
			if err != nil {
				t.Errorf("load live: %v", err)
				return
			}
			live.Protocol = domain.ProtocolOpenAI
			if err := st.UpdateProvider(context.Background(), live); err != nil {
				t.Errorf("update live: %v", err)
			}
		})
	}
	t.Cleanup(func() { testXAIReplayResolved = nil })
	beforeOrigin, beforeOther := origin.count(), other.count()
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, replayBody("combo", details, "one", `[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]`, true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback status %d body %s", resp.StatusCode, out)
	}
	if origin.count() != beforeOrigin || other.count() != beforeOther+1 {
		t.Fatalf("live edit sent replay: origin=%d other=%d", origin.count()-beforeOrigin, other.count()-beforeOther)
	}
	if sent := other.lastBody(); strings.Contains(sent, "cipher-1") || strings.Contains(sent, grokbuild.XAIReplayFormat) || strings.Contains(sent, "fc_1") {
		t.Fatalf("portable fallback forwarded bridge state: %s", sent)
	}
}

func TestXAIChatRefreshRechecksLiveBinding(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(context.Context, *store.Store, int64) error
		failStatus int
	}{
		{"protocol change", func(ctx context.Context, st *store.Store, id int64) error {
			live, err := st.GetProvider(ctx, id)
			if err != nil {
				return err
			}
			live.Protocol = domain.ProtocolOpenAI
			return st.UpdateProvider(ctx, live)
		}, 0},
		{"endpoint change", func(ctx context.Context, st *store.Store, id int64) error {
			live, err := st.GetProvider(ctx, id)
			if err != nil {
				return err
			}
			live.BaseURL = "https://api.x.ai.evil/v1"
			return st.UpdateProvider(ctx, live)
		}, 0},
		{"same grant refresh", nil, 0},
		{"same grant refresh forbidden", nil, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origin := &xaiCapture{sse: grokChatSSE("one", "cipher-1", "sum-1", false, "fc_1|call_1|read|{}")}
			originSrv := httptest.NewServer(origin)
			t.Cleanup(originSrv.Close)
			other := &xaiCapture{sse: xaiPlainSSE("fallback")}
			otherSrv := httptest.NewServer(other)
			t.Cleanup(otherSrv.Close)
			st := newTestStore(t)
			a := addXAIChatProvider(t, st, "a", "gen-a", "user-a")
			b := addXAIChatProvider(t, st, "b", "gen-b", "user-b")
			xaiCombo(t, st, domain.ComboTarget{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}, domain.ComboTarget{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true})
			key := xaiAccessKey(t, st)
			// Same principal, different credential bytes: the refresh must still
			// be treated as one grant.
			newAccess := xaiJWTAccess("user-a") + "x"
			ts := startChatProxyTransport(t, st, xaiAuthRouteTransport{
				"Bearer " + a.OAuthCreds.AccessToken: originSrv.URL,
				"Bearer " + newAccess:                originSrv.URL,
				"Bearer " + b.OAuthCreds.AccessToken: otherSrv.URL,
			})
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatal(resp.StatusCode, string(out))
			}
			details := extractChatDetails(t, out)
			failStatus := tc.failStatus
			if failStatus == 0 {
				failStatus = http.StatusUnauthorized
			}
			origin.mu.Lock()
			origin.failN = origin.n + 1
			origin.failStatus = failStatus
			origin.mu.Unlock()
			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "auth.x.ai" {
					return originalTransport.RoundTrip(r)
				}
				if tc.mutate != nil {
					if err := tc.mutate(r.Context(), st, a.ID); err != nil {
						t.Errorf("mutate live row: %v", err)
					}
				}
				raw, _ := json.Marshal(map[string]any{
					"access_token": newAccess, "refresh_token": "refresh-rotated", "expires_in": 600,
				})
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(raw)),
					Header:     make(http.Header),
				}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			beforeOrigin, beforeOther := origin.count(), other.count()
			resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, replayBody("combo", details, "one", `[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]`, true))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("replay status %d body %s", resp.StatusCode, out)
			}
			if tc.mutate != nil {
				if origin.count() != beforeOrigin+1 || other.count() != beforeOther+1 {
					t.Fatalf("metadata change retried replay: origin=%d other=%d", origin.count()-beforeOrigin, other.count()-beforeOther)
				}
				if sent := other.lastBody(); strings.Contains(sent, "cipher-1") || strings.Contains(sent, grokbuild.XAIReplayFormat) {
					t.Fatalf("fallback forwarded ciphertext: %s", sent)
				}
				return
			}
			if origin.count() != beforeOrigin+2 || other.count() != beforeOther {
				t.Fatalf("ordinary refresh lost replay: origin=%d other=%d", origin.count()-beforeOrigin, other.count()-beforeOther)
			}
			last := origin.lastBody()
			if !strings.Contains(last, "cipher-1") || !strings.Contains(last, "fc_1") || strings.Contains(last, grokbuild.XAIReplayFormat) {
				t.Fatalf("refreshed retry did not preserve replay: %s", last)
			}
			origin.mu.Lock()
			lastAuth := origin.auth[len(origin.auth)-1]
			origin.mu.Unlock()
			if lastAuth != "Bearer "+newAccess {
				t.Fatalf("retry did not send refreshed credential: %q", lastAuth)
			}
		})
	}
}

func TestXAIChatReplayKeepsExactPriorInput(t *testing.T) {
	for _, stream := range []bool{true, false} {
		name := "unary"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			firstSSE := xaiSSEUsage(grokChatSSE("one", "cipher-1", "sum-1", false, "fc_1|call_1|read|{\"path\":\"a\"}"))
			secondSSE := xaiSSEUsage(grokChatSSE("two", "", "", false, "fc_2|call_2|list|{\"q\":\"b\"}"))
			cap := &xaiCapture{sse: firstSSE, jsonBody: xaiUnaryBridgeResponse()}
			if stream {
				cap.jsonBody = ""
			}
			up := httptest.NewServer(cap)
			t.Cleanup(up.Close)
			st := newTestStore(t)
			p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
			xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
			key := xaiAccessKey(t, st)
			ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
			firstBody := `{"model":"combo","reasoning_effort":"high","temperature":0.2,"top_p":0.9,"messages":[{"role":"user","content":"first"}]}`
			if stream {
				firstBody = `{"model":"combo","stream":true,"reasoning_effort":"high","temperature":0.2,"top_p":0.9,"messages":[{"role":"user","content":"first"}]}`
			}
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, firstBody)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("first status %d body %s", resp.StatusCode, out)
			}
			details := extractChatDetails(t, out)
			cap.mu.Lock()
			if stream {
				cap.sse = secondSSE
			} else {
				cap.jsonBody = xaiUnaryToolResponse()
			}
			cap.mu.Unlock()
			secondRequest := map[string]any{
				"model": "combo", "stream": stream, "reasoning_effort": "high",
				"temperature": 0.2, "top_p": 0.9,
				"messages": []any{
					map[string]any{"role": "user", "content": "first"},
					map[string]any{"role": "assistant", "content": "one", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"a"}`}}}, "reasoning_details": details},
					map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result-1"},
					map[string]any{"role": "user", "content": "second"},
				},
			}
			raw, _ := json.Marshal(secondRequest)
			resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, string(raw))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("second status %d body %s", resp.StatusCode, out)
			}
			secondUpstream := cap.lastBody()
			secondInput := xaiUpstreamInput(t, secondUpstream)
			secondConfig := xaiUpstreamConfig(t, secondUpstream)
			if read, write := xaiChatUsage(t, out); read != 9 || write != 3 {
				t.Fatalf("second cache usage read=%d write=%d body=%s", read, write, out)
			}
			secondRequest["messages"] = append(secondRequest["messages"].([]any),
				map[string]any{"role": "assistant", "content": "two", "tool_calls": []any{map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "list", "arguments": `{"q":"b"}`}}}},
				map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "result-2"},
				map[string]any{"role": "user", "content": "third"},
			)
			raw, _ = json.Marshal(secondRequest)
			resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, string(raw))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("third status %d body %s", resp.StatusCode, out)
			}
			thirdUpstream := cap.lastBody()
			thirdInput := xaiUpstreamInput(t, thirdUpstream)
			if len(thirdInput) < len(secondInput) || !reflect.DeepEqual(thirdInput[:len(secondInput)], secondInput) {
				t.Fatalf("prior upstream input changed:\nsecond=%s\nthird=%s", secondUpstream, thirdUpstream)
			}
			thirdConfig := xaiUpstreamConfig(t, thirdUpstream)
			for _, key := range []string{"model", "store", "include", "reasoning", "temperature", "top_p", "prompt_cache_key"} {
				if !reflect.DeepEqual(secondConfig[key], thirdConfig[key]) {
					t.Fatalf("config %s changed: second=%v third=%v", key, secondConfig[key], thirdConfig[key])
				}
			}
			for _, want := range []string{`"encrypted_content":"cipher-1"`, `"id":"fc_1"`, `"call_id":"call_1"`, `"call_id":"call_2"`, "result-2"} {
				if !strings.Contains(thirdUpstream, want) {
					t.Fatalf("third upstream missing %s: %s", want, thirdUpstream)
				}
			}
			if strings.Contains(thirdUpstream, "cipher-2") || strings.Contains(thirdUpstream, grokbuild.XAIReplayFormat) {
				t.Fatalf("third upstream leaked newer bridge state: %s", thirdUpstream)
			}
			if read, write := xaiChatUsage(t, out); read != 9 || write != 3 {
				t.Fatalf("third cache usage read=%d write=%d body=%s", read, write, out)
			}
		})
	}
}

func TestXAIChatUnsupportedUnaryOutputOmitsMarker(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"hosted tool", `{"type":"reasoning","id":"rs_u","status":"completed","encrypted_content":"cipher-u","summary":[{"type":"summary_text","text":"think"}]},` +
			`{"type":"message","id":"msg_u","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
			`{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"x"}}`},
		{"unsupported part", `{"type":"reasoning","id":"rs_u","status":"completed","encrypted_content":"cipher-u","summary":[{"type":"summary_text","text":"think"}]},` +
			`{"type":"message","id":"msg_u","role":"assistant","content":[{"type":"output_text","text":"hello"},{"type":"output_image","image_url":"https://example.test/x.png"}]}`},
		{"refusal", `{"type":"reasoning","id":"rs_u","status":"completed","encrypted_content":"cipher-u","summary":[{"type":"summary_text","text":"think"}]},` +
			`{"type":"message","id":"msg_u","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &xaiCapture{jsonBody: `{"id":"resp_u","model":"grok-4.7","status":"completed","output":[` + tc.output + `],"usage":{"input_tokens":14,"output_tokens":2,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":3}}}`}
			up := httptest.NewServer(cap)
			t.Cleanup(up.Close)
			st := newTestStore(t)
			p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
			xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
			key := xaiAccessKey(t, st)
			ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
			if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte("reasoning_details")) {
				t.Fatalf("unsupported unary status %d body %s", resp.StatusCode, out)
			}
			if tc.name != "refusal" && !bytes.Contains(out, []byte("hello")) {
				t.Fatalf("visible output lost: %s", out)
			}
			if read, write := xaiChatUsage(t, out); read != 9 || write != 3 {
				t.Fatalf("unary cache usage read=%d write=%d body=%s", read, write, out)
			}
		})
	}
}

func TestXAIChatUnsupportedStreamOutputOmitsMarker(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs_s","status":"completed","encrypted_content":"cipher-s","summary":[{"type":"summary_text","text":"think"}]}`
	message := `{"type":"message","id":"msg_s","role":"assistant","content":[{"type":"output_text","text":"hello"},{"type":"output_image","image_url":"https://example.test/x.png"}]}`
	hosted := `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"x"}}`
	sse := "data: " + `{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_s","status":"completed","output":[` + reasoning + `,` + message + `,` + hosted + `],"usage":{"input_tokens":14,"output_tokens":2,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":3}}}}` + "\n\n" +
		"data: [DONE]\n\n"
	cap := &xaiCapture{sse: sse}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte("reasoning_details")) || bytes.Contains(out, []byte(`"type":"error"`)) {
		t.Fatalf("unsupported stream status %d body %s", resp.StatusCode, out)
	}
	if !bytes.Contains(out, []byte("hello")) {
		t.Fatalf("visible stream output lost: %s", out)
	}
	if read, write := xaiChatUsage(t, out); read != 9 || write != 3 {
		t.Fatalf("stream cache usage read=%d write=%d body=%s", read, write, out)
	}
}

func TestXAIChatSilentAddedReasoningDoesNotBlock(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs_late","status":"completed","encrypted_content":"cipher-late","summary":[{"type":"summary_text","text":"think"}]}`
	tool := `{"type":"function_call","id":"fc_late","call_id":"call_late","name":"read","arguments":"{\"path\":\"a\"}","status":"completed"}`
	sse := "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_late","status":"in_progress","summary":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"","call_id":"","name":"read","arguments":""}}` + "\n\n" +
		"data: " + `{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":"}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","output_index":1,"item":` + tool + `}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_late","status":"completed","output":[` + reasoning + `,` + tool + `],"usage":{"input_tokens":4,"output_tokens":5}}}` + "\n\n" +
		"data: [DONE]\n\n"
	cap := &xaiCapture{sse: sse}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte(`"type":"error"`)) {
		t.Fatalf("late stream status %d body %s", resp.StatusCode, out)
	}
	raw := string(mustChatJSON(extractChatDetails(t, out)))
	if !strings.Contains(raw, "cipher-late") || !strings.Contains(raw, `"item":"fc_late"`) {
		t.Fatalf("late authoritative state was lost: %s", raw)
	}
}

func TestXAIRequestHintIgnoresStandaloneRequestID(t *testing.T) {
	header := http.Header{}
	header.Set("X-Client-Request-Id", "request-1")
	if hint := xaiRequestHint(header, []byte(`{"model":"combo"}`)); hint != "" {
		t.Fatalf("standalone X-Client-Request-Id became a hint: %q", hint)
	}
	header.Set("X-Client-Request-Id", "request-2")
	if hint := xaiRequestHint(header, nil); hint != "" {
		t.Fatalf("successive request id invented a hint: %q", hint)
	}
	header.Set("X-Session-Id", "stable-session")
	if hint := xaiRequestHint(header, nil); hint != "stable-session" {
		t.Fatalf("stable session hint = %q", hint)
	}
	header.Del("X-Session-Id")
	header.Set("X-Session-Affinity", "affinity")
	if hint := xaiRequestHint(header, nil); hint != "affinity" {
		t.Fatalf("session affinity hint = %q", hint)
	}
	header.Set("X-Grok-Conv-Id", "conv")
	header.Set("X-Client-Request-Id", "volatile")
	if hint := xaiRequestHint(header, nil); hint != "conv" {
		t.Fatalf("explicit conversation hint = %q", hint)
	}
	if hint := xaiRequestHint(header, []byte(`{"prompt_cache_key":"prompt"}`)); hint != "prompt" {
		t.Fatalf("explicit prompt cache key = %q", hint)
	}
}

func TestXAIChatCacheIgnoresStandaloneRequestID(t *testing.T) {
	cap := &xaiCapture{sse: xaiPlainSSE("ok")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	post := func(requestID, session string) string {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Client-Request-Id", requestID)
		if session != "" {
			req.Header.Set("X-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		return cap.lastBody()
	}
	first := post("request-1", "")
	second := post("request-2", "")
	if strings.Contains(first, "prompt_cache_key") || strings.Contains(second, "prompt_cache_key") {
		t.Fatalf("standalone request id injected a cache key: %s %s", first, second)
	}
	first = post("request-3", "stable-session")
	second = post("request-4", "stable-session")
	var firstDoc, secondDoc map[string]any
	if json.Unmarshal([]byte(first), &firstDoc) != nil || json.Unmarshal([]byte(second), &secondDoc) != nil {
		t.Fatalf("cache bodies: %s %s", first, second)
	}
	firstKey, _ := firstDoc["prompt_cache_key"].(string)
	secondKey, _ := secondDoc["prompt_cache_key"].(string)
	if firstKey == "" || firstKey != secondKey || strings.Contains(first, "stable-session") || strings.Contains(first, "request-3") {
		t.Fatalf("stable session key unstable or leaked: %s %s", first, second)
	}
}

func TestXAIChatLiveCredentialMismatchDeclinesPinnedReplay(t *testing.T) {
	origin := &xaiCapture{sse: grokChatSSE("one", "cipher-1", "sum-1", false)}
	originSrv := httptest.NewServer(origin)
	t.Cleanup(originSrv.Close)
	other := &xaiCapture{sse: xaiPlainSSE("fallback")}
	otherSrv := httptest.NewServer(other)
	t.Cleanup(otherSrv.Close)
	st := newTestStore(t)
	a := addXAIChatProvider(t, st, "a", "gen-a", "user-a")
	b := addXAIChatProvider(t, st, "b", "gen-b", "user-b")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}, domain.ComboTarget{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiAuthRouteTransport{
		"Bearer " + a.OAuthCreds.AccessToken: originSrv.URL,
		"Bearer " + b.OAuthCreds.AccessToken: otherSrv.URL,
	})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	var once sync.Once
	testXAIReplayResolved = func(*domain.Provider) {
		once.Do(func() {
			live, err := st.GetProvider(context.Background(), a.ID)
			if err != nil {
				t.Errorf("load live: %v", err)
				return
			}
			// Same principal and generation, different credential bytes.
			live.OAuthCreds.AccessToken = xaiJWTAccess("user-a") + "x"
			if err := st.UpdateProvider(context.Background(), live); err != nil {
				t.Errorf("update live: %v", err)
			}
		})
	}
	t.Cleanup(func() { testXAIReplayResolved = nil })
	beforeOrigin, beforeOther := origin.count(), other.count()
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key, replayBody("combo", details, "one", "", true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback status %d body %s", resp.StatusCode, out)
	}
	if origin.count() != beforeOrigin || other.count() != beforeOther+1 {
		t.Fatalf("credential mismatch sent replay: origin=%d other=%d", origin.count()-beforeOrigin, other.count()-beforeOther)
	}
	if sent := other.lastBody(); strings.Contains(sent, "cipher-1") {
		t.Fatalf("fallback forwarded ciphertext: %s", sent)
	}
}

func TestXAIChatStreamRefusalOmitsMarker(t *testing.T) {
	reasoning := `{"type":"reasoning","id":"rs_r","status":"completed","encrypted_content":"cipher-r","summary":[{"type":"summary_text","text":"think"}]}`
	message := `{"type":"message","id":"msg_r","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}`
	sse := "data: " + `{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}` + "\n\n" +
		"data: " + `{"type":"response.refusal.delta","output_index":1,"item_id":"msg_r","content_index":0,"delta":"no"}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_r","status":"completed","output":[` + reasoning + `,` + message + `],"usage":{"input_tokens":3,"output_tokens":2}}}` + "\n\n" +
		"data: [DONE]\n\n"
	cap := &xaiCapture{sse: sse}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addXAIChatProvider(t, st, "xai", "gen-1", "user-1")
	xaiCombo(t, st, domain.ComboTarget{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true})
	key := xaiAccessKey(t, st)
	ts := startChatProxyTransport(t, st, xaiRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte("reasoning_details")) || bytes.Contains(out, []byte(`"type":"error"`)) {
		t.Fatalf("refusal stream status %d body %s", resp.StatusCode, out)
	}
}
