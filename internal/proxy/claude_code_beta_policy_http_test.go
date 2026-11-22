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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

type betaPolicyIngress struct {
	name, path, body string
}

func betaPolicyIngresses() []betaPolicyIngress {
	return []betaPolicyIngress{
		{"chat", "/v1/chat/completions", `{"model":"default","messages":[{"role":"system","content":"keep"},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"mcp__caller__search_ide","parameters":{"type":"object"}}}]}`},
		{"responses", "/v1/responses", `{"model":"default","instructions":"keep","input":"hi","tools":[{"type":"function","name":"mcp__caller__search_ide","parameters":{"type":"object"}}]}`},
		{"messages", "/v1/messages", `{"model":"default","max_tokens":32,"system":[{"type":"text","text":"keep"}],"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"mcp__caller__search_ide","input_schema":{"type":"object"}}]}`},
	}
}

func betaPolicyBody(body string, stream bool, extra string) string {
	if stream {
		extra += `,"stream":true`
	}
	return strings.TrimSuffix(body, "}") + extra + "}"
}

// Exact selector allowlists guard the simulated upstream before dispatch.
// Only the ingress uses sockets; official hosts and OAuth URLs are never dialed.
func newBetaPolicyProxy(t *testing.T, targets []selectorTarget, respond func(*http.Request, []byte) *http.Response) (string, string, *http.Client) {
	t.Helper()
	tr := installSelectorTransport(t)
	base, token, _ := newSelectorProxy(t, tr, targets...)
	ingress, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	original := tr.next
	tr.next = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == ingress.Host {
			return original.RoundTrip(req)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		resp := respond(req, body)
		resp.Request = req
		return resp, nil
	})
	return base, token, selectorClient(tr)
}

func betaPolicyHTTPReply(req *http.Request, body []byte, status int) *http.Response {
	rec := httptest.NewRecorder()
	if status != http.StatusOK {
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(status)
		_, _ = io.WriteString(rec, `{"type":"error","error":{"type":"api_error","message":"synthetic rejection"}}`)
	} else {
		profileReply(rec, strings.Contains(string(body), `"stream":true`))
	}
	return rec.Result()
}

func assertBetaPolicyBody(t *testing.T, body []byte, wantContext bool) {
	t.Helper()
	var doc struct {
		Model             string          `json:"model"`
		Thinking          json.RawMessage `json:"thinking"`
		OutputConfig      json.RawMessage `json:"output_config"`
		ContextManagement json.RawMessage `json:"context_management"`
		Tools             []struct {
			Name string `json:"name"`
		} `json:"tools"`
		System   []map[string]any `json:"system"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Thinking) != 0 || len(doc.OutputConfig) != 0 || (len(doc.ContextManagement) != 0) != wantContext {
		t.Fatalf("beta advertisement changed request features: %s", body)
	}
	if len(doc.Tools) != 1 || doc.Tools[0].Name != "mcp__caller__search_ide" {
		t.Fatalf("caller tools changed: %s", body)
	}
	if len(doc.System) < 2 || doc.System[0]["cache_control"] != nil {
		t.Fatalf("billing cache policy changed: %s", body)
	}
}

func TestClaudeCodeBetaPolicyBaselineHTTP(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "claude-haiku-4-5-20251001", "claude-3-7-sonnet", "claude-sonnet-4-6-preview"} {
		for _, official := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/official=%v", model, official), func(t *testing.T) {
				backend := "https://beta-custom.invalid/v1"
				if official {
					backend = "https://api.anthropic.com/v1"
				}
				captures := make(chan profileWire, 8)
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{{protocol: domain.ProtocolClaudeCode, baseURL: backend, model: model}}, func(req *http.Request, body []byte) *http.Response {
					captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
					return betaPolicyHTTPReply(req, body, http.StatusOK)
				})
				for _, ingress := range betaPolicyIngresses() {
					for _, stream := range []bool{false, true} {
						resp, out := postSelector(t, client, base+ingress.path, token, betaPolicyBody(ingress.body, stream, ""), nil)
						if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
							t.Fatalf("%s stream=%v status %d: %s", ingress.name, stream, resp.StatusCode, out)
						}
						wire := profileCapture(t, captures)
						beta := wire.header.Get("anthropic-beta")
						known := model == "claude-sonnet-4-6" || model == "claude-haiku-4-5-20251001"
						assertBetaFlagCount(t, beta, claudecode.BetaInterleaved, boolCount(known))
						assertBetaFlagCount(t, beta, claudecode.BetaContextMgmt, boolCount(known))
						assertBetaFlagCount(t, beta, claudecode.BetaPromptCache, boolCount(official))
						for _, flag := range []string{claudecode.BetaRedactThinking, claudecode.BetaStructured, claudecode.BetaLongContext, claudecode.BetaExtendedTTL, claudecode.BetaFastMode} {
							assertBetaFlagCount(t, beta, flag, 0)
						}
						assertBetaPolicyBody(t, wire.body, false)
					}
				}
			})
		}
	}
}

func TestClaudeCodeBetaPolicyHostAndCacheHTTP(t *testing.T) {
	for _, tc := range []struct {
		base     string
		official bool
	}{
		{"https://api.anthropic.com/v1", true},
		{"https://API.ANTHROPIC.COM:443/v1", true},
		{"http://api.anthropic.com:80/v1", true},
		{"https://api.anthropic.com:8443/v1", false},
		{"https://api.anthropic.com.evil.invalid/v1", false},
		{"https://api.anthropic.com./v1", false},
		{"https://beta-custom.invalid/v1", false},
	} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%v", tc.base, explicit), func(t *testing.T) {
				captures := make(chan profileWire, 2)
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{{protocol: domain.ProtocolClaudeCode, baseURL: tc.base, model: "claude-sonnet-4-6"}}, func(req *http.Request, body []byte) *http.Response {
					captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
					return betaPolicyHTTPReply(req, body, http.StatusOK)
				})
				body := betaPolicyIngresses()[2].body
				if explicit {
					body = strings.Replace(body, `"text":"keep"`, `"text":"keep","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}`, 1)
				}
				resp, out := postSelector(t, client, base+"/v1/messages", token, body, nil)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status %d: %s", resp.StatusCode, out)
				}
				wire := profileCapture(t, captures)
				assertBetaFlagCount(t, wire.header.Get("anthropic-beta"), claudecode.BetaPromptCache, boolCount(tc.official || explicit))
				assertBetaFlagCount(t, wire.header.Get("anthropic-beta"), claudecode.BetaExtendedTTL, boolCount(explicit))
				var doc struct {
					System []map[string]any `json:"system"`
				}
				if err := json.Unmarshal(wire.body, &doc); err != nil {
					t.Fatal(err)
				}
				wantMark := map[string]any{"type": "ephemeral", "ttl": "5m"}
				if explicit {
					wantMark = map[string]any{"type": "ephemeral", "ttl": "1h", "scope": "global"}
				}
				if doc.System[0]["cache_control"] != nil || !reflect.DeepEqual(doc.System[len(doc.System)-1]["cache_control"], wantMark) {
					t.Fatalf("cache markers changed: %s", wire.body)
				}
			})
		}
	}
}

func TestClaudeCodeBetaPolicyThinkingAndContextHTTP(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5", "claude-sonnet-4-6"} {
		for _, tc := range []struct {
			name, controls, thinking, display string
			budget                            int
			redact                            bool
		}{
			{"absent", "", "", "", 0, false},
			{"disabled", `,"thinking":{"type":"disabled","display":"summarized","budget_tokens":1024}`, "disabled", "", 0, false},
			{"active", `,"thinking":{"type":"enabled","budget_tokens":1024}`, "enabled", "", 1024, true},
			{"auto", `,"thinking":{"type":"enabled"}`, "enabled", "", 0, true},
			{"summarized-budget", `,"thinking":{"type":"enabled","budget_tokens":1024,"display":"summarized"}`, "enabled", "summarized", 1024, false},
			{"summarized-auto", `,"thinking":{"type":"enabled","display":"summarized"}`, "enabled", "summarized", 0, false},
		} {
			for _, contextMgmt := range []bool{false, true} {
				for _, ingress := range betaPolicyIngresses() {
					if contextMgmt && ingress.name != "messages" {
						continue
					}
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/context=%v/%s/stream=%v", model, tc.name, contextMgmt, ingress.name, stream), func(t *testing.T) {
							captures := make(chan profileWire, 2)
							base, token, client := newBetaPolicyProxy(t, []selectorTarget{{protocol: domain.ProtocolClaudeCode, baseURL: "https://beta-custom.invalid/v1", model: model}}, func(req *http.Request, body []byte) *http.Response {
								captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
								return betaPolicyHTTPReply(req, body, http.StatusOK)
							})
							extra := tc.controls
							const contextJSON = `{"edits":[{"type":"compact_20250101"}]}`
							if contextMgmt {
								extra += `,"context_management":` + contextJSON
							}
							body := betaPolicyBody(strings.Replace(ingress.body, `"max_tokens":32`, `"max_tokens":4096`, 1), stream, extra)
							resp, out := postSelector(t, client, base+ingress.path, token, body, map[string]string{"anthropic-beta": strings.Join([]string{claudecode.BetaInterleaved, claudecode.BetaContextMgmt, claudecode.BetaRedactThinking, claudecode.BetaInterleaved, "unknown-beta"}, ",")})
							if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "ok") {
								t.Fatalf("status %d: %s", resp.StatusCode, out)
							}
							wire := profileCapture(t, captures)
							beta := wire.header.Get("anthropic-beta")
							assertBetaFlagCount(t, beta, claudecode.BetaInterleaved, 1)
							assertBetaFlagCount(t, beta, claudecode.BetaContextMgmt, 1)
							assertBetaFlagCount(t, beta, claudecode.BetaRedactThinking, boolCount(tc.redact))
							assertBetaFlagCount(t, beta, "unknown-beta", 0)
							var doc struct {
								Thinking          map[string]any  `json:"thinking"`
								ContextManagement json.RawMessage `json:"context_management"`
							}
							if err := json.Unmarshal(wire.body, &doc); err != nil {
								t.Fatal(err)
							}
							if (len(doc.ContextManagement) > 0) != contextMgmt || contextMgmt && !sameFinding6JSON(t, doc.ContextManagement, []byte(contextJSON)) {
								t.Fatalf("context intent changed: %s", wire.body)
							}
							wantType := tc.thinking
							if wantType == "enabled" && model == "claude-sonnet-4-6" {
								wantType = "adaptive"
							}
							if wantType == "" {
								if doc.Thinking != nil {
									t.Fatalf("thinking invented: %s", wire.body)
								}
							} else if doc.Thinking["type"] != wantType {
								t.Fatalf("thinking type = %v, want %s; body=%s", doc.Thinking["type"], wantType, wire.body)
							}
							gotDisplay, hasDisplay := doc.Thinking["display"]
							if hasDisplay != (tc.display != "") || hasDisplay && gotDisplay != tc.display {
								t.Fatalf("thinking display = %v, want %q; body=%s", gotDisplay, tc.display, wire.body)
							}
							budget, hasBudget := doc.Thinking["budget_tokens"]
							wantBudget := tc.budget
							if model == "claude-sonnet-4-6" {
								wantBudget = 0
							}
							if hasBudget != (wantBudget > 0) || hasBudget && budget != float64(wantBudget) {
								t.Fatalf("thinking budget = %v, want %d; body=%s", budget, wantBudget, wire.body)
							}
						})
					}
				}
			}
		}
	}
}

func TestClaudeCodeBetaPolicyNativeContextHTTP(t *testing.T) {
	for _, tc := range []struct {
		model              string
		selector, explicit bool
	}{
		{"claude-sonnet-5", false, true},
		{"claude-opus-4-7", false, true},
		{"claude-haiku-5-5", false, true},
		{"claude-sonnet-4-6", true, true},
		{"claude-haiku-4-5-20251001", true, false},
		{"claude-sonnet-4-6-preview", true, false},
	} {
		for _, source := range []string{"selector", "caller", "both"} {
			t.Run(tc.model+"/"+source, func(t *testing.T) {
				target := tc.model
				if source != "caller" {
					target += "[1m]"
				}
				captures := make(chan profileWire, 8)
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{{protocol: domain.ProtocolClaudeCode, baseURL: "https://beta-custom.invalid/v1", model: target}}, func(req *http.Request, body []byte) *http.Response {
					captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
					return betaPolicyHTTPReply(req, body, http.StatusOK)
				})
				headers := map[string]string{}
				if source != "selector" {
					headers["anthropic-beta"] = claudecode.BetaLongContext + "," + claudecode.BetaLongContext
				}
				want := source != "caller" && tc.selector || source != "selector" && tc.explicit
				for _, ingress := range betaPolicyIngresses() {
					for _, stream := range []bool{false, true} {
						resp, out := postSelector(t, client, base+ingress.path, token, betaPolicyBody(ingress.body, stream, ""), headers)
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("%s stream=%v status %d: %s", ingress.name, stream, resp.StatusCode, out)
						}
						wire := profileCapture(t, captures)
						assertBetaFlagCount(t, wire.header.Get("anthropic-beta"), claudecode.BetaLongContext, boolCount(want))
						if strings.Contains(string(wire.body), "[1m]") {
							t.Fatalf("selector survived serialization: %s", wire.body)
						}
					}
				}
			})
		}
	}
}

func TestClaudeCodeBetaPolicyStructuredOutputHTTP(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"claude-opus-4-0", false},
		{"claude-sonnet-4-20250514", false},
		{"claude-opus-4-1-20250805", true},
	} {
		for _, source := range []string{"body", "caller"} {
			t.Run(tc.model+"/"+source, func(t *testing.T) {
				captures := make(chan profileWire, 8)
				base, token, client := newBetaPolicyProxy(t, []selectorTarget{{protocol: domain.ProtocolClaudeCode, baseURL: "https://beta-custom.invalid/v1", model: tc.model}}, func(req *http.Request, body []byte) *http.Response {
					captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
					return betaPolicyHTTPReply(req, body, http.StatusOK)
				})
				for _, ingress := range betaPolicyIngresses() {
					for _, stream := range []bool{false, true} {
						extra := ""
						headers := map[string]string{}
						if source == "caller" {
							headers["anthropic-beta"] = claudecode.BetaStructured + "," + claudecode.BetaStructured
						} else {
							switch ingress.name {
							case "chat":
								extra = `,"response_format":{"type":"json_schema","json_schema":{"name":"Out","schema":{"type":"object"}}}`
							case "responses":
								extra = `,"text":{"format":{"type":"json_schema","name":"Out","schema":{"type":"object"}}}`
							case "messages":
								extra = `,"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}`
							}
						}
						resp, out := postSelector(t, client, base+ingress.path, token, betaPolicyBody(ingress.body, stream, extra), headers)
						if resp.StatusCode != http.StatusOK {
							t.Fatalf("%s stream=%v status %d: %s", ingress.name, stream, resp.StatusCode, out)
						}
						wire := profileCapture(t, captures)
						assertBetaFlagCount(t, wire.header.Get("anthropic-beta"), claudecode.BetaStructured, boolCount(tc.want))
						if source == "body" && !bytes.Contains(wire.body, []byte(`"format":{"schema":{"type":"object"},"type":"json_schema"}`)) {
							var doc struct {
								OutputConfig struct {
									Format json.RawMessage `json:"format"`
								} `json:"output_config"`
							}
							if err := json.Unmarshal(wire.body, &doc); err != nil || len(doc.OutputConfig.Format) == 0 {
								t.Fatalf("structured body dropped: %s", wire.body)
							}
						}
					}
				}
			})
		}
	}
}

func TestClaudeCodeBetaPolicyStructuredLongContextIntent(t *testing.T) {
	for _, extra := range []string{
		`,"betas":["context-1m-2025-08-07"]`,
		`,"metadata":{"betas":["context-1m-2025-08-07"]}`,
	} {
		body := []byte(`{"model":"claude-sonnet-5[1m]","max_tokens":32,"messages":[{"role":"user","content":"hi"}]` + extra + `}`)
		provider := &domain.Provider{Protocol: domain.ProtocolClaudeCode, BaseURL: "https://beta-custom.invalid/v1"}
		req := claudeBetaRequest(context.Background(), provider, body)
		if !req.ContextOneMillion || !req.ExplicitOneMillion {
			t.Fatalf("explicit and selector intent conflated: %+v", req)
		}
		assertBetaFlagCount(t, claudecode.BetaHeaders(req), claudecode.BetaLongContext, 1)
	}
	body := []byte(`{"model":"claude-sonnet-5[1m]","tools":[{"name":"t","input_schema":{"description":"context-1m-2025-08-07","betas":["context-1m-2025-08-07"]}}]}`)
	provider := &domain.Provider{Protocol: domain.ProtocolClaudeCode}
	req := claudeBetaRequest(context.Background(), provider, body)
	if req.ExplicitOneMillion {
		t.Fatalf("schema text became beta intent: %+v", req)
	}
	assertBetaFlagCount(t, claudecode.BetaHeaders(req), claudecode.BetaLongContext, 0)
}

func TestClaudeCodeBetaPolicyFailoverAndRefreshHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("failover/stream=%v", stream), func(t *testing.T) {
			captures := make(chan profileWire, 4)
			var hits atomic.Int32
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5[1m]"},
				{protocol: domain.ProtocolClaudeCode, baseURL: "https://beta-backup.invalid/v1", model: "claude-sonnet-4-6[1m]"},
			}, func(req *http.Request, body []byte) *http.Response {
				captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
				status := http.StatusOK
				if hits.Add(1) == 1 {
					status = http.StatusInternalServerError
				}
				return betaPolicyHTTPReply(req, body, status)
			})
			resp, out := postSelector(t, client, base+"/v1/messages", token, betaPolicyBody(betaPolicyIngresses()[2].body, stream, ""), nil)
			if resp.StatusCode != http.StatusOK || hits.Load() != 2 {
				t.Fatalf("status %d hits %d: %s", resp.StatusCode, hits.Load(), out)
			}
			first, second := profileCapture(t, captures), profileCapture(t, captures)
			assertBetaFlagCount(t, first.header.Get("anthropic-beta"), claudecode.BetaLongContext, 0)
			assertBetaFlagCount(t, first.header.Get("anthropic-beta"), claudecode.BetaPromptCache, 1)
			assertBetaFlagCount(t, second.header.Get("anthropic-beta"), claudecode.BetaLongContext, 1)
			assertBetaFlagCount(t, second.header.Get("anthropic-beta"), claudecode.BetaPromptCache, 0)
		})
		t.Run(fmt.Sprintf("refresh/stream=%v", stream), func(t *testing.T) {
			const endpoint = "https://beta-token.invalid/refresh"
			captures := make(chan profileWire, 4)
			var hits, tokenHits atomic.Int32
			creds := &domain.OAuthCreds{
				ClaudeCodeAuth: true, AccessToken: "opaque-old", RefreshToken: "rt-old", ClientID: "cid-synthetic",
				TokenURL: endpoint, ExpiresAt: time.Now().Add(time.Hour).Unix(), ClaudeScopes: "user:inference",
				ClaudeAccountUUID: "account-old", ClaudeDeviceID: strings.Repeat("ab", 32),
			}
			base, token, client := newBetaPolicyProxy(t, []selectorTarget{{protocol: domain.ProtocolClaudeCode, baseURL: "https://api.anthropic.com/v1", model: "claude-sonnet-5[1m]", oauth: creds}}, func(req *http.Request, body []byte) *http.Response {
				if req.URL.String() == endpoint {
					tokenHits.Add(1)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"sk-ant-oat-new","refresh_token":"rt-new","expires_in":3600,"scope":"user:profile","account":{"uuid":"account-new"}}`))}
				}
				captures <- profileWire{req.Header.Clone(), append([]byte(nil), body...)}
				status := http.StatusOK
				if hits.Add(1) == 1 {
					status = http.StatusUnauthorized
				}
				return betaPolicyHTTPReply(req, body, status)
			})
			tr := client.Transport.(*selectorTransport)
			tr.allow(t, http.MethodPost, endpoint)
			resp, out := postSelector(t, client, base+"/v1/messages", token, betaPolicyBody(betaPolicyIngresses()[2].body, stream, ""), nil)
			if resp.StatusCode != http.StatusOK || hits.Load() != 2 || tokenHits.Load() != 1 {
				t.Fatalf("status %d hits %d refresh %d: %s", resp.StatusCode, hits.Load(), tokenHits.Load(), out)
			}
			for i, wantAccount := range []string{"account-old", "account-new"} {
				wire := profileCapture(t, captures)
				beta := wire.header.Get("anthropic-beta")
				assertBetaFlagCount(t, beta, claudecode.BetaOAuth, boolCount(i == 0))
				assertBetaFlagCount(t, beta, claudecode.BetaLongContext, 0)
				assertBetaFlagCount(t, beta, claudecode.BetaPromptCache, 1)
				var doc struct {
					Metadata struct {
						UserID string `json:"user_id"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(wire.body, &doc); err != nil {
					t.Fatal(err)
				}
				var id claudecode.UserID
				if err := json.Unmarshal([]byte(doc.Metadata.UserID), &id); err != nil || id.AccountUUID != wantAccount || id.SessionID != wire.header.Get(claudecode.SessionIDHeader) {
					t.Fatalf("refresh identity mismatch: %s", wire.body)
				}
			}
		})
	}
}
