package anthropic

import (
	"fmt"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// finding9LateFrag is argument bytes sent after the tool block already stopped.
// It avoids quotes so SSE JSON embedding needs no escaping and the payload-free
// error check can match it byte-for-byte.
const finding9LateFrag = "LATE_TAIL_BYTES"

const finding9MsgStart = `{"type":"message_start","message":{"id":"msg_9","model":"m","usage":{"input_tokens":1}}}`

const finding9MsgDeltaTool = `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`

const finding9MsgStop = `{"type":"message_stop"}`

func finding9ToolStart(index int, id, name string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, index, id, name)
}

func finding9ToolDelta(index int, frag string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%q}}`, index, frag)
}

func finding9TextStart(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":%q}}`, index, text)
}

func finding9TextDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, index, text)
}

func finding9Stop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

func finding9AssertProtocolError(t *testing.T, err error, events []ir.StreamEvent, secret string) *ir.StreamFailure {
	t.Helper()
	assertPayloadFreeSequenceError(t, err, events, secret)
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if sf.Type != "invalid_request_error" {
		t.Fatalf("failure type = %q want invalid_request_error", sf.Type)
	}
	return sf
}

func finding9CountKind(events []ir.StreamEvent, kind ir.StreamEventKind) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func finding9ArgsByIndex(events []ir.StreamEvent) map[int]string {
	args := map[int]string{}
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta {
			args[ev.Index] += ev.ArgsFrag
		}
	}
	return args
}

// A fragment arriving after content_block_stop closed the tool must be rejected
// before it is emitted, with a typed payload-free protocol error.
func TestFinding9LateToolFragmentRejected(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"city":"SF"}`),
		finding9Stop(0),
		finding9ToolDelta(0, finding9LateFrag),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, finding9LateFrag)
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta && strings.Contains(ev.ArgsFrag, finding9LateFrag) {
			t.Fatalf("late fragment emitted after stop: %+v", ev)
		}
		if ev.Kind == ir.EventFinish {
			t.Fatal("finish emitted after late fragment failure")
		}
	}
	if args := finding9ArgsByIndex(events); args[0] != `{"city":"SF"}` {
		t.Fatalf("valid prefix fragments = %q, want %q", args[0], `{"city":"SF"}`)
	}
}

// A tool start on an index already spent by a stopped tool is a restart and
// must be rejected before the second identity is emitted.
func TestFinding9ToolStartReuseOfStoppedToolIndexRejected(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1}`),
		finding9Stop(0),
		finding9ToolStart(0, "toolu_2", "send_email"),
		finding9ToolDelta(0, `{"b":2}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, "send_email")
	if n := finding9CountKind(events, ir.EventToolCallStart); n != 1 {
		t.Fatalf("tool starts = %d, want 1", n)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart && ev.ToolName != "get_weather" {
			t.Fatalf("restart identity emitted: %+v", ev)
		}
	}
}

// A repeated tool start on an index that is still open is tolerated identity
// backfill even when a higher index's stop advanced stoppedThrough past it.
// Only reuse of a spent index is a restart.
func TestFinding9RepeatedToolStartOnOpenIndexAfterHigherStopBackfills(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1`),
		finding9TextStart(5, ""),
		finding9TextDelta(5, "mid"),
		finding9Stop(5),
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `,"b":2}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatalf("repeated start on a still-open index was rejected after a higher stop: %v", err)
	}
	var starts []ir.StreamEvent
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart && ev.Index == 0 {
			starts = append(starts, ev)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("tool starts for index 0 = %d, want 2", len(starts))
	}
	if starts[1].ToolID != "toolu_1" || starts[1].ToolName != "get_weather" {
		t.Fatalf("backfill identity = %+v, want toolu_1/get_weather", starts[1])
	}
	if args := finding9ArgsByIndex(events); args[0] != `{"a":1,"b":2}` {
		t.Fatalf("args = %q, want %q", args[0], `{"a":1,"b":2}`)
	}
	if n := finding9CountKind(events, ir.EventFinish); n != 1 {
		t.Fatalf("finish events = %d, want 1", n)
	}
}

// At the open-tool cap, a repeated start on an existing open index adds no
// state and stays allowed with its identity backfilled; opening a new index
// at the cap is still rejected.
func TestFinding9RepeatedToolStartAtCapBackfillsAndNewIndexRejected(t *testing.T) {
	datas := []string{finding9MsgStart}
	for i := 0; i < finding9OpenToolCap; i++ {
		datas = append(datas, finding9ToolStart(i, fmt.Sprintf("toolu_%d", i), "get_weather"))
	}
	datas = append(datas,
		finding9ToolStart(0, "toolu_0", "get_weather"),
		finding9ToolStart(finding9OpenToolCap, "toolu_over", "send_email"),
	)
	events, err := collectDecode(t, anthropicSSE(datas...))
	finding9AssertProtocolError(t, err, events, "send_email")
	if n := finding9CountKind(events, ir.EventToolCallStart); n != finding9OpenToolCap+1 {
		t.Fatalf("tool starts = %d, want %d (cap plus backfill)", n, finding9OpenToolCap+1)
	}
	var starts0 []ir.StreamEvent
	for _, ev := range events {
		if ev.Kind != ir.EventToolCallStart {
			continue
		}
		if ev.Index == finding9OpenToolCap || ev.ToolName == "send_email" {
			t.Fatalf("over-cap start emitted: %+v", ev)
		}
		if ev.Index == 0 {
			starts0 = append(starts0, ev)
		}
	}
	if len(starts0) != 2 {
		t.Fatalf("starts for index 0 = %d, want 2 (original plus backfill)", len(starts0))
	}
	if starts0[1].ToolID != "toolu_0" || starts0[1].ToolName != "get_weather" {
		t.Fatalf("backfill identity = %+v, want toolu_0/get_weather", starts0[1])
	}
}

// A text delta on a still-open text index is accepted even after a higher tool
// stop advanced stoppedThrough past it. Once that text block stops, its late
// deltas stay rejected.
func TestFinding9OpenTextDeltaBelowToolStopWatermark(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9TextStart(0, ""),
		finding9ToolStart(5, "toolu_1", "get_weather"),
		finding9Stop(5),
		finding9TextDelta(0, "hi"),
		finding9Stop(0),
		finding9TextDelta(0, finding9LateFrag),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, finding9LateFrag)
	deltas := 0
	for _, ev := range events {
		if ev.Kind != ir.EventTextDelta {
			continue
		}
		deltas++
		if !ev.TextIndexed || ev.Index != 0 || ev.Text != "hi" {
			t.Fatalf("open text delta = %+v, want indexed index 0 %q", ev, "hi")
		}
	}
	if deltas != 1 {
		t.Fatalf("text deltas = %d, want 1", deltas)
	}
}

// A tool start on an index spent by a stopped text block is cross-kind reuse.
func TestFinding9ToolStartReuseOfTextStoppedIndexRejected(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9TextStart(0, ""),
		finding9TextDelta(0, "hi"),
		finding9Stop(0),
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, "get_weather")
	if n := finding9CountKind(events, ir.EventToolCallStart); n != 0 {
		t.Fatalf("tool starts = %d, want 0", n)
	}
}

// A tool start on an index spent by a stopped server block is cross-kind reuse.
func TestFinding9ToolStartReuseOfServerStoppedIndexRejected(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		`{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}}`,
		finding9Stop(0),
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, "get_weather")
	if n := finding9CountKind(events, ir.EventToolCallStart); n != 0 {
		t.Fatalf("tool starts = %d, want 0", n)
	}
}

// A tool still open at a lower index keeps receiving fragments after a higher
// index closes; the stoppedThrough watermark must not mark the open call done.
func TestFinding9StillOpenToolKeepsFragmentsAfterHigherStop(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"ci`),
		finding9TextStart(5, ""),
		finding9TextDelta(5, "mid"),
		finding9Stop(5),
		finding9ToolDelta(0, `ty":"SF"}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatalf("still-open lower-index tool was closed by a higher stop: %v", err)
	}
	args := finding9ArgsByIndex(events)
	if args[0] != `{"city":"SF"}` {
		t.Fatalf("fragmented args = %q, want %q", args[0], `{"city":"SF"}`)
	}
	if n := finding9CountKind(events, ir.EventFinish); n != 1 {
		t.Fatalf("finish events = %d, want 1", n)
	}
}

// finding9OpenToolCap pins the simultaneous open-tool bound at the same value
// as the thinking and text open-block caps. It is a local literal so this file
// still compiles against a production tree that lacks the bound entirely.
const finding9OpenToolCap = 64

// Simultaneously open tools are bounded; the bound counts open blocks, not
// stream history, and matches the thinking/text open-block caps.
func TestFinding9OpenToolBound(t *testing.T) {
	datas := []string{finding9MsgStart}
	for i := 0; i < finding9OpenToolCap; i++ {
		datas = append(datas, finding9ToolStart(i, fmt.Sprintf("toolu_%d", i), "get_weather"))
	}
	datas = append(datas, finding9ToolStart(finding9OpenToolCap, "toolu_over", "send_email"))
	events, err := collectDecode(t, anthropicSSE(datas...))
	finding9AssertProtocolError(t, err, events, "send_email")
	if n := finding9CountKind(events, ir.EventToolCallStart); n != finding9OpenToolCap {
		t.Fatalf("tool starts = %d, want %d", n, finding9OpenToolCap)
	}
}

// Sequential tool lifecycles release per-tool state. After a long history a
// late fragment for a long-closed tool is still rejected.
func TestFinding9SequentialToolsReleaseLifecycleState(t *testing.T) {
	const cycles = 300
	datas := []string{finding9MsgStart}
	for i := 0; i < cycles; i++ {
		datas = append(datas,
			finding9ToolStart(i, fmt.Sprintf("toolu_%d", i), "get_weather"),
			finding9ToolDelta(i, fmt.Sprintf(`{"v":%d}`, i)),
			finding9Stop(i),
		)
	}
	datas = append(datas, finding9ToolDelta(0, finding9LateFrag), finding9MsgDeltaTool, finding9MsgStop)
	events, err := collectDecode(t, anthropicSSE(datas...))
	finding9AssertProtocolError(t, err, events, finding9LateFrag)
	if n := finding9CountKind(events, ir.EventToolCallStart); n != cycles {
		t.Fatalf("tool starts = %d, want %d", n, cycles)
	}
	args := finding9ArgsByIndex(events)
	if args[0] != `{"v":0}` {
		t.Fatalf("tool 0 args = %q, want %q", args[0], `{"v":0}`)
	}
	if strings.Contains(args[0], finding9LateFrag) {
		t.Fatalf("late fragment emitted after long history: %q", args[0])
	}
}

// A text delta on an index spent by a stopped tool block is a stale cross-kind
// delta, consistent with the existing stoppedThrough rejection for other kinds.
func TestFinding9TextDeltaOnToolStoppedIndexRejected(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1}`),
		finding9Stop(0),
		finding9TextDelta(0, "hi"),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, "hi")
	if n := finding9CountKind(events, ir.EventTextDelta); n != 0 {
		t.Fatalf("text deltas = %d, want 0", n)
	}
}

// Valid fragmented arguments for one tool survive the lifecycle tracking.
func TestFinding9ValidFragmentedToolStream(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"ci`),
		finding9ToolDelta(0, `ty":"SF"}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	args := finding9ArgsByIndex(events)
	if args[0] != `{"city":"SF"}` {
		t.Fatalf("args = %q, want %q", args[0], `{"city":"SF"}`)
	}
	if events[1].Kind != ir.EventToolCallStart || events[1].ToolID != "toolu_1" || events[1].ToolName != "get_weather" {
		t.Fatalf("start = %+v", events[1])
	}
	if last := events[len(events)-1]; last.Kind != ir.EventFinish || last.StopReason != ir.StopToolUse || last.OutputTokens != 4 {
		t.Fatalf("finish = %+v", last)
	}
}

// Multiple tools with gapped indexes and stops out of index order keep their
// fragments attributed to the right call.
func TestFinding9GappedInterleavedTools(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_a", "alpha"),
		finding9ToolDelta(0, `{"x":`),
		finding9ToolStart(3, "toolu_b", "beta"),
		finding9ToolDelta(3, `{"y":`),
		finding9ToolDelta(3, `2}`),
		finding9Stop(3),
		finding9ToolDelta(0, `1}`),
		finding9Stop(0),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	args := finding9ArgsByIndex(events)
	if args[0] != `{"x":1}` || args[3] != `{"y":2}` {
		t.Fatalf("args = %v, want 0:%q 3:%q", args, `{"x":1}`, `{"y":2}`)
	}
}

// A tool delta on an index that never started and never stopped is still
// relayed; missing starts are not newly rejected by the lifecycle tracking.
func TestFinding9LegacyUnstartedToolDeltaRelays(t *testing.T) {
	events, err := collectDecode(t, anthropicSSE(finding9ToolDelta(4, `{"o":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != ir.EventToolCallDelta || events[0].Index != 4 || events[0].ArgsFrag != `{"o":1}` {
		t.Fatalf("events = %+v", events)
	}
	if events[1].Kind != ir.EventFinish {
		t.Fatalf("finish = %+v", events[1])
	}
}

// Legacy unindexed text deltas keep working while a tool block is open, and an
// open tool at message_stop stays tolerated as before.
func TestFinding9LegacyTextDeltaWithOpenTool(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(1, "toolu_1", "get_weather"),
		finding9TextDelta(0, "hi"),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	if err != nil {
		t.Fatal(err)
	}
	var text *ir.StreamEvent
	for i := range events {
		if events[i].Kind == ir.EventTextDelta {
			text = &events[i]
		}
	}
	if text == nil || text.TextIndexed || text.Index != 0 || text.Text != "hi" {
		t.Fatalf("legacy text delta = %+v", text)
	}
	if last := events[len(events)-1]; last.Kind != ir.EventFinish {
		t.Fatalf("finish = %+v", last)
	}
}

// A stream that ends without message_stop while a tool is open still
// synthesizes Finish; EOF with open tools is not newly rejected.
func TestFinding9EOFSynthesizesFinishWithOpenTool(t *testing.T) {
	events, err := collectDecode(t, anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1}`),
	))
	if err != nil {
		t.Fatal(err)
	}
	if last := events[len(events)-1]; last.Kind != ir.EventFinish {
		t.Fatalf("finish = %+v", last)
	}
}

// A late fragment rejected through the watermark after a later, higher index
// closed stays rejected; this pins the mixed-kind precedent.
func TestFinding9LateFragmentAfterLaterHigherStopRejected(t *testing.T) {
	body := anthropicSSE(
		finding9MsgStart,
		finding9ToolStart(0, "toolu_1", "get_weather"),
		finding9ToolDelta(0, `{"a":1}`),
		finding9Stop(0),
		finding9TextStart(5, ""),
		finding9TextDelta(5, "mid"),
		finding9Stop(5),
		finding9ToolDelta(0, finding9LateFrag),
		finding9MsgDeltaTool,
		finding9MsgStop,
	)
	events, err := collectDecode(t, body)
	finding9AssertProtocolError(t, err, events, finding9LateFrag)
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta && strings.Contains(ev.ArgsFrag, finding9LateFrag) {
			t.Fatalf("late fragment emitted: %+v", ev)
		}
	}
}
