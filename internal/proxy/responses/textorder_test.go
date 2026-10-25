package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// textDeltaList returns the joined EventTextDelta bytes and their count, so a
// test asserts the actual emitted fragment order, not the final object.
func textDeltaList(t *testing.T, evs []ir.StreamEvent) (string, int) {
	t.Helper()
	var b strings.Builder
	n := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventTextDelta {
			b.WriteString(ev.Text)
			n++
		}
	}
	return b.String(), n
}

func finalMsgs(t *testing.T, evs []ir.StreamEvent) []string {
	t.Helper()
	var out []string
	for _, ev := range evs {
		if ev.Kind != ir.EventReasoningItemDone || ev.Reasoning == nil {
			continue
		}
		var b strings.Builder
		for _, p := range ev.Reasoning.Summary {
			b.WriteString(p.Text)
		}
		out = append(out, b.String())
	}
	return out
}

// sseTextDelta builds an identified output_text delta.
func sseTextDelta(itemID string, hasIdx bool, idx int, delta string) string {
	idxPart := ""
	if hasIdx {
		idxPart = fmt.Sprintf(`,"output_index":%d`, idx)
	}
	idPart := ""
	if itemID != "" {
		idPart = fmt.Sprintf(`,"item_id":%q`, itemID)
	}
	return fmt.Sprintf(`{"type":"response.output_text.delta"%s%s,"delta":%q}`, idPart, idxPart, delta)
}

func sseMsgFinalItem(id, text string) string {
	if text == "" {
		return fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[]}`, id)
	}
	return fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q}]}`, id, text)
}

// decodeText runs the bounded decoder with tiny text and pending budgets
// injected, so only the text tracking under test can fail.
func decodeText(t *testing.T, body string, maxBytes, maxItems, maxPendingBytes, maxPendingEvents int) ([]ir.StreamEvent, error) {
	t.Helper()
	var out []ir.StreamEvent
	err := decodeStreamBounded(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}, maxRetainedToolArgBytes, maxTrackedToolCalls, maxBytes, maxItems, maxPendingBytes, maxPendingEvents, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts)
	return out, err
}

// newTextDecoder constructs a decoder with injected text and pending budgets
// so limit and release behavior can be asserted through its state.
func newTextDecoder(emit func(ir.StreamEvent) error, maxBytes, maxItems, maxPendingBytes, maxPendingEvents int) *responsesToolDecoder {
	d := &responsesToolDecoder{
		emit: emit, ensureStarted: func() error { return nil },
		maxTextBytes: maxBytes, maxTextItems: maxItems,
		maxPendingBytes: maxPendingBytes, maxPendingEvents: maxPendingEvents,
	}
	d.reasoning = newResponsesReasoningDecoder(emit, d.ensureStarted, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts)
	return d
}

func textDeltaEnv(t *testing.T, frame string) *streamEnvelope {
	t.Helper()
	var env streamEnvelope
	if err := json.Unmarshal([]byte(frame), &env); err != nil {
		t.Fatal(err)
	}
	return &env
}

// TestDecodeStreamPerMessageTextOrder covers BLOCKER3: per-message recovered
// suffixes must appear in emitted EventTextDelta order, never ahead of a still
// open earlier message, and each message joins once.
func TestDecodeStreamPerMessageTextOrder(t *testing.T) {
	completed := func(items ...string) string {
		return sseData(completedEvent(finalOutput(items...), ""))
	}

	t.Run("interleaved suffix after later message", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hel"),
			sseTextDelta("msg_b", false, 1, "world"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "hello"), sseMsgFinalItem("msg_b", "world")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "helloworld" || n != 3 {
			t.Fatalf("text deltas joined = %q (%d deltas), want helloworld in 3 fragments", got, n)
		}
	})

	t.Run("later message closes first", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hel"),
			sseTextDelta("msg_b", false, 1, "wo"),
			sseTextDelta("msg_b", false, 1, "rld"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "hello"), sseMsgFinalItem("msg_b", "world")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "helloworld" || n != 3 {
			t.Fatalf("text deltas joined = %q (%d deltas), want helloworld in 3 fragments", got, n)
		}
	})

	t.Run("final-only messages emit in output order", func(t *testing.T) {
		body := completed(
			sseMsgFinalItem("msg_a", "first"),
			sseMsgFinalItem("msg_b", "second"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "firstsecond" || n != 2 {
			t.Fatalf("final-only deltas = %q (%d), want first then second", got, n)
		}
	})

	t.Run("no final snapshot keeps streamed deltas", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hel"),
			sseTextDelta("msg_b", false, 1, "world"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "helworld" || n != 2 {
			t.Fatalf("streamed-only deltas = %q (%d), want helworld", got, n)
		}
	})

	t.Run("identical text distinct ids stay separate", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "dup"),
			sseTextDelta("msg_b", false, 1, "dup"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "dup"), sseMsgFinalItem("msg_b", "dup")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "dupdup" || n != 2 {
			t.Fatalf("identical-text deltas = %q (%d), want dupdup in 2 fragments", got, n)
		}
	})

	t.Run("unkeyed legacy delta skips final recovery", func(t *testing.T) {
		body := sseData(
			sseTextDelta("", false, 0, "legacy"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "legacysnap")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "legacy" || n != 1 {
			t.Fatalf("unkeyed deltas = %q (%d), want legacy only, no guessed suffix", got, n)
		}
	})

	t.Run("final only after unkeyed", func(t *testing.T) {
		body := sseData(
			sseTextDelta("", false, 0, "legacy"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "snap")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := textDeltaList(t, evs)
		if got != "legacy" {
			t.Fatalf("deltas = %q, want legacy only", got)
		}
	})

	t.Run("sparse output indices with recovered suffix", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", true, 7, "one"),
			sseTextDelta("msg_b", true, 3, "two"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "onemore"), sseMsgFinalItem("msg_b", "two")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "onemoretwo" || n != 3 {
			t.Fatalf("sparse-index deltas = %q (%d), want onemoretwo in 3 fragments", got, n)
		}
	})

	t.Run("delta output_index matches final ordinal", func(t *testing.T) {
		// Streamed index 0 is also final ordinal 0: identity by index binds it.
		body := sseData(
			sseTextDelta("", true, 0, "one"),
			sseTextDelta("", true, 1, "two"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "onemore"), sseMsgFinalItem("msg_b", "two")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "onemoretwo" || n != 3 {
			t.Fatalf("index-matched deltas = %q (%d), want onemoretwo", got, n)
		}
	})

	t.Run("message done releases queued later text", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hel"),
			sseTextDelta("msg_b", false, 1, "world"),
			`{"type":"response.output_item.done","output_index":0,"item":`+sseMsgFinalItem("msg_a", "hello")+`}`,
			`{"type":"response.output_item.done","output_index":1,"item":`+sseMsgFinalItem("msg_b", "world")+`}`,
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "hello"), sseMsgFinalItem("msg_b", "world")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "helloworld" || n != 3 {
			t.Fatalf("done-release deltas = %q (%d), want helloworld in 3 fragments", got, n)
		}
	})

	t.Run("output text done releases queued later text", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hel"),
			sseTextDelta("msg_b", false, 1, "world"),
			`{"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":0,"text":"hello"}`,
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "hello"), sseMsgFinalItem("msg_b", "world")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "helloworld" || n != 3 {
			t.Fatalf("text-done deltas = %q (%d), want helloworld in 3 fragments", got, n)
		}
	})

	t.Run("later part done does not contradict first part", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "part1"),
			`{"type":"response.output_text.done","item_id":"msg_a","content_index":0,"text":"part1"}`,
			sseTextDelta("msg_a", false, 0, "part2"),
			`{"type":"response.content_part.done","item_id":"msg_a","content_index":1,"part":{"type":"output_text","text":"part2"}}`,
			completedEvent(finalOutput(`{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"part1"},{"type":"output_text","text":"part2"}]}`), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "part1part2" || n != 2 {
			t.Fatalf("part-done deltas = %q (%d), want part1part2 in 2 fragments", got, n)
		}
	})

	t.Run("reasoning final before already-wired message is rejected", func(t *testing.T) {
		// Streamed index 0, but the final array has reasoning first, so message
		// A is final ordinal 1. The message text already reached the client, so
		// the reasoning cannot be inserted ahead of it retroactively. The
		// offending reasoning frames must not be emitted.
		body := sseData(
			sseTextDelta("msg_a", true, 0, "A"),
			completedEvent(finalOutput(
				`{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"R"}]}`,
				sseMsgFinalItem("msg_a", "A"),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
			t.Fatalf("err = %v, want conflicting message text order", err)
		}
		for _, ev := range evs {
			switch ev.Kind {
			case ir.EventReasoningDelta, ir.EventReasoningItemDone, ir.EventFinish:
				t.Fatalf("offending reasoning frame or finish emitted: %+v", ev)
			}
		}
	})

	t.Run("streamed index need not equal final ordinal", func(t *testing.T) {
		// Streamed index 0, but the final array has reasoning first, so message
		// A is final ordinal 1. Index 0 must not be treated as the ordinal, and
		// the reasoning at ordinal 0 still precedes the final-only message.
		body := sseData(
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_a","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			completedEvent(finalOutput(
				`{"id":"rs_1","type":"reasoning","status":"completed","summary":[]}`,
				sseMsgFinalItem("msg_a", "A"),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "A" || n != 1 {
			t.Fatalf("deltas = %q (%d), want A once", got, n)
		}
	})

	t.Run("mixed message reasoning tool order", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "A"),
			sseTextDelta("msg_b", false, 2, "B"),
			completedEvent(finalOutput(
				sseMsgFinalItem("msg_a", "A"),
				`{"id":"rs_1","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"r"}]}`,
				completedItem("fc_1", "call_1", "read", "{}"),
				sseMsgFinalItem("msg_b", "B"),
			), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "AB" || n != 2 {
			t.Fatalf("mixed deltas = %q (%d), want A then B, no reordered suffix", got, n)
		}
		if msgs := finalMsgs(t, evs); len(msgs) != 1 || msgs[0] != "r" {
			t.Fatalf("final reasoning snapshot = %v, want [r]", msgs)
		}
	})

	t.Run("empty message final between streamed items", func(t *testing.T) {
		// B's final snapshot is empty and explicitly contradictory to its
		// streamed bytes; recovery must not guess and must reject.
		body := sseData(
			sseTextDelta("msg_a", false, 0, "A"),
			sseTextDelta("msg_b", false, 1, "B"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "A"), sseMsgFinalItem("msg_b", "")), ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("changed terminal text is rejected", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "A"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "B")), ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("multiple content parts join once", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "part1"),
			sseTextDelta("msg_a", false, 0, "part2"),
			`{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"part1"},{"type":"output_text","text":"part2"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "part1part2" || n != 2 {
			t.Fatalf("multi-part deltas = %q (%d), want part1part2 in 2 fragments", got, n)
		}
	})

	t.Run("repeat terminal identical final is free", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "A"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "AB")), ""),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "AB")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "AB" || n != 2 {
			t.Fatalf("repeat-terminal deltas = %q (%d), want AB once", got, n)
		}
	})

	t.Run("repeat terminal with growth is rejected", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "A"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "AB")), ""),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "ABC")), ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("new message after terminal rejected", func(t *testing.T) {
		body := sseData(
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "A")), ""),
			sseTextDelta("msg_b", false, 0, "late"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message item identity", err)
		}
	})
}

// TestDecodeStreamTextOrderFinalOnlyAndCollisions covers final-only messages
// around streamed messages and identity collisions in distinct namespaces.
func TestDecodeStreamTextOrderFinalOnlyAndCollisions(t *testing.T) {
	t.Run("final-only after streamed joins after suffix", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hel"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "hello"), sseMsgFinalItem("msg_f", "tail")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		got, n := textDeltaList(t, evs)
		if got != "hellotail" || n != 3 {
			t.Fatalf("deltas = %q (%d), want hellotail in 3 fragments", got, n)
		}
	})

	t.Run("final-only before emitted streamed text is rejected", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", false, 0, "hello"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_f", "pre"), sseMsgFinalItem("msg_a", "hello")), ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message text order") {
			t.Fatalf("err = %v, want conflicting message text order", err)
		}
	})

	t.Run("same item id at two indices is rejected", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", true, 0, "x"),
			sseTextDelta("msg_a", true, 1, "y"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message item identity") {
			t.Fatalf("err = %v, want conflicting message item identity", err)
		}
	})

	t.Run("same index with two ids is rejected", func(t *testing.T) {
		body := sseData(
			sseTextDelta("msg_a", true, 0, "x"),
			sseTextDelta("msg_b", true, 0, "y"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message item identity") {
			t.Fatalf("err = %v, want conflicting message item identity", err)
		}
	})

	t.Run("final ordinal reused by a second id is rejected", func(t *testing.T) {
		body := sseData(
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "A")), ""),
			completedEvent(finalOutput(sseMsgFinalItem("msg_b", "B")), ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting message item identity") {
			t.Fatalf("err = %v, want conflicting message item identity", err)
		}
	})
}

// TestDecodeStreamToEncodeStreamTextOrder covers the decode to EncodeStream
// path so the fixed event order survives response re-encoding, not just the
// final object.
func TestDecodeStreamToEncodeStreamTextOrder(t *testing.T) {
	body := sseData(
		sseTextDelta("msg_a", false, 0, "hel"),
		sseTextDelta("msg_b", false, 1, "world"),
		completedEvent(finalOutput(sseMsgFinalItem("msg_a", "hello"), sseMsgFinalItem("msg_b", "world")), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	wire := runQueueEvents(t, evs, true)
	if got := strings.Join(queuedTextDeltas(t, wire), ""); got != "helloworld" {
		t.Fatalf("encoded text deltas = %q, want helloworld", got)
	}
	final := finalResponseObject(t, wire)
	output, _ := final["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("encoded output = %+v, want one merged message", output)
	}
	msg, _ := output[0].(map[string]any)
	if got := queuedSummaryTextOfContent(t, msg); len(got) != 1 || got[0] != "helloworld" {
		t.Fatalf("encoded message content = %v, want helloworld", msg["content"])
	}
}

// TestDecodeStreamTextOverlimit covers text byte/item and pending byte/event
// budgets. An offending event must not be emitted and must not stay retained.
func TestDecodeStreamTextOverlimit(t *testing.T) {
	t.Run("text byte limit rejects offending delta", func(t *testing.T) {
		_, err := decodeText(t, sseData(sseTextDelta("msg_a", false, 0, strings.Repeat("x", 64))), 32, 8, maxPendingTextBytes, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "message text too large") {
			t.Fatalf("err = %v, want message text too large", err)
		}
	})
	t.Run("suffix over byte limit rejects final", func(t *testing.T) {
		_, err := decodeText(t, sseData(
			sseTextDelta("msg_a", false, 0, strings.Repeat("x", 40)),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", strings.Repeat("x", 80))), ""),
		), 64, 8, maxPendingTextBytes, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "message text too large") {
			t.Fatalf("err = %v, want message text too large", err)
		}
	})
	t.Run("text item limit", func(t *testing.T) {
		var events []string
		for i := 0; i < 4; i++ {
			events = append(events, sseTextDelta(fmt.Sprintf("msg_%d", i), false, i, "a"))
		}
		_, err := decodeText(t, sseData(events...), 4096, 2, maxPendingTextBytes, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "too many message text items") {
			t.Fatalf("err = %v, want too many message text items", err)
		}
	})
	t.Run("pending event limit rejects offending delta", func(t *testing.T) {
		_, err := decodeText(t, sseData(
			sseTextDelta("msg_a", false, 0, "a"),
			sseTextDelta("msg_b", false, 1, "b"),
			sseTextDelta("msg_b", false, 1, "c"),
			sseTextDelta("msg_b", false, 1, "OFFENDING"),
		), 4096, 8, maxPendingTextBytes, 2)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v, want pending message text exceeded limit", err)
		}
	})
	t.Run("pending byte limit rejects offending delta", func(t *testing.T) {
		_, err := decodeText(t, sseData(
			sseTextDelta("msg_a", false, 0, "a"),
			sseTextDelta("msg_b", false, 1, "OFFENDING-BYTES"),
		), 4096, 8, 8, maxPendingTextEvents)
		if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
			t.Fatalf("err = %v, want pending message text exceeded limit", err)
		}
	})
	t.Run("overlimit event not emitted or retained", func(t *testing.T) {
		cases := []struct {
			name            string
			maxPendingBytes int
			maxPendingItems int
			offending       string
		}{
			{"pending byte limit", 60, maxPendingTextEvents, "OFFENDING-BYTES"},
			{"pending event limit", maxPendingTextBytes, 1, "OFFENDING"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var out []ir.StreamEvent
				d := newTextDecoder(func(ev ir.StreamEvent) error {
					out = append(out, ev)
					return nil
				}, maxRetainedTextBytes, maxTrackedTextItems, tc.maxPendingBytes, tc.maxPendingItems)
				if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_a", false, 0, "a")), false); err != nil {
					t.Fatal(err)
				}
				if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_b", false, 1, "ok")), false); err != nil {
					t.Fatal(err)
				}
				err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_b", false, 1, tc.offending)), false)
				if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
					t.Fatalf("err = %v, want pending message text exceeded limit", err)
				}
				for _, ev := range out {
					if strings.Contains(ev.Text, tc.offending) {
						t.Fatalf("offending delta emitted: %+v", ev)
					}
				}
				if d.queueBytes != len("ok")+textQueueEntryOverhead || len(d.textQueue) != 1 || d.textQueueEvents != 1 {
					t.Fatalf("queue changed after rejection: bytes=%d queue=%d events=%d", d.queueBytes, len(d.textQueue), d.textQueueEvents)
				}
				// The offending delta must not be retained in the message builder.
				if strings.Contains(d.texts[1].text.String(), tc.offending) {
					t.Fatalf("offending delta retained in message text: %q", d.texts[1].text.String())
				}
			})
		}
	})
	t.Run("release zeros queued references", func(t *testing.T) {
		var out []ir.StreamEvent
		d := newTextDecoder(func(ev ir.StreamEvent) error {
			out = append(out, ev)
			return nil
		}, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
		if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_a", false, 0, "hel")), false); err != nil {
			t.Fatal(err)
		}
		if err := d.observeTextDelta(textDeltaEnv(t, sseTextDelta("msg_b", false, 1, "world")), false); err != nil {
			t.Fatal(err)
		}
		if d.queueBytes == 0 || d.textQueueEvents == 0 {
			t.Fatal("queued work was not charged")
		}
		done := sseMsgFinalItem("msg_a", "hello")
		_ = done
		if err := d.observeMessageDone(&streamEnvelope{OutputIndex: 0, OutputIndexSet: true, Item: &streamItem{ID: "msg_a", Type: "message", Content: []respOutputContent{{Type: "output_text", Text: "hello"}}}}); err != nil {
			t.Fatal(err)
		}
		if d.queueBytes != 0 || len(d.textQueue) != 0 || d.textQueueEvents != 0 {
			t.Fatalf("queue not released: bytes=%d queue=%d events=%d", d.queueBytes, len(d.textQueue), d.textQueueEvents)
		}
		var b strings.Builder
		for _, ev := range out {
			if ev.Kind == ir.EventTextDelta {
				b.WriteString(ev.Text)
			}
		}
		if b.String() != "helloworld" {
			t.Fatalf("deltas = %q, want helloworld", b.String())
		}
	})
}

// sseMsgFinalItemParts builds a terminal output-array message with several
// output_text content parts, so a multipart streamed message matches its final
// snapshot by part instead of by the concatenated text.
func sseMsgFinalItemParts(id string, parts ...string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[`, id))
	for i, p := range parts {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(sseTextContent(p))
	}
	b.WriteString("]}")
	return b.String()
}

// TestDecodeStreamClosedLaterMessageBlockedByOpenEarlier reproduces the
// ordering defect: a later message's terminal snapshot must not release its
// queued text while an earlier message is still open. Before the fix the
// terminal flag shortcut released world before the earlier message grew,
// producing helworldlo instead of helloworld.
func TestDecodeStreamClosedLaterMessageBlockedByOpenEarlier(t *testing.T) {
	body := sseData(
		sseTextDeltaCI("msg_a", 0, 0, "hel"),
		sseTextDeltaCI("msg_b", 1, 0, "world"),
		sseItemDoneMessage(1, "msg_b", sseTextContent("world")),
		sseTextDone("msg_a", 0, 0, "hel"),
		sseTextDeltaCI("msg_a", 0, 1, "lo"),
		sseItemDoneMessage(0, "msg_a", sseTextContent("hel"), sseTextContent("lo")),
		completedEvent(finalOutput(
			sseMsgFinalItemParts("msg_a", "hel", "lo"),
			sseMsgFinalItem("msg_b", "world"),
		), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	got, n := textDeltaList(t, evs)
	if got != "helloworld" || n != 3 {
		t.Fatalf("deltas = %q (%d), want helloworld (hel, lo, world)", got, n)
	}
}

// TestDecodeStreamClosedLaterMessageEncodeOrder runs the same defect through
// decode then encode, so the recovered suffix order survives the Responses
// output, not only the IR event order.
func TestDecodeStreamClosedLaterMessageEncodeOrder(t *testing.T) {
	body := sseData(
		sseTextDeltaCI("msg_a", 0, 0, "hel"),
		sseTextDeltaCI("msg_b", 1, 0, "world"),
		sseItemDoneMessage(1, "msg_b", sseTextContent("world")),
		sseTextDone("msg_a", 0, 0, "hel"),
		sseTextDeltaCI("msg_a", 0, 1, "lo"),
		sseItemDoneMessage(0, "msg_a", sseTextContent("hel"), sseTextContent("lo")),
		completedEvent(finalOutput(
			sseMsgFinalItemParts("msg_a", "hel", "lo"),
			sseMsgFinalItem("msg_b", "world"),
		), ""),
	)
	evs, wire, err := decodeThenEncode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	got, n := textDeltaList(t, evs)
	if got != "helloworld" || n != 3 {
		t.Fatalf("decoded deltas = %q (%d), want helloworld in 3 fragments", got, n)
	}
	if encoded := strings.Join(queuedTextDeltas(t, wire), ""); encoded != "helloworld" {
		t.Fatalf("encoded deltas = %q, want helloworld", encoded)
	}
	if msgs := strings.Join(finalMessageTexts(t, wire), ""); msgs != "helloworld" {
		t.Fatalf("encoded message texts = %v, want helloworld", finalMessageTexts(t, wire))
	}
}

// TestDecodeStreamClosedLaterMessageDrainExactlyOnce asserts on decoder state
// that a closed later message stays queued through an earlier part done and
// drains exactly once when the earlier message finally closes.
func TestDecodeStreamClosedLaterMessageDrainExactlyOnce(t *testing.T) {
	var out []ir.StreamEvent
	d := newTextDecoder(func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_a", 0, 0, "hel")), false); err != nil {
		t.Fatal(err)
	}
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_b", 1, 0, "world")), false); err != nil {
		t.Fatal(err)
	}
	if err := d.observeMessageDone(&streamEnvelope{OutputIndex: 1, OutputIndexSet: true, Item: &streamItem{ID: "msg_b", Type: "message", Content: []respOutputContent{{Type: "output_text", Text: "world"}}}}); err != nil {
		t.Fatal(err)
	}
	if len(d.textQueue) == 0 {
		t.Fatal("closed later message released before earlier closed")
	}
	if err := d.observeTextDone(textDeltaEnv(t, sseTextDone("msg_a", 0, 0, "hel")), false); err != nil {
		t.Fatal(err)
	}
	if len(d.textQueue) == 0 {
		t.Fatal("closed later message released at earlier part done")
	}
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_a", 0, 1, "lo")), false); err != nil {
		t.Fatal(err)
	}
	if err := d.observeMessageDone(&streamEnvelope{OutputIndex: 0, OutputIndexSet: true, Item: &streamItem{ID: "msg_a", Type: "message", Content: []respOutputContent{{Type: "output_text", Text: "hel"}, {Type: "output_text", Text: "lo"}}}}); err != nil {
		t.Fatal(err)
	}
	if len(d.textQueue) != 0 || d.queueBytes != 0 || d.textQueueEvents != 0 {
		t.Fatalf("queue not released: bytes=%d queue=%d events=%d", d.queueBytes, len(d.textQueue), d.textQueueEvents)
	}
	var b strings.Builder
	deltaCount := 0
	for _, ev := range out {
		if ev.Kind == ir.EventTextDelta {
			b.WriteString(ev.Text)
			deltaCount++
		}
	}
	if b.String() != "helloworld" || deltaCount != 3 {
		t.Fatalf("deltas = %q (%d), want helloworld once", b.String(), deltaCount)
	}
}

// TestDecodeStreamThreeMessagesClosedOutOfOrder closes two later messages
// before the first, recovers a second part on the first, and requires the
// release order A, then B, then C.
func TestDecodeStreamThreeMessagesClosedOutOfOrder(t *testing.T) {
	finalA := `{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"A"},{"type":"output_text","text":"Z"}]}`
	body := sseData(
		sseTextDeltaCI("msg_a", 0, 0, "A"),
		sseTextDeltaCI("msg_b", 1, 0, "B"),
		sseTextDeltaCI("msg_c", 2, 0, "C"),
		sseItemDoneMessage(2, "msg_c", sseTextContent("C")),
		sseItemDoneMessage(1, "msg_b", sseTextContent("B")),
		sseTextDone("msg_a", 0, 0, "A"),
		sseTextDeltaCI("msg_a", 0, 1, "Z"),
		sseItemDoneMessage(0, "msg_a", sseTextContent("A"), sseTextContent("Z")),
		completedEvent(finalOutput(finalA, sseMsgFinalItem("msg_b", "B"), sseMsgFinalItem("msg_c", "C")), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	got, n := textDeltaList(t, evs)
	if got != "AZBC" || n != 4 {
		t.Fatalf("deltas = %q (%d), want AZBC (A, Z, B, C)", got, n)
	}
}

// TestDecodeStreamInterleavedQueueNoEarlyRelease keeps queued tool and
// reasoning work behind a closed later message and an open first message. The
// part done on the first message must not release any of it.
func TestDecodeStreamInterleavedQueueNoEarlyRelease(t *testing.T) {
	enc := "cipher"
	body := sseData(
		sseTextDeltaCI("msg_a", 0, 0, "A"),
		sseTextDeltaCI("msg_b", 1, 0, "B"),
		sseItemDoneMessage(1, "msg_b", sseTextContent("B")),
		addedEvent(2, "fc_1", "call_1", "read", `{}`),
		reasoningAddedEvent(3, "rs_1", "in_progress"),
		reasoningDeltaEvent(3, "rs_1", 0, "R"),
		sseTextDone("msg_a", 0, 0, "A"),
		sseTextDeltaCI("msg_a", 0, 1, "Z"),
		sseItemDoneMessage(0, "msg_a", sseTextContent("A"), sseTextContent("Z")),
		completedEvent(finalOutput(
			sseMsgFinalItemParts("msg_a", "A", "Z"),
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
		"text:A", "text:Z", "text:B", "toolstart", `toolargs:{}`, "reason:R", "reasondone")
}

// TestDecodeStreamOpenMessageDeltasStayLive confirms the fix does not defer the
// bytes of the current open message: its deltas emit immediately and leave no
// queued work behind.
func TestDecodeStreamOpenMessageDeltasStayLive(t *testing.T) {
	var out []ir.StreamEvent
	d := newTextDecoder(func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_a", 0, 0, "a")), false); err != nil {
		t.Fatal(err)
	}
	if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_a", 0, 0, "b")), false); err != nil {
		t.Fatal(err)
	}
	if len(d.textQueue) != 0 || d.queueBytes != 0 || d.textQueueEvents != 0 {
		t.Fatalf("open deltas queued: bytes=%d queue=%d", d.queueBytes, len(d.textQueue))
	}
	var b strings.Builder
	for _, ev := range out {
		if ev.Kind == ir.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	if b.String() != "ab" {
		t.Fatalf("deltas = %q, want ab", b.String())
	}
}
