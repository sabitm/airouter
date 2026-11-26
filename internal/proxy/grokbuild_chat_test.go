package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"airouter/internal/codexgrant"
	"airouter/internal/domain"
	"airouter/internal/oauth"
	"airouter/internal/proxy/grokbuild"
	"airouter/internal/store"
)

type chatCapture struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
	auth   []string
	n      int
	failN  int
	sse    string
}

func (c *chatCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.paths = append(c.paths, r.URL.Path)
	c.bodies = append(c.bodies, append([]byte(nil), body...))
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	n := c.n
	c.n++
	fail := n < c.failN
	sse := c.sse
	c.mu.Unlock()
	if fail {
		http.Error(w, "upstream down", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sse)
}

func (c *chatCapture) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return string(c.bodies[len(c.bodies)-1])
}

type chatRouteTransport struct {
	base string
}

func (g chatRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !grokbuild.IsFixedInferenceURL(r.URL.String()) {
		return nil, io.ErrUnexpectedEOF
	}
	u, err := url.Parse(g.base + r.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func chatCreds(conn, user, access string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, GrokBuildAuth: true, Preset: grokbuild.Preset,
		ClientID: grokbuild.ClientID, TokenURL: grokbuild.TokenURL, AuthURL: grokbuild.Issuer,
		Scopes: grokbuild.Scopes, GrokBuildConnection: conn, UserID: user,
		AccessToken: access, RefreshToken: "refresh-" + conn, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
}

func addGrokChatProvider(t *testing.T, st *store.Store, name, conn, user string) *domain.Provider {
	t.Helper()
	p := &domain.Provider{
		Name: name, BaseURL: grokbuild.BaseURL, Protocol: domain.ProtocolGrokBuild,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: chatCreds(conn, user, "access-"+conn),
	}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func startChatProxy(t *testing.T, st *store.Store, upstream string) *httptest.Server {
	t.Helper()
	return startChatProxyTransport(t, st, chatRouteTransport{base: upstream})
}

func startChatProxyTransport(t *testing.T, st *store.Store, rt http.RoundTripper) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	px := NewWithOAuth(st, slog.New(slog.NewTextHandler(io.Discard, nil)), oauth.New(st))
	px.streamClient = &http.Client{Transport: rt}
	px.client = px.streamClient
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

type authRouteTransport struct {
	byAuth map[string]string
	plain  string
}

func (a authRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := ""
	if grokbuild.IsFixedInferenceURL(r.URL.String()) || codexgrant.IsStandardResponsesURL(r.URL.String()) {
		base = a.byAuth[r.Header.Get("Authorization")]
	} else if a.plain != "" {
		base = a.plain
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

func chatPost(t *testing.T, rawURL, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, out
}

func grokChatSSE(text, cipher, summary string, late bool, tools ...string) string {
	var b strings.Builder
	write := func(s string) { b.WriteString("data: " + s + "\n\n") }
	idx := 0
	reasoning := `{"type":"reasoning","id":"rs_out","status":"completed","encrypted_content":` + grokJSONString(cipher) + `,"summary":[{"type":"summary_text","text":` + grokJSONString(summary) + `},{"type":"summary_text","text":""}]}`
	if cipher != "" && !late {
		write(`{"type":"response.output_item.added","output_index":0,"item":` + reasoning + `}`)
		write(`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`)
		idx = 1
	}
	if text != "" {
		write(`{"type":"response.output_text.delta","output_index":` + jsonInt(idx) + `,"delta":` + grokJSONString(text) + `}`)
		idx++
	}
	for i, tool := range tools {
		parts := strings.SplitN(tool, "|", 4)
		item := `{"type":"function_call","id":"` + parts[0] + `","call_id":"` + parts[1] + `","name":"` + parts[2] + `","arguments":` + grokJSONString(parts[3]) + `,"status":"completed"}`
		write(`{"type":"response.output_item.added","output_index":` + jsonInt(idx+i) + `,"item":` + item + `}`)
		write(`{"type":"response.output_item.done","output_index":` + jsonInt(idx+i) + `,"item":` + item + `}`)
	}
	if cipher != "" && late {
		write(`{"type":"response.output_item.done","output_index":` + jsonInt(idx) + `,"item":` + reasoning + `}`)
	}
	write(`{"type":"response.completed","response":{"id":"resp_chat","status":"completed","usage":{"input_tokens":4,"output_tokens":5}}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func jsonInt(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func extractChatDetails(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	text := string(body)
	var joined []json.RawMessage
	if strings.Contains(text, "data:") {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "" || line == "[DONE]" || !strings.HasPrefix(strings.TrimSpace(line), "{") && !strings.Contains(line, "reasoning_details") {
				if !strings.Contains(line, "reasoning_details") {
					continue
				}
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Details []json.RawMessage `json:"reasoning_details"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))), &chunk) != nil {
				continue
			}
			if len(chunk.Choices) > 0 {
				joined = append(joined, chunk.Choices[0].Delta.Details...)
			}
		}
	} else {
		var doc struct {
			Choices []struct {
				Message struct {
					Details []json.RawMessage `json:"reasoning_details"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &doc) == nil && len(doc.Choices) > 0 {
			joined = doc.Choices[0].Message.Details
		}
	}
	if len(joined) == 0 {
		t.Fatalf("no reasoning_details in %s", text)
	}
	return joined
}

func replayBody(model string, details []json.RawMessage, content, calls string, stream bool) string {
	msg := map[string]any{"role": "assistant", "content": content, "reasoning_details": details}
	messages := []any{map[string]any{"role": "user", "content": "first"}, msg}
	if calls != "" {
		var tools []any
		_ = json.Unmarshal([]byte(calls), &tools)
		msg["tool_calls"] = tools
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": "call_a", "content": "file text"})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "next"})
	raw, _ := json.Marshal(map[string]any{
		"model": model, "stream": stream,
		"stream_options":   map[string]any{"include_usage": true},
		"reasoning_effort": "high",
		"messages":         messages,
	})
	return string(raw)
}

func inputTypes(t *testing.T, body string) []string {
	t.Helper()
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) != nil {
		t.Fatalf("upstream body: %s", body)
	}
	input, _ := doc["input"].([]any)
	var out []string
	for _, item := range input {
		m, _ := item.(map[string]any)
		id, _ := m["id"].(string)
		typ, _ := m["type"].(string)
		if typ == "" {
			typ, _ = m["role"].(string)
		}
		out = append(out, typ+":"+id)
	}
	return out
}

func TestGrokChatBridgeStreamEmitsAndReplays(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("Hello", "cipher-live", "think", false, "fc_a|call_a|read|{\"path\":\"a\"}", "fc_b|call_b|list|{\"path\":\"b\"}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7(high)", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("[DONE]")) || !bytes.Contains(out, []byte("Hello")) {
		t.Fatalf("status %d body %s", resp.StatusCode, out)
	}
	if !bytes.Contains(out, []byte(`"reasoning_content":"think"`)) {
		t.Fatalf("missing readable summary: %s", out)
	}
	details := extractChatDetails(t, out)
	rawDetails := string(json.RawMessage(func() []byte { raw, _ := json.Marshal(details); return raw }()))
	if strings.Count(rawDetails, "cipher-live") != 1 || !strings.Contains(rawDetails, grokbuild.ChatBridgeFormat) {
		t.Fatalf("details = %s", rawDetails)
	}
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "Hello", `[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"path\": \"a\"}"}},{"id":"call_b","type":"function","function":{"name":"list","arguments":"{\"path\":\"b\"}"}}]`, true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status %d %s", resp.StatusCode, out)
	}
	sent := cap.lastBody()
	for _, want := range []string{"cipher-live", "rs_out", "fc_a", "fc_b", "call_a", "file text", "think", `"effort":"high"`} {
		if !strings.Contains(sent, want) {
			t.Fatalf("replay missing %s in %s", want, sent)
		}
	}
	if !strings.Contains(sent, `{"path": "a"}`) && !strings.Contains(sent, `{\"path\": \"a\"}`) {
		t.Fatalf("reserialized args lost: %s", sent)
	}
	got := strings.Join(inputTypes(t, sent), ",")
	if !strings.Contains(got, "reasoning:rs_out,message:,function_call:fc_a,function_call:fc_b") {
		t.Fatalf("order = %s body %s", got, sent)
	}
}

func TestGrokChatBridgeUnaryLateCipher(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("Done", "cipher-late", "late-sum", true)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	p := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: p.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("Done")) || !bytes.Contains(out, []byte("late-sum")) {
		t.Fatalf("status %d %s", resp.StatusCode, out)
	}
	details := extractChatDetails(t, out)
	if strings.Count(string(json.RawMessage(func() []byte { raw, _ := json.Marshal(details); return raw }())), "cipher-late") != 1 {
		t.Fatalf("details = %s", details)
	}
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "Done", "", false))
	if resp.StatusCode != http.StatusOK || !strings.Contains(cap.lastBody(), "cipher-late") {
		t.Fatalf("unary replay %d %s sent %s", resp.StatusCode, out, cap.lastBody())
	}
}

func TestGrokChatPinnedSourceWinsDespiteRank(t *testing.T) {
	low := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	high := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(low)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(high)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-conn-a": upA.URL,
		"Bearer access-conn-b": upB.URL,
	}})
	if err := st.SetTargetEnabled(context.Background(), comboTargetID(t, st, "combo", a.ID), false); err != nil {
		t.Fatal(err)
	}
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("from-b")) {
		t.Fatalf("seed %d %s", resp.StatusCode, out)
	}
	details := extractChatDetails(t, out)
	if err := st.SetTargetEnabled(context.Background(), comboTargetID(t, st, "combo", a.ID), true); err != nil {
		t.Fatal(err)
	}
	beforeA := low.n
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "from-b", "", true))
	if resp.StatusCode != http.StatusOK || !strings.Contains(high.lastBody(), "cipher-b") || low.n != beforeA {
		t.Fatalf("pin status %d Acalls=%d B=%s", resp.StatusCode, low.n-beforeA, high.lastBody())
	}
}

func TestGrokChatOriginFailureFallsBackClean(t *testing.T) {
	origin := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	fallback := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(fallback)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-conn-a": upA.URL, "Bearer access-conn-b": upB.URL,
	}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	origin.failN = origin.n + 1
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "from-a", "", true))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("from-b")) {
		t.Fatalf("fallback %d %s", resp.StatusCode, out)
	}
	sent := fallback.lastBody()
	if strings.Contains(sent, "cipher-a") || strings.Contains(sent, "conn-a") || strings.Contains(sent, grokbuild.ChatBridgeFormat) {
		t.Fatalf("fallback leaked origin state: %s", sent)
	}
	if !strings.Contains(sent, "sum-a") || !strings.Contains(sent, "from-a") {
		t.Fatalf("fallback dropped visible history: %s", sent)
	}
	next := extractChatDetails(t, out)
	origin.failN = 0
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", next, "from-b", "", true))
	if resp.StatusCode != http.StatusOK || !strings.Contains(fallback.lastBody(), "cipher-b") || strings.Contains(origin.lastBody(), "cipher-b") {
		t.Fatalf("restamp did not pin B: %d A=%s B=%s", resp.StatusCode, origin.lastBody(), fallback.lastBody())
	}
}

func TestGrokChatPassthroughFallbackDropsCipher(t *testing.T) {
	origin := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	plain := &chatCapture{sse: "data: {\"choices\":[{\"delta\":{\"content\":\"plain\"}}]}\n\ndata: [DONE]\n\n"}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upPlain := httptest.NewServer(plain)
	t.Cleanup(upPlain.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	chat := &domain.Provider{Name: "chat", BaseURL: upPlain.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(context.Background(), chat); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: chat.ID, UpstreamModel: "chat-model", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{"Bearer access-conn-a": upA.URL}, plain: upPlain.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	origin.failN = origin.n + 1
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "from-a", "", true))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("plain")) {
		t.Fatalf("passthrough fallback %d %s", resp.StatusCode, out)
	}
	sent := plain.lastBody()
	if strings.Contains(sent, "cipher-a") || strings.Contains(sent, "reasoning.encrypted") || strings.Contains(sent, "conn-a") {
		t.Fatalf("passthrough leaked cipher: %s", sent)
	}
	for _, want := range []string{"from-a", "sum-a", "include_usage", "reasoning_effort"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("passthrough missing %s in %s", want, sent)
		}
	}
}

func TestGrokChatTamperRejectedBeforeUpstream(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	body := replayBody("combo", details, "from-a", "", false)
	body = strings.Replace(body, "cipher-a", "cipher-x", 1)
	before := cap.n
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, body)
	if resp.StatusCode != http.StatusBadRequest || cap.n != before || bytes.Contains(out, []byte("cipher")) {
		t.Fatalf("tamper status %d calls %d body %s", resp.StatusCode, cap.n-before, out)
	}
}

func TestGrokChatReconnectDropsCipherBeforeSend(t *testing.T) {
	origin := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	other := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(other)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-conn-a": upA.URL, "Bearer access-conn-new": upA.URL, "Bearer access-conn-b": upB.URL,
	}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	if err := st.UpdateProviderOAuth(context.Background(), a.ID, chatCreds("conn-new", "user-a", "access-conn-new")); err != nil {
		t.Fatal(err)
	}
	live, err := st.GetProvider(context.Background(), a.ID)
	if err != nil || live.OAuthCreds.GrokBuildConnection != "conn-new" {
		t.Fatalf("stored reconnect = %+v %v", live.OAuthCreds, err)
	}
	origin.failN = origin.n + 1
	beforeA, beforeB := origin.n, other.n
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "from-a", "", true))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("from-b")) {
		t.Fatalf("reconnect fallback %d containsB=%v", resp.StatusCode, bytes.Contains(out, []byte("from-b")))
	}
	if strings.Contains(other.lastBody(), "cipher-a") || strings.Contains(other.lastBody(), "conn-a") {
		t.Fatalf("ciphertext survived reconnect B=%s", other.lastBody())
	}
	if !strings.Contains(other.lastBody(), "sum-a") || !strings.Contains(other.lastBody(), "from-a") {
		t.Fatalf("fallback dropped visible history: %s", other.lastBody())
	}
	if origin.n <= beforeA || other.n <= beforeB {
		t.Fatalf("replay calls A=%d B=%d", origin.n-beforeA, other.n-beforeB)
	}
}

func TestGrokChatRefreshKeepsReplay(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	a.OAuthCreds.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if err := st.UpdateProviderOAuth(context.Background(), a.ID, a.OAuthCreds); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	grokbuild.SetTestTransport(refreshRouteTransport{base: up.URL})
	t.Cleanup(func() { grokbuild.SetTestTransport(nil) })
	ts := startChatProxyTransport(t, st, refreshRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	if !bytes.Contains(out, []byte("conn-a")) {
		t.Fatalf("refresh changed generation: %s", out)
	}
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "from-a", "", true))
	if resp.StatusCode != http.StatusOK || !strings.Contains(cap.lastBody(), "cipher-a") {
		t.Fatalf("refresh replay %d %s", resp.StatusCode, cap.lastBody())
	}
}

type refreshRouteTransport struct{ base string }

func (r refreshRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "auth.x.ai" || strings.Contains(req.URL.String(), "/oauth2/token") {
		raw := []byte(`{"access_token":"access-rotated","refresh_token":"refresh-rotated","expires_in":3600}`)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	}
	return chatRouteTransport{base: r.base}.RoundTrip(req)
}

func TestGrokChatNativeResponsesAndGenericChatUntouched(t *testing.T) {
	cap := &chatCapture{sse: grokBuildSSE()}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	body := `{"model":"combo","input":[{"type":"message","role":"user","content":"hi"},{"type":"reasoning","id":"rs_keep","encrypted_content":"opaque-native","summary":[]}],"stream":false}`
	resp, out := chatPost(t, ts.URL+"/v1/responses", key.Token, body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(cap.lastBody(), "opaque-native") || bytes.Contains(out, []byte(grokbuild.ChatBridgeFormat)) {
		t.Fatalf("native responses changed: %d %s sent %s", resp.StatusCode, out, cap.lastBody())
	}
	generic := `{"model":"combo","stream":false,"messages":[{"role":"assistant","content":"hello","reasoning_details":[{"type":"reasoning.text","text":"plain"}]}]}`
	before := cap.lastBody()
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, generic)
	if resp.StatusCode != http.StatusOK || !strings.Contains(cap.lastBody(), "plain") || strings.Contains(cap.lastBody(), grokbuild.ChatBridgeFormat) {
		t.Fatalf("generic chat changed: %d sent %s before %s", resp.StatusCode, cap.lastBody(), before)
	}
	if bytes.Contains(out, []byte(grokbuild.ChatBridgeFormat)) {
		t.Fatalf("generic response stamped: %s", out)
	}
}

func TestGrokChatResolveRaceDropsCipher(t *testing.T) {
	origin := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	other := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(other)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-conn-a": upA.URL, "Bearer access-conn-b": upB.URL,
	}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	testGrokBuildReplayResolved = func(p *domain.Provider) {
		if p != nil && p.ID == a.ID && p.OAuthCreds != nil {
			p.OAuthCreds.GrokBuildConnection = "conn-raced"
		}
	}
	t.Cleanup(func() { testGrokBuildReplayResolved = nil })
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "from-a", "", true))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("from-b")) {
		t.Fatalf("race fallback %d %s", resp.StatusCode, out)
	}
	if strings.Contains(origin.lastBody(), "cipher-a") || strings.Contains(other.lastBody(), "cipher-a") {
		t.Fatalf("race sent ciphertext A=%s B=%s", origin.lastBody(), other.lastBody())
	}
}

func TestGrokChatCancelDoesNotFallThrough(t *testing.T) {
	origin := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	other := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(other)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-conn-a": upA.URL, "Bearer access-conn-b": upB.URL,
	}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(replayBody("combo", details, "from-a", "", true)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key.Token)
	beforeB := other.n
	resp, err = http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if other.n != beforeB {
		t.Fatalf("canceled request reached fallback: %d", other.n-beforeB)
	}
}

func TestGrokChatPostCommitDoesNotFallThrough(t *testing.T) {
	bad := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
	origin := &chatCapture{sse: bad}
	other := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(other)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-conn-a": upA.URL, "Bearer access-conn-b": upB.URL,
	}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || other.n != 0 || !bytes.Contains(out, []byte("partial")) {
		t.Fatalf("post-commit status %d B=%d body %s", resp.StatusCode, other.n, out)
	}
}

func TestGrokChatNoTargetDoesNotSend(t *testing.T) {
	cap := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: false}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	body := `{"model":"combo","messages":[{"role":"assistant","content":"from-a","reasoning_details":[{"type":"reasoning.encrypted","format":"airouter-grok-build-v1","id":"rs","index":0,"data":"cipher","layout":[],"bind":{},"mac":"aa"}]}]}`
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, body)
	if resp.StatusCode != http.StatusBadRequest || cap.n != 0 || bytes.Contains(out, []byte("cipher")) {
		t.Fatalf("no target status %d calls %d body %s", resp.StatusCode, cap.n, out)
	}
}

func TestGrokChatPiAppLoop(t *testing.T) {
	cap := runPiGrokChatLoop(t, "", "marker")
	if cap.n < 2 || !strings.Contains(cap.lastBody(), "cipher-live") || !strings.Contains(cap.lastBody(), "call_a") {
		t.Fatalf("proxy replay calls=%d body=%s", cap.n, cap.lastBody())
	}
}

func TestGrokChatPiWhitespaceTextLoop(t *testing.T) {
	cap := runPiGrokChatLoop(t, "  \n", "whitespace")
	body := cap.lastBody()
	if cap.n < 2 || !strings.Contains(body, "file text") || !strings.Contains(body, "call_a") {
		t.Fatalf("tool result did not reach upstream calls=%d body=%s", cap.n, body)
	}
	if strings.Contains(body, "cipher-live") || strings.Contains(body, grokbuild.ChatBridgeFormat) {
		t.Fatalf("whitespace replay forwarded ciphertext: %s", body)
	}
}

func runPiGrokChatLoop(t *testing.T, text, mode string) *chatCapture {
	t.Helper()
	adapter := os.Getenv("AIROUTER_TEST_PI_CHAT_ADAPTER")
	if adapter == "" {
		t.Skip("AIROUTER_TEST_PI_CHAT_ADAPTER is not set")
	}
	if _, err := os.Stat(adapter); err != nil {
		t.Skip("Pi chat adapter is not available")
	}
	cap := &chatCapture{sse: grokChatSSE(text, "cipher-live", "", false, "fc_a|call_a|read|{\"path\":\"a\"}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	script := filepath.Join(t.TempDir(), "pi-loop.mjs")
	if err := os.WriteFile(script, []byte(piGrokChatLoopScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script, ts.URL, key.Token, adapter, mode)
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("pi-loop-ok")) {
		t.Fatalf("pi loop: %v %s calls=%d", err, out, cap.n)
	}
	return cap
}

func TestGrokChatHistoryRetainsOlderSource(t *testing.T) {
	cap := &chatCapture{}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	cap.sse = grokChatSSE("one", "cipher-1", "sum-1", false, "fc_1|call_1|read|{}")
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	first := extractChatDetails(t, out)
	cap.sse = grokChatSSE("two", "cipher-2", "sum-2", false, "fc_2|call_2|list|{}")
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", first, "one", `[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]`, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	second := extractChatDetails(t, out)
	body := map[string]any{"model": "combo", "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "one", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}, "reasoning_details": first},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result-1"},
		map[string]any{"role": "user", "content": "next"},
		map[string]any{"role": "assistant", "content": "two", "tool_calls": []any{map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "list", "arguments": "{}"}}}, "reasoning_details": second},
		map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "result-2"},
		map[string]any{"role": "user", "content": "third"},
	}}
	raw, _ := json.Marshal(body)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("third %d %s", resp.StatusCode, out)
	}
	sent := cap.lastBody()
	for _, want := range []string{"cipher-1", "cipher-2", "fc_1", "fc_2", "call_1", "call_2", "result-1", "result-2"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("history missing %s in %s", want, sent)
		}
	}
}

func TestGrokChatOlderMarkerDoesNotPin(t *testing.T) {
	origin := &chatCapture{sse: grokChatSSE("from-a", "cipher-a", "sum-a", false)}
	other := &chatCapture{sse: grokChatSSE("from-b", "cipher-b", "sum-b", false)}
	upA := httptest.NewServer(origin)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(other)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	b := addGrokChatProvider(t, st, "gb", "conn-b", "user-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: b.ID, UpstreamModel: "grok-4.7", Enabled: true},
		{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{"Bearer access-conn-a": upA.URL, "Bearer access-conn-b": upB.URL}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || origin.n != 0 {
		t.Fatalf("first route A=%d B=%d", origin.n, other.n)
	}
	old := extractChatDetails(t, out)
	body := map[string]any{"model": "combo", "messages": []any{
		map[string]any{"role": "assistant", "content": "from-b", "reasoning_details": old},
		map[string]any{"role": "assistant", "content": "plain latest"},
		map[string]any{"role": "user", "content": "next"},
	}}
	raw, _ := json.Marshal(body)
	beforeA := origin.n
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, string(raw))
	if resp.StatusCode != http.StatusOK || origin.n != beforeA {
		t.Fatalf("older marker changed order: status %d A=%d", resp.StatusCode, origin.n-beforeA)
	}
	if strings.Contains(other.lastBody(), "cipher-b") || strings.Contains(other.lastBody(), grokbuild.ChatBridgeFormat) {
		t.Fatalf("older marker replayed: %s", other.lastBody())
	}
	if !strings.Contains(other.lastBody(), "sum-b") || !strings.Contains(other.lastBody(), "plain latest") {
		t.Fatalf("readable history lost: %s", other.lastBody())
	}
}

func TestGrokChatSourceOrderAndBudget(t *testing.T) {
	cap := &chatCapture{sse: grokReviewOrderSSE()}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addGrokChatProvider(t, st, "ga", "conn-a", "user-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "grok-4.7", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxy(t, st, up.URL)
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("[DONE]")) || bytes.Contains(out, []byte(`"error"`)) {
		t.Fatalf("status %d %s", resp.StatusCode, out)
	}
	raw := string(mustChatJSON(extractChatDetails(t, out)))
	if strings.Index(raw, `"id":"rs_early"`) > strings.Index(raw, `"k":"tool"`) || !strings.Contains(raw, "cipher-late") {
		t.Fatalf("late order lost: %s", raw)
	}
	if strings.Count(raw, `"summary":"think"`) != 1 || strings.Count(raw, `"summary":"two"`) != 1 {
		t.Fatalf("summary parts lost: %s", raw)
	}
	cap.sse = grokSilentOrderSSE()
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte(`"error"`)) {
		t.Fatalf("silent status %d %s", resp.StatusCode, out)
	}
	raw = string(mustChatJSON(extractChatDetails(t, out)))
	if strings.Index(raw, `"id":"rs_silent"`) < 0 || strings.Index(raw, `"id":"rs_silent"`) > strings.Index(raw, `"k":"tool"`) || !strings.Contains(raw, "cipher-silent") {
		t.Fatalf("silent source order lost: %s", raw)
	}
}

func grokSilentOrderSSE() string {
	frames := []string{
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_silent","status":"completed","encrypted_content":"cipher-silent","summary":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_silent","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	var b strings.Builder
	for _, frame := range frames {
		b.WriteString("data: " + frame + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func grokReviewOrderSSE() string {
	frames := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_early","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"item_id":"rs_early","summary_index":0,"delta":"think"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}","status":"completed"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_early","status":"completed","encrypted_content":"cipher-late","summary":[{"type":"summary_text","text":"think"},{"type":"summary_text","text":"two"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_order","status":"completed","output":[{"type":"reasoning","id":"rs_early","encrypted_content":"cipher-late","summary":[{"type":"summary_text","text":"think"},{"type":"summary_text","text":"two"}]},{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	}
	var b strings.Builder
	for _, frame := range frames {
		b.WriteString("data: " + frame + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func mustChatJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

const piGrokChatLoopScript = `import { pathToFileURL } from "node:url";
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
const mode = process.argv[5] || "marker";
const signature = message.content.find((b) => b.type === "thinking")?.thinkingSignature || "";
if (mode === "whitespace") {
  const text = message.content.filter((b) => b.type === "text").map((b) => b.text).join("");
  if (text !== "  \n") throw new Error("visible text changed: " + JSON.stringify(text));
  if (signature.includes("cipher-live") || signature.includes("airouter-grok-build-v1")) throw new Error("whitespace turn stamped a marker");
} else if (mode !== "marker" || !signature.includes("cipher-live") || !signature.includes("airouter-grok-build-v1")) {
  throw new Error("signature missing bridge");
}
context.messages.push(message);
context.messages.push({ role: "toolResult", toolCallId: "call_a", toolName: "read", content: [{ type: "text", text: "file text" }], timestamp: 2 });
const second = stream(model, context, { apiKey: token, fetch: fetchImpl });
for await (const ev of second) {
  if (ev.type === "error") throw new Error(ev.error.errorMessage || "second stream error");
}
console.log("pi-loop-ok");
`

func comboTargetID(t *testing.T, st *store.Store, combo string, providerID int64) int64 {
	t.Helper()
	got, err := st.GetComboByName(context.Background(), combo)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range got.Targets {
		if target.ProviderID == providerID {
			return target.ID
		}
	}
	t.Fatalf("target for provider %d not found", providerID)
	return 0
}
