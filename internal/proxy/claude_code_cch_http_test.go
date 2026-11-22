package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/harlog"
	"airouter/internal/proxy/claudecode"
)

// The reference below is independent of claudecode.ApplyCCH. Integration tests
// must fail when the send path stamps the wrong body, not only when the unit
// helper disagrees with itself.

func TestClaudeCodeCCHIngress(t *testing.T) {
	requireCCHReference(t)
	baseURL := "https://api.anthropic.com/v1"
	captures := make(chan cchWire, 8)
	base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
		protocol: domain.ProtocolClaudeCode, baseURL: baseURL, model: "claude-sonnet-5",
	}}, func(req *http.Request, body []byte) *http.Response {
		assertCCHRequestBody(t, req, body)
		captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), body...)}
		return cchHTTPReply(req, body, http.StatusOK)
	})
	wantURL := cchUpstreamURL(baseURL)
	for _, ingress := range cchIngresses() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", ingress.name, stream), func(t *testing.T) {
				resp, out := postSelector(t, client, base+ingress.path, token, cchStreamBody(ingress.body, stream), nil)
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
					t.Fatalf("status %d body %s", resp.StatusCode, out)
				}
				wire := cchCapture(t, captures)
				if wire.url != wantURL {
					t.Fatalf("url %s want %s", wire.url, wantURL)
				}
				assertCCHPayload(t, wire.body, ingress.name == "messages")
				assertCCHSession(t, wire.header, wire.body)
				assertFinalCCH(t, wire.url, wire.body, true)
				if strings.Contains(string(wire.body), "up-key") {
					t.Fatal("credential leaked into body")
				}
			})
		}
	}
}

func TestClaudeCodeCCHHosts(t *testing.T) {
	requireCCHReference(t)
	cases := []struct {
		name       string
		base       string
		active     bool
		literal    bool
		allIngress bool
	}{
		{name: "official", base: "https://api.anthropic.com/v1", active: true, allIngress: true},
		{name: "official-https-443", base: "https://api.anthropic.com:443/v1", active: true},
		{name: "official-http-80", base: "http://api.anthropic.com:80/v1", active: true},
		{name: "official-host-case", base: "https://API.ANTHROPIC.COM/v1", active: true},
		{name: "official-no-v1", base: "https://api.anthropic.com", literal: true},
		{name: "official-v1-case", base: "https://api.anthropic.com/V1", literal: true},
		{name: "nondefault-port", base: "https://api.anthropic.com:8443/v1"},
		{name: "lookalike-suffix", base: "https://api.anthropic.com.evil.invalid/v1"},
		{name: "lookalike-path", base: "https://evil.invalid/api.anthropic.com"},
		{name: "custom-v1", base: "https://beta-custom.invalid/v1", allIngress: true},
		{name: "custom-no-v1", base: "https://beta-custom.invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captures := make(chan cchWire, 8)
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
				protocol: domain.ProtocolClaudeCode, baseURL: tc.base, model: "claude-sonnet-5",
			}}, func(req *http.Request, body []byte) *http.Response {
				assertCCHRequestBody(t, req, body)
				captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), body...)}
				return cchHTTPReply(req, body, http.StatusOK)
			})
			ingresses := cchIngresses()[2:]
			if tc.allIngress {
				ingresses = cchIngresses()
			}
			for _, ingress := range ingresses {
				for _, stream := range []bool{false, true} {
					resp, out := postSelector(t, client, base+ingress.path, token, cchStreamBody(ingress.body, stream), nil)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("%s stream %v status %d: %s", ingress.name, stream, resp.StatusCode, out)
					}
					wire := cchCapture(t, captures)
					if wire.url != cchUpstreamURL(tc.base) {
						t.Fatalf("url %s", wire.url)
					}
					assertCCHPayload(t, wire.body, ingress.name == "messages")
					assertCCHSession(t, wire.header, wire.body)
					assertFinalCCH(t, wire.url, wire.body, tc.active)
					bill := cchBillingText(t, wire.body)
					if tc.active || tc.literal {
						if !strings.Contains(bill, "cch=") {
							t.Fatalf("official billing lost cch: %s", bill)
						}
					} else if strings.Contains(bill, "cch=") {
						t.Fatalf("non-official billing gained cch: %s", bill)
					}
					if tc.literal && !strings.Contains(bill, "cch=00000;") {
						t.Fatalf("inactive official url was patched: %s", bill)
					}
				}
			}
		})
	}
}

func TestClaudeCodeCCHNoHostGate(t *testing.T) {
	requireCCHReference(t)
	const baseURL = "https://not-official.invalid/v1"
	body := `{"model":"default","max_tokens":32,"system":[{"type":"text","text":"cch=00000","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],"messages":[{"role":"user","content":"hi"}]}`
	captures := make(chan cchWire, 2)
	base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
		protocol: domain.ProtocolClaudeCode, baseURL: baseURL, model: "claude-sonnet-5",
	}}, func(req *http.Request, got []byte) *http.Response {
		captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), got...)}
		return cchHTTPReply(req, got, http.StatusOK)
	})
	resp, out := postSelector(t, client, base+"/v1/messages", token, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	wire := cchCapture(t, captures)
	if wire.url != cchUpstreamURL(baseURL) || !strings.Contains(wire.url, "/v1/messages") || strings.Contains(wire.url, "api.anthropic.com") {
		t.Fatalf("url %s", wire.url)
	}
	bill := cchBillingText(t, wire.body)
	if strings.Contains(bill, "cch=") {
		t.Fatalf("builder added cch on a custom host: %s", bill)
	}
	assertFinalCCH(t, wire.url, wire.body, true)
	if !strings.Contains(string(wire.body), `"ttl":"1h"`) || !strings.Contains(string(wire.body), `"scope":"global"`) {
		t.Fatalf("caller cache changed: %s", wire.body)
	}
}

func TestClaudeCodeCCHRefresh(t *testing.T) {
	requireCCHReference(t)
	for _, tc := range []struct {
		name    string
		account string
	}{
		{name: "same-account", account: "acct-cch-old"},
		{name: "account-change", account: "acct-cch-new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
					const endpoint = "https://cch-token.invalid/refresh"
					const oldToken = "sk-ant-oat-cch-old"
					const newToken = "sk-ant-oat-cch-new"
					captures := make(chan cchWire, 4)
					var hits atomic.Int32
					creds := &domain.OAuthCreds{
						ClaudeCodeAuth: true, AccessToken: oldToken, RefreshToken: "rt-old", ClientID: "cid-synthetic",
						TokenURL: endpoint, ExpiresAt: time.Now().Add(time.Hour).Unix(), ClaudeScopes: "user:inference",
						ClaudeAccountUUID: "acct-cch-old", ClaudeDeviceID: strings.Repeat("ab", 32),
					}
					base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
						protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5", oauth: creds,
					}}, func(req *http.Request, body []byte) *http.Response {
						if req.URL.String() == endpoint {
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": []string{"application/json"}},
								Body:       io.NopCloser(strings.NewReader(`{"access_token":"` + newToken + `","refresh_token":"rt-new","expires_in":3600,"scope":"user:inference","account":{"uuid":"` + tc.account + `"}}`)),
							}
						}
						assertCCHRequestBody(t, req, body)
						captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), body...)}
						status := http.StatusOK
						if hits.Add(1) == 1 {
							status = http.StatusUnauthorized
						}
						return cchHTTPReply(req, body, status)
					})
					client.Transport.(*selectorTransport).allow(t, http.MethodPost, endpoint)
					resp, out := postSelector(t, client, base+"/v1/messages", token, cchStreamBody(cchIngresses()[2].body, stream), map[string]string{
						"X-Claude-Code-Session-Id": "11111111-1111-4111-8111-111111111111",
					})
					if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
						t.Fatalf("status %d hits %d: %s", resp.StatusCode, hits.Load(), out)
					}
					first, second := cchCapture(t, captures), cchCapture(t, captures)
					if first.header.Get("Authorization") != "Bearer "+oldToken || second.header.Get("Authorization") != "Bearer "+newToken {
						t.Fatalf("auth %s / %s", first.header.Get("Authorization"), second.header.Get("Authorization"))
					}
					assertCCHRefreshBody(t, first, "acct-cch-old", oldToken, newToken)
					assertCCHRefreshBody(t, second, tc.account, oldToken, newToken)
					if tc.account == "acct-cch-old" {
						if first.header.Get(claudecode.SessionIDHeader) != second.header.Get(claudecode.SessionIDHeader) {
							t.Fatal("same account changed session")
						}
					} else if first.header.Get(claudecode.SessionIDHeader) == second.header.Get(claudecode.SessionIDHeader) {
						t.Fatal("account change reused the session")
					}
				})
			}
		})
	}
}

func TestClaudeCodeCCHFailover(t *testing.T) {
	requireCCHReference(t)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("official/stream=%v", stream), func(t *testing.T) {
			captures := make(chan cchWire, 4)
			var hits atomic.Int32
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com:443/v1", model: "claude-haiku-4-5"},
			}, func(req *http.Request, body []byte) *http.Response {
				assertCCHRequestBody(t, req, body)
				captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), body...)}
				status := http.StatusOK
				if hits.Add(1) == 1 {
					status = http.StatusInternalServerError
				}
				return cchHTTPReply(req, body, status)
			})
			resp, out := postSelector(t, client, base+"/v1/messages", token, cchStreamBody(cchIngresses()[2].body, stream), nil)
			if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
				t.Fatalf("status %d hits %d: %s", resp.StatusCode, hits.Load(), out)
			}
			first, second := cchCapture(t, captures), cchCapture(t, captures)
			if first.url == second.url || !strings.Contains(string(first.body), `"model":"claude-sonnet-5"`) || !strings.Contains(string(second.body), `"model":"claude-haiku-4-5"`) {
				t.Fatalf("failover targets collapsed %s %s", first.url, second.url)
			}
			assertFinalCCH(t, first.url, first.body, true)
			assertFinalCCH(t, second.url, second.body, true)
			assertCCHSession(t, first.header, first.body)
			assertCCHSession(t, second.header, second.body)
		})
		t.Run(fmt.Sprintf("custom-backup/stream=%v", stream), func(t *testing.T) {
			captures := make(chan cchWire, 4)
			var hits atomic.Int32
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://beta-backup.invalid/v1", model: "claude-haiku-4-5"},
			}, func(req *http.Request, body []byte) *http.Response {
				captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), body...)}
				status := http.StatusOK
				if hits.Add(1) == 1 {
					status = http.StatusInternalServerError
				}
				return cchHTTPReply(req, body, status)
			})
			resp, out := postSelector(t, client, base+"/v1/chat/completions", token, cchStreamBody(cchIngresses()[0].body, stream), nil)
			if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
				t.Fatalf("status %d hits %d: %s", resp.StatusCode, hits.Load(), out)
			}
			first, second := cchCapture(t, captures), cchCapture(t, captures)
			assertFinalCCH(t, first.url, first.body, true)
			assertFinalCCH(t, second.url, second.body, false)
			if strings.Contains(cchBillingText(t, second.body), "cch=") {
				t.Fatal("custom failover target gained cch")
			}
		})
	}
}

func TestClaudeCodeCCHLogsAndHAR(t *testing.T) {
	requireCCHReference(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	const token = "sk-ant-oat-logcheck-9f3a"
	const payload = "payload-cch-log-9f3a"
	captures := make(chan cchWire, 4)
	var hits atomic.Int32
	base, access, client := newBetaPolicyProxy(t, []selectorTarget{
		{protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5", oauth: &domain.OAuthCreds{
			ClaudeCodeAuth: true, AccessToken: token, RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			ClaudeAccountUUID: "acct-log", ClaudeDeviceID: strings.Repeat("cd", 32), ClaudeScopes: "user:inference",
		}},
		{protocol: domain.ProtocolClaudeCode, baseURL: "https://beta-backup.invalid/v1", model: "claude-sonnet-5"},
	}, func(req *http.Request, body []byte) *http.Response {
		captures <- cchWire{url: req.URL.String(), header: req.Header.Clone(), body: append([]byte(nil), body...)}
		status := http.StatusOK
		if hits.Add(1) == 1 {
			status = http.StatusInternalServerError
		}
		return cchHTTPReply(req, body, status)
	})
	resp, out := postSelector(t, client, base+"/v1/messages", access, `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"`+payload+`"}]}`, nil)
	if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("status %d hits %d: %s", resp.StatusCode, hits.Load(), out)
	}
	first := cchCapture(t, captures)
	assertFinalCCH(t, first.url, first.body, true)
	if !strings.Contains(logs.String(), "upstream_attempt_failed") {
		t.Fatalf("missing attempt log: %s", logs.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), payload) || strings.Contains(logs.String(), "cch=") {
		t.Fatalf("log contained auth or body: %s", logs.String())
	}

	rec := harlog.New("cch")
	var harBody []byte
	var harLen int64
	var gotGetBody bool
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		harBody = append([]byte(nil), raw...)
		harLen = req.ContentLength
		if req.GetBody == nil {
			return nil, fmt.Errorf("GetBody is nil")
		}
		rc, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		again, _ := io.ReadAll(rc)
		rc.Close()
		gotGetBody = bytes.Equal(again, raw)
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rec, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		resp := rec.Result()
		resp.Request = req
		return resp, nil
	})
	p := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.client = &http.Client{Transport: transport}
	p.streamClient = &http.Client{Transport: transport}
	provider := &domain.Provider{
		ID: 9, Name: "cc", BaseURL: "https://api.anthropic.com/v1", Protocol: domain.ProtocolClaudeCode, APIKey: token,
	}
	ctx := WithTraceInfo(withClaudeRequest(context.Background(), http.Header{
		"X-Claude-Code-Session-Id": []string{"11111111-1111-4111-8111-111111111111"},
	}, nil), &TraceInfo{RequestID: "cch-req-1", HAR: rec})
	ingress := []byte(`{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"` + payload + `"}]}`)
	saved := append([]byte(nil), ingress...)
	status, respBody, err := p.forward(ctx, provider, claudecode.UpstreamPath, ingress, nil)
	if err != nil || status != http.StatusOK || !bytes.Equal(ingress, saved) {
		t.Fatalf("forward status %d err %v body %s", status, err, respBody)
	}
	if !gotGetBody || harLen != int64(len(harBody)) {
		t.Fatalf("GetBody %v ContentLength %d len %d", gotGetBody, harLen, len(harBody))
	}
	assertFinalCCH(t, cchUpstreamURL(provider.BaseURL), harBody, true)
	data, err := rec.MarshalHAR()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Log struct {
			Entries []struct {
				PageRef string `json:"pageref"`
				Request struct {
					URL      string `json:"url"`
					Headers  []struct{ Name, Value string }
					PostData *struct{ Text string } `json:"postData"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Log.Entries) != 1 || doc.Log.Entries[0].PageRef != "page_cch-req-1" {
		t.Fatalf("har entries %+v", doc.Log.Entries)
	}
	entry := doc.Log.Entries[0]
	if entry.Request.URL != cchUpstreamURL(provider.BaseURL) || entry.Request.PostData == nil || entry.Request.PostData.Text != string(harBody) {
		t.Fatalf("har request url %s post %v", entry.Request.URL, entry.Request.PostData)
	}
	foundAuth := false
	for _, h := range entry.Request.Headers {
		if strings.EqualFold(h.Name, "Authorization") && h.Value == "Bearer "+token {
			foundAuth = true
		}
	}
	if !foundAuth {
		t.Fatal("HAR lost the upstream authorization header")
	}
}

func TestClaudeCodeCCHDiscoveryUnchanged(t *testing.T) {
	requireCCHReference(t)
	body := []byte(`{"system":[{"text":"cch=00000"}],"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"payload-cch-keep"}]}`)
	var seen []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		seen = append(seen, req.URL.String()+"\n"+string(raw))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"claude-sonnet-5"}]}`)), Header: make(http.Header), Request: req}, nil
	})
	p := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.client = &http.Client{Transport: transport}
	provider := &domain.Provider{BaseURL: "https://api.anthropic.com/v1", Protocol: domain.ProtocolClaudeCode, APIKey: "sk-ant-discovery"}
	ctx := withClaudeRequest(context.Background(), nil, nil)
	status, _, err := p.forward(ctx, provider, "/models?limit=1000", body, nil)
	if err != nil || status != http.StatusOK || len(seen) != 1 || !strings.Contains(seen[0], "cch=00000") || !strings.Contains(seen[0], "/models?limit=1000") {
		t.Fatalf("discovery changed: status %d err %v seen %v", status, err, seen)
	}
	other := &domain.Provider{BaseURL: "https://api.anthropic.com/v1", Protocol: domain.ProtocolAnthropic, APIKey: "other"}
	status, _, err = p.forward(ctx, other, "/messages", append([]byte(nil), body...), nil)
	if err != nil || status != http.StatusOK || len(seen) != 2 || !strings.HasSuffix(seen[1], string(body)) {
		t.Fatalf("other protocol changed: status %d err %v seen %v", status, err, seen)
	}
	cursor := &domain.Provider{BaseURL: "https://cursor.example", Protocol: domain.ProtocolCursor, APIKey: "cursor"}
	status, _, err = p.forward(ctx, cursor, "/messages", append([]byte(nil), body...), nil)
	if err != nil || status != http.StatusOK || !strings.HasSuffix(seen[2], string(body)) {
		t.Fatalf("cursor path changed: %v %v", err, seen)
	}

	captures := make(chan cchWire, 2)
	var upstream atomic.Int32
	base, token, client := newBetaPolicyProxy(t, []selectorTarget{{
		protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5",
	}}, func(req *http.Request, got []byte) *http.Response {
		upstream.Add(1)
		captures <- cchWire{url: req.URL.String(), body: append([]byte(nil), got...)}
		return cchHTTPReply(req, got, http.StatusOK)
	})
	tr := client.Transport.(*selectorTransport)
	tr.allow(t, http.MethodGet, base+"/v1/models")
	tr.allow(t, http.MethodGet, base+"/models")
	for _, path := range []string{"/v1/models", "/models"} {
		resp, out := getSelector(t, client, base+path, token)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), `"id":"default"`) {
			t.Fatalf("%s status %d body %s", path, resp.StatusCode, out)
		}
	}
	if upstream.Load() != 0 {
		t.Fatal("client model list contacted upstream")
	}
}

func TestClaudeCodeCCHZeroChecksum(t *testing.T) {
	requireCCHReference(t)
	const rawURL = "https://api.anthropic.com/v1/messages"
	body := []byte(`{"system":[{"text":"cch=00000"}],"nonce":841999}`)
	saved := bytes.Clone(body)
	included, ok := cchRefIncluded(body)
	if !ok {
		t.Fatal("reference exclusion scan failed")
	}
	if digest := cchRefXXH64(included); digest != 0x1fc4d9a1fe000000 {
		t.Fatalf("reference digest = %016x, want 1fc4d9a1fe000000", digest)
	}
	if want := cchRefCandidate(rawURL, body); want != "00000" {
		t.Fatalf("reference checksum = %q, want 00000", want)
	}
	out := claudecode.ApplyCCH(rawURL, body)
	if !bytes.Equal(out, saved) || !bytes.Equal(body, saved) {
		t.Fatal("zero checksum changed the body or input buffer")
	}
	assertFinalCCH(t, rawURL, out, true)
}

func TestClaudeCodeCCHInputUnmodified(t *testing.T) {
	requireCCHReference(t)
	provider := &domain.Provider{ID: 4, BaseURL: "https://api.anthropic.com/v1", Protocol: domain.ProtocolClaudeCode, APIKey: "sk-test"}
	ctx := withClaudeRequest(context.Background(), nil, nil)
	ingress := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"payload-cch-keep"}]}`)
	saved := append([]byte(nil), ingress...)
	first, err := claudeWireBody(ctx, provider, claudecode.UpstreamPath, ingress)
	if err != nil {
		t.Fatal(err)
	}
	second, err := claudeWireBody(ctx, provider, claudecode.UpstreamPath, ingress)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ingress, saved) || !bytes.Equal(first, second) {
		t.Fatal("send path mutated the ingress body or reused checksum state")
	}
	assertFinalCCH(t, cchUpstreamURL(provider.BaseURL), first, true)
}

type cchWire struct {
	url    string
	header http.Header
	body   []byte
}

func cchIngresses() []struct{ name, path, body string } {
	return []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"payload-cch-keep"}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]}`},
		{"responses", "/v1/responses", `{"model":"default","input":"payload-cch-keep","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}`},
		{"messages", "/v1/messages", `{"model":"default","max_tokens":32,"system":[{"type":"text","text":"keep","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],"messages":[{"role":"user","content":"payload-cch-keep"}],"tools":[{"name":"search","input_schema":{"type":"object"}}]}`},
	}
}

func cchStreamBody(body string, stream bool) string {
	if !stream {
		return body
	}
	return strings.TrimSuffix(body, "}") + `,"stream":true}`
}

func cchUpstreamURL(base string) string {
	return strings.TrimRight(base, "/") + claudecode.UpstreamPath
}

func cchHTTPReply(req *http.Request, body []byte, status int) *http.Response {
	if status != http.StatusOK {
		return betaPolicyHTTPReply(req, body, status)
	}
	rec := httptest.NewRecorder()
	profileReply(rec, strings.Contains(req.Header.Get("Accept"), "text/event-stream") || strings.Contains(string(body), `"stream":true`))
	resp := rec.Result()
	resp.Request = req
	return resp
}

func cchCapture(t *testing.T, ch <-chan cchWire) cchWire {
	t.Helper()
	select {
	case wire := <-ch:
		return wire
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream capture")
		return cchWire{}
	}
}

func assertCCHRequestBody(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	if req.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength %d want %d", req.ContentLength, len(body))
	}
	if req.GetBody == nil {
		t.Fatal("GetBody is nil")
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("GetBody mismatch err %v", err)
	}
}

func assertCCHPayload(t *testing.T, body []byte, messages bool) {
	t.Helper()
	raw := string(body)
	if !strings.Contains(raw, "payload-cch-keep") || !strings.Contains(raw, `"name":"search"`) || !strings.Contains(raw, `"model":"claude-sonnet-5"`) {
		t.Fatalf("payload changed: %s", body)
	}
	if messages {
		if !strings.Contains(raw, `"ttl":"1h"`) || !strings.Contains(raw, `"scope":"global"`) || !strings.Contains(raw, `"text":"keep"`) {
			t.Fatalf("caller cache changed: %s", body)
		}
	} else if !strings.Contains(raw, `"ttl":"5m"`) {
		t.Fatalf("default cache missing: %s", body)
	}
	fp := claudecode.Fingerprint("payload-cch-keep")
	if !strings.Contains(raw, "cc_version="+claudecode.CLIVersion+"."+fp+";") {
		t.Fatalf("cc_version fingerprint missing: %s", body)
	}
}

func assertCCHSession(t *testing.T, header http.Header, body []byte) {
	t.Helper()
	var doc struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var id claudecode.UserID
	if err := json.Unmarshal([]byte(doc.Metadata.UserID), &id); err != nil {
		t.Fatal(err)
	}
	if id.SessionID == "" || header.Get(claudecode.SessionIDHeader) != id.SessionID || !strings.Contains(string(body), id.SessionID) {
		t.Fatalf("session header %q body %q", header.Get(claudecode.SessionIDHeader), id.SessionID)
	}
}

func assertCCHRefreshBody(t *testing.T, wire cchWire, account, oldToken, newToken string) {
	t.Helper()
	assertFinalCCH(t, wire.url, wire.body, true)
	assertCCHSession(t, wire.header, wire.body)
	if strings.Contains(string(wire.body), oldToken) || strings.Contains(string(wire.body), newToken) {
		t.Fatal("token leaked into body")
	}
	var doc struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(wire.body, &doc); err != nil {
		t.Fatal(err)
	}
	var id claudecode.UserID
	if err := json.Unmarshal([]byte(doc.Metadata.UserID), &id); err != nil || id.AccountUUID != account || id.DeviceID != strings.Repeat("ab", 32) {
		t.Fatalf("identity account %q device %q body %s", id.AccountUUID, id.DeviceID, wire.body)
	}
}

func cchBillingText(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.System) == 0 {
		t.Fatalf("billing missing: %v %s", err, body)
	}
	text, _ := doc.System[0]["text"].(string)
	return text
}

func getSelector(t *testing.T, client *http.Client, rawURL, token string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

func assertFinalCCH(t *testing.T, rawURL string, body []byte, wantActive bool) {
	t.Helper()
	requireCCHReference(t)
	// A valid 20-bit checksum can equal the builder placeholder.
	want, at, ok := cchRefPatched(rawURL, body)
	if ok != wantActive {
		t.Fatalf("activation %v want %v url %s", ok, wantActive, rawURL)
	}
	if !ok {
		return
	}
	if string(body[at+4:at+9]) != want {
		t.Fatalf("cch %s want %s at %d", body[at+4:at+9], want, at)
	}
	restored := bytes.Clone(body)
	copy(restored[at+4:at+9], []byte("00000"))
	if !bytes.Equal(body[:at+4], restored[:at+4]) || !bytes.Equal(body[at+9:], restored[at+9:]) {
		t.Fatal("checksum patch changed bytes outside the five digits")
	}
}

var (
	cchRefOnce sync.Once
	cchRefErr  error
)

func requireCCHReference(t *testing.T) {
	t.Helper()
	cchRefOnce.Do(func() { cchRefErr = checkCCHReferencePins() })
	if cchRefErr != nil {
		t.Fatal(cchRefErr)
	}
}

func checkCCHReferencePins() error {
	pins := []struct {
		data   []byte
		digest uint64
	}{
		{nil, 0xB8B30E7DE65B46C5},
		{[]byte("a"), 0xDE28AE28B0E07BB2},
		{bytes.Repeat([]byte("x"), 32), 0x7AA9F75436524E7D},
		{bytes.Repeat([]byte("x"), 33), 0x887C9E32DDAE69A1},
	}
	for i, pin := range pins {
		if got := cchRefXXH64(pin.data); got != pin.digest {
			return fmt.Errorf("reference hash pin %d = %016x", i, got)
		}
	}
	basic := []byte(`{"model":"claude-x","max_tokens":8,"system":[{"type":"text","text":" cch=00000;"}],"messages":[]}`)
	checks := []struct {
		url, cch string
		body     []byte
	}{
		{"https://api.anthropic.com/v1/messages", "8ca93", basic},
		{"https://api.anthropic.com/v1/messages?beta=true", "8ca93", basic},
		{"/v1/messages", "8ca93", basic},
		{"https://api.anthropic.com/v1/complete", "", basic},
		{"/v1/MessagesXXXX", "", basic},
		{"https://api.anthropic.com/v1/messages", "81da4", []byte(`{"model":"m","max_tokens":12,"system":[{"text":"cch=00000"}]}`)},
		{"https://api.anthropic.com/v1/messages", "81da4", []byte(`{"model":"m","max_tokens":99,"system":[{"text":"cch=00000"}]}`)},
		{"https://api.anthropic.com/v1/messages", "1d70d", []byte(`x,"max_tokens":3,{"system":[cch=00000]}`)},
		{"https://api.anthropic.com/v1/messages", "c27b1", []byte(`x,"max_tokens":3}{"system":[cch=00000]}`)},
		{"https://api.anthropic.com/v1/messages", "0d8ef", []byte(`{"fallback_credit_token":"a\"b","system":[{"text":"cch=00000"}]}`)},
		{"https://api.anthropic.com/v1/messages", "8ed50", []byte(`{"fallback_credit_token":"a\"z","system":[{"text":"cch=00000"}]}`)},
		{"https://api.anthropic.com/v1/messages", "44dc6", cchRefWindow(281)},
		{"https://api.anthropic.com/v1/messages", "", cchRefWindow(282)},
	}
	for _, check := range checks {
		got := cchRefCandidate(check.url, check.body)
		if got != check.cch {
			return fmt.Errorf("reference %q = %q, want %q", check.url, got, check.cch)
		}
	}
	return nil
}

func cchRefWindow(offset int) []byte {
	body := append([]byte("{"), []byte(`"system":[`)...)
	body = append(body, bytes.Repeat([]byte("A"), offset)...)
	return append(body, []byte("cch=00000]}")...)
}

func cchRefPatched(rawURL string, body []byte) (string, int, bool) {
	marker := bytes.Index(body, []byte(`"system":[`))
	if marker < 0 {
		return "", -1, false
	}
	windowEnd := marker + 300
	if windowEnd > len(body) {
		windowEnd = len(body)
	}
	for start := marker; start+9 <= windowEnd; {
		rel := bytes.Index(body[start:windowEnd], []byte("cch="))
		if rel < 0 {
			return "", -1, false
		}
		at := start + rel
		if at+9 > windowEnd {
			return "", -1, false
		}
		if cchRefHex5(body[at+4 : at+9]) {
			restored := bytes.Clone(body)
			copy(restored[at+4:at+9], []byte("00000"))
			if cchRefActivate(rawURL, restored) == at {
				return cchRefCandidate(rawURL, restored), at, true
			}
		}
		start = at + 1
	}
	return "", -1, false
}

func cchRefHex5(b []byte) bool {
	if len(b) != 5 {
		return false
	}
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func cchRefCandidate(rawURL string, body []byte) string {
	if cchRefActivate(rawURL, body) < 0 {
		return ""
	}
	parts, ok := cchRefIncluded(body)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%05x", cchRefXXH64(parts)&0xFFFFF)
}

func cchRefActivate(rawURL string, body []byte) int {
	if len(rawURL) <= 11 || !bytes.Contains([]byte(rawURL), []byte("/v1/messages")) {
		return -1
	}
	if len(body) < 10 {
		return -1
	}
	marker := bytes.Index(body, []byte(`"system":[`))
	if marker < 0 {
		return -1
	}
	windowEnd := marker + 300
	if windowEnd > len(body) {
		windowEnd = len(body)
	}
	return cchRefFind(body, []byte("cch=00000"), marker, windowEnd)
}

func cchRefFind(body, needle []byte, start, end int) int {
	if start < 0 {
		start = 0
	}
	if end > len(body) {
		end = len(body)
	}
	if start >= end || len(needle) == 0 || end-start < len(needle) {
		return -1
	}
	rel := bytes.Index(body[start:end], needle)
	if rel < 0 {
		return -1
	}
	return start + rel
}

func cchRefFindFrom(body, needle []byte, start int) int {
	if start < 0 || start > len(body) || len(needle) == 0 {
		return -1
	}
	rel := bytes.Index(body[start:], needle)
	if rel < 0 {
		return -1
	}
	return start + rel
}

func cchRefIncluded(body []byte) ([]byte, bool) {
	size := len(body)
	cursor := 0
	var parts [][]byte
	for steps := 0; ; steps++ {
		if steps > size+2 {
			return nil, false
		}
		span, found := cchRefExclusion(body, cursor)
		model := -1
		if size-cursor >= 9 {
			if marker := cchRefFindFrom(body, []byte(`"model":"`), cursor); marker >= 0 {
				model = marker
			}
		}
		if found && !(model >= 0 && span[0] >= model) {
			if span[1] <= cursor || span[0] < cursor || span[0] > size || span[1] > size {
				return nil, false
			}
			parts = append(parts, body[cursor:span[0]])
			cursor = span[1]
			continue
		}
		if model < 0 {
			parts = append(parts, body[cursor:])
			break
		}
		quote := cchRefFindFrom(body, []byte{'"'}, model+9)
		if quote < 0 {
			parts = append(parts, body[cursor:])
			break
		}
		parts = append(parts, body[cursor:model+9])
		cursor = quote
	}
	return bytes.Join(parts, nil), true
}

func cchRefExclusion(body []byte, cursor int) ([2]int, bool) {
	size := len(body)
	if cursor >= size || size-cursor < 13 {
		return [2]int{}, false
	}
	var best [2]int
	found := false
	if size-cursor > 12 {
		if marker := cchRefFindFrom(body, []byte(`"fallbacks":[`), cursor); marker >= 0 && marker+13 < size {
			if end, ok := cchRefBrackets(body, marker+13); ok {
				best = cchRefSpan(body, cursor, marker, end+1)
				found = true
			}
		}
	}
	if size-cursor >= 25 {
		if marker := cchRefFindFrom(body, []byte(`"fallback_credit_token":"`), cursor); marker >= 0 {
			if quote := cchRefFindFrom(body, []byte{'"'}, marker+25); quote >= 0 {
				span := cchRefSpan(body, cursor, marker, quote+1)
				if !found || span[0] < best[0] {
					best = span
					found = true
				}
			}
		}
	}
	search := cursor
	for size-search >= 13 {
		marker := cchRefFindFrom(body, []byte(`"max_tokens":`), search)
		if marker < 0 || marker+13 >= size {
			break
		}
		index := marker + 13
		for index < size && body[index] >= '0' && body[index] <= '9' {
			index++
		}
		if index == marker+13 {
			search = marker + 13
			continue
		}
		span := cchRefSpan(body, cursor, marker, index)
		if !found || span[0] < best[0] {
			best = span
			found = true
		}
		break
	}
	return best, found
}

func cchRefSpan(body []byte, cursor, marker, contentEnd int) [2]int {
	if contentEnd < len(body) && body[contentEnd] == ',' {
		return [2]int{marker, contentEnd + 1}
	}
	until := marker
	if marker > cursor && body[marker-1] == ',' {
		until = marker - 1
	}
	return [2]int{until, contentEnd}
}

func cchRefBrackets(body []byte, index int) (int, bool) {
	depth := 1
	inString := false
	for index < len(body) {
		b := body[index]
		if inString {
			if b == '"' {
				inString = false
				index++
				continue
			}
			if b == '\\' && index+1 < len(body) {
				index += 2
				continue
			}
			index++
			continue
		}
		switch b {
		case '"':
			inString = true
			index++
		case '[':
			depth++
			index++
		case ']':
			depth--
			if depth == 0 {
				return index, true
			}
			index++
		default:
			index++
		}
	}
	return 0, false
}

func cchRefRound(acc, lane uint64) uint64 {
	acc += lane * 0xC2B2AE3D27D4EB4F
	acc = bits.RotateLeft64(acc, 31)
	return acc * 0x9E3779B185EBCA87
}

func cchRefXXH64(data []byte) uint64 {
	const (
		seed = 0x4D659218E32A3268
		p1   = 0x9E3779B185EBCA87
		p2   = 0xC2B2AE3D27D4EB4F
		p3   = 0x165667B19E3779F9
		p4   = 0x85EBCA77C2B2AE63
		p5   = 0x27D4EB2F165667C5
	)
	size := len(data)
	var digest uint64
	index := 0
	if size >= 32 {
		var accs [4]uint64
		accs[0] = seed
		accs[0] += p1
		accs[0] += p2
		accs[1] = seed
		accs[1] += p2
		accs[2] = seed
		accs[3] = seed
		accs[3] -= p1
		for index+32 <= size {
			for lane := 0; lane < 4; lane++ {
				start := index + 8*lane
				accs[lane] = cchRefRound(accs[lane], binary.LittleEndian.Uint64(data[start:start+8]))
			}
			index += 32
		}
		digest = bits.RotateLeft64(accs[0], 1) + bits.RotateLeft64(accs[1], 7) + bits.RotateLeft64(accs[2], 12) + bits.RotateLeft64(accs[3], 18)
		for _, acc := range accs {
			digest ^= cchRefRound(0, acc)
			digest = digest*p1 + p4
		}
		index = (size / 32) * 32
	} else {
		digest = seed + p5
	}
	digest += uint64(size)
	for index+8 <= size {
		lane := binary.LittleEndian.Uint64(data[index : index+8])
		digest ^= cchRefRound(0, lane)
		digest = bits.RotateLeft64(digest, 27)*p1 + p4
		index += 8
	}
	if index+4 <= size {
		lane := uint64(binary.LittleEndian.Uint32(data[index : index+4]))
		digest ^= lane * p1
		digest = bits.RotateLeft64(digest, 23)*p2 + p3
		index += 4
	}
	for index < size {
		digest ^= uint64(data[index]) * p5
		digest = bits.RotateLeft64(digest, 11) * p1
		index++
	}
	digest ^= digest >> 33
	digest *= p2
	digest ^= digest >> 29
	digest *= p3
	digest ^= digest >> 32
	return digest
}
