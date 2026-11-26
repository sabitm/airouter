package responses

import (
	"net/http/httptest"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/sse"
)

// codexReconcileCollect decodes one body with the Codex reconcile policy.
func codexReconcileCollect(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var out []ir.StreamEvent
	err := DecodeStreamCodexReconciled(strings.NewReader(body), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	return out, err
}

// encodeCodexReconcile decodes a body with the Codex reconcile policy and
// re-encodes it with the matching encoder policy.
func encodeCodexReconcile(t *testing.T, body string) ([]ir.StreamEvent, string, error) {
	t.Helper()
	evs, err := codexReconcileCollect(t, body)
	if err != nil {
		return evs, "", err
	}
	enc := NewCodexStreamEncoder("m")
	out, err := encodeWithEncoder(t, enc, evs)
	return evs, out, err
}

func encodeWithEncoder(t *testing.T, enc *StreamEncoder, evs []ir.StreamEvent) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	w, ok := sse.NewWriter(rec)
	if !ok {
		t.Fatal("sse writer")
	}
	for _, ev := range evs {
		if err := enc.Encode(ev, w); err != nil {
			return rec.Body.String(), err
		}
	}
	if err := enc.Close(w); err != nil {
		return rec.Body.String(), err
	}
	return rec.Body.String(), nil
}

// reasoningItemJSON builds one reasoning wire item. encrypted nil omits the
// field; a non-nil pointer includes it, including the explicit empty string.
func reasoningItemJSON(id, status, summary, extra string, encrypted *string, _ string) string {
	item := `{"id":"` + id + `","type":"reasoning","summary":` + summary
	if status != "" {
		item += `,"status":"` + status + `"`
	}
	item += extra
	if encrypted != nil {
		item += `,"encrypted_content":"` + *encrypted + `"`
	}
	return item + "}"
}

// reasoningDoneItems returns the emitted output_item.done reasoning items in
// wire order, and every output_item.done item type in wire order.
func reasoningDoneItems(t *testing.T, body string) ([]map[string]any, []string) {
	t.Helper()
	var items []map[string]any
	var order []string
	for _, f := range parseResponsesStream(t, body) {
		if f.Name != "response.output_item.done" {
			continue
		}
		item, _ := f.Data["item"].(map[string]any)
		typ, _ := item["type"].(string)
		order = append(order, typ)
		if typ == "reasoning" {
			items = append(items, item)
		}
	}
	return items, order
}

func encryptedOf(item map[string]any) (string, bool) {
	value, ok := item["encrypted_content"]
	if !ok {
		return "", false
	}
	text, _ := value.(string)
	return text, true
}

// TestCodexReconcileToolBeforeReasoningDone covers the interleaving where a tool
// starts before the reasoning completion. The reasoning item keeps its arrival
// position and the one emitted completion carries the final bytes.
func TestCodexReconcileToolBeforeReasoningDone(t *testing.T) {
	const cipher = "cipher-final"
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
			added := `{"type":"response.output_item.added","output_index":0,"item":` + reasoningItemJSON("rs_a", "", "[]", "",
				nil, "in_progress") + `}`
			if tc.provisional != nil {
				added = `{"type":"response.output_item.added","output_index":0,"item":` + reasoningItemJSON("rs_a", "", "[]", "",
					tc.provisional, "in_progress") + `}`
			}
			done := `{"type":"response.output_item.done","output_index":0,"item":` + reasoningItemJSON("rs_a", "", `[{"type":"summary_text","text":"reason"}]`, "",
				tc.doneCipher, "completed") + `}`
			tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
			body := sseData(
				`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
				added,
				`{"type":"response.reasoning_summary_text.delta","item_id":"rs_a","output_index":0,"summary_index":0,"delta":"reason"}`,
				`{"type":"response.output_item.added","output_index":1,"item":`+tool+`}`,
				done,
				`{"type":"response.output_item.done","output_index":1,"item":`+tool+`}`,
				completedEvent(finalOutput(
					reasoningItemJSON("rs_a", "", `[{"type":"summary_text","text":"reason"}]`, "", tc.terminal, "completed"),
					completedItem("fc_a", "call_a", "f", `{"x":1}`),
				), ""),
			)
			_, out, err := encodeCodexReconcile(t, body)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			items, order := reasoningDoneItems(t, out)
			if len(items) != 1 {
				t.Fatalf("reasoning done frames = %d, want 1: %s", len(items), out)
			}
			if len(order) != 2 || order[0] != "reasoning" || order[1] != "function_call" {
				t.Fatalf("done order = %v, want reasoning then function_call: %s", order, out)
			}
			got, present := encryptedOf(items[0])
			if (tc.wantCipher == nil) != !present {
				t.Fatalf("encrypted presence = %v, want %v: %v", present, tc.wantCipher != nil, items[0])
			}
			if tc.wantCipher != nil && got != *tc.wantCipher {
				t.Fatalf("encrypted = %q, want %q", got, *tc.wantCipher)
			}
			if id, _ := items[0]["id"].(string); id != "rs_a" {
				t.Fatalf("reasoning id = %q, want rs_a", id)
			}
			if summary, _ := items[0]["summary"].([]any); len(summary) != 1 {
				t.Fatalf("summary = %v, want one part", items[0]["summary"])
			}
			terminalItems, _ := finalResponseObject(t, out)["output"].([]any)
			if len(terminalItems) != 2 {
				t.Fatalf("terminal output = %v", terminalItems)
			}
			terminalReasoning, _ := terminalItems[0].(map[string]any)
			if got, present := encryptedOf(terminalReasoning); (tc.wantCipher == nil) != !present || (tc.wantCipher != nil && got != *tc.wantCipher) {
				t.Fatalf("terminal reasoning = %v, want cipher %v", terminalReasoning, tc.wantCipher)
			}
		})
	}
}

// TestCodexReconcileSilentReasoningKeepsPosition checks that an opaque-only item
// with no readable summary does not reorder already emitted items, keeps its
// single completion, and stays consistent with the terminal output array.
func TestCodexReconcileSilentReasoningKeepsPosition(t *testing.T) {
	const cipher = "cipher-silent"
	tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
	text := `{"type":"message","id":"msg_a","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`
	cases := []struct {
		name  string
		body  string
		order []string
	}{
		{
			name: "reasoning before tool",
			body: sseData(
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
				`{"type":"response.output_item.added","output_index":1,"item":`+tool+`}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[],"encrypted_content":"`+cipher+`"}}`,
				`{"type":"response.output_item.done","output_index":1,"item":`+tool+`}`,
				completedEvent(finalOutput(
					reasoningItemJSON("rs_a", "", "[]", "", strPtr(cipher), "completed"), tool,
				), ""),
			),
			order: []string{"reasoning", "function_call"},
		},
		{
			name: "reasoning before text",
			body: sseData(
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
				`{"type":"response.output_text.delta","output_index":1,"item_id":"msg_a","content_index":0,"delta":"hi"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[],"encrypted_content":"`+cipher+`"}}`,
				`{"type":"response.output_item.done","output_index":1,"item":`+text+`}`,
				completedEvent(finalOutput(
					reasoningItemJSON("rs_a", "", "[]", "", strPtr(cipher), "completed"), text,
				), ""),
			),
			order: []string{"reasoning", "message"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := encodeCodexReconcile(t, tc.body)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			items, order := reasoningDoneItems(t, out)
			if len(items) != 1 {
				t.Fatalf("reasoning done frames = %d: %s", len(items), out)
			}
			if got, _ := encryptedOf(items[0]); got != cipher {
				t.Fatalf("encrypted = %q, want %q", got, cipher)
			}
			// The indexed added frame establishes the silent item's wire position.
			if strings.Join(order, ",") != strings.Join(tc.order, ",") {
				t.Fatalf("done order = %v, want %v: %s", order, tc.order, out)
			}
			assertDoneMatchesTerminal(t, out)
		})
	}
}

// assertDoneMatchesTerminal compares the emitted output_item.added/done order
// with the completed output array: every emitted item must occupy the terminal
// ordinal its frames used.
func assertDoneMatchesTerminal(t *testing.T, body string) {
	t.Helper()
	type slot struct{ typ, id string }
	var emitted []slot
	seen := map[string]int{}
	for _, f := range parseResponsesStream(t, body) {
		switch f.Name {
		case "response.output_item.added":
			item, _ := f.Data["item"].(map[string]any)
			typ, _ := item["type"].(string)
			id, _ := item["id"].(string)
			seen[id] = len(emitted)
			emitted = append(emitted, slot{typ, id})
		case "response.output_item.done":
			item, _ := f.Data["item"].(map[string]any)
			id, _ := item["id"].(string)
			typ, _ := item["type"].(string)
			idx, ok := seen[id]
			if !ok || emitted[idx].typ != typ {
				t.Fatalf("done without matching added for %s/%s", typ, id)
			}
			seen[id] = -1
		}
	}
	output, _ := finalResponseObject(t, body)["output"].([]any)
	if len(output) != len(emitted) {
		t.Fatalf("terminal output %d != emitted %d: %s", len(output), len(emitted), body)
	}
	for i, raw := range output {
		item, _ := raw.(map[string]any)
		id, _ := item["id"].(string)
		typ, _ := item["type"].(string)
		if emitted[i].id != id || emitted[i].typ != typ {
			t.Fatalf("terminal slot %d = %s/%s, emitted %s/%s", i, typ, id, emitted[i].typ, emitted[i].id)
		}
	}
}

// TestCodexReconcileRepeatedAndConflictingCompletions checks dedup and fail
// closed behavior.
func TestCodexReconcileRepeatedAndConflictingCompletions(t *testing.T) {
	const cipher = "cipher-a"
	base := sseData(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+reasoningItemJSON("rs_a", "", "[]", "", strPtr(cipher), "completed")+`}`,
	)
	evs, err := codexReconcileCollect(t, base+sseData(completedEvent(finalOutput(
		reasoningItemJSON("rs_a", "", "[]", "", strPtr(cipher), "completed"),
	), "")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	dones := 0
	for _, ev := range evs {
		if ev.Kind == ir.EventReasoningItemDone {
			dones++
		}
	}
	if dones != 1 {
		t.Fatalf("decoder done events = %d, want 1", dones)
	}

	conflict := sseData(
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+reasoningItemJSON("rs_a", "", "[]", "", strPtr(cipher), "completed")+`}`,
		completedEvent(finalOutput(
			reasoningItemJSON("rs_a", "", "[]", "", strPtr("cipher-b"), "completed"),
		), ""),
	)
	if _, err := codexReconcileCollect(t, conflict); err == nil || !strings.Contains(err.Error(), "conflicting reasoning encrypted content") {
		t.Fatalf("conflicting bytes err = %v", err)
	}
}

// TestCodexReconcileEOFFinishesWithoutCipher proves a successful item-only EOF
// still emits the one completion, without inventing opaque bytes.
func TestCodexReconcileEOFFinishesWithoutCipher(t *testing.T) {
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+reasoningItemJSON("rs_a", "", `[{"type":"summary_text","text":"r"}]`, "", nil, "completed")+`}`,
	)
	_, out, err := encodeCodexReconcile(t, body)
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

// TestCodexReconcileFailureAfterCommitEmitsNoSuccessProves a failed stream does
// not turn into a successful partial replay.
func TestCodexReconcileFailureAfterCommit(t *testing.T) {
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_a","output_index":0,"summary_index":0,"delta":"visible"}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+reasoningItemJSON("rs_a", "", `[{"type":"summary_text","text":"visible"}]`, "", nil, "completed")+`}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"type":"api_error","message":"boom"}}}`,
	)
	evs, err := codexReconcileCollect(t, body)
	if err == nil {
		t.Fatal("failed stream decoded as success")
	}
	for _, ev := range evs {
		if ev.Kind == ir.EventFinish {
			t.Fatalf("failed stream emitted Finish: %+v", evs)
		}
	}
}

// TestCodexReconcilePendingCapFails proves the shared pending bound still
// rejects an over-budget hold instead of growing without limit.
func TestCodexReconcilePendingCapFails(t *testing.T) {
	var events []string
	events = append(events, `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`)
	events = append(events, `{"type":"response.output_item.done","output_index":0,"item":`+reasoningItemJSON("rs_a", "", "[]", "", nil, "completed")+`}`)
	for i := 0; i < 8; i++ {
		events = append(events, `{"type":"response.output_text.delta","output_index":`+itoa(i+1)+`,"item_id":"msg_`+itoa(i)+`","content_index":0,"delta":"`+strings.Repeat("x", 32)+`"}`)
	}
	var out []ir.StreamEvent
	err := decodeStreamBoundedParts(strings.NewReader(sseData(events...)), func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	}, maxRetainedToolArgBytes, maxTrackedToolCalls, maxRetainedTextBytes, maxTrackedTextItems, maxTrackedTextParts,
		textQueueEntryOverhead, 2, maxRetainedReasoningBytes, maxTrackedReasoningItems, maxTrackedReasoningParts, decodePolicy{reconcileReasoning: true})
	if err == nil || !strings.Contains(err.Error(), "pending message text exceeded limit") {
		t.Fatalf("err = %v, want pending limit", err)
	}
}

// TestCodexReconcileSilentAfterToolTerminalOnly covers a reasoning item that
// streams no frames and completes without bytes after a tool. The one emitted
// completion must take the terminal bytes, not the earlier empty completion.
func TestCodexReconcileSilentAfterToolTerminalOnly(t *testing.T) {
	const cipher = "cipher-silent-terminal"
	tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":`+tool+`}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_b","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_b","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+tool+`}`,
		completedEvent(finalOutput(tool, reasoningItemJSON("rs_b", "", "[]", "", strPtr(cipher), "completed")), ""),
	)
	_, out, err := encodeCodexReconcile(t, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	items, _ := reasoningDoneItems(t, out)
	if len(items) != 1 {
		t.Fatalf("reasoning done frames = %d: %s", len(items), out)
	}
	if got, present := encryptedOf(items[0]); !present || got != cipher {
		t.Fatalf("reasoning completion = %v, want cipher %q", items[0], cipher)
	}
	assertDoneMatchesTerminal(t, out)
}

func TestCodexReconcileReasoningPlaceholderDoesNotCloseEarlierTool(t *testing.T) {
	body := sseData(
		addedEvent(0, "fc_a", "call_a", "f", ""),
		reasoningAddedEvent(1, "rs_b", "in_progress"),
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_a","delta":"{\"x\":1}"}`,
		itemDoneEvent(0, "fc_a", "call_a", "f", `{"x":1}`),
		reasoningItemDoneEvent(1, "rs_b", "completed", strPtr("cipher"), `[]`),
		completedEvent(finalOutput(
			completedItem("fc_a", "call_a", "f", `{"x":1}`),
			reasoningFinalItem("rs_b", "completed", strPtr("cipher"), `[]`),
		), ""),
	)
	_, out, err := encodeCodexReconcile(t, body)
	if err != nil {
		t.Fatal(err)
	}
	items, order := reasoningDoneItems(t, out)
	if strings.Join(order, ",") != "function_call,reasoning" || items[0]["encrypted_content"] != "cipher" {
		t.Fatalf("incorrect completed items: %v, %v", order, items)
	}
	for _, f := range parseResponsesStream(t, out) {
		if f.Name == "response.output_item.done" {
			item := f.Data["item"].(map[string]any)
			if item["id"] == "fc_a" && item["arguments"] != `{"x":1}` {
				t.Fatalf("tool arguments = %v", item["arguments"])
			}
		}
	}
	assertDoneMatchesTerminal(t, out)
}

func TestCodexReconcileLateReasoningIDBeforeTool(t *testing.T) {
	const summary = `[{"type":"summary_text","text":"ab"}]`
	body := sseData(
		reasoningDeltaEvent(0, "", 0, "a"),
		addedEvent(1, "fc_a", "call_a", "f", "{}"),
		reasoningDeltaEvent(0, "", 0, "b"),
		reasoningItemDoneEvent(0, "rs_a", "completed", nil, summary),
		itemDoneEvent(1, "fc_a", "call_a", "f", "{}"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_a", "completed", strPtr("cipher"), summary),
			completedItem("fc_a", "call_a", "f", "{}"),
		), ""),
	)
	_, out, err := encodeCodexReconcile(t, body)
	if err != nil {
		t.Fatal(err)
	}
	items, order := reasoningDoneItems(t, out)
	if strings.Join(order, ",") != "reasoning,function_call" || items[0]["id"] != "rs_a" || items[0]["encrypted_content"] != "cipher" {
		t.Fatalf("incorrect completed items: %v, %v", order, items)
	}
	assertDoneMatchesTerminal(t, out)
}

func TestCodexReconcileAcrossToolAndTwoReasoningItems(t *testing.T) {
	for _, cipherA := range []*string{strPtr("ca"), nil} {
		name := "both have ciphertext"
		if cipherA == nil {
			name = "first has no ciphertext"
		}
		t.Run(name, func(t *testing.T) {
			checkCodexReconcileAcrossItems(t, cipherA)
		})
	}
}

func checkCodexReconcileAcrossItems(t *testing.T, cipherA *string) {
	t.Helper()
	summaryA := `[{"type":"summary_text","text":"a"}]`
	summaryB := `[{"type":"summary_text","text":"b"}]`
	body := sseData(
		reasoningAddedEvent(0, "rs_a", "in_progress"),
		reasoningDeltaEvent(0, "rs_a", 0, "a"),
		addedEvent(1, "fc_a", "call_a", "f", "{}"),
		reasoningItemDoneEvent(0, "rs_a", "completed", nil, summaryA),
		reasoningAddedEvent(2, "rs_b", "in_progress"),
		reasoningDeltaEvent(2, "rs_b", 0, "b"),
		itemDoneEvent(1, "fc_a", "call_a", "f", "{}"),
		reasoningItemDoneEvent(2, "rs_b", "completed", nil, summaryB),
		sseTextDelta("msg_a", true, 3, "answer"),
		completedEvent(finalOutput(
			reasoningFinalItem("rs_a", "completed", cipherA, summaryA),
			completedItem("fc_a", "call_a", "f", "{}"),
			reasoningFinalItem("rs_b", "completed", strPtr("cb"), summaryB),
			sseMsgFinalItem("msg_a", "answer"),
		), ""),
	)
	_, out, err := encodeCodexReconcile(t, body)
	if err != nil {
		t.Fatal(err)
	}
	items, order := reasoningDoneItems(t, out)
	if strings.Join(order, ",") != "reasoning,function_call,reasoning,message" || len(items) != 2 || items[1]["encrypted_content"] != "cb" {
		t.Fatalf("incorrect completed items: %v, %v", order, items)
	}
	gotA, present := encryptedOf(items[0])
	if present != (cipherA != nil) || present && gotA != *cipherA {
		t.Fatalf("unexpected ciphertext on first item: %v", items[0])
	}
	assertDoneMatchesTerminal(t, out)
}

// TestCodexReconcileTerminalOmitsReasoning proves a held completion still emits
// exactly one done at Finish when no terminal snapshot carries the item.
func TestCodexReconcileTerminalOmitsReasoning(t *testing.T) {
	tool := `{"type":"function_call","id":"fc_a","call_id":"call_a","name":"f","arguments":"{\"x\":1}"}`
	body := sseData(
		`{"type":"response.created","response":{"id":"resp","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_a","output_index":0,"summary_index":0,"delta":"r"}`,
		`{"type":"response.output_item.added","output_index":1,"item":`+tool+`}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_a","summary":[{"type":"summary_text","text":"r"}]}}`,
		`{"type":"response.output_item.done","output_index":1,"item":`+tool+`}`,
		completedEvent(finalOutput(tool), ""),
	)
	_, out, err := encodeCodexReconcile(t, body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	items, types := reasoningDoneItems(t, out)
	if len(items) != 1 {
		t.Fatalf("reasoning done frames = %d: %s", len(items), out)
	}
	if types[0] != "reasoning" || types[1] != "function_call" {
		t.Fatalf("done order = %v: %s", types, out)
	}
	if _, present := encryptedOf(items[0]); present {
		t.Fatalf("omitted terminal invented bytes: %v", items[0])
	}
	if strings.Count(out, "event: response.output_item.done") != 2 {
		t.Fatalf("done frames = %d: %s", strings.Count(out, "event: response.output_item.done"), out)
	}
}
