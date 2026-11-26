package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"airouter/internal/store"
)

type codexChatCapture struct {
	mu       sync.Mutex
	bodies   [][]byte
	auth     []string
	account  []string
	session  []string
	n        int
	failN    int
	sse      string
	redirect string
}

func (c *codexChatCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, append([]byte(nil), body...))
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	c.account = append(c.account, r.Header.Get("chatgpt-account-id"))
	c.session = append(c.session, r.Header.Get("session_id"))
	n := c.n
	c.n++
	fail := n < c.failN
	sse := c.sse
	redirect := c.redirect
	c.mu.Unlock()
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	if fail {
		http.Error(w, "upstream down", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, sse)
}

func (c *codexChatCapture) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return string(c.bodies[len(c.bodies)-1])
}

type codexRouteTransport struct {
	base string
}

func (g codexRouteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !codexgrant.IsStandardResponsesURL(r.URL.String()) {
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

func codexChatCreds(account, generation, access string) *domain.OAuthCreds {
	return &domain.OAuthCreds{
		Mode: domain.OAuthAuto, Preset: codexgrant.Preset, ClientID: codexgrant.ClientID,
		TokenURL: codexgrant.TokenURL, RefreshJSON: true,
		AccessToken: access, RefreshToken: "refresh-" + account, AccountID: account,
		CodexGrantGeneration: generation, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
}

func addCodexChatProvider(t *testing.T, st *store.Store, name, account, generation string) *domain.Provider {
	t.Helper()
	p := &domain.Provider{
		Name: name, BaseURL: codexgrant.APIBase, Protocol: domain.ProtocolOpenAICodex,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: codexChatCreds(account, generation, "access-"+account),
	}
	if err := st.CreateProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func codexChatSSE(text, itemID, cipher, summary string, tools ...string) string {
	var b strings.Builder
	write := func(s string) { b.WriteString("data: " + s + "\n\n") }
	idx := 0
	if itemID != "" {
		provisional := `{"type":"reasoning","id":"` + itemID + `","status":"in_progress","summary":[]}`
		final := `{"type":"reasoning","id":"` + itemID + `","status":"completed","summary":[{"type":"summary_text","text":` + grokJSONString(summary) + `}]`
		if cipher != "" {
			final += `,"encrypted_content":` + grokJSONString(cipher)
		}
		final += `}`
		write(`{"type":"response.output_item.added","output_index":0,"item":` + provisional + `}`)
		write(`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":` + grokJSONString(summary) + `}`)
		write(`{"type":"response.output_item.done","output_index":0,"item":` + final + `}`)
		idx = 1
	}
	if text != "" {
		write(`{"type":"response.output_text.delta","output_index":` + jsonInt(idx) + `,"delta":` + grokJSONString(text) + `}`)
		idx++
	}
	for _, tool := range tools {
		parts := strings.SplitN(tool, "|", 4)
		item := `{"type":"function_call","id":"` + parts[0] + `","call_id":"` + parts[1] + `","name":"` + parts[2] + `","arguments":` + grokJSONString(parts[3]) + `,"status":"completed"}`
		write(`{"type":"response.output_item.done","output_index":` + jsonInt(idx) + `,"item":` + item + `}`)
		idx++
	}
	usage := `{"input_tokens":3,"output_tokens":5,"total_tokens":8}`
	write(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":` + usage + `,"output":[]}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestCodexChatTwoTurnSSEAndUnary(t *testing.T) {
	cap := &codexChatCapture{sse: codexChatSSE("answer", "rs_1", "cipher-1", "sum-1", "fc_1|call_1|read|{\"path\":\"a\"}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex-high", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(out, []byte("[DONE]")) {
		t.Fatalf("first %d %s", resp.StatusCode, out)
	}
	if !strings.Contains(cap.lastBody(), `"model":"gpt-5.3-codex-high"`) {
		t.Fatalf("seed wire model: %s", cap.lastBody())
	}
	if !bytes.Contains(out, []byte("sum-1")) || !bytes.Contains(out, []byte("answer")) {
		t.Fatalf("display missing: %s", out)
	}
	details := extractChatDetails(t, out)
	raw := string(mustChatJSON(details))
	if !strings.Contains(raw, "airouter-codex-v1") || !strings.Contains(raw, "\"data\":\"cipher-1\"") || !strings.Contains(raw, "\"id\":\"rs_1\"") {
		t.Fatalf("marker missing protected fields: %s", raw)
	}
	if strings.Contains(string(out), "cipher-1") && !bytes.Contains(out, []byte("reasoning_details")) {
		t.Fatalf("ciphertext outside marker: %s", out)
	}
	replay := replayBody("combo", details, "answer", `[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]`, false)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replay)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay %d %s", resp.StatusCode, out)
	}
	sent := cap.lastBody()
	for _, want := range []string{`"id":"rs_1"`, "cipher-1", `"call_id":"call_1"`, "file text", `"model":"gpt-5.3-codex-high"`} {
		if !strings.Contains(sent, want) {
			t.Fatalf("replay missing %s in %s", want, sent)
		}
	}
	if strings.Contains(sent, "airouter-codex-v1") {
		t.Fatal("marker forwarded")
	}
	if cap.account[len(cap.account)-1] != "acct-a" || cap.session[len(cap.session)-1] == "" {
		t.Fatalf("headers account=%v session=%v", cap.account, cap.session)
	}
}

func TestCodexChatPortableFailoverAndOlderHistory(t *testing.T) {
	aCap := &codexChatCapture{sse: codexChatSSE("from-a", "rs_a", "cipher-a", "sum-a")}
	bCap := &codexChatCapture{sse: codexChatSSE("from-b", "rs_b", "cipher-b", "sum-b")}
	upA := httptest.NewServer(aCap)
	t.Cleanup(upA.Close)
	upB := httptest.NewServer(bCap)
	t.Cleanup(upB.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	b := addCodexChatProvider(t, st, "cb", "acct-b", "gen-b")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
		"Bearer access-acct-a": upA.URL,
		"Bearer access-acct-b": upB.URL,
	}})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"seed"}]}`)
	if resp.StatusCode != http.StatusOK || aCap.n != 1 {
		t.Fatalf("seed status %d A=%d", resp.StatusCode, aCap.n)
	}
	old := extractChatDetails(t, out)
	aCap.failN = aCap.n + 1
	body := map[string]any{"model": "combo", "messages": []any{
		map[string]any{"role": "assistant", "content": "from-a", "reasoning_details": old},
		map[string]any{"role": "assistant", "content": "plain latest"},
		map[string]any{"role": "user", "content": "next"},
	}}
	raw, _ := json.Marshal(body)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, string(raw))
	if resp.StatusCode != http.StatusOK || bCap.n != 1 {
		t.Fatalf("older marker pinned or failed: %d B=%d %s", resp.StatusCode, bCap.n, out)
	}
	if strings.Contains(bCap.lastBody(), "cipher-a") || strings.Contains(bCap.lastBody(), "rs_a") {
		t.Fatalf("older ciphertext forwarded: %s", bCap.lastBody())
	}
	latest := extractChatDetails(t, out)
	aCap.failN = 0
	replay := replayBody("combo", latest, "from-b", "", false)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replay)
	if resp.StatusCode != http.StatusOK || bCap.n < 2 {
		t.Fatalf("same-source pin failed: %d B=%d A=%d %s", resp.StatusCode, bCap.n, aCap.n, out)
	}
	if !strings.Contains(bCap.lastBody(), "cipher-b") || strings.Contains(aCap.lastBody(), "cipher-b") {
		t.Fatalf("ciphertext went to the wrong source")
	}
	beforeA := aCap.n
	bCap.failN = bCap.n + 1
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replay)
	if resp.StatusCode != http.StatusOK || aCap.n != beforeA+1 {
		t.Fatalf("portable failover failed: %d A=%d B=%d %s", resp.StatusCode, aCap.n, bCap.n, out)
	}
	if strings.Contains(aCap.lastBody(), "cipher-b") || strings.Contains(aCap.lastBody(), "rs_b") {
		t.Fatalf("B ciphertext sent to A: %s", aCap.lastBody())
	}
	if !strings.Contains(aCap.lastBody(), "sum-b") {
		t.Fatalf("readable summary lost: %s", aCap.lastBody())
	}
}

func TestCodexChatRejectsTamperAndWrongBinding(t *testing.T) {
	cap := &codexChatCapture{sse: codexChatSSE("answer", "rs_1", "cipher-1", "sum-1")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	otherKey, err := st.NewAccessKey(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	before := cap.n
	bad := bytes.Replace(mustChatJSON(details), []byte("airouter-codex-v1"), []byte("airouter-codex-v2"), 1)
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", []json.RawMessage{bad}, "answer", "", false))
	if resp.StatusCode != http.StatusBadRequest || cap.n != before || !bytes.Contains(out, []byte("invalid codex replay state")) {
		t.Fatalf("tamper status %d calls=%d %s", resp.StatusCode, cap.n-before, out)
	}
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", otherKey.Token, replayBody("combo", details, "answer", "", false))
	if resp.StatusCode != http.StatusBadRequest || cap.n != before {
		t.Fatalf("cross-caller status %d %s", resp.StatusCode, out)
	}
	wrapped := `{"model":"combo","messages":[{"role":"user","content":{"reasoning_details":` + string(mustChatJSON(details)) + `}}]}`
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, wrapped)
	if resp.StatusCode != http.StatusBadRequest || cap.n != before {
		t.Fatalf("wrapped status %d %s", resp.StatusCode, out)
	}
}

func TestCodexChatGrantRefreshKeepsReplay(t *testing.T) {
	cap := &codexChatCapture{sse: codexChatSSE("answer", "rs_1", "cipher-1", "sum-1")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	live, err := st.GetProvider(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	live.OAuthCreds.AccessToken = "access-rotated"
	live.OAuthCreds.ExpiresAt = time.Now().Add(time.Hour).Unix()
	if err := st.UpdateProvider(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "answer", "", false))
	if resp.StatusCode != http.StatusOK || !strings.Contains(cap.lastBody(), "cipher-1") {
		t.Fatalf("refresh broke replay: %d %s", resp.StatusCode, out)
	}
	if cap.auth[len(cap.auth)-1] != "Bearer access-rotated" {
		t.Fatalf("rotated token not used: %v", cap.auth)
	}
}

func TestCodexChatNoKeySkipsMarker(t *testing.T) {
	cap := &codexChatCapture{sse: codexChatSSE("answer", "rs_1", "cipher-1", "sum-1")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := openTestStore(t, false)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK || bytes.Contains(out, []byte("airouter-codex-v1")) || !bytes.Contains(out, []byte("answer")) {
		t.Fatalf("no-key should stay readable: %d %s", resp.StatusCode, out)
	}
	marked := `{"model":"combo","messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.encrypted","format":"airouter-codex-v1","data":"x"}]},{"role":"user","content":"next"}]}`
	before := cap.n
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, marked)
	if resp.StatusCode != http.StatusBadRequest || cap.n != before {
		t.Fatalf("no-key marked request contacted upstream: %d calls=%d %s", resp.StatusCode, cap.n, out)
	}
}

func TestCodexChatRedirectDoesNotLeaveStandardHost(t *testing.T) {
	redirected := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		http.Error(w, "should not arrive", http.StatusOK)
	}))
	t.Cleanup(evil.Close)
	cap := &codexChatCapture{sse: codexChatSSE("answer", "rs_1", "cipher-1", "sum-1")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
	resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(out))
	}
	details := extractChatDetails(t, out)
	cap.redirect = evil.URL + "/responses"
	resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, replayBody("combo", details, "answer", "", false))
	if resp.StatusCode == http.StatusOK || redirected {
		t.Fatalf("redirect followed: status %d arrived=%v %s", resp.StatusCode, redirected, out)
	}
}

func TestCodexChatWireModelAndPortableFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			cap := &codexChatCapture{sse: codexChatSSE("answer", "rs_wire", "cipher-wire", "sum-wire", "fc_w|call_w|read|{\"path\":\"a\"}")}
			up := httptest.NewServer(cap)
			t.Cleanup(up.Close)
			st := newTestStore(t)
			a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
			b := addCodexChatProvider(t, st, "cb", "acct-b", "gen-b")
			if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
				{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex-high", Enabled: true},
				{ProviderID: b.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
			}}); err != nil {
				t.Fatal(err)
			}
			key, err := st.NewAccessKey(context.Background(), "k")
			if err != nil {
				t.Fatal(err)
			}
			ts := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{
				"Bearer access-acct-a": up.URL,
				"Bearer access-acct-b": up.URL,
			}})
			seed := fmt.Sprintf(`{"model":"combo","stream":%t,"reasoning_effort":"high","messages":[{"role":"user","content":"first"}]}`, stream)
			resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, seed)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("seed %d %s", resp.StatusCode, out)
			}
			if !strings.Contains(cap.lastBody(), `"model":"gpt-5.3-codex-high"`) || strings.Contains(cap.lastBody(), "cipher-wire") {
				t.Fatalf("seed wire: %s", cap.lastBody())
			}
			details := extractChatDetails(t, out)
			same := replayBody("combo", details, "answer", `[{"id":"call_w","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]`, stream)
			before := cap.n
			resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, same)
			if resp.StatusCode != http.StatusOK || cap.n != before+1 {
				t.Fatalf("same effort %d calls=%d %s", resp.StatusCode, cap.n-before, out)
			}
			sent := cap.lastBody()
			for _, want := range []string{`"model":"gpt-5.3-codex-high"`, "cipher-wire", `"id":"rs_wire"`, "call_w", "file text"} {
				if !strings.Contains(sent, want) {
					t.Fatalf("same effort missing %s in %s", want, sent)
				}
			}
			changed := bytes.Replace([]byte(same), []byte(`,"reasoning_effort":"high"`), nil, 1)
			before = cap.n
			auth := len(cap.auth)
			resp, out = chatPost(t, ts.URL+"/v1/chat/completions", key.Token, string(changed))
			if resp.StatusCode != http.StatusOK || cap.n != before+1 || len(cap.auth) != auth+1 {
				t.Fatalf("changed effort looped or failed: %d calls=%d auth=%d %s", resp.StatusCode, cap.n-before, len(cap.auth)-auth, out)
			}
			sent = cap.lastBody()
			if strings.Contains(sent, "cipher-wire") || strings.Contains(sent, "rs_wire") || !strings.Contains(sent, `"model":"gpt-5.3-codex"`) || strings.Contains(sent, "gpt-5.3-codex-high") || !strings.Contains(sent, "answer") {
				t.Fatalf("changed effort leaked or dropped history: %s", sent)
			}
			if cap.auth[len(cap.auth)-1] != "Bearer access-acct-a" {
				t.Fatalf("changed effort left the signed route: %v", cap.auth)
			}
		})
	}
}

func TestCodexChatMisplacedMarkerDoesNotReachPassthrough(t *testing.T) {
	cap := &codexChatCapture{sse: codexChatSSE("seed", "rs_hide", "only-synthetic-review-state", "summary")}
	orig := httptest.NewServer(cap)
	t.Cleanup(orig.Close)
	plainCap := &chatCapture{sse: "data: {\"choices\":[{\"delta\":{\"content\":\"plain\"}}]}\n\ndata: [DONE]\n\n"}
	plain := httptest.NewServer(plainCap)
	t.Cleanup(plain.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	b := &domain.Provider{Name: "plain", BaseURL: plain.URL, Protocol: domain.ProtocolOpenAI, APIKey: "mock"}
	if err := st.CreateProvider(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	combo := &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true},
		{ProviderID: b.ID, UpstreamModel: "plain", Enabled: true},
	}}
	if err := st.CreateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	srv := startChatProxyTransport(t, st, authRouteTransport{byAuth: map[string]string{"Bearer access-acct-a": orig.URL}, plain: plain.URL})
	endpoint := srv.URL + "/v1/chat/completions"
	resp, out := chatPost(t, endpoint, key.Token, `{"model":"combo","messages":[{"role":"user","content":"first"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed %d %s", resp.StatusCode, out)
	}
	emitted := extractChatDetails(t, out)
	details := string(mustChatJSON(emitted))
	var encrypted string
	for _, item := range emitted {
		if bytes.Contains(item, []byte(`"type":"reasoning.encrypted"`)) {
			encrypted = string(item)
			break
		}
	}
	if encrypted == "" {
		t.Fatal("seed produced no encrypted detail")
	}
	var fields strings.Builder
	for i := range 160 {
		fmt.Fprintf(&fields, `"field_%d":null,`, i)
	}
	wide := fields.String()
	combo.Targets[0].Enabled = false
	if err := st.UpdateCombo(context.Background(), combo); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"model":"combo","metadata":{"extra":` + details + `},"stream":true,"messages":[{"role":"user","content":"first"}]}`,
		`{"model":"combo","stream":true,"messages":[{"role":"assistant","content":"seed","reasoning_details":` + details + `,"reasoning_details":[]},{"role":"user","content":"next"}]}`,
		`{"model":"combo","stream":true,"messages":[{"role":"assistant","content":"seed","reasoning_details":` + details + `}],"messages":[{"role":"user","content":"clean"}]}`,
		`{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}],` + encrypted[1:],
		`{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}],"metadata":{` + wide + `"nested":` + encrypted + `}}`,
		`{"model":"combo","stream":true,"messages":[{"role":"user","content":"first"}],"metadata":` + strings.Repeat(`[`, 64) + encrypted + strings.Repeat(`]`, 64) + `}`,
	}
	for i, body := range cases {
		before := plainCap.n
		resp, out = chatPost(t, endpoint, key.Token, body)
		if resp.StatusCode != http.StatusBadRequest || plainCap.n != before || bytes.Contains([]byte(plainCap.lastBody()), []byte("only-synthetic-review-state")) {
			t.Fatalf("case %d status=%d calls=%d body=%s resp=%s", i, resp.StatusCode, plainCap.n-before, plainCap.lastBody(), out)
		}
	}
	// A valid marked turn with unrelated numeric metadata must still go through
	// scrubbing rather than the original-body passthrough.
	largeNumber := strings.Replace(details, encrypted, `{"unrelated":1e400,`+encrypted[1:], 1)
	var numericDetails []json.RawMessage
	if err := json.Unmarshal([]byte(largeNumber), &numericDetails); err != nil {
		t.Fatal(err)
	}
	marked := replayBody("combo", numericDetails, "seed", "", true)
	before := plainCap.n
	resp, out = chatPost(t, endpoint, key.Token, marked)
	if resp.StatusCode != http.StatusOK || plainCap.n != before+1 ||
		strings.Contains(plainCap.lastBody(), "only-synthetic-review-state") || !strings.Contains(plainCap.lastBody(), "summary") {
		t.Fatalf("numeric metadata did not preserve readable fallback: status=%d calls=%d", resp.StatusCode, plainCap.n-before)
	}

	ordinary := `{"model":"combo","stream":true,"messages":[{"role":"user","content":"mentions airouter-codex-v1"}],"content":"dup","content":"kept","metadata":{` + wide + `"format":"third-party","number":1e400}}`
	before = plainCap.n
	resp, out = chatPost(t, endpoint, key.Token, ordinary)
	if resp.StatusCode != http.StatusOK || plainCap.n != before+1 ||
		strings.Contains(plainCap.lastBody(), "only-synthetic-review-state") || !strings.Contains(plainCap.lastBody(), `"number":1e400`) {
		t.Fatalf("ordinary metadata rejected or changed: status=%d calls=%d", resp.StatusCode, plainCap.n-before)
	}
}

func TestCodexChatEmptyCipherIsAuthoritative(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			cap := &codexChatCapture{}
			up := httptest.NewServer(cap)
			t.Cleanup(up.Close)
			st := newTestStore(t)
			a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
			if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			key, err := st.NewAccessKey(context.Background(), "k")
			if err != nil {
				t.Fatal(err)
			}
			ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
			post := func(sse, body string) (int, []byte) {
				cap.sse = sse
				resp, out := chatPost(t, ts.URL+"/v1/chat/completions", key.Token, body)
				return resp.StatusCode, out
			}
			seed := fmt.Sprintf(`{"model":"combo","stream":%t,"messages":[{"role":"user","content":"first"}]}`, stream)
			status, out := post(codexEmptySSE("", false, false), seed)
			if status != http.StatusOK || bytes.Contains(out, []byte("airouter-codex-v1")) {
				t.Fatalf("absent stamped: %d %s", status, out)
			}
			status, out = post(codexEmptySSE("provisional-only", true, false), seed)
			if status != http.StatusOK || bytes.Contains(out, []byte("airouter-codex-v1")) || bytes.Contains(out, []byte("provisional-only")) {
				t.Fatalf("provisional stamped: %d %s", status, out)
			}
			status, out = post(codexEmptySSE("", false, true), seed)
			if status != http.StatusOK || !bytes.Contains(out, []byte(`"data":""`)) || !bytes.Contains(out, []byte("airouter-codex-v1")) {
				t.Fatalf("empty not stamped: %d %s", status, out)
			}
			details := extractChatDetails(t, out)
			replay := replayBody("combo", details, "answer", "", stream)
			status, _ = post(codexChatSSE("next", "rs_2", "cipher-2", "sum-2"), replay)
			if status != http.StatusOK || !strings.Contains(cap.lastBody(), `"encrypted_content":""`) || strings.Contains(cap.lastBody(), "airouter-codex-v1") {
				t.Fatalf("empty replay: %d %s", status, cap.lastBody())
			}
			status, out = post(codexTerminalToolSSE(), seed)
			if status != http.StatusOK || !bytes.Contains(out, []byte("terminal-cipher")) {
				t.Fatalf("terminal-only missing: %d %s", status, out)
			}
			details = extractChatDetails(t, out)
			replay = replayBody("combo", details, "need tool", `[{"id":"call_pi","type":"function","function":{"name":"read","arguments":"{\"path\":\"a\"}"}}]`, stream)
			status, _ = post(codexChatSSE("ok", "rs_3", "cipher-3", "sum-3"), replay)
			sent := cap.lastBody()
			if status != http.StatusOK || !strings.Contains(sent, "terminal-cipher") || !strings.Contains(sent, "call_pi") || !strings.Contains(sent, "path") {
				t.Fatalf("terminal replay: %d %s", status, sent)
			}
		})
	}
}

func codexEmptySSE(provisional string, provisionalOnly, explicitEmpty bool) string {
	var b strings.Builder
	write := func(s string) { b.WriteString("data: " + s + "\n\n") }
	added := `{"type":"reasoning","id":"rs_empty","status":"in_progress","summary":[]}`
	if provisional != "" {
		added = `{"type":"reasoning","id":"rs_empty","status":"in_progress","summary":[],"encrypted_content":` + grokJSONString(provisional) + `}`
	}
	write(`{"type":"response.output_item.added","output_index":0,"item":` + added + `}`)
	done := `{"type":"reasoning","id":"rs_empty","status":"completed","summary":[{"type":"summary_text","text":"sum"}]}`
	if explicitEmpty {
		done = `{"type":"reasoning","id":"rs_empty","status":"completed","summary":[{"type":"summary_text","text":"sum"}],"encrypted_content":""}`
	}
	write(`{"type":"response.output_item.done","output_index":0,"item":` + done + `}`)
	write(`{"type":"response.output_text.delta","output_index":1,"delta":"answer"}`)
	terminal := done
	if provisionalOnly {
		terminal = `{"type":"reasoning","id":"rs_empty","status":"completed","summary":[{"type":"summary_text","text":"sum"}]}`
	}
	write(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"output":[` + terminal + `,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func codexTerminalToolSSE() string {
	var b strings.Builder
	write := func(s string) { b.WriteString("data: " + s + "\n\n") }
	write(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_term","status":"in_progress","summary":[]}}`)
	write(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_pi","call_id":"call_pi","name":"read","arguments":"","status":"in_progress"}}`)
	write(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\""}`)
	write(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":":\"a\"}"}`)
	write(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_pi","call_id":"call_pi","name":"read","arguments":"{\"path\":\"a\"}","status":"completed"}}`)
	write(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_term","status":"completed","summary":[{"type":"summary_text","text":"sum"}]}}`)
	write(`{"type":"response.output_text.delta","output_index":2,"delta":"need tool"}`)
	write(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"output":[{"type":"reasoning","id":"rs_term","status":"completed","summary":[{"type":"summary_text","text":"sum"}],"encrypted_content":"terminal-cipher"},{"type":"function_call","id":"fc_pi","call_id":"call_pi","name":"read","arguments":"{\"path\":\"a\"}","status":"completed"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"need tool"}]}]}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestCodexChatPiAdapterLoop(t *testing.T) {
	adapter := os.Getenv("AIROUTER_TEST_PI_CHAT_ADAPTER")
	if adapter == "" {
		t.Skip("AIROUTER_TEST_PI_CHAT_ADAPTER is unset")
	}
	cap := &codexChatCapture{sse: codexChatSSE("need tool", "rs_pi", "cipher-pi", "sum-pi", "fc_pi|call_pi|read|{}")}
	up := httptest.NewServer(cap)
	t.Cleanup(up.Close)
	st := newTestStore(t)
	a := addCodexChatProvider(t, st, "ca", "acct-a", "gen-a")
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "combo", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: a.ID, UpstreamModel: "gpt-5.3-codex", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ts := startChatProxyTransport(t, st, codexRouteTransport{base: up.URL})
	script := filepath.Join(t.TempDir(), "pi-codex-loop.mjs")
	if err := os.WriteFile(script, []byte(piCodexChatLoopScript), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", script, ts.URL, key.Token, adapter, "tool")
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("pi-codex-ok")) {
		t.Fatalf("pi tool loop: %v %s", err, out)
	}
	if !strings.Contains(cap.lastBody(), "cipher-pi") || !strings.Contains(cap.lastBody(), "call_pi") {
		t.Fatalf("pi replay missing state: %s", cap.lastBody())
	}
}

const piCodexChatLoopScript = `import { pathToFileURL } from "node:url";
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
const signature = message?.content.find((b) => b.type === "thinking")?.thinkingSignature || "";
const call = message?.content.find((b) => b.type === "toolCall");
if (!signature.includes("cipher-pi") || !signature.includes("airouter-codex-v1") || call?.id !== "call_pi") {
  throw new Error("missing signature or tool call");
}
context.messages.push(message);
context.messages.push({ role: "toolResult", toolCallId: call.id, toolName: call.name, content: [{ type: "text", text: "file text" }], timestamp: 2 });
const second = stream(model, context, { apiKey: token, fetch: fetchImpl });
for await (const ev of second) {
  if (ev.type === "error") throw new Error(ev.error.errorMessage || "second stream error");
}
console.log("pi-codex-ok");
`
