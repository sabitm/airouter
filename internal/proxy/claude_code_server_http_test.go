package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"airouter/internal/crypto"
	"airouter/internal/domain"
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
	"airouter/internal/store"
)

type proxyTarget struct {
	protocol domain.Protocol
	handler  http.HandlerFunc
	hits     *atomic.Int32
}

func newProxyWithTargets(t *testing.T, targets ...proxyTarget) (string, string) {
	t.Helper()
	c, err := crypto.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	combo := &domain.Combo{Name: "default", Strategy: domain.StrategyFailover}
	for i, target := range targets {
		hits := target.hits
		if hits == nil {
			hits = &atomic.Int32{}
		}
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			target.handler(w, r)
		}))
		t.Cleanup(up.Close)
		prov := &domain.Provider{Name: "p" + string(rune('0'+i)), BaseURL: up.URL, APIKey: "up-key", Protocol: target.protocol}
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		combo.Targets = append(combo.Targets, domain.ComboTarget{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true})
	}
	if err := st.CreateCombo(ctx, combo); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token
}

func TestHTTPUnmarkedSystemBlocksAndRequestState(t *testing.T) {
	var got []byte
	base, token := newProxyWithTargets(t, proxyTarget{
		protocol: domain.ProtocolClaudeCode,
		handler: func(w http.ResponseWriter, r *http.Request) {
			got, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, anthropicUpstreamBody)
		},
	})
	body := `{"model":"default","max_tokens":8,"system":[{"type":"text","text":"one"},{"type":"text","text":"two; caller","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],"output_config":{"task_budget":{"type":"tokens","total":1000,"remaining":0}},"fallbacks":[{"model":"claude-opus-4-8"}],"container":{"id":"c1"},"messages":[{"role":"user","content":"hi"}]}`
	resp, out := post(t, base+"/v1/messages", token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	var doc struct {
		System    []map[string]any `json:"system"`
		Output    map[string]any   `json:"output_config"`
		Fallbacks []map[string]any `json:"fallbacks"`
		Container map[string]any   `json:"container"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, got)
	}
	var one, two bool
	for _, b := range doc.System {
		if b["text"] == "one" {
			one = true
		}
		if b["text"] == "two; caller" {
			two = true
			sameJSON(t, b["cache_control"], `{"type":"ephemeral","ttl":"1h","scope":"global"}`)
		}
		if b["text"] == "onetwo; caller" {
			t.Fatalf("system blocks were rejoined: %s", got)
		}
	}
	if !one || !two {
		t.Fatalf("system boundaries or caller text lost: %s", got)
	}
	budget, _ := doc.Output["task_budget"].(map[string]any)
	if budget == nil || budget["total"] != float64(1000) || budget["remaining"] != float64(0) {
		t.Fatalf("task_budget=%v body=%s", doc.Output["task_budget"], got)
	}
	if len(doc.Fallbacks) != 1 || doc.Fallbacks[0]["model"] != "claude-opus-4-8" {
		t.Fatalf("fallbacks=%v", doc.Fallbacks)
	}
	if doc.Container["id"] != "c1" {
		t.Fatalf("container=%v", doc.Container)
	}
}

func TestHTTPNestedServerHistoryRejectedBeforeSend(t *testing.T) {
	var hits atomic.Int32
	base, token := newProxyWithTargets(t, proxyTarget{
		protocol: domain.ProtocolOpenAI,
		hits:     &hits,
		handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, openaiUpstreamBody)
		},
	})
	body := `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"search_result","source":"doc","title":"T","content":[{"type":"text","text":"x"}],"citations":{"enabled":true}}]},{"type":"text","text":"continue"}]}]}`
	resp, out := post(t, base+"/v1/messages", token, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if hits.Load() != 0 {
		t.Fatalf("unsafe target received %d requests", hits.Load())
	}
}

func TestHTTPUnaryServerOnlyFailsClosed(t *testing.T) {
	base, token := newProxyWithTargets(t, proxyTarget{
		protocol: domain.ProtocolAnthropic,
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"up","content":[{"type":"compaction","content":"summary"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		},
	})
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("server-only response became success: %s", out)
	}
	if !strings.Contains(string(out), "error") {
		t.Fatalf("no error envelope: %s", out)
	}
}

func anthropicEventStream(events ...string) string {
	return strings.Join(events, "")
}

func TestHTTPStreamPreCommitServerFailover(t *testing.T) {
	first := anthropicEventStream(
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"input\":{}}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	second := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second-target\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var secondHits atomic.Int32
	base, token := newProxyWithTargets(t,
		proxyTarget{protocol: domain.ProtocolAnthropic, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, first)
		}},
		proxyTarget{protocol: domain.ProtocolOpenAI, hits: &secondHits, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, second)
		}},
	)
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if secondHits.Load() != 1 {
		t.Fatalf("pre-commit failure did not fail over; second hits=%d body=%s", secondHits.Load(), out)
	}
	if !strings.Contains(string(out), "second-target") {
		t.Fatalf("failover text missing: %s", out)
	}
}

func TestHTTPStreamPostCommitServerNoRetry(t *testing.T) {
	first := anthropicEventStream(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"committed\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"input\":{}}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	second := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"must-not-appear\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
	var secondHits atomic.Int32
	base, token := newProxyWithTargets(t,
		proxyTarget{protocol: domain.ProtocolAnthropic, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, first)
		}},
		proxyTarget{protocol: domain.ProtocolOpenAI, hits: &secondHits, handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, second)
		}},
	)
	resp, out := post(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if secondHits.Load() != 0 {
		t.Fatalf("post-commit stream retried; second hits=%d", secondHits.Load())
	}
	if !strings.Contains(string(out), "committed") {
		t.Fatalf("first target output was not committed: %s", out)
	}
	if strings.Contains(string(out), "must-not-appear") {
		t.Fatalf("retried after commit: %s", out)
	}
	if !strings.Contains(string(out), "error") {
		t.Fatalf("missing terminal error frame: %s", out)
	}
}

func TestHTTPCompleteServerBlockPreCommitFailover(t *testing.T) {
	fallback := strings.ReplaceAll(openaiSSE, `"model":"up"`, `"model":"fallback-model"`)
	fallback = strings.ReplaceAll(fallback, `"id":"chatcmpl-1"`, `"id":"chatcmpl-fallback"`)
	primary := []struct {
		name   string
		stream string
	}{
		{
			name: "server only",
			stream: anthropicEventStream(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"primary-msg\",\"model\":\"primary-model\",\"usage\":{\"input_tokens\":99,\"output_tokens\":0}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"input\":{}}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			),
		},
		{
			name: "server before text",
			stream: anthropicEventStream(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"primary-msg\",\"model\":\"primary-model\",\"usage\":{\"input_tokens\":99,\"output_tokens\":0}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"input\":{}}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"primary answer\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			),
		},
	}
	ingresses := []struct {
		name  string
		path  string
		body  string
		usage func(*testing.T, string) (int, int, int)
	}{
		{"openai", "/v1/chat/completions", `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`, collectOpenAIUsage},
		{"responses", "/v1/responses", `{"model":"default","input":"hi","stream":true}`, collectResponsesUsage},
	}
	for _, ingress := range ingresses {
		for _, primaryCase := range primary {
			t.Run(ingress.name+"/"+primaryCase.name, func(t *testing.T) {
				var firstHits, secondHits atomic.Int32
				first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					firstHits.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, primaryCase.stream)
					w.(http.Flusher).Flush()
				}))
				t.Cleanup(first.Close)
				second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					secondHits.Add(1)
					body, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(body), `"model":"fallback-model"`) {
						t.Errorf("fallback request model = %s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, fallback)
					w.(http.Flusher).Flush()
				}))
				t.Cleanup(second.Close)

				base, token, px := setupTwo(t, domain.ProtocolAnthropic, first.URL, domain.ProtocolOpenAI, second.URL)
				setComboModels(t, px, "primary-model", "fallback-model")
				resp, out := post(t, base+ingress.path, token, ingress.body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status=%d body=%s", resp.StatusCode, out)
				}
				if firstHits.Load() != 1 || secondHits.Load() != 1 {
					t.Fatalf("hits first=%d second=%d body=%s", firstHits.Load(), secondHits.Load(), out)
				}
				for _, leak := range []string{"primary-model", "primary-msg", "primary answer", "server_tool_use", `"prompt_tokens":99`, `"input_tokens":99`, `"completion_tokens":7`, `"output_tokens":7`} {
					if strings.Contains(string(out), leak) {
						t.Fatalf("first target byte leaked %q: %s", leak, out)
					}
				}
				if !strings.Contains(string(out), "Hello") || !strings.Contains(string(out), "world") {
					t.Fatalf("fallback text missing: %s", out)
				}
				in, output, _ := ingress.usage(t, string(out))
				if in != 3 || output != 2 {
					t.Fatalf("usage = %d/%d, want fallback 3/2; body=%s", in, output, out)
				}
			})
		}
	}
}

func TestHTTPCompleteServerBlockPostCommitNoRetry(t *testing.T) {
	first := anthropicEventStream(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"committed\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"input\":{}}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	second := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"fallback-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"must-not-appear\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
	ingresses := []struct {
		name string
		path string
		body string
	}{
		{"openai", "/v1/chat/completions", `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"default","input":"hi","stream":true}`},
	}
	for _, ingress := range ingresses {
		t.Run(ingress.name, func(t *testing.T) {
			var secondHits atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, first)
				w.(http.Flusher).Flush()
			}))
			t.Cleanup(primary.Close)
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondHits.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, second)
				w.(http.Flusher).Flush()
			}))
			t.Cleanup(fallback.Close)

			base, token, px := setupTwo(t, domain.ProtocolAnthropic, primary.URL, domain.ProtocolOpenAI, fallback.URL)
			setComboModels(t, px, "primary-model", "fallback-model")
			resp, out := post(t, base+ingress.path, token, ingress.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			if secondHits.Load() != 0 {
				t.Fatalf("post-commit stream retried; second hits=%d", secondHits.Load())
			}
			if !strings.Contains(string(out), "committed") {
				t.Fatalf("first target output was not committed: %s", out)
			}
			if strings.Contains(string(out), "must-not-appear") {
				t.Fatalf("retried after commit: %s", out)
			}
			if !strings.Contains(string(out), "error") {
				t.Fatalf("missing terminal error frame: %s", out)
			}
			if strings.Contains(string(out), "[DONE]") || strings.Contains(string(out), "response.completed") {
				t.Fatalf("success trailer after error frame: %s", out)
			}
		})
	}
}

func TestCollectStreamCitationsAndCompaction(t *testing.T) {
	raw := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"char_location\",\"start_char_index\":0,\"end_char_index\":1}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"compaction\",\"content\":\"\",\"encrypted_content\":\"e1\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"compaction_delta\",\"content\":\"summary\",\"encrypted_content\":\"e2\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srvtoolu_1\",\"name\":\"web_search\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"cats\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":\"STOP\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	resp, err := collectStreamResponse(strings.NewReader(raw), anthropicCodec, nil, "up", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("content=%+v", resp.Content)
	}
	if resp.Content[0].Type != ir.BlockText || resp.Content[0].Text != "hello" || len(resp.Content[0].Citations) != 1 || resp.Content[0].Citations[0].Type != "char_location" {
		t.Fatalf("text=%+v", resp.Content[0])
	}
	sameJSON(t, resp.Content[0].Citations[0].Raw, `{"type":"char_location","start_char_index":0,"end_char_index":1}`)
	if resp.Content[1].Type != ir.BlockServer || resp.Content[1].AnthropicServer.WireType != "compaction" {
		t.Fatalf("server=%+v", resp.Content[1])
	}
	sameJSON(t, resp.Content[1].AnthropicServer.Raw, `{"type":"compaction","content":"summary","encrypted_content":"e2"}`)
	if resp.Content[2].Type != ir.BlockServer || resp.Content[2].AnthropicServer.WireType != "server_tool_use" {
		t.Fatalf("server tool=%+v", resp.Content[2])
	}
	sameJSON(t, resp.Content[2].AnthropicServer.Raw, `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"cats"}}`)
	if resp.StopSequence == nil || *resp.StopSequence != "STOP" {
		t.Fatalf("stop_sequence=%v", resp.StopSequence)
	}
}

func TestAnthropicGuardPortability(t *testing.T) {
	inspect := func(body string) *attachmentPrep {
		t.Helper()
		req, err := anthropic.DecodeRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		prep := &attachmentPrep{}
		if err := prep.inspectDecoded(req); err != nil {
			t.Fatal(err)
		}
		return prep
	}
	meta := inspect(`{"model":"m","max_tokens":8,"metadata":{"user_id":"u"},"messages":[{"role":"user","content":"hi"}]}`)
	if meta.preserveAnthropicState {
		t.Fatal("metadata was treated as owned state")
	}
	if got := meta.checkThinking(openaiCodec); got != "" {
		t.Fatalf("metadata blocked OpenAI: %s", got)
	}
	if got := meta.checkThinking(cursorCodec); got != "" {
		t.Fatalf("metadata blocked Cursor: %s", got)
	}
	for _, body := range []string{
		`{"model":"m","max_tokens":8,"metadata":null,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":8,"metadata":{},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":8,"context_management":null,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","max_tokens":8,"context_management":{"edits":[]},"messages":[{"role":"user","content":"hi"}]}`,
	} {
		if prep := inspect(body); prep.preserveAnthropicState {
			t.Fatalf("empty state blocked targets: %s", body)
		}
	}

	format := inspect(`{"model":"m","max_tokens":8,"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`)
	if format.checkThinking(openaiCodec) != "" || format.checkThinking(responsesCodec) != "" {
		t.Fatal("output format blocked a mapping target")
	}
	if format.checkThinking(cursorCodec) == "" {
		t.Fatal("output format reached a target with no representation")
	}

	strict := inspect(`{"model":"m","max_tokens":8,"tools":[{"name":"t","input_schema":{"type":"object"},"strict":true}],"messages":[{"role":"user","content":"hi"}]}`)
	for name, c := range map[string]codec{"openai": openaiCodec, "responses": responsesCodec, "cursor": cursorCodec} {
		if strict.checkThinking(c) != "" {
			t.Fatalf("strict custom tool blocked portable target %s", name)
		}
	}

	deferred := inspect(`{"model":"m","max_tokens":8,"tools":[{"name":"t","input_schema":{"type":"object"},"defer_loading":true}],"messages":[{"role":"user","content":"hi"}]}`)
	if deferred.checkThinking(openaiCodec) == "" {
		t.Fatal("provider-specific defer_loading reached OpenAI")
	}
	if deferred.checkThinking(anthropicCodec) != "" || deferred.checkThinking(opencodeMessagesCodec) != "" {
		t.Fatal("defer_loading blocked a preserving target")
	}

	nested := inspect(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"search_result","source":"d","title":"T","content":[{"type":"text","text":"x"}],"citations":{"enabled":true}}]}]}]}`)
	if nested.checkThinking(openaiCodec) == "" {
		t.Fatal("nested server state reached OpenAI")
	}
}

func TestHTTPAnthropicResponseStateRoundTrip(t *testing.T) {
	const upstreamBody = `{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[{"type":"text","text":"hello","citations":[{"type":"char_location","document_index":0,"start_char_index":1,"end_char_index":4,"document_title":"Doc","cited_text":"ell"}]}],"stop_reason":"stop_sequence","stop_sequence":"STOP","usage":{"input_tokens":2,"output_tokens":3},"container":{"id":"c1","expires_at":"2030-01-01T00:00:00Z","extra":"keep"},"context_management":{"edits":[{"type":"compact_x","keep":true}]},"stop_details":{"type":"refusal","reason":"x"},"input_transformations":[{"type":"thinking_dropped","path":"messages.0","reason":"prefix_binding_mismatch"}]}`
	base, token := newProxyWithTargets(t, proxyTarget{
		protocol: domain.ProtocolClaudeCode,
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, upstreamBody)
		},
	})
	resp, out := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	var got struct {
		Content              []map[string]json.RawMessage `json:"content"`
		StopReason           string                       `json:"stop_reason"`
		StopSequence         *string                      `json:"stop_sequence"`
		Container            json.RawMessage              `json:"container"`
		ContextManagement    json.RawMessage              `json:"context_management"`
		StopDetails          json.RawMessage              `json:"stop_details"`
		InputTransformations json.RawMessage              `json:"input_transformations"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("response: %v body=%s", err, out)
	}
	if len(got.Content) != 1 || got.StopReason != "stop_sequence" || got.StopSequence == nil || *got.StopSequence != "STOP" {
		t.Fatalf("response state = %+v", got)
	}
	if string(got.Content[0]["citations"]) != `[{"type":"char_location","document_index":0,"start_char_index":1,"end_char_index":4,"document_title":"Doc","cited_text":"ell"}]` {
		t.Fatalf("citations = %s", got.Content[0]["citations"])
	}
	for name, pair := range map[string][2]string{
		"container":             {string(got.Container), `{"id":"c1","expires_at":"2030-01-01T00:00:00Z","extra":"keep"}`},
		"context_management":    {string(got.ContextManagement), `{"edits":[{"type":"compact_x","keep":true}]}`},
		"stop_details":          {string(got.StopDetails), `{"type":"refusal","reason":"x"}`},
		"input_transformations": {string(got.InputTransformations), `[{"type":"thinking_dropped","path":"messages.0","reason":"prefix_binding_mismatch"}]`},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s = %s, want %s", name, pair[0], pair[1])
		}
	}
}

func readSSEEvents(t *testing.T, body string) []sse.Event {
	t.Helper()
	var events []sse.Event
	r := sse.NewReader(strings.NewReader(body))
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("parse SSE: %v body=%s", err, body)
		}
		events = append(events, ev)
	}
}

func TestHTTPAnthropicServerStreamRoundTrip(t *testing.T) {
	upstream := anthropicEventStream(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"container\":{\"id\":\"c1\"},\"context_management\":{\"edits\":[]},\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"web_search_result_location\",\"url\":\"https://example.test/doc\",\"title\":\"Doc\",\"cited_text\":\"Hello\"}}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srvtoolu_1\",\"name\":\"web_search\",\"input\":{}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"cats\\\"}\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"compaction\",\"content\":\"\",\"encrypted_content\":\"e1\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"compaction_delta\",\"content\":\"summary\",\"encrypted_content\":\"e2\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"stop_sequence\",\"stop_sequence\":\"STOP\",\"container\":{\"id\":\"c2\"}},\"context_management\":{\"edits\":[{\"type\":\"compact_x\"}]},\"input_transformations\":[{\"type\":\"thinking_dropped\",\"path\":\"messages.0\",\"reason\":\"x\"}],\"usage\":{\"output_tokens\":4}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	base, token := newProxyWithTargets(t, proxyTarget{
		protocol: domain.ProtocolClaudeCode,
		handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, upstream)
		},
	})
	resp, out := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	events := readSSEEvents(t, string(out))
	starts := map[int]string{}
	var textDeltas, citations, serverDeltas []map[string]any
	var compaction map[string]any
	var finish map[string]any
	var stops []int
	for _, ev := range events {
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("event %s: %v %s", ev.Name, err, ev.Data)
		}
		switch ev.Name {
		case "content_block_start":
			idx := int(payload["index"].(float64))
			block := payload["content_block"].(map[string]any)
			typ, _ := block["type"].(string)
			starts[idx] = typ
			if typ == "server_tool_use" && block["id"] != "srvtoolu_1" {
				t.Fatalf("server tool id = %v", block["id"])
			}
		case "content_block_stop":
			stops = append(stops, int(payload["index"].(float64)))
		case "content_block_delta":
			delta := payload["delta"].(map[string]any)
			switch delta["type"] {
			case "text_delta":
				textDeltas = append(textDeltas, delta)
			case "citations_delta":
				citations = append(citations, delta)
			case "input_json_delta":
				serverDeltas = append(serverDeltas, map[string]any{"index": payload["index"], "delta": delta})
			case "compaction_delta":
				compaction = map[string]any{"index": payload["index"], "delta": delta}
			}
		case "message_delta":
			finish = payload
		}
	}
	if starts[0] != "text" || starts[1] != "server_tool_use" || starts[2] != "compaction" {
		t.Fatalf("block starts = %v", starts)
	}
	if len(textDeltas) != 1 || textDeltas[0]["text"] != "Hello" {
		t.Fatalf("text deltas = %v", textDeltas)
	}
	if len(citations) != 1 {
		t.Fatalf("citations = %v", citations)
	}
	citation := citations[0]["citation"].(map[string]any)
	if citation["type"] != "web_search_result_location" || citation["url"] != "https://example.test/doc" || citation["title"] != "Doc" || citation["cited_text"] != "Hello" {
		t.Fatalf("citation = %v", citation)
	}
	if len(serverDeltas) != 2 {
		t.Fatalf("server deltas = %v", serverDeltas)
	}
	if serverDeltas[0]["index"] != float64(1) || serverDeltas[0]["delta"].(map[string]any)["partial_json"] != `{"query":` {
		t.Fatalf("first fragment = %v", serverDeltas[0])
	}
	if serverDeltas[1]["index"] != float64(1) || serverDeltas[1]["delta"].(map[string]any)["partial_json"] != `"cats"}` {
		t.Fatalf("second fragment = %v", serverDeltas[1])
	}
	if compaction == nil || compaction["index"] != float64(2) {
		t.Fatalf("compaction = %v", compaction)
	}
	cd := compaction["delta"].(map[string]any)
	if cd["content"] != "summary" || cd["encrypted_content"] != "e2" {
		t.Fatalf("compaction delta = %v", cd)
	}
	if len(stops) != 3 || stops[0] != 0 || stops[1] != 1 || stops[2] != 2 {
		t.Fatalf("stops = %v", stops)
	}
	if finish == nil {
		t.Fatal("missing message_delta")
	}
	delta := finish["delta"].(map[string]any)
	if delta["stop_reason"] != "stop_sequence" || delta["stop_sequence"] != "STOP" {
		t.Fatalf("finish delta = %v", delta)
	}
	sameJSON(t, delta["container"], `{"id":"c2"}`)
	sameJSON(t, finish["context_management"], `{"edits":[{"type":"compact_x"}]}`)
	sameJSON(t, finish["input_transformations"], `[{"type":"thinking_dropped","path":"messages.0","reason":"x"}]`)
}

func sameJSON(t *testing.T, got any, want string) {
	t.Helper()
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if json.Unmarshal(gotRaw, &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		t.Fatalf("invalid JSON: %s / %s", gotRaw, want)
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Fatalf("JSON = %s, want %s", aj, bj)
	}
}

func TestHTTPStructuredOutputMapped(t *testing.T) {
	for name, tc := range map[string]struct {
		protocol domain.Protocol
		body     string
		check    func(t *testing.T, doc map[string]any)
	}{
		"openai": {
			protocol: domain.ProtocolOpenAI,
			body:     `{"id":"c","object":"chat.completion","created":1,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			check: func(t *testing.T, doc map[string]any) {
				rf := doc["response_format"].(map[string]any)
				js := rf["json_schema"].(map[string]any)
				if rf["type"] != "json_schema" || js["name"] != "Out" {
					t.Fatalf("response_format = %v", rf)
				}
				schema := js["schema"].(map[string]any)
				if schema["type"] != "object" || schema["title"] != "Out" {
					t.Fatalf("schema = %v", js["schema"])
				}
			},
		},
		"responses": {
			protocol: domain.ProtocolOpenAIResponses,
			body:     `{"id":"r","object":"response","created_at":1,"model":"up","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			check: func(t *testing.T, doc map[string]any) {
				text := doc["text"].(map[string]any)
				format := text["format"].(map[string]any)
				if format["type"] != "json_schema" || format["name"] != "Out" {
					t.Fatalf("text.format = %v", format)
				}
				schema := format["schema"].(map[string]any)
				if schema["type"] != "object" || schema["title"] != "Out" {
					t.Fatalf("schema = %v", format["schema"])
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got []byte
			base, token := newProxyWithTargets(t, proxyTarget{
				protocol: tc.protocol,
				handler: func(w http.ResponseWriter, r *http.Request) {
					got, _ = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
				},
			})
			req := `{"model":"default","max_tokens":8,"metadata":{"user_id":"portable"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object","title":"Out","properties":{}}}},"messages":[{"role":"user","content":"hi"}]}`
			resp, out := post(t, base+"/v1/messages", token, req)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			var doc map[string]any
			if err := json.Unmarshal(got, &doc); err != nil {
				t.Fatalf("upstream body: %v %s", err, got)
			}
			tc.check(t, doc)
			meta, _ := doc["metadata"].(map[string]any)
			if meta["user_id"] != "portable" {
				t.Fatalf("metadata = %v body=%s", doc["metadata"], got)
			}
		})
	}
}

func TestHTTPStreamServerFailures(t *testing.T) {
	cases := map[string]string{
		"malformed citation": anthropicEventStream(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{}}}\n\n",
		),
		"truncated server": anthropicEventStream(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"input\":{}}}\n\n",
		),
		"oversized server": anthropicEventStream(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"up\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"w\",\"pad\":\""+strings.Repeat("x", 1<<20)+"\"}}\n\n",
		),
	}
	for name, upstream := range cases {
		t.Run(name, func(t *testing.T) {
			base, token := newProxyWithTargets(t, proxyTarget{
				protocol: domain.ProtocolClaudeCode,
				handler: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, upstream)
				},
			})
			resp, out := post(t, base+"/v1/messages", token, `{"model":"default","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode == http.StatusOK {
				if !strings.Contains(string(out), `"type":"error"`) {
					t.Fatalf("stream failure became apparent success: %s", out)
				}
				if strings.Contains(string(out), `"type":"message_stop"`) {
					t.Fatalf("failure emitted message_stop: %s", out)
				}
				return
			}
			if !strings.Contains(string(out), "error") {
				t.Fatalf("status=%d without error envelope: %s", resp.StatusCode, out)
			}
		})
	}
}
