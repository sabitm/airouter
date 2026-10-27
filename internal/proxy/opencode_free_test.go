package proxy

import (
	"bytes"
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
	"airouter/internal/harlog"
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
	sessionID        string
	sessionIDValues  []string
	request          string
	ua               string
	client           string
	auth             string
	anthropicVersion string
}

func (c *freeUpstreamCapture) record(r *http.Request, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.Path
	c.body = append([]byte(nil), body...)
	c.accept = r.Header.Get("Accept")
	c.session = r.Header.Get("x-opencode-session")
	c.sessionID = r.Header.Get(opencode.SessionIDHeader)
	c.sessionIDValues = append([]string(nil), r.Header.Values(opencode.SessionIDHeader)...)
	c.request = r.Header.Get("x-opencode-request")
	c.ua = r.Header.Get("User-Agent")
	c.client = r.Header.Get("x-opencode-client")
	c.auth = r.Header.Get("Authorization")
	c.anthropicVersion = r.Header.Get("anthropic-version")
}

// snapshot returns a lock-free copy for assertions after the request completes.
func (c *freeUpstreamCapture) snapshot() freeUpstreamSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return freeUpstreamSnapshot{
		path:             c.path,
		body:             append([]byte(nil), c.body...),
		accept:           c.accept,
		session:          c.session,
		sessionID:        c.sessionID,
		sessionIDValues:  append([]string(nil), c.sessionIDValues...),
		request:          c.request,
		ua:               c.ua,
		client:           c.client,
		auth:             c.auth,
		anthropicVersion: c.anthropicVersion,
	}
}

type freeUpstreamSnapshot struct {
	path             string
	body             []byte
	accept           string
	session          string
	sessionID        string
	sessionIDValues  []string
	request          string
	ua               string
	client           string
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

func setupOpencodeProviderMux(t *testing.T, model, key, tierBase string, handler http.HandlerFunc) (string, string, *http.ServeMux) {
	t.Helper()
	base, token, px := setupOpencodeProvider(t, model, key, tierBase, handler)
	mux := http.NewServeMux()
	px.Mount(mux)
	return base, token, mux
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
			if got.ua != opencode.UserAgent || got.client != opencode.DefaultClient || got.sessionID != "" || len(got.sessionIDValues) != 0 {
				t.Fatalf("paid identity changed: ua=%q client=%q sessionID=%q values=%v", got.ua, got.client, got.sessionID, got.sessionIDValues)
			}
			if !strings.Contains(string(out), "hello from openai") {
				t.Fatalf("unary response lost: %s", out)
			}
		})
	}
}

func TestOpencodeFreeIdentityAllBackends(t *testing.T) {
	nativeReq := "msg_0123456789abABCDEFGHIJKLMN"
	nativeSes := "ses_0123456789abABCDEFGHIJKLMN"
	otherSes := "ses_0123456789abOTHERSESSIONID"
	ingresses := []struct {
		name  string
		path  string
		body  string
		model string
	}{
		{name: "chat", path: "/v1/chat/completions", model: "big-pickle", body: `{"model":"default","messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses", path: "/v1/responses", model: "muse-spark-1.3-contributor-free", body: `{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{name: "messages", path: "/v1/messages", model: "minimax-m3-free", body: `{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, ing := range ingresses {
		for _, mode := range []string{"unary", "stream"} {
			t.Run(ing.name+"/"+mode+"/caller", func(t *testing.T) {
				var cap freeUpstreamCapture
				base, token, px := setupOpencodeProvider(t, ing.model, "real-zen-key", opencode.LegacyZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					cap.record(r, body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, freeTextSSE(ing.name))
				})
				body := ing.body
				if mode == "stream" {
					body = withStream(body)
				}
				req, _ := http.NewRequest(http.MethodPost, base+ing.path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("User-Agent", "opencode/1.2.3")
				req.Header.Set("x-opencode-client", "my-cli")
				req.Header.Set("x-opencode-request", nativeReq)
				req.Header.Set("x-session-id", "client-session")
				req.Header.Set(opencode.SessionIDHeader, otherSes)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				out, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d body=%s", resp.StatusCode, out)
				}
				got := cap.snapshot()
				if !opencode.IsNativeSessionID(got.session) || got.session == opencode.NativeSessionID("client-session") || got.session == otherSes {
					t.Fatalf("generic session was not proxy-mapped: %q", got.session)
				}
				if got.ua != "opencode/1.2.3" || got.client != "my-cli" || got.request != nativeReq || got.sessionID != got.session || len(got.sessionIDValues) != 1 {
					t.Fatalf("caller identity = ua=%q client=%q request=%q session=%q mirror=%q values=%v", got.ua, got.client, got.request, got.session, got.sessionID, got.sessionIDValues)
				}
				if got.session == otherSes {
					t.Fatal("mirror-only session became the conversation session")
				}
				if got.auth != "Bearer real-zen-key" {
					t.Fatalf("auth = %q", got.auth)
				}
				providers, err := px.store.ListProviders(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(providers) != 1 || providers[0].BaseURL != opencode.LegacyZenBaseURL {
					t.Fatalf("stored provider changed: %+v", providers)
				}
			})
			t.Run(ing.name+"/"+mode+"/sanitized", func(t *testing.T) {
				var cap freeUpstreamCapture
				_, token, mux := setupOpencodeProviderMux(t, ing.model, "public", opencode.ZenBaseURL, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					cap.record(r, body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, freeTextSSE(ing.name))
				})
				body := ing.body
				if mode == "stream" {
					body = withStream(body)
				}
				req := httptest.NewRequest(http.MethodPost, ing.path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("User-Agent", "curl/8.0")
				req.Header.Set("x-opencode-client", strings.Repeat("c", 300))
				req.Header.Set("x-opencode-session", nativeSes+"\r\nX-Injected: 1")
				req.Header.Add(opencode.SessionIDHeader, otherSes)
				req.Header.Add(opencode.SessionIDHeader, nativeSes+"\nX-Injected: 1")
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				got := cap.snapshot()
				if got.ua != opencode.FreeUserAgent || got.client != opencode.FreeClient {
					t.Fatalf("malformed identity not replaced: ua=%q client=%q", got.ua, got.client)
				}
				if !opencode.IsNativeSessionID(got.session) || got.session == nativeSes || strings.ContainsAny(got.session, "\r\n") {
					t.Fatalf("malformed session forwarded: %q", got.session)
				}
				if got.sessionID != got.session || len(got.sessionIDValues) != 1 || strings.ContainsAny(got.sessionID, "\r\n") {
					t.Fatalf("mirror drifted or injected: %q values=%v", got.sessionID, got.sessionIDValues)
				}
			})
		}
	}
}

func freeTextSSE(kind string) string {
	switch kind {
	case "responses":
		return freeResponsesTextSSE()
	case "messages":
		return freeAnthropicTextSSE()
	default:
		return freeChatTextSSE()
	}
}

// TestOpencodeFreeHeadersPreservePreparedBody proves header application does
// not mutate the prepared body or the bytes a transport reads. The expected
// snapshot is copied before headers run; it is not derived from captured output.
func TestOpencodeFreeHeadersPreservePreparedBody(t *testing.T) {
	nativeReq := "msg_0123456789abABCDEFGHIJKLMN"
	nativeSes := "ses_0123456789abABCDEFGHIJKLMN"
	cases := []struct {
		name     string
		path     string
		body     string
		caller   bool
		stream   bool
		wantSess string
	}{
		{name: "chat/unary/caller", path: opencode.ChatPath, body: freePreparedChatBody(false), caller: true, wantSess: nativeSes},
		{name: "chat/stream/caller", path: opencode.ChatPath, body: freePreparedChatBody(true), caller: true, stream: true, wantSess: nativeSes},
		{name: "responses/unary/caller", path: opencode.ResponsesPath, body: freePreparedResponsesBody(false), caller: true, wantSess: nativeSes},
		{name: "responses/stream/caller", path: opencode.ResponsesPath, body: freePreparedResponsesBody(true), caller: true, stream: true, wantSess: nativeSes},
		{name: "messages/unary/caller", path: opencode.MessagesPath, body: freePreparedMessagesBody(false), caller: true, wantSess: nativeSes},
		{name: "messages/stream/caller", path: opencode.MessagesPath, body: freePreparedMessagesBody(true), caller: true, stream: true, wantSess: nativeSes},
		{name: "chat/unary/fallback", path: opencode.ChatPath, body: freePreparedChatBody(false), stream: false},
		{name: "responses/stream/fallback", path: opencode.ResponsesPath, body: freePreparedResponsesBody(true), stream: true},
		{name: "messages/unary/fallback", path: opencode.MessagesPath, body: freePreparedMessagesBody(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared := []byte(tc.body)
			expected := append([]byte(nil), prepared...)
			provider := &domain.Provider{
				ID:       7,
				Name:     "zen",
				BaseURL:  opencode.ZenBaseURL,
				APIKey:   "real-zen-key",
				Protocol: domain.ProtocolOpencode,
			}
			trace := &TraceInfo{}
			ctx := WithTraceInfo(context.Background(), trace)
			clientHeaders := http.Header{}
			if tc.caller {
				clientHeaders.Set("User-Agent", "opencode/1.2.3")
				clientHeaders.Set("x-opencode-client", "my-cli")
				clientHeaders.Set("x-opencode-request", nativeReq)
				clientHeaders.Set("x-opencode-session", nativeSes)
				ctx = withOpencodeRequest(ctx, "nonce", clientHeaders, nil)
			} else {
				clientHeaders.Set("User-Agent", "curl/8.0")
				clientHeaders.Set("x-opencode-client", strings.Repeat("c", 300))
				clientHeaders.Set("x-opencode-session", nativeSes+"\r\nX-Injected: 1")
				ctx = withOpencodeRequest(ctx, "nonce", nil, nil)
			}
			ctx = withOpencodeSessions(ctx, opencode.NewSessionMap(8))
			prepareOpencodeSession(ctx, provider, prepared)
			if !bytes.Equal(prepared, expected) {
				t.Fatal("session resolution mutated the prepared body")
			}
			// The fixture is the post-preparation baseline. Confirming it here keeps
			// a later rewrite from being mistaken for header preservation.
			if again, _, err := opencode.PrepareFreeTierBody(prepared, freeCodecID(tc.path)); err != nil || !bytes.Equal(again, expected) {
				t.Fatalf("fixture is not an unchanged prepared body: err=%v", err)
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, opencode.ZenBaseURL+tc.path, bytes.NewReader(prepared))
			if err != nil {
				t.Fatal(err)
			}
			applyUpstreamHeaders(req, provider, clientHeaders, ctx, prepared)
			if !bytes.Equal(prepared, expected) {
				t.Fatal("header application mutated the prepared body slice")
			}
			if req.Body == nil {
				t.Fatal("header application consumed req.Body")
			}
			gotReqBody, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotReqBody, expected) {
				t.Fatalf("req.Body after headers = %q, want exact prepared snapshot", gotReqBody)
			}

			var cap freeUpstreamCapture
			px := New(nil, nil)
			px.client = &http.Client{Transport: freeBodyCaptureTransport(&cap)}
			px.streamClient = px.client
			if tc.stream {
				resp, err := px.forwardStream(ctx, provider, tc.path, prepared, clientHeaders, "")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			} else if _, _, err := px.forward(ctx, provider, tc.path, prepared, clientHeaders); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(prepared, expected) {
				t.Fatal("forwarding mutated the prepared body slice")
			}
			wire := cap.snapshot()
			if !bytes.Equal(wire.body, expected) {
				t.Fatalf("wire body differs from prepared snapshot:\n got %q\nwant %q", wire.body, expected)
			}
			if tc.stream && wire.accept != "text/event-stream" {
				t.Fatalf("stream Accept = %q", wire.accept)
			}
			if !tc.stream && wire.accept == "text/event-stream" {
				t.Fatalf("unary Accept forced stream: %q", wire.accept)
			}
			if wire.auth != "Bearer real-zen-key" {
				t.Fatalf("auth = %q", wire.auth)
			}
			wantSession := tc.wantSess
			if wantSession == "" {
				wantSession = trace.OpencodeSessionID
			}
			if !opencode.IsNativeSessionID(wantSession) || wire.session != wantSession || wire.sessionID != wantSession || len(wire.sessionIDValues) != 1 {
				t.Fatalf("session=%q mirror=%q values=%v want=%q", wire.session, wire.sessionID, wire.sessionIDValues, wantSession)
			}
			if tc.caller {
				if wire.ua != "opencode/1.2.3" || wire.client != "my-cli" || wire.request != nativeReq {
					t.Fatalf("caller identity lost: ua=%q client=%q request=%q", wire.ua, wire.client, wire.request)
				}
			} else if wire.ua != opencode.FreeUserAgent || wire.client != opencode.FreeClient || !opencode.IsNativeRequestID(wire.request) {
				t.Fatalf("fallback identity missing: ua=%q client=%q request=%q", wire.ua, wire.client, wire.request)
			}
			if strings.Contains(tc.path, "messages") && wire.anthropicVersion == "" {
				t.Fatal("messages request lost anthropic-version")
			}
		})
	}
}

func freeBodyCaptureTransport(cap *freeUpstreamCapture) roundTripFunc {
	return func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		cap.record(req, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	}
}

func freeCodecID(path string) string {
	switch path {
	case opencode.ResponsesPath:
		return "opencode-responses"
	case opencode.MessagesPath:
		return "opencode-messages"
	default:
		return "opencode-chat"
	}
}

func freePreparedChatBody(stream bool) string {
	body := "{\n" +
		"  \"model\": \"big-pickle\",\n" +
		"  \"messages\": [{\n" +
		"    \"role\": \"system\",\n" +
		"    \"content\": \"  keep   whitespace  \"\n" +
		"  }, {\n" +
		"    \"role\": \"assistant\",\n" +
		"    \"content\": null,\n" +
		"    \"tool_calls\": [{\n" +
		"      \"id\": \"call_1\",\n" +
		"      \"type\": \"function\",\n" +
		"      \"function\": {\"name\": \"bash\", \"arguments\": \"{\\\"limit\\\":1.50}\"}\n" +
		"    }]\n" +
		"  }, {\n" +
		"    \"role\": \"tool\",\n" +
		"    \"tool_call_id\": \"call_1\",\n" +
		"    \"content\": \"done\"\n" +
		"  }],\n" +
		"  \"tools\": [\n" +
		freeCanonicalNestedTools() +
		"  ],\n" +
		"  \"tool_choice\": {\"type\": \"function\", \"function\": {\"name\": \"bash\"}},\n" +
		"  \"max_tokens\": 128,\n" +
		"  \"reasoning_effort\": \"low\",\n" +
		"  \"prompt_cache_key\": \"cache key  with spaces\",\n" +
		"  \"stream\": false\n" +
		"}"
	if stream {
		return strings.Replace(body, `"stream": false`, `"stream": true,
  "stream_options": {"include_usage": true}`, 1)
	}
	return body
}

func freePreparedResponsesBody(stream bool) string {
	body := "{\n" +
		"  \"model\": \"muse-spark-1.3-contributor-free\",\n" +
		"  \"instructions\": \"  system   line  \",\n" +
		"  \"input\": [{\n" +
		"    \"type\": \"function_call\",\n" +
		"    \"call_id\": \"call_1\",\n" +
		"    \"name\": \"bash\",\n" +
		"    \"arguments\": \"{\\\"limit\\\":1.50}\"\n" +
		"  }, {\n" +
		"    \"type\": \"function_call_output\",\n" +
		"    \"call_id\": \"call_1\",\n" +
		"    \"output\": \"done\"\n" +
		"  }],\n" +
		"  \"tools\": [\n" +
		freeCanonicalFlatTools(`"parameters"`) +
		"  ],\n" +
		"  \"tool_choice\": {\"type\": \"function\", \"name\": \"bash\"},\n" +
		"  \"max_output_tokens\": 256,\n" +
		"  \"reasoning\": {\"effort\": \"medium\"},\n" +
		"  \"prompt_cache_key\": \"resp-cache\",\n" +
		"  \"stream\": false\n" +
		"}"
	if stream {
		return strings.Replace(body, `"stream": false`, `"stream": true`, 1)
	}
	return body
}

func freePreparedMessagesBody(stream bool) string {
	body := "{\n" +
		"  \"model\": \"minimax-m3-free\",\n" +
		"  \"system\": \"  keep   whitespace  \",\n" +
		"  \"max_tokens\": 128,\n" +
		"  \"messages\": [{\n" +
		"    \"role\": \"assistant\",\n" +
		"    \"content\": [{\"type\": \"tool_use\", \"id\": \"toolu_1\", \"name\": \"bash\", \"input\": {\"limit\": 1.50}}]\n" +
		"  }, {\n" +
		"    \"role\": \"user\",\n" +
		"    \"content\": [{\"type\": \"tool_result\", \"tool_use_id\": \"toolu_1\", \"content\": \"done\"}]\n" +
		"  }],\n" +
		"  \"tools\": [\n" +
		freeCanonicalMessagesTools() +
		"  ],\n" +
		"  \"tool_choice\": {\"type\": \"tool\", \"name\": \"bash\"},\n" +
		"  \"metadata\": {\"user_id\": \"cache key  with spaces\"},\n" +
		"  \"thinking\": {\"type\": \"enabled\", \"budget_tokens\": 64},\n" +
		"  \"stream\": false\n" +
		"}"
	if stream {
		return strings.Replace(body, `"stream": false`, `"stream": true`, 1)
	}
	return body
}

func freeCanonicalNestedTools() string {
	return "    {\"type\": \"function\", \"function\": {\"name\": \"bash\", \"description\": \"run\", \"parameters\": {\"type\": \"object\", \"properties\": {\"limit\": {\"type\": \"number\", \"minimum\": 1.50}}}}},\n" +
		"    {\"type\": \"function\", \"function\": {\"name\": \"glob\", \"description\": \"find paths\", \"parameters\": {\"type\": \"object\", \"properties\": {}}}},\n" +
		"    {\"type\": \"function\", \"function\": {\"name\": \"grep\", \"description\": \"search\", \"parameters\": {\"type\": \"object\", \"properties\": {}}}},\n" +
		"    {\"type\": \"function\", \"function\": {\"name\": \"read\", \"description\": \"read file\", \"parameters\": {\"type\": \"object\", \"properties\": {}}}}"
}

func freeCanonicalFlatTools(schemaKey string) string {
	return "    {\"type\": \"function\", \"name\": \"bash\", \"description\": \"run\", " + schemaKey + ": {\"type\": \"object\", \"properties\": {\"limit\": {\"type\": \"number\", \"minimum\": 1.50}}}},\n" +
		"    {\"type\": \"function\", \"name\": \"glob\", \"description\": \"find paths\", " + schemaKey + ": {\"type\": \"object\", \"properties\": {}}},\n" +
		"    {\"type\": \"function\", \"name\": \"grep\", \"description\": \"search\", " + schemaKey + ": {\"type\": \"object\", \"properties\": {}}},\n" +
		"    {\"type\": \"function\", \"name\": \"read\", \"description\": \"read file\", " + schemaKey + ": {\"type\": \"object\", \"properties\": {}}}"
}

func freeCanonicalMessagesTools() string {
	return "    {\"name\": \"bash\", \"description\": \"run\", \"input_schema\": {\"type\": \"object\", \"properties\": {\"limit\": {\"type\": \"number\", \"minimum\": 1.50}}}},\n" +
		"    {\"name\": \"glob\", \"description\": \"find paths\", \"input_schema\": {\"type\": \"object\", \"properties\": {}}},\n" +
		"    {\"name\": \"grep\", \"description\": \"search\", \"input_schema\": {\"type\": \"object\", \"properties\": {}}},\n" +
		"    {\"name\": \"read\", \"description\": \"read file\", \"input_schema\": {\"type\": \"object\", \"properties\": {}}}"
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
	if cap.ua != opencode.FreeUserAgent {
		t.Fatalf("UA = %q, want %q", cap.ua, opencode.FreeUserAgent)
	}
	if cap.client != opencode.FreeClient {
		t.Fatalf("client = %q, want %q", cap.client, opencode.FreeClient)
	}
	if cap.sessionID != cap.session || len(cap.sessionIDValues) != 1 {
		t.Fatalf("session id mirror = %q values=%v, want %q", cap.sessionID, cap.sessionIDValues, cap.session)
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

// urlRecordTransport records each request URL before the wrapped transport
// rewrites it to the mock host, so tests observe the real wire destination.
type urlRecordTransport struct {
	mu   sync.Mutex
	urls []string
	next http.RoundTripper
}

func (t *urlRecordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.urls = append(t.urls, req.URL.String())
	t.mu.Unlock()
	return t.next.RoundTrip(req)
}

func (t *urlRecordTransport) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.urls...)
}

// TestOpencodeEndpointMigrationOnWire proves the stored legacy Zen base URL is
// rewritten to the current inference endpoint on the actual request, for every
// backend path and both client roles, while the provider row stays unchanged.
func TestOpencodeEndpointMigrationOnWire(t *testing.T) {
	backends := []struct {
		name         string
		ingress      string
		upstreamPath string
		model        string
		build        func(stream bool) string
		unaryBody    string
		streamBody   string
	}{
		{
			name:         "chat",
			ingress:      "/v1/chat/completions",
			upstreamPath: opencode.ChatPath,
			model:        "deepseek-v4-pro",
			build: func(stream bool) string {
				if stream {
					return `{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}`
				}
				return `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
			},
			unaryBody:  openaiUpstreamBody,
			streamBody: openaiSSE,
		},
		{
			name:         "responses",
			ingress:      "/v1/responses",
			upstreamPath: opencode.ResponsesPath,
			model:        "gpt-5.5",
			build: func(stream bool) string {
				if stream {
					return `{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`
				}
				return `{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
			},
			unaryBody:  responsesUpstreamBody,
			streamBody: responsesSSE,
		},
		{
			name:         "messages",
			ingress:      "/v1/messages",
			upstreamPath: opencode.MessagesPath,
			model:        "qwen3.6-plus",
			build: func(stream bool) string {
				if stream {
					return `{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":true}`
				}
				return `{"model":"default","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`
			},
			unaryBody:  anthropicUpstreamBody,
			streamBody: anthropicSSE,
		},
	}
	bases := []struct {
		name string
		base string
	}{
		{"new zen", opencode.ZenBaseURL},
		{"stored legacy zen", opencode.LegacyZenBaseURL},
	}
	for _, backend := range backends {
		for _, base := range bases {
			for _, mode := range []string{"unary", "stream"} {
				t.Run(backend.name+"/"+base.name+"/"+mode, func(t *testing.T) {
					stream := mode == "stream"
					response := backend.unaryBody
					contentType := "application/json"
					if stream {
						response = backend.streamBody
						contentType = "text/event-stream"
					}
					baseURL, token, px := setupOpencodeProvider(t, backend.model, "zen-key", base.base, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", contentType)
						_, _ = io.WriteString(w, response)
					})
					rec := &urlRecordTransport{next: px.client.Transport}
					px.client.Transport = rec
					px.streamClient.Transport = rec

					if stream {
						resp, out := postStream(t, baseURL+backend.ingress, token, backend.build(true))
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("status=%d body=%s", resp.StatusCode, out)
						}
					} else {
						resp, out := post(t, baseURL+backend.ingress, token, backend.build(false))
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("status=%d body=%s", resp.StatusCode, out)
						}
					}

					want := opencode.ZenBaseURL + backend.upstreamPath
					urls := rec.snapshot()
					if len(urls) != 1 || urls[0] != want {
						t.Fatalf("wire URLs = %v, want [%s]", urls, want)
					}

					providers, err := px.store.ListProviders(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if len(providers) != 1 || providers[0].BaseURL != base.base {
						t.Fatalf("stored provider changed: %+v", providers)
					}
				})
			}
		}
	}
}

// TestOpencodeEndpointAliasScopedToProtocol proves the legacy Zen alias applies
// only to OpenCode rows; other protocols and the Go/custom endpoints stay
// unchanged.
func TestOpencodeEndpointAliasScopedToProtocol(t *testing.T) {
	cases := []struct {
		name     string
		provider *domain.Provider
		path     string
		want     string
	}{
		{"legacy zen opencode", &domain.Provider{BaseURL: opencode.LegacyZenBaseURL, Protocol: domain.ProtocolOpencode}, opencode.ChatPath, opencode.ZenBaseURL + opencode.ChatPath},
		{"legacy zen other protocol", &domain.Provider{BaseURL: opencode.LegacyZenBaseURL, Protocol: domain.ProtocolOpenAI}, opencode.ChatPath, opencode.LegacyZenBaseURL + opencode.ChatPath},
		{"new zen opencode", &domain.Provider{BaseURL: opencode.ZenBaseURL, Protocol: domain.ProtocolOpencode}, opencode.ResponsesPath, opencode.ZenBaseURL + opencode.ResponsesPath},
		{"legacy go opencode", &domain.Provider{BaseURL: opencode.GoBaseURL, Protocol: domain.ProtocolOpencode}, opencode.MessagesPath, opencode.GoBaseURL + opencode.MessagesPath},
		{"custom zen path opencode", &domain.Provider{BaseURL: "https://example.test/zen/v1", Protocol: domain.ProtocolOpencode}, opencode.ChatPath, "https://example.test/zen/v1" + opencode.ChatPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamURL(tc.provider, tc.path); got != tc.want {
				t.Fatalf("upstreamURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOpencodeLegacyEndpointAliasInTraceAndHAR proves the resolved wire URL
// recorded in TraceInfo and HAR matches the migrated endpoint, not the stored
// legacy value, for both the unary and streaming clients.
func TestOpencodeLegacyEndpointAliasInTraceAndHAR(t *testing.T) {
	want := opencode.ZenBaseURL + opencode.ChatPath
	provider := &domain.Provider{Name: "zen", BaseURL: opencode.LegacyZenBaseURL, APIKey: opencode.PublicKey, Protocol: domain.ProtocolOpencode}
	for _, stream := range []bool{false, true} {
		name := "unary"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			rec := harlog.New("opencode-alias")
			trace := &TraceInfo{RequestID: name, HAR: rec}
			ctx := WithTraceInfo(context.Background(), trace)
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
				}, nil
			})
			p := New(nil, nil)
			p.client = &http.Client{Transport: transport}
			p.streamClient = &http.Client{Transport: transport}
			if stream {
				resp, err := p.forwardStream(ctx, provider, opencode.ChatPath, []byte(`{}`), nil, "")
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			} else if _, _, err := p.forward(ctx, provider, opencode.ChatPath, []byte(`{}`), nil); err != nil {
				t.Fatal(err)
			}
			if trace.UpstreamURL != want {
				t.Fatalf("trace upstream URL = %q, want %q", trace.UpstreamURL, want)
			}
			data, err := rec.MarshalHAR()
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Log struct {
					Entries []struct {
						Request struct {
							URL string `json:"url"`
						} `json:"request"`
					} `json:"entries"`
				} `json:"log"`
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Log.Entries) != 1 || doc.Log.Entries[0].Request.URL != want {
				t.Fatalf("HAR entries = %+v, want URL %q", doc.Log.Entries, want)
			}
		})
	}
}

// TestOpencodeFreeTierClientSelection pins the upstream client choice: only
// official free-model inference POSTs take the impersonated-TLS client, and a
// test-replaced client always wins.
func TestOpencodeFreeTierClientSelection(t *testing.T) {
	px := New(nil, nil)
	freeURL := opencode.ZenBaseURL + opencode.ChatPath
	freeBody := []byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`)
	paidBody := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	zen := opencodeTestProvider()
	goProvider := opencodeTestProvider()
	goProvider.BaseURL = opencode.GoBaseURL
	legacy := opencodeTestProvider()
	legacy.BaseURL = opencode.LegacyZenBaseURL
	custom := &domain.Provider{Name: "mirror", BaseURL: "https://mirror.test/v1", Protocol: domain.ProtocolOpencode}

	if got := px.upstreamClient(zen, freeURL, freeBody, true); got != px.freeTierClient {
		t.Fatalf("official free stream client = %v, want freeTierClient", got)
	}
	if got := px.upstreamClient(zen, freeURL, freeBody, false); got != px.freeTierClient {
		t.Fatalf("official free unary client = %v, want freeTierClient", got)
	}
	// Legacy stored base aliases to the official endpoint at send time.
	if got := px.upstreamClient(legacy, opencode.ZenBaseURL+opencode.ChatPath, freeBody, true); got != px.freeTierClient {
		t.Fatalf("legacy free client = %v, want freeTierClient", got)
	}
	// Selection is scoped to the opencode protocol: another protocol stored on
	// the official URL keeps the standard client even for a free model.
	wrongProto := &domain.Provider{Name: "openai", BaseURL: opencode.ZenBaseURL, Protocol: domain.ProtocolOpenAI}
	if got := px.upstreamClient(wrongProto, freeURL, freeBody, true); got != px.streamClient {
		t.Fatalf("non-opencode protocol client = %v, want streamClient", got)
	}
	// A custom port on the official host is not the free endpoint.
	if got := px.upstreamClient(zen, "https://opencode.ai:8443"+opencode.ChatPath, freeBody, true); got != px.streamClient {
		t.Fatalf("custom port client = %v, want streamClient", got)
	}
	if got := px.upstreamClient(zen, freeURL, paidBody, true); got != px.streamClient {
		t.Fatalf("paid model client = %v, want streamClient", got)
	}
	if got := px.upstreamClient(goProvider, freeURL, freeBody, true); got != px.streamClient {
		t.Fatalf("paid endpoint client = %v, want streamClient", got)
	}
	if got := px.upstreamClient(custom, "https://mirror.test/v1/chat/completions", freeBody, true); got != px.streamClient {
		t.Fatalf("custom endpoint client = %v, want streamClient", got)
	}
	if got := px.upstreamClient(zen, "https://opencode.ai/zen/go/v1/chat/completions", freeBody, true); got != px.streamClient {
		t.Fatalf("paid path client = %v, want streamClient", got)
	}

	// A replaced streamClient wins verbatim: tests inject capture transports
	// and must never be routed to the real endpoint. The still-default unary
	// client keeps its own selection meanwhile.
	injected := &http.Client{}
	px.streamClient = injected
	if got := px.upstreamClient(zen, freeURL, freeBody, true); got != injected {
		t.Fatalf("injected stream client bypassed: %v", got)
	}
	if got := px.upstreamClient(zen, freeURL, freeBody, false); got != px.freeTierClient {
		t.Fatalf("unary client after stream injection = %v, want freeTierClient", got)
	}
	px.client = injected
	if got := px.upstreamClient(zen, freeURL, freeBody, false); got != injected {
		t.Fatalf("injected unary client bypassed: %v", got)
	}
}

// TestOpencodeFreeTierEnvironmentPreparation verifies the environment section
// lands in the prepared wire body for all three free codecs, is absent for
// paid rows, and stays idempotent when preparation runs twice.
func TestOpencodeFreeTierEnvironmentPreparation(t *testing.T) {
	px := New(nil, nil)
	p := opencodeTestProvider()
	bodies := []struct {
		codec codec
		in    string
	}{
		{backendCodec(domain.ProtocolOpencode, "big-pickle"), `{"model":"big-pickle","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
		{backendCodec(domain.ProtocolOpencode, "muse-spark-1.3-contributor-free"), `{"model":"muse-spark-1.3-contributor-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`},
		{backendCodec(domain.ProtocolOpencode, "minimax-m3-free"), `{"model":"minimax-m3-free","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range bodies {
		trace := &TraceInfo{}
		out, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), trace), tc.codec, p, []byte(tc.in))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "Here is some useful information about the environment you are running in:") {
			t.Fatalf("%s: environment section missing: %s", tc.codec.id, out)
		}
		// The wire escapes < and >, so count the marker in decoded strings.
		decoded := decodeJSONMap(t, out)
		if got := countJSONStrings(decoded, "<env>"); got != 1 {
			t.Fatalf("%s: env marker count = %d, want 1", tc.codec.id, got)
		}
		again, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), tc.codec, p, out)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(out) {
			t.Fatalf("%s: preparation is not idempotent", tc.codec.id)
		}
	}

	paidBody := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)
	out, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backendCodec(domain.ProtocolOpencode, "deepseek-v4-pro"), p, paidBody)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Here is some useful information about the environment you are running in:") {
		t.Fatalf("paid row carries environment section: %s", out)
	}
}

// TestOpencodeFreeTierPassthroughEnvironmentKept verifies a caller that
// already carries an environment block (real CLI passthrough) is forwarded
// without a second block. The fixture is built with json.Marshal because the
// block contains literal newlines, which raw JSON strings cannot hold.
func TestOpencodeFreeTierPassthroughEnvironmentKept(t *testing.T) {
	px := New(nil, nil)
	p := opencodeTestProvider()
	callerEnv := "Here is some useful information about the environment you are running in:\n<env>\n  Working directory: /caller\n</env>"
	in := mustJSON(map[string]any{
		"model": "big-pickle", "stream": true, "max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "system", "content": callerEnv},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out, err := px.prepareUpstreamRequest(WithTraceInfo(context.Background(), &TraceInfo{}), backendCodec(domain.ProtocolOpencode, "big-pickle"), p, in)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeJSONMap(t, out)
	if got := countJSONStrings(got, "<env>"); got != 1 {
		t.Fatalf("env marker count = %d, want 1", got)
	}
	msgs := got["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["content"] != callerEnv {
		t.Fatalf("caller environment block changed: %#v", sys["content"])
	}
	if msgs[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("user message changed: %#v", msgs[1])
	}
}

// countJSONStrings counts needle across every decoded JSON string so the wire
// escaping of < and > cannot hide occurrences.
func countJSONStrings(v any, needle string) int {
	switch x := v.(type) {
	case string:
		return strings.Count(x, needle)
	case []any:
		n := 0
		for _, item := range x {
			n += countJSONStrings(item, needle)
		}
		return n
	case map[string]any:
		n := 0
		for _, value := range x {
			n += countJSONStrings(value, needle)
		}
		return n
	default:
		return 0
	}
}
