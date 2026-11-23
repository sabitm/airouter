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
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
)

func TestCollectChatStreamCreatedAt(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"grok-4.7\",\"created\":1710000111,\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"created\":0,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	resp, err := collectStreamResponse(strings.NewReader(body), openaiCodec, nil, "fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.CreatedAt != 1710000111 || resp.ID != "chatcmpl-1" || resp.Model != "grok-4.7" {
		t.Fatalf("collected = %+v", resp)
	}
	out, err := responses.EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := numericCreatedAt(t, out); got != 1710000111 {
		t.Fatalf("encoded created_at = %d body=%s", got, out)
	}
}

func TestCollectStreamResponseCreatedAtIgnoresLaterValues(t *testing.T) {
	backend := openaiCodec
	backend.decodeStream = func(_ io.Reader, emit func(ir.StreamEvent) error) error {
		if err := emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "a", CreatedAt: 1710000111}); err != nil {
			return err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EventMessageStart, CreatedAt: 0}); err != nil {
			return err
		}
		if err := emit(ir.StreamEvent{Kind: ir.EventMessageStart, CreatedAt: 42}); err != nil {
			return err
		}
		return emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: "ok"})
	}
	resp, err := collectStreamResponse(strings.NewReader(""), backend, nil, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.CreatedAt != 1710000111 {
		t.Fatalf("collected created_at = %d, want 1710000111", resp.CreatedAt)
	}
}

func TestChatToResponsesUnaryCreatedAt(t *testing.T) {
	const withCreated = `{"id":"chatcmpl-u","object":"chat.completion","created":1710000333,"model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	base, token := setupCreatedAtProxy(t, domain.ProtocolOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, withCreated)
	})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, out)
	}
	if got := numericCreatedAt(t, out); got != 1710000333 {
		t.Fatalf("created_at = %d body=%s", got, out)
	}

	before := time.Now().Unix()
	base, token = setupCreatedAtProxy(t, domain.ProtocolOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-z","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	})
	resp, out = post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	after := time.Now().Unix()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, out)
	}
	got := numericCreatedAt(t, out)
	if got < before || got > after {
		t.Fatalf("fallback created_at = %d, want %d..%d body=%s", got, before, after, out)
	}
}

func TestChatToResponsesStreamCreatedAtToolRoundTrip(t *testing.T) {
	const upstreamToolSSE = `data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1710000111,"model":"grok-4.7","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-9","model":"grok-4.7","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"created":"bad","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"created":0,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"paris\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}

data: [DONE]

`
	const upstreamTextSSE = `data: {"id":"chatcmpl-2","model":"grok-4.7","choices":[{"index":0,"delta":{"content":"thanks"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var bodies [][]byte
	var paths []string
	base, token := setupCreatedAtProxy(t, domain.ProtocolOpenAI, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, upstreamToolSSE)
		} else {
			_, _ = io.WriteString(w, upstreamTextSSE)
		}
		w.(http.Flusher).Flush()
	})

	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"weather?"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	times := responseEnvelopeTimes(t, body)
	if times["response.created"] != 1710000111 || times["response.completed"] != 1710000111 {
		t.Fatalf("timestamps = %v body=%s", times, body)
	}
	name, args, done := collectResponsesToolStream(t, body)
	if name != "get_weather" || args != `{"city":"paris"}` || done != `{"city":"paris"}` {
		t.Fatalf("tool name=%q args=%q done=%q body=%s", name, args, done, body)
	}
	if got := responsesFunctionCallID(t, body); got != "call_9" {
		t.Fatalf("call_id = %q body=%s", got, body)
	}

	followUp := `{"model":"default","stream":true,"input":[{"type":"message","role":"user","content":"weather?"},{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"paris\"}"},{"type":"function_call_output","call_id":"call_9","output":"sunny"}]}`
	resp, body = postStream(t, base+"/v1/responses", token, followUp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d body=%s", resp.StatusCode, body)
	}
	if len(bodies) != 2 || paths[0] != "/chat/completions" || paths[1] != "/chat/completions" {
		t.Fatalf("upstream calls = %v %d", paths, len(bodies))
	}
	second := string(bodies[1])
	if !strings.Contains(second, `"tool_call_id":"call_9"`) || !strings.Contains(second, `"id":"call_9"`) || !strings.Contains(second, "sunny") {
		t.Fatalf("tool result was not forwarded: %s", second)
	}
	if !strings.Contains(second, `"model":"real-model"`) {
		t.Fatalf("upstream model = %s", second)
	}
}

func TestChatToResponsesStreamFallbackCreatedAt(t *testing.T) {
	const upstream = `data: {"id":"chatcmpl-1","model":"up","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	before := time.Now().Unix()
	base, token := setupCreatedAtProxy(t, domain.ProtocolOpenAI, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstream)
		w.(http.Flusher).Flush()
	})
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	after := time.Now().Unix()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	times := responseEnvelopeTimes(t, body)
	created := times["response.created"]
	if created < before || created > after || times["response.completed"] != created {
		t.Fatalf("timestamps = %v, want one clock value in %d..%d body=%s", times, before, after, body)
	}
}

func TestResponsesPassthroughPreservesCreatedAt(t *testing.T) {
	const unary = `{"id":"resp_native","object":"response","created_at":1710000222,"status":"completed","model":"up","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"native","annotations":[]}],"vendor_extra":"keep"}]}`
	var gotReq []byte
	base, token := setupCreatedAtProxy(t, domain.ProtocolOpenAIResponses, func(w http.ResponseWriter, r *http.Request) {
		gotReq, _ = io.ReadAll(r.Body)
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, unary)
	})
	resp, out := post(t, base+"/v1/responses", token, `{"model":"default","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, out)
	}
	if string(out) != unary {
		t.Fatalf("passthrough changed unary body:\n got %s\nwant %s", out, unary)
	}
	if !strings.Contains(string(gotReq), `"model":"real-model"`) {
		t.Fatalf("request model was not rewritten: %s", gotReq)
	}

	const streamBody = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"created_at\":1710000222,\"status\":\"in_progress\",\"vendor_extra\":\"keep\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"native\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_native\",\"object\":\"response\",\"created_at\":1710000222,\"status\":\"completed\",\"output\":[],\"vendor_extra\":\"keep\"}}\n\n"
	base, token = setupCreatedAtProxy(t, domain.ProtocolOpenAIResponses, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streamBody)
		w.(http.Flusher).Flush()
	})
	resp, body := postStream(t, base+"/v1/responses", token, `{"model":"default","stream":true,"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if body != streamBody {
		t.Fatalf("passthrough changed stream bytes:\n got %q\nwant %q", body, streamBody)
	}
}

func setupCreatedAtProxy(t *testing.T, protocol domain.Protocol, handler http.HandlerFunc) (string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	prov := &domain.Provider{Name: "p", BaseURL: upstream.URL, APIKey: "up-key", Protocol: protocol}
	if err := st.CreateProvider(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{
		Name:     "default",
		Strategy: domain.StrategyFailover,
		Targets:  []domain.ComboTarget{{ProviderID: prov.ID, UpstreamModel: "real-model", Enabled: true}},
	}); err != nil {
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

func responseEnvelopeTimes(t *testing.T, body string) map[string]int64 {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	got := map[string]int64{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Name {
		case "response.created", "response.completed", "response.incomplete":
			n := numericCreatedAt(t, ev.Data)
			if prev, ok := got[ev.Name]; ok && prev != n {
				t.Fatalf("%s created_at changed from %d to %d", ev.Name, prev, n)
			}
			got[ev.Name] = n
		}
	}
	if got["response.created"] == 0 || (got["response.completed"] == 0 && got["response.incomplete"] == 0) {
		t.Fatalf("missing terminal response envelope: %v body=%s", got, body)
	}
	want := got["response.created"]
	for name, n := range got {
		if n != want {
			t.Fatalf("%s created_at = %d, want %d", name, n, want)
		}
	}
	return got
}

func numericCreatedAt(t *testing.T, data []byte) int64 {
	t.Helper()
	var env struct {
		CreatedAt json.RawMessage `json:"created_at"`
		Response  struct {
			CreatedAt json.RawMessage `json:"created_at"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("json: %v body=%s", err, data)
	}
	raw := env.Response.CreatedAt
	if len(bytesTrimSpace(raw)) == 0 {
		raw = env.CreatedAt
	}
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || raw[0] == '"' {
		t.Fatalf("created_at must be a number, got %s in %s", raw, data)
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			t.Fatalf("created_at is not a plain integer: %s", raw)
		}
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n <= 0 {
		t.Fatalf("created_at %s: %v", raw, err)
	}
	return n
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func responsesFunctionCallID(t *testing.T, body string) string {
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
		var raw map[string]any
		if json.Unmarshal(ev.Data, &raw) != nil {
			continue
		}
		if item, ok := raw["item"].(map[string]any); ok {
			if id, _ := item["call_id"].(string); id != "" {
				return id
			}
		}
		resp, _ := raw["response"].(map[string]any)
		output, _ := resp["output"].([]any)
		for _, el := range output {
			item, _ := el.(map[string]any)
			if id, _ := item["call_id"].(string); id != "" {
				return id
			}
		}
	}
	t.Fatal("missing function call id")
	return ""
}
