package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/sse"
	"airouter/internal/store"
)

const openaiSSE = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`

const anthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":3,"cache_creation_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

// anthropicToolSSE streams a single tool_use block whose JSON input arrives in
// two partial_json fragments.
const anthropicToolSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`

const responsesSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"up","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello "}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"world"}

event: response.output_text.done
data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"Hello world"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"up","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`

// responsesToolSSE streams a single function_call whose arguments arrive in two
// fragments, to verify backend Responses tool reassembly.
const responsesToolSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_2","model":"up","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","status":"in_progress","call_id":"call_9","name":"get_weather","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"paris\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_2","model":"up","status":"completed","usage":{"input_tokens":3,"output_tokens":9,"total_tokens":12}}}

`

func streamingUpstream(t *testing.T, anthropicBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = io.WriteString(w, anthropicBody)
		case strings.HasSuffix(r.URL.Path, "/responses"):
			_, _ = io.WriteString(w, responsesSSE)
		default:
			_, _ = io.WriteString(w, openaiSSE)
		}
		w.(http.Flusher).Flush()
	}))
}

func setupStreaming(t *testing.T, backend domain.Protocol, anthropicBody string) (string, string) {
	t.Helper()
	base, token, _ := setupStreamingWithStore(t, backend, anthropicBody)
	return base, token
}

func setupStreamingWithStore(t *testing.T, backend domain.Protocol, anthropicBody string) (string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	upstream := streamingUpstream(t, anthropicBody)
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: backend}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model"}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, false, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, key.Token, st
}

func TestStreamMatrix(t *testing.T) {
	cases := []struct {
		name    string
		backend domain.Protocol
		ingress string
	}{
		{"openai->openai", domain.ProtocolOpenAI, "/v1/chat/completions"},
		{"openai->anthropic", domain.ProtocolAnthropic, "/v1/chat/completions"},
		{"anthropic->anthropic", domain.ProtocolAnthropic, "/v1/messages"},
		{"anthropic->openai", domain.ProtocolOpenAI, "/v1/messages"},
		{"openai->responses", domain.ProtocolOpenAIResponses, "/v1/chat/completions"},
		{"anthropic->responses", domain.ProtocolOpenAIResponses, "/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, token := setupStreaming(t, tc.backend, anthropicSSE)
			resp, body := postStream(t, base+tc.ingress, token, `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("content-type = %q", ct)
			}
			text, finished := collectStreamText(t, tc.ingress, body)
			if text != "Hello world" {
				t.Errorf("text = %q, want %q", text, "Hello world")
			}
			if !finished {
				t.Errorf("stream did not signal completion")
			}
		})
	}
}

// Anthropic tool_use stream translated to an OpenAI ingress should reassemble
// into an OpenAI tool_call with concatenated arguments.
func TestStreamToolAnthropicToOpenAI(t *testing.T) {
	base, token := setupStreaming(t, domain.ProtocolAnthropic, anthropicToolSSE)
	resp, body := postStream(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	name, args, finish := collectOpenAIToolStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool name = %q", name)
	}
	if args != `{"city":"paris"}` {
		t.Errorf("tool args = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
}

// A backend Responses tool_call stream translated to an OpenAI ingress should
// reassemble into an OpenAI tool_call with concatenated arguments.
func TestStreamToolResponsesToOpenAI(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesToolSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAIResponses}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model"}}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(st, false, nil).Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	name, args, finish := collectOpenAIToolStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool name = %q", name)
	}
	if args != `{"city":"paris"}` {
		t.Errorf("tool args = %q", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
}

// TestResponsesStreamPassthrough verifies a streaming /v1/responses request to a
// Responses provider relays the SSE unchanged and still sniffs usage out of the
// nested response.usage on response.completed.
func TestResponsesStreamPassthrough(t *testing.T) {
	base, token, st := setupStreamingWithStore(t, domain.ProtocolOpenAIResponses, anthropicSSE)
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","input":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"delta":"world"`) || !strings.Contains(body, "response.completed") {
		t.Errorf("relayed body missing expected events: %s", body)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.InputTokens != 3 || l.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 3/2", l.InputTokens, l.OutputTokens)
	}
}

// TestStreamUsageRecorded verifies token counts are captured from streaming
// responses across the matrix. Both mock streams report input 3 / output 2: the
// translated paths read it through the IR stream events, and the passthrough
// paths sniff it out of the relayed SSE without altering the bytes.
func TestStreamUsageRecorded(t *testing.T) {
	// Anthropic-backed cases see input 3 plus cache_creation 10 = 13; OpenAI-backed
	// cases use the OpenAI mock stream which reports a flat input of 3.
	cases := []struct {
		name    string
		backend domain.Protocol
		ingress string
		wantIn  int
		wantOut int
	}{
		{"openai->openai", domain.ProtocolOpenAI, "/v1/chat/completions", 3, 2},
		{"openai->anthropic", domain.ProtocolAnthropic, "/v1/chat/completions", 13, 2},
		{"anthropic->anthropic", domain.ProtocolAnthropic, "/v1/messages", 13, 2},
		{"anthropic->openai", domain.ProtocolOpenAI, "/v1/messages", 3, 2},
		{"openai->responses", domain.ProtocolOpenAIResponses, "/v1/chat/completions", 3, 2},
		{"anthropic->responses", domain.ProtocolOpenAIResponses, "/v1/messages", 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, token, st := setupStreamingWithStore(t, tc.backend, anthropicSSE)
			resp, body := postStream(t, base+tc.ingress, token, `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			l := waitForLogs(t, st, 1)[0]
			if l.InputTokens != tc.wantIn || l.OutputTokens != tc.wantOut {
				t.Errorf("tokens = %d/%d, want %d/%d", l.InputTokens, l.OutputTokens, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func postStream(t *testing.T, url, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(out)
}

// collectStreamText reconstructs assistant text from a client SSE response in
// whichever ingress format was requested, and whether it terminated cleanly.
func collectStreamText(t *testing.T, ingress, body string) (string, bool) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var text strings.Builder
	finished := false
	anthropic := strings.HasSuffix(ingress, "/messages")
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if anthropic {
			switch ev.Name {
			case "content_block_delta":
				var d struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
				}
				_ = json.Unmarshal(ev.Data, &d)
				text.WriteString(d.Delta.Text)
			case "message_stop":
				finished = true
			}
			continue
		}
		if string(ev.Data) == "[DONE]" {
			finished = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev.Data, &chunk)
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
		}
	}
	return text.String(), finished
}

func collectOpenAIToolStream(t *testing.T, body string) (name, args, finish string) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var argBuf strings.Builder
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		_ = json.Unmarshal(ev.Data, &chunk)
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				if tc.Function.Name != "" {
					name = tc.Function.Name
				}
				argBuf.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	return name, argBuf.String(), finish
}
