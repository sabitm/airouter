package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/opencode"
	"airouter/internal/proxy/sse"
)

func TestOpencodeFreeTierCodecSelection(t *testing.T) {
	cases := []struct {
		base, model, id string
		streamOnly      bool
	}{
		{opencode.ZenBaseURL, "big-pickle", "opencode-chat", true},
		{opencode.ZenBaseURL, "muse-spark-1.3-contributor-free", "opencode-responses", true},
		{opencode.ZenBaseURL, "minimax-m3-free", "opencode-messages", true},
		{opencode.ZenBaseURL, "qwen3.6-plus-free", "opencode-messages", true},
		{opencode.ZenBaseURL, "deepseek-v4-pro", "opencode-chat", false},
		{opencode.ZenBaseURL, "gpt-5", "opencode-responses", false},
		{opencode.GoBaseURL, "minimax-m3", "opencode-messages", false},
		{opencode.GoBaseURL, "ox-alpha-free", "opencode-chat", false},
		{"https://example.test/v1", "big-pickle", "opencode-chat", true},
	}
	for _, tc := range cases {
		c := backendCodecFor(domain.ProtocolOpencode, tc.base, tc.model)
		if c.id != tc.id || c.streamOnly != tc.streamOnly {
			t.Errorf("%s %s codec=%s streamOnly=%v, want %s %v", tc.base, tc.model, c.id, c.streamOnly, tc.id, tc.streamOnly)
		}
	}
	if opencodeChatCodec.streamOnly || opencodeResponsesCodec.streamOnly || opencodeMessagesCodec.streamOnly {
		t.Fatal("shared codec streamOnly was mutated")
	}
}

// freeToolArgs is the JSON argument object the free-tier upstream emits. The
// caller tool is declared as Bash, so restoration must produce Bash with this
// exact object, not a substring.
const freeToolArgs = `{"city":"SF"}`

type freeUpstreamCapture struct {
	mu               sync.Mutex
	path             string
	body             []byte
	accept           string
	session          string
	request          string
	ua               string
	auth             string
	anthropicVersion string
}

func (c *freeUpstreamCapture) record(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.body = body
	c.accept = r.Header.Get("Accept")
	c.session = r.Header.Get("x-opencode-session")
	c.request = r.Header.Get("x-opencode-request")
	c.ua = r.Header.Get("User-Agent")
	c.auth = r.Header.Get("Authorization")
	c.anthropicVersion = r.Header.Get("anthropic-version")
}

// snapshot returns a lock-free copy for assertions after the request completes.
func (c *freeUpstreamCapture) snapshot() freeUpstreamSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return freeUpstreamSnapshot{
		path:             c.path,
		body:             c.body,
		accept:           c.accept,
		session:          c.session,
		request:          c.request,
		ua:               c.ua,
		auth:             c.auth,
		anthropicVersion: c.anthropicVersion,
	}
}

type freeUpstreamSnapshot struct {
	path             string
	body             []byte
	accept           string
	session          string
	request          string
	ua               string
	auth             string
	anthropicVersion string
}

// setupOpencodeProvider wires one opencode provider whose BaseURL is the real
// tier URL. Traffic is redirected to the mock upstream through a transport so
// Tier(), not the mock host, decides the tier.
func setupOpencodeProvider(t *testing.T, model, key, tierBase string, handler http.HandlerFunc) (string, string, *Proxy) {
	t.Helper()
	if tierBase == "" {
		t.Fatal("tier base URL required")
	}
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "p", BaseURL: tierBase, APIKey: key, Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: model, Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	access, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	px := New(st, nil)
	redirect := redirectTransport(upstream.URL)
	px.client = &http.Client{Transport: redirect, Timeout: px.client.Timeout}
	px.streamClient = &http.Client{Transport: redirect}
	mux := http.NewServeMux()
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, access.Token, px
}

func redirectTransport(upstreamURL string) roundTripFunc {
	u, err := url.Parse(upstreamURL)
	if err != nil {
		panic(err)
	}
	return func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = u.Scheme
		req.URL.Host = u.Host
		return http.DefaultTransport.RoundTrip(req)
	}
}

func setupFailover(t *testing.T, url1, model1 string, proto1 domain.Protocol, url2, model2 string, proto2 domain.Protocol) (string, string) {
	t.Helper()
	st := newTestStore(t)
	p1 := &domain.Provider{Name: "bad", BaseURL: url1, APIKey: "public", Protocol: proto1}
	p2 := &domain.Provider{Name: "good", BaseURL: url2, APIKey: "k", Protocol: proto2}
	ctx := context.Background()
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: model1, Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: model2, Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	access, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, access.Token
}

func TestOpencodeFreeTierMatrix(t *testing.T) {
	chatTools := `[{"type":"function","function":{"name":"Bash","description":"caller-bash","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}},{"type":"function","function":{"name":"shell","description":"caller-shell","parameters":{"type":"object"}}}]`
	responsesTools := `[{"type":"function","name":"Bash","description":"caller-bash","parameters":{"type":"object","properties":{"city":{"type":"string"}}}},{"type":"function","name":"shell","description":"caller-shell","parameters":{"type":"object"}}]`
	messagesTools := `[{"name":"Bash","description":"caller-bash","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}},{"name":"shell","description":"caller-shell","input_schema":{"type":"object"}}]`
	ingresses := []struct {
		name   string
		path   string
		build  func(stream bool) string
		unary  func(t *testing.T, out []byte) parsedTool
		stream func(t *testing.T, out string) parsedTool
	}{
		{
			name: "chat",
			path: "/v1/chat/completions",
			build: func(stream bool) string {
				return `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":` + chatTools + `}`
			},
			unary:  parseUnaryChat,
			stream: parseStreamChat,
		},
		{
			name: "responses",
			path: "/v1/responses",
			build: func(stream bool) string {
				return `{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":` + responsesTools + `}`
			},
			unary:  parseUnaryResponses,
			stream: parseStreamResponses,
		},
		{
			name: "messages",
			path: "/v1/messages",
			build: func(stream bool) string {
				return `{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"tools":` + messagesTools + `}`
			},
			unary:  parseUnaryMessages,
			stream: parseStreamMessages,
		},
	}
	backends := []struct {
		name    string
		model   string
		codecID string
		sse     func() string
	}{
		{name: "chat-backend", model: "big-pickle", codecID: "opencode-chat", sse: func() string { return freeChatToolSSE("bash", freeToolArgs) }},
		{name: "responses-backend", model: "muse-spark-1.3-contributor-free", codecID: "opencode-responses", sse: func() string { return freeResponsesToolSSE("bash", freeToolArgs) }},
		{name: "messages-backend", model: "minimax-m3-free", codecID: "opencode-messages", sse: func() string { return freeAnthropicToolSSE("bash", freeToolArgs) }},
	}
	for _, backend := range backends {
		for _, ing := range ingresses {
			t.Run(ing.name+"-"+backend.name, func(t *testing.T) {
				var cap freeUpstreamCapture
				base, token, _ := setupOpencodeProvider(t, backend.model, "public", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					cap.record(r, body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, backend.sse())
				})

				resp, out := post(t, base+ing.path, token, ing.build(false))
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("unary status=%d body=%s", resp.StatusCode, out)
				}
				got := cap.snapshot()
				assertFreeUpstreamShape(t, got, backend.codecID, "Bearer public", true)
				pt := ing.unary(t, out)
				assertRestoredTool(t, pt, "unary")
				sresp, sout := postStream(t, base+ing.path, token, withStream(ing.build(true)))
				if sresp.StatusCode != http.StatusOK {
					t.Fatalf("stream status=%d body=%s", sresp.StatusCode, sout)
				}
				got = cap.snapshot()
				assertFreeUpstreamShape(t, got, backend.codecID, "Bearer public", true)
				pt = ing.stream(t, sout)
				assertRestoredTool(t, pt, "stream")
				assertStreamTerminated(t, ing.path, sout)
			})
		}
	}
}

func TestOpencodeFreeToolLessBackends(t *testing.T) {
	backends := []struct {
		name    string
		model   string
		codecID string
		sse     func() string
	}{
		{name: "chat", model: "big-pickle", codecID: "opencode-chat", sse: freeChatTextSSE},
		{name: "responses", model: "muse-spark-1.3-contributor-free", codecID: "opencode-responses", sse: freeResponsesTextSSE},
		{name: "messages", model: "minimax-m3-free", codecID: "opencode-messages", sse: freeAnthropicTextSSE},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			var cap freeUpstreamCapture
			base, token, _ := setupOpencodeProvider(t, backend.model, "public", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				cap.record(r, body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, backend.sse())
			})
			resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			got := cap.snapshot()
			assertFreeUpstreamShape(t, got, backend.codecID, "Bearer public", false)
			var root map[string]json.RawMessage
			if err := json.Unmarshal(got.body, &root); err != nil {
				t.Fatal(err)
			}
			var tools []json.RawMessage
			if err := json.Unmarshal(root["tools"], &tools); err != nil || len(tools) != 4 {
				t.Fatalf("decoy count = %d body=%s", len(tools), got.body)
			}
			if backend.codecID != "opencode-messages" {
				var choice string
				if err := json.Unmarshal(root["tool_choice"], &choice); err != nil || choice != "none" {
					t.Fatalf("tool_choice = %s body=%s", root["tool_choice"], got.body)
				}
			} else if len(root["tool_choice"]) != 0 {
				t.Fatalf("messages invented tool_choice: %s", got.body)
			}
			if !strings.Contains(string(out), "ok") {
				t.Fatalf("text missing: %s", out)
			}
		})
	}
}

func TestOpencodeFreeRealKeyStillCloaks(t *testing.T) {
	var cap freeUpstreamCapture
	base, token, _ := setupOpencodeProvider(t, "big-pickle", "real-zen-key", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(r, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, freeChatTextSSE())
	})
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	got := cap.snapshot()
	if got.auth != "Bearer real-zen-key" {
		t.Fatalf("auth = %q", got.auth)
	}
	assertFreeUpstreamShape(t, got, "opencode-chat", "Bearer real-zen-key", false)
}

func TestOpencodePaidTiersStayUnary(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		model string
		key   string
	}{
		{name: "paid-zen", base: opencode.ZenBaseURL, model: "deepseek-v4-pro", key: "zen-key"},
		{name: "go-free-id", base: opencode.GoBaseURL, model: "ox-alpha-free", key: "go-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap freeUpstreamCapture
			base, token, _ := setupOpencodeProvider(t, tc.model, tc.key, tc.base, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				cap.record(r, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, openaiUpstreamBody)
			})
			resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			got := cap.snapshot()
			if got.accept == "text/event-stream" {
				t.Fatalf("paid tier forced stream accept: %q", got.accept)
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(got.body, &root); err != nil {
				t.Fatal(err)
			}
			if raw := root["stream"]; len(raw) != 0 && string(raw) != "false" {
				t.Fatalf("paid tier forced stream: %s", got.body)
			}
			for _, name := range opencode.FingerprintTools {
				if strings.Contains(string(got.body), `"name":"`+name+`"`) {
					t.Fatalf("paid tier fingerprint tool %s injected: %s", name, got.body)
				}
			}
			if !strings.Contains(string(out), "hello from openai") {
				t.Fatalf("unary response lost: %s", out)
			}
		})
	}
}

// TestOpencodeCustomTierCloaksDefaultZen proves a custom base URL defaults to
// the zen tier: free models there still cloak and stream.
func TestOpencodeCustomTierCloaksDefaultZen(t *testing.T) {
	var cap freeUpstreamCapture
	base, token, _ := setupOpencodeProvider(t, "big-pickle", "public", "https://custom.example/v1", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(r, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, freeChatTextSSE())
	})
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	got := cap.snapshot()
	assertFreeUpstreamShape(t, got, "opencode-chat", "Bearer public", false)
}

func TestOpencodeFreeDecoyFailsOverBeforeCommit(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, freeChatToolSSE("bash", `{}`))
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	}))
	t.Cleanup(up2.Close)
	base, token := setupFailover(t, up1.URL, "big-pickle", domain.ProtocolOpencode, up2.URL, "m2", domain.ProtocolOpenAI)
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 || resp.StatusCode != http.StatusOK {
		t.Fatalf("n1=%d n2=%d status=%d body=%s", n1, n2, resp.StatusCode, out)
	}
	if strings.Contains(string(out), `"bash"`) {
		t.Fatalf("decoy leaked: %s", out)
	}
	if !strings.Contains(string(out), "hello from openai") {
		t.Fatalf("fallback text missing: %s", out)
	}
}

// TestOpencodeFreeDecoyStreamingFailsOver proves the pre-commit gate on the
// streaming path: the free target fails before any client byte, so the walk
// continues to the second target instead of surfacing the decoy.
func TestOpencodeFreeDecoyStreamingFailsOver(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, precommitDecoySSE("bash"))
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, openaiSSE)
	}))
	t.Cleanup(up2.Close)
	base, token := setupFailover(t, up1.URL, "big-pickle", domain.ProtocolOpencode, up2.URL, "m2", domain.ProtocolOpenAI)
	resp, out := postStream(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if strings.Contains(out, "bash") || strings.Contains(out, "call_bad") {
		t.Fatalf("decoy leaked: %s", out)
	}
	if !strings.Contains(out, "Hello ") || !strings.Contains(out, "[DONE]") {
		t.Fatalf("fallback stream incomplete: %s", out)
	}
}

// TestOpencodeFreeFailoverPaidNamesUntouched proves the request-local tool map
// dies with the free attempt: the winning paid target's lowercase tool name is
// not restored to the caller spelling.
func TestOpencodeFreeFailoverPaidNamesUntouched(t *testing.T) {
	zenUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, precommitDecoySSE("bash"))
	}))
	t.Cleanup(zenUp.Close)
	goUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, paidToolBody("read"))
	}))
	t.Cleanup(goUp.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "zen", BaseURL: opencode.ZenBaseURL, APIKey: "public", Protocol: domain.ProtocolOpencode}
	p2 := &domain.Provider{Name: "go", BaseURL: opencode.GoBaseURL, APIKey: "go-key", Protocol: domain.ProtocolOpencode}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "big-pickle", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "ox-alpha-free", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	access, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	px := New(st, nil)
	transport := routeByPath(zenUp.URL, goUp.URL)
	px.client = &http.Client{Transport: transport}
	px.streamClient = &http.Client{Transport: transport}
	mux := http.NewServeMux()
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	body := `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Read","description":"file","parameters":{"type":"object"}}}]}`
	resp, out := post(t, ts.URL+"/v1/chat/completions", access.Token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), `"name":"read"`) {
		t.Fatalf("paid tool name was restored to caller spelling: %s", out)
	}
	if strings.Contains(string(out), `"name":"Read"`) {
		t.Fatalf("caller spelling leaked from the free attempt map: %s", out)
	}
}

func TestOpencodeFreeCollisionIsTerminal(t *testing.T) {
	var hits int
	base, token, _ := setupOpencodeProvider(t, "big-pickle", "public", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, freeChatTextSSE())
	})
	body := `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"Bash","description":"a","parameters":{"type":"object"}}},{"type":"function","function":{"name":"bash","description":"b","parameters":{"type":"object"}}}]}`
	resp, out := post(t, base+"/v1/chat/completions", token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hits != 0 {
		t.Fatalf("collision still reached upstream %d times", hits)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil || envelope.Error.Type != "invalid_request_error" || !strings.Contains(envelope.Error.Message, "map to") {
		t.Fatalf("envelope = %s err=%v", out, err)
	}
}

func TestOpencodeFreeDecoyPostCommitTerminates(t *testing.T) {
	base, token, _ := setupOpencodeProvider(t, "big-pickle", "public", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n")
	})
	resp, out := postStream(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if strings.Contains(out, `"name":"bash"`) || strings.Contains(out, "call_1") || strings.Contains(out, "[DONE]") {
		t.Fatalf("decoy or clean finish leaked after commit: %s", out)
	}
	if !strings.Contains(out, "unavailable tool") {
		t.Fatalf("terminal error missing: %s", out)
	}
	if !strings.Contains(out, "error") {
		t.Fatalf("chat error envelope missing: %s", out)
	}
}

func TestCollectFreeAnthropicThinking(t *testing.T) {
	backend := backendCodecFor(domain.ProtocolOpencode, opencode.ZenBaseURL, "minimax-m3-free")
	resp, err := collectStreamResponse(strings.NewReader(freeAnthropicThinkingSSE()), backend, nil, "minimax-m3-free", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []ir.ContentBlock{
		{Type: ir.BlockReasoning, Text: "chain", AnthropicSignature: "sig-1"},
		{Type: ir.BlockReasoning, Text: "", AnthropicSignature: "sig-empty"},
		{Type: ir.BlockRedactedReasoning, RedactedData: "redacted-data"},
		{Type: ir.BlockText, Text: "ok"},
	}
	if len(resp.Content) != len(want) {
		t.Fatalf("content = %+v", resp.Content)
	}
	for i := range want {
		got := resp.Content[i]
		if got.Type != want[i].Type || got.Text != want[i].Text || got.AnthropicSignature != want[i].AnthropicSignature || got.RedactedData != want[i].RedactedData {
			t.Fatalf("block %d = %+v want %+v", i, got, want[i])
		}
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 3 || resp.Usage.CacheReadTokens != 2 || resp.Usage.CacheWriteTokens != 1 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	anthOut, err := anthropicCodec.encodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var anth struct {
		Content []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			Data      string `json:"data"`
			Text      string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(anthOut, &anth); err != nil {
		t.Fatal(err)
	}
	if len(anth.Content) != 4 ||
		anth.Content[0].Type != "thinking" || anth.Content[0].Thinking != "chain" || anth.Content[0].Signature != "sig-1" ||
		anth.Content[1].Type != "thinking" || anth.Content[1].Thinking != "" || anth.Content[1].Signature != "sig-empty" ||
		anth.Content[2].Type != "redacted_thinking" || anth.Content[2].Data != "redacted-data" ||
		anth.Content[3].Type != "text" || anth.Content[3].Text != "ok" {
		t.Fatalf("anthropic unary = %s", anthOut)
	}
	if anth.Usage.InputTokens+anth.Usage.CacheReadInputTokens+anth.Usage.CacheCreationInputTokens != 8 ||
		anth.Usage.CacheReadInputTokens != 2 || anth.Usage.CacheCreationInputTokens != 1 || anth.Usage.OutputTokens != 3 {
		t.Fatalf("anthropic usage = %+v", anth.Usage)
	}

	for _, enc := range []struct {
		name string
		enc  func(*ir.Response) ([]byte, error)
	}{
		{"openai", openaiCodec.encodeResponse},
		{"responses", responsesCodec.encodeResponse},
	} {
		out, err := enc.enc(resp)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "chain") || !strings.Contains(string(out), "ok") {
			t.Fatalf("%s lost readable text: %s", enc.name, out)
		}
		if strings.Contains(string(out), "sig-1") || strings.Contains(string(out), "sig-empty") || strings.Contains(string(out), "redacted-data") {
			t.Fatalf("%s mapped opaque thinking: %s", enc.name, out)
		}
	}
}

// TestOpencodeFreeMessagesThinkingUnary runs the signed/redacted collection
// end to end: Messages ingress, free messages backend, collected unary reply.
func TestOpencodeFreeMessagesThinkingUnary(t *testing.T) {
	var cap freeUpstreamCapture
	base, token, _ := setupOpencodeProvider(t, "minimax-m3-free", "public", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.record(r, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, freeAnthropicThinkingSSE())
	})
	resp, out := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	got := cap.snapshot()
	assertFreeUpstreamShape(t, got, "opencode-messages", "Bearer public", false)
	var gotResp struct {
		Content []struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
			Data      string `json:"data"`
			Text      string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			OutputTokens             int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &gotResp); err != nil {
		t.Fatal(err)
	}
	if len(gotResp.Content) != 4 ||
		gotResp.Content[0].Type != "thinking" || gotResp.Content[0].Thinking != "chain" || gotResp.Content[0].Signature != "sig-1" ||
		gotResp.Content[1].Type != "thinking" || gotResp.Content[1].Signature != "sig-empty" ||
		gotResp.Content[2].Type != "redacted_thinking" || gotResp.Content[2].Data != "redacted-data" ||
		gotResp.Content[3].Type != "text" || gotResp.Content[3].Text != "ok" {
		t.Fatalf("messages unary = %s", out)
	}
	// Anthropic reports the ordinary input partition plus cache subsets.
	gotIn := gotResp.Usage.InputTokens + gotResp.Usage.CacheReadInputTokens + gotResp.Usage.CacheCreationInputTokens
	if gotIn != 8 || gotResp.Usage.CacheReadInputTokens != 2 || gotResp.Usage.CacheCreationInputTokens != 1 || gotResp.Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", gotResp.Usage)
	}
}

// --- helpers ---

func assertFreeUpstreamShape(t *testing.T, cap freeUpstreamSnapshot, codecID, auth string, callerTools bool) {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(cap.body, &root); err != nil {
		t.Fatalf("upstream body not JSON: %v body=%s", err, cap.body)
	}
	var stream bool
	if err := json.Unmarshal(root["stream"], &stream); err != nil || !stream {
		t.Fatalf("upstream stream flag missing: %s", cap.body)
	}
	if cap.accept != "text/event-stream" {
		t.Fatalf("accept = %q", cap.accept)
	}
	if !opencode.IsNativeSessionID(cap.session) || !opencode.IsNativeRequestID(cap.request) {
		t.Fatalf("identity session=%q request=%q", cap.session, cap.request)
	}
	if cap.ua == "" {
		t.Fatal("UA missing")
	}
	if cap.auth != auth {
		t.Fatalf("auth = %q, want %q", cap.auth, auth)
	}
	if codecID == "opencode-messages" && cap.anthropicVersion == "" {
		t.Fatalf("anthropic-version missing for messages backend: %v", cap)
	}
	if codecID == "opencode-chat" {
		var opts map[string]json.RawMessage
		if json.Unmarshal(root["stream_options"], &opts) != nil {
			t.Fatalf("stream_options missing: %s", cap.body)
		}
		var include bool
		if json.Unmarshal(opts["include_usage"], &include) != nil || !include {
			t.Fatalf("include_usage missing: %s", cap.body)
		}
	}

	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(root["tools"], &tools); err != nil {
		t.Fatalf("tools missing: %s", cap.body)
	}
	type toolInfo struct {
		desc   string
		schema json.RawMessage
		typ    string
	}
	byName := map[string]toolInfo{}
	for _, tool := range tools {
		info := toolInfo{}
		var name string
		if codecID == "opencode-chat" {
			var fn map[string]json.RawMessage
			if json.Unmarshal(tool["function"], &fn) != nil {
				t.Fatalf("chat tool without function: %s", tool)
			}
			_ = json.Unmarshal(fn["name"], &name)
			_ = json.Unmarshal(fn["description"], &info.desc)
			info.schema = fn["parameters"]
		} else {
			_ = json.Unmarshal(tool["name"], &name)
			_ = json.Unmarshal(tool["description"], &info.desc)
			info.typ = string(tool["type"])
			if codecID == "opencode-responses" {
				info.schema = tool["parameters"]
			} else {
				info.schema = tool["input_schema"]
			}
		}
		byName[name] = info
	}
	if callerTools {
		if byName["bash"].desc != "caller-bash" || !strings.Contains(string(byName["bash"].schema), "city") {
			t.Fatalf("bash tool not canonicalized with schema: %+v %s", byName["bash"], cap.body)
		}
		if byName["shell"].desc != "caller-shell" {
			t.Fatalf("caller shell changed: %+v", byName["shell"])
		}
	}
	for _, name := range opencode.FingerprintTools {
		if callerTools && name == "bash" {
			continue
		}
		info := byName[name]
		if info.desc != decoyDescription() || len(info.schema) == 0 {
			t.Fatalf("decoy %s malformed: %+v %s", name, info, cap.body)
		}
		switch {
		case codecID == "opencode-responses" && info.typ != `"function"`:
			t.Fatalf("responses decoy %s missing type:function: %s", name, info.typ)
		case codecID == "opencode-messages" && info.typ != "":
			t.Fatalf("messages decoy %s gained a type: %s", name, info.typ)
		}
	}
}

func assertRestoredTool(t *testing.T, pt parsedTool, what string) {
	t.Helper()
	if pt.text != "ok" {
		t.Fatalf("%s text = %q", what, pt.text)
	}
	if pt.name != "Bash" {
		t.Fatalf("%s tool name = %q, want restored Bash", what, pt.name)
	}
	if compactJSONValue(t, pt.args) != compactJSONValue(t, freeToolArgs) {
		t.Fatalf("%s tool args = %s, want %s", what, pt.args, freeToolArgs)
	}
	if pt.inputTok == 0 || pt.outputTok == 0 {
		t.Fatalf("%s usage missing: %+v", what, pt)
	}
}

func compactJSONValue(t *testing.T, raw string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("args not JSON: %q", raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertStreamTerminated(t *testing.T, path, body string) {
	t.Helper()
	switch path {
	case "/v1/chat/completions":
		if !strings.Contains(body, "[DONE]") {
			t.Fatalf("chat stream not terminated: %s", body)
		}
	case "/v1/responses":
		if !strings.Contains(body, "response.completed") {
			t.Fatalf("responses stream not terminated: %s", body)
		}
	case "/v1/messages":
		if !strings.Contains(body, "message_stop") {
			t.Fatalf("messages stream not terminated: %s", body)
		}
	}
}

func routeByPath(zenURL, goURL string) roundTripFunc {
	zu, _ := url.Parse(zenURL)
	gu, _ := url.Parse(goURL)
	return func(req *http.Request) (*http.Response, error) {
		target := zu
		if strings.Contains(req.URL.Path, "/zen/go/") {
			target = gu
		}
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		return http.DefaultTransport.RoundTrip(req)
	}
}

func withStream(body string) string {
	if strings.Contains(body, `"stream"`) {
		return body
	}
	return strings.Replace(body, "{", `{"stream":true,`, 1)
}

func decoyDescription() string {
	return "This tool is currently unavailable and must not be used."
}

type parsedTool struct {
	text      string
	name      string
	args      string
	inputTok  int
	outputTok int
}

func parseUnaryChat(t *testing.T, out []byte) parsedTool {
	t.Helper()
	var resp struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("chat unary decode: %v body=%s", err, out)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("chat unary choices: %s", out)
	}
	pt := parsedTool{inputTok: resp.Usage.PromptTokens, outputTok: resp.Usage.CompletionTokens}
	if resp.Choices[0].Message.Content != nil {
		pt.text = *resp.Choices[0].Message.Content
	}
	if len(resp.Choices[0].Message.ToolCalls) > 0 {
		pt.name = resp.Choices[0].Message.ToolCalls[0].Function.Name
		pt.args = resp.Choices[0].Message.ToolCalls[0].Function.Arguments
	}
	return pt
}

func parseUnaryResponses(t *testing.T, out []byte) parsedTool {
	t.Helper()
	var resp struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("responses unary decode: %v body=%s", err, out)
	}
	pt := parsedTool{inputTok: resp.Usage.InputTokens, outputTok: resp.Usage.OutputTokens}
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				pt.text += c.Text
			}
		case "function_call":
			pt.name = item.Name
			pt.args = item.Arguments
		}
	}
	return pt
}

func parseUnaryMessages(t *testing.T, out []byte) parsedTool {
	t.Helper()
	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("messages unary decode: %v body=%s", err, out)
	}
	pt := parsedTool{inputTok: resp.Usage.InputTokens, outputTok: resp.Usage.OutputTokens}
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			pt.text += block.Text
		case "tool_use":
			pt.name = block.Name
			if len(block.Input) > 0 {
				pt.args = string(block.Input)
			}
		}
	}
	return pt
}

func parseStreamChat(t *testing.T, body string) parsedTool {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	pt := parsedTool{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(ev.Data, &chunk) != nil {
			continue
		}
		for _, c := range chunk.Choices {
			pt.text += c.Delta.Content
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" {
					pt.name = tc.Function.Name
				}
				pt.args += tc.Function.Arguments
			}
		}
		if chunk.Usage != nil {
			pt.inputTok = chunk.Usage.PromptTokens
			pt.outputTok = chunk.Usage.CompletionTokens
		}
	}
	return pt
}

func parseStreamResponses(t *testing.T, body string) parsedTool {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	pt := parsedTool{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "response.output_text.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				pt.text += d.Delta
			}
		case "response.output_item.added":
			var d struct {
				Item struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"item"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Item.Type == "function_call" {
				pt.name = d.Item.Name
			}
		case "response.function_call_arguments.delta":
			var d struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				pt.args += d.Delta
			}
		case "response.completed":
			var d struct {
				Response struct {
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				pt.inputTok = d.Response.Usage.InputTokens
				pt.outputTok = d.Response.Usage.OutputTokens
			}
		}
	}
	return pt
}

func parseStreamMessages(t *testing.T, body string) parsedTool {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	pt := parsedTool{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "content_block_start":
			var d struct {
				ContentBlock struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.ContentBlock.Type == "tool_use" {
				pt.name = d.ContentBlock.Name
			}
		case "content_block_delta":
			var d struct {
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			switch d.Delta.Type {
			case "text_delta":
				pt.text += d.Delta.Text
			case "input_json_delta":
				pt.args += d.Delta.PartialJSON
			}
		case "message_start":
			var d struct {
				Message struct {
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				pt.inputTok = d.Message.Usage.InputTokens + d.Message.Usage.CacheReadInputTokens + d.Message.Usage.CacheCreationInputTokens
			}
		case "message_delta":
			// message_start already went out when input arrived late, so the
			// terminal delta carries the corrected partition.
			var d struct {
				Usage struct {
					InputTokens              *int `json:"input_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
					OutputTokens             int  `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				if d.Usage.InputTokens != nil {
					pt.inputTok = *d.Usage.InputTokens
				}
				if d.Usage.CacheReadInputTokens != nil {
					pt.inputTok += *d.Usage.CacheReadInputTokens
				}
				if d.Usage.CacheCreationInputTokens != nil {
					pt.inputTok += *d.Usage.CacheCreationInputTokens
				}
				pt.outputTok = d.Usage.OutputTokens
			}
		}
	}
	return pt
}

// --- SSE fixtures ---

func joinSSE(events ...string) string {
	return strings.Join(events, "\n\n") + "\n\n"
}

func freeChatTextSSE() string {
	return joinSSE(
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		`data: [DONE]`,
	)
}

func freeChatToolSSE(name, args string) string {
	return joinSSE(
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+jsonString(args[:2])+`}}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":`+jsonString(name)+`,"arguments":`+jsonString(args[2:])+`}}]}}]}`,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
		`data: [DONE]`,
	)
}

func freeResponsesTextSSE() string {
	return joinSSE(
		"event: response.created\ndata: "+`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		"event: response.output_text.delta\ndata: "+`{"type":"response.output_text.delta","delta":"ok"}`,
		"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`,
	)
}

func freeResponsesToolSSE(name, args string) string {
	return joinSSE(
		"event: response.created\ndata: "+`{"type":"response.created","response":{"id":"resp_1","model":"m"}}`,
		"event: response.output_text.delta\ndata: "+`{"type":"response.output_text.delta","delta":"ok"}`,
		"event: response.function_call_arguments.delta\ndata: "+`{"type":"response.function_call_arguments.delta","output_index":1,"delta":`+jsonString(args[:2])+`}`,
		"event: response.output_item.added\ndata: "+`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":`+jsonString(name)+`}}`,
		"event: response.function_call_arguments.delta\ndata: "+`{"type":"response.function_call_arguments.delta","output_index":1,"delta":`+jsonString(args[2:])+`}`,
		"event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":4,"output_tokens":2},"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]},{"type":"function_call","call_id":"call_1","name":`+jsonString(name)+`,"arguments":`+jsonString(args)+`}]}}`,
	)
}

func freeAnthropicTextSSE() string {
	return joinSSE(
		"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","usage":{"input_tokens":3,"output_tokens":0}}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		"event: message_stop\ndata: "+`{"type":"message_stop"}`,
	)
}

func freeAnthropicToolSSE(name, args string) string {
	return joinSSE(
		"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","usage":{"input_tokens":4,"output_tokens":0}}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
		// Arguments arrive before the tool identity so the stream filter must hold them.
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+jsonString(args[:2])+`}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":`+jsonString(name)+`}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+jsonString(args[2:])+`}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":1}`,
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`,
		"event: message_stop\ndata: "+`{"type":"message_stop"}`,
	)
}

func freeAnthropicThinkingSSE() string {
	return joinSSE(
		"event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","usage":{"input_tokens":5,"cache_read_input_tokens":2,"cache_creation_input_tokens":1,"output_tokens":0}}}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"chain"}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`,
		// A signed thinking block with no readable text must survive collection.
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-empty"}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":1}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":2,"content_block":{"type":"redacted_thinking","data":"redacted-data"}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":2}`,
		"event: content_block_start\ndata: "+`{"type":"content_block_start","index":3,"content_block":{"type":"text","text":""}}`,
		"event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":3,"delta":{"type":"text_delta","text":"ok"}}`,
		"event: content_block_stop\ndata: "+`{"type":"content_block_stop","index":3}`,
		"event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"event: message_stop\ndata: "+`{"type":"message_stop"}`,
	)
}

// precommitDecoySSE holds no client-visible output before the decoy tool call.
func precommitDecoySSE(name string) string {
	return joinSSE(
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_bad","function":{"name":`+jsonString(name)+`,"arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: [DONE]`,
	)
}

func paidToolBody(name string) string {
	return `{"id":"chatcmpl-paid","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":` + jsonString(name) + `,"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
