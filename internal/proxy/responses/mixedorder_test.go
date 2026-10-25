package responses

import (
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// irOrderTokens renders the ordering-relevant IR event sequence so a test can
// assert the actual emission order, not only the final object.
func irOrderTokens(evs []ir.StreamEvent) []string {
	var out []string
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventTextDelta:
			out = append(out, "text:"+ev.Text)
		case ir.EventToolCallStart:
			out = append(out, "toolstart")
		case ir.EventToolCallDelta:
			out = append(out, "toolargs:"+ev.ArgsFrag)
		case ir.EventReasoningDelta:
			out = append(out, "reason:"+ev.Text)
		case ir.EventReasoningItemDone:
			out = append(out, "reasondone")
		}
	}
	return out
}

func assertIRTokens(t *testing.T, evs []ir.StreamEvent, want ...string) {
	t.Helper()
	got := irOrderTokens(evs)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("IR order = %q, want %q", got, want)
	}
}

func assertEncodedOutputTypes(t *testing.T, body string, want ...string) {
	t.Helper()
	final := finalResponseObject(t, body)
	output, _ := final["output"].([]any)
	if len(output) != len(want) {
		t.Fatalf("encoded output = %+v, want %d items", output, len(want))
	}
	for i, o := range output {
		m, _ := o.(map[string]any)
		if got, _ := m["type"].(string); got != want[i] {
			t.Fatalf("encoded output[%d] type = %q, want %q", i, got, want[i])
		}
	}
}

// TestDecodeStreamMixedToolOrderBehindMessage reproduces the PART C blocker: a
// streamed message retains a recoverable suffix while a later function call
// emits immediately, so the recovered suffix lands after the tool. The IR and
// the re-encoded Responses output must both keep the message ahead of the call.
func TestDecodeStreamMixedToolOrderBehindMessage(t *testing.T) {
	full := `{"path":"/x"}`
	body := sseData(
		sseTextDelta("msg_a", false, 0, "hel"),
		addedEvent(1, "fc_1", "call_1", "read", ""),
		deltaEvent(1, "fc_1", `{"path":`),
		deltaEvent(1, "fc_1", `"/x"}`),
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "hello"),
			completedItem("fc_1", "call_1", "read", full),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs,
		"text:hel", "text:lo", "toolstart", `toolargs:{"path":`, `toolargs:"/x"}`)

	_, wire, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertEncodedOutputTypes(t, wire, "message", "function_call")
	msgs := finalMessageTexts(t, wire)
	if len(msgs) != 1 || msgs[0] != "hello" {
		t.Fatalf("encoded message text = %v, want hello before function_call", msgs)
	}
}

// TestDecodeStreamMixedReasoningOrderBehindMessage keeps a streamed reasoning
// item behind an unresolved message. The recovered message suffix must precede
// the reasoning summary, and the single reasoning done must keep its identity,
// cipher, and summary text.
func TestDecodeStreamMixedReasoningOrderBehindMessage(t *testing.T) {
	enc := "cipher"
	body := sseData(
		sseTextDelta("msg_a", false, 0, "hel"),
		reasoningAddedEvent(1, "rs_1", "in_progress"),
		reasoningDeltaEvent(1, "rs_1", 0, "think"),
		reasoningItemDoneEvent(1, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"think"}]`),
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "hello"),
			reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"think"}]`),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs, "text:hel", "text:lo", "reason:think", "reasondone")
	dones := 0
	for _, ev := range evs {
		if ev.Kind != ir.EventReasoningItemDone {
			continue
		}
		dones++
		if ev.Reasoning == nil || ev.Reasoning.ID != "rs_1" {
			t.Fatalf("reasoning done identity = %+v", ev.Reasoning)
		}
		if ev.Reasoning.EncryptedContent == nil || *ev.Reasoning.EncryptedContent != enc {
			t.Fatalf("reasoning done cipher = %+v", ev.Reasoning.EncryptedContent)
		}
		if len(ev.Reasoning.Summary) != 1 || ev.Reasoning.Summary[0].Text != "think" {
			t.Fatalf("reasoning done summary = %+v", ev.Reasoning.Summary)
		}
	}
	if dones != 1 {
		t.Fatalf("reasoning done count = %d, want 1", dones)
	}
}

// TestDecodeStreamMixedInterleavedDrain queues a later message, tool, and
// reasoning behind an open earlier message, then recovers the earlier suffix
// and drains all deferred work in arrival order without duplicates.
func TestDecodeStreamMixedInterleavedDrain(t *testing.T) {
	enc := "cipher"
	body := sseData(
		sseTextDelta("msg_a", false, 0, "A"),
		sseTextDelta("msg_b", false, 1, "B"),
		addedEvent(2, "fc_1", "call_1", "read", `{}`),
		reasoningAddedEvent(3, "rs_1", "in_progress"),
		reasoningDeltaEvent(3, "rs_1", 0, "R"),
		`{"type":"response.output_item.done","output_index":0,"item":`+sseMsgFinalItem("msg_a", "Aa")+`}`,
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "Aa"),
			sseMsgFinalItem("msg_b", "B"),
			completedItem("fc_1", "call_1", "read", `{}`),
			reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"R"}]`),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs,
		"text:A", "text:a", "text:B", "toolstart", `toolargs:{}`, "reason:R", "reasondone")

	// A repeated identical completion must not duplicate any item.
	repeated := body + sseData(completedEvent(finalOutput(
		sseMsgFinalItem("msg_a", "Aa"),
		sseMsgFinalItem("msg_b", "B"),
		completedItem("fc_1", "call_1", "read", `{}`),
		reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"R"}]`),
	), ""))
	evs2, err := collectStream(t, repeated)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, tok := range irOrderTokens(evs2) {
		counts[tok]++
	}
	if counts["text:A"] != 1 || counts["text:a"] != 1 || counts["text:B"] != 1 ||
		counts["toolstart"] != 1 || counts["reasondone"] != 1 || counts["reason:R"] != 1 {
		t.Fatalf("repeated completion duplicated content: %v", counts)
	}
}

// TestDecodeStreamMixedEOFPreservesContent checks that an incomplete stream
// still emits every arrived item exactly once in arrival order, with the tool
// before and after message text both preserved.
func TestDecodeStreamMixedEOFPreservesContent(t *testing.T) {
	t.Run("tool after message", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hi"),
			addedEvent(1, "fc_1", "call_1", "read", `{"a":1}`),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertIRTokens(t, evs, "text:hi", "toolstart", "toolargs:{\"a\":1}")
	})
	t.Run("tool before message", func(t *testing.T) {
		body := sseData(
			addedEvent(0, "fc_1", "call_1", "read", `{"a":1}`),
			sseTextDelta("msg_a", false, 1, "hi"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertIRTokens(t, evs, "toolstart", "toolargs:{\"a\":1}", "text:hi")
	})
}

// TestDecodeStreamMixedPendingOverlimit injects the combined pending budget and
// checks that tool arguments and reasoning payloads are charged against it. The
// offending event must not be emitted or retained.
func TestDecodeStreamMixedPendingOverlimit(t *testing.T) {
	t.Run("tool args charged", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "a"),
			addedEvent(1, "fc_1", "call_1", "read", ""),
			deltaEvent(1, "fc_1", strings.Repeat("x", 64)),
		)
		evs, err := decodeText(t, body, 4096, 8, 32, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v, want pending limit", err)
		}
		for _, ev := range evs {
			if strings.Contains(ev.ArgsFrag, "xxxx") {
				t.Fatalf("offending tool args emitted: %+v", ev)
			}
		}
	})
	t.Run("reasoning charged", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "a"),
			reasoningAddedEvent(1, "rs_1", "in_progress"),
			reasoningDeltaEvent(1, "rs_1", 0, strings.Repeat("y", 64)),
		)
		evs, err := decodeText(t, body, 4096, 8, 32, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v, want pending limit", err)
		}
		for _, ev := range evs {
			if ev.Kind == ir.EventReasoningDelta && strings.Contains(ev.Text, "yyy") {
				t.Fatalf("offending reasoning delta emitted: %+v", ev)
			}
		}
	})
}

// finalMessageTexts returns every message content text in wire order.
func finalMessageTexts(t *testing.T, body string) []string {
	t.Helper()
	final := finalResponseObject(t, body)
	output, _ := final["output"].([]any)
	var out []string
	for _, o := range output {
		m, _ := o.(map[string]any)
		if m["type"] != "message" {
			continue
		}
		out = append(out, queuedSummaryTextOfContent(t, m)...)
	}
	return out
}

// TestDecodeStreamMixedPartDoneKeepsLaterQueued shows that a content-part done
// is not a message terminal: later message work stays queued until the whole
// message closes, then releases.
func TestDecodeStreamMixedPartDoneKeepsLaterQueued(t *testing.T) {
	var out []ir.StreamEvent
	d := newTextDecoder(func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_a", false, 0, "p1")), false); err != nil {
		t.Fatal(err)
	}
	// The barrier message's own delta is live, not buffered.
	if len(out) == 0 || out[len(out)-1].Kind != ir.EventTextDelta || out[len(out)-1].Text != "p1" {
		t.Fatalf("barrier delta not live: %+v", out)
	}
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_b", false, 1, "B")), false); err != nil {
		t.Fatal(err)
	}
	part := &reasoningSummary{Type: "output_text", Text: "p1", TextSet: true}
	if err := d.observePartDone(&streamEnvelope{ItemID: "msg_a", ContentIndexSet: true, ContentIndex: 0, Part: part}); err != nil {
		t.Fatal(err)
	}
	if len(d.textQueue) == 0 || d.textQueueEvents == 0 {
		t.Fatal("part done released later message work prematurely")
	}
	if err := d.observeMessageDone(&streamEnvelope{OutputIndex: 0, OutputIndexSet: true, Item: &streamItem{ID: "msg_a", Type: "message", Content: []respOutputContent{{Type: "output_text", Text: "p1"}}}}); err != nil {
		t.Fatal(err)
	}
	if len(d.textQueue) != 0 || d.textQueueEvents != 0 {
		t.Fatalf("queue not released after message done: %d", len(d.textQueue))
	}
	var b strings.Builder
	for _, ev := range out {
		if ev.Kind == ir.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	if b.String() != "p1B" {
		t.Fatalf("deltas = %q, want p1B", b.String())
	}
}

// TestDecodeStreamMixedReasoningLateID keeps a reasoning item that is queued
// behind a message before its source ID is known. The recovered message suffix
// must still precede the reasoning delta and the single done, which carries the
// late identity.
func TestDecodeStreamMixedReasoningLateID(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", false, 0, "a"),
		reasoningAddedEvent(1, "", "in_progress"),
		reasoningDeltaEvent(1, "", 0, "R"),
		reasoningItemDoneEvent(1, "rs_late", "completed", nil, `[{"type":"summary_text","text":"R"}]`),
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "ab"),
			reasoningFinalItem("rs_late", "completed", nil, `[{"type":"summary_text","text":"R"}]`),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs, "text:a", "text:b", "reason:R", "reasondone")
	dones := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			dones++
			if ev.Reasoning == nil || ev.Reasoning.ID != "rs_late" {
				t.Fatalf("late reasoning id = %+v", ev.Reasoning)
			}
		}
	}
	if dones != 1 {
		t.Fatalf("reasoning done count = %d, want 1", dones)
	}
}

// TestDecodeStreamMixedReorderConflict checks that a final-only item at an
// earlier array ordinal cannot be emitted before an already-wired streamed item
// at a later ordinal. The provider order is unrecoverable, so it must fail.
func TestDecodeStreamMixedReorderConflict(t *testing.T) {
	t.Run("final-only tool before wired tool", func(t *testing.T) {
		body := sseData(
			addedEvent(5, "fc_1", "call_1", "alpha", `{}`),
			completedEvent(finalOutput(
				completedItem("fc_2", "call_2", "beta", `{}`),
				completedItem("fc_1", "call_1", "alpha", `{}`),
			), ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting function call order") {
			t.Fatalf("err = %v, want conflicting function call order", err)
		}
	})
	t.Run("normal tool order succeeds", func(t *testing.T) {
		body := sseData(
			addedEvent(5, "fc_1", "call_1", "alpha", `{}`),
			completedEvent(finalOutput(
				completedItem("fc_1", "call_1", "alpha", `{}`),
				completedItem("fc_2", "call_2", "beta", `{}`),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertIRTokens(t, evs, "toolstart", "toolargs:{}", "toolstart", "toolargs:{}")
	})
}

// TestDecodeStreamMixedPendingExactCost injects the combined pending byte
// budget at the exact cost of a queued reasoning delta plus its queued done
// event, including the encrypted cipher, the summary type, and the per-part
// slice overhead.
func TestDecodeStreamMixedPendingExactCost(t *testing.T) {
	enc := "cipher"
	summary := []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "R"}}
	deltaCost := pendingOrderEventBytes(ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", Text: "R"})
	doneCost := pendingOrderEventBytes(ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
		ID: "rs_1", Status: "completed", EncryptedContent: &enc, Summary: summary,
	}})
	total := deltaCost + doneCost
	body := sseData(
		sseTextDelta("msg_a", false, 0, "a"),
		reasoningItemDoneEvent(1, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"R"}]`),
	)
	t.Run("exact budget succeeds", func(t *testing.T) {
		if _, err := decodeText(t, body, 4096, 8, total, maxPendingTextEvents); err != nil {
			t.Fatalf("exact budget failed: %v", err)
		}
	})
	t.Run("one byte under fails without emitting", func(t *testing.T) {
		evs, err := decodeText(t, body, 4096, 8, total-1, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v, want pending limit", err)
		}
		for _, ev := range evs {
			if ev.Kind == ir.EventReasoningItemDone {
				t.Fatalf("offending reasoning done emitted: %+v", ev)
			}
		}
	})
}

// TestDecodeStreamMixedPendingEventCount checks that tool and reasoning events
// share the event count cap and that a rejected event is not emitted.
func TestDecodeStreamMixedPendingEventCount(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", false, 0, "a"),
		reasoningDeltaEvent(1, "rs_1", 0, "R1"),
		reasoningDeltaEvent(1, "rs_1", 0, "R2"),
	)
	evs, err := decodeText(t, body, 4096, 8, maxPendingTextBytes, 1)
	if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
		t.Fatalf("err = %v, want pending limit", err)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta && strings.Contains(ev.Text, "R2") {
			t.Fatalf("offending reasoning delta emitted: %+v", ev)
		}
	}
}

// TestDecodeStreamMixedQueueReleased checks that a mixed queue holds zero
// references and counters once the blocking message closes.
func TestDecodeStreamMixedQueueReleased(t *testing.T) {
	var out []ir.StreamEvent
	d := newTextDecoder(func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
	d.maxArgBytes = maxRetainedToolArgBytes
	d.maxCalls = maxTrackedToolCalls
	d.reasoning.orderEmit = d.orderEmitReasoning
	d.reasoning.msgCount = func() int { return len(d.texts) }
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_a", false, 0, "a")), false); err != nil {
		t.Fatal(err)
	}
	if err := d.reasoning.observeDelta(textDeltaEnv(t, reasoningDeltaEvent(1, "rs_1", 0, "R"))); err != nil {
		t.Fatal(err)
	}
	if d.queueBytes == 0 || d.textQueueEvents == 0 {
		t.Fatal("mixed queue was not charged")
	}
	if err := d.observeMessageDone(&streamEnvelope{OutputIndex: 0, OutputIndexSet: true, Item: &streamItem{ID: "msg_a", Type: "message", Content: []respOutputContent{{Type: "output_text", Text: "a"}}}}); err != nil {
		t.Fatal(err)
	}
	if d.queueBytes != 0 || len(d.textQueue) != 0 || d.textQueueEvents != 0 {
		t.Fatalf("queue not released: bytes=%d queue=%d events=%d", d.queueBytes, len(d.textQueue), d.textQueueEvents)
	}
	sawReason := false
	for _, ev := range out {
		if ev.Kind == ir.EventReasoningDelta && ev.Text == "R" {
			sawReason = true
		}
	}
	if !sawReason {
		t.Fatalf("released reasoning delta missing: %+v", out)
	}
}

// TestDecodeStreamFinalOrderingGuard covers the unified terminal-array guard:
// any slot with new ordering work must not be emitted after an item that
// already reached the client owns a later array ordinal. The check spans text,
// tool, and reasoning because their terminal ordinals share one output order.
// The offending frames must not be emitted and no Finish may follow.
func TestDecodeStreamFinalOrderingGuard(t *testing.T) {
	assertNoFinish := func(t *testing.T, evs []ir.StreamEvent, offending func(ir.StreamEvent) bool, label string) {
		t.Helper()
		for _, ev := range evs {
			if ev.Kind == ir.EventFinish {
				t.Fatalf("%s: finish emitted after reorder: %+v", label, ev)
			}
			if offending(ev) {
				t.Fatalf("%s: offending frame emitted: %+v", label, ev)
			}
		}
	}
	isToolFrame := func(ev ir.StreamEvent) bool {
		return ev.Kind == ir.EventToolCallStart || ev.Kind == ir.EventToolCallDelta
	}
	isReasonFrame := func(ev ir.StreamEvent) bool {
		return ev.Kind == ir.EventReasoningDelta || ev.Kind == ir.EventReasoningItemDone
	}
	isTextFrame := func(ev ir.StreamEvent) bool {
		return ev.Kind == ir.EventTextDelta
	}

	t.Run("final-only tool before emitted text", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hello"),
			completedEvent(finalOutput(
				completedItem("fc_1", "call_1", "read", `{}`),
				sseMsgFinalItem("msg_a", "hello"),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
			t.Fatalf("err = %v, want conflicting message text order", err)
		}
		assertNoFinish(t, evs, isToolFrame, "tool before text")
	})

	t.Run("reasoning final before emitted text", func(t *testing.T) {
		enc := "cipher"
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hello"),
			completedEvent(finalOutput(
				reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"R"}]`),
				sseMsgFinalItem("msg_a", "hello"),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
			t.Fatalf("err = %v, want conflicting message text order", err)
		}
		assertNoFinish(t, evs, isReasonFrame, "reasoning before text")
	})

	t.Run("final-only message before emitted tool", func(t *testing.T) {
		body := sseData(
			addedEvent(5, "fc_1", "call_1", "read", `{}`),
			completedEvent(finalOutput(
				sseMsgFinalItem("msg_f", "pre"),
				completedItem("fc_1", "call_1", "read", `{}`),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting function call order") {
			t.Fatalf("err = %v, want conflicting function call order", err)
		}
		assertNoFinish(t, evs, isTextFrame, "message before tool")
	})

	t.Run("final-only message before emitted reasoning", func(t *testing.T) {
		body := sseData(
			reasoningAddedEvent(5, "rs_1", "in_progress"),
			reasoningDeltaEvent(5, "rs_1", 0, "R"),
			completedEvent(finalOutput(
				sseMsgFinalItem("msg_f", "pre"),
				reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"R"}]`),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting reasoning item order") {
			t.Fatalf("err = %v, want conflicting reasoning item order", err)
		}
		assertNoFinish(t, evs, isTextFrame, "message before reasoning")
	})

	t.Run("reasoning before emitted tool", func(t *testing.T) {
		body := sseData(
			addedEvent(5, "fc_1", "call_1", "read", `{}`),
			completedEvent(finalOutput(
				reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"R"}]`),
				completedItem("fc_1", "call_1", "read", `{}`),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting function call order") {
			t.Fatalf("err = %v, want conflicting function call order", err)
		}
		assertNoFinish(t, evs, isReasonFrame, "reasoning before tool")
	})

	t.Run("final-only tool before emitted reasoning", func(t *testing.T) {
		body := sseData(
			reasoningAddedEvent(5, "rs_1", "in_progress"),
			reasoningDeltaEvent(5, "rs_1", 0, "R"),
			completedEvent(finalOutput(
				completedItem("fc_1", "call_1", "read", `{}`),
				reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"R"}]`),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting reasoning item order") {
			t.Fatalf("err = %v, want conflicting reasoning item order", err)
		}
		assertNoFinish(t, evs, isToolFrame, "tool before reasoning")
	})

	t.Run("repeated terminal consistent snapshot is free", func(t *testing.T) {
		enc := "cipher"
		final := func() string {
			return completedEvent(finalOutput(
				completedItem("fc_1", "call_1", "read", `{}`),
				reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"R"}]`),
				sseMsgFinalItem("msg_a", "A"),
			), "")
		}
		body := sseData(final(), final())
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, tok := range irOrderTokens(evs) {
			counts[tok]++
		}
		if counts["toolstart"] != 1 || counts["toolargs:{}"] != 1 ||
			counts["reason:R"] != 1 || counts["reasondone"] != 1 || counts["text:A"] != 1 {
			t.Fatalf("consistent repeat duplicated content: %v", counts)
		}
	})

	t.Run("queued later work at final slots keeps array order", func(t *testing.T) {
		enc := "cipher"
		body := sseData(
			sseTextDelta("msg_a", false, 0, "A"),
			sseTextDelta("msg_b", false, 1, "B"),
			reasoningAddedEvent(2, "rs_1", "in_progress"),
			reasoningDeltaEvent(2, "rs_1", 0, "R"),
			addedEvent(3, "fc_1", "call_1", "read", `{}`),
			completedEvent(finalOutput(
				sseMsgFinalItem("msg_a", "A"),
				sseMsgFinalItem("msg_b", "B"),
				reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"R"}]`),
				completedItem("fc_1", "call_1", "read", `{}`),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		assertIRTokens(t, evs,
			"text:A", "text:B", "reason:R", "reasondone", "toolstart", `toolargs:{}`)
	})
}

func countKind(evs []ir.StreamEvent, kind ir.StreamEventKind) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func reasoningDone(t *testing.T, evs []ir.StreamEvent, id string) ir.StreamEvent {
	t.Helper()
	var found *ir.StreamEvent
	for i := range evs {
		ev := &evs[i]
		if ev.Kind != ir.EventReasoningItemDone || ev.ReasoningID != id {
			continue
		}
		if found != nil {
			t.Fatalf("duplicate reasoning done for %s", id)
		}
		found = ev
	}
	if found == nil {
		t.Fatalf("missing reasoning done for %s", id)
	}
	return *found
}

func assertSummaryTexts(t *testing.T, ev ir.StreamEvent, want ...string) {
	t.Helper()
	if ev.Reasoning == nil {
		t.Fatalf("reasoning snapshot missing: %+v", ev)
	}
	got := make([]string, 0, len(ev.Reasoning.Summary))
	for _, part := range ev.Reasoning.Summary {
		got = append(got, part.Text)
	}
	if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
}

func decodeEncodeFinal(t *testing.T, body string) (map[string]any, []string) {
	t.Helper()
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := encodeStreamEventsErr(t, evs)
	if err != nil {
		t.Fatal(err)
	}
	return finalResponseObject(t, stream), sseEventNames(t, stream)
}

func reasoningOutput(t *testing.T, final map[string]any, index int) map[string]any {
	t.Helper()
	output, _ := final["output"].([]any)
	if index >= len(output) {
		t.Fatalf("output len %d, want index %d: %+v", len(output), index, output)
	}
	item, _ := output[index].(map[string]any)
	if item["type"] != "reasoning" {
		t.Fatalf("output[%d] = %+v", index, item)
	}
	return item
}

// TestDecodeStreamLateReasoningMetadataAfterText keeps a wired reasoning item
// that already emitted every readable summary byte when the only terminal
// snapshot arrives after later keyed text. Missing item.done is not new output
// order, so the final encrypted state must survive and re-encode once.
func TestDecodeStreamLateReasoningMetadataAfterText(t *testing.T) {
	enc := "cipher"
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		sseTextDelta("msg_a", true, 1, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"},{"type":"summary_text","text":""}]`),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs, "reason:chain", "text:answer", "reasondone")
	if countKind(evs, ir.EventTextDelta) != 1 || countKind(evs, ir.EventReasoningDelta) != 1 || countKind(evs, ir.EventReasoningItemDone) != 1 {
		t.Fatalf("duplicated frames: %+v", irOrderTokens(evs))
	}
	done := reasoningDone(t, evs, "rs_1")
	if done.Reasoning == nil || done.Reasoning.ID != "rs_1" || done.Reasoning.EncryptedContent == nil || *done.Reasoning.EncryptedContent != enc {
		t.Fatalf("late metadata = %+v", done.Reasoning)
	}
	assertSummaryTexts(t, done, "chain", "")

	final, names := decodeEncodeFinal(t, body)
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("encoded output = %+v names=%v", output, names)
	}
	item := reasoningOutput(t, final, 0)
	if item["id"] != "rs_1" || item["encrypted_content"] != enc || item["status"] != "completed" {
		t.Fatalf("final reasoning = %+v", item)
	}
	summary, _ := item["summary"].([]any)
	if len(summary) != 2 {
		t.Fatalf("final summary = %+v", summary)
	}
	first, _ := summary[0].(map[string]any)
	second, _ := summary[1].(map[string]any)
	if first["text"] != "chain" || second["text"] != "" {
		t.Fatalf("final summary texts = %+v", summary)
	}
	msg, _ := output[1].(map[string]any)
	if msg["type"] != "message" || msg["id"] == "" {
		t.Fatalf("final message = %+v", msg)
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("message content = %+v", content)
	}
	part, _ := content[0].(map[string]any)
	if part["text"] != "answer" {
		t.Fatalf("message text = %+v", part)
	}
	if strings.Count(strings.Join(names, "\n"), "response.output_item.done") != 2 {
		t.Fatalf("item done count in %v", names)
	}
}

func TestDecodeStreamReasoningDoneBeforeTextKeepsLateCipher(t *testing.T) {
	enc := "cipher"
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"chain"}]`),
		sseTextDelta("msg_a", true, 1, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"}]`),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs, "reason:chain", "reasondone", "text:answer", "reasondone")
	var dones []ir.StreamEvent
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			dones = append(dones, ev)
		}
	}
	if len(dones) != 2 || dones[0].Reasoning == nil || dones[0].Reasoning.EncryptedContent != nil {
		t.Fatalf("first done = %+v", dones)
	}
	if dones[1].Reasoning == nil || dones[1].Reasoning.EncryptedContent == nil || *dones[1].Reasoning.EncryptedContent != enc {
		t.Fatalf("late cipher dropped: %+v", dones[1].Reasoning)
	}
	if countKind(evs, ir.EventTextDelta) != 1 || countKind(evs, ir.EventReasoningDelta) != 1 {
		t.Fatalf("readable frames duplicated: %+v", irOrderTokens(evs))
	}
}

func TestDecodeStreamFinalOnlyEarlierReasoningStillRejected(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", true, 1, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_new", "completed", nil, `[{"type":"summary_text","text":"late"}]`),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
		t.Fatalf("err = %v, want conflicting message text order", err)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta || ev.Kind == ir.EventReasoningItemDone || ev.Kind == ir.EventFinish {
			t.Fatalf("offending frame emitted: %+v", ev)
		}
	}
}

func TestDecodeStreamRecoveredReasoningSuffixAfterTextRejected(t *testing.T) {
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		sseTextDelta("msg_a", true, 1, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"chain-more"}]`),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
		t.Fatalf("err = %v, want conflicting message text order", err)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone || ev.Kind == ir.EventFinish {
			t.Fatalf("recovered suffix reached terminal frames: %+v", ev)
		}
		if ev.Kind == ir.EventReasoningDelta && ev.Text != "chain" {
			t.Fatalf("recovered suffix emitted: %+v", ev)
		}
	}
}

func TestDecodeStreamQueuedReasoningContentNotMetadata(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", true, 0, "answer"),
		reasoningAddedEvent(1, "rs_1", "in_progress"),
		reasoningDeltaEvent(1, "rs_1", 0, "queued"),
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "answer"),
			reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"queued"}]`),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	assertIRTokens(t, evs, "text:answer", "reason:queued", "reasondone")
}

func TestDecodeStreamRepeatedFinalReasoningMetadataNetFree(t *testing.T) {
	enc := "cipher"
	empty := ""
	final := completedEvent(finalOutput(
		reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"},{"type":"summary_text","text":""}]`),
		sseMsgFinalItem("msg_a", "answer"),
	), "")
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		sseTextDelta("msg_a", true, 1, "answer"),
		final,
		final,
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if countKind(evs, ir.EventReasoningItemDone) != 1 || countKind(evs, ir.EventTextDelta) != 1 {
		t.Fatalf("repeat duplicated frames: %+v", irOrderTokens(evs))
	}
	done := reasoningDone(t, evs, "rs_1")
	if done.Reasoning.EncryptedContent == nil || *done.Reasoning.EncryptedContent != enc {
		t.Fatalf("cipher = %+v", done.Reasoning)
	}

	emptyBody := sseData(
		reasoningAddedEvent(0, "rs_empty", "in_progress"),
		reasoningDeltaEvent(0, "rs_empty", 0, "chain"),
		sseTextDelta("msg_a", true, 1, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_empty", "completed", &empty, `[{"type":"summary_text","text":"chain"}]`),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	emptyEvs, err := collectStream(t, emptyBody)
	if err != nil {
		t.Fatal(err)
	}
	emptyDone := reasoningDone(t, emptyEvs, "rs_empty")
	if emptyDone.Reasoning.EncryptedContent == nil || *emptyDone.Reasoning.EncryptedContent != "" {
		t.Fatalf("explicit empty cipher = %+v", emptyDone.Reasoning.EncryptedContent)
	}

	conflict := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		reasoningItemDoneEvent(0, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"}]`),
		sseTextDelta("msg_a", true, 1, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_1", "completed", &empty, `[{"type":"summary_text","text":"chain"}]`),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	_, err = collectStream(t, conflict)
	if err == nil || !strings.Contains(err.Error(), "conflicting reasoning encrypted content") {
		t.Fatalf("err = %v, want conflicting encrypted content", err)
	}
}
