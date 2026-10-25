package responses

import (
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// sseTextDeltaCI builds an output_text delta that carries its content_index, so
// the decoder can attribute the bytes to a real message part.
func sseTextDeltaCI(itemID string, outIdx, contentIdx int, delta string) string {
	return fmt.Sprintf(`{"type":"response.output_text.delta","item_id":%q,"output_index":%d,"content_index":%d,"delta":%q}`, itemID, outIdx, contentIdx, delta)
}

func sseTextDone(itemID string, outIdx, contentIdx int, text string) string {
	return fmt.Sprintf(`{"type":"response.output_text.done","item_id":%q,"output_index":%d,"content_index":%d,"text":%q}`, itemID, outIdx, contentIdx, text)
}

func sseContentPartDone(itemID string, outIdx, contentIdx int, typ, text string) string {
	return fmt.Sprintf(`{"type":"response.content_part.done","item_id":%q,"output_index":%d,"content_index":%d,"part":{"type":%q,"text":%q}}`, itemID, outIdx, contentIdx, typ, text)
}

func sseTextContent(text string) string {
	return fmt.Sprintf(`{"type":"output_text","text":%q}`, text)
}

func sseItemDoneMessage(outIdx int, id string, parts ...string) string {
	content := "[" + strings.Join(parts, ",") + "]"
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"id":%q,"type":"message","status":"completed","role":"assistant","content":%s}}`, outIdx, id, content)
}

func sseItemDoneMessageNoContent(outIdx int, id string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"id":%q,"type":"message","status":"completed","role":"assistant"}}`, outIdx, id)
}

// joinText concatenates every EventTextDelta in order and returns the fragment
// count so a regression asserts emission order, not just the joined value.
func joinText(evs []ir.StreamEvent) (string, int) {
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

// TestDecodeStreamTextPartLateDoneAndRecovery covers per-content-part
// reconciliation: a late part-zero done after later-part bytes is valid,
// missing per-part suffixes recover once in canonical order, and part-one
// done is not silently skipped.
func TestDecodeStreamTextPartLateDoneAndRecovery(t *testing.T) {
	t.Run("late part zero done after part one streamed", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "part1"),
			sseTextDeltaCI("msg_a", 0, 1, "part2"),
			sseTextDone("msg_a", 0, 0, "part1"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatalf("late part done rejected: %v", err)
		}
		if got, _ := joinText(evs); got != "part1part2" {
			t.Fatalf("text = %q, want part1part2", got)
		}
	})

	t.Run("part one missing suffix recovered once in order", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, "a"),
			sseTextDeltaCI("msg_a", 0, 1, "b"),
			sseContentPartDone("msg_a", 0, 1, "output_text", "bcd"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatalf("part one recovery failed: %v", err)
		}
		got, n := joinText(evs)
		if got != "abcd" {
			t.Fatalf("text = %q, want abcd", got)
		}
		if n != 3 {
			t.Fatalf("fragments = %d, want 3 (a, b, cd)", n)
		}
	})

	t.Run("part zero done then valid part one accepted", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, "a"),
			sseTextDeltaCI("msg_a", 0, 1, "b"),
			sseContentPartDone("msg_a", 0, 1, "output_text", "b"),
			completedEvent(finalOutput(`{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}`), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatalf("part zero then part one rejected: %v", err)
		}
		if got, _ := joinText(evs); got != "ab" {
			t.Fatalf("text = %q, want ab", got)
		}
	})
}

// TestDecodeStreamTextPartTerminalProtection covers part done validation and
// terminal-state protection for message text.
func TestDecodeStreamTextPartTerminalProtection(t *testing.T) {
	t.Run("explicit empty part done with retained text rejected", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, ""),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("identical repeated part done adds no duplicate", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, "a"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatal(err)
		}
		if got, n := joinText(evs); got != "a" || n != 1 {
			t.Fatalf("text = %q (%d), want a once", got, n)
		}
	})

	t.Run("changed part done after close rejected", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, "a"),
			sseTextDone("msg_a", 0, 0, "ab"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("part done after response terminal growth rejected", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "a")), ""),
			sseTextDone("msg_a", 0, 0, "ab"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("part done after item terminal growth rejected", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseItemDoneMessage(0, "msg_a", sseTextContent("a")),
			sseTextDone("msg_a", 0, 0, "ab"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("consistent duplicate after response terminal accepted", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "a")), ""),
			sseTextDone("msg_a", 0, 0, "a"),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatalf("consistent duplicate rejected: %v", err)
		}
		if got, n := joinText(evs); got != "a" || n != 1 {
			t.Fatalf("text = %q (%d), want a once", got, n)
		}
	})

	t.Run("explicit empty item done with retained text rejected", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseItemDoneMessage(0, "msg_a"),
		)
		_, err := collectStream(t, body)
		if err == nil || !strings.Contains(err.Error(), "conflicting") {
			t.Fatalf("err = %v, want conflicting message text", err)
		}
	})

	t.Run("absent content item done stays placeholder", func(t *testing.T) {
		body := sseData(
			sseTextDeltaCI("msg_a", 0, 0, "a"),
			sseItemDoneMessageNoContent(0, "msg_a"),
			completedEvent(finalOutput(sseMsgFinalItem("msg_a", "a")), ""),
		)
		evs, err := collectStream(t, body)
		if err != nil {
			t.Fatalf("placeholder item done rejected: %v", err)
		}
		if got, _ := joinText(evs); got != "a" {
			t.Fatalf("text = %q, want a", got)
		}
	})
}

// TestDecodeStreamTextPartMalformed verifies that a malformed supported part
// field fails with a payload-free error instead of being dropped.
func TestDecodeStreamTextPartMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
		leak string
	}{
		{
			name: "content part text number",
			body: sseData(`{"type":"response.content_part.done","item_id":"msg_a","output_index":0,"content_index":0,"part":{"type":"output_text","text":12345}}`),
			leak: "12345",
		},
		{
			name: "output text done text number",
			body: sseData(`{"type":"response.output_text.done","item_id":"msg_a","output_index":0,"content_index":0,"text":12345}`),
			leak: "12345",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStream(t, tc.body)
			if err == nil {
				t.Fatalf("malformed part field accepted")
			}
			if strings.Contains(err.Error(), tc.leak) {
				t.Fatalf("error echoed a field value: %v", err)
			}
		})
	}
}

// TestDecodeStreamTextPartBounds covers the injected content-part cap and the
// net-charge rule: an identical complete snapshot is free.
func TestDecodeStreamTextPartBounds(t *testing.T) {
	emit := func(ir.StreamEvent) error { return nil }

	t.Run("part count bound rejects excess part", func(t *testing.T) {
		d := newTextDecoder(emit, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
		d.maxTextParts = 2
		if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_a", 0, 0, "a")), false); err != nil {
			t.Fatal(err)
		}
		if err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_b", 1, 0, "b")), false); err != nil {
			t.Fatal(err)
		}
		err := d.observeTextDelta(textDeltaEnv(t, sseTextDeltaCI("msg_b", 1, 1, "c")), false)
		if err == nil || !strings.Contains(err.Error(), "too many message text parts") {
			t.Fatalf("err = %v, want too many message text parts", err)
		}
	})

	t.Run("repeated complete snapshot charges once", func(t *testing.T) {
		d := newTextDecoder(emit, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
		env := textDeltaEnv(t, sseItemDoneMessage(0, "msg_a", sseTextContent("hello")))
		if err := d.observeMessageDone(env); err != nil {
			t.Fatal(err)
		}
		after := d.textBytes
		if err := d.observeMessageDone(env); err != nil {
			t.Fatal(err)
		}
		if d.textBytes != after {
			t.Fatalf("repeated snapshot charged %d -> %d", after, d.textBytes)
		}
	})
}

// TestDecodeStreamToEncodeStreamMultipartTextOrder runs the fixed decode order
// through the real stream encoder so frame lifecycle keeps the recovered part.
func TestDecodeStreamToEncodeStreamMultipartTextOrder(t *testing.T) {
	body := sseData(
		sseTextDeltaCI("msg_a", 0, 0, "part1"),
		sseTextDeltaCI("msg_a", 0, 1, "part2"),
		sseTextDone("msg_a", 0, 0, "part1"),
	)
	evs, err := collectStream(t, body)
	if err != nil {
		t.Fatal(err)
	}
	wire := runQueueEvents(t, evs, true)
	if got := strings.Join(queuedTextDeltas(t, wire), ""); got != "part1part2" {
		t.Fatalf("encoded text deltas = %q, want part1part2", got)
	}
	final := finalResponseObject(t, wire)
	output, _ := final["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("encoded output = %+v, want one message", output)
	}
	msg, _ := output[0].(map[string]any)
	if got := queuedSummaryTextOfContent(t, msg); len(got) != 1 || got[0] != "part1part2" {
		t.Fatalf("encoded message content = %v, want part1part2", msg["content"])
	}
}

// collectStreamNoPanic runs the real DecodeStream path and converts a decoder
// panic into a clear test failure, so a stale text-part offset fails one test
// instead of crashing the package run.
func collectStreamNoPanic(t *testing.T, body string) ([]ir.StreamEvent, error) {
	t.Helper()
	var (
		evs []ir.StreamEvent
		err error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("DecodeStream panicked: %v", r)
			}
		}()
		evs, err = collectStream(t, body)
	}()
	return evs, err
}

// assertPartOffsetsMonotonic checks the text-part invariant: parts partition the
// aggregate so partStart(i) <= parts[i].end <= text.Len for every index.
func assertPartOffsetsMonotonic(t *testing.T, item *responsesTextItem) {
	t.Helper()
	prev := 0
	for i, p := range item.parts {
		if p.end < prev || p.end > item.text.Len() {
			t.Fatalf("part %d end %d outside [%d,%d]", i, p.end, prev, item.text.Len())
		}
		prev = p.end
	}
}

// TestDecodeStreamTextPartEarlierGrowthShift reproduces the slice panic: an
// empty later part done creates placeholders, an earlier part then grows, and a
// later reconcile slices text[start:end] with a stale end. Growth around later
// empty placeholders must shift every later boundary, keeping start <= end.
func TestDecodeStreamTextPartEarlierGrowthShift(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		frag int
	}{
		{
			name: "later empty part done then earlier growth",
			body: sseData(
				sseTextDone("msg_a", 0, 1, ""),
				sseTextDone("msg_a", 0, 0, "a"),
				sseTextDone("msg_a", 0, 1, ""),
			),
			want: "a",
			frag: 1,
		},
		{
			name: "index two gap then earlier growth",
			body: sseData(
				sseTextDone("msg_a", 0, 2, ""),
				sseTextDone("msg_a", 0, 0, "a"),
				sseTextDone("msg_a", 0, 1, ""),
				sseTextDone("msg_a", 0, 2, ""),
			),
			want: "a",
			frag: 1,
		},
		{
			name: "two earlier parts grow around trailing placeholder",
			body: sseData(
				sseTextDone("msg_a", 0, 2, ""),
				sseTextDone("msg_a", 0, 0, "a"),
				sseTextDone("msg_a", 0, 1, "b"),
				sseTextDone("msg_a", 0, 2, ""),
			),
			want: "ab",
			frag: 2,
		},
		{
			name: "whole message done after earlier growth",
			body: sseData(
				sseTextDone("msg_a", 0, 1, ""),
				sseTextDone("msg_a", 0, 0, "a"),
				sseItemDoneMessage(0, "msg_a", sseTextContent("a"), sseTextContent("")),
			),
			want: "a",
			frag: 1,
		},
		{
			name: "final completion after earlier growth",
			body: sseData(
				sseTextDone("msg_a", 0, 1, ""),
				sseTextDone("msg_a", 0, 0, "a"),
				completedEvent(finalOutput(`{"id":"msg_a","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"a"},{"type":"output_text","text":""}]}`), ""),
			),
			want: "a",
			frag: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := collectStreamNoPanic(t, tc.body)
			if err != nil {
				t.Fatalf("decode failed: %v", err)
			}
			got, n := joinText(evs)
			if got != tc.want || n != tc.frag {
				t.Fatalf("text = %q (%d fragments), want %q (%d)", got, n, tc.want, tc.frag)
			}
		})
	}
}

// TestReconcileTextPartOffsetsMonotonic asserts the offset invariant directly on
// the decoder state after each growth, so a stale end is caught before any
// slice panic and no part can point past the aggregate text.
func TestReconcileTextPartOffsetsMonotonic(t *testing.T) {
	d := newTextDecoder(func(ir.StreamEvent) error { return nil }, maxRetainedTextBytes, maxTrackedTextItems, maxPendingTextBytes, maxPendingTextEvents)
	if err := d.observeTextDone(textDeltaEnv(t, sseTextDone("msg_a", 0, 2, "")), false); err != nil {
		t.Fatal(err)
	}
	if err := d.observeTextDone(textDeltaEnv(t, sseTextDone("msg_a", 0, 0, "a")), false); err != nil {
		t.Fatal(err)
	}
	item := d.textByItem["msg_a"]
	assertPartOffsetsMonotonic(t, item)
	if err := d.observeTextDone(textDeltaEnv(t, sseTextDone("msg_a", 0, 1, "b")), false); err != nil {
		t.Fatal(err)
	}
	assertPartOffsetsMonotonic(t, item)
	if err := d.observeTextDone(textDeltaEnv(t, sseTextDone("msg_a", 0, 2, "")), false); err != nil {
		t.Fatal(err)
	}
	assertPartOffsetsMonotonic(t, item)
}

// TestDecodeStreamTextPartIndexBounds keeps full index validation: a negative
// or out-of-range content_index is a protocol error, never an unbounded slice
// or allocation.
func TestDecodeStreamTextPartIndexBounds(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"negative content index", sseData(sseTextDone("msg_a", 0, -1, "x"))},
		{"huge content index", sseData(sseTextDone("msg_a", 0, 1<<30, "x"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectStreamNoPanic(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), "part index out of range") {
				t.Fatalf("err = %v, want part index out of range", err)
			}
		})
	}
}
