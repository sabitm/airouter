package responses

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func awaitEvents(t *testing.T, body string) []ir.StreamEvent {
	t.Helper()
	var evs []ir.StreamEvent
	err := DecodeStreamAwaitingItemID(strings.NewReader(body), func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func awaitTokens(evs []ir.StreamEvent) []string {
	var out []string
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventTextDelta:
			out = append(out, "text:"+ev.Text)
		case ir.EventToolCallStart:
			out = append(out, "start:"+ev.ResponsesItemID+"/"+ev.ToolID+"/"+ev.ToolName)
		case ir.EventToolCallDelta:
			out = append(out, "args:"+ev.ResponsesItemID+"/"+ev.ArgsFrag)
		case ir.EventReasoningDelta:
			out = append(out, "reason:"+ev.Text)
		case ir.EventReasoningItemDone:
			id := ""
			if ev.Reasoning != nil {
				id = ev.Reasoning.ID
			}
			out = append(out, "reasondone:"+id)
		}
	}
	return out
}

func assertAwaitTokens(t *testing.T, evs []ir.StreamEvent, want ...string) {
	t.Helper()
	got := awaitTokens(evs)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("IR order = %q, want %q", got, want)
	}
}

func addedNoItem(index int, callID, name string) string {
	item := `{"type":"function_call","call_id":"` + callID + `","name":"` + name + `","arguments":""}`
	return `{"type":"response.output_item.added","output_index":` + strconv.Itoa(index) + `,"item":` + item + `}`
}

func doneWithItem(index int, itemID, callID, name, args string) string {
	item := `{"type":"function_call","id":"` + itemID + `","call_id":"` + callID + `","name":"` + name + `","arguments":"` + args + `"}`
	return `{"type":"response.output_item.done","output_index":` + strconv.Itoa(index) + `,"item":` + item + `}`
}

func completedCalls(items ...string) string {
	return completedEvent("["+strings.Join(items, ",")+"]", "")
}

func fnFinal(itemID, callID, name, args string) string {
	return `{"type":"function_call","id":"` + itemID + `","call_id":"` + callID + `","name":"` + name + `","arguments":"` + args + `"}`
}

func encodeAwait(t *testing.T, evs []ir.StreamEvent) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range evs {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(w); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String()
}

func addedItemIDs(t *testing.T, body string) []string {
	t.Helper()
	var ids []string
	reader := sse.NewReader(strings.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type string `json:"type"`
			Item struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
			} `json:"item"`
		}
		if json.Unmarshal(ev.Data, &frame) != nil || frame.Type != "response.output_item.added" {
			continue
		}
		if frame.Item.Type == "function_call" {
			ids = append(ids, frame.Item.ID+"/"+frame.Item.CallID)
		}
	}
	return ids
}

func TestAwaitParallelToolIDsKeepRegistrationOrder(t *testing.T) {
	cases := []struct {
		name  string
		dones []string
	}{
		{"id A then B", []string{
			doneWithItem(0, "fc_a", "call_a", "read", "{}"),
			doneWithItem(1, "fc_b", "call_b", "read", "{}"),
		}},
		{"id B then A", []string{
			doneWithItem(1, "fc_b", "call_b", "read", "{}"),
			doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{
				addedNoItem(0, "call_a", "read"),
				addedNoItem(1, "call_b", "read"),
			}
			events = append(events, tc.dones...)
			events = append(events, completedCalls(
				fnFinal("fc_a", "call_a", "read", "{}"),
				fnFinal("fc_b", "call_b", "read", "{}"),
			))
			evs := awaitEvents(t, sseData(events...))
			assertAwaitTokens(t, evs,
				"start:fc_a/call_a/read", "args:fc_a/{}",
				"start:fc_b/call_b/read", "args:fc_b/{}",
			)
			ids := addedItemIDs(t, encodeAwait(t, evs))
			if strings.Join(ids, ",") != "fc_a/call_a,fc_b/call_b" {
				t.Fatalf("SSE added order = %v", ids)
			}
		})
	}
}

func TestAwaitThreePendingCandidatesKeepOrder(t *testing.T) {
	body := sseData(
		addedNoItem(0, "call_a", "read"),
		addedNoItem(1, "call_b", "read"),
		addedNoItem(2, "call_c", "search"),
		doneWithItem(2, "fc_c", "call_c", "search", "{}"),
		doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		doneWithItem(1, "fc_b", "call_b", "read", "{}"),
		completedCalls(
			fnFinal("fc_a", "call_a", "read", "{}"),
			fnFinal("fc_b", "call_b", "read", "{}"),
			fnFinal("fc_c", "call_c", "search", "{}"),
		),
	)
	evs := awaitEvents(t, body)
	assertAwaitTokens(t, evs,
		"start:fc_a/call_a/read", "args:fc_a/{}",
		"start:fc_b/call_b/read", "args:fc_b/{}",
		"start:fc_c/call_c/search", "args:fc_c/{}",
	)
}

func TestAwaitEarlierTextDoesNotMoveBehindBlockedTool(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", true, 0, "hel"),
		addedNoItem(1, "call_a", "read"),
		addedNoItem(2, "call_b", "read"),
		sseTextDelta("msg_a", true, 0, "lo"),
		doneWithItem(2, "fc_b", "call_b", "read", "{}"),
		doneWithItem(1, "fc_a", "call_a", "read", "{}"),
		`{"type":"response.output_item.done","output_index":0,"item":`+sseMsgFinalItem("msg_a", "hello")+`}`,
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "hello"),
			fnFinal("fc_a", "call_a", "read", "{}"),
			fnFinal("fc_b", "call_b", "read", "{}"),
		), ""),
	)
	evs := awaitEvents(t, body)
	assertAwaitTokens(t, evs,
		"text:hel", "text:lo",
		"start:fc_a/call_a/read", "args:fc_a/{}",
		"start:fc_b/call_b/read", "args:fc_b/{}",
	)
}

func TestAwaitEarlierTextStaysAheadWhenToolIDArrivesFirst(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", true, 0, "hel"),
		addedNoItem(1, "call_a", "read"),
		doneWithItem(1, "fc_a", "call_a", "read", "{}"),
		sseTextDelta("msg_a", true, 0, "lo"),
		`{"type":"response.output_item.done","output_index":0,"item":`+sseMsgFinalItem("msg_a", "hello")+`}`,
		completedEvent(finalOutput(
			sseMsgFinalItem("msg_a", "hello"),
			fnFinal("fc_a", "call_a", "read", "{}"),
		), ""),
	)
	evs := awaitEvents(t, body)
	assertAwaitTokens(t, evs,
		"text:hel", "text:lo",
		"start:fc_a/call_a/read", "args:fc_a/{}",
	)
}

func TestAwaitTextAndReasoningKeepPositionsAroundBlockedTools(t *testing.T) {
	enc := "cipher"
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "think"),
		addedNoItem(1, "call_a", "read"),
		sseTextDelta("msg_b", true, 2, "hi"),
		addedNoItem(3, "call_b", "read"),
		reasoningDeltaEvent(0, "rs_1", 0, " more"),
		doneWithItem(3, "fc_b", "call_b", "read", "{}"),
		doneWithItem(1, "fc_a", "call_a", "read", "{}"),
		reasoningItemDoneEvent(0, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"think more"}]`),
		`{"type":"response.output_item.done","output_index":2,"item":`+sseMsgFinalItem("msg_b", "hi")+`}`,
		completedEvent(finalOutput(
			reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"think more"}]`),
			fnFinal("fc_a", "call_a", "read", "{}"),
			sseMsgFinalItem("msg_b", "hi"),
			fnFinal("fc_b", "call_b", "read", "{}"),
		), ""),
	)
	evs := awaitEvents(t, body)
	// rs_1 was registered before both tools. Its late done must not move behind
	// call_b, which still waits for the open message.
	assertAwaitTokens(t, evs,
		"reason:think", "reason: more",
		"start:fc_a/call_a/read", "args:fc_a/{}",
		"text:hi",
		"reasondone:rs_1",
		"start:fc_b/call_b/read", "args:fc_b/{}",
	)
	var done int
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			done++
		}
		if ev.Kind == ir.EventToolCallStart && ev.ToolID == ev.ResponsesItemID {
			t.Fatalf("item id replaced call id: %+v", ev)
		}
	}
	if done != 1 {
		t.Fatalf("reasoning done count = %d", done)
	}
}

func TestAwaitReasoningBetweenBlockedToolsStaysBetween(t *testing.T) {
	body := sseData(
		addedNoItem(0, "call_a", "read"),
		reasoningAddedEvent(1, "rs_mid", "in_progress"),
		reasoningDeltaEvent(1, "rs_mid", 0, "mid"),
		addedNoItem(2, "call_b", "read"),
		doneWithItem(2, "fc_b", "call_b", "read", "{}"),
		reasoningItemDoneEvent(1, "rs_mid", "completed", nil, `[{"type":"summary_text","text":"mid"}]`),
		doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		completedEvent(finalOutput(
			fnFinal("fc_a", "call_a", "read", "{}"),
			reasoningFinalItem("rs_mid", "completed", nil, `[{"type":"summary_text","text":"mid"}]`),
			fnFinal("fc_b", "call_b", "read", "{}"),
		), ""),
	)
	evs := awaitEvents(t, body)
	assertAwaitTokens(t, evs,
		"start:fc_a/call_a/read", "args:fc_a/{}",
		"reason:mid", "reasondone:rs_mid",
		"start:fc_b/call_b/read", "args:fc_b/{}",
	)
}

func TestAwaitReasoningAfterBlockedToolStaysAfter(t *testing.T) {
	body := sseData(
		addedNoItem(0, "call_a", "read"),
		reasoningAddedEvent(1, "rs_late", "in_progress"),
		reasoningDeltaEvent(1, "rs_late", 0, "later"),
		doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		reasoningItemDoneEvent(1, "rs_late", "completed", nil, `[{"type":"summary_text","text":"later"}]`),
		completedEvent(finalOutput(
			fnFinal("fc_a", "call_a", "read", "{}"),
			reasoningFinalItem("rs_late", "completed", nil, `[{"type":"summary_text","text":"later"}]`),
		), ""),
	)
	evs := awaitEvents(t, body)
	assertAwaitTokens(t, evs,
		"start:fc_a/call_a/read", "args:fc_a/{}",
		"reason:later", "reasondone:rs_late",
	)
}

func TestAwaitMissingItemIDUsesSyntheticFallbackInOrder(t *testing.T) {
	t.Run("eof", func(t *testing.T) {
		body := sseData(
			addedNoItem(0, "call_a", "read"),
			addedNoItem(1, "call_b", "read"),
			doneWithItem(1, "fc_b", "call_b", "read", "{}"),
		)
		evs := awaitEvents(t, body)
		assertAwaitTokens(t, evs,
			"start:/call_a/read",
			"start:fc_b/call_b/read", "args:fc_b/{}",
		)
		wire := encodeAwait(t, evs)
		ids := addedItemIDs(t, wire)
		if len(ids) != 2 || !strings.HasPrefix(ids[0], "fc_") || ids[0] == "fc_b/call_b" || ids[1] != "fc_b/call_b" {
			t.Fatalf("SSE fallback order = %v body %s", ids, wire)
		}
		if strings.Contains(ids[0], "fc_b/") {
			t.Fatalf("synthetic id collided with the native id: %v", ids)
		}
	})
	t.Run("terminal snapshot", func(t *testing.T) {
		body := sseData(
			addedNoItem(0, "call_a", "read"),
			addedNoItem(1, "call_b", "read"),
			completedCalls(
				fnFinal("", "call_a", "read", "{}"),
				fnFinal("", "call_b", "read", "{}"),
			),
		)
		evs := awaitEvents(t, body)
		assertAwaitTokens(t, evs,
			"start:/call_a/read", "args:/{}",
			"start:/call_b/read", "args:/{}",
		)
	})
}

func TestAwaitCompletedArrayConflictIsAnError(t *testing.T) {
	body := sseData(
		addedNoItem(0, "call_a", "read"),
		addedNoItem(1, "call_b", "read"),
		doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		doneWithItem(1, "fc_b", "call_b", "read", "{}"),
		completedCalls(
			fnFinal("fc_b", "call_b", "read", "{}"),
			fnFinal("fc_a", "call_a", "read", "{}"),
		),
	)
	err := DecodeStreamAwaitingItemID(strings.NewReader(body), func(ir.StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "conflicting function call order") {
		t.Fatalf("err = %v, want conflicting function call order", err)
	}
}

func TestAwaitFinalOnlyIdentitiesFollowArrayWhenUncommitted(t *testing.T) {
	body := sseData(completedCalls(
		fnFinal("fc_b", "call_b", "read", "{}"),
		fnFinal("fc_a", "call_a", "read", "{}"),
	))
	evs := awaitEvents(t, body)
	assertAwaitTokens(t, evs,
		"start:fc_b/call_b/read", "args:fc_b/{}",
		"start:fc_a/call_a/read", "args:fc_a/{}",
	)
}

func TestAwaitEmitErrorPropagates(t *testing.T) {
	body := sseData(
		addedNoItem(0, "call_a", "read"),
		doneWithItem(0, "fc_a", "call_a", "read", "{}"),
	)
	want := errors.New("consumer failed")
	err := DecodeStreamAwaitingItemID(strings.NewReader(body), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want consumer failed", err)
	}
}

func TestAwaitItemBarrierChargesPendingBudget(t *testing.T) {
	parse := func(t *testing.T, raw string) *streamEnvelope {
		t.Helper()
		var env streamEnvelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatal(err)
		}
		return &env
	}
	d := &responsesToolDecoder{
		emit:              func(ir.StreamEvent) error { return nil },
		ensureStarted:     func() error { return nil },
		awaitNativeItemID: true,
		maxPendingBytes:   textQueueEntryOverhead,
		maxPendingEvents:  1,
		maxArgBytes:       1024,
		maxCalls:          8,
	}
	if err := d.observeAdded(parse(t, addedNoItem(0, "call_a", "read"))); err != nil {
		t.Fatal(err)
	}
	if d.queueBytes != textQueueEntryOverhead || d.textQueueEvents != 1 || len(d.textQueue) != 1 || d.textQueue[0].kind != pendingOrderItemBarrier {
		t.Fatalf("barrier budget = bytes %d events %d queue %d", d.queueBytes, d.textQueueEvents, len(d.textQueue))
	}
	err := d.observeAdded(parse(t, addedNoItem(1, "call_b", "read")))
	if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
		t.Fatalf("err = %v, want pending limit", err)
	}
	if d.queueBytes != textQueueEntryOverhead || d.textQueueEvents != 1 || len(d.textQueue) != 1 {
		t.Fatalf("rejected barrier mutated queue: bytes %d events %d len %d", d.queueBytes, d.textQueueEvents, len(d.textQueue))
	}

	var got []string
	ready := &responsesToolDecoder{
		emit: func(ev ir.StreamEvent) error {
			if ev.Kind == ir.EventToolCallStart {
				got = append(got, ev.ResponsesItemID)
			}
			return nil
		},
		ensureStarted:     func() error { return nil },
		awaitNativeItemID: true,
		maxPendingBytes:   4096,
		maxPendingEvents:  8,
		maxArgBytes:       1024,
		maxCalls:          8,
	}
	if err := ready.observeAdded(parse(t, addedNoItem(0, "call_a", "read"))); err != nil {
		t.Fatal(err)
	}
	if err := ready.observeAdded(parse(t, addedNoItem(1, "call_b", "read"))); err != nil {
		t.Fatal(err)
	}
	if err := ready.observeItemDone(parse(t, doneWithItem(1, "fc_b", "call_b", "read", "{}"))); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("B emitted before A: %v", got)
	}
	if ready.queueBytes == 0 || ready.textQueueEvents == 0 {
		t.Fatalf("held work was not charged: bytes %d events %d", ready.queueBytes, ready.textQueueEvents)
	}
	if err := ready.observeItemDone(parse(t, doneWithItem(0, "fc_a", "call_a", "read", "{}"))); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "fc_a,fc_b" {
		t.Fatalf("order = %v", got)
	}
	if ready.queueBytes != 0 || ready.textQueueEvents != 0 || len(ready.textQueue) != 0 {
		t.Fatalf("queue not released: bytes %d events %d len %d", ready.queueBytes, ready.textQueueEvents, len(ready.textQueue))
	}
}

func TestAwaitItemBarrierFullPathBudget(t *testing.T) {
	body := sseData(
		addedNoItem(0, "call_a", "read"),
		addedNoItem(1, "call_b", "read"),
	)
	var evs []ir.StreamEvent
	err := decodeStreamBoundedParts(strings.NewReader(body), func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	}, maxRetainedToolArgBytes, maxTrackedToolCalls, maxRetainedTextBytes, maxTrackedTextItems, maxTrackedTextParts, textQueueEntryOverhead, 1, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts, true)
	if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
		t.Fatalf("err = %v, want pending limit", err)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("start emitted before budget failure: %+v", ev)
		}
	}
}

func installDecoderCapture(t *testing.T) func() *responsesToolDecoder {
	t.Helper()
	var dec *responsesToolDecoder
	testCaptureDecoder = func(d *responsesToolDecoder) { dec = d }
	t.Cleanup(func() { testCaptureDecoder = nil })
	return func() *responsesToolDecoder { return dec }
}

func reasoningAddedAlias(envelopeID, nestedID string) string {
	return `{"type":"response.output_item.added","item_id":"` + envelopeID + `","item":{"id":"` + nestedID + `","type":"reasoning","status":"in_progress","summary":[]}}`
}

func reasoningAddedEnvelopeOnly(envelopeID string) string {
	return `{"type":"response.output_item.added","item_id":"` + envelopeID + `","item":{"type":"reasoning","status":"in_progress","summary":[]}}`
}

func reasoningAddedIndexedAlias(index int, envelopeID, nestedID string) string {
	return `{"type":"response.output_item.added","item_id":"` + envelopeID + `","output_index":` + strconv.Itoa(index) + `,"item":{"id":"` + nestedID + `","type":"reasoning","status":"in_progress","summary":[]}}`
}

func reasoningDoneAlias(envelopeID, nestedID, encrypted string) string {
	item := `{"id":"` + nestedID + `","type":"reasoning","status":"completed","summary":[],"encrypted_content":"` + encrypted + `"}`
	return `{"type":"response.output_item.done","item_id":"` + envelopeID + `","item":` + item + `}`
}

func summaryDeltaAlias(envelopeID, nestedID, delta string) string {
	return `{"type":"response.reasoning_summary_text.delta","item_id":"` + envelopeID + `","delta":"` + delta + `","item":{"id":"` + nestedID + `","type":"reasoning"}}`
}

func summaryDeltaNested(nestedID, delta string) string {
	return `{"type":"response.reasoning_summary_text.delta","delta":"` + delta + `","item":{"id":"` + nestedID + `","type":"reasoning"}}`
}

func summaryDeltaEnvelope(envelopeID, delta string) string {
	return `{"type":"response.reasoning_summary_text.delta","item_id":"` + envelopeID + `","delta":"` + delta + `"}`
}

func summaryPartAlias(envelopeID, nestedID, text string) string {
	return `{"type":"response.reasoning_summary_part.added","item_id":"` + envelopeID + `","summary_index":0,"part":{"type":"summary_text","text":"` + text + `"},"item":{"id":"` + nestedID + `","type":"reasoning"}}`
}

func assertReasonRegs(t *testing.T, d *responsesToolDecoder, wantIDs []string, wantIndexes []int) {
	t.Helper()
	if d == nil || d.reasoning == nil {
		t.Fatal("decoder was not captured")
	}
	if len(d.seqByID) != len(wantIDs) {
		t.Fatalf("seqByID = %#v, want %v", d.seqByID, wantIDs)
	}
	for _, id := range wantIDs {
		seq, ok := d.seqByID[id]
		if !ok {
			t.Fatalf("seqByID = %#v, missing %s", d.seqByID, id)
		}
		bound := false
		for _, item := range d.reasoning.items {
			if item.id != id {
				continue
			}
			bound = true
			if d.reasonSeq[item] != seq {
				t.Fatalf("reasonSeq[%s] = %d, seqByID = %d", id, d.reasonSeq[item], seq)
			}
		}
		if !bound {
			t.Fatalf("seqByID key %q is not a bound reasoning item", id)
		}
	}
	if len(d.seqByOutput) != len(wantIndexes) {
		t.Fatalf("seqByOutput = %#v, want %v", d.seqByOutput, wantIndexes)
	}
	for _, index := range wantIndexes {
		seq, ok := d.seqByOutput[index]
		if !ok {
			t.Fatalf("seqByOutput = %#v, missing %d", d.seqByOutput, index)
		}
		bound := false
		for _, item := range d.reasoning.items {
			if !item.hasIndex || item.outputIndex != index {
				continue
			}
			bound = true
			if d.reasonSeq[item] != seq {
				t.Fatalf("reasonSeq index %d = %d, seqByOutput = %d", index, d.reasonSeq[item], seq)
			}
		}
		if !bound {
			t.Fatalf("seqByOutput key %d is not a bound reasoning item", index)
		}
	}
	wantSeq := len(d.reasoning.items) + len(d.calls) + len(d.texts)
	if d.nextSeq != wantSeq {
		t.Fatalf("nextSeq = %d, bound items = %d; alias sequences were allocated", d.nextSeq, wantSeq)
	}
}

func TestAwaitMisleadingEnvelopeIDsDoNotAliasReasoning(t *testing.T) {
	enc := "cipher"
	t.Run("opaque item stays ahead of a later blocked tool", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			reasoningAddedAlias("alias-1", "rs_real"),
			reasoningAddedAlias("alias-2", "rs_real"),
			addedNoItem(0, "call_a", "read"),
			reasoningAddedAlias("alias-3", "rs_real"),
			reasoningDoneAlias("alias-done", "rs_real", enc),
			doneWithItem(0, "fc_a", "call_a", "read", "{}"),
			completedEvent(finalOutput(
				reasoningFinalItem("rs_real", "completed", &enc, `[]`),
				fnFinal("fc_a", "call_a", "read", "{}"),
			), ""),
		)
		var evs []ir.StreamEvent
		var sawDone bool
		err := DecodeStreamAwaitingItemID(strings.NewReader(body), func(ev ir.StreamEvent) error {
			evs = append(evs, ev)
			if ev.Kind != ir.EventReasoningItemDone || sawDone {
				return nil
			}
			sawDone = true
			dec := captured()
			if dec == nil || dec.queueBytes != textQueueEntryOverhead || dec.textQueueEvents != 1 || len(dec.textQueue) != 1 {
				t.Fatalf("alias frames changed the pending queue: %+v", dec)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !sawDone {
			t.Fatal("reasoning done was not emitted")
		}
		assertAwaitTokens(t, evs,
			"reasondone:rs_real",
			"start:fc_a/call_a/read", "args:fc_a/{}",
		)
		dec := captured()
		assertReasonRegs(t, dec, []string{"rs_real"}, nil)
		if len(dec.reasoning.items) != 1 {
			t.Fatalf("reasoning items = %d", len(dec.reasoning.items))
		}
		if dec.seqByID["rs_real"] >= dec.calls[0].seq {
			t.Fatalf("reasoning seq %d is not before blocked tool seq %d", dec.seqByID["rs_real"], dec.calls[0].seq)
		}
	})

	t.Run("item registered after a blocked tool stays after it", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			addedNoItem(0, "call_a", "read"),
			reasoningAddedAlias("alias-1", "rs_real"),
			reasoningAddedAlias("alias-2", "rs_real"),
			reasoningDoneAlias("alias-done", "rs_real", enc),
			doneWithItem(0, "fc_a", "call_a", "read", "{}"),
			completedEvent(finalOutput(
				fnFinal("fc_a", "call_a", "read", "{}"),
				reasoningFinalItem("rs_real", "completed", &enc, `[]`),
			), ""),
		)
		evs := awaitEvents(t, body)
		assertAwaitTokens(t, evs,
			"start:fc_a/call_a/read", "args:fc_a/{}",
			"reasondone:rs_real",
		)
		dec := captured()
		assertReasonRegs(t, dec, []string{"rs_real"}, nil)
		if dec.seqByID["rs_real"] <= dec.calls[0].seq {
			t.Fatalf("reasoning seq %d moved ahead of earlier tool seq %d", dec.seqByID["rs_real"], dec.calls[0].seq)
		}
	})

	t.Run("output index is stored and envelope id is not", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			reasoningAddedIndexedAlias(4, "alias-idx", "rs_idx"),
			reasoningAddedIndexedAlias(4, "alias-idx-2", "rs_idx"),
			addedNoItem(0, "call_a", "read"),
			reasoningDoneAlias("alias-done", "rs_idx", enc),
			doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		)
		evs := awaitEvents(t, body)
		assertAwaitTokens(t, evs,
			"reasondone:rs_idx",
			"start:fc_a/call_a/read", "args:fc_a/{}",
		)
		dec := captured()
		assertReasonRegs(t, dec, []string{"rs_idx"}, []int{4})
		if dec.seqByOutput[4] >= dec.calls[0].seq {
			t.Fatalf("indexed reasoning seq %d is not before tool seq %d", dec.seqByOutput[4], dec.calls[0].seq)
		}
	})

	t.Run("summary frames prefer envelope id then nested id", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			summaryDeltaAlias("rs_env", "rs_nested", "P"),
			summaryPartAlias("rs_env", "rs_other", "PQ"),
			addedNoItem(0, "call_a", "read"),
			summaryDeltaEnvelope("rs_env", "R"),
			doneWithItem(0, "fc_a", "call_a", "read", "{}"),
		)
		evs := awaitEvents(t, body)
		assertAwaitTokens(t, evs,
			"reason:P", "reason:Q", "reason:R",
			"start:fc_a/call_a/read", "args:fc_a/{}",
		)
		dec := captured()
		assertReasonRegs(t, dec, []string{"rs_env"}, nil)
		if _, ok := dec.seqByID["rs_nested"]; ok {
			t.Fatal("summary frame registered the nested id instead of the envelope id")
		}
		if dec.seqByID["rs_env"] >= dec.calls[0].seq {
			t.Fatalf("summary seq %d is not before blocked tool seq %d", dec.seqByID["rs_env"], dec.calls[0].seq)
		}
	})

	t.Run("summary frame falls back to nested id", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			summaryDeltaNested("rs_nested", "a"),
			addedNoItem(1, "call_a", "read"),
			summaryDeltaNested("rs_nested", "b"),
			doneWithItem(1, "fc_a", "call_a", "read", "{}"),
		)
		evs := awaitEvents(t, body)
		assertAwaitTokens(t, evs,
			"reason:a", "reason:b",
			"start:fc_a/call_a/read", "args:fc_a/{}",
		)
		assertReasonRegs(t, captured(), []string{"rs_nested"}, nil)
	})

	t.Run("envelope id alone is not an added identity", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			reasoningAddedEnvelopeOnly("alias-1"),
			reasoningAddedEnvelopeOnly("alias-2"),
			reasoningAddedEnvelopeOnly("alias-3"),
		)
		if err := DecodeStreamAwaitingItemID(strings.NewReader(body), func(ir.StreamEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		dec := captured()
		if dec == nil {
			t.Fatal("decoder was not captured")
		}
		if len(dec.seqByID) != 0 || len(dec.seqByOutput) != 0 {
			t.Fatalf("envelope-only added allocated keys: ids %#v indexes %#v", dec.seqByID, dec.seqByOutput)
		}
		if dec.nextSeq != len(dec.reasoning.items) {
			t.Fatalf("nextSeq = %d, items = %d; alias sequences were allocated", dec.nextSeq, len(dec.reasoning.items))
		}
	})

	t.Run("rejected reasoning item allocates no sequence", func(t *testing.T) {
		captured := installDecoderCapture(t)
		body := sseData(
			reasoningAddedAlias("alias-ok", "rs_ok"),
			reasoningAddedIndexedAlias(9, "alias-over", "rs_over"),
		)
		err := decodeStreamBoundedParts(strings.NewReader(body), func(ir.StreamEvent) error { return nil },
			maxRetainedToolArgBytes, maxTrackedToolCalls, maxRetainedTextBytes, maxTrackedTextItems, maxTrackedTextParts,
			maxPendingTextBytes, maxPendingTextEvents, maxRetainedReasoningBytes, 1, maxTrackedReasoningParts, true)
		if err == nil || !strings.Contains(err.Error(), "too many reasoning items") {
			t.Fatalf("err = %v, want too many reasoning items", err)
		}
		dec := captured()
		assertReasonRegs(t, dec, []string{"rs_ok"}, nil)
		if _, ok := dec.seqByID["rs_over"]; ok {
			t.Fatal("rejected item id was registered")
		}
		if _, ok := dec.seqByID["alias-over"]; ok {
			t.Fatal("rejected envelope id was registered")
		}
		if _, ok := dec.seqByOutput[9]; ok {
			t.Fatal("rejected output index was registered")
		}
	})
}

func TestDecodeStreamDoesNotRetainAwaitBookkeeping(t *testing.T) {
	captured := installDecoderCapture(t)
	body := sseData(
		reasoningAddedAlias("alias-normal", "rs_normal"),
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_a","call_id":"call_a","name":"read","arguments":""}}`,
	)
	if err := DecodeStream(strings.NewReader(body), func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	dec := captured()
	if dec == nil {
		t.Fatal("decoder was not captured")
	}
	if dec.itemWire != nil || dec.wireClock != 0 {
		t.Fatalf("normal Responses retained wire ranks: %+v clock %d", dec.itemWire, dec.wireClock)
	}
	if len(dec.seqByID) != 0 || len(dec.seqByOutput) != 0 || len(dec.reasonSeq) != 0 {
		t.Fatalf("normal Responses pre-registered reasoning: ids %#v indexes %#v seq %#v", dec.seqByID, dec.seqByOutput, dec.reasonSeq)
	}
}
