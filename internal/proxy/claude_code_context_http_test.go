package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

type claudeSelectorWire struct {
	model     string
	beta      string
	thinking  string
	budget    int
	effort    string
	maxTokens int
	body      string
}

func captureClaudeSelector(r *http.Request) claudeSelectorWire {
	body, _ := io.ReadAll(r.Body)
	var doc struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Thinking  *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		OutputConfig *struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	_ = json.Unmarshal(body, &doc)
	wire := claudeSelectorWire{
		model:     doc.Model,
		maxTokens: doc.MaxTokens,
		beta:      r.Header.Get("anthropic-beta"),
		body:      string(body),
	}
	if doc.Thinking != nil {
		wire.thinking = doc.Thinking.Type
		wire.budget = doc.Thinking.BudgetTokens
	}
	if doc.OutputConfig != nil {
		wire.effort = doc.OutputConfig.Effort
	}
	return wire
}

type selectorCapture struct {
	ch       chan claudeSelectorWire
	overflow atomic.Int32
}

func newSelectorCapture(capacity int) *selectorCapture {
	return &selectorCapture{ch: make(chan claudeSelectorWire, capacity)}
}

// record never blocks the upstream handler. An unexpected extra attempt is
// counted so cleanup cannot deadlock on a full channel.
func (c *selectorCapture) record(wire claudeSelectorWire) {
	select {
	case c.ch <- wire:
	default:
		c.overflow.Add(1)
	}
}

func (c *selectorCapture) next(t *testing.T) claudeSelectorWire {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case wire := <-c.ch:
		return wire
	case <-timer.C:
		t.Fatal("timed out waiting for upstream capture")
	case <-t.Context().Done():
		t.Fatalf("test context ended waiting for upstream capture: %v", t.Context().Err())
	}
	return claudeSelectorWire{}
}

func (c *selectorCapture) assertNoMore(t *testing.T) {
	t.Helper()
	if n := c.overflow.Load(); n != 0 {
		t.Fatalf("unexpected extra upstream request(s): %d", n)
	}
	select {
	case wire := <-c.ch:
		t.Fatalf("unexpected extra upstream capture model=%q", wire.model)
	default:
	}
}

// selectorTransport accepts only exact method/scheme/host/path/query keys.
// Redirect targets pass through this same check, so a redirect cannot reach a
// destination absent from the fixture allowlist.
type selectorTransport struct {
	next    http.RoundTripper
	allowed map[string]struct{}
}

func selectorDestination(method, scheme, host, path, rawQuery string) string {
	if path == "" {
		path = "/"
	}
	return method + " " + scheme + "://" + host + path + "?" + rawQuery
}

func selectorKey(method, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return selectorDestination(strings.ToUpper(method), u.Scheme, u.Host, u.EscapedPath(), u.RawQuery), nil
}

func (tr *selectorTransport) allow(t *testing.T, method, rawURL string) {
	t.Helper()
	key, err := selectorKey(method, rawURL)
	if err != nil {
		t.Fatalf("allow %s: %v", rawURL, err)
	}
	tr.allowed[key] = struct{}{}
}

func (tr *selectorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := selectorDestination(req.Method, req.URL.Scheme, req.URL.Host, req.URL.EscapedPath(), req.URL.RawQuery)
	if _, ok := tr.allowed[key]; !ok {
		return nil, fmt.Errorf("selector transport rejected destination %s", key)
	}
	return tr.next.RoundTrip(req)
}

// installSelectorTransport saves the process default because the OAuth service
// client has a nil Transport. Tests in this package do not run in parallel, and
// cleanup restores the default after servers registered later are closed.
func installSelectorTransport(t *testing.T) *selectorTransport {
	t.Helper()
	original := http.DefaultTransport
	tr := &selectorTransport{next: original, allowed: map[string]struct{}{}}
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = original })
	return tr
}

func selectorClient(tr *selectorTransport) *http.Client {
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func selectorBackendPath(t *testing.T, protocol domain.Protocol) string {
	t.Helper()
	switch protocol {
	case domain.ProtocolClaudeCode:
		return claudecode.UpstreamPath
	case domain.ProtocolAnthropic:
		return "/messages"
	case domain.ProtocolOpenAI:
		return "/chat/completions"
	case domain.ProtocolOpenAIResponses:
		return "/responses"
	default:
		t.Fatalf("unsupported selector fixture protocol %q", protocol)
		return ""
	}
}

type selectorTarget struct {
	protocol domain.Protocol
	baseURL  string
	model    string
	oauth    *domain.OAuthCreds
}

func newSelectorProxy(t *testing.T, tr *selectorTransport, targets ...selectorTarget) (string, string, *Proxy) {
	t.Helper()
	st := newTestStore(t)
	comboTargets := make([]domain.ComboTarget, 0, len(targets))
	for i, target := range targets {
		prov := &domain.Provider{
			Name:     fmt.Sprintf("p%d", i),
			BaseURL:  target.baseURL,
			Protocol: target.protocol,
		}
		if target.oauth != nil {
			prov.AuthMethod = domain.AuthOAuth
			prov.OAuthCreds = target.oauth
		} else {
			prov.APIKey = "up-key"
		}
		if err := st.CreateProvider(t.Context(), prov); err != nil {
			t.Fatal(err)
		}
		comboTargets = append(comboTargets, domain.ComboTarget{ProviderID: prov.ID, UpstreamModel: target.model, Enabled: true})
		tr.allow(t, http.MethodPost, target.baseURL+selectorBackendPath(t, target.protocol))
	}
	if err := st.CreateCombo(t.Context(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: comboTargets}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(t.Context(), "client")
	if err != nil {
		t.Fatal(err)
	}
	px := New(st, nil)
	px.client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	px.streamClient = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	mux := http.NewServeMux()
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	// Only the exercised client-to-proxy POST ingress endpoints are allowed.
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		tr.allow(t, http.MethodPost, ts.URL+path)
	}
	return ts.URL, key.Token, px
}

func postSelector(t *testing.T, client *http.Client, url, token, body string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func betaFlagCount(header, flag string) int {
	count := 0
	for _, part := range strings.Split(header, ",") {
		if strings.TrimSpace(part) == flag {
			count++
		}
	}
	return count
}

func assertBetaFlagCount(t *testing.T, header, flag string, want int) {
	t.Helper()
	if got := betaFlagCount(header, flag); got != want {
		t.Fatalf("%s count = %d want %d; beta=%s", flag, got, want, header)
	}
}

func assertClaudeHighWire(t *testing.T, wire claudeSelectorWire, wantModel string, wantLongContext bool) {
	t.Helper()
	if wire.model != wantModel {
		t.Fatalf("wire model = %q want %q; body=%s", wire.model, wantModel, wire.body)
	}
	if wire.thinking != "adaptive" || wire.effort != "high" {
		t.Fatalf("thinking = %q effort = %q; body=%s", wire.thinking, wire.effort, wire.body)
	}
	if strings.Contains(wire.body, `"budget_tokens"`) {
		t.Fatalf("budget thinking survived adaptive intent: %s", wire.body)
	}
	if wire.maxTokens != 32 {
		t.Fatalf("max_tokens = %d want 32; body=%s", wire.maxTokens, wire.body)
	}
	assertBetaFlagCount(t, wire.beta, claudecode.BetaLongContext, boolCount(wantLongContext))
	assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 1)
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

type selectorUsage struct {
	in         int
	out        int
	total      int
	cacheRead  int
	cacheWrite int
}

func selectorUsageFor(path string, stream bool) selectorUsage {
	if stream {
		return selectorUsage{in: 1, out: 2, total: 3}
	}
	if path == "/v1/messages" {
		return selectorUsage{in: 3, out: 4, total: 13, cacheWrite: 10}
	}
	return selectorUsage{in: 13, out: 4, total: 17, cacheWrite: 10}
}

type selectorUnaryUsage struct {
	Usage struct {
		InputTokens              int                   `json:"input_tokens"`
		OutputTokens             int                   `json:"output_tokens"`
		CacheReadInputTokens     int                   `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int                   `json:"cache_creation_input_tokens"`
		PromptTokens             int                   `json:"prompt_tokens"`
		CompletionTokens         int                   `json:"completion_tokens"`
		TotalTokens              int                   `json:"total_tokens"`
		PromptTokensDetails      *selectorCacheDetails `json:"prompt_tokens_details"`
		InputTokensDetails       *selectorCacheDetails `json:"input_tokens_details"`
	} `json:"usage"`
}

type selectorCacheDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

func assertSelectorUsage(t *testing.T, path string, stream bool, body string) {
	t.Helper()
	want := selectorUsageFor(path, stream)
	if stream {
		var in, out, total int
		switch path {
		case "/v1/messages":
			in, out, total = collectAnthropicUsage(t, body)
		case "/v1/responses":
			in, out, total = collectResponsesUsage(t, body)
		default:
			in, out, total = collectOpenAIUsage(t, body)
		}
		if in != want.in || out != want.out || total != want.total {
			t.Fatalf("usage = in %d/out %d/total %d want in %d/out %d/total %d; body=%s", in, out, total, want.in, want.out, want.total, body)
		}
		return
	}

	var doc selectorUnaryUsage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode unary response: %v; body=%s", err, body)
	}
	switch path {
	case "/v1/messages":
		usage := doc.Usage
		if usage.InputTokens != want.in || usage.OutputTokens != want.out || usage.CacheCreationInputTokens != want.cacheWrite || usage.CacheReadInputTokens != want.cacheRead {
			t.Fatalf("anthropic usage = %+v want in %d/out %d/write %d/read %d; body=%s", usage, want.in, want.out, want.cacheWrite, want.cacheRead, body)
		}
		if inclusive := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens; inclusive != want.total {
			t.Fatalf("anthropic inclusive input = %d want %d; body=%s", inclusive, want.total, body)
		}
	case "/v1/chat/completions":
		usage := doc.Usage
		if usage.PromptTokens != want.in || usage.CompletionTokens != want.out || usage.TotalTokens != want.total {
			t.Fatalf("chat usage = %+v want in %d/out %d/total %d; body=%s", usage, want.in, want.out, want.total, body)
		}
		if usage.PromptTokensDetails == nil || usage.PromptTokensDetails.CachedTokens != want.cacheRead || usage.PromptTokensDetails.CacheWriteTokens != want.cacheWrite {
			t.Fatalf("chat cache usage = %+v want read %d/write %d; body=%s", usage.PromptTokensDetails, want.cacheRead, want.cacheWrite, body)
		}
	case "/v1/responses":
		usage := doc.Usage
		if usage.InputTokens != want.in || usage.OutputTokens != want.out || usage.TotalTokens != want.total {
			t.Fatalf("responses usage = %+v want in %d/out %d/total %d; body=%s", usage, want.in, want.out, want.total, body)
		}
		if usage.InputTokensDetails == nil || usage.InputTokensDetails.CachedTokens != want.cacheRead || usage.InputTokensDetails.CacheWriteTokens != want.cacheWrite {
			t.Fatalf("responses cache usage = %+v want read %d/write %d; body=%s", usage.InputTokensDetails, want.cacheRead, want.cacheWrite, body)
		}
	default:
		t.Fatalf("unsupported ingress path %q", path)
	}
}

func TestClaudeCodeContextSelectorHTTP(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
		text string
	}{
		{"messages-unary", "/v1/messages", `{"model":"default","max_tokens":32,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`, "hello from anthropic"},
		{"messages-stream", "/v1/messages", `{"model":"default","max_tokens":32,"stream":true,"thinking":{"type":"disabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`, "answer"},
		{"chat-unary", "/v1/chat/completions", `{"model":"default","max_tokens":32,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, "hello from anthropic"},
		{"chat-stream", "/v1/chat/completions", `{"model":"default","max_tokens":32,"stream":true,"reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`, "answer"},
		{"responses-unary", "/v1/responses", `{"model":"default","max_output_tokens":32,"reasoning":{"effort":"low"},"input":"hi"}`, "hello from anthropic"},
		{"responses-stream", "/v1/responses", `{"model":"default","max_output_tokens":32,"stream":true,"reasoning":{"effort":"none"},"input":"hi"}`, "answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			streamBody := omittedThinkingThenAnswerSSE()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captures.record(captureClaudeSelector(r))
				if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, streamBody)
					w.(http.Flusher).Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6[1m](high)",
			})
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if !strings.Contains(string(out), tc.text) {
				t.Fatalf("client response lost content: %s", out)
			}
			assertClaudeHighWire(t, captures.next(t), "claude-sonnet-4-6", true)
			captures.assertNoMore(t)
			assertSelectorUsage(t, tc.path, strings.Contains(tc.body, `"stream":true`), string(out))
		})
	}
}

func TestClaudeCodeContextSelectorNegativeHTTP(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		wantModel string
		adaptive  bool
		longCtx   bool
		budget    bool
	}{
		{"untagged", "claude-sonnet-4-6(high)", "claude-sonnet-4-6", true, false, false},
		{"two-million", "claude-sonnet-4-6[2m](high)", "claude-sonnet-4-6", true, false, false},
		{"uppercase-repeated-nontrailing", "CLAUDE-[1M]SONNET-4-6[2m](high)", "CLAUDE-SONNET-4-6", true, true, false},
		{"unknown-bracket", "claude-sonnet-4-6[9m](high)", "claude-sonnet-4-6[9m]", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captures.record(captureClaudeSelector(r))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    tc.model,
			})
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != tc.wantModel {
				t.Fatalf("wire model = %q want %q", wire.model, tc.wantModel)
			}
			if got := wire.thinking == "adaptive"; got != tc.adaptive {
				t.Fatalf("adaptive = %v want %v; body=%s", got, tc.adaptive, wire.body)
			}
			if got := strings.Contains(wire.body, `"budget_tokens"`); got != tc.budget {
				t.Fatalf("budget field = %v want %v; body=%s", got, tc.budget, wire.body)
			}
			assertBetaFlagCount(t, wire.beta, claudecode.BetaLongContext, boolCount(tc.longCtx))
			if tc.adaptive {
				if wire.effort != "high" {
					t.Fatalf("effort = %q want high; body=%s", wire.effort, wire.body)
				}
				assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 1)
			} else {
				assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 0)
			}
		})
	}
}

func TestClaudeCodeContextSelectorHaikuOneMillionHTTP(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		callerBeta bool
		longCtx    bool
	}{
		{"untagged", "claude-haiku-4-5", false, false},
		{"tagged", "claude-haiku-4-5[1m]", false, true},
		{"caller-untagged", "claude-haiku-4-5", true, false},
		{"caller-tagged", "claude-haiku-4-5[1m]", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captures.record(captureClaudeSelector(r))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    tc.model,
			})
			headers := map[string]string{}
			if tc.callerBeta {
				headers["anthropic-beta"] = claudecode.BetaLongContext
			}
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, `{"model":"default","max_tokens":4096,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`, headers)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != "claude-haiku-4-5" {
				t.Fatalf("wire model = %q want clean haiku id; body=%s", wire.model, wire.body)
			}
			if wire.thinking != "enabled" || wire.budget != 2048 {
				t.Fatalf("thinking = %q budget = %d want enabled/2048; body=%s", wire.thinking, wire.budget, wire.body)
			}
			assertBetaFlagCount(t, wire.beta, claudecode.BetaLongContext, boolCount(tc.longCtx))
			assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 0)
		})
	}
}

func TestNonClaudeBackendKeepsSelectorSpelling(t *testing.T) {
	tr := installSelectorTransport(t)
	captures := newSelectorCapture(2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiUpstreamBody)
	}))
	t.Cleanup(upstream.Close)
	base, token, _ := newSelectorProxy(t, tr, selectorTarget{
		protocol: domain.ProtocolOpenAI,
		baseURL:  upstream.URL,
		model:    "gpt-4o[1m](high)",
	})
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	wire := captures.next(t)
	captures.assertNoMore(t)
	if wire.model != "gpt-4o[1m]" {
		t.Fatalf("non-Claude model was normalized: %q body=%s", wire.model, wire.body)
	}
}

func TestClaudeCodeContextSelectorFailover(t *testing.T) {
	cases := []struct {
		name        string
		firstModel  string
		secondModel string
		firstLong   bool
		secondLong  bool
		stream      bool
	}{
		{"tagged-to-untagged-unary", "claude-sonnet-4-6[1m](high)", "claude-sonnet-4-6(high)", true, false, false},
		{"tagged-to-untagged-stream", "claude-sonnet-4-6[1m](high)", "claude-sonnet-4-6(high)", true, false, true},
		{"untagged-to-tagged-unary", "claude-sonnet-4-6(high)", "claude-sonnet-4-6[1m](high)", false, true, false},
		{"untagged-to-tagged-stream", "claude-sonnet-4-6(high)", "claude-sonnet-4-6[1m](high)", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			firstCh := newSelectorCapture(2)
			secondCh := newSelectorCapture(2)
			streamBody := omittedThinkingThenAnswerSSE()
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstCh.record(captureClaudeSelector(r))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"retry"}}`)
			}))
			t.Cleanup(first.Close)
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCh.record(captureClaudeSelector(r))
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, streamBody)
					w.(http.Flusher).Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(second.Close)
			base, token, _ := newSelectorProxy(t, tr,
				selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: first.URL, model: tc.firstModel},
				selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: second.URL, model: tc.secondModel},
			)
			body := `{"model":"default","max_tokens":32,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
			if tc.stream {
				body = `{"model":"default","max_tokens":32,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
			}
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			assertClaudeHighWire(t, firstCh.next(t), "claude-sonnet-4-6", tc.firstLong)
			assertClaudeHighWire(t, secondCh.next(t), "claude-sonnet-4-6", tc.secondLong)
			firstCh.assertNoMore(t)
			secondCh.assertNoMore(t)
		})
	}
}

func TestClaudeCodeContextSelectorSurvivesAuthRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
	}{
		{"unary", false},
		{"stream", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			var calls atomic.Int32
			captures := newSelectorCapture(2)
			streamBody := omittedThinkingThenAnswerSSE()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				captures.record(captureClaudeSelector(r))
				if calls.Load() == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"no"}}`)
					return
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, streamBody)
					w.(http.Flusher).Flush()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			var tokenCalls atomic.Int32
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/" || r.URL.RawQuery != "" {
					t.Errorf("token request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"sk-ant-oat-refreshed","refresh_token":"rt-next","expires_in":3600,"scope":"user:inference","account":{"uuid":"acct-1"}}`)
			}))
			t.Cleanup(tokenSrv.Close)
			tr.allow(t, http.MethodPost, tokenSrv.URL)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6[1m](high)",
				oauth: &domain.OAuthCreds{
					ClaudeCodeAuth: true, AccessToken: "sk-ant-oat-stale", RefreshToken: "rt-old",
					ExpiresAt: time.Now().Add(time.Hour).Unix(), TokenURL: tokenSrv.URL, ClientID: "cid",
					ClaudeAccountUUID: "acct-1", ClaudeDeviceID: strings.Repeat("cd", 32),
				},
			})
			body := `{"model":"default","max_tokens":32,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
			if tc.stream {
				body = `{"model":"default","max_tokens":32,"stream":true,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`
			}
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if calls.Load() != 2 {
				t.Fatalf("upstream calls=%d want 2", calls.Load())
			}
			if tokenCalls.Load() != 1 {
				t.Fatalf("token calls=%d want 1", tokenCalls.Load())
			}
			for i := 0; i < 2; i++ {
				assertClaudeHighWire(t, captures.next(t), "claude-sonnet-4-6", true)
			}
			captures.assertNoMore(t)
		})
	}
}

func TestSelectorTransportRejectsUnexpectedDestinations(t *testing.T) {
	var calls atomic.Int32
	next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	})
	tr := &selectorTransport{next: next, allowed: map[string]struct{}{}}
	tr.allow(t, http.MethodPost, "http://127.0.0.1:54321/messages?beta=true")
	client := selectorClient(tr)

	cases := []struct {
		name   string
		method string
		url    string
	}{
		{"official-host", http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true"},
		{"wrong-port", http.MethodPost, "http://127.0.0.1:54322/messages?beta=true"},
		{"wrong-path", http.MethodPost, "http://127.0.0.1:54321/v1/messages?beta=true"},
		{"wrong-query", http.MethodPost, "http://127.0.0.1:54321/messages?beta=false"},
		{"missing-query", http.MethodPost, "http://127.0.0.1:54321/messages"},
		{"wrong-method", http.MethodGet, "http://127.0.0.1:54321/messages?beta=true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err == nil || resp != nil {
				t.Fatalf("destination accepted: resp=%v err=%v", resp, err)
			}
			if calls.Load() != 0 {
				t.Fatalf("underlying transport calls = %d want 0", calls.Load())
			}
		})
	}

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:54321/messages?beta=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		t.Fatalf("allowed destination rejected: resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("underlying transport calls = %d want 1", calls.Load())
	}
}

func TestSelectorTransportRejectsRedirectEscape(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
	}{
		{"external-302", http.StatusFound, "https://api.anthropic.com/v1/messages?beta=true"},
		{"external-307", http.StatusTemporaryRedirect, "https://api.anthropic.com/v1/messages?beta=true"},
		{"external-308", http.StatusPermanentRedirect, "https://api.anthropic.com/v1/messages?beta=true"},
		{"relative-302", http.StatusFound, "/escape"},
		{"relative-307", http.StatusTemporaryRedirect, "/escape"},
		{"relative-308", http.StatusPermanentRedirect, "/escape"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			next := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if calls.Load() == 1 {
					return &http.Response{
						StatusCode: tc.status,
						Header:     http.Header{"Location": []string{tc.location}},
						Body:       io.NopCloser(strings.NewReader("")),
						Request:    req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{}`)),
					Request:    req,
				}, nil
			})
			tr := &selectorTransport{next: next, allowed: map[string]struct{}{}}
			tr.allow(t, http.MethodPost, "http://127.0.0.1:54321/messages?beta=true")
			client := selectorClient(tr)
			req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:54321/messages?beta=true", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Fatal("redirect to an unlisted destination succeeded")
			}
			if calls.Load() != 1 {
				t.Fatalf("underlying transport calls = %d want 1", calls.Load())
			}
		})
	}
}
