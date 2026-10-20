package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

func TestClaudeBetaUsesOnlyWireCachePositions(t *testing.T) {
	provider := &domain.Provider{Protocol: domain.ProtocolClaudeCode, APIKey: "sk-ant-oat-test"}
	schema := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":"keep","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"t","input_schema":{"type":"object","properties":{"cache_control":{"type":"object","properties":{"scope":{"const":"global"},"ttl":{"const":"1h"},"type":{"const":"evict"}}}}}}]}`)
	req := claudeBetaRequest(context.Background(), provider, schema)
	if req.CacheScope || req.ExtendedTTL || req.CacheEvict {
		t.Fatalf("schema keys became cache policy: %+v", req)
	}
	wire := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":"keep","cache_control":{"type":"ephemeral","scope":"global","ttl":"1h","type":"evict"}}],"messages":[{"role":"user","content":"hi"}]}`)
	req = claudeBetaRequest(context.Background(), provider, wire)
	if !req.CacheScope || !req.ExtendedTTL || !req.CacheEvict {
		t.Fatalf("wire markers were ignored: %+v", req)
	}
}

func TestClaudeCodeHTTPIngressesCarryIdentity(t *testing.T) {
	device := strings.Repeat("ab", 32)
	account := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	session := "11111111-1111-4111-8111-111111111111"
	parent := "22222222-2222-4222-8222-222222222222"
	seenCh := make(chan capturedClaude, 7)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenCh <- capturedClaude{
			path:   r.URL.RequestURI(),
			auth:   r.Header.Get("Authorization"),
			beta:   r.Header.Get("anthropic-beta"),
			sess:   r.Header.Get(claudecode.SessionIDHeader),
			ua:     r.Header.Get("User-Agent"),
			stream: r.URL.Query().Get("stream") != "" || strings.Contains(r.Header.Get("Accept"), "event-stream"),
			body:   append([]byte(nil), body...),
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") || strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "old-name", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth, AuthScheme: domain.AuthBearer,
		OAuthCreds: &domain.OAuthCreds{
			ClaudeCodeAuth: true, AccessToken: "sk-ant-oat-live", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(time.Hour).Unix(), ClaudeAccountUUID: account, ClaudeDeviceID: device,
		},
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"openai-unary", "/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"abcd😀fZ"}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]}`},
		{"openai-stream", "/v1/chat/completions", `{"model":"default","stream":true,"messages":[{"role":"user","content":"abcd😀fZ"}]}`},
		{"anthropic-unary", "/v1/messages", `{"model":"default","max_tokens":16,"messages":[{"role":"user","content":"abcd😀fZ"}],"tools":[{"name":"search","input_schema":{"type":"object"}}]}`},
		{"anthropic-stream", "/v1/messages", `{"model":"default","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"abcd😀fZ"}]}`},
		{"responses-unary", "/v1/responses", `{"model":"default","input":"abcd😀fZ","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}`},
		{"responses-stream", "/v1/responses", `{"model":"default","stream":true,"input":"abcd😀fZ"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key.Token)
			req.Header.Set("X-Claude-Code-Session-Id", session)
			req.Header.Set("X-Claude-Code-Parent-Session-Id", parent)
			req.Header.Set("anthropic-beta", claudecode.BetaLongContext+",not-a-real-flag")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if !strings.Contains(string(out), "ok") {
				t.Fatalf("client body lost text: %s", out)
			}
		})
	}
	seen := make([]capturedClaude, len(cases))
	for i := range seen {
		seen[i] = <-seenCh
	}
	var sessionHeader string
	for i, cap := range seen {
		if cap.path != "/messages?beta=true" {
			t.Fatalf("path=%s", cap.path)
		}
		if cap.auth != "Bearer sk-ant-oat-live" {
			t.Fatalf("auth=%s", cap.auth)
		}
		if !strings.HasPrefix(cap.ua, "claude-cli/"+claudecode.CLIVersion) {
			t.Fatalf("ua=%s", cap.ua)
		}
		if strings.Contains(cap.beta, "not-a-real-flag") {
			t.Fatalf("unknown beta forwarded: %s", cap.beta)
		}
		if !strings.Contains(cap.beta, claudecode.BetaLongContext) {
			t.Fatalf("caller long-context beta missing: %s", cap.beta)
		}
		if !strings.Contains(cap.beta, claudecode.BetaOAuth) {
			t.Fatalf("oauth beta missing: %s", cap.beta)
		}
		if strings.Contains(cap.beta, claudecode.BetaRedactThinking) {
			t.Fatalf("summarized-independent redact beta invented: %s", cap.beta)
		}
		var doc map[string]any
		if err := json.Unmarshal(cap.body, &doc); err != nil {
			t.Fatalf("body: %v %s", err, cap.body)
		}
		if doc["model"] != "claude-sonnet-5" {
			t.Fatalf("model=%v", doc["model"])
		}
		meta := doc["metadata"].(map[string]any)
		uid := meta["user_id"].(string)
		if !strings.Contains(uid, device) || !strings.Contains(uid, account) {
			t.Fatalf("user_id missing device or account: %s", uid)
		}
		if !strings.Contains(uid, cap.sess) {
			t.Fatalf("body session %s != header %s", uid, cap.sess)
		}
		if sessionHeader == "" {
			sessionHeader = cap.sess
		} else if cap.sess != sessionHeader {
			t.Fatalf("session changed across ingresses: %s / %s", sessionHeader, cap.sess)
		}
		if !strings.Contains(uid, "parent_session_id") {
			t.Fatalf("parent missing: %s", uid)
		}
		sys := doc["system"].([]any)
		bill := sys[0].(map[string]any)["text"].(string)
		if !strings.Contains(bill, "cc_version="+claudecode.CLIVersion+".9a3") {
			t.Fatalf("billing fingerprint missing: %s", bill)
		}
		if strings.Contains(bill, "cch=") && !strings.Contains(bill, "cch=00000;") {
			t.Fatalf("cch is not the literal 00000: %s", bill)
		}
		if _, ok := sys[0].(map[string]any)["cache_control"]; ok {
			t.Fatal("billing block gained cache_control")
		}
		markers := 0
		for _, block := range sys {
			if block.(map[string]any)["cache_control"] != nil {
				markers++
			}
		}
		if markers > 4 {
			t.Fatalf("cache markers=%d body=%s", markers, cap.body)
		}
		if i == 0 || i == 2 || i == 4 {
			tools, _ := doc["tools"].([]any)
			if len(tools) == 0 || tools[0].(map[string]any)["name"] != "search" {
				t.Fatalf("tool identity lost: %s", cap.body)
			}
		}
		if strings.Contains(string(cap.body), "sk-ant-oat") {
			t.Fatal("token leaked into body")
		}
	}

	// Rename must not split the scoped session on the next attempt.
	saved, err := st.GetProvider(context.Background(), prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved.Name = "renamed"
	if err := st.UpdateProvider(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"abcd😀fZ"}]}`))
	req.Header.Set("Authorization", "Bearer "+key.Token)
	req.Header.Set("X-Claude-Code-Session-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	last := <-seenCh
	if last.sess != sessionHeader {
		t.Fatalf("rename changed session %s -> %s", sessionHeader, last.sess)
	}
}

func TestClaudeCodeRefreshRetriesBeforeCommit(t *testing.T) {
	var calls int
	var auths []string
	var sessions []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		auths = append(auths, r.Header.Get("Authorization"))
		sessions = append(sessions, r.Header.Get(claudecode.SessionIDHeader))
		if !strings.Contains(string(body), r.Header.Get(claudecode.SessionIDHeader)) {
			t.Errorf("session header/body diverged on attempt %d", calls)
		}
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"no"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"sk-ant-oat-refreshed","refresh_token":"rt-next","expires_in":3600,"scope":"user:inference","account":{"uuid":"acct-1"}}`)
	}))
	t.Cleanup(tokenSrv.Close)
	st := newTestStore(t)
	prov := &domain.Provider{
		Name: "cc", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode,
		AuthMethod: domain.AuthOAuth,
		OAuthCreds: &domain.OAuthCreds{
			ClaudeCodeAuth: true, AccessToken: "sk-ant-oat-stale", RefreshToken: "rt-old",
			ExpiresAt: time.Now().Add(time.Hour).Unix(), TokenURL: tokenSrv.URL, ClientID: "cid",
			ClaudeAccountUUID: "acct-1", ClaudeDeviceID: strings.Repeat("cd", 32),
		},
	}
	if err := st.CreateProvider(context.Background(), prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-5", Enabled: true}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(context.Background(), "client")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	resp, out := post(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if calls != 2 {
		t.Fatalf("upstream calls=%d want 2 before commit", calls)
	}
	if auths[0] != "Bearer sk-ant-oat-stale" || auths[1] != "Bearer sk-ant-oat-refreshed" {
		t.Fatalf("auths=%v", auths)
	}
	if sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("retry session changed: %v", sessions)
	}
	saved, err := st.GetProvider(context.Background(), prov.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OAuthCreds.AccessToken != "sk-ant-oat-refreshed" || saved.OAuthCreds.ClaudeDeviceID != strings.Repeat("cd", 32) {
		t.Fatalf("persisted creds lost pairing: %+v", saved.OAuthCreds)
	}
}

func TestClaudeCodeAccountChangeRescopesSession(t *testing.T) {
	cases := []struct {
		name      string
		expiresIn time.Duration
		proactive bool
	}{
		{name: "proactive", expiresIn: time.Minute, proactive: true},
		{name: "reactive", expiresIn: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const oldToken = "sk-ant-oat-old"
			const newToken = "sk-ant-oat-new"
			const oldAccount = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			const newAccount = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			var tokenCalls int
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"`+newToken+`","refresh_token":"rt-new","expires_in":3600,"scope":"user:inference","account":{"uuid":"`+newAccount+`"}}`)
			}))
			t.Cleanup(tokenSrv.Close)

			var calls int
			var auths, sessions, accounts []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				var doc struct {
					Metadata struct {
						UserID string `json:"user_id"`
					} `json:"metadata"`
				}
				_ = json.Unmarshal(body, &doc)
				var uid claudecode.UserID
				_ = json.Unmarshal([]byte(doc.Metadata.UserID), &uid)
				auths = append(auths, r.Header.Get("Authorization"))
				sessions = append(sessions, r.Header.Get(claudecode.SessionIDHeader))
				accounts = append(accounts, uid.AccountUUID)
				if !strings.Contains(string(body), r.Header.Get(claudecode.SessionIDHeader)) || uid.SessionID != r.Header.Get(claudecode.SessionIDHeader) {
					t.Errorf("attempt %d header/body session diverged: %s", calls, body)
				}
				if calls == 1 && !tc.proactive {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"no"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":2}}`)
			}))
			t.Cleanup(upstream.Close)

			st := newTestStore(t)
			prov := &domain.Provider{
				Name: "cc", BaseURL: upstream.URL, Protocol: domain.ProtocolClaudeCode,
				AuthMethod: domain.AuthOAuth,
				OAuthCreds: &domain.OAuthCreds{
					ClaudeCodeAuth: true, AccessToken: oldToken, RefreshToken: "rt-old",
					ExpiresAt: time.Now().Add(tc.expiresIn).Unix(), TokenURL: tokenSrv.URL, ClientID: "cid",
					ClaudeAccountUUID: oldAccount, ClaudeDeviceID: strings.Repeat("cd", 32),
				},
			}
			if err := st.CreateProvider(context.Background(), prov); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateCombo(context.Background(), &domain.Combo{Name: "default", Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "claude-sonnet-5", Enabled: true}}}); err != nil {
				t.Fatal(err)
			}
			key, err := st.NewAccessKey(context.Background(), "client")
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			New(st, nil).Mount(mux)
			ts := httptest.NewServer(mux)
			t.Cleanup(ts.Close)

			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(`{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+key.Token)
			req.Header.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if tokenCalls != 1 {
				t.Fatalf("token calls=%d want 1", tokenCalls)
			}
			if tc.proactive {
				if calls != 1 || auths[0] != "Bearer "+newToken {
					t.Fatalf("proactive calls=%d auths=%v", calls, auths)
				}
				if accounts[0] != newAccount {
					t.Fatalf("proactive account=%s, want %s", accounts[0], newAccount)
				}
			} else {
				if calls != 2 || auths[0] != "Bearer "+oldToken || auths[1] != "Bearer "+newToken {
					t.Fatalf("reactive calls=%d auths=%v", calls, auths)
				}
				if accounts[0] != oldAccount || accounts[1] != newAccount {
					t.Fatalf("reactive accounts=%v", accounts)
				}
				if sessions[0] == sessions[1] {
					t.Fatalf("account change reused the old session: %v", sessions)
				}
			}
			if sessions[len(sessions)-1] == "11111111-1111-4111-8111-111111111111" {
				t.Fatal("client session was not scoped")
			}
			saved, err := st.GetProvider(context.Background(), prov.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.OAuthCreds.ClaudeAccountUUID != newAccount || saved.OAuthCreds.AccessToken != newToken {
				t.Fatalf("saved creds not paired: %+v", saved.OAuthCreds)
			}
		})
	}
}

type capturedClaude struct {
	path   string
	auth   string
	beta   string
	sess   string
	ua     string
	stream bool
	body   []byte
}
