package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

type profileWire struct {
	header http.Header
	body   []byte
}

func profileCapture(t *testing.T, ch <-chan profileWire) profileWire {
	t.Helper()
	select {
	case wire := <-ch:
		return wire
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for profile request")
		return profileWire{}
	}
}

func profileReply(w http.ResponseWriter, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, finding9SSE(
			finding9Frame("message_start", `{"type":"message_start","message":{"id":"msg_profile","type":"message","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":3}}}`),
			finding9Frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			finding9Frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			finding9Frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
			finding9Frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`),
			finding9Frame("message_stop", `{"type":"message_stop"}`),
		))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"msg_profile","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
}

func TestClaudeCodeProfileHeadersAcrossIngresses(t *testing.T) {
	tr := installSelectorTransport(t)
	captures := make(chan profileWire, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captures <- profileWire{r.Header.Clone(), body}
		profileReply(w, strings.Contains(string(body), `"stream":true`))
	}))
	defer upstream.Close()
	base, token, _ := newSelectorProxy(t, tr, selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: upstream.URL, model: "claude-sonnet-5"})
	cases := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"default","messages":[{"role":"system","content":"keep"},{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"mcp__caller__search_ide","parameters":{"type":"object"}}}]}`},
		{"responses", "/v1/responses", `{"model":"default","instructions":"keep","input":"hello","tools":[{"type":"function","name":"mcp__caller__search_ide","parameters":{"type":"object"}}]}`},
		{"messages", "/v1/messages", `{"model":"default","max_tokens":8,"system":[{"type":"text","text":"keep","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"mcp__caller__search_ide","input_schema":{"type":"object"}}]}`},
	}
	const session = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	const agent = "33333333-3333-4333-8333-333333333333"
	const prompt = "44444444-4444-4444-8444-444444444444"
	var stableAgent, stableSession string
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			name := tc.name + "/unary"
			body := tc.body
			if stream {
				name = tc.name + "/stream"
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			t.Run(name, func(t *testing.T) {
				resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, body, map[string]string{
					"X-Client-App": "my-app", claudeSessionHeader: session, claudeParentHeader: parent,
					claudeAgentHeader: agent, claudePromptHeader: prompt,
					"X-Stainless-Helper-Method": "stream", "anthropic-beta": claudecode.BetaLongContext,
				})
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
					t.Fatalf("response = %d / %s", resp.StatusCode, out)
				}
				wire := profileCapture(t, captures)
				if _, ok := wire.header["X-Stainless-Helper-Method"]; ok {
					t.Fatal("helper-method forwarded")
				}
				if wire.header.Get("User-Agent") != "claude-cli/2.1.318 (external, cli, client-app/my-app)" || wire.header.Get("X-Client-App") != "my-app" || wire.header.Get(claudePromptHeader) != prompt {
					t.Fatalf("optional/profile headers = %v", wire.header)
				}
				if wire.header.Get("Authorization") != "Bearer up-key" {
					t.Fatal("provider auth changed")
				}
				gotAgent := wire.header.Get(claudeAgentHeader)
				gotSession := wire.header.Get(claudeSessionHeader)
				if !validClaudeUUID(gotAgent) || gotAgent == agent || gotSession == session || gotAgent == gotSession {
					t.Fatal("optional identity not scoped")
				}
				if stableAgent == "" {
					stableAgent, stableSession = gotAgent, gotSession
				} else if gotAgent != stableAgent || gotSession != stableSession {
					t.Fatal("identity changed between ingresses or stream modes")
				}
				var doc struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
					System   []map[string]any `json:"system"`
					Metadata struct {
						UserID string `json:"user_id"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(wire.body, &doc); err != nil {
					t.Fatal(err)
				}
				var uid claudecode.UserID
				if err := json.Unmarshal([]byte(doc.Metadata.UserID), &uid); err != nil {
					t.Fatal(err)
				}
				if uid.SessionID != gotSession || uid.ParentSessionID == parent || !validClaudeUUID(uid.ParentSessionID) {
					t.Fatal("body/session/parent identity diverged")
				}
				if len(doc.Tools) != 1 || doc.Tools[0].Name != "mcp__caller__search_ide" {
					t.Fatal("caller tool renamed or decoys added")
				}
				bill, _ := doc.System[0]["text"].(string)
				if !strings.Contains(bill, "cc_version=2.1.318.") || strings.Contains(bill, "cc_project=") {
					t.Fatalf("billing = %q", bill)
				}
				if _, ok := doc.System[0]["cache_control"]; ok {
					t.Fatal("billing gained cache policy")
				}
				if tc.name == "messages" {
					last := doc.System[len(doc.System)-1]
					want := map[string]any{"type": "ephemeral", "ttl": "1h", "scope": "global"}
					if last["text"] != "keep" || !reflect.DeepEqual(last["cache_control"], want) {
						t.Fatalf("caller cache policy changed: %v", last)
					}
				}
				wantBetas := strings.Join([]string{claudecode.BetaClaudeCode, claudecode.BetaInterleaved, claudecode.BetaContextMgmt}, ",")
				if tc.name == "messages" {
					wantBetas += "," + claudecode.BetaPromptCache + "," + claudecode.BetaExtendedTTL
				}
				wantBetas += "," + claudecode.BetaLongContext
				if wire.header.Get("anthropic-beta") != wantBetas {
					t.Fatalf("beta policy changed: %q, want %q", wire.header.Get("anthropic-beta"), wantBetas)
				}
			})
		}
	}
}

func TestClaudeCodeProfileRejectsInvalidOptionalHeadersHTTP(t *testing.T) {
	tr := installSelectorTransport(t)
	captures := make(chan profileWire, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captures <- profileWire{r.Header.Clone(), body}
		profileReply(w, strings.Contains(string(body), `"stream":true`))
	}))
	defer upstream.Close()
	base, token, _ := newSelectorProxy(t, tr, selectorTarget{protocol: domain.ProtocolClaudeCode, baseURL: upstream.URL, model: "claude-sonnet-5"})
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"default","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"default","input":"hello"}`},
		{"/v1/messages", `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`},
	} {
		for _, duplicate := range []bool{false, true} {
			body := tc.body
			if duplicate {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+tc.path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			values := map[string]string{
				"X-Client-App": "my-app", claudeAgentHeader: "33333333-3333-4333-8333-333333333333",
				claudePromptHeader: "44444444-4444-4444-8444-444444444444",
			}
			for key, value := range values {
				if duplicate {
					req.Header.Add(key, value)
					req.Header.Add(key, value)
				} else {
					req.Header.Set(key, "bad; value")
				}
			}
			req.Header.Set("X-Stainless-Helper-Method", "list")
			resp, err := selectorClient(tr).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("response = %d / %s / %v", resp.StatusCode, out, err)
			}
			wire := profileCapture(t, captures)
			for _, key := range []string{"X-Client-App", claudeAgentHeader, claudePromptHeader, "X-Stainless-Helper-Method"} {
				if wire.header.Get(key) != "" {
					t.Fatalf("invalid/duplicate header %s survived %s", key, tc.path)
				}
			}
			if wire.header.Get("User-Agent") != "claude-cli/2.1.318 (external, cli)" {
				t.Fatal("invalid optional header affected the profile")
			}
		}
	}
}

func TestClaudeCodeProfileOptionalHeadersRescopeAfterRefresh(t *testing.T) {
	for _, changedAccount := range []bool{false, true} {
		name := "same-account"
		if changedAccount {
			name = "new-account"
		}
		t.Run(name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			account := "old-account"
			if changedAccount {
				account = "new-account"
			}
			tokenCalls := 0
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls++
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"access_token":"sk-ant-oat-new","refresh_token":"rt-new","expires_in":3600,"scope":"user:inference","account":{"uuid":"`+account+`"}}`)
			}))
			defer tokenSrv.Close()
			tr.allow(t, http.MethodPost, tokenSrv.URL)
			captures := make(chan profileWire, 4)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captures <- profileWire{r.Header.Clone(), body}
				calls++
				if calls == 1 {
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"expired"}}`)
					return
				}
				profileReply(w, false)
			}))
			defer upstream.Close()
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode, baseURL: upstream.URL, model: "claude-sonnet-5",
				oauth: &domain.OAuthCreds{
					ClaudeCodeAuth: true, AccessToken: "sk-ant-oat-old", RefreshToken: "rt-old",
					ExpiresAt: time.Now().Add(time.Hour).Unix(), TokenURL: tokenSrv.URL, ClientID: "synthetic",
					ClaudeAccountUUID: "old-account", ClaudeDeviceID: strings.Repeat("ab", 32),
				},
			})
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token,
				`{"model":"default","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
					"X-Client-App": "my-app", claudeAgentHeader: "33333333-3333-4333-8333-333333333333",
					claudeSessionHeader: "11111111-1111-4111-8111-111111111111", claudePromptHeader: "44444444-4444-4444-8444-444444444444",
				})
			if resp.StatusCode != http.StatusOK || calls != 2 || tokenCalls != 1 {
				t.Fatalf("response %d / calls %d / refresh %d: %s", resp.StatusCode, calls, tokenCalls, out)
			}
			first, second := profileCapture(t, captures), profileCapture(t, captures)
			for i, wire := range []profileWire{first, second} {
				if wire.header.Get("X-Client-App") != "my-app" || wire.header.Get(claudePromptHeader) != "44444444-4444-4444-8444-444444444444" || wire.header.Get("X-Stainless-Helper-Method") != "" {
					t.Fatal("optional headers changed during refresh")
				}
				if !strings.Contains(string(wire.body), wire.header.Get(claudeSessionHeader)) {
					t.Fatal("session/body mismatch after refresh")
				}
				wantToken := "Bearer sk-ant-oat-old"
				if i == 1 {
					wantToken = "Bearer sk-ant-oat-new"
				}
				if wire.header.Get("Authorization") != wantToken {
					t.Fatal("refresh token not paired")
				}
			}
			for _, key := range []string{claudeAgentHeader, claudeSessionHeader} {
				if first.header.Get(key) == "" || (first.header.Get(key) != second.header.Get(key)) != changedAccount {
					t.Fatalf("scope %s did not track account change", key)
				}
			}
		})
	}
}
