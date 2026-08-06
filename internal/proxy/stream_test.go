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
