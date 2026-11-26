package responses

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

func encodeGrokReconcile(t *testing.T, body string) ([]ir.StreamEvent, string, error) {
	t.Helper()
	var evs []ir.StreamEvent
	err := DecodeStreamAwaitingItemIDReconciled(strings.NewReader(body), func(ev ir.StreamEvent) error {
		evs = append(evs, ev)
		return nil
	})
	if err != nil {
		return evs, "", err
	}
	out, err := encodeWithEncoder(t, NewReconciledStreamEncoder("m"), evs)
	return evs, out, err
}

func allDoneItems(t *testing.T, body string) []map[string]any {
	t.Helper()
	var items []map[string]any
	for _, f := range parseResponsesStream(t, body) {
		if f.Name != "response.output_item.done" {
			continue
		}
		item, _ := f.Data["item"].(map[string]any)
		items = append(items, item)
	}
	return items
}

func doneItemField(item map[string]any, field string) string {
	text, _ := item[field].(string)
	return text
}

// TestGrokReconcileLateReasoningAndToolID covers a native function item whose
// id arrives only at its done frame and a reasoning item whose authoritative
// ciphertext arrives only at the terminal snapshot.
func TestGrokReconcileLateReasoningAndToolID(t *testing.T) {
	const cipher = "cipher-final"
	const summary = `[{"type":"summary_text","text":"reason"}]`
	tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
	cases := []struct {
		name        string
		doneCipher  *string
		terminal    *string
		provisional *string
		wantCipher  *string
	}{
		{name: "completion carries bytes", doneCipher: strPtr(cipher), terminal: strPtr(cipher), wantCipher: strPtr(cipher)},
		{name: "terminal only", terminal: strPtr(cipher), wantCipher: strPtr(cipher)},
		{name: "never supplied", wantCipher: nil},
		{name: "provisional only is not promoted", provisional: strPtr("provisional"), wantCipher: nil},
		{name: "provisional then final", provisional: strPtr("provisional"), doneCipher: strPtr(cipher), terminal: strPtr(cipher), wantCipher: strPtr(cipher)},
		{name: "explicit empty", doneCipher: strPtr(""), terminal: strPtr(""), wantCipher: strPtr("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			added := reasoningAddedEvent(0, "rs_a", "in_progress")
			if tc.provisional != nil {
				added = `{"type":"response.output_item.added","output_index":0,"item":` +
					reasoningItemJSON("rs_a", "in_progress", "[]", "", tc.provisional, "") + `}`
			}
			body := sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				added,
				reasoningDeltaEvent(0, "rs_a", 0, "reason"),
				addedNoItem(1, "call_a", "f"),
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"x\":"}`,
				reasoningItemDoneEvent(0, "rs_a", "completed", tc.doneCipher, summary),
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"1}"}`,
				itemDoneEvent(1, "fc_a", "call_a", "f", `{"x":1}`),
				completedEvent(finalOutput(
					reasoningItemJSON("rs_a", "completed", summary, "", tc.terminal, "completed"),
					tool,
				), `{"input_tokens":2,"output_tokens":3}`),
			)
			_, out, err := encodeGrokReconcile(t, body)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			items, order := reasoningDoneItems(t, out)
			if len(items) != 1 {
				t.Fatalf("reasoning done frames = %d, want 1: %s", len(items), out)
			}
			if strings.Join(order, ",") != "reasoning,function_call" {
				t.Fatalf("done order = %v, want reasoning then function_call: %s", order, out)
			}
			got, present := encryptedOf(items[0])
			if (tc.wantCipher == nil) != !present {
				t.Fatalf("encrypted presence = %v, want %v: %v", present, tc.wantCipher != nil, items[0])
			}
			if tc.wantCipher != nil && got != *tc.wantCipher {
				t.Fatalf("encrypted = %q, want %q", got, *tc.wantCipher)
			}
			if doneItemField(items[0], "id") != "rs_a" || doneItemField(items[0], "status") != "completed" {
				t.Fatalf("reasoning identity = %v", items[0])
			}
			summaryJSON, _ := json.Marshal(items[0]["summary"])
			var gotSummary, wantSummary any
			if json.Unmarshal(summaryJSON, &gotSummary) != nil || json.Unmarshal([]byte(summary), &wantSummary) != nil ||
				!reflect.DeepEqual(gotSummary, wantSummary) {
				t.Fatalf("reasoning summary = %s, want %s", summaryJSON, summary)
			}
			if strings.Contains(out, "provisional") {
				t.Fatalf("provisional ciphertext leaked: %s", out)
			}
			dones := allDoneItems(t, out)
			if len(dones) != 2 || doneItemField(dones[1], "id") != "fc_a" ||
				doneItemField(dones[1], "call_id") != "call_a" || doneItemField(dones[1], "arguments") != `{"x":1}` {
				t.Fatalf("tool completion = %v", dones)
			}
			assertDoneMatchesTerminal(t, out)
			terminal, _ := finalResponseObject(t, out)["output"].([]any)
			if len(terminal) != 2 {
				t.Fatalf("terminal output = %v", terminal)
			}
			terminalReasoning, _ := terminal[0].(map[string]any)
			terminalCipher, terminalPresent := encryptedOf(terminalReasoning)
			if (tc.wantCipher == nil) != !terminalPresent || (tc.wantCipher != nil && terminalCipher != *tc.wantCipher) {
				t.Fatalf("terminal reasoning = %v, want cipher %v", terminalReasoning, tc.wantCipher)
			}
		})
	}
}

// TestGrokReconcileParallelLateToolIDs keeps two late tool identities in
// registration order even when the later item finishes first.
func TestGrokReconcileParallelLateToolIDs(t *testing.T) {
	const cipher = "cipher-parallel"
	const summary = `[{"type":"summary_text","text":"plan"}]`
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_a", "in_progress"),
		reasoningDeltaEvent(0, "rs_a", 0, "plan"),
		addedNoItem(1, "call_a", "f"),
		addedNoItem(2, "call_b", "f"),
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":\"a\""}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"path\":\"b\""}`,
		reasoningItemDoneEvent(0, "rs_a", "completed", nil, summary),
		itemDoneEvent(2, "fc_b", "call_b", "f", `{"path":"b"}`),
		itemDoneEvent(1, "fc_a", "call_a", "f", `{"path":"a"}`),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_a", "completed", strPtr(cipher), summary),
			completedItem("fc_a", "call_a", "f", `{"path":"a"}`),
			completedItem("fc_b", "call_b", "f", `{"path":"b"}`),
		), `{"input_tokens":4,"output_tokens":5}`),
	)
	_, out, err := encodeGrokReconcile(t, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dones := allDoneItems(t, out)
	if len(dones) != 3 {
		t.Fatalf("done frames = %d, want 3: %s", len(dones), out)
	}
	want := []struct {
		typ, id, callID, args string
	}{
		{"reasoning", "rs_a", "", ""},
		{"function_call", "fc_a", "call_a", `{"path":"a"}`},
		{"function_call", "fc_b", "call_b", `{"path":"b"}`},
	}
	for i, w := range want {
		if doneItemField(dones[i], "type") != w.typ || doneItemField(dones[i], "id") != w.id ||
			doneItemField(dones[i], "call_id") != w.callID || doneItemField(dones[i], "arguments") != w.args {
			t.Fatalf("done[%d] = %v, want %+v: %s", i, dones[i], w, out)
		}
	}
	if got, present := encryptedOf(dones[0]); !present || got != cipher {
		t.Fatalf("reasoning cipher = %q, present %v: %v", got, present, dones[0])
	}
	assertDoneMatchesTerminal(t, out)
}

// TestGrokReconcileSilentReasoningKeepsPosition checks that an added-only
// silent placeholder takes no position until a completion confirms it. A real
// completion may precede an earlier-indexed tool only when it arrives first;
// a later placeholder follows the tool that already reached the wire.
func TestGrokReconcileSilentReasoningKeepsPosition(t *testing.T) {
	const cipher = "cipher-silent"
	tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
	cases := []struct {
		name  string
		body  string
		order string
	}{
		{
			name: "indexed completion before tool",
			body: sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_s","status":"in_progress","summary":[]}}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_s","status":"completed","summary":[]}}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"f","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"x\":1}"}`,
				`{"type":"response.output_item.done","output_index":1,"item":`+tool+`}`,
				completedEvent(finalOutput(
					reasoningFinalItem("rs_s", "completed", strPtr(cipher), `[]`), tool,
				), ""),
			),
			order: "reasoning,function_call",
		},
		{
			name: "indexed placeholder after earlier tool",
			body: sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_s","status":"in_progress","summary":[]}}`,
				addedNoItem(0, "call_a", "f"),
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"x\":"}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}`,
				itemDoneEvent(0, "fc_a", "call_a", "f", `{"x":1}`),
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_s","status":"completed","summary":[]}}`,
				completedEvent(finalOutput(
					tool, reasoningFinalItem("rs_s", "completed", strPtr(cipher), `[]`),
				), ""),
			),
			order: "function_call,reasoning",
		},
		{
			name: "terminal only after tool",
			body: sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				addedNoItem(0, "call_a", "f"),
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"x\":"}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"1}"}`,
				itemDoneEvent(0, "fc_a", "call_a", "f", `{"x":1}`),
				completedEvent(finalOutput(
					tool, reasoningFinalItem("rs_s", "completed", strPtr(cipher), `[]`),
				), ""),
			),
			order: "function_call,reasoning",
		},
		{
			name: "terminal only before pending tool",
			body: sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				addedNoItem(1, "call_a", "f"),
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"x\":1}"}`,
				completedEvent(finalOutput(
					reasoningFinalItem("rs_s", "completed", strPtr(cipher), `[]`), tool,
				), ""),
			),
			order: "reasoning,function_call",
		},
		{
			name: "unindexed before tool",
			body: sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				`{"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_s","status":"in_progress","summary":[]}}`,
				`{"type":"response.output_item.added","output_index":0,"item":`+tool+`}`,
				`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_s","status":"completed","summary":[]}}`,
				completedEvent(finalOutput(
					tool, reasoningFinalItem("rs_s", "completed", strPtr(cipher), `[]`),
				), ""),
			),
			order: "function_call,reasoning",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := encodeGrokReconcile(t, tc.body)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			items, order := reasoningDoneItems(t, out)
			if len(items) != 1 || strings.Join(order, ",") != tc.order {
				t.Fatalf("done order = %v, want %s: %s", order, tc.order, out)
			}
			if got, present := encryptedOf(items[0]); !present || got != cipher {
				t.Fatalf("reasoning cipher = %q, present %v: %v", got, present, items[0])
			}
			assertDoneMatchesTerminal(t, out)
		})
	}
}

// TestGrokReconcileAddedOnlyPlaceholderDoesNotEmit proves an added-only silent
// placeholder neither emits reasoning frames nor holds later message text. The
// split text stays incremental and the terminal output contains only the
// message, with exact text and no invented completion.
func TestGrokReconcileAddedOnlyPlaceholderDoesNotEmit(t *testing.T) {
	var want strings.Builder
	events := []string{
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_placeholder", "in_progress"),
	}
	for i := 0; i < maxEncoderPendingEvents+1; i++ {
		events = append(events, sseTextDelta("msg_b", true, 1, "x"))
		want.WriteString("x")
	}
	events = append(events, completedEvent(finalOutput(sseMsgFinalItem("msg_b", want.String())), ""))
	_, out, err := encodeGrokReconcile(t, sseData(events...))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	frames := parseResponsesStream(t, out)
	completedAt := -1
	deltas := 0
	var streamed strings.Builder
	for i, f := range frames {
		switch f.Name {
		case "response.completed":
			completedAt = i
		case "response.reasoning_summary_text.delta", "response.reasoning_summary_part.added",
			"response.reasoning_summary_text.done", "response.reasoning_summary_part.done":
			t.Fatalf("placeholder emitted reasoning frame %q: %s", f.Name, out)
		case "response.output_text.delta":
			if completedAt >= 0 {
				t.Fatalf("text delta after completion: %s", out)
			}
			delta, _ := f.Data["delta"].(string)
			streamed.WriteString(delta)
			deltas++
		}
		if f.Name == "response.output_item.added" || f.Name == "response.output_item.done" {
			item, _ := f.Data["item"].(map[string]any)
			if item["type"] == "reasoning" {
				t.Fatalf("placeholder emitted reasoning item: %s", out)
			}
		}
	}
	if completedAt < 0 {
		t.Fatalf("stream did not complete: %s", out)
	}
	if deltas != maxEncoderPendingEvents+1 || streamed.String() != want.String() {
		t.Fatalf("text deltas = %d/%q, want %d/%q", deltas, streamed.String(), maxEncoderPendingEvents+1, want.String())
	}
	output, _ := finalResponseObject(t, out)["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("terminal output = %v, want only the message", output)
	}
	message, _ := output[0].(map[string]any)
	content, _ := message["content"].([]any)
	part, _ := content[0].(map[string]any)
	if message["type"] != "message" || part["text"] != want.String() {
		t.Fatalf("terminal message = %v, want exact text", message)
	}
}

// TestGrokReconcileConfirmedHoldPendingLimitFails checks a completion-confirmed
// reasoning item without bytes. Its hold stays bounded by the shared event and
// byte caps, the failure echoes no payload, and no successful completion is
// emitted after the error.
func TestGrokReconcileConfirmedHoldPendingLimitFails(t *testing.T) {
	const summary = `[{"type":"summary_text","text":"r"}]`
	header := []string{
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_h", "in_progress"),
		reasoningDeltaEvent(0, "rs_h", 0, "r"),
		reasoningItemDoneEvent(0, "rs_h", "completed", nil, summary),
	}
	assertHoldError := func(t *testing.T, out string, err error, sentinel string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "reasoning identity pending limit exceeded") {
			t.Fatalf("err = %v, want reasoning pending limit", err)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("error echoed payload: %v", err)
		}
		if strings.Contains(out, `"type":"response.completed"`) {
			t.Fatalf("successful completion after error: %s", out)
		}
	}
	t.Run("event count", func(t *testing.T) {
		events := append([]string{}, header...)
		var pending strings.Builder
		for i := 0; i < maxEncoderPendingEvents+1; i++ {
			events = append(events, sseTextDelta("msg_b", true, 1, "q"))
			pending.WriteString("q")
		}
		events = append(events, completedEvent(finalOutput(
			reasoningFinalItem("rs_h", "completed", strPtr("cipher"), summary),
			sseMsgFinalItem("msg_b", pending.String()),
		), ""))
		_, out, err := encodeGrokReconcile(t, sseData(events...))
		assertHoldError(t, out, err, "q")
	})
	t.Run("byte count", func(t *testing.T) {
		// Direct IR keeps the oversized payload out of one SSE line. The hold
		// still uses the same encoder bound as the full decode path.
		evs := []ir.StreamEvent{
			{Kind: ir.EventMessageStart, ID: "resp"},
			{Kind: ir.EventReasoningItemDone, ReasoningID: "rs_h", ReasoningIndexSet: true, ReasoningIndex: 0,
				Reasoning: &ir.ResponsesReasoning{ID: "rs_h", Status: "completed", Summary: []ir.ReasoningSummaryPart{{Type: "summary_text", Text: "r"}}}},
			{Kind: ir.EventTextDelta, Text: strings.Repeat("z", maxEncoderPendingBytes)},
		}
		out, err := encodeWithEncoder(t, NewReconciledStreamEncoder("m"), evs)
		assertHoldError(t, out, err, "zzz")
	})
}

// TestGrokReconcileEOFWithoutCipher proves a successful item-only EOF emits one
// completion without inventing opaque bytes.
func TestGrokReconcileEOFWithoutCipher(t *testing.T) {
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_e","summary":[]}}`,
		reasoningDeltaEvent(0, "rs_e", 0, "r"),
		reasoningItemDoneEvent(0, "rs_e", "completed", nil, `[{"type":"summary_text","text":"r"}]`),
	)
	_, out, err := encodeGrokReconcile(t, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	items, _ := reasoningDoneItems(t, out)
	if len(items) != 1 {
		t.Fatalf("reasoning done frames = %d: %s", len(items), out)
	}
	if _, present := encryptedOf(items[0]); present {
		t.Fatalf("EOF path invented encrypted content: %v", items[0])
	}
	if strings.Count(out, "event: response.output_item.done") != 1 {
		t.Fatalf("done frames = %d: %s", strings.Count(out, "event: response.output_item.done"), out)
	}
}

// TestGrokReconcilePendingTextKeepsOrder keeps a later message behind a late
// tool id and an unresolved reasoning completion, then drains it in order.
func TestGrokReconcilePendingTextKeepsOrder(t *testing.T) {
	const cipher = "cipher-text"
	const summary = `[{"type":"summary_text","text":"think"}]`
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		reasoningAddedEvent(0, "rs_t", "in_progress"),
		reasoningDeltaEvent(0, "rs_t", 0, "think"),
		addedNoItem(1, "call_a", "f"),
		sseTextDelta("msg_b", true, 2, "hi"),
		itemDoneEvent(1, "fc_a", "call_a", "f", `{}`),
		reasoningItemDoneEvent(0, "rs_t", "completed", nil, summary),
		`{"type":"response.output_item.done","output_index":2,"item":`+sseMsgFinalItem("msg_b", "hi")+`}`,
		completedEvent(finalOutput(
			reasoningFinalItem("rs_t", "completed", strPtr(cipher), summary),
			completedItem("fc_a", "call_a", "f", `{}`),
			sseMsgFinalItem("msg_b", "hi"),
		), ""),
	)
	_, out, err := encodeGrokReconcile(t, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Count(out, "event: response.output_item.done") != 3 {
		t.Fatalf("done frames = %d, want 3: %s", strings.Count(out, "event: response.output_item.done"), out)
	}
	assertDoneMatchesTerminal(t, out)
	items, order := reasoningDoneItems(t, out)
	if len(items) != 1 || strings.Join(order, ",") != "reasoning,function_call,message" {
		t.Fatalf("done order = %v, want reasoning,function_call,message: %s", order, out)
	}
	if got, present := encryptedOf(items[0]); !present || got != cipher {
		t.Fatalf("reasoning cipher = %q/%v: %v", got, present, items[0])
	}
	if !strings.Contains(out, `"text":"hi"`) || !strings.Contains(out, `"type":"output_text"`) {
		t.Fatalf("message text lost: %s", out)
	}
}
