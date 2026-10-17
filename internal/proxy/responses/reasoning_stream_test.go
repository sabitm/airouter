package responses

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

func reasoningAddedEvent(index int, id, status string) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":{"id":%q,"type":"reasoning","status":%q,"summary":[]}}`, index, id, status)
}

func reasoningDeltaEvent(index int, itemID string, summaryIndex int, delta string) string {
	return fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","item_id":%q,"output_index":%d,"summary_index":%d,"delta":%q}`, itemID, index, summaryIndex, delta)
}

func reasoningTextDoneEvent(index int, itemID string, summaryIndex int, text string) string {
	return fmt.Sprintf(`{"type":"response.reasoning_summary_text.done","item_id":%q,"output_index":%d,"summary_index":%d,"text":%q}`, itemID, index, summaryIndex, text)
}

func reasoningPartDoneEvent(index int, itemID string, summaryIndex int, typ, text string) string {
	return fmt.Sprintf(`{"type":"response.reasoning_summary_part.done","item_id":%q,"output_index":%d,"summary_index":%d,"part":{"type":%q,"text":%q}}`, itemID, index, summaryIndex, typ, text)
}

func reasoningItemDoneEvent(index int, id, status string, encrypted *string, summary string) string {
	item := fmt.Sprintf(`{"id":%q,"type":"reasoning","status":%q,"summary":%s`, id, status, summary)
	if encrypted != nil {
		item += fmt.Sprintf(`,"encrypted_content":%q`, *encrypted)
	}
	item += "}"
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item)
}

func reasoningFinalItem(id, status string, encrypted *string, summary string) string {
	item := fmt.Sprintf(`{"id":%q,"type":"reasoning","status":%q,"summary":%s`, id, status, summary)
	if encrypted != nil {
		item += fmt.Sprintf(`,"encrypted_content":%q`, *encrypted)
	}
	item += "}"
	return item
}

func TestDecodeStreamReasoningStructuredLifecycle(t *testing.T) {
	enc := "cipher"
	body := sseData(
		`{"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		reasoningTextDoneEvent(0, "rs_1", 0, "chain"),
		reasoningPartDoneEvent(0, "rs_1", 0, "summary_text", "chain"),
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":""}}`,
		reasoningPartDoneEvent(0, "rs_1", 1, "summary_text", ""),
		reasoningItemDoneEvent(0, "rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"},{"type":"summary_text","text":""}]`),
		completedEvent(`[`+reasoningFinalItem("rs_1", "completed", &enc, `[{"type":"summary_text","text":"chain"},{"type":"summary_text","text":""}]`)+`]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Kind != ir.EventMessageStart || evs[0].ID != "resp_1" {
		t.Fatalf("start = %+v", evs[0])
	}
	delta := evs[1]
	if delta.Kind != ir.EventReasoningDelta || delta.Text != "chain" || delta.ReasoningID != "rs_1" || !delta.ReasoningIndexSet || delta.ReasoningIndex != 0 || delta.SummaryIndex != 0 {
		t.Fatalf("delta = %+v", delta)
	}
	done := evs[2]
	if done.Kind != ir.EventReasoningItemDone || done.ReasoningID != "rs_1" {
		t.Fatalf("done = %+v", done)
	}
	rr := done.Reasoning
	if rr == nil || rr.ID != "rs_1" || rr.Status != "completed" {
		t.Fatalf("snapshot = %+v", rr)
	}
	if rr.EncryptedContent == nil || *rr.EncryptedContent != "cipher" {
		t.Fatalf("encrypted = %+v", rr.EncryptedContent)
	}
	if len(rr.Summary) != 2 || rr.Summary[0].Text != "chain" || rr.Summary[1].Text != "" {
		t.Fatalf("summary = %+v", rr.Summary)
	}
	if done.Text != "" {
		t.Fatalf("opaque bytes leaked into stream text: %+v", done)
	}
	if evs[3].Kind != ir.EventFinish {
		t.Fatalf("finish = %+v", evs[3])
	}
}

func TestDecodeStreamReasoningFinalOnlyAndLateMetadata(t *testing.T) {
	body := sseData(
		completedEvent(finalOutput(reasoningFinalItem("rs_f", "completed", nil, `[{"type":"summary_text","text":"late"}]`)), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[1].Kind != ir.EventReasoningDelta || evs[1].Text != "late" || evs[1].ReasoningID != "rs_f" {
		t.Fatalf("delta = %+v", evs[1])
	}
	if evs[2].Kind != ir.EventReasoningItemDone || evs[2].Reasoning == nil || evs[2].Reasoning.EncryptedContent != nil {
		t.Fatalf("done = %+v", evs[2])
	}

	enc := "late-cipher"
	body = sseData(
		completedEvent(finalOutput(reasoningFinalItem("rs_l", "completed", nil, `[{"type":"summary_text","text":"x"}]`)), ""),
		completedEvent(finalOutput(reasoningFinalItem("rs_l", "completed", &enc, `[{"type":"summary_text","text":"x"},{"type":"summary_text","text":""}]`)), ""),
	)
	evs, err = collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var dones []ir.StreamEvent
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			dones = append(dones, ev)
		}
	}
	if len(dones) != 2 {
		t.Fatalf("done events = %+v", dones)
	}
	if dones[0].Reasoning.EncryptedContent != nil {
		t.Fatalf("first done carried encrypted: %+v", dones[0].Reasoning)
	}
	if dones[1].Reasoning.EncryptedContent == nil || *dones[1].Reasoning.EncryptedContent != "late-cipher" {
		t.Fatalf("late metadata lost: %+v", dones[1].Reasoning)
	}
	if len(dones[1].Reasoning.Summary) != 2 || dones[1].Reasoning.Summary[1].Text != "" {
		t.Fatalf("late empty summary part lost: %+v", dones[1].Reasoning.Summary)
	}
}

func TestDecodeStreamReasoningRepeatedSnapshotsDedup(t *testing.T) {
	enc := "cipher"
	summary := `[{"type":"summary_text","text":"chain"}]`
	body := sseData(
		reasoningAddedEvent(0, "rs_1", "in_progress"),
		reasoningDeltaEvent(0, "rs_1", 0, "chain"),
		reasoningItemDoneEvent(0, "rs_1", "completed", &enc, summary),
		reasoningItemDoneEvent(0, "rs_1", "completed", &enc, summary),
		completedEvent(`[`+reasoningFinalItem("rs_1", "completed", &enc, summary)+`]`, ""),
		completedEvent(`[`+reasoningFinalItem("rs_1", "completed", &enc, summary)+`]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("deltas=%d dones=%d events=%+v", deltas, dones, evs)
	}
}

func TestDecodeStreamReasoningConflicts(t *testing.T) {
	cases := []struct {
		name    string
		events  []string
		message string
	}{
		{
			name: "identity",
			events: []string{
				reasoningAddedEvent(0, "rs_1", "in_progress"),
				reasoningDeltaEvent(0, "rs_2", 0, "x"),
			},
			message: "conflicting reasoning item identity",
		},
		{
			name: "summary",
			events: []string{
				reasoningAddedEvent(0, "rs_1", "in_progress"),
				reasoningDeltaEvent(0, "rs_1", 0, "abc"),
				reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"abd"}]`),
			},
			message: "conflicting reasoning summary",
		},
		{
			name: "summary shorter snapshot",
			events: []string{
				reasoningAddedEvent(0, "rs_1", "in_progress"),
				reasoningDeltaEvent(0, "rs_1", 0, "abcdef"),
				reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"abc"}]`),
			},
			message: "conflicting reasoning summary",
		},
		{
			name: "encrypted",
			events: []string{
				reasoningAddedEvent(0, "rs_1", "in_progress"),
				reasoningItemDoneEvent(0, "rs_1", "completed", strPtr("a"), `[]`),
				completedEvent(finalOutput(reasoningFinalItem("rs_1", "completed", strPtr("b"), `[]`)), ""),
			},
			message: "conflicting reasoning encrypted content",
		},
		{
			name: "status",
			events: []string{
				reasoningAddedEvent(0, "rs_1", "in_progress"),
				reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[]`),
				completedEvent(finalOutput(reasoningFinalItem("rs_1", "incomplete", nil, `[]`)), ""),
			},
			message: "conflicting reasoning item status",
		},
		{
			name: "summary index out of range",
			events: []string{
				reasoningDeltaEvent(0, "rs_1", 1<<20, "x"),
			},
			message: "reasoning summary index out of range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStream(t, sseData(tc.events...))
			sf, ok := ir.AsStreamFailure(err)
			if !ok || sf.Message != tc.message {
				t.Fatalf("err = %v, want %q", err, tc.message)
			}
			if strings.Contains(err.Error(), "cipher") || strings.Contains(err.Error(), "abcdef") || strings.Contains(err.Error(), "abd") {
				t.Fatalf("error echoed payload: %v", err)
			}
		})
	}
}

func TestDecodeStreamReasoningLegacyDeltaOnly(t *testing.T) {
	body := sseData(
		`{"type":"response.reasoning_summary_text.delta","delta":"chain"}`,
		completedEvent(`[]`, ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []ir.StreamEvent
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningDelta {
			deltas = append(deltas, ev)
		}
	}
	if len(deltas) != 1 || deltas[0].Text != "chain" || deltas[0].ReasoningID != "" || deltas[0].ReasoningIndexSet {
		t.Fatalf("legacy deltas = %+v", deltas)
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			t.Fatalf("legacy stream invented a structured item: %+v", ev)
		}
	}
}

func TestDecodeStreamReasoningFinalOnlyMixedOrder(t *testing.T) {
	args := `{}`
	body := sseData(completedEvent(finalOutput(
		reasoningFinalItem("rs_a", "completed", nil, `[{"type":"summary_text","text":"a"}]`),
		completedItem("fc_a", "call_a", "read", args),
		reasoningFinalItem("rs_b", "completed", nil, `[{"type":"summary_text","text":"b"}]`),
	), ""))
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var reasoningIDs []string
	var sawTool bool
	var sequence []string
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EventReasoningItemDone:
			reasoningIDs = append(reasoningIDs, ev.ReasoningID)
			sequence = append(sequence, "reasoning:"+ev.ReasoningID)
		case ir.EventToolCallStart:
			sawTool = true
			sequence = append(sequence, "tool")
		}
	}
	if len(reasoningIDs) != 2 || reasoningIDs[0] != "rs_a" || reasoningIDs[1] != "rs_b" || !sawTool {
		t.Fatalf("order lost: sequence=%v ids=%v", sequence, reasoningIDs)
	}
	want := []string{"reasoning:rs_a", "tool", "reasoning:rs_b"}
	if strings.Join(sequence, ",") != strings.Join(want, ",") {
		t.Fatalf("sequence = %v want %v", sequence, want)
	}
}

func TestDecodeStreamReasoningLimits(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		err := decodeStreamLimitedAll(strings.NewReader(sseData(
			reasoningDeltaEvent(0, "rs_1", 0, "abcdef"),
		)), func(ir.StreamEvent) error { return nil }, 1024, 10, 5, 4, 4)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "reasoning state too large" {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "abcdef") {
			t.Fatalf("error leaked reasoning text: %v", err)
		}
	})
	t.Run("items", func(t *testing.T) {
		err := decodeStreamLimitedAll(strings.NewReader(sseData(
			reasoningAddedEvent(0, "rs_a", "in_progress"),
			reasoningAddedEvent(1, "rs_b", "in_progress"),
			reasoningAddedEvent(2, "rs_c", "in_progress"),
		)), func(ir.StreamEvent) error { return nil }, 1024, 10, 1<<20, 2, 4)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "too many reasoning items" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("parts", func(t *testing.T) {
		err := decodeStreamLimitedAll(strings.NewReader(sseData(
			reasoningDeltaEvent(0, "rs_a", 0, "a"),
			reasoningDeltaEvent(1, "rs_b", 0, "b"),
		)), func(ir.StreamEvent) error { return nil }, 1024, 10, 1<<20, 4, 1)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "too many reasoning summary parts" {
			t.Fatalf("err = %v", err)
		}
	})
}

func encodeStreamEventsErr(t *testing.T, events []ir.StreamEvent) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	writer, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("recorder does not support flushing")
	}
	enc := NewStreamEncoder("m")
	for _, ev := range events {
		if err := enc.Encode(ev, writer); err != nil {
			return rec.Body.String(), err
		}
	}
	if err := enc.Close(writer); err != nil {
		return rec.Body.String(), err
	}
	return rec.Body.String(), nil
}

func TestEncodeStreamReasoningStructuredLifecycle(t *testing.T) {
	encBytes := "cipher"
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "chain"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 0, Reasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Status: "completed", EncryptedContent: &encBytes,
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}, {Type: "summary_text", Text: ""}},
		}},
		{Kind: ir.EventTextDelta, Text: "answer"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"id":"rs_1"`,
		`"summary_index":0`,
		`"delta":"chain"`,
		`"encrypted_content":"cipher"`,
		`"status":"completed"`,
		`"output_text"`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("stream missing %s: %s", want, stream)
		}
	}
	names := sseEventNames(t, stream)
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v", names)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("final output = %+v", final["output"])
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "reasoning" || first["id"] != "rs_1" || first["encrypted_content"] != "cipher" {
		t.Fatalf("final reasoning = %+v", first)
	}
	summary, _ := first["summary"].([]any)
	if len(summary) != 2 {
		t.Fatalf("final summary = %+v", summary)
	}
	second, _ := output[1].(map[string]any)
	if second["type"] != "message" {
		t.Fatalf("final message = %+v", second)
	}
}

func TestEncodeStreamReasoningDoneDedupAndLateMetadata(t *testing.T) {
	encBytes := "cipher"
	base := &ir.ResponsesReasoning{ID: "rs_1", Status: "completed", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}}}
	withEnc := base.Clone()
	withEnc.EncryptedContent = &encBytes
	withPart := withEnc.Clone()
	withPart.Summary = append(withPart.Summary, ir.ReasoningSummaryPart{Type: "summary_text", Text: ""})
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", SummaryIndex: 0, Text: "chain"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: base},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: base},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: withEnc},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: withPart},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stream, `"delta":"chain"`) != 1 || strings.Count(stream, `"type":"response.output_item.done"`) != 1 {
		t.Fatalf("duplicate emission: %s", stream)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	first, _ := output[0].(map[string]any)
	if first["encrypted_content"] != "cipher" {
		t.Fatalf("late metadata missing: %+v", first)
	}
	summary, _ := first["summary"].([]any)
	if len(summary) != 2 {
		t.Fatalf("late empty summary part missing: %+v", first)
	}
}

func TestEncodeStreamReasoningIndexIdentityUpgrade(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 3, SummaryIndex: 0, Text: "chain"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_late", ReasoningIndexSet: true, ReasoningIndex: 3, Reasoning: &ir.ResponsesReasoning{
			ID: "rs_late", Status: "completed", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
		}},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	if ids := reasoningWireIDs(t, stream); len(ids) != 1 || ids[0] != "rs_late" {
		t.Fatalf("wire ids = %v, want only the source id", ids)
	}
	if strings.Count(stream, `"delta":"chain"`) != 1 {
		t.Fatalf("deferred summary duplicated: %s", stream)
	}
	for _, frame := range []string{"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.output_item.done"} {
		if !strings.Contains(stream, `"type":"`+frame+`"`) {
			t.Fatalf("missing frame %s: %s", frame, stream)
		}
	}
	if !strings.Contains(stream, `"item_id":"rs_late"`) || !strings.Contains(stream, `"summary_index":0`) || !strings.Contains(stream, `"output_index":0`) {
		t.Fatalf("reasoning frame identity/index mismatch: %s", stream)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %+v", output)
	}
	first, _ := output[0].(map[string]any)
	if first["id"] != "rs_late" {
		t.Fatalf("source id lost: %+v", first)
	}
}

func TestEncodeStreamReasoningConflicts(t *testing.T) {
	encBytes := "a"
	other := "b"
	cases := []struct {
		name    string
		events  []ir.StreamEvent
		message string
	}{
		{
			name: "summary",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", Text: "abc"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
					ID: "rs_1", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "abd"}},
				}},
			},
			message: "conflicting reasoning summary",
		},
		{
			name: "encrypted",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", EncryptedContent: &encBytes}},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1", EncryptedContent: &other}},
			},
			message: "conflicting reasoning encrypted content",
		},
		{
			name: "delta after done",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{ID: "rs_1"}},
				{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", Text: "late"},
			},
			message: "reasoning summary after item done",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := encodeStreamEventsErr(t, tc.events)
			sf, ok := ir.AsStreamFailure(err)
			if !ok || sf.Message != tc.message {
				t.Fatalf("err = %v want %q", err, tc.message)
			}
			if strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "late") {
				t.Fatalf("error echoed payload: %v", err)
			}
		})
	}
}

func TestEncodeStreamReasoningItemLimit(t *testing.T) {
	events := []ir.StreamEvent{{Kind: ir.EventMessageStart, ID: "resp_1"}}
	for i := 0; i <= maxEncoderReasoningItems; i++ {
		events = append(events, ir.StreamEvent{
			Kind: ir.EventReasoningItemDone, ReasoningID: fmt.Sprintf("rs_%d", i),
			Reasoning: &ir.ResponsesReasoning{ID: fmt.Sprintf("rs_%d", i)},
		})
	}
	_, err := encodeStreamEventsErr(t, events)
	sf, ok := ir.AsStreamFailure(err)
	if !ok || sf.Message != "too many reasoning items" {
		t.Fatalf("err = %v", err)
	}
}

func sseEventNames(t *testing.T, body string) []string {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	var names []string
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, ev.Name)
	}
	return names
}

func finalResponseObject(t *testing.T, body string) map[string]any {
	t.Helper()
	reader := sse.NewReader(strings.NewReader(body))
	for {
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Name != "response.completed" {
			continue
		}
		var raw struct {
			Response map[string]any `json:"response"`
		}
		if err := json.Unmarshal(ev.Data, &raw); err != nil {
			t.Fatal(err)
		}
		return raw.Response
	}
	t.Fatal("no response.completed event")
	return nil
}

func TestDecodeStreamReasoningMalformedSupportedFields(t *testing.T) {
	cases := []struct {
		name   string
		events []string
	}{
		{"done item encrypted number", []string{`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":123,"summary":[]}}`}},
		{"done item encrypted object", []string{`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":{"c":1},"summary":[]}}`}},
		{"done item null summary entry", []string{`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[null]}}`}},
		{"added item id number", []string{`{"type":"response.output_item.added","output_index":0,"item":{"id":7,"type":"reasoning","summary":[]}}`}},
		{"delta summary index string", []string{`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":"zero","delta":"x"}`}},
		{"delta value number", []string{`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":0,"delta":5}`}},
		{"part added part number", []string{`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":0,"part":5}`}},
		{"part done part text number", []string{`{"type":"response.reasoning_summary_part.done","item_id":"rs_1","summary_index":0,"part":{"type":"summary_text","text":9}}`}},
		{"completed malformed encrypted", []string{completedEvent(finalOutput(`{"id":"rs_1","type":"reasoning","status":"completed","encrypted_content":123,"summary":[]}`), "")}},
		{"completed null summary entry", []string{completedEvent(finalOutput(`{"id":"rs_1","type":"reasoning","status":"completed","summary":[null]}`), "")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStream(t, sseData(tc.events...))
			sf, ok := ir.AsStreamFailure(err)
			if !ok || !strings.Contains(sf.Message, "malformed reasoning") {
				t.Fatalf("err = %v, want payload-free reasoning protocol error", err)
			}
			if strings.Contains(err.Error(), "123") || strings.Contains(err.Error(), "zero") || strings.Contains(err.Error(), "{") {
				t.Fatalf("error echoed a field value: %v", err)
			}
		})
	}
	// An unknown or unrelated malformed event keeps the tolerated skip.
	if _, err := collectStream(t, sseData(`{"type":"response.unknown.event","output_index":"x"}`)); err != nil {
		t.Fatalf("unknown malformed event must stay tolerated: %v", err)
	}
}

func TestDecodeStreamReasoningFinalOnlyMixedOrderWithText(t *testing.T) {
	message := func(id, text string) string {
		return fmt.Sprintf(`{"id":%q,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q}]}`, id, text)
	}
	body := sseData(completedEvent(finalOutput(
		reasoningFinalItem("rs_a", "completed", nil, `[{"type":"summary_text","text":"a"}]`),
		message("msg_a", "answer-a"),
		completedItem("fc_a", "call_a", "read", `{}`),
		reasoningFinalItem("rs_b", "completed", nil, `[{"type":"summary_text","text":"b"}]`),
		message("msg_b", "answer-b"),
	), ""))
	evs, err := collectStream(t, body)
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
		case ir.EventTextDelta:
			seq = append(seq, "text:"+ev.Text)
		case ir.EventToolCallStart:
			seq = append(seq, "tool")
		}
	}
	want := []string{"delta:rs_a:a", "done:rs_a", "text:answer-a", "tool", "delta:rs_b:b", "done:rs_b", "text:answer-b"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("sequence = %v want %v", seq, want)
	}
}

func TestDecodeStreamReasoningStreamedTextNotDuplicatedAtFinal(t *testing.T) {
	message := `{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	body := sseData(
		`{"type":"response.output_text.delta","delta":"hello"}`,
		completedEvent(finalOutput(message), ""),
		completedEvent(finalOutput(message), ""),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, ev := range evs {
		if ev.Kind == ir.EventTextDelta {
			text += ev.Text
		}
	}
	if text != "hello" {
		t.Fatalf("text = %q, want one copy", text)
	}
}

func TestDecodeStreamReasoningSmallLimits(t *testing.T) {
	run := func(maxBytes int, events ...string) error {
		return decodeStreamLimitedAll(strings.NewReader(sseData(events...)), func(ir.StreamEvent) error { return nil }, 1024, 10, maxBytes, 8, 8)
	}
	enc := "cipher"
	t.Run("encrypted bytes", func(t *testing.T) {
		item := reasoningItemDoneEvent(0, "rs_1", "completed", &enc, `[]`)
		want := &ir.ResponsesReasoning{ID: "rs_1", Status: "completed", EncryptedContent: &enc}
		allowed := reasoningItemOverhead + len("rs_1") + reasoningKeyOverhead + reasoningKeyOverhead +
			len(enc) + len("completed") + retainedSnapshotBytes(want)
		if err := run(allowed, item); err != nil {
			t.Fatalf("exact encrypted limit failed: %v", err)
		}
		err := run(allowed-1, item)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "reasoning state too large" {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "cipher") {
			t.Fatalf("error leaked encrypted bytes: %v", err)
		}
	})
	t.Run("id bytes", func(t *testing.T) {
		item := reasoningItemDoneEvent(0, "rs_1", "", nil, `[]`)
		want := &ir.ResponsesReasoning{ID: "rs_1"}
		allowed := reasoningItemOverhead + reasoningKeyOverhead + len("rs_1") + reasoningKeyOverhead + retainedSnapshotBytes(want)
		if err := run(allowed, item); err != nil {
			t.Fatalf("exact id limit failed: %v", err)
		}
		if err := run(allowed-1, item); err == nil {
			t.Fatal("id bytes were not charged")
		}
	})
	t.Run("status bytes", func(t *testing.T) {
		item := reasoningItemDoneEvent(0, "", "completed", nil, `[]`)
		want := &ir.ResponsesReasoning{Status: "completed"}
		allowed := reasoningItemOverhead + reasoningKeyOverhead + len("completed") + retainedSnapshotBytes(want)
		if err := run(allowed, item); err != nil {
			t.Fatalf("exact status limit failed: %v", err)
		}
		if err := run(allowed-1, item); err == nil {
			t.Fatal("status bytes were not charged")
		}
	})
	t.Run("summary type bytes", func(t *testing.T) {
		item := reasoningItemDoneEvent(0, "", "", nil, `[{"type":"custom_type","text":""}]`)
		want := &ir.ResponsesReasoning{Summary: []ir.ReasoningSummaryPart{{Type: "custom_type"}}}
		allowed := reasoningItemOverhead + reasoningKeyOverhead + reasoningPartOverhead + len("custom_type") + retainedSnapshotBytes(want)
		if err := run(allowed, item); err != nil {
			t.Fatalf("exact type limit failed: %v", err)
		}
		if err := run(allowed-1, item); err == nil {
			t.Fatal("summary type bytes were not charged")
		}
	})
	t.Run("many empty parts", func(t *testing.T) {
		parts := make([]string, 9)
		for i := range parts {
			parts[i] = `{"text":""}`
		}
		err := run(1<<20, reasoningItemDoneEvent(0, "rs_1", "", nil, "["+strings.Join(parts, ",")+"]"))
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "reasoning summary index out of range" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("repeated identical snapshots", func(t *testing.T) {
		item := reasoningItemDoneEvent(0, "rs_1", "completed", nil, `[{"type":"summary_text","text":"chain"}]`)
		want := &ir.ResponsesReasoning{ID: "rs_1", Status: "completed", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}}}
		allowed := reasoningItemOverhead + len("rs_1") + reasoningKeyOverhead + reasoningKeyOverhead +
			len("completed") + reasoningPartOverhead + len("summary_text") + len("chain") + retainedSnapshotBytes(want)
		events := []string{item, item, item, item}
		if err := run(allowed, events...); err != nil {
			t.Fatalf("repeated snapshots double-charged: %v", err)
		}
	})
}

func TestEncodeStreamReasoningMultiNonemptyParts(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 0, Text: "a"},
		{Kind: ir.EventReasoningDelta, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 0, SummaryIndex: 1, Text: "b"},
		{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", ReasoningIndexSet: true, ReasoningIndex: 0, Reasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Status: "completed",
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "a"}, {Type: "summary_text", Text: "b"}, {Type: "summary_text", Text: ""}},
		}},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	names := sseEventNames(t, stream)
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v", names)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %+v", output)
	}
	first, _ := output[0].(map[string]any)
	if first["id"] != "rs_1" {
		t.Fatalf("reasoning id = %+v", first)
	}
	summary, _ := first["summary"].([]any)
	if len(summary) != 3 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary[0].(map[string]any)["text"] != "a" || summary[1].(map[string]any)["text"] != "b" || summary[2].(map[string]any)["text"] != "" {
		t.Fatalf("summary texts = %+v", summary)
	}
}

func TestEncodeStreamReasoningLegacySegments(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventReasoningDelta, Text: "one"},
		{Kind: ir.EventTextDelta, Text: "answer"},
		{Kind: ir.EventReasoningDelta, Text: "two"},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output = %+v", output)
	}
	first, _ := output[0].(map[string]any)
	third, _ := output[2].(map[string]any)
	if first["type"] != "reasoning" || third["type"] != "reasoning" {
		t.Fatalf("types = %v/%v", first["type"], third["type"])
	}
	idA, _ := first["id"].(string)
	idB, _ := third["id"].(string)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("legacy segments collapsed: %q %q", idA, idB)
	}
}

func TestEncodeStreamReasoningUnidentifiedItemsDistinct(t *testing.T) {
	done := func(text string) ir.StreamEvent {
		return ir.StreamEvent{Kind: ir.EventReasoningItemDone, Reasoning: &ir.ResponsesReasoning{
			Status:  "completed",
			Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: text}},
		}}
	}
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		done("first"),
		done("second"),
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unidentified items collapsed: %+v", output)
	}
	for i, raw := range output {
		item := raw.(map[string]any)
		if item["type"] != "reasoning" || item["id"] == "" {
			t.Fatalf("item %d = %+v", i, item)
		}
	}
}

func TestEncodeStreamReasoningLateIndexIDLifecycle(t *testing.T) {
	enc := "cipher"
	cases := []struct {
		name       string
		events     []ir.StreamEvent
		wantSource bool
	}{
		{
			name: "id arrives before finalization",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 3, SummaryIndex: 0, Text: "chain"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_late", ReasoningIndexSet: true, ReasoningIndex: 3, Reasoning: &ir.ResponsesReasoning{
					ID: "rs_late", Status: "completed", EncryptedContent: &enc,
					Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
				}},
			},
			wantSource: true,
		},
		{
			name: "text queues until late id arrives",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 3, SummaryIndex: 0, Text: "chain"},
				{Kind: ir.EventTextDelta, Text: "answer"},
				{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_late", ReasoningIndexSet: true, ReasoningIndex: 3, Reasoning: &ir.ResponsesReasoning{
					ID: "rs_late", Status: "completed", EncryptedContent: &enc,
					Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "chain"}},
				}},
			},
			wantSource: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]ir.StreamEvent{{Kind: ir.EventMessageStart, ID: "resp_1"}}, tc.events...)
			events = append(events, ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn})
			stream, err := encodeStreamEventsErr(t, events)
			if err != nil {
				t.Fatal(err)
			}
			ids := reasoningWireIDs(t, stream)
			if tc.wantSource {
				if len(ids) != 1 || ids[0] != "rs_late" {
					t.Fatalf("wire ids = %v, want only rs_late", ids)
				}
				// One stable source id on added, deltas, done, and the final item.
				if got := strings.Count(stream, `"rs_late"`); got < 4 {
					t.Fatalf("source id on %d frames: %s", got, stream)
				}
				if strings.Count(stream, `"delta":"chain"`) != 1 {
					t.Fatalf("deferred summary duplicated: %s", stream)
				}
				// Queued text renders only after the reasoning item closed, so no
				// generated id was ever emitted while the source id was pending.
				// The source id arrives with the done snapshot, which unblocks
				// the queue before finish closes the response.
				if tc.name == "text queues until late id arrives" && !strings.Contains(stream, `"delta":"answer"`) {
					t.Fatalf("queued text lost: %s", stream)
				}
			} else {
				if len(ids) != 1 || ids[0] == "rs_late" {
					t.Fatalf("wire ids = %v, want one stable generated id", ids)
				}
			}
			if strings.Contains(stream, `"type":"response.reasoning_summary_part.added"`) {
				return
			}
			t.Fatalf("reasoning never opened: %s", stream)
		})
	}
}

// reasoningWireIDs returns every distinct reasoning item id used by reasoning
// frames. A valid lifecycle uses one id for added, deltas, and done.
func reasoningWireIDs(t *testing.T, body string) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	reader := sse.NewReader(strings.NewReader(body))
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
		ids := []string{}
		if strings.HasPrefix(ev.Name, "response.reasoning") && raw.ItemID != "" {
			ids = append(ids, raw.ItemID)
		}
		if (ev.Name == "response.output_item.added" || ev.Name == "response.output_item.done") &&
			raw.Item != nil && raw.Item.Type == "reasoning" && raw.Item.ID != "" {
			ids = append(ids, raw.Item.ID)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func TestEncodeStreamReasoningSmallLimits(t *testing.T) {
	encBytes := "cipher"
	done := func(status string, enc *string, summary ...ir.ReasoningSummaryPart) ir.StreamEvent {
		return ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_1", Reasoning: &ir.ResponsesReasoning{
			ID: "rs_1", Status: status, EncryptedContent: enc, Summary: summary,
		}}
	}
	run := func(enc *StreamEncoder, events ...ir.StreamEvent) error {
		rec := httptest.NewRecorder()
		writer, ok := sse.NewWriter(rec)
		if !ok {
			t.Fatal("recorder does not support flushing")
		}
		for _, ev := range events {
			if err := enc.Encode(ev, writer); err != nil {
				return err
			}
		}
		return enc.Close(writer)
	}
	t.Run("id bytes", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		enc.maxReasoningBytes = encoderReasoningItemOverhead + len("id:rs_1") + encoderReasoningKeyOverhead
		if err := run(enc, done("", nil)); err == nil {
			t.Fatal("id bytes were not charged")
		}
	})
	t.Run("status bytes", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		enc.maxReasoningBytes = encoderReasoningItemOverhead + len("id:rs_1") + encoderReasoningKeyOverhead +
			len("rs_1") + len("completed") - 1
		if err := run(enc, done("completed", nil)); err == nil {
			t.Fatal("status bytes were not charged")
		}
	})
	t.Run("encrypted bytes", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		enc.maxReasoningBytes = encoderReasoningItemOverhead + len("id:rs_1") + encoderReasoningKeyOverhead +
			len("rs_1") + len(encBytes) - 1
		if err := run(enc, done("", &encBytes)); err == nil {
			t.Fatal("encrypted bytes were not charged")
		}
	})
	t.Run("summary type bytes", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		enc.maxReasoningBytes = encoderReasoningItemOverhead + len("id:rs_1") + encoderReasoningKeyOverhead +
			len("rs_1") + encoderReasoningPartOverhead + len("custom_type") - 1
		if err := run(enc, done("", nil, ir.ReasoningSummaryPart{Type: "custom_type"})); err == nil {
			t.Fatal("summary type bytes were not charged")
		}
	})
	t.Run("final snapshot copy", func(t *testing.T) {
		// Everything fits until the terminal map copy is charged.
		enc := NewStreamEncoder("m")
		enc.maxReasoningBytes = encoderReasoningItemOverhead + len("id:rs_1") + encoderReasoningKeyOverhead +
			len("rs_1") + encoderReasoningPartOverhead + len("summary_text") + len("chain")
		if err := run(enc, done("", nil, ir.ReasoningSummaryPart{Type: "summary_text", Text: "chain"})); err == nil {
			t.Fatal("final snapshot copy was not charged")
		}
	})
	t.Run("repeated identical snapshots", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		// Exact charge for one done snapshot; repeats must not grow it. The id
		// bytes are charged once for the map entry and once for the id itself.
		enc.maxReasoningBytes = encoderReasoningItemOverhead + len("id:rs_1") + encoderReasoningKeyOverhead +
			len("rs_1") + len("id:rs_1") + encoderReasoningKeyOverhead +
			encoderReasoningPartOverhead + len("summary_text") + len("chain") +
			encoderReasoningItemOverhead + len("rs_1") + encoderReasoningPartOverhead + len("summary_text") + len("chain")
		repeat := done("", nil, ir.ReasoningSummaryPart{Type: "summary_text", Text: "chain"})
		if err := run(enc, repeat, repeat, repeat); err != nil {
			t.Fatalf("repeated snapshots double-charged: %v", err)
		}
	})
	t.Run("item cap", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		enc.maxReasoningItems = 1
		err := run(enc,
			ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_a", Reasoning: &ir.ResponsesReasoning{ID: "rs_a"}},
			ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_b", Reasoning: &ir.ResponsesReasoning{ID: "rs_b"}},
		)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "too many reasoning items" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("part cap", func(t *testing.T) {
		enc := NewStreamEncoder("m")
		enc.maxReasoningParts = 2
		err := run(enc,
			done("", nil, ir.ReasoningSummaryPart{Type: "summary_text", Text: "a"}, ir.ReasoningSummaryPart{Type: "summary_text", Text: "b"}),
			ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_2", Reasoning: &ir.ResponsesReasoning{
				ID: "rs_2", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "c"}},
			}},
		)
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Message != "too many reasoning summary parts" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestEncodeStreamReasoningAnthropicSegments(t *testing.T) {
	events := []ir.StreamEvent{
		{Kind: ir.EventMessageStart, ID: "resp_1"},
		{Kind: ir.EventReasoningStart, Index: 0},
		{Kind: ir.EventReasoningDelta, Index: 0, Indexed: true, Text: "one"},
		{Kind: ir.EventReasoningEnd, Index: 0},
		{Kind: ir.EventReasoningStart, Index: 1},
		{Kind: ir.EventReasoningDelta, Index: 1, Indexed: true, Text: "two"},
		{Kind: ir.EventReasoningEnd, Index: 1},
		{Kind: ir.EventFinish, StopReason: ir.StopEndTurn},
	}
	stream, err := encodeStreamEventsErr(t, events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stream, `"delta":"one"`) != 1 || strings.Count(stream, `"delta":"two"`) != 1 {
		t.Fatalf("segments lost: %s", stream)
	}
	ids := reasoningWireIDs(t, stream)
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("reasoning segment ids = %v", ids)
	}
	final := finalResponseObject(t, stream)
	output, _ := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %+v", output)
	}
	for i, want := range []string{"one", "two"} {
		item := output[i].(map[string]any)
		summary, _ := item["summary"].([]any)
		if len(summary) != 1 || summary[0].(map[string]any)["text"] != want {
			t.Fatalf("item %d = %+v", i, item)
		}
	}
}

func TestEncodeStreamReasoningDeferredOrder(t *testing.T) {
	done := func(id string, index int, text string) ir.StreamEvent {
		return ir.StreamEvent{Kind: ir.EventReasoningItemDone, ReasoningID: id, ReasoningIndexSet: true, ReasoningIndex: index, Reasoning: &ir.ResponsesReasoning{
			ID: id, Status: "completed", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: text}},
		}}
	}
	cases := []struct {
		name   string
		events []ir.StreamEvent
	}{
		{
			name: "finish closes both",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 1, SummaryIndex: 0, Text: "a"},
				{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "b"},
			},
		},
		{
			name: "late id closes first then second",
			events: []ir.StreamEvent{
				{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 1, SummaryIndex: 0, Text: "a"},
				{Kind: ir.EventReasoningDelta, ReasoningIndexSet: true, ReasoningIndex: 2, SummaryIndex: 0, Text: "b"},
				done("rs_b", 2, "b"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]ir.StreamEvent{{Kind: ir.EventMessageStart, ID: "resp_1"}}, tc.events...)
			events = append(events, ir.StreamEvent{Kind: ir.EventFinish, StopReason: ir.StopEndTurn})
			stream, err := encodeStreamEventsErr(t, events)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(stream, `"delta":"a"`) != 1 || strings.Count(stream, `"delta":"b"`) != 1 {
				t.Fatalf("deferred deltas emitted wrong number of times: %s", stream)
			}
			if strings.Count(stream, `"type":"response.output_item.added"`) != 2 ||
				strings.Count(stream, `"type":"response.output_item.done"`) != 2 {
				t.Fatalf("deferred lifecycle frames = %s", stream)
			}
			final := finalResponseObject(t, stream)
			output, _ := final["output"].([]any)
			if len(output) != 2 {
				t.Fatalf("output = %+v", output)
			}
			for i, want := range []string{"a", "b"} {
				item := output[i].(map[string]any)
				summary, _ := item["summary"].([]any)
				if len(summary) != 1 || summary[0].(map[string]any)["text"] != want {
					t.Fatalf("item %d = %+v", i, item)
				}
			}
		})
	}
}
