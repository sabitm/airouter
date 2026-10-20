package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/domain"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestUnmarkedSystemBlocksPreserveBoundaries(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"system":[{"type":"text","text":"one"},{"type":"text","text":"two"}],"messages":[{"role":"user","content":"hi"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "onetwo" || len(req.SystemBlocks) != 2 {
		t.Fatalf("system=%q blocks=%+v", req.System, req.SystemBlocks)
	}
	for _, encode := range []func(*ir.Request) ([]byte, error){EncodeRequest, EncodeRequestClaudeCode} {
		out, err := encode(req)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		var blocks []map[string]any
		if err := json.Unmarshal(doc["system"], &blocks); err != nil {
			t.Fatalf("system is not an array: %s", doc["system"])
		}
		if len(blocks) != 2 || blocks[0]["text"] != "one" || blocks[1]["text"] != "two" {
			t.Fatalf("system blocks = %s", doc["system"])
		}
	}
}

func TestBillingStripLeavesCallerText(t *testing.T) {
	const header = "x-anthropic-billing-header: cc_version=2.1.303; cc_entrypoint=cli; cch=00000;"
	const caller = "keep this; text; here"
	raw := mustJSONRaw([]map[string]any{{"type": "text", "text": header + caller}})
	flat, blocks, err := decodeSystem(raw)
	if err != nil {
		t.Fatal(err)
	}
	if flat != caller {
		t.Fatalf("flat=%q want %q", flat, caller)
	}
	if len(blocks) != 1 || blocks[0].Text != caller {
		t.Fatalf("blocks=%+v", blocks)
	}
}

func TestRequestServerBlockTypesPreserved(t *testing.T) {
	types := []string{
		"server_tool_use", "tool_reference", "compaction", "server_tool_result",
		"web_search_tool_result", "web_fetch_tool_result", "advisor_tool_result",
		"code_execution_tool_result", "bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result", "tool_search_tool_result",
		"mcp_tool_use", "mcp_tool_result", "container_upload", "fallback",
		"mid_conv_system", "search_result",
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":%q,"marker":"keep-%s"}]}]}`, typ, typ))
			req, err := DecodeRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
				t.Fatalf("content=%+v", req.Messages)
			}
			b := req.Messages[0].Content[0]
			if b.Type != ir.BlockServer || b.AnthropicServer == nil || b.AnthropicServer.WireType != typ {
				t.Fatalf("block=%+v", b)
			}
			if !strings.Contains(string(b.AnthropicServer.Raw), "keep-"+typ) {
				t.Fatalf("raw lost marker: %s", b.AnthropicServer.Raw)
			}
			out, err := EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"type":"`+typ+`"`) || !strings.Contains(string(out), "keep-"+typ) {
				t.Fatalf("encoded = %s", out)
			}
		})
	}
}

func TestNestedSearchResultPreserved(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"search_result","source":"doc","title":"T","content":[{"type":"text","text":"body"}],"citations":{"enabled":true}}]}]}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Type != ir.BlockToolResult {
		t.Fatalf("content=%+v", req.Messages[0].Content)
	}
	nested := req.Messages[0].Content[0].ToolResult
	if len(nested) != 1 || nested[0].Type != ir.BlockServer || nested[0].AnthropicServer.WireType != "search_result" {
		t.Fatalf("nested=%+v", nested)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"search_result"`) || !strings.Contains(string(out), `"enabled":true`) {
		t.Fatalf("encoded = %s", out)
	}
}

func TestUnknownRequestBlockFailsClosed(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"future_block","value":1}]}]}`)
	if _, err := DecodeRequest(body); err == nil {
		t.Fatal("unknown request block was silently dropped")
	}
}

func TestResponseServerTypesDistinguishRequestOnly(t *testing.T) {
	accepted := []string{"server_tool_use", "web_search_tool_result", "advisor_tool_result", "tool_search_tool_result", "container_upload", "compaction", "fallback", "mcp_tool_use"}
	for _, typ := range accepted {
		t.Run("accept-"+typ, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":%q,"marker":"m"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, typ))
			resp, err := DecodeResponse(body)
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Content) != 1 || resp.Content[0].Type != ir.BlockServer {
				t.Fatalf("content=%+v", resp.Content)
			}
			out, err := EncodeResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), "marker") {
				t.Fatalf("encoded = %s", out)
			}
		})
	}
	for _, typ := range []string{"mid_conv_system", "tool_reference", "search_result"} {
		t.Run("reject-"+typ, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":%q}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, typ))
			if _, err := DecodeResponse(body); err == nil {
				t.Fatalf("response accepted request-only block %s", typ)
			}
		})
	}
}

func TestTextCitationsRoundTrip(t *testing.T) {
	body := []byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"hello","citations":[{"type":"char_location","document_index":0,"start_char_index":1,"end_char_index":4,"document_title":"Doc","cited_text":"ell"}]}],"stop_reason":"end_turn","stop_sequence":"STOP","usage":{"input_tokens":1,"output_tokens":2},"container":{"id":"c1"},"context_management":{"edits":[{"type":"compact_x"}]},"stop_details":{"type":"refusal"},"input_transformations":[{"type":"thinking_dropped","path":"messages.0","reason":"prefix_binding_mismatch"}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || len(resp.Content[0].Citations) != 1 || resp.Content[0].Citations[0].Type != "char_location" {
		t.Fatalf("citations=%+v", resp.Content)
	}
	if resp.StopSequence == nil || *resp.StopSequence != "STOP" {
		t.Fatalf("stop_sequence=%v", resp.StopSequence)
	}
	if resp.Anthropic == nil || len(resp.Anthropic.Container) == 0 || len(resp.Anthropic.ContextManagement) == 0 || len(resp.Anthropic.StopDetails) == 0 || len(resp.Anthropic.InputTransformations) == 0 {
		t.Fatalf("response state=%+v", resp.Anthropic)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"citations":[`, `"char_location"`, `"stop_sequence":"STOP"`, `"container":{"id":"c1"}`, `"context_management"`, `"stop_details"`, `"input_transformations"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestRequestStateRoundTripAndValidation(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8,"output_config":{"task_budget":{"type":"tokens","total":1000,"remaining":0},"format":{"type":"json_schema","schema":{"type":"object","title":"Out"}}},"fallbacks":[{"model":"claude-opus-4-8"}],"container":{"id":"c1","extra":true},"metadata":{"user_id":"u"},"context_management":{"edits":[{"type":"compact_x"}]},"messages":[{"role":"user","content":"hi"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Anthropic == nil || req.Anthropic.TaskBudget == nil || req.Anthropic.TaskBudget.Total != 1000 || req.Anthropic.TaskBudget.Remaining == nil || *req.Anthropic.TaskBudget.Remaining != 0 {
		t.Fatalf("task_budget=%+v", req.Anthropic)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"task_budget":{"type":"tokens","total":1000,"remaining":0}`, `"fallbacks":[{"model":"claude-opus-4-8"}]`, `"container":{"id":"c1","extra":true}`, `"metadata":{"user_id":"u"}`, `"context_management"`, `"json_schema"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}

	for name, raw := range map[string]string{
		"task budget type":       `"task_budget":{"type":"words","total":1}`,
		"task budget no total":   `"task_budget":{"type":"tokens"}`,
		"task budget negative":   `"task_budget":{"type":"tokens","total":-1}`,
		"fallbacks string":       `"fallbacks":"other"`,
		"fallbacks empty model":  `"fallbacks":[{"model":""}]`,
		"container number":       `"container":7`,
		"context edits not list": `"context_management":{"edits":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var extra string
			if strings.HasPrefix(raw, `"task_budget"`) {
				extra = `"output_config":{` + raw + `}`
			} else {
				extra = raw
			}
			bad := []byte(`{"model":"m","max_tokens":8,` + extra + `,"messages":[{"role":"user","content":"hi"}]}`)
			if _, err := DecodeRequest(bad); err == nil {
				t.Fatalf("accepted invalid request state: %s", bad)
			}
		})
	}
}

func TestStreamCitationsAndCompaction(t *testing.T) {
	raw := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"x\",\"content\":[],\"container\":{\"id\":\"c1\"},\"context_management\":{\"edits\":[]},\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\",\"citations\":[{\"type\":\"char_location\",\"start_char_index\":0,\"end_char_index\":1}]}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"web_search_result_location\",\"url\":\"https://example.test\"}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"compaction\",\"content\":\"\",\"encrypted_content\":\"enc1\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"compaction_delta\",\"content\":\"summary\",\"encrypted_content\":\"enc2\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":\"STOP\"},\"context_management\":{\"edits\":[{\"type\":\"compact_x\"}]},\"input_transformations\":[{\"type\":\"thinking_dropped\",\"path\":\"messages.0\",\"reason\":\"x\"}],\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	var events []ir.StreamEvent
	if err := DecodeStream(strings.NewReader(raw), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var citations, updates int
	for _, ev := range events {
		switch ev.Kind {
		case ir.EventTextCitation:
			citations++
		case ir.EventServerUpdate:
			updates++
			if !strings.Contains(ev.Data, `"content":"summary"`) || !strings.Contains(ev.Data, `"encrypted_content":"enc2"`) {
				t.Fatalf("update=%s", ev.Data)
			}
		}
	}
	if citations != 2 || updates != 1 {
		t.Fatalf("citations=%d updates=%d", citations, updates)
	}
	finish := events[len(events)-1]
	if finish.StopSequence == nil || *finish.StopSequence != "STOP" || len(finish.ContextManagement) == 0 || len(finish.InputTransformations) == 0 {
		t.Fatalf("finish=%+v", finish)
	}

	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("no writer")
	}
	enc := NewStreamEncoder("x")
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatalf("encode %v: %v", ev.Kind, err)
		}
	}
	body := rec.Body.String()
	for _, want := range []string{`"citations_delta"`, `"char_location"`, `"web_search_result_location"`, `"compaction_delta"`, `"content":"summary"`, `"encrypted_content":"enc2"`, `"stop_sequence":"STOP"`, `"context_management"`, `"input_transformations"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}

func TestStreamServerBoundsAndIndexes(t *testing.T) {
	start := func(index int, typ, pad string) string {
		return fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":%q,\"pad\":%q}}\n\n", index, typ, pad)
	}
	stop := func(index int) string {
		return fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index)
	}
	head := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"
	tail := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	decode := func(body string) error {
		return DecodeStream(strings.NewReader(body), func(ir.StreamEvent) error { return nil })
	}

	t.Run("negative index", func(t *testing.T) {
		if err := decode(head + start(-1, "server_tool_use", "x") + tail); err == nil {
			t.Fatal("negative index accepted")
		}
	})
	t.Run("duplicate open index", func(t *testing.T) {
		if err := decode(head + start(0, "server_tool_use", "x") + start(0, "server_tool_use", "y") + tail); err == nil {
			t.Fatal("duplicate index accepted")
		}
	})
	t.Run("reused after stop", func(t *testing.T) {
		if err := decode(head + start(0, "server_tool_use", "x") + stop(0) + start(0, "server_tool_use", "y") + tail); err == nil {
			t.Fatal("reused index accepted")
		}
	})
	t.Run("oversized start", func(t *testing.T) {
		if err := decode(head + start(0, "server_tool_use", strings.Repeat("x", maxServerStartBytes)) + tail); err == nil {
			t.Fatal("oversized start accepted")
		}
	})
	t.Run("open at eof", func(t *testing.T) {
		if err := decode(head + start(0, "server_tool_use", "x")); err == nil {
			t.Fatal("open server accepted at EOF")
		}
	})
	t.Run("bytes released on stop", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(head)
		pad := strings.Repeat("x", 100*1024)
		for i := 0; i < 40; i++ {
			b.WriteString(start(i, "server_tool_use", pad))
			b.WriteString(stop(i))
		}
		b.WriteString(tail)
		if err := decode(b.String()); err != nil {
			t.Fatalf("sequential blocks hit a cumulative limit: %v", err)
		}
	})
}

func TestAnthropicApplyThinkingUsesModelCaps(t *testing.T) {
	req := &ir.Request{
		Model:     "claude-opus-4-1",
		MaxTokens: 100000,
		Thinking:  &ir.Thinking{Mode: ir.ThinkingLevel, Level: "max"},
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "hi"}}}},
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MaxTokens != 32000 {
		t.Fatalf("max_tokens=%d want source-backed opus-4-1 ceiling 32000; body=%s", doc.MaxTokens, out)
	}
}

var _ = domain.ProtocolAnthropic

func TestDisabledThinkingDropsDisplayAndBudget(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"thinking":{"type":"disabled","display":"summarized","budget_tokens":8192},"messages":[{"role":"user","content":"hi"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Thinking map[string]any `json:"thinking"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Thinking) != 1 || got.Thinking["type"] != "disabled" {
		t.Fatalf("disabled thinking kept extra fields: %s", out)
	}
}

func requireProtocolFailure(t *testing.T, body string) {
	t.Helper()
	events, err := collectDecode(t, body)
	if err == nil {
		t.Fatalf("stream was accepted: %+v", events)
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
}

func TestExplicitTextDeltaAfterStopFails(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"late"}}`,
		`{"type":"message_stop"}`,
	)
	requireProtocolFailure(t, body)
}

func TestExplicitTextDeltaWithoutStartFails(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"missing"}}`,
		`{"type":"message_stop"}`,
	)
	requireProtocolFailure(t, body)
}

func TestExplicitTextBlockIndexReuseFails(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"first"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"again"}}`,
		`{"type":"message_stop"}`,
	)
	requireProtocolFailure(t, body)
}

func TestCitationsRequireTheirOpenTextBlock(t *testing.T) {
	cases := map[string]string{
		"after text stop": anthropicSSE(
			`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"first"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}`,
			`{"type":"message_stop"}`,
		),
		"without text start": anthropicSSE(
			`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}`,
			`{"type":"message_stop"}`,
		),
		"on stopped server block": anthropicSSE(
			`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s","name":"w","input":{}}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}`,
			`{"type":"message_stop"}`,
		),
		"on thinking block": anthropicSSE(
			`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"char_location","start_char_index":0,"end_char_index":1}}}`,
			`{"type":"message_stop"}`,
		),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			requireProtocolFailure(t, body)
		})
	}
}

func TestWrongTypeDeltasOnStoppedServerFail(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s","name":"w","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"wrong"}}`,
		`{"type":"message_stop"}`,
	)
	requireProtocolFailure(t, body)
}

func TestMultipleExplicitCitedTextBlocksRoundTrip(t *testing.T) {
	raw := anthropicSSE(
		`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"first","citations":[{"type":"char_location","start_char_index":0,"end_char_index":1}]}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"second"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.test"}}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_stop"}`,
	)
	events, err := collectDecode(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	assertCitations := func(events []ir.StreamEvent) {
		t.Helper()
		var citations []ir.StreamEvent
		for _, ev := range events {
			if ev.Kind == ir.EventTextCitation {
				citations = append(citations, ev)
			}
		}
		if len(citations) != 2 {
			t.Fatalf("citations = %+v", citations)
		}
		if citations[0].Index != 0 || !citations[0].TextIndexed || !strings.Contains(citations[0].Data, `"char_location"`) {
			t.Fatalf("first citation = %+v", citations[0])
		}
		if citations[1].Index != 1 || !citations[1].TextIndexed || !strings.Contains(citations[1].Data, `"web_search_result_location"`) {
			t.Fatalf("second citation = %+v", citations[1])
		}
	}
	assertCitations(events)
	again, err := collectDecode(t, encodeStream(t, events))
	if err != nil {
		t.Fatal(err)
	}
	assertCitations(again)
}
