package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"airouter/internal/domain"
	"airouter/internal/proxy/claudecode"
)

const finding6HTTPschema = `{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993},"f":{"const":1.2300},"e":{"const":1e400}},"required":["n"],"additionalProperties":false}`

type finding6WireFormat struct {
	Type        string          `json:"type"`
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict"`
}

func compactFinding6(t *testing.T, raw []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatalf("compact JSON: %v", err)
	}
	return out.String()
}

func sameFinding6JSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	decode := func(raw []byte) any {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode JSON: %v; raw=%s", err, raw)
		}
		return v
	}
	return reflect.DeepEqual(decode(a), decode(b))
}

func finding6ClaudeFormat(t *testing.T, body string) finding6WireFormat {
	t.Helper()
	var doc struct {
		OutputConfig struct {
			Format finding6WireFormat `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode Claude body: %v; body=%s", err, body)
	}
	return doc.OutputConfig.Format
}

// finding6FormatMembers returns the raw output_config.format members so key
// presence is checked against the literal wire bytes (a *bool decode collapses
// explicit null into nil).
func finding6FormatMembers(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		OutputConfig struct {
			Format map[string]json.RawMessage `json:"format"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode Claude body: %v; body=%s", err, body)
	}
	return doc.OutputConfig.Format
}

// finding6AssertClaudeFormat enforces the approved Claude mapping: type+schema
// only, exact schema bytes, and never name/description/strict — including an
// explicit null (which must not be re-encoded as a wire member).
func finding6AssertClaudeFormat(t *testing.T, wireBody, schema string) {
	t.Helper()
	members := finding6FormatMembers(t, wireBody)
	if string(members["type"]) != `"json_schema"` {
		t.Fatalf("output_config.format type = %s; body=%s", members["type"], wireBody)
	}
	if !sameFinding6JSON(t, members["schema"], []byte(schema)) {
		t.Fatalf("schema changed: %s", members["schema"])
	}
	for _, banned := range []string{"name", "description", "strict"} {
		if v, ok := members[banned]; ok {
			t.Fatalf("Claude wire gained %s member %s: %s", banned, v, wireBody)
		}
	}
}

func TestFinding6StructuredOutputClaudeHTTP(t *testing.T) {
	schema := finding6HTTPschema
	cases := []struct {
		name   string
		path   string
		body   string
		stream bool
		text   string
	}{
		{
			name: "chat-unary", path: "/v1/chat/completions", text: "hello from anthropic",
			body: `{"model":"default","max_tokens":32,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":true,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-stream", path: "/v1/chat/completions", stream: true, text: "answer",
			body: `{"model":"default","max_tokens":32,"stream":true,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":true,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-unary", path: "/v1/responses", text: "hello from anthropic",
			body: `{"model":"default","max_output_tokens":32,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":true,"schema":` + schema + `}},"input":"hi"}`,
		},
		{
			name: "responses-stream", path: "/v1/responses", stream: true, text: "answer",
			body: `{"model":"default","max_output_tokens":32,"stream":true,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":true,"schema":` + schema + `}},"input":"hi"}`,
		},
		{
			name: "chat-unary-null", path: "/v1/chat/completions", text: "hello from anthropic",
			body: `{"model":"default","max_tokens":32,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":null,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-stream-null", path: "/v1/chat/completions", stream: true, text: "answer",
			body: `{"model":"default","max_tokens":32,"stream":true,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":null,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-unary-null", path: "/v1/responses", text: "hello from anthropic",
			body: `{"model":"default","max_output_tokens":32,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":null,"schema":` + schema + `}},"input":"hi"}`,
		},
		{
			name: "responses-stream-null", path: "/v1/responses", stream: true, text: "answer",
			body: `{"model":"default","max_output_tokens":32,"stream":true,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":null,"schema":` + schema + `}},"input":"hi"}`,
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
				model:    "claude-sonnet-4-6",
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
			finding6AssertClaudeFormat(t, wire.body, schema)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaStructured, 1)
			assertBetaFlagCount(t, wire.beta, claudecode.BetaClaudeCode, 1)
			assertSelectorUsage(t, tc.path, tc.stream, string(out))
		})
	}
}

func TestFinding6StructuredOutputKeepsThinkingControls(t *testing.T) {
	tr := installSelectorTransport(t)
	captures := newSelectorCapture(2)
	upstream := finding4Upstream(t, captures, false)
	base, token, _ := newSelectorProxy(t, tr, selectorTarget{
		protocol: domain.ProtocolClaudeCode,
		baseURL:  upstream.URL,
		model:    "claude-sonnet-4-6",
	})
	body := `{"model":"default","max_tokens":32,"reasoning_effort":"high","response_format":{"type":"json_schema","json_schema":{"name":"Out","schema":` + finding6HTTPschema + `}},"messages":[{"role":"user","content":"hi"}]}`
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	wire := captures.next(t)
	captures.assertNoMore(t)
	if wire.thinking != "adaptive" || wire.effort != "high" {
		t.Fatalf("thinking = %q effort = %q; body=%s", wire.thinking, wire.effort, wire.body)
	}
	format := finding6ClaudeFormat(t, wire.body)
	if format.Type != "json_schema" || !sameFinding6JSON(t, format.Schema, []byte(finding6HTTPschema)) {
		t.Fatalf("format changed with thinking controls: %+v; body=%s", format, wire.body)
	}
	assertBetaFlagCount(t, wire.beta, claudecode.BetaStructured, 1)
	assertBetaFlagCount(t, wire.beta, claudecode.BetaEffort, 1)
}

func TestFinding6UnsupportedFormatRejectedBeforeClaudeHTTP(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		body      string
		responses bool
	}{
		{
			name: "chat-json-object", path: "/v1/chat/completions",
			body: `{"model":"default","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-strict-false", path: "/v1/chat/completions",
			body: `{"model":"default","response_format":{"type":"json_schema","json_schema":{"name":"Out","strict":false,"schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-unknown", path: "/v1/chat/completions",
			body: `{"model":"default","response_format":{"type":"regex","pattern":"x"},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-malformed", path: "/v1/chat/completions",
			body: `{"model":"default","response_format":{"type":"json_schema","json_schema":{"name":"Out","schema":null}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-json-object", path: "/v1/responses", responses: true,
			body: `{"model":"default","text":{"format":{"type":"json_object"}},"input":"hi"}`,
		},
		{
			name: "responses-strict-false", path: "/v1/responses", responses: true,
			body: `{"model":"default","text":{"format":{"type":"json_schema","name":"Out","strict":false,"schema":{"type":"object"}}},"input":"hi"}`,
		},
		{
			name: "responses-unknown", path: "/v1/responses", responses: true,
			body: `{"model":"default","text":{"format":{"type":"regex"}},"input":"hi"}`,
		},
		{
			name: "responses-malformed", path: "/v1/responses", responses: true,
			body: `{"model":"default","text":{"format":{"type":"json_schema","name":"Out","schema":[]}},"input":"hi"}`,
		},
		{
			name: "chat-strict-string", path: "/v1/chat/completions",
			body: `{"model":"default","response_format":{"type":"json_schema","json_schema":{"name":"Out","strict":"yes","schema":{"type":"object"}}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-strict-number", path: "/v1/responses", responses: true,
			body: `{"model":"default","text":{"format":{"type":"json_schema","name":"Out","strict":1,"schema":{"type":"object"}}},"input":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicUpstreamBody)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolClaudeCode,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
			})
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d want 400; body=%s", resp.StatusCode, out)
			}
			if hits.Load() != 0 {
				t.Fatalf("upstream received %d requests for unsupported format", hits.Load())
			}
			var doc struct {
				Error struct {
					Message string          `json:"message"`
					Type    string          `json:"type"`
					Code    json.RawMessage `json:"code"`
					Param   json.RawMessage `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("error envelope: %v; body=%s", err, out)
			}
			if doc.Error.Type != "invalid_request_error" || doc.Error.Message == "" {
				t.Fatalf("wrong ingress error envelope: %s", out)
			}
			if tc.responses && (doc.Error.Code == nil || doc.Error.Param == nil) {
				t.Fatalf("Responses error envelope missing code/param: %s", out)
			}
			if !tc.responses && doc.Error.Code == nil {
				t.Fatalf("Chat error envelope missing code: %s", out)
			}
		})
	}
}

type finding6RawCapture struct {
	ch chan []byte
}

func newFinding6RawCapture() *finding6RawCapture {
	return &finding6RawCapture{ch: make(chan []byte, 2)}
}

func (c *finding6RawCapture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	select {
	case c.ch <- body:
	default:
	}
}

func (c *finding6RawCapture) next(t *testing.T) []byte {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case body := <-c.ch:
		return body
	case <-timer.C:
		t.Fatal("timed out waiting for upstream body")
	case <-t.Context().Done():
		t.Fatalf("test context ended waiting for upstream body: %v", t.Context().Err())
	}
	return nil
}

func TestFinding6UnsupportedTargetFilteredThenFormatRetained(t *testing.T) {
	tr := installSelectorTransport(t)
	var firstHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(first.Close)
	captures := newFinding6RawCapture()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responsesUpstreamBody)
	}))
	t.Cleanup(second.Close)
	base, token, _ := newSelectorProxy(t, tr,
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: first.URL, model: "claude"},
		selectorTarget{protocol: domain.ProtocolOpenAIResponses, baseURL: second.URL, model: "gpt"},
	)
	body := `{"model":"default","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if firstHits.Load() != 0 {
		t.Fatalf("incompatible target received %d requests", firstHits.Load())
	}
	upstreamBody := captures.next(t)
	var doc struct {
		Text struct {
			Format struct {
				Type string `json:"type"`
			} `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(upstreamBody, &doc); err != nil {
		t.Fatalf("second upstream body: %v; body=%s", err, upstreamBody)
	}
	if doc.Text.Format.Type != "json_object" {
		t.Fatalf("format dropped on failover target: %s", upstreamBody)
	}
	if !strings.Contains(string(out), "hello from responses") {
		t.Fatalf("second target response lost: %s", out)
	}
}

func TestFinding6SchemaRetainedAcrossClaudeFailover(t *testing.T) {
	schema := finding6HTTPschema
	tr := installSelectorTransport(t)
	captures := newSelectorCapture(2)
	// Primary: valid Claude target that fails with 503 before any byte is
	// committed, so the attempt is retryable and failover advances.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"primary upstream exploded"}}`)
	}))
	t.Cleanup(first.Close)
	// Backup: healthy Claude target answering the unary fixture.
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures.record(captureClaudeSelector(r))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicUpstreamBody)
	}))
	t.Cleanup(second.Close)
	base, token, _ := newSelectorProxy(t, tr,
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: first.URL, model: "claude-sonnet-4-6"},
		selectorTarget{protocol: domain.ProtocolAnthropic, baseURL: second.URL, model: "claude-sonnet-4-6"},
	)
	body := `{"model":"default","max_tokens":32,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":null,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`
	resp, out := postSelector(t, selectorClient(tr), base+"/v1/chat/completions", token, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "hello from anthropic") {
		t.Fatalf("winner response lost content: %s", out)
	}
	if strings.Contains(string(out), "primary upstream exploded") || strings.Contains(strings.ToLower(string(out)), `"error"`) {
		t.Fatalf("failed attempt leaked into final response: %s", out)
	}
	// Both upstreams must have been contacted, each with a fresh IR encoding of
	// the format intent: the schema survives every attempt, strict stays off the
	// wire (explicit null is unspecified on the Claude mapping).
	wire1 := captures.next(t)
	wire2 := captures.next(t)
	captures.assertNoMore(t)
	finding6AssertClaudeFormat(t, wire1.body, schema)
	finding6AssertClaudeFormat(t, wire2.body, schema)
	// The winner owns the final response: its usage counts, no error envelope.
	assertSelectorUsage(t, "/v1/chat/completions", false, string(out))
	var doc struct {
		Model string          `json:"model"`
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("final response: %v; body=%s", err, out)
	}
	if doc.Model == "" {
		t.Fatalf("final response has no winner model: %s", out)
	}
	if doc.Error != nil || doc.Usage == nil {
		t.Fatalf("winner response must carry usage and no error: %s", out)
	}
}

func TestFinding6NativePassthroughPreservesUnknownFormat(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		protocol domain.Protocol
		body     string
		upstream string
		check    func(t *testing.T, body []byte)
	}{
		{
			name: "chat", path: "/v1/chat/completions", protocol: domain.ProtocolOpenAI,
			body:     `{"model":"default","response_format":{"type":"future_native","shape":{"n":9007199254740993}},"future_field":{"keep":1},"messages":[{"role":"user","content":"hi"}]}`,
			upstream: openaiUpstreamBody,
			check: func(t *testing.T, body []byte) {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(body, &doc); err != nil {
					t.Fatal(err)
				}
				if compactFinding6(t, doc["response_format"]) != compactFinding6(t, []byte(`{"type":"future_native","shape":{"n":9007199254740993}}`)) {
					t.Fatalf("response_format changed: %s", doc["response_format"])
				}
				if compactFinding6(t, doc["future_field"]) != `{"keep":1}` {
					t.Fatalf("future field changed: %s", doc["future_field"])
				}
			},
		},
		{
			name: "responses", path: "/v1/responses", protocol: domain.ProtocolOpenAIResponses,
			body:     `{"model":"default","text":{"format":{"type":"future_native","x":1},"verbosity":"low"},"future_field":{"keep":1},"input":"hi"}`,
			upstream: responsesUpstreamBody,
			check: func(t *testing.T, body []byte) {
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(body, &doc); err != nil {
					t.Fatal(err)
				}
				if compactFinding6(t, doc["text"]) != compactFinding6(t, []byte(`{"format":{"type":"future_native","x":1},"verbosity":"low"}`)) {
					t.Fatalf("text changed: %s", doc["text"])
				}
				if compactFinding6(t, doc["future_field"]) != `{"keep":1}` {
					t.Fatalf("future field changed: %s", doc["future_field"])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newFinding6RawCapture()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captures.record(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.upstream)
			}))
			t.Cleanup(upstream.Close)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{protocol: tc.protocol, baseURL: upstream.URL, model: "upstream"})
			resp, out := postSelector(t, selectorClient(tr), base+tc.path, token, tc.body, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, out)
			}
			body := captures.next(t)
			var doc struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("upstream body: %v; body=%s", err, body)
			}
			if doc.Model != "upstream" {
				t.Fatalf("model = %q; body=%s", doc.Model, body)
			}
			tc.check(t, body)
		})
	}
}

func TestFinding6StructuredOutputGenericAnthropicHTTP(t *testing.T) {
	schema := finding6HTTPschema
	cases := []struct {
		name   string
		path   string
		body   string
		stream bool
		text   string
	}{
		{
			name: "chat-unary", path: "/v1/chat/completions", text: "hello from anthropic",
			body: `{"model":"default","max_tokens":32,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":true,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-stream", path: "/v1/chat/completions", stream: true, text: "answer",
			body: `{"model":"default","max_tokens":32,"stream":true,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":true,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-unary", path: "/v1/responses", text: "hello from anthropic",
			body: `{"model":"default","max_output_tokens":32,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":true,"schema":` + schema + `}},"input":"hi"}`,
		},
		{
			name: "responses-stream", path: "/v1/responses", stream: true, text: "answer",
			body: `{"model":"default","max_output_tokens":32,"stream":true,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":true,"schema":` + schema + `}},"input":"hi"}`,
		},
		{
			name: "chat-unary-null", path: "/v1/chat/completions", text: "hello from anthropic",
			body: `{"model":"default","max_tokens":32,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":null,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat-stream-null", path: "/v1/chat/completions", stream: true, text: "answer",
			body: `{"model":"default","max_tokens":32,"stream":true,"response_format":{"type":"json_schema","json_schema":{"name":"Out","description":"result","strict":null,"schema":` + schema + `}},"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses-unary-null", path: "/v1/responses", text: "hello from anthropic",
			body: `{"model":"default","max_output_tokens":32,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":null,"schema":` + schema + `}},"input":"hi"}`,
		},
		{
			name: "responses-stream-null", path: "/v1/responses", stream: true, text: "answer",
			body: `{"model":"default","max_output_tokens":32,"stream":true,"text":{"format":{"type":"json_schema","name":"Out","description":"result","strict":null,"schema":` + schema + `}},"input":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := installSelectorTransport(t)
			captures := newSelectorCapture(2)
			upstream := finding4Upstream(t, captures, tc.stream)
			base, token, _ := newSelectorProxy(t, tr, selectorTarget{
				protocol: domain.ProtocolAnthropic,
				baseURL:  upstream.URL,
				model:    "claude-sonnet-4-6",
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
			finding6AssertClaudeFormat(t, wire.body, schema)
			assertSelectorUsage(t, tc.path, tc.stream, string(out))
		})
	}
}
