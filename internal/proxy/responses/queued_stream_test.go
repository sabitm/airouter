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

// runQueueEvents encodes events and closes without Finish, so Close-time
// flushing is exercised separately from Finish-time flushing.
func runQueueEvents(t *testing.T, events []ir.StreamEvent, useFinish bool) string {
	t.Helper()
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range events {
		if err := enc.Encode(ev, w); err != nil {
			t.Fatal(err)
		}
	}
	if useFinish {
		if err := enc.Close(w); err != nil {
			t.Fatal(err)
		}
	} else if err := enc.Close(w); err != nil {
		t.Fatal(err)
	}
	return rec.Body.String()
}

func queuedSummaryText(t *testing.T, data map[string]any) []string {
	t.Helper()
	raw, _ := data["summary"].([]any)
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		m, _ := entry.(map[string]any)
		s, _ := m["text"].(string)
		out = append(out, s)
	}
	return out
}

func queuedWireIDs(t *testing.T, body string) []string {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var ids []string
	seen := map[string]bool{}
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var raw struct {
			ItemID string `json:"item_id"`
			Item   *struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(ev.Data, &raw) != nil {
			continue
		}
		if strings.HasPrefix(ev.Name, "response.reasoning") && raw.ItemID != "" && !seen[raw.ItemID] {
			seen[raw.ItemID] = true
			ids = append(ids, raw.ItemID)
		}
		if (ev.Name == "response.output_item.added" || ev.Name == "response.output_item.done") &&
			raw.Item != nil && raw.Item.Type == "reasoning" && raw.Item.ID != "" && !seen[raw.Item.ID] {
			seen[raw.Item.ID] = true
			ids = append(ids, raw.Item.ID)
		}
	}
	return ids
}

// queuedSummaryDeltas returns the emitted reasoning summary delta text in wire
// order, so the actual fragment order is asserted rather than a final object.
func queuedSummaryDeltas(t *testing.T, body string) []string {
	t.Helper()
	frames := parseResponsesStream(t, body)
	var out []string
	for _, f := range frames {
		if f.Name != "response.reasoning_summary_text.delta" {
			continue
		}
		s, _ := f.Data["delta"].(string)
		out = append(out, s)
	}
	return out
}

// queuedStableItemIDs reports that each reasoning item keeps one item ID and
// one output index across its whole lifecycle, and that item frames are
// contiguous: once another item starts, the previous id must not resume.
func queuedStableItemIDs(t *testing.T, body string) {
	t.Helper()
	curID := ""
	curIdx := -1
	seen := map[string]bool{}
	for _, f := range parseResponsesStream(t, body) {
		switch f.Name {
		case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
			got, _ := f.Data["item_id"].(string)
			idx := -1
			if v, ok := f.Data["output_index"].(float64); ok {
				idx = int(v)
			}
			if got != curID {
				if seen[got] {
					t.Fatalf("reasoning frames interleave items: %q reappears after %q", got, curID)
				}
				seen[curID] = true
				curID = got
				curIdx = idx
			}
			if idx != curIdx {
				t.Fatalf("output index changed mid-lifecycle: %d then %d for %s", curIdx, idx, got)
			}
		}
	}
}

// TestEncodeQueuedReasoningSummaryOrderFIFO covers BLOCKER1: a later identity
// event must never overtake queued predecessors. Index-only summary 'a', then
// index-only fragment 'b', then the done snapshot must give wire order a,b and
// no generated ID. The ID-bearing delta variant must keep order a,b,c.
func TestEncodeQueuedReasoningSummaryOrderFIFO(t *testing.T) {
	t.Run("done carries identity", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "a"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "b"},
			{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_q", ReasoningIndexSet: true, ReasoningIndex: 2, Reasoning: &ir.ResponsesReasoning{
				ID: "rs_q", Status: "completed",
				Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "ab"}},
			}},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}
		body := runQueueEvents(t, events, true)
		if ids := queuedWireIDs(t, body); len(ids) != 1 || ids[0] != "rs_q" {
			t.Fatalf("wire ids = %v, want only source id rs_q", ids)
		}
		// 'b' must not replay after the done frame; the fragments join to ab in order.
		if got := strings.Join(queuedSummaryDeltas(t, body), ""); got != "ab" {
			t.Fatalf("summary deltas joined = %q, want ab in order", got)
		}
		final := finalResponseObject(t, body)
		output, _ := final["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("final output = %+v", output)
		}
		first, _ := output[0].(map[string]any)
		if first["id"] != "rs_q" {
			t.Fatalf("source id lost: %+v", first)
		}
		if got := queuedSummaryText(t, first); len(got) != 1 || got[0] != "ab" {
			t.Fatalf("final summary = %v", got)
		}
	})
	t.Run("id delta carries identity", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "a"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "b"},
			{Kind: ir.EventReasoningDelta, ReasoningID: "rs_q", ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "c"},
			{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_q", ReasoningIndexSet: true, ReasoningIndex: 2, Reasoning: &ir.ResponsesReasoning{
				ID: "rs_q", Status: "completed",
				Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "abc"}},
			}},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}
		body := runQueueEvents(t, events, true)
		if ids := queuedWireIDs(t, body); len(ids) != 1 || ids[0] != "rs_q" {
			t.Fatalf("wire ids = %v, want only source id rs_q", ids)
		}
		// The id-bearing delta must not overtake queued a,b: fragments join to abc.
		if got := strings.Join(queuedSummaryDeltas(t, body), ""); got != "abc" {
			t.Fatalf("summary deltas joined = %q, want abc in order", got)
		}
	})
}

// TestEncodeQueuedReasoningMixedItemsOrder covers BLOCKER1 across multiple
// unresolved items with intervening text, tool calls, and fragments: every
// queued predecessor must drain before later work, in arrival order.
func TestEncodeQueuedReasoningMixedItemsOrder(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		// Unresolved reasoning A keyed by index 0.
		{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "A1"},
		// Unresolved reasoning B keyed by index 1.
		{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 1, SummaryIndex: 0, Text: "B1"},
		// Intervening message text and tool fragments, all queued behind A and B.
		{Kind: ir.EventTextDelta, Text: "t1"},
		{Kind: ir.EventTextDelta, Text: "t2"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"x":`},
		{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1", ToolName: "alpha"},
		{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `1}`},
		// Identity for A arrives from a done snapshot.
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_a", ReasoningIndexSet: true, ReasoningIndex: 0, Reasoning: &ir.ResponsesReasoning{
			ID: "rs_a", Status: "completed",
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "A1"}},
		}},
		// Identity for B arrives from its own done snapshot.
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_b", ReasoningIndexSet: true, ReasoningIndex: 1, Reasoning: &ir.ResponsesReasoning{
			ID: "rs_b", Status: "completed",
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "B1"}},
		}},
		{Kind: ir.EventTextDelta, Text: "t3"},
		{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
	}
	body := runQueueEvents(t, events, true)
	if ids := queuedWireIDs(t, body); len(ids) != 2 || ids[0] != "rs_a" || ids[1] != "rs_b" {
		t.Fatalf("wire ids = %v, want rs_a then rs_b in arrival order", ids)
	}
	names := sseEventNames(t, body)
	// Reasoning A and B must not swap ahead of each other: fragments join in
	// arrival order A1,B1,B2.
	if got := strings.Join(queuedSummaryDeltas(t, body), ""); got != "A1B1" {
		t.Fatalf("summary deltas joined = %q, want A1B1 in FIFO order", got)
	}
	// First reasoning delta must come before first text delta.
	firstText := -1
	firstReasoning := -1
	firstToolDelta := -1
	for i, n := range names {
		if firstText < 0 && n == "response.output_text.delta" {
			firstText = i
		}
		if firstReasoning < 0 && n == "response.reasoning_summary_text.delta" {
			firstReasoning = i
		}
		if firstToolDelta < 0 && n == "response.function_call_arguments.delta" {
			firstToolDelta = i
		}
	}
	if firstReasoning < 0 || firstText < 0 || firstReasoning > firstText {
		t.Fatalf("queued text emitted before deferred reasoning: reasoning=%d text=%d", firstReasoning, firstText)
	}
	if firstToolDelta < firstText {
		t.Fatalf("tool delta emitted before queued text: tool=%d text=%d", firstToolDelta, firstText)
	}
	// Every reasoning item keeps one contiguous frame block with a stable id.
	queuedStableItemIDs(t, body)
	final := finalResponseObject(t, body)
	output, _ := final["output"].([]any)
	if len(output) != 5 {
		t.Fatalf("final output = %+v", output)
	}
	types := []string{}
	for _, o := range output {
		m, _ := o.(map[string]any)
		tt, _ := m["type"].(string)
		types = append(types, tt)
	}
	// Arrival order: reasoning A, reasoning B, queued text, tool, late text.
	// A text item after a tool opens a second message item; both must close.
	if types[0] != "reasoning" || types[1] != "reasoning" || types[2] != "message" || types[3] != "function_call" || types[4] != "message" {
		t.Fatalf("final output order = %v", types)
	}
	first, _ := output[0].(map[string]any)
	if first["id"] != "rs_a" {
		t.Fatalf("rs_a source id lost: %+v", first)
	}
	second, _ := output[1].(map[string]any)
	if second["id"] != "rs_b" {
		t.Fatalf("rs_b source id lost: %+v", second)
	}
	msg, _ := output[2].(map[string]any)
	if got := queuedSummaryTextOfContent(t, msg); len(got) != 1 || got[0] != "t1t2" {
		t.Fatalf("first message text = %v, want t1t2", msg["content"])
	}
	late, _ := output[4].(map[string]any)
	if got := queuedSummaryTextOfContent(t, late); len(got) != 1 || got[0] != "t3" {
		t.Fatalf("late message text = %v, want t3", late["content"])
	}
}

// TestEncodeQueuedReasoningStableSourceIDAcrossLifecycle covers one stable
// source ID from deferred delta through id-bearing done and the final object,
// and Close-time flushing of truly unresolved items.
func TestEncodeQueuedReasoningStableSourceIDAcrossLifecycle(t *testing.T) {
	t.Run("stable id across added delta done final", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 5, SummaryIndex: 0, Text: "s"},
			{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_stable", ReasoningIndexSet: true, ReasoningIndex: 5, Reasoning: &ir.ResponsesReasoning{
				ID: "rs_stable", Status: "completed",
				Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "s"}},
			}},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}
		body := runQueueEvents(t, events, true)
		// added, summary frames, done, and final must all carry rs_stable.
		if n := strings.Count(body, "rs_stable"); n < 3 {
			t.Fatalf("source id appears %d times, want stable id across lifecycle: %s", n, body)
		}
		for _, frame := range []string{"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done"} {
			if !strings.Contains(body, `"type":"`+frame+`"`) {
				t.Fatalf("missing frame %s: %s", frame, body)
			}
		}
		if ids := queuedWireIDs(t, body); len(ids) != 1 || ids[0] != "rs_stable" {
			t.Fatalf("wire ids = %v, want only rs_stable", ids)
		}
	})
	t.Run("close flushes unresolved item", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 3, SummaryIndex: 0, Text: "open"},
		}
		body := runQueueEvents(t, events, false)
		if got := queuedSummaryDeltas(t, body); len(got) != 1 || got[0] != "open" {
			t.Fatalf("close-time deltas = %q, want open", got)
		}
		final := finalResponseObject(t, body)
		output, _ := final["output"].([]any)
		if len(output) != 1 {
			t.Fatalf("final output = %+v", output)
		}
		first, _ := output[0].(map[string]any)
		if first["type"] != "reasoning" {
			t.Fatalf("final item = %+v", first)
		}
		if got := queuedSummaryText(t, first); len(got) != 1 || got[0] != "open" {
			t.Fatalf("close-time summary = %v", got)
		}
	})
}

// TestEncodeQueuedReasoningPendingLimits covers queue count and byte budgets,
// rejection without retention, and ref release after repeated drains.
func TestEncodeQueuedReasoningPendingLimits(t *testing.T) {
	t.Run("event count limit", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		rec := httptest.NewRecorder()
		w, ok := sse.NewWriter(rec)
		if !ok {
			t.Fatal("recorder does not support flushing")
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "resp_1"}, w); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "a"}, w); err != nil {
			t.Fatal(err)
		}
		var err error
		for i := 0; i < maxEncoderPendingEvents+1; i++ {
			err = enc.Encode(ir.StreamEvent{Kind: ir.EventTextDelta, Text: "PENDINGDATA"}, w)
			if err != nil {
				break
			}
		}
		if err == nil {
			t.Fatalf("pending event cap not enforced after %d queued text events", maxEncoderPendingEvents+1)
		}
		if strings.Contains(err.Error(), "PENDINGDATA") {
			t.Fatalf("error echoed payload: %v", err)
		}
		// Finish must still flush the retained queue in order.
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn}, w); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(queuedSummaryDeltas(t, rec.Body.String()), ""); got != "a" {
			t.Fatalf("deltas after flush joined = %q, want a", got)
		}
	})
	t.Run("byte limit counts queued payload", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		rec := httptest.NewRecorder()
		w, ok := sse.NewWriter(rec)
		if !ok {
			t.Fatal("recorder does not support flushing")
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "resp_1"}, w); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "a"}, w); err != nil {
			t.Fatal(err)
		}
		big := strings.Repeat("z", maxEncoderPendingBytes)
		err := enc.Encode(ir.StreamEvent{Kind: ir.EventTextDelta, Text: big}, w)
		if err == nil {
			t.Fatalf("pending byte cap not enforced for %d byte payload", len(big))
		}
		if strings.Contains(err.Error(), "zzz") {
			t.Fatalf("error echoed payload: %v", err)
		}
	})
	t.Run("rejected event not retained", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		rec := httptest.NewRecorder()
		w, ok := sse.NewWriter(rec)
		if !ok {
			t.Fatal("recorder does not support flushing")
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "resp_1"}, w); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "a"}, w); err != nil {
			t.Fatal(err)
		}
		// Queue one normal event behind the unresolved identity, then reject an
		// oversize one: the retained event must survive unchanged.
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventTextDelta, Text: "PENDING"}, w); err != nil {
			t.Fatal(err)
		}
		big := strings.Repeat("z", maxEncoderPendingBytes+1)
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventTextDelta, Text: big}, w); err == nil {
			t.Fatal("oversize event accepted")
		}
		want := encoderPendingEventBytes(ir.StreamEvent{Kind: ir.EventTextDelta, Text: "PENDING"})
		if len(enc.pendingEvents) != 1 || enc.pendingBytes != want {
			t.Fatalf("queue after rejection: events=%d bytes=%d, want 1 and %d", len(enc.pendingEvents), enc.pendingBytes, want)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn}, w); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(queuedSummaryDeltas(t, rec.Body.String()), ""); got != "a" {
			t.Fatalf("deltas after rejection joined = %q, want a", got)
		}
		final := finalResponseObject(t, rec.Body.String())
		output, _ := final["output"].([]any)
		if len(output) != 2 {
			t.Fatalf("final output = %+v", output)
		}
		first, _ := output[0].(map[string]any)
		if got := queuedSummaryText(t, first); len(got) != 1 || got[0] != "a" {
			t.Fatalf("final summary after rejection = %v", got)
		}
		msg, _ := output[1].(map[string]any)
		if got := queuedSummaryTextOfContent(t, msg); len(got) != 1 || got[0] != "PENDING" {
			t.Fatalf("final message after rejection = %v, want PENDING", msg["content"])
		}
	})
	t.Run("repeated drain releases queue refs", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		rec := httptest.NewRecorder()
		w, ok := sse.NewWriter(rec)
		if !ok {
			t.Fatal("recorder does not support flushing")
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventMessageStart, ID: "resp_1"}, w); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "a"}, w); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventReasoningDelta, ReasoningID: "rs_dr", ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "b"}, w); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn}, w); err != nil {
			t.Fatal(err)
		}
		if len(enc.pendingEvents) != 0 || enc.pendingBytes != 0 {
			t.Fatalf("queue not released after drain: events=%d bytes=%d", len(enc.pendingEvents), enc.pendingBytes)
		}
		if got := strings.Join(queuedSummaryDeltas(t, rec.Body.String()), ""); got != "ab" {
			t.Fatalf("deltas joined = %q, want ab in order", got)
		}
	})
}

// TestEncodeQueuedTextLifecycleCoversBlockedFinish covers BLOCKER2: Finish and
// Close emit the full message lifecycle exactly once for queued text, with
// correct text, no duplicate completed, and no suppression of terminal frames.
func TestEncodeQueuedTextLifecycleCoversBlockedFinish(t *testing.T) {
	t.Run("finish path", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "r"},
			{Kind: ir.EventTextDelta, Text: "q1"},
			{Kind: ir.EventTextDelta, Text: "q2"},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}
		body := runQueueEvents(t, events, true)
		names := sseEventNames(t, body)
		if n := strings.Count(body, `"type":"response.completed"`); n != 1 {
			t.Fatalf("completed frames = %d, want 1: %s", n, body)
		}
		if n := strings.Count(strings.Join(names, ","), "response.output_text.done"); n != 1 {
			t.Fatalf("output_text.done count = %d, want 1: %v", n, names)
		}
		if n := strings.Count(strings.Join(names, ","), "response.content_part.done"); n != 1 {
			t.Fatalf("content_part.done count = %d, want 1: %v", n, names)
		}
		// The message item must be closed exactly once with the queued text.
		var textDone, partDone, itemDone map[string]any
		for _, f := range parseResponsesStream(t, body) {
			switch f.Name {
			case "response.output_text.done":
				if textDone == nil {
					textDone = f.Data
				}
			case "response.content_part.done":
				if partDone == nil {
					partDone = f.Data
				}
			case "response.output_item.done":
				if item, _ := f.Data["item"].(map[string]any); item != nil && item["type"] == "message" {
					if itemDone == nil {
						itemDone = f.Data
					}
				}
			}
		}
		if textDone == nil || partDone == nil || itemDone == nil {
			t.Fatalf("queued text lifecycle incomplete: text=%v part=%v item=%v", textDone, partDone, itemDone)
		}
		if textDone["text"] != "q1q2" {
			t.Fatalf("output_text.done = %v, want q1q2", textDone["text"])
		}
		part, _ := partDone["part"].(map[string]any)
		if part == nil || part["text"] != "q1q2" {
			t.Fatalf("content_part.done = %v, want q1q2", partDone["part"])
		}
		item, _ := itemDone["item"].(map[string]any)
		if got := queuedSummaryTextOfContent(t, item); len(got) != 1 || got[0] != "q1q2" {
			t.Fatalf("output_item.done content = %v, want q1q2", item["content"])
		}
		// The terminal frames must agree with the queued deltas and final object.
		if got := queuedTextDeltas(t, body); len(got) != 2 || got[0] != "q1" || got[1] != "q2" {
			t.Fatalf("text deltas = %q, want q1 then q2", got)
		}
	})
	t.Run("close path", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "r"},
			{Kind: ir.EventTextDelta, Text: "c1"},
			{Kind: ir.EventTextDelta, Text: "c2"},
		}
		body := runQueueEvents(t, events, false)
		if n := strings.Count(body, `"type":"response.completed"`); n != 1 {
			t.Fatalf("completed frames = %d, want 1: %s", n, body)
		}
		names := sseEventNames(t, body)
		if n := strings.Count(strings.Join(names, ","), "response.output_text.done"); n != 1 {
			t.Fatalf("output_text.done count = %d, want 1: %v", n, names)
		}
		if got := queuedTextDeltas(t, body); len(got) != 2 || got[0] != "c1" || got[1] != "c2" {
			t.Fatalf("text deltas = %q, want c1 then c2", got)
		}
		var textDone map[string]any
		for _, f := range parseResponsesStream(t, body) {
			if f.Name == "response.output_text.done" {
				if textDone == nil {
					textDone = f.Data
				}
			}
		}
		if textDone == nil {
			t.Fatalf("close path missing output_text.done: %s", body)
		}
		if textDone["text"] != "c1c2" {
			t.Fatalf("close output_text.done = %v, want c1c2", textDone["text"])
		}
	})
}

// TestEncodeQueuedTextTransitions covers text to tool and text to reasoning
// transitions after queued text, so each item closes with its own text.
func TestEncodeQueuedTextTransitions(t *testing.T) {
	t.Run("text then tool", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "r"},
			{Kind: ir.EventTextDelta, Text: "call"},
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `{"a":`},
			{Kind: ir.EventToolCallStart, Index: 0, ToolID: "call_1", ToolName: "alpha"},
			{Kind: ir.EventToolCallDelta, Index: 0, ArgsFrag: `1}`},
			{Kind: ir.EventFinish, StopReason: ir.StopToolUse},
		}
		body := runQueueEvents(t, events, true)
		names := sseEventNames(t, body)
		if n := strings.Count(strings.Join(names, ","), "response.output_text.done"); n != 1 {
			t.Fatalf("output_text.done count = %d, want 1: %v", n, names)
		}
		if got := queuedTextDeltas(t, body); len(got) != 1 || got[0] != "call" {
			t.Fatalf("text deltas = %q, want call", got)
		}
		final := finalResponseObject(t, body)
		output, _ := final["output"].([]any)
		if len(output) != 3 {
			t.Fatalf("final output = %+v", output)
		}
		types := []string{}
		for _, o := range output {
			m, _ := o.(map[string]any)
			tt, _ := m["type"].(string)
			types = append(types, tt)
		}
		if types[0] != "reasoning" || types[1] != "message" || types[2] != "function_call" {
			t.Fatalf("final output order = %v", types)
		}
		msg, _ := output[1].(map[string]any)
		if got := queuedSummaryTextOfContent(t, msg); len(got) != 1 || got[0] != "call" {
			t.Fatalf("message text = %v, want call", msg["content"])
		}
	})
	t.Run("text then reasoning", func(t *testing.T) {
		events := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp_1"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "r1"},
			{Kind: ir.EventTextDelta, Text: "mid"},
			{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 1, SummaryIndex: 0, Text: "r2"},
			{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
		}
		body := runQueueEvents(t, events, true)
		if got := queuedSummaryDeltas(t, body); len(got) != 2 || got[0] != "r1" || got[1] != "r2" {
			t.Fatalf("reasoning deltas = %q, want r1 then r2", got)
		}
		final := finalResponseObject(t, body)
		output, _ := final["output"].([]any)
		if len(output) != 3 {
			t.Fatalf("final output = %+v", output)
		}
		types := []string{}
		for _, o := range output {
			m, _ := o.(map[string]any)
			tt, _ := m["type"].(string)
			types = append(types, tt)
		}
		if types[0] != "reasoning" || types[1] != "message" || types[2] != "reasoning" {
			t.Fatalf("final output order = %v", types)
		}
		msg, _ := output[1].(map[string]any)
		if got := queuedSummaryTextOfContent(t, msg); len(got) != 1 || got[0] != "mid" {
			t.Fatalf("message text = %v, want mid", msg["content"])
		}
	})
}

func queuedTextDeltas(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, f := range parseResponsesStream(t, body) {
		if f.Name != "response.output_text.delta" {
			continue
		}
		s, _ := f.Data["delta"].(string)
		out = append(out, s)
	}
	return out
}

func queuedSummaryTextOfContent(t *testing.T, item map[string]any) []string {
	t.Helper()
	content, _ := item["content"].([]any)
	out := make([]string, 0, len(content))
	for _, c := range content {
		m, _ := c.(map[string]any)
		s, _ := m["text"].(string)
		out = append(out, s)
	}
	return out
}
