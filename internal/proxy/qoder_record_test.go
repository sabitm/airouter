package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/qoder"
	"airouter/internal/proxy/responses"
)

// qoderRecordFields are the identity and session fields shared by the Qoder
// plaintext envelope.
type qoderRecordFields struct {
	Chat    string `json:"chat_record_id"`
	Set     string `json:"request_set_id"`
	Session string `json:"session_id"`
}

// decodeQoderIngress runs one ingress body through its decoder and the Qoder
// backend encoder. It returns the identity fields and the encoded wire body.
func decodeQoderIngress(t *testing.T, decode func([]byte) (*ir.Request, error), body string) (qoderRecordFields, []byte) {
	t.Helper()
	req, err := decode([]byte(body))
	if err != nil {
		t.Fatalf("decode ingress: %v", err)
	}
	out, err := qoder.EncodeRequest(req)
	if err != nil {
		t.Fatalf("qoder encode: %v", err)
	}
	var f qoderRecordFields
	if err := json.Unmarshal(out, &f); err != nil {
		t.Fatalf("decode qoder body: %v", err)
	}
	return f, out
}

func assertQoderRecordFields(t *testing.T, f qoderRecordFields) {
	t.Helper()
	if f.Chat != f.Set {
		t.Fatalf("chat_record_id %q != request_set_id %q", f.Chat, f.Set)
	}
	if len(f.Chat) != 16 {
		t.Fatalf("record id %q is not 16 hex chars", f.Chat)
	}
	if f.Session == "" {
		t.Fatal("session_id is empty")
	}
}

// The fixtures below each carry one tool call and one tool result. They differ
// only in the tool-result text "sunny" -> "rainy" so a record-id change proves
// the tool round trip reached the Qoder encoder.
const openAIToolRoundTrip = `{
	"model":"default",
	"stream":false,
	"max_tokens":256,
	"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":"calling","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"paris\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"sunny"}
	],
	"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}}]
}`

const responsesToolRoundTrip = `{
	"model":"default",
	"stream":false,
	"max_output_tokens":256,
	"instructions":"be brief",
	"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"paris\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny"}
	],
	"tools":[{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}}]
}`

const anthropicToolRoundTrip = `{
	"model":"default",
	"stream":false,
	"max_tokens":256,
	"system":"be brief",
	"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"paris"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}
	],
	"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}]
}`

func TestIngressToQoderRecordIdentitySensitiveToToolContent(t *testing.T) {
	fixtures := []struct {
		name   string
		decode func([]byte) (*ir.Request, error)
		body   string
	}{
		{"openai chat", openai.DecodeRequest, openAIToolRoundTrip},
		{"openai responses", responses.DecodeRequest, responsesToolRoundTrip},
		{"anthropic messages", anthropic.DecodeRequest, anthropicToolRoundTrip},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			base, baseBody := decodeQoderIngress(t, tc.decode, tc.body)
			assertQoderRecordFields(t, base)
			if !strings.Contains(string(baseBody), "get_weather") || !strings.Contains(string(baseBody), "sunny") {
				t.Fatalf("ingress tool call/result missing from Qoder body: %s", baseBody)
			}

			changedBody := strings.Replace(tc.body, "sunny", "rainy", 1)
			if changedBody == tc.body {
				t.Fatal("fixture does not contain the tool-result text")
			}
			changed, changedOut := decodeQoderIngress(t, tc.decode, changedBody)
			assertQoderRecordFields(t, changed)
			if !strings.Contains(string(changedOut), "rainy") {
				t.Fatalf("changed tool result missing from Qoder body: %s", changedOut)
			}

			if base.Chat == changed.Chat || base.Set == changed.Set {
				t.Fatalf("record ids did not change with tool-result text: %q/%q", base.Chat, base.Set)
			}
			if base.Session != changed.Session {
				t.Fatalf("session scope changed with tool content: %q vs %q", base.Session, changed.Session)
			}
		})
	}
}

// Qoder's upstream request is always stream=true, so the ingress stream flag is
// request intent only and must not feed record identity.
func TestIngressToQoderRecordIdentityIndependentOfStreamFlag(t *testing.T) {
	fixtures := []struct {
		name   string
		decode func([]byte) (*ir.Request, error)
		body   string
	}{
		{"openai chat", openai.DecodeRequest, openAIToolRoundTrip},
		{"openai responses", responses.DecodeRequest, responsesToolRoundTrip},
		{"anthropic messages", anthropic.DecodeRequest, anthropicToolRoundTrip},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			unary, _ := decodeQoderIngress(t, tc.decode, tc.body)
			assertQoderRecordFields(t, unary)
			streaming, _ := decodeQoderIngress(t, tc.decode, strings.Replace(tc.body, `"stream":false`, `"stream":true`, 1))
			assertQoderRecordFields(t, streaming)
			if unary.Chat != streaming.Chat || unary.Set != streaming.Set {
				t.Fatalf("stream flag changed record ids: %q/%q vs %q/%q", unary.Chat, unary.Set, streaming.Chat, streaming.Set)
			}
			if unary.Session != streaming.Session {
				t.Fatalf("stream flag changed session scope: %q vs %q", unary.Session, streaming.Session)
			}
			// The identity stays content-sensitive under streaming too.
			streamedChanged := strings.Replace(strings.Replace(tc.body, `"stream":false`, `"stream":true`, 1), "sunny", "rainy", 1)
			changed, _ := decodeQoderIngress(t, tc.decode, streamedChanged)
			if changed.Chat == streaming.Chat {
				t.Fatalf("streaming record id ignored tool content: %q", changed.Chat)
			}
		})
	}
}

// Distinct tool-call fields flow through ingress decode and must each move the
// record ids. The mutations edit the decoded IR after a real ingress decode so
// the same Qoder encode path is exercised.
func TestIngressToQoderRecordIdentitySensitiveToToolCallFields(t *testing.T) {
	base, _ := decodeQoderIngress(t, openai.DecodeRequest, openAIToolRoundTrip)
	mutations := []struct {
		name   string
		mutate func(*ir.Request)
	}{
		{"tool call id", func(r *ir.Request) { r.Messages[1].Content[1].ToolID = "call_2" }},
		{"tool call name", func(r *ir.Request) { r.Messages[1].Content[1].ToolName = "get_temp" }},
		{"tool call arguments", func(r *ir.Request) { r.Messages[1].Content[1].ToolInput = json.RawMessage(`{"city":"rome"}`) }},
		{"tool result reference id", func(r *ir.Request) { r.Messages[2].Content[0].ToolUseID = "call_9" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			req, err := openai.DecodeRequest([]byte(openAIToolRoundTrip))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(req)
			out, err := qoder.EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			var f qoderRecordFields
			if err := json.Unmarshal(out, &f); err != nil {
				t.Fatal(err)
			}
			assertQoderRecordFields(t, f)
			if f.Chat == base.Chat || f.Set == base.Set {
				t.Fatalf("record ids unchanged after %s: %q/%q", tc.name, f.Chat, f.Set)
			}
			if f.Session != base.Session {
				t.Fatalf("session scope changed after %s: %q vs %q", tc.name, f.Session, base.Session)
			}
		})
	}
}
