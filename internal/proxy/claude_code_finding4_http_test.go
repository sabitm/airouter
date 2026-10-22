package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

func finding4Upstream(t *testing.T, captures *selectorCapture, stream bool) *httptest.Server {
	t.Helper()
	streamBody := omittedThinkingThenAnswerSSE()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, streamBody)
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClaudeCodeFinding4Mythos5HTTP(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		body   string
		stream bool
		text   string
	}{
		{
			name: "messages-unary", path: "/v1/messages", text: "hello from anthropic",
			body: `{"model":"default","max_tokens":32,"output_config":{"effort":"high"},"context_management":{"edits":[{"type":"compact_20250101"}]},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "messages-stream", path: "/v1/messages", stream: true, text: "answer",
			body: `{"model":"default","max_tokens":32,"stream":true,"output_config":{"effort":"high"},"context_management":{"edits":[{"type":"compact_20250101"}]},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-unary", path: "/v1/chat/completions", text: "hello from anthropic",
			body: `{"model":"default","max_tokens":32,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-stream", path: "/v1/chat/completions", stream: true, text: "answer",
			body: `{"model":"default","max_tokens":32,"stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-unary", path: "/v1/responses", text: "hello from anthropic",
			body: `{"model":"default","max_output_tokens":32,"reasoning":{"effort":"high"},"input":"hi"}`,
		},
		{
			name: "responses-stream", path: "/v1/responses", stream: true, text: "answer",
			body: `{"model":"default","max_output_tokens":32,"stream":true,"reasoning":{"effort":"high"},"input":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, tc.stream)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-mythos-5",
			})
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if !strings.Contains(string(out), tc.text) {
				t.Fatalf("client response lost content: %s", out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != "claude-mythos-5" {
				t.Fatalf("wire model = %q; body=%s", wire.model, wire.body)
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
			assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaInterleaved, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaRedactThinking, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaContextMgmt, boolCount(tc.path == "/v1/messages"))
			if tc.path == "/v1/messages" && !strings.Contains(wire.body, `"context_management"`) {
				t.Fatalf("context_management intent lost: %s", wire.body)
			}
			assertSelectorUsage(t, tc.path, tc.stream, string(out))
		})
	}
}

func TestClaudeCodeFinding4Mythos5IntentHTTP(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		body       string
		wantEffort string
		wantThink  bool
	}{
		{
			name: "suffix-xhigh-over-budget", target: "claude-mythos-5(xhigh)", wantEffort: "xhigh", wantThink: true,
			body: `{"model":"default","max_tokens":32768,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "suffix-max-over-low", target: "claude-mythos-5(max)", wantEffort: "max", wantThink: true,
			body: `{"model":"default","max_tokens":32768,"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "suffix-none-omits-controls", target: "claude-mythos-5(none)", wantThink: false,
			body: `{"model":"default","max_tokens":32768,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "no-intent", target: "claude-mythos-5", wantThink: false,
			body: `{"model":"default","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, false)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    tc.target,
			})
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != "claude-mythos-5" {
				t.Fatalf("wire model = %q; body=%s", wire.model, wire.body)
			}
			if !tc.wantThink {
				if strings.Contains(wire.body, `"thinking"`) || strings.Contains(wire.body, `"output_config"`) {
					t.Fatalf("no-intent request gained thinking: %s", wire.body)
				}
				assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 0)
				assertBetaFlagCount(t, wire.beta, claudecode.BetaInterleaved, 0)
				assertBetaFlagCount(t, wire.beta, claudecode.BetaRedactThinking, 0)
				return
			}
			if wire.thinking != "adaptive" || wire.effort != tc.wantEffort {
				t.Fatalf("thinking = %q effort = %q want %q; body=%s", wire.thinking, wire.effort, tc.wantEffort, wire.body)
			}
			if strings.Contains(wire.body, `"budget_tokens"`) || strings.Contains(wire.body, `"type":"disabled"`) {
				t.Fatalf("disabled or budget thinking survived: %s", wire.body)
			}
			assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaInterleaved, 1)
		})
	}
}

func TestClaudeCodeFinding4Mythos5DisableHTTP(t *testing.T) {
	messageBody := func(stream bool, controls string) string {
		streamField := ""
		if stream {
			streamField = `"stream":true,`
		}
		return `{"model":"default","max_tokens":32,` + streamField + controls + `"messages":[{"role":"user","content":"hi"}]}`
	}
	chatBody := func(stream bool, controls string) string {
		streamField := ""
		if stream {
			streamField = `"stream":true,`
		}
		return `{"model":"default","max_tokens":32,` + streamField + controls + `"messages":[{"role":"user","content":"hi"}]}`
	}
	responsesBody := func(stream bool, controls string) string {
		streamField := ""
		if stream {
			streamField = `"stream":true,`
		}
		return `{"model":"default","max_output_tokens":32,` + streamField + controls + `"input":"hi"}`
	}

	type ingress struct {
		name    string
		path    string
		body    func(bool, string) string
		sibling bool
	}
	ingresses := []ingress{
		{name: "messages", path: "/v1/messages", body: messageBody, sibling: true},
		{name: "chat", path: "/v1/chat/completions", body: chatBody},
		{name: "responses", path: "/v1/responses", body: responsesBody},
	}
	siblings := `"format":{"type":"json_schema","schema":{"type":"object","title":"Out"}},"task_budget":{"type":"tokens","total":1000,"remaining":0}`

	for _, in := range ingresses {
		var directControl, highControl string
		switch in.name {
		case "messages":
			directControl = `"thinking":{"type":"disabled"},"output_config":{` + siblings + `},`
			highControl = `"output_config":{"effort":"high",` + siblings + `},`
		case "chat":
			directControl = `"reasoning_effort":"none",`
			highControl = `"reasoning_effort":"high",`
		case "responses":
			directControl = `"reasoning":{"effort":"none"},`
			highControl = `"reasoning":{"effort":"high"},`
		}
		scenarios := []struct {
			name    string
			target  string
			control string
		}{
			{name: "direct-disabled", target: "claude-mythos-5", control: directControl},
			{name: "suffix-none-over-high", target: "claude-mythos-5(none)", control: highControl},
		}
		for _, sc := range scenarios {
			for _, stream := range []bool{false, true} {
				label := "unary"
				if stream {
					label = "stream"
				}
				t.Run(in.name+"-"+sc.name+"-"+label, func(t *testing.T) {
					tr := installSelectorTransport(t)
					captures := newSelectorCapture(2)
					upstream := finding4Upstream(t, captures, stream)
					base, token, _ := newSelectorProxy(t, tr, selectorTarget{
						protocol: domain.ProtocolClaudeCode,
						baseURL:  upstream.URL,
						model:    sc.target,
					})
					resp, out := postSelector(t, selectorClient(tr), base+in.path, token, in.body(stream, sc.control), nil)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("status=%d body=%s", resp.StatusCode, out)
					}
					wire := captures.next(t)
					captures.assertNoMore(t)
					if wire.model != "claude-mythos-5" {
						t.Fatalf("wire model = %q; body=%s", wire.model, wire.body)
					}
					assertMythos5DisableRequest(t, wire, in.sibling)
					assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 0)
					assertBetaFlagCount(t, wire.beta, claudecode.BetaInterleaved, 0)
					assertBetaFlagCount(t, wire.beta, claudecode.BetaRedactThinking, 0)
					assertSelectorUsage(t, in.path, stream, string(out))
				})
			}
		}
	}
}

func assertMythos5DisableRequest(t *testing.T, wire claudeSelectorWire, wantSiblings bool) {
	t.Helper()
	if wire.thinking != "" || wire.effort != "" {
		t.Fatalf("thinking = %q effort = %q; body=%s", wire.thinking, wire.effort, wire.body)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(wire.body), &doc); err != nil {
		t.Fatalf("decode wire: %v; body=%s", err, wire.body)
	}
	for _, key := range []string{"thinking", "reasoning_effort", "reasoning", "enable_thinking", "thinking_budget"} {
		if _, ok := doc[key]; ok {
			t.Fatalf("%s leaked: %s", key, wire.body)
		}
	}
	raw, has := doc["output_config"]
	if !has {
		if wantSiblings {
			t.Fatalf("output_config siblings lost: %s", wire.body)
		}
		return
	}
	var oc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatalf("decode output_config: %v; body=%s", err, wire.body)
	}
	if _, ok := oc["effort"]; ok {
		t.Fatalf("output_config.effort leaked: %s", wire.body)
	}
	if wantSiblings && (!strings.Contains(string(oc["format"]), "json_schema") || !strings.Contains(string(oc["task_budget"]), `"total":1000`)) {
		t.Fatalf("output_config siblings lost: %s", wire.body)
	}
}

func TestClaudeCodeFinding4Haiku45HTTP(t *testing.T) {
	cases := []struct {
		name       string
		model      string
		callerBeta bool
		context    bool
		wantModel  string
		wantBudget int
		longCtx    bool
	}{
		{
			name: "canonical-context", model: "claude-haiku-4-5", callerBeta: true, context: true,
			wantModel: "claude-haiku-4-5", wantBudget: 2048,
		},
		{
			name: "dated-context", model: "claude-haiku-4-5-20251001", callerBeta: true, context: true,
			wantModel: "claude-haiku-4-5-20251001", wantBudget: 2048,
		},
		{
			name: "dated-selector-high", model: "claude-haiku-4-5-20251001[1m](high)",
			wantModel: "claude-haiku-4-5-20251001", wantBudget: 24576, longCtx: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, false)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    tc.model,
			})
			body := `{"model":"default","max_tokens":32768,"messages":[{"role":"user","content":"hi"}]}`
			if tc.context {
				body = `{"model":"default","max_tokens":32768,"thinking":{"type":"enabled","budget_tokens":2048},"context_management":{"edits":[{"type":"compact_20250101"}]},"messages":[{"role":"user","content":"hi"}]}`
			}
			headers := map[string]string{}
			if tc.callerBeta {
				headers["anthropic-beta"] = claudecode.BetaLongContext
			}
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, headers)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != tc.wantModel {
				t.Fatalf("wire model = %q want %q; body=%s", wire.model, tc.wantModel, wire.body)
			}
			if wire.thinking != "enabled" || wire.budget != tc.wantBudget {
				t.Fatalf("thinking = %q budget = %d want enabled/%d; body=%s", wire.thinking, wire.budget, tc.wantBudget, wire.body)
			}
			if wire.effort != "" || strings.Contains(wire.body, `"type":"adaptive"`) {
				t.Fatalf("haiku gained adaptive or effort: %s", wire.body)
			}
			assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 0)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaInterleaved, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaRedactThinking, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaLongContext, boolCount(tc.longCtx))
			assertBetaFlagCount(t, wire.beta, claudecode.BetaContextMgmt, boolCount(tc.context))
			if tc.context && !strings.Contains(wire.body, `"context_management"`) {
				t.Fatalf("context_management intent lost: %s", wire.body)
			}
		})
	}
}

func TestClaudeCodeFinding4DatedContextHTTP(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-20250514", "claude-opus-4-20250514"} {
		t.Run(model, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, false)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    model,
			})
			body := `{"model":"default","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":2048},"context_management":{"edits":[{"type":"compact_20250101"}]},"messages":[{"role":"user","content":"hi"}]}`
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != model {
				t.Fatalf("wire model = %q want %q; body=%s", wire.model, model, wire.body)
			}
			if wire.thinking != "enabled" || wire.budget != 2048 {
				t.Fatalf("thinking = %q budget = %d; body=%s", wire.thinking, wire.budget, wire.body)
			}
			assertBetaFlagCount(t, wire.beta, claudecode.BetaContextMgmt, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaInterleaved, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 0)
		})
	}
}

func TestClaudeCodeFinding4GenericAnthropicUnchanged(t *testing.T) {
	for _, model := range []string{"claude-mythos-5", "claude-sonnet-4-20250514"} {
		t.Run(model, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, false)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolAnthropic,
				baseURL:  upstream.URL,
				model:    model,
			})
			body := `{"model":"default","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`
			resp, out := postSelector(t, selectorClient(tr), base+"/v1/messages", token, body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			wire := captures.next(t)
			captures.assertNoMore(t)
			if wire.model != model {
				t.Fatalf("wire model = %q want %q; body=%s", wire.model, model, wire.body)
			}
			if wire.thinking != "enabled" || wire.budget != 2048 {
				t.Fatalf("generic thinking = %q budget = %d; body=%s", wire.thinking, wire.budget, wire.body)
			}
			if wire.effort != "" || strings.Contains(wire.body, `"type":"adaptive"`) {
				t.Fatalf("generic anthropic gained adaptive or effort: %s", wire.body)
			}
		})
	}
}
