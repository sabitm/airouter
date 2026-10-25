package responses

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestDecodeResponseRefusalOnlyCompleted(t *testing.T) {
	body := []byte(`{"id":"resp_r","model":"m","status":"completed","incomplete_details":null,"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopRefusal {
		t.Fatalf("stop = %q, want refusal", resp.StopReason)
	}
	if len(resp.Content) != 1 || !resp.Content[0].Refusal || resp.Content[0].Text != "no" {
		t.Fatalf("content = %+v", resp.Content)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"completed"`) || !strings.Contains(string(out), `"incomplete_details":null`) {
		t.Fatalf("encoded status = %s", out)
	}
	if !strings.Contains(string(out), `"type":"refusal"`) || !strings.Contains(string(out), `"refusal":"no"`) {
		t.Fatalf("encoded part = %s", out)
	}
	if strings.Contains(string(out), `"output_text"`) {
		t.Fatalf("refusal duplicated as text: %s", out)
	}
}

func TestDecodeResponseEmptyRefusal(t *testing.T) {
	body := []byte(`{"id":"resp_e","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":""}]}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopRefusal || len(resp.Content) != 1 || !resp.Content[0].Refusal || resp.Content[0].Text != "" {
		t.Fatalf("got stop=%q content=%+v", resp.StopReason, resp.Content)
	}
}

func TestDecodeResponseMixedSegments(t *testing.T) {
	body := []byte(`{"id":"resp_m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"before"},{"type":"refusal","refusal":"no"},{"type":"output_text","text":"after"}]}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("segments = %d, want 3: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Refusal || resp.Content[0].Text != "before" || !resp.Content[1].Refusal || resp.Content[1].Text != "no" || resp.Content[2].Refusal || resp.Content[2].Text != "after" {
		t.Fatalf("content = %+v", resp.Content)
	}
}

func TestDecodeResponseContentFilterOutranksRefusal(t *testing.T) {
	body := []byte(`{"id":"resp_f","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"},{"type":"refusal","refusal":"no"}]}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopContentFilter {
		t.Fatalf("stop = %q, want content_filter", resp.StopReason)
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"status":"incomplete"`) || !strings.Contains(string(out), `"reason":"content_filter"`) {
		t.Fatalf("encoded = %s", out)
	}
}

func TestDecodeResponseToolOutranksRefusal(t *testing.T) {
	body := []byte(`{"id":"resp_t","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`)
	resp, err := DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ir.StopToolUse {
		t.Fatalf("stop = %q, want tool_use", resp.StopReason)
	}
}

func TestDecodeRequestRefusalHistoryPreserved(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"first"},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":""}]},{"type":"message","role":"user","content":"second"}]}`)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(req.Messages), req.Messages)
	}
	mid := req.Messages[1]
	if mid.Role != ir.RoleAssistant || len(mid.Content) != 1 || !mid.Content[0].Refusal || mid.Content[0].Text != "" {
		t.Fatalf("middle = %+v", mid)
	}
	out, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"refusal":""`) {
		t.Fatalf("empty refusal dropped: %s", out)
	}
	codex, err := EncodeCodexRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codex), `"refusal":""`) || strings.Count(string(codex), `"role":"user"`) < 2 {
		t.Fatalf("codex history = %s", codex)
	}
}

func TestDecodeRequestMalformedRefusalFailsClosed(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":1}]}]}`)
	if _, err := DecodeRequest(body); err == nil {
		t.Fatal("malformed refusal accepted")
	}
}

func TestDecodeStreamRefusalDeltaAndDone(t *testing.T) {
	body := sseData(
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_r","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.content_part.added","item_id":"msg_r","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
		`{"type":"response.refusal.delta","item_id":"msg_r","output_index":0,"content_index":0,"delta":"no"}`,
		`{"type":"response.refusal.done","item_id":"msg_r","output_index":0,"content_index":0,"refusal":"nope"}`,
		`{"type":"response.completed","response":{"id":"resp_r","status":"completed","incomplete_details":null,"output":[{"id":"msg_r","type":"message","content":[{"type":"refusal","refusal":"nope"}]}],"usage":{"input_tokens":4,"output_tokens":1}}}`,
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var text []ir.StreamEvent
	var finish *ir.StreamEvent
	for i := range evs {
		if evs[i].Kind == ir.EventTextDelta {
			text = append(text, evs[i])
		}
		if evs[i].Kind == ir.EventFinish {
			finish = &evs[i]
		}
	}
	if len(text) != 2 || text[0].Text != "no" || !text[0].Refusal || text[1].Text != "pe" || !text[1].Refusal {
		t.Fatalf("deltas = %+v", text)
	}
	if finish == nil || finish.StopReason != ir.StopRefusal || finish.InputTokens != 4 || finish.OutputTokens != 1 {
		t.Fatalf("finish = %+v", finish)
	}
}

func TestDecodeStreamRefusalFinalOnlyAndEmpty(t *testing.T) {
	t.Run("final only", func(t *testing.T) {
		body := sseData(`{"type":"response.completed","response":{"id":"resp_r","status":"completed","output":[{"id":"msg_r","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := joinText(evs)
		if got != "no" || n != 1 || !evsTextRefusal(evs) {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("empty refusal", func(t *testing.T) {
		body := sseData(`{"type":"response.completed","response":{"id":"resp_e","status":"completed","output":[{"id":"msg_e","type":"message","content":[{"type":"refusal","refusal":""}]}]}}`)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) < 2 || evs[0].Kind != ir.EventMessageStart || !evs[1].Refusal || evs[1].Text != "" {
			t.Fatalf("events = %+v", evs)
		}
	})
}

func TestDecodeStreamMixedRefusalRecovery(t *testing.T) {
	body := sseData(
		`{"type":"response.output_text.delta","item_id":"msg_m","output_index":0,"content_index":0,"delta":"before"}`,
		`{"type":"response.output_text.done","item_id":"msg_m","output_index":0,"content_index":0,"text":"before"}`,
		`{"type":"response.refusal.delta","item_id":"msg_m","output_index":0,"content_index":1,"delta":"no"}`,
		`{"type":"response.completed","response":{"id":"resp_m","status":"completed","output":[{"id":"msg_m","type":"message","content":[{"type":"output_text","text":"before"},{"type":"refusal","refusal":"nope"},{"type":"output_text","text":"after"}]}]}}`,
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []ir.StreamEvent
	for _, ev := range evs {
		if ev.Kind == ir.EventTextDelta {
			deltas = append(deltas, ev)
		}
	}
	if len(deltas) != 4 || deltas[0].Text != "before" || deltas[0].Refusal || deltas[1].Text != "no" || !deltas[1].Refusal || deltas[2].Text != "pe" || !deltas[2].Refusal || deltas[3].Text != "after" || deltas[3].Refusal {
		t.Fatalf("prefix recovery = %+v", deltas)
	}
	again, err := collectStream(t, body+sseData(`{"type":"response.completed","response":{"id":"resp_m","status":"completed","output":[{"id":"msg_m","type":"message","content":[{"type":"output_text","text":"before"},{"type":"refusal","refusal":"nope"},{"type":"output_text","text":"after"}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := joinText(again); got != "beforenopeafter" {
		t.Fatalf("repeated snapshot duplicated: %q", got)
	}
}

func TestDecodeStreamRefusalConflicts(t *testing.T) {
	cases := []string{
		sseData(`{"type":"response.refusal.delta","item_id":"msg_r","output_index":0,"content_index":0,"delta":1}`),
		sseData(
			`{"type":"response.refusal.delta","item_id":"msg_r","output_index":0,"content_index":0,"delta":"no"}`,
			`{"type":"response.refusal.done","item_id":"msg_r","output_index":0,"content_index":0,"refusal":"other"}`,
		),
		sseData(
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"msg_r","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
			`{"type":"response.refusal.delta","item_id":"msg_r","output_index":0,"content_index":0,"delta":"late"}`,
		),
	}
	for _, body := range cases {
		if _, err := collectStream(t, body); err == nil {
			t.Fatalf("accepted invalid refusal stream: %s", body)
		}
	}
}

func TestEncodeStreamRefusalKindTransition(t *testing.T) {
	body := runQueueEvents(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventTextDelta, Text: "before"},
		{Kind: ir.EventTextDelta, Text: "no", Refusal: true},
		{Kind: ir.EventTextDelta, Refusal: true},
		{Kind: ir.EventFinish, StopReason: ir.StopRefusal, InputTokens: 2, OutputTokens: 1},
	}, false)
	if !strings.Contains(body, `"type":"response.output_text.delta"`) || !strings.Contains(body, `"type":"response.refusal.delta"`) {
		t.Fatalf("missing kind events: %s", body)
	}
	if !strings.Contains(body, `"type":"response.refusal.done"`) || strings.Contains(body, `"text":"beforeno"`) {
		t.Fatalf("parts merged or refusal done missing: %s", body)
	}
	if strings.Contains(body, `"type":"response.incomplete"`) || !strings.Contains(body, `"status":"completed"`) {
		t.Fatalf("terminal = %s", body)
	}
	filter := runQueueEvents(t, []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_2"},
		{Kind: ir.EventTextDelta, Text: "partial"},
		{Kind: ir.EventFinish, StopReason: ir.StopContentFilter},
	}, false)
	if !strings.Contains(filter, `"type":"response.incomplete"`) || !strings.Contains(filter, `"reason":"content_filter"`) {
		t.Fatalf("content filter terminal = %s", filter)
	}
	maxTokens := runQueueEvents(t, []ir.StreamEvent{
		{Kind: ir.EventFinish, StopReason: ir.StopMaxTokens},
	}, false)
	if !strings.Contains(maxTokens, `"type":"response.incomplete"`) || !strings.Contains(maxTokens, `"reason":"max_output_tokens"`) {
		t.Fatalf("max token terminal = %s", maxTokens)
	}
}

func evsTextRefusal(evs []ir.StreamEvent) bool {
	for _, ev := range evs {
		if ev.Kind == ir.EventTextDelta && ev.Text != "" && !ev.Refusal {
			return false
		}
	}
	return true
}

func TestDecodeStreamRefusalIdentityShapes(t *testing.T) {
	t.Run("item only", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","item_id":"msg_r","delta":"no"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"msg_r","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "no", ir.StopRefusal)
	})
	t.Run("index only", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"no"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"msg_r","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "no", ir.StopRefusal)
	})
	t.Run("omitted content index recovers", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"delta":"no"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "no", ir.StopRefusal)
	})
	t.Run("unkeyed refusal rejected", func(t *testing.T) {
		body := sseData(`{"type":"response.refusal.delta","delta":"no"}`)
		if _, err := collectStream(t, body); err == nil {
			t.Fatal("unkeyed refusal accepted")
		}
	})
	t.Run("marker change rejected", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"m","output_index":0,"delta":"kept"}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"delta":"no"}`,
		)
		if _, err := collectStream(t, body); err == nil {
			t.Fatal("mixed unindexed marker accepted")
		}
	})
}

func TestDecodeStreamContentPartAddedRefusal(t *testing.T) {
	t.Run("added only nonempty", func(t *testing.T) {
		body := sseData(`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"no"}}`)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := joinText(evs)
		if got != "no" || n != 1 || !evsTextRefusal(evs) {
			t.Fatalf("events = %+v", evs)
		}
		// A nonempty added refusal is recorded without closing its part. EOF still
		// selects StopRefusal unless a stronger terminal applies.
		var finish *ir.StreamEvent
		for i := range evs {
			if evs[i].Kind == ir.EventFinish {
				finish = &evs[i]
			}
		}
		if finish == nil || finish.StopReason != ir.StopRefusal {
			t.Fatalf("finish = %+v, want refusal", finish)
		}
	})
	t.Run("placeholder then deltas and done", func(t *testing.T) {
		body := sseData(
			`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"n"}`,
			`{"type":"response.refusal.done","item_id":"m","output_index":0,"content_index":0,"refusal":"no"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","content":[{"type":"refusal","refusal":"no"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "no", ir.StopRefusal)
	})
	t.Run("malformed added fails", func(t *testing.T) {
		body := sseData(`{"type":"response.content_part.added","part":{"type":"refusal","refusal":1}}`)
		if _, err := collectStream(t, body); err == nil {
			t.Fatal("malformed added refusal accepted")
		}
	})
}

func TestDecodeStreamRefusalLimits(t *testing.T) {
	t.Run("byte", func(t *testing.T) {
		_, err := decodeText(t, sseData(`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"`+strings.Repeat("x", 64)+`"}`), 32, 4, maxPendingTextBytes, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "message text too large") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("part", func(t *testing.T) {
		var out []ir.StreamEvent
		err := decodeStreamBoundedParts(strings.NewReader(sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"a"}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":1,"delta":"b"}`,
		)), func(ev ir.StreamEvent) error {
			out = append(out, ev)
			return nil
		}, maxRetainedToolArgBytes, maxTrackedToolCalls, 4096, 4, 1, maxPendingTextBytes, maxPendingTextEvents, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts)
		if err == nil || !strings.Contains(err.Error(), "message text part index out of range") {
			t.Fatalf("err = %v events=%+v", err, out)
		}
	})
	t.Run("pending", func(t *testing.T) {
		_, err := decodeText(t, sseData(
			`{"type":"response.refusal.delta","item_id":"a","output_index":0,"delta":"a"}`,
			`{"type":"response.refusal.delta","item_id":"b","output_index":1,"delta":"b"}`,
			`{"type":"response.refusal.delta","item_id":"b","output_index":1,"delta":"offending"}`,
		), 4096, 4, maxPendingTextBytes, 1)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDecodeStreamMixedRefusalToolOrder(t *testing.T) {
	body := sseData(
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"think"}`,
		`{"type":"response.output_text.delta","item_id":"m","output_index":1,"content_index":0,"delta":"kept"}`,
		`{"type":"response.refusal.delta","item_id":"m","output_index":1,"content_index":1,"delta":"n"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"a\""}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":":1}"}`,
		`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":9,"output_tokens":4,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}},"output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"id":"m","type":"message","content":[{"type":"output_text","text":"kept"},{"type":"refusal","refusal":"no"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"fn","arguments":"{\"a\":1}"}]}}`,
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var finish *ir.StreamEvent
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventReasoningDelta:
			kinds = append(kinds, "reason:"+ev.Text)
		case ir.EventTextDelta:
			kinds = append(kinds, fmt.Sprintf("text:%s:%t", ev.Text, ev.Refusal))
		case ir.EventToolCallDelta:
			kinds = append(kinds, "tool:"+ev.ArgsFrag)
		case ir.EventFinish:
			copied := ev
			finish = &copied
		}
	}
	want := []string{"reason:think", "text:kept:false", "text:n:true", "text:o:true", "tool:{\"a\":1}"}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v", kinds)
	}
	if finish == nil || finish.StopReason != ir.StopToolUse || finish.CacheReadTokens != 3 || finish.CacheWriteTokens != 2 {
		t.Fatalf("finish = %+v", finish)
	}
	again, err := collectStream(t, body+sseData(`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"id":"m","type":"message","content":[{"type":"output_text","text":"kept"},{"type":"refusal","refusal":"no"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"fn","arguments":"{\"a\":1}"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, n := joinText(again); got != "keptno" || n != 3 {
		t.Fatalf("repeat = %q (%d)", got, n)
	}
}

func TestEncodeResponseProjectsAnthropicUnaryRefusal(t *testing.T) {
	resp := &ir.Response{
		ID: "msg", Model: "m", StopReason: ir.StopRefusal,
		Content: []ir.ContentBlock{{Type: ir.BlockText, Text: "no"}},
		Usage:   ir.Usage{InputTokens: 4, OutputTokens: 1, CacheReadTokens: 2, CacheWriteTokens: 1},
	}
	out, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"refusal"`) || !strings.Contains(string(out), `"refusal":"no"`) || strings.Contains(string(out), `"output_text"`) {
		t.Fatalf("encoded = %s", out)
	}
	if resp.Content[0].Refusal {
		t.Fatal("projection mutated shared content")
	}
	if !strings.Contains(string(out), `"cached_tokens":2`) || !strings.Contains(string(out), `"cache_write_tokens":1`) {
		t.Fatalf("usage dropped: %s", out)
	}
}

func TestStreamEncoderTextAllocGrowth(t *testing.T) {
	const frag = "12345678"
	measure := func(n int) int64 {
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				enc := NewStreamEncoder("m")
				w, ok := sse.NewWriter(&discardResponseWriter{})
				if !ok {
					b.Fatal("discard writer does not flush")
				}
				if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "r"}, w); err != nil {
					b.Fatal(err)
				}
				for j := 0; j < n; j++ {
					if err := enc.Encode(ir.StreamEvent{Kind: ir.EventTextDelta, Text: frag}, w); err != nil {
						b.Fatal(err)
					}
				}
				if err := enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn}, w); err != nil {
					b.Fatal(err)
				}
			}
		})
		return result.AllocedBytesPerOp()
	}
	small := measure(1000)
	large := measure(2000)
	// A copied growing string allocates about four times as much when the fragment
	// count doubles. Geometric builder growth stays near two times.
	if small <= 0 || large > small*3 {
		t.Fatalf("bytes/op small=%d large=%d", small, large)
	}
}

type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header         { return http.Header{} }
func (discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardResponseWriter) WriteHeader(int)             {}
func (discardResponseWriter) Flush()                      {}

func TestDecodeStreamAddedRefusalOrdering(t *testing.T) {
	t.Run("later added waits for earlier message", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"a","output_index":0,"content_index":0,"delta":"A"}`,
			`{"type":"response.content_part.added","item_id":"b","output_index":1,"content_index":0,"part":{"type":"refusal","refusal":"NO"}}`,
			`{"type":"response.output_item.done","item_id":"a","output_index":0,"item":{"id":"a","type":"message","content":[{"type":"output_text","text":"AB"}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		var deltas []ir.StreamEvent
		for _, ev := range evs {
			if ev.Kind == ir.EventTextDelta {
				deltas = append(deltas, ev)
			}
		}
		if len(deltas) != 3 || deltas[0].Text != "A" || deltas[0].Refusal || deltas[1].Text != "B" || deltas[1].Refusal || deltas[2].Text != "NO" || !deltas[2].Refusal {
			t.Fatalf("order = %+v", deltas)
		}
	})
	t.Run("completed snapshot keeps array order", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"a","output_index":0,"content_index":0,"delta":"A"}`,
			`{"type":"response.content_part.added","item_id":"b","output_index":1,"content_index":0,"part":{"type":"refusal","refusal":"NO"}}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"a","type":"message","content":[{"type":"output_text","text":"AB"}]},{"id":"b","type":"message","content":[{"type":"refusal","refusal":"NO"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := joinText(evs)
		if got != "ABNO" || n != 3 {
			t.Fatalf("joined = %q (%d) events=%+v", got, n, evs)
		}
	})
	t.Run("added stays open for a later delta", func(t *testing.T) {
		body := sseData(
			`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"N"}}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"O"}`,
			`{"type":"response.refusal.done","item_id":"m","output_index":0,"content_index":0,"refusal":"NO"}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("repeated added snapshot does not duplicate", func(t *testing.T) {
		body := sseData(
			`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"NO"}}`,
			`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"NO"}}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","content":[{"type":"refusal","refusal":"NO"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("pending budget rejects before retention", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"a","output_index":0,"content_index":0,"delta":"A"}`,
			`{"type":"response.content_part.added","item_id":"b","output_index":1,"content_index":0,"part":{"type":"refusal","refusal":"NO"}}`,
		)
		evs, err := decodeText(t, body, 4096, 4, len("NO")+textQueueEntryOverhead-1, 4)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v events=%+v", err, evs)
		}
		got, _ := joinText(evs)
		if strings.Contains(got, "NO") {
			t.Fatalf("rejected added content was emitted: %+v", evs)
		}
	})
}

func TestDecodeStreamPartialIndexMarkers(t *testing.T) {
	t.Run("indexed then loose fails closed", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"kept"}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"delta":"NO"}`,
		)
		evs, err := collectStream(t, body)
		if err == nil {
			t.Fatalf("mixed marker accepted: %+v", evs)
		}
		got, _ := joinText(evs)
		if strings.Contains(got, "NO") {
			t.Fatalf("misclassified bytes emitted: %+v", evs)
		}
	})
	t.Run("loose then indexed fails closed", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"delta":"NO"}`,
			`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"kept"}`,
		)
		evs, err := collectStream(t, body)
		if err == nil {
			t.Fatalf("mixed marker accepted: %+v", evs)
		}
		for _, ev := range evs {
			if ev.Kind == ir.EventTextDelta && ev.Text == "NO" && !ev.Refusal {
				t.Fatal("loose refusal lost its marker")
			}
			if ev.Kind == ir.EventTextDelta && ev.Text == "kept" {
				t.Fatal("indexed sibling emitted after conflict")
			}
		}
	})
	t.Run("same loose marker stays marked", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"delta":"N"}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"delta":"O"}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("snapshot marker conflict", func(t *testing.T) {
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"kept"}`,
			`{"type":"response.output_text.done","item_id":"m","output_index":0,"content_index":0,"text":"kept"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","content":[{"type":"refusal","refusal":"kept"}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err == nil {
			t.Fatalf("snapshot reclassified emitted text: %+v", evs)
		}
	})
}

func TestDecodeStreamLooseSnapshotMarker(t *testing.T) {
	loose := func(kind, delta string) string {
		return `{"type":"response.` + kind + `.delta","item_id":"m","output_index":0,"delta":` + strconv.Quote(delta) + `}`
	}
	final := func(status, part string) string {
		return `{"type":"response.` + status + `","response":{"id":"r","status":"` + status + `","output":[{"id":"m","type":"message","content":[` + part + `]}]}}`
	}
	itemDone := func(part string) string {
		return `{"type":"response.output_item.done","output_index":0,"item":{"id":"m","type":"message","content":[` + part + `]}}`
	}
	partDone := func(kind, field, value string) string {
		return `{"type":"response.content_part.done","item_id":"m","output_index":0,"content_index":0,"part":{"type":` + strconv.Quote(kind) + `,` + strconv.Quote(field) + `:` + strconv.Quote(value) + `}}`
	}
	wireDone := func(kind, field, value string) string {
		return `{"type":"response.` + kind + `.done","item_id":"m","output_index":0,"content_index":0,` + strconv.Quote(field) + `:` + strconv.Quote(value) + `}`
	}
	assertConflict := func(t *testing.T, frames ...string) {
		t.Helper()
		evs, err := collectStream(t, sseData(frames...))
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v events=%+v", err, evs)
		}
		for _, ev := range evs {
			if ev.Kind != ir.EventTextDelta || ev.Text == "" {
				continue
			}
			if !ev.Refusal && ev.Text != "NO" {
				t.Fatalf("conflicting text emitted unmarked: %+v", evs)
			}
			if ev.Refusal && ev.Text != "NO" {
				t.Fatalf("conflicting text emitted as refusal: %+v", evs)
			}
		}
	}
	for _, status := range []string{"completed", "incomplete"} {
		t.Run(status+" opposite snapshot extended", func(t *testing.T) {
			assertConflict(t, loose("refusal", "NO"), final(status, `{"type":"output_text","text":"NOPE"}`))
			assertConflict(t, loose("output_text", "NO"), final(status, `{"type":"refusal","refusal":"NOPE"}`))
		})
		t.Run(status+" opposite snapshot exact", func(t *testing.T) {
			assertConflict(t, loose("refusal", "NO"), final(status, `{"type":"output_text","text":"NO"}`))
			assertConflict(t, loose("output_text", "NO"), final(status, `{"type":"refusal","refusal":"NO"}`))
		})
		t.Run(status+" repeated opposite snapshot", func(t *testing.T) {
			assertConflict(t,
				loose("refusal", "NO"),
				final(status, `{"type":"output_text","text":"NO"}`),
				final(status, `{"type":"output_text","text":"NO"}`),
			)
		})
	}
	t.Run("part done and item done both directions", func(t *testing.T) {
		assertConflict(t, loose("refusal", "NO"), partDone("output_text", "text", "NOPE"))
		assertConflict(t, loose("output_text", "NO"), partDone("refusal", "refusal", "NOPE"))
		assertConflict(t, loose("refusal", "NO"), wireDone("output_text", "text", "NO"))
		assertConflict(t, loose("output_text", "NO"), wireDone("refusal", "refusal", "NO"))
		assertConflict(t, loose("refusal", "NO"), itemDone(`{"type":"output_text","text":"NOPE"}`))
		assertConflict(t, loose("output_text", "NO"), itemDone(`{"type":"refusal","refusal":"NO"}`))
	})
	t.Run("queued earlier message does not emit conflict", func(t *testing.T) {
		// Message a stays open, so the later loose prefix is queued. The opposite
		// snapshot must fail before that queued prefix or its recovered suffix emits.
		body := sseData(
			`{"type":"response.output_text.delta","item_id":"a","output_index":0,"content_index":0,"delta":"A"}`,
			`{"type":"response.refusal.delta","item_id":"m","output_index":1,"delta":"NO"}`,
			`{"type":"response.content_part.done","item_id":"m","output_index":1,"content_index":0,"part":{"type":"output_text","text":"NOPE"}}`,
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v events=%+v", err, evs)
		}
		for _, ev := range evs {
			if ev.Kind == ir.EventTextDelta && (ev.Text == "NO" || ev.Text == "PE" || ev.Text == "NOPE") {
				t.Fatalf("queued conflict emitted: %+v", evs)
			}
		}
	})
	t.Run("same marker terminal still recovers", func(t *testing.T) {
		body := sseData(
			loose("refusal", "NO"),
			final("completed", `{"type":"refusal","refusal":"NOPE"}`),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NOPE", ir.StopRefusal)
	})
	t.Run("ordinary loose prefix and refusal suffix stays split", func(t *testing.T) {
		body := sseData(
			loose("output_text", "NO"),
			final("completed", `{"type":"output_text","text":"NO"},{"type":"refusal","refusal":"PE"}`),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		var ordinary, marked string
		for _, ev := range evs {
			if ev.Kind != ir.EventTextDelta {
				continue
			}
			if ev.Refusal {
				marked += ev.Text
				continue
			}
			ordinary += ev.Text
		}
		if ordinary != "NO" || marked != "PE" {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("loose after part done keeps marker", func(t *testing.T) {
		body := sseData(
			loose("refusal", "N"),
			wireDone("refusal", "refusal", "N"),
			loose("refusal", "O"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("empty placeholder then loose deltas stay marked", func(t *testing.T) {
		body := sseData(
			`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`,
			loose("refusal", "N"),
			loose("refusal", "O"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("nonempty added then loose delta stays marked", func(t *testing.T) {
		body := sseData(
			`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"N"}}`,
			loose("refusal", "O"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
}

func TestDecodeStreamRefusalTerminalWithoutDone(t *testing.T) {
	t.Run("delta then eof", func(t *testing.T) {
		body := sseData(`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"NO"}`)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("completed omits output", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"NO"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed"}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("completed message omits content", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"NO"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message"}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopRefusal)
	})
	t.Run("empty added placeholder is not a refusal", func(t *testing.T) {
		body := sseData(`{"type":"response.content_part.added","item_id":"m","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""}}`)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Kind == ir.EventTextDelta || (ev.Kind == ir.EventFinish && ev.StopReason == ir.StopRefusal) {
				t.Fatalf("placeholder invented output: %+v", evs)
			}
		}
	})
	t.Run("explicit empty done remains a refusal", func(t *testing.T) {
		body := sseData(
			`{"type":"response.refusal.done","item_id":"m","output_index":0,"content_index":0,"refusal":""}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"m","type":"message","content":[{"type":"refusal","refusal":""}]}]}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		var saw bool
		var finish *ir.StreamEvent
		for i := range evs {
			if evs[i].Kind == ir.EventTextDelta && evs[i].Refusal && evs[i].Text == "" {
				saw = true
			}
			if evs[i].Kind == ir.EventFinish {
				finish = &evs[i]
			}
		}
		if !saw || finish == nil || finish.StopReason != ir.StopRefusal {
			t.Fatalf("events = %+v", evs)
		}
	})
	t.Run("tool and filter outrank a delta", func(t *testing.T) {
		tool := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"NO"}`,
			`{"type":"response.completed","response":{"id":"r","status":"completed","output":[{"id":"fc","type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}}`,
		)
		evs, err := collectStream(t, tool)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopToolUse)
		filter := sseData(
			`{"type":"response.refusal.delta","item_id":"m","output_index":0,"content_index":0,"delta":"NO"}`,
			`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`,
		)
		evs, err = collectStream(t, filter)
		if err != nil {
			t.Fatal(err)
		}
		assertRefusalText(t, evs, "NO", ir.StopContentFilter)
	})
}

func TestDecodeResponseTextAllocGrowth(t *testing.T) {
	measure := func(parts int) int64 {
		var b strings.Builder
		b.WriteString(`{"id":"r","status":"completed","output":[{"type":"message","content":[`)
		for i := 0; i < parts; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"type":"output_text","text":"12345678"}`)
		}
		b.WriteString(`]}]}`)
		body := []byte(b.String())
		result := testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := DecodeResponse(body); err != nil {
					b.Fatal(err)
				}
			}
		})
		return result.AllocedBytesPerOp()
	}
	small := measure(4000)
	large := measure(8000)
	// Quadratic concatenation is about four times when the part count doubles.
	// Builder growth stays near two times.
	if small <= 0 || large > small*3 {
		t.Fatalf("bytes/op small=%d large=%d", small, large)
	}
}

func assertRefusalText(t *testing.T, evs []ir.StreamEvent, want string, stop ir.StopReason) {
	t.Helper()
	got, n := joinText(evs)
	if got != want || n == 0 || !evsTextRefusal(evs) {
		t.Fatalf("text = %q (%d) events=%+v", got, n, evs)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventFinish && ev.StopReason != stop {
			t.Fatalf("finish = %+v", ev)
		}
	}
}
