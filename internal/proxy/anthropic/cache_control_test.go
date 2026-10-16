package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// wireCacheMarker is the marker subset under test. The production decoder never
// reinterprets unknown keys, so tests assert exactly Type and TTL.
type wireCacheMarker struct {
	Type string `json:"type"`
	TTL  string `json:"ttl"`
}

type wireToolOut struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type wireBlockOut struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Thinking     *string         `json:"thinking"`
	Signature    string          `json:"signature"`
	Data         string          `json:"data"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	Title        string          `json:"title"`
	CacheControl json.RawMessage `json:"cache_control"`
}

type wireMessageOut struct {
	Role    string         `json:"role"`
	Content []wireBlockOut `json:"content"`
}

type wireRequestOut struct {
	Tools    []wireToolOut    `json:"tools"`
	Messages []wireMessageOut `json:"messages"`
	System   json.RawMessage  `json:"system"`
}

func decodeWireOut(t *testing.T, body []byte) wireRequestOut {
	t.Helper()
	var out wireRequestOut
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("encoded body invalid: %v (%s)", err, body)
	}
	return out
}

func requireMarker(t *testing.T, path string, raw json.RawMessage, typ, ttl string) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("%s: cache_control missing", path)
	}
	var m wireCacheMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: cache_control invalid: %v (%s)", path, err, raw)
	}
	if m.Type != typ || m.TTL != ttl {
		t.Fatalf("%s: cache_control = {type:%q ttl:%q}, want {type:%q ttl:%q}", path, m.Type, m.TTL, typ, ttl)
	}
}

func requireNoMarker(t *testing.T, path string, raw json.RawMessage) {
	t.Helper()
	if len(raw) != 0 {
		t.Fatalf("%s: unexpected cache_control %s", path, raw)
	}
}

func compactRaw(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

// TestDecodeEncodeCacheControlRequest covers the full raw decode -> IR -> encode
// path at every eligible request location: tools and top-level message
// text/image/PDF document/tool_use/outer tool_result blocks. It also asserts
// placement/order, both TTL values, absent/null omission, no implicit TTL, and
// raw schema/argument number preservation.
func TestDecodeEncodeCacheControlRequest(t *testing.T) {
	const schemaRaw = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"minimum":1,"multipleOf":0.5}`
	const argsRaw = `{"q":"x","n":1.0,"m":2}`
	body := []byte(`{
	  "model":"m","max_tokens":16,
	  "tools":[
	    {"name":"search","description":"Find one record","input_schema":` + schemaRaw + `,"cache_control":{"type":"ephemeral"}},
	    {"name":"lookup","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"5m"}},
	    {"name":"calc","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}},
	    {"name":"plain","description":"d","input_schema":{"type":"object"}},
	    {"name":"nullcc","description":"d","input_schema":{"type":"object"},"cache_control":null}
	  ],
	  "tool_choice":{"type":"tool","name":"search"},
	  "messages":[
	    {"role":"user","content":"hello"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"","signature":"sig1","cache_control":{"type":"ephemeral","ttl":"1h"}},
	      {"type":"redacted_thinking","data":"opaque","cache_control":{"type":"ephemeral","ttl":"5m"}},
	      {"type":"text","text":"answer","cache_control":{"type":"ephemeral","ttl":"5m"}},
	      {"type":"tool_use","id":"toolu_1","name":"search","input":` + argsRaw + `,"cache_control":{"type":"ephemeral","ttl":"1h"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","is_error":true,
	       "content":[{"type":"text","text":"result","cache_control":{"type":"ephemeral","ttl":"1h"}}],
	       "cache_control":{"type":"ephemeral","ttl":"5m"}},
	      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"},"cache_control":{"type":"ephemeral"}},
	      {"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"},"cache_control":{"type":"ephemeral","ttl":"1h"}},
	      {"type":"text","text":"tail","cache_control":{"type":"ephemeral","ttl":"1h"}}
	    ]}
	  ]
	}`)

	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Tools) != 5 {
		t.Fatalf("tools = %d, want 5", len(req.Tools))
	}
	wantToolTTL := []struct {
		present bool
		ttl     string
	}{
		{true, ""}, {true, "5m"}, {true, "1h"}, {false, ""}, {false, ""},
	}
	for i, want := range wantToolTTL {
		cc := req.Tools[i].CacheControl
		if !want.present && cc != nil {
			t.Fatalf("tool[%d] hint = %+v, want nil", i, cc)
		}
		if want.present && (cc == nil || cc.Type != "ephemeral" || cc.TTL != want.ttl) {
			t.Fatalf("tool[%d] hint = %+v, want ephemeral/%s", i, cc, want.ttl)
		}
	}
	if compactRaw(t, req.Tools[0].Parameters) != schemaRaw {
		t.Fatalf("tool schema changed: %s", req.Tools[0].Parameters)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != ir.ToolChoiceTool || req.ToolChoice.Name != "search" {
		t.Fatalf("tool_choice changed: %+v", req.ToolChoice)
	}

	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "hello" || req.Messages[0].Content[0].CacheControl != nil {
		t.Fatalf("string message = %+v", req.Messages[0].Content)
	}
	assistant := req.Messages[1].Content
	if len(assistant) != 4 {
		t.Fatalf("assistant blocks = %d, want 4", len(assistant))
	}
	if assistant[0].Type != ir.BlockReasoning || assistant[0].Text != "" || assistant[0].AnthropicSignature != "sig1" || assistant[0].CacheControl != nil {
		t.Fatalf("signed empty thinking = %+v", assistant[0])
	}
	if assistant[1].Type != ir.BlockRedactedReasoning || assistant[1].RedactedData != "opaque" || assistant[1].CacheControl != nil {
		t.Fatalf("redacted thinking = %+v", assistant[1])
	}
	if assistant[2].Type != ir.BlockText || assistant[2].Text != "answer" || assistant[2].CacheControl == nil || assistant[2].CacheControl.TTL != "5m" {
		t.Fatalf("assistant text = %+v", assistant[2])
	}
	if assistant[3].Type != ir.BlockToolUse || assistant[3].ToolID != "toolu_1" || assistant[3].ToolName != "search" || assistant[3].CacheControl == nil || assistant[3].CacheControl.TTL != "1h" {
		t.Fatalf("assistant tool_use = %+v", assistant[3])
	}
	if compactRaw(t, assistant[3].ToolInput) != argsRaw {
		t.Fatalf("tool input changed: %s", assistant[3].ToolInput)
	}
	user := req.Messages[2].Content
	if len(user) != 4 {
		t.Fatalf("user blocks = %d, want 4", len(user))
	}
	outer := user[0]
	if outer.Type != ir.BlockToolResult || outer.ToolUseID != "toolu_1" || !outer.IsError || outer.CacheControl == nil || outer.CacheControl.TTL != "5m" {
		t.Fatalf("outer tool_result = %+v", outer)
	}
	if len(outer.ToolResult) != 1 || outer.ToolResult[0].Text != "result" || outer.ToolResult[0].CacheControl != nil {
		t.Fatalf("nested tool_result content = %+v", outer.ToolResult)
	}
	if user[1].Type != ir.BlockImage || user[1].Image == nil || user[1].Image.Data != "aW1n" || user[1].CacheControl == nil || user[1].CacheControl.TTL != "" {
		t.Fatalf("image = %+v", user[1])
	}
	if user[2].Type != ir.BlockFile || user[2].File == nil || user[2].File.MediaType != "application/pdf" || user[2].File.Filename != "report.pdf" || user[2].CacheControl == nil || user[2].CacheControl.TTL != "1h" {
		t.Fatalf("document = %+v", user[2])
	}
	if user[3].Type != ir.BlockText || user[3].Text != "tail" || user[3].CacheControl == nil || user[3].CacheControl.TTL != "1h" {
		t.Fatalf("tail text = %+v", user[3])
	}

	encoded, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	out := decodeWireOut(t, encoded)
	if len(out.Tools) != 5 {
		t.Fatalf("encoded tools = %d", len(out.Tools))
	}
	requireMarker(t, "tool[0]", out.Tools[0].CacheControl, "ephemeral", "")
	requireMarker(t, "tool[1]", out.Tools[1].CacheControl, "ephemeral", "5m")
	requireMarker(t, "tool[2]", out.Tools[2].CacheControl, "ephemeral", "1h")
	requireNoMarker(t, "tool[3]", out.Tools[3].CacheControl)
	requireNoMarker(t, "tool[4]", out.Tools[4].CacheControl)
	if out.Tools[0].Description != "Find one record" {
		t.Fatalf("tool description changed: %q", out.Tools[0].Description)
	}
	if compactRaw(t, out.Tools[0].InputSchema) != schemaRaw {
		t.Fatalf("encoded schema changed: %s", out.Tools[0].InputSchema)
	}
	var encodedTop map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &encodedTop); err != nil {
		t.Fatal(err)
	}
	var encodedChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(encodedTop["tool_choice"], &encodedChoice); err != nil {
		t.Fatalf("encoded tool_choice invalid: %v", err)
	}
	if encodedChoice.Type != "tool" || encodedChoice.Name != "search" {
		t.Fatalf("encoded tool_choice changed: %+v", encodedChoice)
	}
	if !bytes.Contains(encoded, []byte(`"input":`+argsRaw)) {
		t.Fatalf("encoded tool input lost raw numbers: %s", encoded)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("encoded messages = %d", len(out.Messages))
	}
	encodedAssistant := out.Messages[1].Content
	requireNoMarker(t, "assistant thinking", encodedAssistant[0].CacheControl)
	if encodedAssistant[0].Thinking == nil || *encodedAssistant[0].Thinking != "" || encodedAssistant[0].Signature != "sig1" {
		t.Fatalf("signed empty thinking lost: %+v", encodedAssistant[0])
	}
	requireNoMarker(t, "assistant redacted", encodedAssistant[1].CacheControl)
	requireMarker(t, "assistant text", encodedAssistant[2].CacheControl, "ephemeral", "5m")
	requireMarker(t, "assistant tool_use", encodedAssistant[3].CacheControl, "ephemeral", "1h")
	var encodedInner []wireBlockOut
	if err := json.Unmarshal(out.Messages[2].Content[0].Content, &encodedInner); err != nil {
		t.Fatalf("encoded nested content invalid: %v", err)
	}
	if len(encodedInner) != 1 || encodedInner[0].Text != "result" {
		t.Fatalf("nested tool content changed: %+v", encodedInner)
	}
	requireNoMarker(t, "nested tool content", encodedInner[0].CacheControl)
	requireMarker(t, "outer tool_result", out.Messages[2].Content[0].CacheControl, "ephemeral", "5m")
	requireMarker(t, "image", out.Messages[2].Content[1].CacheControl, "ephemeral", "")
	requireMarker(t, "document", out.Messages[2].Content[2].CacheControl, "ephemeral", "1h")
	requireMarker(t, "tail text", out.Messages[2].Content[3].CacheControl, "ephemeral", "1h")
	// No implicit ttl anywhere.
	if bytes.Contains(encoded, []byte(`"ttl":""`)) {
		t.Fatalf("empty ttl emitted: %s", encoded)
	}
}

func TestDecodeEncodeCacheControlNullAndMissingTTL(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1,
	  "tools":[{"name":"t","input_schema":{},"cache_control":null}],
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"x","cache_control":null},
	    {"type":"text","text":"y","cache_control":{"type":"ephemeral"}}
	  ]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Tools[0].CacheControl != nil || req.Messages[0].Content[0].CacheControl != nil {
		t.Fatalf("null marker became a hint: tool=%+v block=%+v", req.Tools[0].CacheControl, req.Messages[0].Content[0].CacheControl)
	}
	// A marker without ttl is a valid hint with no implicit ttl.
	if cc := req.Messages[0].Content[1].CacheControl; cc == nil || cc.Type != "ephemeral" || cc.TTL != "" {
		t.Fatalf("missing ttl hint = %+v", cc)
	}
	encoded, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	out := decodeWireOut(t, encoded)
	requireNoMarker(t, "tool null marker", out.Tools[0].CacheControl)
	requireNoMarker(t, "text null marker", out.Messages[0].Content[0].CacheControl)
	requireMarker(t, "text missing ttl", out.Messages[0].Content[1].CacheControl, "ephemeral", "")
}

// TestDecodeEncodeTopLevelCacheControlExcluded covers the top-level
// cache_control shorthand. The canonical IR has no top-level hint slot, so
// decode ignores every value without failing, keeps all content, and encode
// never emits a top-level, tool, or block marker for it.
func TestDecodeEncodeTopLevelCacheControlExcluded(t *testing.T) {
	const schemaRaw = `{"type":"object","properties":{"q":{"type":"string"}}}`
	cases := []struct {
		name   string
		marker string // raw JSON value; empty omits the top-level key
	}{
		{"absent", ""},
		{"ephemeral", `{"type":"ephemeral"}`},
		{"ephemeral ttl 1h", `{"type":"ephemeral","ttl":"1h"}`},
		{"number", `5`},
		{"bool", `true`},
		{"string", `"ephemeral"`},
		{"array", `[]`},
		{"object missing type", `{}`},
		{"null type", `{"type":null}`},
		{"wrong type", `{"type":"persistent"}`},
		{"wrong ttl", `{"type":"ephemeral","ttl":"2m"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := ""
			if tc.marker != "" {
				marker = `,"cache_control":` + tc.marker
			}
			body := []byte(`{"model":"m","max_tokens":1,
			  "system":[{"type":"text","text":"sys one"},{"type":"text","text":" sys two"}],
			  "tools":[{"name":"search","description":"find","input_schema":` + schemaRaw + `}],
			  "messages":[
			    {"role":"user","content":[{"type":"text","text":"hello"}]},
			    {"role":"assistant","content":[
			      {"type":"text","text":"answer"},
			      {"type":"tool_use","id":"t1","name":"search","input":{"q":"x"}}
			    ]},
			    {"role":"user","content":[
			      {"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"result"}]}
			    ]}
			  ]` + marker + `}`)
			req, err := DecodeRequest(body)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			encoded, err := EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			requireTopLevelExcluded(t, req, encoded, schemaRaw)
		})
	}
}

// requireTopLevelExcluded asserts one top-level cache_control case kept the
// system, tool, and message content in the IR, produced no hints, and encoded
// without a cache_control key at the top level, on tools, or on blocks.
func requireTopLevelExcluded(t *testing.T, req *ir.Request, encoded []byte, schemaRaw string) {
	t.Helper()
	if req.System != "sys one sys two" {
		t.Fatalf("system = %q", req.System)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "search" || req.Tools[0].Description != "find" || compactRaw(t, req.Tools[0].Parameters) != schemaRaw || req.Tools[0].CacheControl != nil {
		t.Fatalf("tool = %+v", req.Tools)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	user := req.Messages[0].Content
	if len(user) != 1 || user[0].Text != "hello" || user[0].CacheControl != nil {
		t.Fatalf("user = %+v", user)
	}
	assistant := req.Messages[1].Content
	if len(assistant) != 2 || assistant[0].Text != "answer" || assistant[0].CacheControl != nil {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant[1].Type != ir.BlockToolUse || assistant[1].ToolName != "search" || compactRaw(t, assistant[1].ToolInput) != `{"q":"x"}` || assistant[1].CacheControl != nil {
		t.Fatalf("assistant tool_use = %+v", assistant[1])
	}
	result := req.Messages[2].Content
	if len(result) != 1 || result[0].Type != ir.BlockToolResult || result[0].CacheControl != nil {
		t.Fatalf("tool_result = %+v", result)
	}
	if len(result[0].ToolResult) != 1 || result[0].ToolResult[0].Text != "result" || result[0].ToolResult[0].CacheControl != nil {
		t.Fatalf("nested tool_result = %+v", result[0].ToolResult)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &top); err != nil {
		t.Fatalf("encoded body invalid: %v (%s)", err, encoded)
	}
	requireNoKey(t, "top level", top, "cache_control")
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(top["tools"], &tools); err != nil {
		t.Fatalf("encoded tools invalid: %v", err)
	}
	for i, tool := range tools {
		requireNoKey(t, fmt.Sprintf("tool[%d]", i), tool, "cache_control")
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(top["messages"], &messages); err != nil {
		t.Fatalf("encoded messages invalid: %v", err)
	}
	var final map[string]json.RawMessage
	for mi, m := range messages {
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(m["content"], &blocks); err != nil {
			t.Fatalf("message[%d] content invalid: %v", mi, err)
		}
		for bi, block := range blocks {
			path := fmt.Sprintf("message[%d] block[%d]", mi, bi)
			requireNoKey(t, path, block, "cache_control")
			var inner []map[string]json.RawMessage
			if raw := block["content"]; len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &inner) == nil {
				for ii, sub := range inner {
					requireNoKey(t, fmt.Sprintf("%s content[%d]", path, ii), sub, "cache_control")
				}
			}
			final = block
		}
	}
	// The final content block must not gain an automatic marker.
	requireNoKey(t, "final block", final, "cache_control")
}

func requireNoKey(t *testing.T, path string, obj map[string]json.RawMessage, key string) {
	t.Helper()
	if raw, ok := obj[key]; ok {
		t.Fatalf("%s: unexpected %s key: %s", path, key, raw)
	}
}

func TestDecodeRequestRejectsInvalidCacheControl(t *testing.T) {
	const secret = "secret-caller-text-must-not-leak"
	cases := []struct {
		name   string
		marker string
	}{
		{"number", `5`},
		{"bool", `true`},
		{"string", `"ephemeral"`},
		{"array", `[]`},
		{"object missing type", `{}`},
		{"null type", `{"type":null}`},
		{"wrong type", `{"type":"persistent"}`},
		{"wrong ttl", `{"type":"ephemeral","ttl":"2m"}`},
		{"empty ttl", `{"type":"ephemeral","ttl":""}`},
		{"number ttl", `{"type":"ephemeral","ttl":5}`},
	}
	for _, tc := range cases {
		t.Run("tool/"+tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"m","max_tokens":1,"messages":[],"tools":[{"name":"t","description":%q,"input_schema":{},"cache_control":%s}]}`, secret, tc.marker))
			_, err := DecodeRequest(body)
			if err == nil {
				t.Fatal("invalid tool marker accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked caller content: %v", err)
			}
		})
		t.Run("message/"+tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":%q,"cache_control":%s}]}]}`, secret, tc.marker))
			_, err := DecodeRequest(body)
			if err == nil {
				t.Fatal("invalid message marker accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked caller content: %v", err)
			}
		})
	}
	// ttl:null is absence, not a wrong value.
	t.Run("null ttl accepted", func(t *testing.T) {
		body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":null}}]}]}`)
		req, err := DecodeRequest(body)
		if err != nil {
			t.Fatalf("null ttl rejected: %v", err)
		}
		cc := req.Messages[0].Content[0].CacheControl
		if cc == nil || cc.TTL != "" {
			t.Fatalf("null ttl hint = %+v", cc)
		}
	})
}

// TestDecodeRequestCacheControlExcluded ensures a malformed marker in an
// excluded context cannot fail the decode or swallow system text, thinking,
// nested tool content, or attachments.
func TestDecodeRequestCacheControlExcluded(t *testing.T) {
	body := []byte(`{
	  "model":"m","max_tokens":1,
	  "system":[{"type":"text","text":"sys one","cache_control":{"type":5}},{"type":"text","text":" sys two"}],
	  "messages":[
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"think","signature":"s","cache_control":{"type":"ephemeral","ttl":"9h"}},
	      {"type":"redacted_thinking","data":"red","cache_control":7},
	      {"type":"tool_use","id":"t1","name":"n","input":{},"cache_control":{"type":"ephemeral","ttl":"1h"}},
	      {"type":"tool_result","tool_use_id":"t0","is_error":false,"cache_control":{"type":"ephemeral","ttl":"5m"},
	       "content":[
	         {"type":"text","text":"nested text","cache_control":{"type":false}},
	         {"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"},"cache_control":{"ttl":"bad"}}
	       ]},
	      {"type":"text","text":"after nested","cache_control":{"type":"ephemeral","ttl":"1h"}}
	    ]}
	  ]
	}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("excluded malformed marker failed decode: %v", err)
	}
	if req.System != "sys one sys two" {
		t.Fatalf("system text dropped: %q", req.System)
	}
	content := req.Messages[0].Content
	if len(content) != 5 {
		t.Fatalf("blocks = %d, want 5", len(content))
	}
	if content[0].Type != ir.BlockReasoning || content[0].Text != "think" || content[0].CacheControl != nil {
		t.Fatalf("thinking = %+v", content[0])
	}
	if content[1].Type != ir.BlockRedactedReasoning || content[1].RedactedData != "red" || content[1].CacheControl != nil {
		t.Fatalf("redacted = %+v", content[1])
	}
	if content[2].CacheControl == nil || content[2].CacheControl.TTL != "1h" {
		t.Fatalf("top-level tool_use hint = %+v", content[2].CacheControl)
	}
	outer := content[3]
	if outer.CacheControl == nil || outer.CacheControl.TTL != "5m" {
		t.Fatalf("outer tool_result hint = %+v", outer.CacheControl)
	}
	if len(outer.ToolResult) != 2 || outer.ToolResult[0].Text != "nested text" || outer.ToolResult[0].CacheControl != nil {
		t.Fatalf("nested text = %+v", outer.ToolResult)
	}
	if outer.ToolResult[1].Image == nil || outer.ToolResult[1].Image.Data != "aW1n" || outer.ToolResult[1].CacheControl != nil {
		t.Fatalf("nested image = %+v", outer.ToolResult[1])
	}
	if content[4].Text != "after nested" || content[4].CacheControl == nil {
		t.Fatalf("tail text = %+v", content[4])
	}

	encoded, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	out := decodeWireOut(t, encoded)
	blocks := out.Messages[0].Content
	requireNoMarker(t, "thinking", blocks[0].CacheControl)
	requireNoMarker(t, "redacted", blocks[1].CacheControl)
	requireMarker(t, "tool_use", blocks[2].CacheControl, "ephemeral", "1h")
	requireMarker(t, "outer tool_result", blocks[3].CacheControl, "ephemeral", "5m")
	var inner []wireBlockOut
	if err := json.Unmarshal(blocks[3].Content, &inner); err != nil {
		t.Fatalf("nested content: %v", err)
	}
	requireNoMarker(t, "nested text", inner[0].CacheControl)
	requireNoMarker(t, "nested image", inner[1].CacheControl)
	requireMarker(t, "tail", blocks[4].CacheControl, "ephemeral", "1h")
}

// TestEncodeCacheControlUnsupportedHints ensures hand-built IR hints on
// reasoning, redacted reasoning, and nested tool_result content never reach the
// wire, while an outer tool_result hint does.
func TestEncodeCacheControlUnsupportedHints(t *testing.T) {
	cc := func(ttl string) *ir.CacheControl { return &ir.CacheControl{Type: "ephemeral", TTL: ttl} }
	req := &ir.Request{
		Model:     "m",
		MaxTokens: 1,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{
			{Type: ir.BlockReasoning, Text: "t", AnthropicSignature: "s", CacheControl: cc("5m")},
			{Type: ir.BlockRedactedReasoning, RedactedData: "d", CacheControl: cc("1h")},
			{Type: ir.BlockToolResult, ToolUseID: "t1", CacheControl: cc("1h"), ToolResult: []ir.ContentBlock{
				{Type: ir.BlockText, Text: "nested", CacheControl: cc("5m")},
				{Type: ir.BlockImage, Image: &ir.Image{Data: "aW1n", MediaType: "image/png"}, CacheControl: cc("5m")},
			}},
			{Type: ir.BlockText, Text: "top", CacheControl: cc("5m")},
			{Type: ir.BlockFile, File: &ir.File{MediaType: "text/plain", Data: "cGxhaW4"}, CacheControl: cc("1h")},
		}}},
	}
	encoded, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	out := decodeWireOut(t, encoded)
	blocks := out.Messages[0].Content
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4 (non-PDF file skipped)", len(blocks))
	}
	requireNoMarker(t, "thinking", blocks[0].CacheControl)
	requireNoMarker(t, "redacted", blocks[1].CacheControl)
	requireMarker(t, "outer tool_result", blocks[2].CacheControl, "ephemeral", "1h")
	var inner []wireBlockOut
	if err := json.Unmarshal(blocks[2].Content, &inner); err != nil {
		t.Fatalf("nested content: %v", err)
	}
	if len(inner) != 2 {
		t.Fatalf("nested blocks = %d", len(inner))
	}
	requireNoMarker(t, "nested text", inner[0].CacheControl)
	requireNoMarker(t, "nested image", inner[1].CacheControl)
	requireMarker(t, "top text", blocks[3].CacheControl, "ephemeral", "5m")
}

// TestDecodeRequestCacheControlUnknownKeys shows unknown marker keys are
// ignored by the typed conversion and never round-trip.
func TestDecodeRequestCacheControlUnknownKeys(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h","extra":{"nested":true},"other":7}}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	cc := req.Messages[0].Content[0].CacheControl
	if cc == nil || cc.Type != "ephemeral" || cc.TTL != "1h" {
		t.Fatalf("hint = %+v", cc)
	}
	encoded, err := EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if bytes.Contains(encoded, []byte("extra")) || bytes.Contains(encoded, []byte("other")) {
		t.Fatalf("unknown marker keys leaked: %s", encoded)
	}
	out := decodeWireOut(t, encoded)
	requireMarker(t, "text", out.Messages[0].Content[0].CacheControl, "ephemeral", "1h")
}
