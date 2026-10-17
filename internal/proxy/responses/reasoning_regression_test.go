package responses

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// decoderBudget executes one SSE stream with an injected decoder reasoning
// budget. Every event is recorded so a callback can assert on emissions even
// when the decode ends with an error.
func decoderBudget(t *testing.T, r io.Reader, maxBytes int) ([]ir.StreamEvent, error) {
	t.Helper()
	var evs []ir.StreamEvent
	err := decodeStreamLimitedAll(r, func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	}, 1<<30, 1024, maxBytes, 1024, 1024)
	return evs, err
}

// noBadDoneAfterDeltas guards the reservation invariant: once any reasoning
// delta was emitted, the stream must not end with a reasoning done that
// silently dropped the snapshot, and a failing decode must never carry a
// complete done event for the offending item.
func noBadDoneAfterDeltas(t *testing.T, evs []ir.StreamEvent, err error) {
	t.Helper()
	sawDelta := false
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventReasoningDelta:
			sawDelta = true
		case ir.EventReasoningItemDone:
			if ev.Reasoning == nil {
				t.Fatalf("done event lost its snapshot after deltas=%v: %+v", sawDelta, ev)
			}
		}
	}
	if err != nil && sawDelta {
		for _, ev := range evs {
			if ev.Kind == ir.EventReasoningItemDone && ev.Reasoning.Summary != nil {
				t.Fatalf("offending done emitted before failure: %+v", ev.Reasoning)
			}
		}
	}
}

func TestReservedReasoningSnapshotMatchesState(t *testing.T) {
	enc := "cipher"
	item := &responsesReasoningItem{id: "rs_1", status: "completed", encrypted: &enc}
	d := &responsesReasoningDecoder{maxBytes: 1 << 20, maxParts: 8}
	part, err := d.part(item, 0)
	if err != nil {
		t.Fatal(err)
	}
	part.typ = "summary_text"
	part.text.WriteString("chain")
	// The reservation is the snapshot copy only. Item and key bookkeeping is
	// charged at bind/assign time, not again per snapshot.
	want := len("rs_1") + len("completed") + len(enc) +
		reasoningPartOverhead + len("summary_text") + len("chain")
	if got := item.reasoningReservedSnapshot(); got != want {
		t.Fatalf("reserved = %d want %d", got, want)
	}
	if reserved, _ := item.reservedResponsesReasoning(); reserved != want {
		t.Fatalf("probe reserved=%d want %d", reserved, want)
	}
	// An empty explicit part still pays the part overhead.
	empty := &responsesReasoningItem{}
	d2 := &responsesReasoningDecoder{maxBytes: 1 << 20, maxParts: 8}
	if _, err := d2.part(empty, 0); err != nil {
		t.Fatal(err)
	}
	if got := empty.reasoningReservedSnapshot(); got != reasoningPartOverhead+len("summary_text") {
		t.Fatalf("empty part reserved = %d", got)
	}
}

func TestReservedReasoningSnapshotMatchesLegacyByteCount(t *testing.T) {
	enc := "cipher"
	item := &responsesReasoningItem{id: "rs_1", status: "completed", encrypted: &enc}
	d := &responsesReasoningDecoder{maxBytes: 1 << 20, maxParts: 8}
	for i := 0; i < 3; i++ {
		part, err := d.part(item, i)
		if err != nil {
			t.Fatal(err)
		}
		part.text.WriteString(strings.Repeat("p", i+1))
	}
	item.parts[2].typ = "other"
	snap := (&responsesReasoningDecoder{}).snapshot(item)
	if item.reasoningReservedSnapshot() != retainedSnapshotBytes(snap) {
		t.Fatalf("state-derived %d != snapshot %d", item.reasoningReservedSnapshot(), retainedSnapshotBytes(snap))
	}
}

func TestDecodeStreamReasoningDoneOverBudgetNeverEmitted(t *testing.T) {
	enc := "cipher"
	// state is the snapshot footprint (ID, status, encrypted bytes, part
	// overhead, type, text). full is the exact ledger: item overhead, both
	// identity keys, the built state, and the retained snapshot copy. full-1
	// must fail inside emitDone, after the state and any recovered delta exist.
	item := reasoningItemDoneEvent(0, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"}]`)
	state := len("rs_1") + len(enc) + len("completed") + reasoningPartOverhead + len("summary_text") + len("chain")
	full := reasoningItemOverhead + 2*reasoningKeyOverhead + 2*state
	if full > 1<<20 {
		t.Fatal("budget assumptions drifted")
	}
	t.Run("exact full succeeds", func(t *testing.T) {
		evs, err := decoderBudget(t, strings.NewReader(sseData(item)), full)
		if err != nil {
			t.Fatalf("exact budget failed: %v", err)
		}
		dones := 0
		for _, ev := range evs {
			if ev.Kind == ir.EventReasoningItemDone {
				dones++
				if ev.Reasoning == nil || ev.Reasoning.EncryptedContent == nil || *ev.Reasoning.EncryptedContent != enc {
					t.Fatalf("done snapshot = %+v", ev.Reasoning)
				}
			}
		}
		if dones != 1 {
			t.Fatalf("dones = %d events %+v", dones, evs)
		}
	})
	t.Run("encrypted copy rejected in emitDone", func(t *testing.T) {
		// full-1 lets state creation finish, so the failure must be the emitDone
		// snapshot reservation, not an earlier state charge.
		evs, err := decoderBudget(t, strings.NewReader(sseData(item)), full-1)
		noBadDoneAfterDeltas(t, evs, err)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "reasoning state too large" {
			t.Fatalf("err = %v", err)
		}
		deltas := 0
		for _, ev := range evs {
			switch ev.Kind {
			case ir.EventReasoningDelta:
				deltas++
			case ir.EventReasoningItemDone:
				t.Fatalf("over-limit done was emitted: %+v", ev)
			}
		}
		if deltas == 0 {
			t.Fatal("state creation failed before the recovered delta was emitted")
		}
	})
	t.Run("streamed delta then over-limit done fails with no invalid done", func(t *testing.T) {
		body := sseData(reasoningDeltaEvent(0, "rs_1", 0, "chain"), item)
		// The streamed text fits and is charged once; only the snapshot copy
		// crosses the line inside emitDone.
		evs, err := decoderBudget(t, strings.NewReader(body), full-1)
		noBadDoneAfterDeltas(t, evs, err)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "reasoning state too large" {
			t.Fatalf("err = %v", err)
		}
		deltas := 0
		for _, ev := range evs {
			switch ev.Kind {
			case ir.EventReasoningDelta:
				deltas++
			case ir.EventReasoningItemDone:
				t.Fatalf("offending done emitted: %+v", ev)
			}
		}
		if deltas != 1 {
			t.Fatalf("deltas = %d, want the streamed fragment once", deltas)
		}
	})
}

// TestEmitDoneSnapshotNilWhenReservationFails drives the decoder directly so a
// probe can prove state creation completed and the snapshot copy is the only
// failing reservation. A failed emitDone must leave item.emitted nil.
func TestEmitDoneSnapshotNilWhenReservationFails(t *testing.T) {
	enc := "cipher"
	state := len("rs_1") + len(enc) + len("completed") + reasoningPartOverhead + len("summary_text") + len("chain")
	full := reasoningItemOverhead + 2*reasoningKeyOverhead + 2*state
	dec := newResponsesReasoningDecoder(func(ir.StreamEvent) error { return nil }, nil, full-1, 1024, 1024)
	env := &streamEnvelope{
		OutputIndex: 0, OutputIndexSet: true,
		Item: &streamItem{
			ID: "rs_1", Type: "reasoning", Status: "completed",
			EncryptedContent: &enc,
			Summary:          []reasoningSummary{{Type: "summary_text", Text: "chain", TextSet: true}},
			SummarySet:       true,
		},
	}
	err := dec.observeItemDone(env)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "reasoning state too large" {
		t.Fatalf("err = %v", err)
	}
	if len(dec.items) != 1 {
		t.Fatalf("items = %d, want the state built before the failed snapshot", len(dec.items))
	}
	item := dec.items[0]
	if item.emitted != nil {
		t.Fatalf("failed reservation retained a snapshot: %+v", item.emitted)
	}
	if got := item.reasoningReservedSnapshot(); got != state {
		t.Fatalf("state probe = %d want %d", got, state)
	}
}

func TestDecodeStreamReasoningDoneRepeatedSnapshotsNoCumulativeCharges(t *testing.T) {
	enc := "cipher"
	summary := `[{"type":"summary_text","text":"chain"}]`
	item := reasoningItemDoneEvent(0, "rs_1", "completed", &enc, summary)
	state := len("rs_1") + len(enc) + len("completed") + reasoningPartOverhead + len("summary_text") + len("chain")
	// Identical snapshots after the first must stay free, including the dedup
	// retained copy.
	full := reasoningItemOverhead + 2*reasoningKeyOverhead + 2*state
	evs, err := decoderBudget(t, strings.NewReader(sseData(item, item, item, item)), full)
	if err != nil {
		t.Fatalf("repeated snapshots double-charged: %v", err)
	}
	dones := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			dones++
		}
	}
	if dones != 1 {
		t.Fatalf("dones = %d", dones)
	}
}

func TestDecodeStreamReasoningDoneGrowthChargedIncrementally(t *testing.T) {
	base := reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"chain"}]`)
	baseState := len("rs_1") + len("completed") + reasoningPartOverhead + len("summary_text") + len("chain")
	first := retainedResponsesReasoning(&ir.ResponsesReasoning{ID: "rs_1", Status: "completed",
		Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}}})
	enc := "late-cipher"
	wider := reasoningItemDoneEvent(0, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"},{"type":"summary_text","text":""}]`)
	second := retainedResponsesReasoning(&ir.ResponsesReasoning{ID: "rs_1", Status: "completed", EncryptedContent: &enc,
		Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}, {Type: "summary_text", Text: ""}}})
	growth := second - first
	// The budget covers the base ledger, the first snapshot, and most of the
	// growth: the second done must fail without emitting. The first done stays
	// emitted because it was fully paid for before the wider snapshot arrived.
	fixed := reasoningItemOverhead + 2*reasoningKeyOverhead
	evs, err := decoderBudget(t, strings.NewReader(sseData(base, wider)), fixed+baseState+first+growth+150)
	if sf, ok := ir.AsStreamFailure(err); !ok || sf.Message != "reasoning state too large" {
		t.Fatalf("err = %v", err)
	}
	// The first done is legitimate: it was fully paid for and emitted before the
	// wider snapshot crossed the limit. Only a snapshot wider than first is an
	// offense here, so the shared helper is replaced by a targeted check.
	var offending bool
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone && ev.Reasoning != nil && retainedResponsesReasoning(ev.Reasoning) != first {
			offending = true
		}
	}
	if offending {
		t.Fatalf("wider done emitted before failure: %+v", evs)
	}
	var lastDone *ir.ResponsesReasoning
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			lastDone = ev.Reasoning
		}
	}
	if lastDone == nil || retainedResponsesReasoning(lastDone) != first {
		t.Fatalf("emitted snapshot grew before failure: %+v", lastDone)
	}
	// With the full growth budget both snapshots are emitted.
	// The wider snapshot replaces the retained copy, so the ledger carries the
	// base state once plus the full second snapshot, the item's fixed
	// bookkeeping, and the width-2 part slice of the final array. Anything less
	// still fails.
	evs, err = decoderBudget(t, strings.NewReader(sseData(base, wider)), fixed+baseState+second+151)
	if err != nil {
		t.Fatalf("full budget failed: %v", err)
	}
	dones := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			dones++
		}
	}
	if dones != 2 {
		t.Fatalf("dones = %d", dones)
	}
}

func TestDecodeStreamReasoningChangedFinalIDRejected(t *testing.T) {
	summary := `[{"type":"summary_text","text":"chain"}]`
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		reasoningItemDoneEvent(0, "rs_1", "completed", nil, summary),
		completedEvent(finalOutput(reasoningFinalItem("rs_9", "completed", nil, summary)), ""),
	)
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "conflicting reasoning item identity" {
		t.Fatalf("changed final ID err = %v", err)
	}
}

func TestDecodeStreamReasoningChangedFinalIDOnRepeatOrdinal(t *testing.T) {
	summary := `[{"type":"summary_text","text":"chain"}]`
	// The second completion reuses the ordinal with a different ID. The first
	// item was already claimed, so this must be a conflict, not a rebind.
	body := sseData(
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, summary)), ""),
		completedEvent(finalOutput(reasoningFinalItem("rs_2", "completed", nil, summary)), ""),
	)
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "conflicting reasoning item identity" {
		t.Fatalf("changed ID at repeat ordinal err = %v", err)
	}
}

func TestDecodeStreamReasoningFinalShrinksSummaryRejected(t *testing.T) {
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "a"),
		reasoningDeltaEvent(0, "rs_1", 1, "b"),
		completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"a"}]`)), ""),
	)
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "conflicting reasoning summary" {
		t.Fatalf("shrinking final err = %v", err)
	}
}

func TestDecodeStreamReasoningFinalChangedRetainedTypeRejected(t *testing.T) {
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"custom","text":""}}`,
		reasoningDeltaEvent(0, "rs_1", 0, "x"),
		completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"x"}]`)), ""),
	)
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "conflicting reasoning summary" {
		t.Fatalf("changed retained type err = %v", err)
	}
}

func TestDecodeStreamReasoningFinalChangedRetainedTextRejected(t *testing.T) {
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "hello"),
		reasoningTextDoneEvent(0, "rs_1", 0, "hello"),
		reasoningPartDoneEvent(0, "rs_1", 0, "summary_text", "hello"),
		completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"world"}]`)), ""),
	)
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "conflicting reasoning summary" {
		t.Fatalf("changed retained text err = %v", err)
	}
}

func TestDecodeStreamReasoningEmptyTextDoneContradiction(t *testing.T) {
	// A reasoning_summary_text.done with empty text contradicts the nonempty
	// part already streamed: the part boundary must not erase retained bytes.
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "kept"),
		reasoningTextDoneEvent(0, "rs_1", 0, ""),
	)
	_, err := collectStream(t, body)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "conflicting reasoning summary" {
		t.Fatalf("empty text.done err = %v", err)
	}
}

func TestDecodeStreamReasoningTerminalGrowthAfterDoneRejected(t *testing.T) {
	summary := `[{"type":"summary_text","text":"chain"}]`
	longer := `[{"type":"summary_text","text":"chain-more"}]`
	t.Run("final growth", func(t *testing.T) {
		body := sseData(
			reasoningAddedEvent(0, "rs_1", "in_progress"),
			reasoningItemDoneEvent(0, "rs_1", "completed", nil, summary),
			completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, longer)), ""),
		)
		_, err := collectStream(t, body)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "conflicting reasoning summary" {
			t.Fatalf("terminal growth err = %v", err)
		}
	})
	t.Run("new work after terminal", func(t *testing.T) {
		body := sseData(
			completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, summary)), ""),
			reasoningDeltaEvent(0, "rs_1", 0, "late"),
		)
		_, err := collectStream(t, body)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || (sf.Message != "conflicting response content" && sf.Message != "conflicting reasoning summary") {
			t.Fatalf("new work after terminal err = %v", err)
		}
	})
	t.Run("new item after terminal", func(t *testing.T) {
		body := sseData(
			completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, summary)), ""),
			reasoningAddedEvent(1, "rs_new", "in_progress"),
		)
		_, err := collectStream(t, body)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "conflicting reasoning item identity" {
			t.Fatalf("new item after terminal err = %v", err)
		}
	})
	t.Run("identical terminal accepted", func(t *testing.T) {
		body := sseData(
			reasoningDeltaEvent(0, "rs_1", 0, "chain"),
			completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, summary)), ""),
			completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, summary)), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatalf("identical terminal rejected: %v", err)
		}
		deltas, dones := 0, 0
		for _, ev := range evs {
			switch ev.Kind {
			case ir.EventReasoningDelta:
				deltas++
			case ir.EventReasoningItemDone:
				dones++
			}
		}
		if deltas != 1 || dones != 1 {
			t.Fatalf("deltas=%d dones=%d", deltas, dones)
		}
	})
}

func TestDecodeStreamReasoningAbsentToPresentMetadataAllowed(t *testing.T) {
	enc := "late"
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"chain"}]`),
		completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"}]`)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("absent-to-present rejected: %v", err)
	}
	var last *ir.ResponsesReasoning
	for i := range evs {
		if evs[i].Kind == ir.EventReasoningItemDone {
			last = evs[i].Reasoning
		}
	}
	if last == nil || last.EncryptedContent == nil || *last.EncryptedContent != enc {
		t.Fatalf("late encrypted lost: %+v", last)
	}
}

func TestDecodeStreamReasoningAddedEmptyArraysArePlaceholders(t *testing.T) {
	// output_item.added always carries summary:[]; that array must not count as
	// a complete contradiction of parts added later by deltas.
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "a"),
		reasoningDeltaEvent(0, "rs_1", 1, "b"),
		reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}]`),
		completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", nil, `[{"type":"summary_text","text":"a"},{"type":"summary_text","text":"b"}]`)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("added placeholder treated as complete: %v", err)
	}
	deltas := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta {
			deltas++
		}
	}
	if deltas != 2 {
		t.Fatalf("deltas = %d", deltas)
	}
}

// decodeThenEncode runs SSE through the Responses decoder and re-encodes the
// resulting IR stream, returning the decoded events and the emitted SSE body.
func decodeThenEncode(t *testing.T, body string) ([]ir.StreamEvent, string, error) {
	t.Helper()
	var evs []ir.StreamEvent
	err := DecodeStream(strings.NewReader(body), func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		return evs, "", err
	}
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range evs {
		if err := enc.Encode(ev, w); err != nil {
			return evs, rec.Body.String(), err
		}
	}
	if err := enc.Close(w); err != nil {
		return evs, rec.Body.String(), err
	}
	return evs, rec.Body.String(), nil
}

// summaryDeltaStrings returns wire-order "itemID@summaryIndex:text" fragments.
func summaryDeltaStrings(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, f := range parseResponsesStream(t, body) {
		if f.Name != "response.reasoning_summary_text.delta" {
			continue
		}
		itemID, _ := f.Data["item_id"].(string)
		idx := 0
		if v, ok := f.Data["summary_index"].(float64); ok {
			idx = int(v)
		}
		text, _ := f.Data["delta"].(string)
		out = append(out, itemID+"@"+itoa(idx)+":"+text)
	}
	return out
}

// summaryDoneTexts returns the full text carried by each summary_text.done.
func summaryDoneTexts(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, f := range parseResponsesStream(t, body) {
		if f.Name != "response.reasoning_summary_text.done" {
			continue
		}
		text, _ := f.Data["text"].(string)
		out = append(out, text)
	}
	return out
}

func finalSummaryTexts(t *testing.T, body string, itemIndex int) []string {
	t.Helper()
	final := finalResponseObject(t, body)
	output, _ := final["output"].([]any)
	if itemIndex >= len(output) {
		t.Fatalf("output too short: %+v", output)
	}
	item, _ := output[itemIndex].(map[string]any)
	summary, _ := item["summary"].([]any)
	var out []string
	for _, raw := range summary {
		m, _ := raw.(map[string]any)
		s, _ := m["text"].(string)
		out = append(out, s)
	}
	return out
}

func itoa(n int) string {
	return strings.TrimSpace(jsonNumber(n))
}

func jsonNumber(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func TestSummaryAppendGapSuffixRecoveredExactlyOnce(t *testing.T) {
	// Deltas skip summary_index 0 entirely; the item done fills part 0 and
	// confirms part 1. The recovered suffix must appear exactly once in order.
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_g", "in_progress"),
		reasoningDeltaEvent(0, "rs_g", 1, " tail"),
		reasoningItemDoneEvent(0, "rs_g", "completed", nil, `[{"type":"summary_text","text":"head"},{"type":"summary_text","text":" tail"}]`),
		completedEvent(finalOutput(reasoningFinalItem("rs_g", "completed", nil, `[{"type":"summary_text","text":"head"},{"type":"summary_text","text":" tail"}]`)), ""),
	)
	evs, encoded, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var deltaParts []string
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta {
			deltaParts = append(deltaParts, ev.Text)
		}
	}
	// Exactly once: the streamed suffix and the snapshot-recovered head each
	// appear once in the IR event stream.
	if strings.Join(deltaParts, "\x1f") != " tail\x1fhead" {
		t.Fatalf("decoded deltas = %q, want streamed suffix plus recovered head once", deltaParts)
	}
	got := summaryDeltaStrings(t, encoded)
	want := []string{"rs_g@1: tail", "rs_g@0:head"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wire deltas = %q want %q", got, want)
	}
	if dones := summaryDoneTexts(t, encoded); len(dones) != 2 || dones[0] != " tail" || dones[1] != "head" {
		t.Fatalf("done texts = %q", dones)
	}
	if final := finalSummaryTexts(t, encoded, 0); len(final) != 2 || final[0] != "head" || final[1] != " tail" {
		t.Fatalf("final summary = %q", final)
	}
}

func TestSummaryAppendMultiPartSuffixRecoveredExactlyOnce(t *testing.T) {
	// Deltas stop mid part 1; the item done supplies the missing suffix of part
	// 1 and all of part 2. Every delta must appear exactly once, in part order.
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_m", "in_progress"),
		reasoningDeltaEvent(0, "rs_m", 0, "one"),
		reasoningTextDoneEvent(0, "rs_m", 0, "one"),
		reasoningPartDoneEvent(0, "rs_m", 0, "summary_text", "one"),
		reasoningDeltaEvent(0, "rs_m", 1, "tw"),
		reasoningItemDoneEvent(0, "rs_m", "completed", nil,
			`[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"},{"type":"summary_text","text":"three"}]`),
		completedEvent(finalOutput(reasoningFinalItem("rs_m", "completed", nil,
			`[{"type":"summary_text","text":"one"},{"type":"summary_text","text":"two"},{"type":"summary_text","text":"three"}]`)), ""),
	)
	evs, encoded, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	deltas := ""
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta {
			deltas += ev.Text
		}
	}
	if deltas != "onetwothree" {
		t.Fatalf("decoded deltas joined = %q, want onetwothree exactly once", deltas)
	}
	got := summaryDeltaStrings(t, encoded)
	want := []string{"rs_m@0:one", "rs_m@1:tw", "rs_m@1:o", "rs_m@2:three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wire deltas = %q want %q", got, want)
	}
	if final := finalSummaryTexts(t, encoded, 0); len(final) != 3 || final[0] != "one" || final[1] != "two" || final[2] != "three" {
		t.Fatalf("final summary = %q", final)
	}
}

func TestSummaryAppendHighIndicesOnlyFilledBySnapshot(t *testing.T) {
	// All deltas target indices 2 and 5; parts 0, 1, 3, 4 stay empty
	// placeholders and the wire stream must carry a boundary for each.
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_h", "in_progress"),
		reasoningDeltaEvent(0, "rs_h", 2, "C"),
		reasoningDeltaEvent(0, "rs_h", 5, "F"),
		reasoningItemDoneEvent(0, "rs_h", "completed", nil,
			`[{"text":""},{"text":""},{"type":"summary_text","text":"C"},{"text":""},{"text":""},{"type":"summary_text","text":"F"}]`),
		completedEvent(finalOutput(reasoningFinalItem("rs_h", "completed", nil,
			`[{"text":""},{"text":""},{"type":"summary_text","text":"C"},{"text":""},{"text":""},{"type":"summary_text","text":"F"}]`)), ""),
	)
	_, encoded, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	got := summaryDeltaStrings(t, encoded)
	want := []string{"rs_h@2:C", "rs_h@5:F"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wire deltas = %q want %q", got, want)
	}
	added := 0
	for _, f := range parseResponsesStream(t, encoded) {
		if f.Name == "response.reasoning_summary_part.added" {
			added++
		}
	}
	if added != 6 {
		t.Fatalf("summary part boundaries = %d, want 6", added)
	}
	final := finalResponseObject(t, encoded)
	output, _ := final["output"].([]any)
	item, _ := output[0].(map[string]any)
	summary, _ := item["summary"].([]any)
	if len(summary) != 6 {
		t.Fatalf("final summary length = %d", len(summary))
	}
	texts := []string{}
	for _, raw := range summary {
		m, _ := raw.(map[string]any)
		s, _ := m["text"].(string)
		texts = append(texts, s)
	}
	wantTexts := []string{"", "", "C", "", "", "F"}
	if strings.Join(texts, ",") != strings.Join(wantTexts, ",") {
		t.Fatalf("final texts = %q want %q", texts, wantTexts)
	}
}

func TestSummaryAppendTerminalFinalReconcileExactOnce(t *testing.T) {
	// Final-only reasoning: the whole summary is recovered by the terminal
	// pass, and a repeated completion must not duplicate it.
	items := finalOutput(
		reasoningFinalItem("rs_x", "completed", nil, `[{"type":"summary_text","text":"alpha"}]`),
		completedItem("fc_x", "call_x", "search", `{}`),
		reasoningFinalItem("rs_y", "completed", nil, `[{"type":"summary_text","text":"beta"}]`),
	)
	body := sseData(completedEvent(items, ""), completedEvent(items, ""))
	evs, encoded, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventReasoningDelta:
			seq = append(seq, "delta:"+ev.ReasoningID+":"+ev.Text)
		case ir.EventReasoningItemDone:
			seq = append(seq, "done:"+ev.ReasoningID)
		case ir.EventToolCallStart:
			seq = append(seq, "tool")
		}
	}
	want := []string{"delta:rs_x:alpha", "done:rs_x", "tool", "delta:rs_y:beta", "done:rs_y"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("sequence = %q want %q", seq, want)
	}
	got := summaryDeltaStrings(t, encoded)
	wantDeltas := []string{"rs_x@0:alpha", "rs_y@0:beta"}
	if strings.Join(got, "|") != strings.Join(wantDeltas, "|") {
		t.Fatalf("wire deltas = %q want %q", got, wantDeltas)
	}
	if final := finalSummaryTexts(t, encoded, 0); len(final) != 1 || final[0] != "alpha" {
		t.Fatalf("final summary = %q", final)
	}
}

func TestSummaryAppendMidItemGapSuffixRecoveredExactlyOnce(t *testing.T) {
	// Part 0 streams, part 1 is skipped, part 2 streams. The item done fills
	// part 1; the encoder replays open parts in index order so the wire deltas
	// for part 1 precede part 2 while IR order keeps arrival order.
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_gap", "in_progress"),
		reasoningDeltaEvent(0, "rs_gap", 0, "p0"),
		reasoningDeltaEvent(0, "rs_gap", 2, "p2"),
		reasoningItemDoneEvent(0, "rs_gap", "completed", nil,
			`[{"type":"summary_text","text":"p0"},{"type":"summary_text","text":"p1"},{"type":"summary_text","text":"p2"}]`),
		completedEvent(finalOutput(reasoningFinalItem("rs_gap", "completed", nil,
			`[{"type":"summary_text","text":"p0"},{"type":"summary_text","text":"p1"},{"type":"summary_text","text":"p2"}]`)), ""),
	)
	evs, encoded, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatalf("mid-item gap err: %v", err)
	}
	var got []string
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta {
			got = append(got, itoa(ev.SummaryIndex)+":"+ev.Text)
		}
	}
	// IR keeps arrival order for deltas that already streamed; the gap text is
	// recovered from the snapshot, before the item closes.
	want := []string{"0:p0", "2:p2", "1:p1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("decoded deltas = %q want %q", got, want)
	}
	// The wire replays already-streamed bytes in arrival order and inserts the
	// recovered gap part before the item closes.
	wire := summaryDeltaStrings(t, encoded)
	wantWire := []string{"rs_gap@0:p0", "rs_gap@2:p2", "rs_gap@1:p1"}
	if strings.Join(wire, "|") != strings.Join(wantWire, "|") {
		t.Fatalf("wire deltas = %q want %q", wire, wantWire)
	}
	dones := summaryDoneTexts(t, encoded)
	wantDone := []string{"p0", "p2", "p1"}
	if strings.Join(dones, "|") != strings.Join(wantDone, "|") {
		t.Fatalf("done texts = %q want %q", dones, wantDone)
	}
	if final := finalSummaryTexts(t, encoded, 0); len(final) != 3 || final[0] != "p0" || final[1] != "p1" || final[2] != "p2" {
		t.Fatalf("final summary = %q", final)
	}
}

func TestSummaryAppendNoncontiguousFinalIndicesSuffixOnce(t *testing.T) {
	// Streamed deltas jump 0 -> 3 -> 6. The terminal array carries the missing
	// low indices; every missing part must be emitted once and only once, and
	// a repeated completion must not duplicate them.
	summary := `[{"type":"summary_text","text":"s0"},{"text":""},{"text":""},{"type":"summary_text","text":"s3"},{"text":""},{"text":""},{"type":"summary_text","text":"s6"}]`
	items := finalOutput(
		reasoningFinalItem("rs_n", "completed", nil, summary),
		completedItem("fc_n", "call_n", "search", `{}`),
		reasoningFinalItem("rs_m2", "completed", nil, `[{"type":"summary_text","text":"z"}]`),
	)
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_n", "in_progress"),
		reasoningDeltaEvent(0, "rs_n", 0, "s0"),
		reasoningDeltaEvent(0, "rs_n", 3, "s3"),
		reasoningDeltaEvent(0, "rs_n", 6, "s6"),
		completedEvent(items, ""),
		completedEvent(items, ""),
	)
	evs, encoded, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatalf("noncontiguous final indices err: %v", err)
	}
	var seq []string
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventReasoningDelta:
			seq = append(seq, itoa(ev.SummaryIndex)+":"+ev.Text)
		case ir.EventReasoningItemDone:
			seq = append(seq, "done")
		case ir.EventToolCallStart:
			seq = append(seq, "tool")
		}
	}
	// The gap parts stay empty in the snapshot, so no suffix is recoverable
	// there; the trailing final-only item must still emit exactly once.
	want := []string{"0:s0", "3:s3", "6:s6", "done", "tool", "0:z", "done"}
	got := strings.Join(seq, ",")
	if got != strings.Join(want, ",") {
		t.Fatalf("sequence = %q", got)
	}
	wire := summaryDeltaStrings(t, encoded)
	wantWire := []string{"rs_n@0:s0", "rs_n@3:s3", "rs_n@6:s6", "rs_m2@0:z"}
	if strings.Join(wire, "|") != strings.Join(wantWire, "|") {
		t.Fatalf("wire deltas = %q", wire)
	}
	if final := finalSummaryTexts(t, encoded, 0); len(final) != 7 || final[0] != "s0" || final[3] != "s3" || final[6] != "s6" {
		t.Fatalf("final summary = %q", final)
	}
	if final := finalSummaryTexts(t, encoded, 2); len(final) != 1 || final[0] != "z" {
		t.Fatalf("trailing item final = %q", final)
	}
}
