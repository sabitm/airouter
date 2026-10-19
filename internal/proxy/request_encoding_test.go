package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"airouter/internal/domain"
)

// recordingUpstream counts contacts and retains the last request body.
type recordingUpstream struct {
	mu   sync.Mutex
	hits int32
	body []byte
}

func (r *recordingUpstream) count() int32 { return atomic.LoadInt32(&r.hits) }

func (r *recordingUpstream) lastBody() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.body...)
}

func newRecordingUpstream(t *testing.T, protocol domain.Protocol, rec *recordingUpstream) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rec.hits, 1)
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.body = b
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch protocol {
		case domain.ProtocolAnthropic, domain.ProtocolClaudeCode:
			_, _ = io.WriteString(w, anthropicUpstreamBody)
		default:
			_, _ = io.WriteString(w, openaiUpstreamBody)
		}
	}))
}

// encodingRig is a proxy with n provider targets for one backend protocol.
type encodingRig struct {
	base      string
	token     string
	upstreams []*recordingUpstream
}

func newEncodingRig(t *testing.T, backend domain.Protocol, upstreamModel string, n int) *encodingRig {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	rig := &encodingRig{}
	targets := make([]domain.ComboTarget, 0, n)
	for i := 0; i < n; i++ {
		rec := &recordingUpstream{}
		upstream := newRecordingUpstream(t, backend, rec)
		t.Cleanup(upstream.Close)
		prov := &domain.Provider{Name: fmt.Sprintf("p%d", i), BaseURL: upstream.URL, APIKey: "up-key", Protocol: backend}
		if err := st.CreateProvider(ctx, prov); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, domain.ComboTarget{ProviderID: prov.ID, UpstreamModel: upstreamModel, Enabled: true})
		rig.upstreams = append(rig.upstreams, rec)
	}
	if err := st.CreateCombo(ctx, &domain.Combo{Name: "default", Strategy: domain.StrategyFailover, Targets: targets}); err != nil {
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
	rig.base = ts.URL
	rig.token = key.Token
	return rig
}

func (r *encodingRig) contacts() int32 {
	var total int32
	for _, u := range r.upstreams {
		total += u.count()
	}
	return total
}

// malformedArgs is non-empty invalid JSON, used inside a JSON string field.
const malformedArgs = `{"a":1,}`

func openAIMalformedBody(stream bool) string {
	streamField := ""
	if stream {
		streamField = `"stream":true,`
	}
	return fmt.Sprintf(`{"model":"default",%s"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"sure","tool_calls":[{"id":"call_secret","type":"function","function":{"name":"tool_secret","arguments":"{\"a\":1,}"}}]},
		{"role":"tool","tool_call_id":"call_secret","content":"result"}
	]}`, streamField)
}

func responsesMalformedBody(stream bool) string {
	streamField := ""
	if stream {
		streamField = `"stream":true,`
	}
	return fmt.Sprintf(`{"model":"default",%s"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"call_secret","name":"tool_secret","arguments":"{\"a\":1,}"},
		{"type":"function_call_output","call_id":"call_secret","output":"result"}
	]}`, streamField)
}

// TestTranslatedMalformedToolArgsRejected verifies malformed tool arguments are
// a local HTTP 400 in the ingress envelope with zero upstream contact, for both
// Anthropic backend entrypoints and both ingress formats, unary and streaming.
func TestTranslatedMalformedToolArgsRejected(t *testing.T) {
	cases := []struct {
		name    string
		backend domain.Protocol
		ingress string
		body    string
	}{
		{"openai to anthropic unary", domain.ProtocolAnthropic, "/v1/chat/completions", openAIMalformedBody(false)},
		{"openai to anthropic streaming", domain.ProtocolAnthropic, "/v1/chat/completions", openAIMalformedBody(true)},
		{"responses to anthropic unary", domain.ProtocolAnthropic, "/v1/responses", responsesMalformedBody(false)},
		{"responses to anthropic streaming", domain.ProtocolAnthropic, "/v1/responses", responsesMalformedBody(true)},
		{"openai to claude-code unary", domain.ProtocolClaudeCode, "/v1/chat/completions", openAIMalformedBody(false)},
		{"openai to claude-code streaming", domain.ProtocolClaudeCode, "/v1/chat/completions", openAIMalformedBody(true)},
		{"responses to claude-code unary", domain.ProtocolClaudeCode, "/v1/responses", responsesMalformedBody(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEncodingRig(t, tc.backend, "real-model", 2)
			resp, out := post(t, rig.base+tc.ingress, rig.token, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", resp.StatusCode, out)
			}
			assertIngressErrorEnvelope(t, tc.ingress, out, "invalid_request_error")
			var envelope struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &envelope); err != nil {
				t.Fatalf("error envelope invalid: %v (%s)", err, out)
			}
			if !strings.Contains(envelope.Error.Message, "message ") || !strings.Contains(envelope.Error.Message, "block ") {
				t.Fatalf("error message %q does not identify message and block position", envelope.Error.Message)
			}
			if got := rig.contacts(); got != 0 {
				t.Fatalf("upstream contacts = %d, want 0 (validation must not fail over)", got)
			}
			body := string(out)
			for _, leak := range []string{"call_secret", "tool_secret", malformedArgs} {
				if strings.Contains(body, leak) {
					t.Fatalf("error envelope leaked %q: %s", leak, body)
				}
			}
		})
	}
}

// TestTranslatedValidHistoryPreserved verifies a valid assistant turn (text,
// reasoning, several tool calls) and its matching tool results survive
// translation intact, including the empty-arguments default.
func TestTranslatedValidHistoryPreserved(t *testing.T) {
	rig := newEncodingRig(t, domain.ProtocolAnthropic, "real-model", 1)
	body := `{"model":"default","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","reasoning_content":"think","content":"sure","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}},
			{"id":"c2","type":"function","function":{"name":"f2","arguments":""}},
			{"id":"c3","type":"function","function":{"name":"f3","arguments":"[1,2,3]"}}
		]},
		{"role":"tool","tool_call_id":"c1","content":"r1"},
		{"role":"tool","tool_call_id":"c2","content":"r2"},
		{"role":"tool","tool_call_id":"c3","content":"r3"}
	]}`
	resp, out := post(t, rig.base+"/v1/chat/completions", rig.token, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	upstream := rig.upstreams[0].lastBody()
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				Thinking  *string         `json:"thinking"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(upstream, &got); err != nil {
		t.Fatalf("upstream body invalid: %v (%s)", err, upstream)
	}
	if len(got.Messages) < 2 || got.Messages[1].Role != "assistant" {
		t.Fatalf("assistant turn missing: %s", upstream)
	}
	blocks := got.Messages[1].Content
	if len(blocks) != 5 {
		t.Fatalf("assistant blocks = %d, want 5 (%s)", len(blocks), upstream)
	}
	if blocks[0].Type != "thinking" || blocks[0].Thinking == nil || *blocks[0].Thinking != "think" {
		t.Fatalf("reasoning = %+v", blocks[0])
	}
	if blocks[1].Type != "text" || blocks[1].Text != "sure" {
		t.Fatalf("text = %+v", blocks[1])
	}
	wantTool := []struct {
		id, name, input string
	}{
		{"c1", "f1", `{"a":1}`},
		{"c2", "f2", `{}`},
		{"c3", "f3", `[1,2,3]`},
	}
	for i, want := range wantTool {
		b := blocks[2+i]
		if b.Type != "tool_use" || b.ID != want.id || b.Name != want.name || string(b.Input) != want.input {
			t.Fatalf("tool %d = %+v, want id=%s name=%s input=%s", i, b, want.id, want.name, want.input)
		}
	}
	var results []string
	for _, b := range got.Messages {
		for _, blk := range b.Content {
			if blk.Type == "tool_result" {
				results = append(results, blk.ToolUseID)
			}
		}
	}
	if strings.Join(results, ",") != "c1,c2,c3" {
		t.Fatalf("tool results = %v, want c1,c2,c3", results)
	}
}

// TestPassthroughPreservesMalformedToolArgs verifies a same-codec-id OpenAI
// request forwards its malformed argument string unchanged (only the model is
// rewritten); validation must not touch the passthrough path.
func TestPassthroughPreservesMalformedToolArgs(t *testing.T) {
	rig := newEncodingRig(t, domain.ProtocolOpenAI, "real-model", 1)
	resp, out := post(t, rig.base+"/v1/chat/completions", rig.token, openAIMalformedBody(false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
	}
	upstream := rig.upstreams[0].lastBody()
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(upstream, &got); err != nil {
		t.Fatalf("upstream body invalid: %v (%s)", err, upstream)
	}
	if got.Model != "real-model" {
		t.Fatalf("model = %q, want real-model", got.Model)
	}
	if len(got.Messages) != 3 || len(got.Messages[1].ToolCalls) != 1 {
		t.Fatalf("messages changed: %s", upstream)
	}
	if args := got.Messages[1].ToolCalls[0].Function.Arguments; args != malformedArgs {
		t.Fatalf("arguments = %q, want %q preserved", args, malformedArgs)
	}
}

// TestUnexpectedEncodeErrorStaysInternal verifies a backend.encodeRequest
// failure that is not a client validation error is a generic HTTP 500 with the
// internal detail withheld, unary and streaming. Cursor encode rejects an empty
// model, which the empty upstream model produces.
func TestUnexpectedEncodeErrorStaysInternal(t *testing.T) {
	cases := []struct {
		name   string
		stream bool
	}{
		{"unary", false},
		{"streaming", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEncodingRig(t, domain.ProtocolCursor, "", 1)
			body := `{"model":"default","messages":[{"role":"user","content":"hi"}]}`
			if tc.stream {
				body = `{"model":"default","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			resp, out := post(t, rig.base+"/v1/chat/completions", rig.token, body)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body = %s", resp.StatusCode, out)
			}
			assertIngressErrorEnvelope(t, "/v1/chat/completions", out, "api_error")
			var envelope struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &envelope); err != nil {
				t.Fatalf("error envelope invalid: %v (%s)", err, out)
			}
			if envelope.Error.Message != "failed to encode upstream request" {
				t.Fatalf("error.message = %q, want generic encode failure", envelope.Error.Message)
			}
			if strings.Contains(string(out), "empty model") {
				t.Fatalf("internal detail leaked: %s", out)
			}
			if got := rig.contacts(); got != 0 {
				t.Fatalf("upstream contacts = %d, want 0", got)
			}
		})
	}
}
