package responses

import (
	"strings"
	"testing"
)

type nativeReconcilePolicy struct {
	name   string
	encode func(*testing.T, string) (string, error)
}

func nativeReconcilePolicies() []nativeReconcilePolicy {
	return []nativeReconcilePolicy{
		{
			name: "grok_build_native",
			encode: func(t *testing.T, body string) (string, error) {
				_, out, err := encodeGrokReconcile(t, body)
				return out, err
			},
		},
		{
			name: "codex",
			encode: func(t *testing.T, body string) (string, error) {
				_, out, err := encodeCodexReconcile(t, body)
				return out, err
			},
		},
	}
}

func assertReconciledReasoningItem(t *testing.T, item map[string]any, wantID, wantText string, wantCipher *string) {
	t.Helper()
	id, _ := item["id"].(string)
	typ, _ := item["type"].(string)
	status, _ := item["status"].(string)
	if id != wantID || typ != "reasoning" || status != "completed" {
		t.Fatalf("reasoning item = %v, want %s completed", item, wantID)
	}
	parts, _ := item["summary"].([]any)
	if len(parts) != 1 {
		t.Fatalf("reasoning summary = %v, want one part", item["summary"])
	}
	part, _ := parts[0].(map[string]any)
	if part["type"] != "summary_text" || part["text"] != wantText {
		t.Fatalf("reasoning summary = %v, want text %q", parts, wantText)
	}
	got, present := encryptedOf(item)
	if (wantCipher == nil) != !present || (wantCipher != nil && got != *wantCipher) {
		t.Fatalf("reasoning cipher = %q/%v, want %v: %v", got, present, wantCipher, item)
	}
}

// TestReconciledAdjacentIndexOnlyDeltas covers two adjacent summary fragments
// before the item ID. The second fragment must pass the empty queue as the
// awaited item's own frame; otherwise the completion replays it and fails the
// final snapshot.
func TestReconciledAdjacentIndexOnlyDeltas(t *testing.T) {
	const cipher = "cipher-adjacent"
	const summary = `[{"type":"summary_text","text":"ab"}]`
	body := sseData(
		reasoningDeltaEvent(0, "", 0, "a"),
		reasoningDeltaEvent(0, "", 0, "b"),
		reasoningItemDoneEvent(0, "rs_a", "completed", nil, summary),
		completedEvent(finalOutput(reasoningFinalItem("rs_a", "completed", strPtr(cipher), summary)), ""),
	)
	for _, policy := range nativeReconcilePolicies() {
		t.Run(policy.name, func(t *testing.T) {
			out, err := policy.encode(t, body)
			if err != nil {
				t.Fatalf("encode: %v; partial: %s", err, out)
			}
			if got := strings.Join(queuedSummaryDeltas(t, out), ""); got != "ab" {
				t.Fatalf("streamed summary = %q, want exact ab without duplication: %s", got, out)
			}
			items, order := reasoningDoneItems(t, out)
			if len(items) != 1 || strings.Join(order, ",") != "reasoning" {
				t.Fatalf("reasoning done items = %d, order %v: %s", len(items), order, out)
			}
			assertReconciledReasoningItem(t, items[0], "rs_a", "ab", strPtr(cipher))
			assertDoneMatchesTerminal(t, out)
			terminal, _ := finalResponseObject(t, out)["output"].([]any)
			if len(terminal) != 1 {
				t.Fatalf("terminal output = %v, want one reasoning item", terminal)
			}
			terminalItem, _ := terminal[0].(map[string]any)
			assertReconciledReasoningItem(t, terminalItem, "rs_a", "ab", strPtr(cipher))
		})
	}
}

// TestGrokReconcileTwoLateCompletionsAndLateID covers two reasoning items around
// a late native tool ID and a message. The first fragments stay ahead of the
// tool, the second completion keeps its own cipher, and queued text drains last.
func TestGrokReconcileTwoLateCompletionsAndLateID(t *testing.T) {
	const summaryA = `[{"type":"summary_text","text":"ab"}]`
	const summaryB = `[{"type":"summary_text","text":"next"}]`
	for _, cipherA := range []*string{nil, strPtr("cipher-a")} {
		name := "first terminal cipher missing"
		if cipherA != nil {
			name = "first terminal cipher present"
		}
		t.Run(name, func(t *testing.T) {
			body := sseData(
				reasoningDeltaEvent(0, "", 0, "a"),
				addedNoItem(1, "call_a", "f"),
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"x\":"}`,
				reasoningDeltaEvent(0, "", 0, "b"),
				reasoningItemDoneEvent(0, "rs_a", "completed", nil, summaryA),
				reasoningAddedEvent(2, "rs_b", "in_progress"),
				reasoningDeltaEvent(2, "rs_b", 0, "next"),
				`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"1}"}`,
				itemDoneEvent(1, "fc_a", "call_a", "f", `{"x":1}`),
				reasoningItemDoneEvent(2, "rs_b", "completed", nil, summaryB),
				sseTextDelta("msg_b", true, 3, "answer"),
				completedEvent(finalOutput(
					reasoningFinalItem("rs_a", "completed", cipherA, summaryA),
					completedItem("fc_a", "call_a", "f", `{"x":1}`),
					reasoningFinalItem("rs_b", "completed", strPtr("cipher-b"), summaryB),
					sseMsgFinalItem("msg_b", "answer"),
				), ""),
			)
			_, out, err := encodeGrokReconcile(t, body)
			if err != nil {
				t.Fatalf("encode: %v; partial: %s", err, out)
			}
			deltas := summaryDeltaStrings(t, out)
			if strings.Join(deltas, "|") != "rs_a@0:ab|rs_b@0:next" {
				t.Fatalf("summary deltas = %v, want ordered ab then next: %s", deltas, out)
			}
			items, order := reasoningDoneItems(t, out)
			if len(items) != 2 || strings.Join(order, ",") != "reasoning,function_call,reasoning,message" {
				t.Fatalf("reasoning done items = %d, order %v: %s", len(items), order, out)
			}
			assertReconciledReasoningItem(t, items[0], "rs_a", "ab", cipherA)
			assertReconciledReasoningItem(t, items[1], "rs_b", "next", strPtr("cipher-b"))
			assertDoneMatchesTerminal(t, out)
			terminal, _ := finalResponseObject(t, out)["output"].([]any)
			if len(terminal) != 4 {
				t.Fatalf("terminal output = %v, want four items", terminal)
			}
			itemA, _ := terminal[0].(map[string]any)
			tool, _ := terminal[1].(map[string]any)
			itemB, _ := terminal[2].(map[string]any)
			message, _ := terminal[3].(map[string]any)
			assertReconciledReasoningItem(t, itemA, "rs_a", "ab", cipherA)
			if tool["type"] != "function_call" || tool["id"] != "fc_a" || tool["call_id"] != "call_a" ||
				tool["name"] != "f" || tool["arguments"] != `{"x":1}` {
				t.Fatalf("terminal tool = %v", tool)
			}
			assertReconciledReasoningItem(t, itemB, "rs_b", "next", strPtr("cipher-b"))
			content, _ := message["content"].([]any)
			part, _ := content[0].(map[string]any)
			if message["type"] != "message" || len(content) != 1 || part["text"] != "answer" {
				t.Fatalf("terminal message = %v, want exact answer", message)
			}
		})
	}
}
