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

const anthropicLateUsageSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_late","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":735,"output_tokens":14,"cache_creation_input_tokens":0,"cache_read_input_tokens":1536}}

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

// openAIToolSSE streams a single tool_call whose function.arguments arrive
// in two fragments, mirroring how OpenAI Chat Completions backends stream a
// tool call. Used to exercise the OpenAI stream decoder's fragment reassembly
// on a translate path to an Anthropic ingress.
const openAIToolSSE = `data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"paris\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}

data: [DONE]

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
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
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
		{"openai->claude-code", domain.ProtocolClaudeCode, "/v1/chat/completions"},
		{"anthropic->claude-code", domain.ProtocolClaudeCode, "/v1/messages"},
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

// TestStreamToolClaudeCodeDecloak verifies a claude-code backend stream with a
// cloaked tool_use name is decloaked to the original on the way to an OpenAI
// ingress.
func TestStreamToolClaudeCodeDecloak(t *testing.T) {
	cloaked := strings.Replace(anthropicToolSSE, `"name":"get_weather"`, `"name":"get_weather_ide"`, 1)
	base, token := setupStreaming(t, domain.ProtocolClaudeCode, cloaked)
	resp, body := postStream(t, base+"/v1/chat/completions", token, `{"model":"default","stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	name, args, finish := collectOpenAIToolStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather (decloaked)", name)
	}
	if !strings.Contains(args, "paris") {
		t.Errorf("tool args = %q, want paris", args)
	}
	if finish == "" {
		t.Error("stream did not finish")
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
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
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

// A backend OpenAI Chat Completions tool_call stream translated to an Anthropic
// ingress should reassemble into an Anthropic tool_use block (content_block_start
// + input_json_delta fragments + content_block_stop) and terminate with
// stop_reason "tool_use" on message_delta. Mirrors the Anthropic/Responses
// backend tool-stream tests but for the OpenAI backend direction, exercising
// openai.DecodeStream's fragment reassembly (EventToolCallStart/Delta by Index)
// and anthropic.StreamEncoder's tool_use emission end-to-end.
func TestStreamToolOpenAIToAnthropic(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openAIToolSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/messages", key.Token, `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"weather?"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	name, args, stop, stopped := collectAnthropicToolStream(t, body)
	if name != "get_weather" {
		t.Errorf("tool name = %q", name)
	}
	if args != `{"city":"paris"}` {
		t.Errorf("tool args = %q", args)
	}
	if stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
	if !stopped {
		t.Error("stream did not signal message_stop")
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

// TestOpenAIStreamPassthroughForcesIncludeUsage verifies OpenAI->OpenAI streaming
// passthrough injects stream_options.include_usage=true so upstream emits a
// terminal usage chunk that reaches both the client SSE and the request log.
func TestOpenAIStreamPassthroughForcesIncludeUsage(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var fwd map[string]any
	if err := json.Unmarshal(gotBody, &fwd); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, gotBody)
	}
	opts, ok := fwd["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage=true", fwd["stream_options"])
	}
	in, out, total := collectOpenAIUsage(t, body)
	if in != 3 || out != 2 || total != 5 {
		t.Errorf("client usage = %d/%d/%d, want 3/2/5", in, out, total)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.InputTokens != 3 || l.OutputTokens != 2 {
		t.Errorf("logged tokens = %d/%d, want 3/2", l.InputTokens, l.OutputTokens)
	}
}

func TestAnthropicStreamLateUsageRecordedAndForwarded(t *testing.T) {
	base, token, st := setupStreamingWithStore(t, domain.ProtocolAnthropic, anthropicLateUsageSSE)
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	in, out, total := collectOpenAIUsage(t, body)
	if in != 2271 || out != 14 || total != 2285 {
		t.Errorf("client usage = %d/%d/%d, want 2271/14/2285", in, out, total)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.InputTokens != 2271 || l.OutputTokens != 14 {
		t.Errorf("logged tokens = %d/%d, want 2271/14", l.InputTokens, l.OutputTokens)
	}
}

// TestStreamUsageChunk asserts the client-facing SSE usage carries input
// tokens on the translate path, not just output. TestStreamUsageRecorded only
// checks the log (res.inTok), which is set from the IR and can be correct while
// the ingress encoder drops input on the wire - exactly the OpenAI encoder bug
// that left pi's context gauge tracking output length instead of context size.
func TestStreamUsageChunk(t *testing.T) {
	cases := []struct {
		name    string
		backend domain.Protocol
		ingress string
		body    string
		wantIn  int
		wantOut int
	}{
		// OpenAI ingress encoder; Anthropic backend reports input at message_start.
		{"openai<-anthropic", domain.ProtocolAnthropic, "/v1/chat/completions",
			`{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 13, 2},
		// OpenAI ingress encoder; Responses backend reports input at finish.
		{"openai<-responses", domain.ProtocolOpenAIResponses, "/v1/chat/completions",
			`{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 3, 2},
		// Responses ingress encoder; OpenAI backend reports input at finish (late).
		{"responses<-openai", domain.ProtocolOpenAI, "/v1/responses",
			`{"model":"default","input":"hi","stream":true}`, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, token, _ := setupStreamingWithStore(t, tc.backend, anthropicSSE)
			resp, body := postStream(t, base+tc.ingress, token, tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
			in, out, total := -1, -1, -1
			if strings.HasPrefix(tc.ingress, "/v1/responses") {
				in, out, total = collectResponsesUsage(t, body)
			} else {
				in, out, total = collectOpenAIUsage(t, body)
			}
			if in != tc.wantIn || out != tc.wantOut {
				t.Errorf("usage = in %d/out %d (total %d), want in %d/out %d", in, out, total, tc.wantIn, tc.wantOut)
			}
			if total != in+out {
				t.Errorf("total = %d, want %d (in+out)", total, in+out)
			}
		})
	}
}

// TestStreamErrorBodyCapped asserts an upstream error body is not buffered in
// full: only upstreamErrorMax bytes are read before being surfaced in the
// ingress error envelope. The passthrough path returns the upstream status and a
// best-effort message, so we assert the client sees the upstream status and no
// more than upstreamErrorMax bytes of body are held.
func TestStreamErrorBodyCapped(t *testing.T) {
	// Upstream returns a huge error body on a streaming request.
	bigErr := strings.Repeat("x", upstreamErrorMax+1<<20) // 1 MiB over the cap
	bigErrLen := len(bigErr)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, bigErr)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "m", Enabled: true}}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	// The non-JSON error body is surfaced verbatim as the error.message, so the
	// cap directly bounds the client-facing envelope: it must be no larger than
	// upstreamErrorMax plus the small JSON envelope overhead, and strictly less
	// than the full upstream body.
	if len(body) > upstreamErrorMax+512 {
		t.Fatalf("client body %d bytes exceeds cap+envelope; want <= %d", len(body), upstreamErrorMax+512)
	}
	if len(body) >= bigErrLen {
		t.Fatalf("client body %d not capped (full upstream was %d)", len(body), bigErrLen)
	}
	if !strings.Contains(body, "error") {
		t.Fatalf("want error envelope, got: %s", body)
	}
}

// TestStreamErrorEnvelopePerIngress verifies a pre-commit upstream error on a
// translate streaming path is surfaced in the ingress format's own error
// envelope. streamTranslated returns retryable on a non-2xx upstream status; the
// resolution loop (single target here) then calls res.fail -> writeErr ->
// c.encodeError, which must render the ingress format's envelope shape. The
// upstream error.message is extracted and forwarded; errType is always "api_error"
// for upstream failures.
func TestStreamErrorEnvelopePerIngress(t *testing.T) {
	cases := []struct {
		name    string
		ingress string
		backend domain.Protocol
	}{
		{"openai ingress", "/v1/chat/completions", domain.ProtocolAnthropic},
		{"anthropic ingress", "/v1/messages", domain.ProtocolOpenAI},
		{"responses ingress", "/v1/responses", domain.ProtocolOpenAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream is down","type":"overloaded_error"}}`)
			}))
			t.Cleanup(upstream.Close)

			st := newTestStore(t)
			ctx := context.Background()
			prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: tc.backend}
			if err := st.CreateProvider(ctx, prov); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}}}); err != nil {
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

			reqBody := `{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
			if strings.HasPrefix(tc.ingress, "/v1/responses") {
				reqBody = `{"model":"default","input":"hi","stream":true}`
			}
			resp, body := postStream(t, ts.URL+tc.ingress, key.Token, reqBody)
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502; body = %s", resp.StatusCode, body)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("body not valid JSON in ingress envelope: %s", body)
			}
			errObj, ok := got["error"]
			if !ok {
				t.Fatalf("missing top-level error object: %s", body)
			}
			var e struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"`
				Param   any    `json:"param"`
			}
			if err := json.Unmarshal(errObj, &e); err != nil {
				t.Fatalf("error object malformed: %s", body)
			}
			if e.Message != "upstream is down" {
				t.Errorf("error.message = %q, want upstream is down", e.Message)
			}
			if e.Type != "api_error" {
				t.Errorf("error.type = %q, want api_error", e.Type)
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

// collectOpenAIUsage extracts the final usage-only chunk (empty choices) from an
// OpenAI Chat Completions SSE stream.
func collectOpenAIUsage(t *testing.T, body string) (in, out, total int) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(ev.Data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct{} `json:"choices"`
			Usage   *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(ev.Data, &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && len(chunk.Choices) == 0 {
			return chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens
		}
	}
	return 0, 0, 0
}

// collectResponsesUsage extracts usage from the response.completed event of a
// Responses SSE stream.
func collectResponsesUsage(t *testing.T, body string) (in, out, total int) {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Name != "response.completed" {
			continue
		}
		var d struct {
			Response struct {
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		if d.Response.Usage != nil {
			return d.Response.Usage.InputTokens, d.Response.Usage.OutputTokens, d.Response.Usage.TotalTokens
		}
	}
	return 0, 0, 0
}

// collectAnthropicToolStream reconstructs an Anthropic tool_use block from a
// client SSE response: the tool name from content_block_start, the arguments
// from concatenated input_json_delta fragments, the stop_reason from
// message_delta, and whether message_stop terminated the stream.
func collectAnthropicToolStream(t *testing.T, body string) (name, args, stop string, stopped bool) {
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
		switch ev.Name {
		case "content_block_start":
			var d struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			if d.ContentBlock.Type == "tool_use" {
				name = d.ContentBlock.Name
			}
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			if d.Delta.Type == "input_json_delta" {
				argBuf.WriteString(d.Delta.PartialJSON)
			}
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			_ = json.Unmarshal(ev.Data, &d)
			if d.Delta.StopReason != "" {
				stop = d.Delta.StopReason
			}
		case "message_stop":
			stopped = true
		}
	}
	return name, argBuf.String(), stop, stopped
}

// Codex/Responses overload sequence matching the verified HAR capture.
const codexOverloadSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_overload","model":"up","status":"in_progress"}}

event: response.in_progress
data: {"type":"response.in_progress","response":{"id":"resp_overload","model":"up","status":"in_progress"}}

event: error
data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":2}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_overload","object":"response","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"output":[],"usage":null}}

`

func TestStreamPreCommitOverloadJSONError(t *testing.T) {
	// Single Codex target returns HTTP 200 + HAR overload sequence. Client must
	// get a non-200 JSON OpenAI error (no role/finish/usage/[DONE]).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, codexOverloadSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	st := newTestStore(t)
	ctx := context.Background()
	prov := &domain.Provider{Name: "codex", BaseURL: upstream.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "gpt", Enabled: true}}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	if strings.Contains(body, "finish_reason") || strings.Contains(body, "[DONE]") ||
		strings.Contains(body, `"role":"assistant"`) || strings.Contains(body, "prompt_tokens") {
		t.Fatalf("unexpected success SSE fragments in body: %s", body)
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("body not JSON: %s", body)
	}
	if !strings.Contains(env.Error.Message, "overloaded") {
		t.Errorf("message = %q", env.Error.Message)
	}
}

func TestStreamPreCommitOverloadFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, codexOverloadSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "codex-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	p2 := &domain.Provider{Name: "oai-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "overloaded") || strings.Contains(body, "resp_overload") {
		t.Fatalf("first target bytes leaked: %s", body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
}

func TestOpenAIEmptyChoicesErrorFailover(t *testing.T) {
	const openaiErrorSSE = `data: {"id":"chatcmpl-error","object":"chat.completion.chunk","model":"up","choices":[],"error":{"message":"servers overloaded","type":"server_error","code":"server_is_overloaded"}}

`
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiErrorSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "oai-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "oai-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/messages", key.Token,
		`{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "overloaded") || strings.Contains(body, "chatcmpl-error") {
		t.Fatalf("first target bytes leaked: %s", body)
	}
	text, finished := collectStreamText(t, "/v1/messages", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
	if got := backoffSkips(px, p1.ID); got == 0 {
		t.Fatal("failed provider was not penalized")
	}
}

func TestStreamPostCommitErrorNoFailover(t *testing.T) {
	// First target streams real text then an error frame. Second must not be called.
	const responsesPartialErr = `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"up","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"partial"}

event: error
data: {"type":"error","error":{"type":"server_error","code":"boom","message":"mid-stream boom"}}

`
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesPartialErr)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "r-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "oai-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 {
		t.Fatalf("n1=%d", n1)
	}
	if n2 != 0 {
		t.Fatalf("second target called n2=%d", n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "partial") {
		t.Errorf("missing partial content: %s", body)
	}
	if !strings.Contains(body, "mid-stream boom") {
		t.Errorf("missing ingress error frame: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "[DONE]") ||
		strings.Contains(body, "prompt_tokens") {
		t.Fatalf("success trailer after error frame: %s", body)
	}
}

func TestStreamFailureBackoff(t *testing.T) {
	// Pre-commit overload on target1 penalizes it; success on target2 is not
	// penalized. A subsequent committed failure on a later request keeps penalty.
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, codexOverloadSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAICodex}
	p2 := &domain.Provider{Name: "good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if got := backoffSkips(px, p1.ID); got == 0 {
		t.Fatalf("pre-commit failure should penalize p1, skips=%d", got)
	}
	if got := backoffSkips(px, p2.ID); got != 0 {
		t.Fatalf("successful p2 must not be penalized, skips=%d", got)
	}

	// Now force a committed failure on a penalized provider and ensure the
	// penalty survives: bytes went out so failover is impossible, but the
	// provider was still unhealthy and must not be marked healthy again.
	const partialErr = `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"up","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"x"}

event: error
data: {"type":"error","error":{"type":"server_error","message":"late fail"}}

`
	// Single-target combo on p2 via a dedicated server returning partial+error.
	up3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, partialErr)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up3.Close)
	p3 := &domain.Provider{Name: "partial", BaseURL: up3.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	if err := st.CreateProvider(ctx, p3); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "partial", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p3.ID, UpstreamModel: "m", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	px.penalizeProvider(p3.ID)
	before3 := backoffSkips(px, p3.ID)
	resp2, body2 := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"partial","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("partial status=%d body=%s", resp2.StatusCode, body2)
	}
	after3 := backoffSkips(px, p3.ID)
	if after3 < before3 {
		t.Fatalf("committed failure cleared backoff (before=%d after=%d)", before3, after3)
	}
}

func TestStreamEmptyBodyFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Zero decodable events.
		_, _ = io.WriteString(w, ": keep-alive\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "empty", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	text, _ := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
}

const responsesPreCommitErrSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_bad","model":"up","status":"in_progress"}}

event: response.in_progress
data: {"type":"response.in_progress","response":{"id":"resp_bad","model":"up","status":"in_progress"}}

event: error
data: {"type":"error","error":{"type":"server_error","code":"boom","message":"precommit boom"}}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_bad","status":"failed","error":{"code":"boom","message":"precommit boom"},"output":[],"usage":null}}

`

const openaiPreCommitErrSSE = `data: {"id":"chatcmpl-error","object":"chat.completion.chunk","model":"up","choices":[],"error":{"message":"servers overloaded","type":"server_error","code":"server_is_overloaded"}}

`

const anthropicPreCommitErrSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_bad","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`

const openaiPostCommitErrSSE = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}

data: {"error":{"message":"mid-stream boom","type":"server_error","code":"boom"}}

`

func TestResponsesPassthroughPreCommitErrorFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesPreCommitErrSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "r-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "r-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/responses", key.Token, `{"model":"default","input":"hi","stream":true}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "precommit boom") || strings.Contains(body, "resp_bad") {
		t.Fatalf("first target bytes leaked: %s", body)
	}
	if !strings.Contains(body, `"delta":"world"`) || !strings.Contains(body, "response.completed") {
		t.Errorf("fallback stream missing expected events: %s", body)
	}
	if got := backoffSkips(px, p1.ID); got == 0 {
		t.Fatal("failed provider was not penalized")
	}
}

func TestOpenAIPassthroughPreCommitErrorFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiPreCommitErrSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "oai-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "oai-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "overloaded") || strings.Contains(body, "chatcmpl-error") {
		t.Fatalf("first target bytes leaked: %s", body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
	if got := backoffSkips(px, p1.ID); got == 0 {
		t.Fatal("failed provider was not penalized")
	}
}

func TestAnthropicPassthroughPreCommitErrorFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicPreCommitErrSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "anth-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	p2 := &domain.Provider{Name: "anth-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/messages", key.Token,
		`{"model":"default","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "Overloaded") || strings.Contains(body, "msg_bad") {
		t.Fatalf("first target bytes leaked: %s", body)
	}
	text, finished := collectStreamText(t, "/v1/messages", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
	if got := backoffSkips(px, p1.ID); got == 0 {
		t.Fatal("failed provider was not penalized")
	}
}

func TestPassthroughSuccessfulLifecycleFlush(t *testing.T) {
	base, token, st := setupStreamingWithStore(t, domain.ProtocolOpenAIResponses, anthropicSSE)
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","input":"hi","stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "event: response.created") {
		t.Errorf("missing buffered created event: %s", body)
	}
	if !strings.Contains(body, `"delta":"Hello "`) || !strings.Contains(body, `"delta":"world"`) {
		t.Errorf("missing text deltas: %s", body)
	}
	if !strings.Contains(body, "response.completed") {
		t.Errorf("missing completed trailer: %s", body)
	}
	createdAt := strings.Index(body, "event: response.created")
	helloAt := strings.Index(body, `"delta":"Hello "`)
	worldAt := strings.Index(body, `"delta":"world"`)
	completedAt := strings.Index(body, "response.completed")
	if createdAt < 0 || helloAt < createdAt || worldAt < helloAt || completedAt < worldAt {
		t.Fatalf("event order wrong: created=%d hello=%d world=%d completed=%d", createdAt, helloAt, worldAt, completedAt)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.InputTokens != 3 || l.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 3/2", l.InputTokens, l.OutputTokens)
	}
}

func TestOpenAIPassthroughPostCommitNativeError(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiPostCommitErrSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "oai-partial", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "oai-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 {
		t.Fatalf("n1=%d", n1)
	}
	if n2 != 0 {
		t.Fatalf("second target called n2=%d", n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "partial") {
		t.Errorf("missing partial content: %s", body)
	}
	if strings.Count(body, "mid-stream boom") != 1 {
		t.Fatalf("want exactly one native error, got: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "[DONE]") ||
		strings.Contains(body, "prompt_tokens") {
		t.Fatalf("success trailer after error frame: %s", body)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.ErrMsg == "" || !strings.Contains(l.ErrMsg, "mid-stream boom") {
		t.Fatalf("request log missing failure: %+v", l)
	}
	// Existing penalty must survive a later committed native-error stream.
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "partial", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	px.penalizeProvider(p1.ID)
	before := backoffSkips(px, p1.ID)
	resp2, body2 := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"partial","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("partial status=%d body=%s", resp2.StatusCode, body2)
	}
	after := backoffSkips(px, p1.ID)
	if after < before {
		t.Fatalf("committed failure cleared backoff (before=%d after=%d)", before, after)
	}
}

func TestPassthroughPreCommitReadFailureFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Hijack and drop the connection after headers so the reader sees a transport error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijack unsupported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
		_ = conn.Close()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "drop", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
}

func TestPassthroughPostCommitReadFailureNoFailover(t *testing.T) {
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "drop-late", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	p2 := &domain.Provider{Name: "good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 {
		t.Fatalf("n1=%d", n1)
	}
	if n2 != 0 {
		t.Fatalf("second target called n2=%d", n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "partial") {
		t.Errorf("missing partial content: %s", body)
	}
	if strings.Contains(body, "Hello world") || strings.Contains(body, "[DONE]") {
		t.Fatalf("unexpected success continuation: %s", body)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.ErrMsg == "" {
		t.Fatalf("request log missing failure: %+v", l)
	}
}

const anthropicPreCommitUsageSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":1234,"output_tokens":0}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`

const openaiNoUsageSSE = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"Hello world"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

const openaiWinnerUsageSSE = `data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{"content":"Hello world"},"finish_reason":null}]}

data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`

const anthropicFinalFailUsageSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_final","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":77,"output_tokens":0}}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"final fail"}}

`

const anthropicCommittedFailUsageSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_commit","type":"message","role":"assistant","model":"up","content":[],"stop_reason":null,"usage":{"input_tokens":1234,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

event: error
data: {"type":"error","error":{"type":"server_error","message":"mid-stream boom"}}

`

func findLogByProvider(t *testing.T, logs []*domain.RequestLog, name string) *domain.RequestLog {
	t.Helper()
	for _, l := range logs {
		if l.Provider == name {
			return l
		}
	}
	t.Fatalf("no request log for provider %q", name)
	return nil
}

func setupTranslatedFailover(t *testing.T, p1Name, p2Name, p1Body, p2Body string, p2Proto domain.Protocol) (string, string, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, p1Body)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, p2Body)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	p1 := &domain.Provider{Name: p1Name, BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	p2 := &domain.Provider{Name: p2Name, BaseURL: up2.URL, APIKey: "k", Protocol: p2Proto}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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
	return ts.URL, key.Token, st
}

func TestTranslatedStreamFailedUsageDoesNotLeakToWinnerWithoutUsage(t *testing.T) {
	base, token, st := setupTranslatedFailover(t, "anth-bad", "oai-good", anthropicPreCommitUsageSSE, openaiNoUsageSSE, domain.ProtocolOpenAI)
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
	logs := waitForLogs(t, st, 2)
	failed := findLogByProvider(t, logs, "anth-bad")
	if failed.InputTokens != 1234 || failed.OutputTokens != 0 {
		t.Errorf("failed row tokens = %d/%d, want 1234/0", failed.InputTokens, failed.OutputTokens)
	}
	if failed.ErrMsg == "" {
		t.Errorf("failed row missing err: %+v", failed)
	}
	winner := findLogByProvider(t, logs, "oai-good")
	if winner.InputTokens != 0 || winner.OutputTokens != 0 {
		t.Errorf("winner tokens = %d/%d, want 0/0", winner.InputTokens, winner.OutputTokens)
	}
	if winner.ErrMsg != "" {
		t.Errorf("winner err = %q, want empty", winner.ErrMsg)
	}
}

func TestTranslatedStreamFailedUsageDoesNotLeakToWinnerWithUsage(t *testing.T) {
	base, token, st := setupTranslatedFailover(t, "anth-bad", "oai-good", anthropicPreCommitUsageSSE, openaiWinnerUsageSSE, domain.ProtocolOpenAI)
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	logs := waitForLogs(t, st, 2)
	failed := findLogByProvider(t, logs, "anth-bad")
	if failed.InputTokens != 1234 || failed.OutputTokens != 0 {
		t.Errorf("failed row tokens = %d/%d, want 1234/0", failed.InputTokens, failed.OutputTokens)
	}
	winner := findLogByProvider(t, logs, "oai-good")
	if winner.InputTokens != 3 || winner.OutputTokens != 2 {
		t.Errorf("winner tokens = %d/%d, want 3/2", winner.InputTokens, winner.OutputTokens)
	}
}

func TestTranslatedStreamAllTargetsFailUsesSelectedUsage(t *testing.T) {
	base, token, st := setupTranslatedFailover(t, "anth-early", "anth-final", anthropicPreCommitUsageSSE, anthropicFinalFailUsageSSE, domain.ProtocolAnthropic)
	resp, body := postStream(t, base+"/v1/chat/completions", token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = %d, want failure; body=%s", resp.StatusCode, body)
	}
	logs := waitForLogs(t, st, 2)
	early := findLogByProvider(t, logs, "anth-early")
	if early.InputTokens != 1234 || early.OutputTokens != 0 {
		t.Errorf("early row tokens = %d/%d, want 1234/0", early.InputTokens, early.OutputTokens)
	}
	if early.ErrMsg == "" {
		t.Errorf("early row missing err: %+v", early)
	}
	final := findLogByProvider(t, logs, "anth-final")
	if final.InputTokens != 77 || final.OutputTokens != 0 {
		t.Errorf("final row tokens = %d/%d, want 77/0", final.InputTokens, final.OutputTokens)
	}
	if final.ErrMsg == "" {
		t.Errorf("final row missing err: %+v", final)
	}
}

func TestTranslatedStreamCommittedFailureKeepsAttemptUsage(t *testing.T) {
	var n2 int
	st := newTestStore(t)
	ctx := context.Background()
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicCommittedFailUsageSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiWinnerUsageSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	p1 := &domain.Provider{Name: "anth-partial", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolAnthropic}
	p2 := &domain.Provider{Name: "oai-unused", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
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

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n2 != 0 {
		t.Fatalf("second target called n2=%d", n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "partial") {
		t.Errorf("missing partial content: %s", body)
	}
	if !strings.Contains(body, "mid-stream boom") {
		t.Errorf("missing ingress error frame: %s", body)
	}
	l := waitForLogs(t, st, 1)[0]
	if l.Provider != "anth-partial" {
		t.Errorf("provider = %q, want anth-partial", l.Provider)
	}
	if l.InputTokens != 1234 || l.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 1234/2", l.InputTokens, l.OutputTokens)
	}
	if l.ErrMsg == "" || !strings.Contains(l.ErrMsg, "mid-stream boom") {
		t.Errorf("request log missing failure: %+v", l)
	}
}

func TestResponsesTranslatedNumericErrorFailover(t *testing.T) {
	const numericFailedSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_bad","model":"up","status":"in_progress"}}

event: response.failed
data: {"type":"response.failed","response":{"id":"resp_bad","status":"failed","error":{"code":429,"message":"numeric fail"},"output":[],"usage":null}}

`
	var n1, n2 int
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n1++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, numericFailedSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up1.Close)
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n2++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openaiSSE)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up2.Close)

	st := newTestStore(t)
	ctx := context.Background()
	p1 := &domain.Provider{Name: "r-bad", BaseURL: up1.URL, APIKey: "k", Protocol: domain.ProtocolOpenAIResponses}
	p2 := &domain.Provider{Name: "oai-good", BaseURL: up2.URL, APIKey: "k", Protocol: domain.ProtocolOpenAI}
	if err := st.CreateProvider(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProvider(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: []domain.ComboTarget{
		{ProviderID: p1.ID, UpstreamModel: "m1", Enabled: true},
		{ProviderID: p2.ID, UpstreamModel: "m2", Enabled: true},
	}}); err != nil {
		t.Fatal(err)
	}
	key, err := st.NewAccessKey(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	px := New(st, nil)
	px.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	resp, body := postStream(t, ts.URL+"/v1/chat/completions", key.Token,
		`{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("hits n1=%d n2=%d", n1, n2)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(body, "numeric fail") || strings.Contains(body, "resp_bad") {
		t.Fatalf("first target bytes leaked: %s", body)
	}
	if strings.Contains(body, `"error"`) {
		t.Fatalf("failed-target error leaked: %s", body)
	}
	text, finished := collectStreamText(t, "/v1/chat/completions", body)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if !finished {
		t.Error("stream not finished")
	}
	if got := backoffSkips(px, p1.ID); got == 0 {
		t.Fatal("failed provider was not penalized")
	}
}
