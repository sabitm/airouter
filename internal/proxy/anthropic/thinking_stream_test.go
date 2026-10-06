package anthropic

import (
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func TestDecodeEncodeSignedThinkingStream(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":2}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"one"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" two"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"exact"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	want := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m", InputTokens: 2},
		{Kind: ir.EventReasoningStart, Index: 0},
		{Kind: ir.EventReasoningDelta, Index: 0, Text: "one", Indexed: true},
		{Kind: ir.EventReasoningDelta, Index: 0, Text: " two", Indexed: true},
		{Kind: ir.EventReasoningSignature, Index: 0, Signature: "sig-"},
		{Kind: ir.EventReasoningSignature, Index: 0, Signature: "exact"},
		{Kind: ir.EventReasoningEnd, Index: 0},
		{Kind: ir.EventTextDelta, Text: "answer"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, InputTokens: 2, OutputTokens: 3},
	}
	assertEvents(t, events, want)

	out := encodeStream(t, events)
	sigAt := strings.Index(out, `"signature":"sig-"`)
	sig2 := strings.Index(out, `"signature":"exact"`)
	stop := strings.Index(out, `"type":"content_block_stop"`)
	if sigAt < 0 || sig2 < sigAt || stop < sig2 {
		t.Fatalf("signature order wrong: %s", out)
	}
	if strings.Count(out, `"type":"thinking"`) != 1 {
		t.Fatalf("thinking blocks = %d, want 1: %s", strings.Count(out, `"type":"thinking"`), out)
	}
}

func TestDecodeEncodeAdjacentAndInterleavedThinking(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"only"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"next"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-b"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"search"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"redacted_thinking","data":"opaque"}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":4}`,
		`{"type":"message_stop"}`,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []ir.StreamEventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	if events[1].Kind != ir.EventReasoningStart || events[2].Kind != ir.EventReasoningSignature || events[2].Signature != "only" || events[3].Kind != ir.EventReasoningEnd {
		t.Fatalf("signature-only events = %+v", events[:4])
	}
	foundRedacted := false
	for _, ev := range events {
		if ev.Kind == ir.EventRedactedReasoning {
			foundRedacted = true
			if ev.Data != "opaque" || ev.Text != "" || ev.Index != 3 {
				t.Fatalf("redacted = %+v", ev)
			}
		}
	}
	if !foundRedacted {
		t.Fatalf("missing redacted event: %+v", events)
	}
	out := encodeStream(t, events)
	firstStop := strings.Index(out, `"type":"content_block_stop"`)
	only := strings.Index(out, `"signature":"only"`)
	if only < 0 || only > firstStop {
		t.Fatalf("signature-only not before its stop: %s", out)
	}
	if strings.Count(out, `"type":"thinking"`) != 2 {
		t.Fatalf("adjacent thinking merged: %s", out)
	}
	if !strings.Contains(out, `"type":"redacted_thinking"`) || !strings.Contains(out, `"data":"opaque"`) || strings.Contains(out, `"thinking":"opaque"`) {
		t.Fatalf("redacted wire wrong: %s", out)
	}
	if !strings.Contains(out, `"name":"search"`) || !strings.Contains(out, `"text":"done"`) {
		t.Fatalf("tool/text lost: %s", out)
	}
}

func TestLegacyReasoningDeltaStillEncodes(t *testing.T) {
	out := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventReasoningDelta, Text: "legacy"},
		{Kind: ir.EventTextDelta, Text: "answer"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	})
	if !strings.Contains(out, `"thinking":"legacy"`) || !strings.Contains(out, `"text":"answer"`) {
		t.Fatalf("legacy reasoning lost: %s", out)
	}
	if strings.Contains(out, "signature_delta") {
		t.Fatalf("legacy reasoning invented a signature: %s", out)
	}
}

func TestInvalidSignatureSequenceIsPayloadFree(t *testing.T) {
	body := anthropicSSE(
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"secret-signature"}}`,
	)
	_, err := collectDecode(t, body)
	if err == nil {
		t.Fatal("want protocol error")
	}
	if strings.Contains(err.Error(), "secret-signature") {
		t.Fatalf("error leaked signature: %v", err)
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
}

func TestEncoderRejectsSignatureBeforeStart(t *testing.T) {
	rec := encodeStreamMaybe(t, []ir.StreamEvent{{Kind: ir.EventReasoningSignature, Index: 0, Signature: "secret"}})
	if rec.err == nil {
		t.Fatal("want protocol error")
	}
	if strings.Contains(rec.err.Error(), "secret") || strings.Contains(rec.body, "secret") {
		t.Fatalf("signature leaked err=%v body=%s", rec.err, rec.body)
	}
}

func TestLegacyReasoningTextFinishStopsOnce(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventReasoningDelta, Text: "plan"},
		{Kind: ir.EventTextDelta, Text: "answer"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn, OutputTokens: 1},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	stops := 0
	for _, f := range frames {
		if f.Name == "content_block_stop" {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("stops = %d, want one reasoning stop and one text stop: %s", stops, body)
	}
	if strings.Count(body, `"type":"thinking"`) != 1 {
		t.Fatalf("thinking blocks = %s", body)
	}
}

func TestLegacyReasoningTextReasoningOpensNewBlock(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventReasoningDelta, Text: "first"},
		{Kind: ir.EventTextDelta, Text: "mid"},
		{Kind: ir.EventReasoningDelta, Text: "second"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	if strings.Count(body, `"type":"thinking"`) != 2 {
		t.Fatalf("second reasoning reused a closed block: %s", body)
	}
	first := strings.Index(body, `"thinking":"first"`)
	second := strings.Index(body, `"thinking":"second"`)
	if first < 0 || second < first {
		t.Fatalf("reasoning order wrong: %s", body)
	}
}

func TestInterleavedToolArgsStillReassemble(t *testing.T) {
	body := encodeStream(t, []ir.StreamEvent{
		{Kind: ir.EventToolCallStart, Index: 1, ToolID: "toolu_1", ToolName: "search"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `{"q":`},
		{Kind: ir.EventReasoningDelta, Text: "look"},
		{Kind: ir.EventTextDelta, Text: "ok"},
		{Kind: ir.EventToolCallDelta, Index: 1, ArgsFrag: `"x"}`},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	})
	frames := parseAnthropicStream(t, body)
	assertAnthropicWireOrder(t, frames)
	call := oneTool(t, reassembleAnthropicToolBlocks(t, frames))
	if call["name"] != "search" || call["args"] != `{"q":"x"}` {
		t.Fatalf("tool reassembly = %+v body=%s", call, body)
	}
}

func TestInitialThinkingTextAndSignatureRoundTrip(t *testing.T) {
	body := anthropicSSE(
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"seed","signature":"sig-start"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"-more"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if events[1].Kind != ir.EventReasoningStart || events[1].Text != "seed" || events[1].Signature != "sig-start" {
		t.Fatalf("start dropped initial payload: %+v", events[1])
	}
	out := encodeStream(t, events)
	frames := parseAnthropicStream(t, out)
	assertAnthropicWireOrder(t, frames)
	if !strings.Contains(out, `"thinking":"seed"`) || !strings.Contains(out, `"signature":"sig-start"`) || !strings.Contains(out, `"signature":"-more"`) {
		t.Fatalf("initial text or signature lost: %s", out)
	}
	if strings.Contains(out, `"thinking":""`) {
		t.Fatalf("encoder replaced initial thinking with empty text: %s", out)
	}
}

func TestThinkingSequenceErrorsArePayloadFree(t *testing.T) {
	secret := "secret-signature-value"
	cases := []string{
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"`+secret+`"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"`+secret+`"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thinking-secret"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`+secret+`"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"thinking-secret"}}`,
			`{"type":"message_stop"}`,
		),
	}
	for i, body := range cases {
		events, err := collectDecode(t, body)
		if err == nil {
			t.Fatalf("case %d decoded %+v", i, events)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "thinking-secret") {
			t.Fatalf("case %d leaked payload: %v", i, err)
		}
		for _, ev := range events {
			if ev.Kind == ir.EventFinish {
				t.Fatalf("case %d fabricated finish: %+v", i, events)
			}
		}
	}
}

func TestEncoderRejectsClosedThinkingDeltaAndDuplicateStop(t *testing.T) {
	secret := "closed-secret"
	rec := encodeStreamMaybe(t, []ir.StreamEvent{
		{Kind: ir.EventReasoningStart, Index: 0, Text: "seed"},
		{Kind: ir.EventReasoningEnd, Index: 0},
		{Kind: ir.EventReasoningDelta, Index: 0, Text: secret, Indexed: true},
	})
	if rec.err == nil || strings.Contains(rec.err.Error(), secret) || strings.Contains(rec.body, secret) {
		t.Fatalf("closed delta leaked or was accepted: err=%v body=%s", rec.err, rec.body)
	}
	rec = encodeStreamMaybe(t, []ir.StreamEvent{
		{Kind: ir.EventReasoningStart, Index: 0},
		{Kind: ir.EventReasoningEnd, Index: 0},
		{Kind: ir.EventReasoningEnd, Index: 0},
	})
	if rec.err == nil {
		t.Fatal("duplicate stop was accepted")
	}
	frames := parseAnthropicStream(t, rec.body)
	assertAnthropicWireOrder(t, frames)
	stops := 0
	for _, f := range frames {
		if f.Name == "content_block_stop" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("stops = %d, want 1: %s", stops, rec.body)
	}
}

func TestOverlappingThinkingIndexesAreRejected(t *testing.T) {
	secret := "overlap-secret"
	body := anthropicSSE(
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":"`+secret+`"}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"next"}}`,
	)
	events, err := collectDecode(t, body)
	if err == nil {
		t.Fatal("overlapping thinking indexes were accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked payload: %v", err)
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if len(events) != 1 || events[0].Kind != ir.EventReasoningStart || events[0].Index != 0 {
		t.Fatalf("conflicting start was emitted: %+v", events)
	}
}

func TestOpenThinkingRejectsConflictingStartsAndDeltas(t *testing.T) {
	secret := "transition-secret"
	cases := []string{
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"`+secret+`"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":4,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_start","index":7,"content_block":{"type":"tool_use","id":"toolu_1","name":"search"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_start","index":3,"content_block":{"type":"redacted_thinking","data":"`+secret+`"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"`+secret+`"}}`,
		),
		anthropicSSE(
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"`+secret+`"}}`,
		),
	}
	for i, body := range cases {
		events, err := collectDecode(t, body)
		if err == nil {
			t.Fatalf("case %d accepted conflicting transition: %+v", i, events)
		}
		assertPayloadFreeSequenceError(t, err, events, secret)
		for _, ev := range events {
			if ev.Kind != ir.EventReasoningStart {
				t.Fatalf("case %d emitted conflicting event %+v", i, ev)
			}
		}
	}
}

func TestThinkingStopFreesNextExplicitBlock(t *testing.T) {
	body := anthropicSSE(
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"mid"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":5,"content_block":{"type":"thinking","thinking":"later"}}`,
		`{"type":"content_block_delta","index":5,"delta":{"type":"signature_delta","signature":"sig-later"}}`,
		`{"type":"content_block_stop","index":5}`,
		`{"type":"message_stop"}`,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Index != 0 || events[1].Kind != ir.EventReasoningEnd || events[2].Text != "mid" || events[3].Index != 5 || events[3].Text != "later" {
		t.Fatalf("gapped sequence = %+v", events)
	}
}

func TestEncoderRejectsExplicitThinkingConflicts(t *testing.T) {
	secret := "encoder-secret"
	cases := [][]ir.StreamEvent{
		{
			{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m"},
			{Kind: ir.EventReasoningStart, Index: 0},
			{Kind: ir.EventReasoningStart, Index: 3, Text: "next"},
		},
		{
			{Kind: ir.EventMessageStart, ID: "msg_1", Model: "m"},
			{Kind: ir.EventReasoningStart, Index: 1},
			{Kind: ir.EventTextDelta, Text: secret},
		},
		{
			{Kind: ir.EventReasoningStart, Index: 1},
			{Kind: ir.EventToolCallStart, Index: 4, ToolID: "toolu_1", ToolName: "search"},
		},
		{
			{Kind: ir.EventReasoningStart, Index: 1},
			{Kind: ir.EventToolCallDelta, Index: 4, ArgsFrag: secret},
		},
		{
			{Kind: ir.EventReasoningStart, Index: 2},
			{Kind: ir.EventRedactedReasoning, Index: 8, Data: secret},
		},
		{
			{Kind: ir.EventReasoningStart, Index: 2, Text: "open"},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		},
	}
	for i, events := range cases {
		rec := encodeStreamMaybe(t, events)
		if rec.err == nil {
			t.Fatalf("case %d accepted conflict: %s", i, rec.body)
		}
		if strings.Contains(rec.err.Error(), secret) || strings.Contains(rec.body, secret) {
			t.Fatalf("case %d leaked payload err=%v body=%s", i, rec.err, rec.body)
		}
		if strings.Contains(rec.body, `"type":"message_stop"`) || strings.Count(rec.body, `"type":"content_block_stop"`) != 0 {
			t.Fatalf("case %d fabricated a stop: %s", i, rec.body)
		}
	}
}

func TestEncoderCloseDoesNotFabricateExplicitStop(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 0, Text: "open"}, w); err != nil {
		t.Fatal(err)
	}
	err := enc.Close(w)
	if err == nil || strings.Contains(rec.Body.String(), `"type":"message_stop"`) || strings.Contains(rec.Body.String(), `"type":"content_block_stop"`) {
		t.Fatalf("close fabricated terminal err=%v body=%s", err, rec.Body.String())
	}
}

func TestCompletedThinkingDoesNotGrowEncoderState(t *testing.T) {
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for i := 0; i < 32; i++ {
		events := []ir.StreamEvent{
			{Kind: ir.EventReasoningStart, Index: i, Text: "t"},
			{Kind: ir.EventReasoningSignature, Index: i, Signature: "s"},
			{Kind: ir.EventReasoningEnd, Index: i},
		}
		for _, ev := range events {
			if err := enc.Encode(ev, w); err != nil {
				t.Fatal(err)
			}
		}
		if len(enc.thinking) != 0 {
			t.Fatalf("completed block retained at %d: %d", i, len(enc.thinking))
		}
	}
	if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningStart, Index: 3}, w); err == nil {
		t.Fatal("reused source index was accepted")
	}
	frames := parseAnthropicStream(t, rec.Body.String())
	assertAnthropicWireOrder(t, frames)
}

func anthropicSSE(datas ...string) string {
	var b strings.Builder
	for _, data := range datas {
		name := "message"
		switch {
		case strings.Contains(data, `"content_block_start"`):
			name = "content_block_start"
		case strings.Contains(data, `"content_block_delta"`):
			name = "content_block_delta"
		case strings.Contains(data, `"content_block_stop"`):
			name = "content_block_stop"
		case strings.Contains(data, `"message_start"`):
			name = "message_start"
		case strings.Contains(data, `"message_delta"`):
			name = "message_delta"
		case strings.Contains(data, `"message_stop"`):
			name = "message_stop"
		}
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteString("\ndata: ")
		b.WriteString(data)
		b.WriteString("\n\n")
	}
	return b.String()
}

func assertPayloadFreeSequenceError(t *testing.T, err error, events []ir.StreamEvent, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("want protocol error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked payload: %v", err)
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventFinish || strings.Contains(ev.Text, secret) || strings.Contains(ev.Signature, secret) || strings.Contains(ev.Data, secret) {
			t.Fatalf("emitted conflicting payload: %+v", ev)
		}
	}
}

func assertEvents(t *testing.T, got, want []ir.StreamEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Text != want[i].Text || got[i].Signature != want[i].Signature ||
			got[i].Index != want[i].Index || got[i].Indexed != want[i].Indexed || got[i].Data != want[i].Data {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

type encodeResult struct {
	body string
	err  error
}

func encodeStreamMaybe(t *testing.T, events []ir.StreamEvent) encodeResult {
	t.Helper()
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	var err error
	for _, ev := range events {
		if err = enc.Encode(ev, w); err != nil {
			break
		}
	}
	return encodeResult{body: rec.Body.String(), err: err}
}
