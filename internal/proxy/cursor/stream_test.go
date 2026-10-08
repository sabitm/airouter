package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/openai"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/sse"
)

// agentFrame builds a Connect-RPC frame (uncompressed) wrapping the given
// protobuf.
func agentFrame(t *testing.T, proto []byte) []byte {
	t.Helper()
	return wrapConnectFrame(proto, false)
}

// interactionUpdateFrame wraps one InteractionUpdate variant.
func interactionUpdateFrame(t *testing.T, field int, inner []byte) []byte {
	t.Helper()
	return agentFrame(t, encodeField(asmInteractionUpdate, wireLen,
		encodeField(field, wireLen, inner)))
}

func agentTextFrame(t *testing.T, text string) []byte {
	t.Helper()
	return interactionUpdateFrame(t, iuTextDelta, encodeField(tdText, wireLen, text))
}

func agentTurnEndedFrame(t *testing.T, in, out uint64) []byte {
	t.Helper()
	inner := concatBytes(
		encodeField(teInputTokens, wireVarint, in),
		encodeField(teOutputTokens, wireVarint, out),
	)
	return interactionUpdateFrame(t, iuTurnEnded, inner)
}

func connectEndStreamFrame(t *testing.T, payload []byte) []byte {
	t.Helper()
	frame := wrapConnectFrame(payload, false)
	frame[0] = flagTrailer
	return frame
}

func agentTurnEndedCacheFrame(t *testing.T, in, out, read, write uint64) []byte {
	t.Helper()
	inner := concatBytes(
		encodeField(teInputTokens, wireVarint, in),
		encodeField(teOutputTokens, wireVarint, out),
		encodeField(teCacheReadTokens, wireVarint, read),
		encodeField(teCacheWriteTokens, wireVarint, write),
	)
	return interactionUpdateFrame(t, iuTurnEnded, inner)
}

// kvServerFrame builds a KvServerMessage (field 4) with the given variant.
func kvServerFrame(t *testing.T, variant int) []byte {
	t.Helper()
	kv := concatBytes(
		encodeField(kvsID, wireVarint, uint64(7)),
		encodeField(variant, wireLen, []byte{}),
	)
	return agentFrame(t, encodeField(asmKVServerMessage, wireLen, kv))
}

// execRequestContextFrame builds an ExecServerMessage with request_context_args.
func execRequestContextFrame(t *testing.T) []byte {
	t.Helper()
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(3)),
		encodeField(esmExecID, wireLen, "exec-1"),
		encodeField(esmRequestContextArgs, wireLen, []byte{}),
	)
	return agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex))
}

// mcpExecFrame builds an ExecServerMessage carrying mcp_args.
func mcpExecFrame(t *testing.T, callID, toolName string, args map[string]any) []byte {
	t.Helper()
	var argsMap []byte
	for k, v := range args {
		val := encodeField(3, wireLen, []byte(v.(string))) // string_value (direct scalar)
		entry := concatBytes(encodeField(1, wireLen, k), encodeField(2, wireLen, val))
		argsMap = append(argsMap, encodeField(maArgs, wireLen, entry)...)
	}
	ma := concatBytes(
		encodeField(maName, wireLen, toolName),
		argsMap,
		encodeField(maCallID, wireLen, callID),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(5)),
		encodeField(esmMCPArgs, wireLen, ma),
	)
	return agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex))
}

func collectAgentEvents(t *testing.T, frames ...[]byte) []ir.StreamEvent {
	t.Helper()
	return collectAgentEventsTools(t, nil, frames...)
}

func collectAgentEventsTools(t *testing.T, tools []ir.Tool, frames ...[]byte) []ir.StreamEvent {
	t.Helper()
	out, err := collectAgentEventsToolsErr(t, tools, frames...)
	if err != nil {
		t.Fatalf("DecodeAgentStream: %v", err)
	}
	return out
}

func collectAgentEventsToolsErr(t *testing.T, tools []ir.Tool, frames ...[]byte) ([]ir.StreamEvent, error) {
	t.Helper()
	var out []ir.StreamEvent
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join(frames, nil)), nil, func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	return out, err
}

func TestDecodeAgentStreamToolUpdateFinishesBeforeLaterFrame(t *testing.T) {
	var sawFinish bool
	err := DecodeAgentStreamTools([]ir.Tool{{Name: "read"}}, bytes.NewReader(bytes.Join([][]byte{
		mcpToolCallStartedFrame(t, "call-read", "read", map[string]any{"path": "prices.py"}),
		agentTextFrame(t, "this frame must not be required"),
	}, nil)), nil, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			sawFinish = true
			if ev.StopReason != ir.StopToolUse {
				t.Errorf("stop = %q, want tool_use", ev.StopReason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFinish {
		t.Fatal("decoder waited for a later frame after the tool call had arguments")
	}
}

func TestDecodeAgentStreamPartialToolDoesNotFinishBeforeArgs(t *testing.T) {
	var sawFinish bool
	err := DecodeAgentStreamTools([]ir.Tool{{Name: "read"}}, bytes.NewReader(bytes.Join([][]byte{
		mcpToolCallStartedFrame(t, "call-read", "read", nil),
		mcpPartialArgsFrame(t, "call-read", `{"path":"prices.py"}`),
		agentTextFrame(t, "not read"),
	}, nil)), nil, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			sawFinish = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFinish {
		t.Fatal("decoder did not finish after the argument fragment")
	}
}

func TestDecodeAgentStreamTextDeltas(t *testing.T) {
	events := collectAgentEvents(t,
		agentTextFrame(t, "Hello"),
		agentTextFrame(t, " world"),
		agentTurnEndedFrame(t, 12, 3),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	var text strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("text = %q", text.String())
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopEndTurn {
		t.Fatalf("last event = %+v, want end_turn finish", last)
	}
	if last.InputTokens != 12 || last.OutputTokens != 3 {
		t.Errorf("usage = %d/%d, want 12/3", last.InputTokens, last.OutputTokens)
	}
	if last.CacheReadTokens != 0 || last.CacheWriteTokens != 0 {
		t.Errorf("cache = %d/%d, want 0/0", last.CacheReadTokens, last.CacheWriteTokens)
	}
	if events[0].Kind != ir.EventMessageStart {
		t.Errorf("first event = %+v, want message start", events[0])
	}
}

func TestDecodeAgentStreamCacheTokensInclusive(t *testing.T) {
	events := collectAgentEvents(t,
		agentTextFrame(t, "ok"),
		agentTurnEndedCacheFrame(t, 10, 2, 4, 3),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish {
		t.Fatalf("last event = %+v, want finish", last)
	}
	if last.InputTokens != 10 || last.OutputTokens != 2 {
		t.Errorf("usage = %d/%d, want 10/2", last.InputTokens, last.OutputTokens)
	}
	if last.CacheReadTokens != 4 || last.CacheWriteTokens != 3 {
		t.Errorf("cache = %d/%d, want 4/3", last.CacheReadTokens, last.CacheWriteTokens)
	}
}

func TestDecodeAgentStreamCacheTokensClamped(t *testing.T) {
	events := collectAgentEvents(t,
		agentTextFrame(t, "ok"),
		agentTurnEndedCacheFrame(t, 10, 2, 12, 3),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish {
		t.Fatalf("last event = %+v, want finish", last)
	}
	if last.InputTokens != 10 || last.OutputTokens != 2 {
		t.Errorf("usage = %d/%d, want 10/2", last.InputTokens, last.OutputTokens)
	}
	if last.CacheReadTokens != 10 || last.CacheWriteTokens != 0 {
		t.Errorf("cache = %d/%d, want 10/0", last.CacheReadTokens, last.CacheWriteTokens)
	}
}

func TestDecodeAgentStreamKVReplies(t *testing.T) {
	var writes [][]byte
	var events []ir.StreamEvent
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		kvServerFrame(t, kvsGetBlobArgs),
		kvServerFrame(t, kvsSetBlobArgs),
		agentTextFrame(t, "ok"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 {
		t.Fatalf("client writes = %d, want 2", len(writes))
	}
	for i, w := range writes {
		flags, payload, err := readFrame(bytes.NewReader(w))
		if err != nil || flags != flagNone {
			t.Fatalf("write %d: readFrame err=%v flags=%d", i, err, flags)
		}
		cm, err := decodeMessage(payload)
		if err != nil {
			t.Fatal(err)
		}
		// AgentClientMessage{3: KvClientMessage{1: id, variant: result}}
		if _, ok := cm[3]; !ok {
			t.Fatalf("write %d: not a kv_client_message", i)
		}
		kv, _ := decodeMessage(cm[3][0].value)
		if id, _ := varintField(kv, kvcID); id != 7 {
			t.Errorf("write %d: kv id = %d, want 7", i, id)
		}
		wantVariant := kvcGetBlobRes
		if i == 1 {
			wantVariant = kvcSetBlobRes
		}
		if _, ok := kv[wantVariant]; !ok {
			t.Errorf("write %d: missing result variant %d", i, wantVariant)
		}
	}
	if len(events) == 0 || events[len(events)-1].Kind != ir.EventFinish {
		t.Error("stream did not finish after KV round-trips")
	}
}

func TestDecodeAgentStreamRequestContextReply(t *testing.T) {
	var writes [][]byte
	var events []ir.StreamEvent
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		execRequestContextFrame(t),
		agentTextFrame(t, "hi"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("client writes = %d, want 1", len(writes))
	}
	_, payload, err := readFrame(bytes.NewReader(writes[0]))
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := decodeMessage(payload)
	ecm, ok := cm[2] // exec_client_message
	if !ok {
		t.Fatal("reply is not an exec_client_message")
	}
	em, _ := decodeMessage(ecm[0].value)
	if id, _ := varintField(em, ecmID); id != 3 {
		t.Errorf("exec id = %d, want 3", id)
	}
	if eid, _ := stringField(em, ecmExecID); eid != "exec-1" {
		t.Errorf("exec_id = %q", eid)
	}
	if _, ok := em[ecmRequestContextRes]; !ok {
		t.Error("request_context_result missing")
	}
	if events[len(events)-1].Kind != ir.EventFinish {
		t.Error("stream did not finish")
	}
}

func TestDecodeAgentStreamMCPToolCall(t *testing.T) {
	events := collectAgentEvents(t, mcpExecFrame(t, "call-1", "get_weather", map[string]any{"city": "Tokyo"}))
	var start, delta *ir.StreamEvent
	var nDelta int
	for i, ev := range events {
		switch ev.Kind {
		case ir.EventToolCallStart:
			start = &events[i]
		case ir.EventToolCallDelta:
			nDelta++
			delta = &events[i]
		}
	}
	if start == nil {
		t.Fatal("no tool call start event")
	}
	if start.ToolID != "call-1" || start.ToolName != "get_weather" {
		t.Errorf("tool call = %s/%s", start.ToolID, start.ToolName)
	}
	if start.ArgsFrag != "" {
		t.Errorf("start ArgsFrag = %q, want empty (identity only)", start.ArgsFrag)
	}
	if nDelta != 1 || delta == nil {
		t.Fatalf("tool call deltas = %d, want exactly 1", nDelta)
	}
	if delta.ToolID != start.ToolID || delta.Index != start.Index {
		t.Errorf("delta identity = %s/%d, want %s/%d", delta.ToolID, delta.Index, start.ToolID, start.Index)
	}
	var args json.RawMessage
	if err := json.Unmarshal([]byte(delta.ArgsFrag), &args); err != nil {
		t.Fatalf("delta args not JSON: %q (%v)", delta.ArgsFrag, err)
	}
	if string(args) != `{"city":"Tokyo"}` {
		t.Errorf("args = %s", args)
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v, want tool_use", last)
	}
	// Ingress encoders ignore Start.ArgsFrag; concatenated Deltas are what
	// the client would assemble as function.arguments.
	if assembled := openaiAssembledArgs(events); assembled != `{"city":"Tokyo"}` {
		t.Errorf("assembled openai args = %q", assembled)
	}
}

// TestDecodeAgentStreamMCPExecAcksBeforeFinish writes an empty McpSuccess
// before the client turn ends. AgentService heartbeats until that ack, and
// does not send turn_ended while the exec is open.
func TestDecodeAgentStreamMCPExecAcksBeforeFinish(t *testing.T) {
	var writes [][]byte
	var events []ir.StreamEvent
	err := DecodeAgentStream(bytes.NewReader(mcpExecFrame(t, "call-1", "read", map[string]any{"path": "prices.py"})), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("acks = %d, want 1", len(writes))
	}
	_, payload, err := readFrame(bytes.NewReader(writes[0]))
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := decodeMessage(payload)
	ecm, ok := cm[2]
	if !ok {
		t.Fatal("ack is not an exec_client_message")
	}
	em, _ := decodeMessage(ecm[0].value)
	if id, _ := varintField(em, ecmID); id != 5 {
		t.Errorf("exec id = %d, want 5", id)
	}
	res, ok := em[ecmMCPResult]
	if !ok || len(res) == 0 {
		t.Fatal("mcp_result missing")
	}
	mr, _ := decodeMessage(res[0].value)
	if _, ok := mr[mcpResultSuccess]; !ok {
		t.Fatal("mcp success missing")
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v, want tool_use", last)
	}
}

// TestDecodeAgentStreamMCPExecReturnsBeforeTurnEnded does not wait for
// usage after a client tool call. An empty ack does not make Cursor emit
// turn_ended, and reading until it arrives holds the chat open.
func TestDecodeAgentStreamMCPExecReturnsBeforeTurnEnded(t *testing.T) {
	var sawFinish bool
	err := DecodeAgentStream(bytes.NewReader(mcpExecFrame(t, "call-1", "read", map[string]any{"path": "prices.py"})), func([]byte) error {
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			sawFinish = true
			if ev.StopReason != ir.StopToolUse {
				t.Errorf("stop = %q, want tool_use", ev.StopReason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFinish {
		t.Fatal("decoder waited for more frames after the tool call")
	}
}

// TestDecodeAgentStreamRejectsUnmatchedGrepThenContinues covers the live
// stall: Cursor asks for files_with_matches after status text. The client
// has bash, not grep. Rejecting the search must not end the turn, so a
// later declared tool can still be delivered.
func TestDecodeAgentStreamRejectsUnmatchedGrepThenContinues(t *testing.T) {
	var writes [][]byte
	events := collectAgentEventsTools(t, []ir.Tool{{Name: "bash"}, {Name: "read"}},
		agentTextFrame(t, "I'll search the remaining sources."),
		grepExecFrame(t, "call-grep", "/tmp", "**/xai*"),
		mcpExecFrame(t, "call-bash", "bash", map[string]any{"command": "rg xai"}),
	)
	// The collect helper drops writes. Re-run with a writer for the ack.
	err := DecodeAgentStreamTools([]ir.Tool{{Name: "bash"}}, bytes.NewReader(bytes.Join([][]byte{
		grepExecFrame(t, "call-grep", "/tmp", "**/xai*"),
		agentTurnEndedFrame(t, 10, 2),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("rejects = %d, want 1", len(writes))
	}
	_, payload, err := readFrame(bytes.NewReader(writes[0]))
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := decodeMessage(payload)
	ecm, ok := cm[2]
	if !ok {
		t.Fatal("reject is not an exec_client_message")
	}
	em, _ := decodeMessage(ecm[0].value)
	if id, _ := varintField(em, ecmID); id != 9 {
		t.Fatalf("exec client id = %d, want 9", id)
	}
	if eid, _ := stringField(em, ecmExecID); eid != "exec-grep" {
		t.Fatalf("exec_id = %q, want exec-grep", eid)
	}
	res, ok := em[5]
	if !ok || len(res) == 0 {
		t.Fatal("grep result field missing")
	}
	gr, _ := decodeMessage(res[0].value)
	// GrepResult.error is field 2. GrepError.text is field 1. Field 2 of the
	// error message is not a reason.
	errVar, ok := gr[2]
	if !ok || len(errVar) == 0 {
		t.Fatal("grep error variant missing")
	}
	errMsg, _ := decodeMessage(errVar[0].value)
	if msg, ok := stringField(errMsg, 1); !ok || msg == "" {
		t.Fatal("grep error text missing")
	}
	if _, ok := errMsg[2]; ok {
		t.Fatal("grep error invented a reason field")
	}
	var names []string
	var stop ir.StopReason
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			names = append(names, ev.ToolName)
		}
		if ev.Kind == ir.EventFinish {
			stop = ev.StopReason
		}
	}
	if len(names) != 1 || names[0] != "bash" {
		t.Fatalf("tools = %v, want [bash]", names)
	}
	if stop != ir.StopToolUse {
		t.Fatalf("stop = %q, want tool_use", stop)
	}
}

func grepExecFrame(t *testing.T, callID, path, pattern string) []byte {
	t.Helper()
	args := concatBytes(
		encodeField(1, wireLen, pattern),
		encodeField(2, wireLen, path),
		encodeField(3, wireLen, callID),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(9)),
		encodeField(esmExecID, wireLen, "exec-grep"),
		encodeField(5, wireLen, args),
	)
	return agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex))
}

// TestDecodeAgentStreamMCPToolCallEmptyThenDeltas covers incremental args:
// tool_call_started with an empty McpArgs map ("{}") must not emit a "{}"
// Delta, or later ptcArgsDelta fragments would concatenate to invalid JSON.
func TestDecodeAgentStreamMCPToolCallEmptyThenDeltas(t *testing.T) {
	events := collectAgentEvents(t,
		mcpToolCallStartedFrame(t, "call-2", "websearch", nil),
		mcpPartialArgsFrame(t, "call-2", `{"query":`),
		mcpPartialArgsFrame(t, "call-2", `"SPUS"}`),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	var start *ir.StreamEvent
	var frags []string
	for i, ev := range events {
		switch ev.Kind {
		case ir.EventToolCallStart:
			start = &events[i]
		case ir.EventToolCallDelta:
			frags = append(frags, ev.ArgsFrag)
		}
	}
	if start == nil {
		t.Fatal("no tool call start event")
	}
	if start.ToolName != "websearch" || start.ToolID != "call-2" {
		t.Errorf("tool call = %s/%s", start.ToolID, start.ToolName)
	}
	if start.ArgsFrag != "" {
		t.Errorf("start ArgsFrag = %q, want empty", start.ArgsFrag)
	}
	for _, f := range frags {
		if f == "{}" {
			t.Fatalf("emitted empty-object delta %q among %v", f, frags)
		}
	}
	got := strings.Join(frags, "")
	var args json.RawMessage
	if err := json.Unmarshal([]byte(got), &args); err != nil {
		t.Fatalf("concatenated deltas not JSON: %q (%v)", got, err)
	}
	if string(args) != `{"query":"SPUS"}` {
		t.Errorf("args = %s from frags %v", args, frags)
	}
}

func TestDecodeAgentStreamEmptyStartThenEmptyExec(t *testing.T) {
	events := collectAgentEvents(t,
		mcpToolCallStartedFrame(t, "call-empty", "noop", nil),
		mcpExecFrame(t, "call-empty", "noop", nil),
	)
	got := openaiAssembledArgs(events)
	if got != "{}" {
		t.Fatalf("args = %q, want one authoritative {}", got)
	}
	var deltas int
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta {
			deltas++
			if ev.ArgsFrag != "{}" {
				t.Fatalf("delta = %q", ev.ArgsFrag)
			}
		}
	}
	if deltas != 1 {
		t.Fatalf("deltas = %d, want 1", deltas)
	}
}

func TestDecodeAgentStreamPartialThenFullSnapshot(t *testing.T) {
	events := collectAgentEvents(t,
		mcpToolCallStartedFrame(t, "call-snap", "read", nil),
		mcpPartialArgsFrame(t, "call-snap", `{"path":`),
		mcpExecFrame(t, "call-snap", "read", map[string]any{"path": "prices.py"}),
	)
	got := openaiAssembledArgs(events)
	if got != `{"path":"prices.py"}` {
		t.Fatalf("args = %q, want snapshot without provisional fragments", got)
	}
	if strings.Contains(got, `{"path":{`) || strings.Count(got, "{") != 1 {
		t.Fatalf("args concatenated placeholder and snapshot: %q", got)
	}
}

func TestDecodeAgentStreamRepeatedExecDoesNotDuplicateArgs(t *testing.T) {
	first := mcpExecFrame(t, "call-1", "get_weather", map[string]any{"city": "Tokyo"})
	second := mcpExecFrame(t, "call-1", "get_weather", map[string]any{"city": "Osaka"})
	events := collectAgentEvents(t, first, second)
	if got := openaiAssembledArgs(events); got != `{"city":"Tokyo"}` {
		t.Fatalf("args = %q, want the first authoritative snapshot once", got)
	}
}

func TestDecodeAgentStreamDuplicateExecIDDoesNotRejectMCP(t *testing.T) {
	// A complete tool_call_started ends the turn before a later exec. The
	// duplicate-id case is the still-open exec snapshot repeated on the same
	// stream. The second copy must not become a built-in rejection, and the
	// first authoritative args must not be concatenated.
	var writes int
	events := []ir.StreamEvent{}
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		mcpExecFrame(t, "call-1", "get_weather", map[string]any{"city": "Tokyo"}),
		mcpExecFrame(t, "call-1", "get_weather", map[string]any{"city": "Osaka"}),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("writes = %d, want one MCP ack and no built-in rejection", writes)
	}
	if got := openaiAssembledArgs(events); got != `{"city":"Tokyo"}` {
		t.Fatalf("args = %q", got)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart && ev.ToolName != "get_weather" {
			t.Fatalf("surfaced %s", ev.ToolName)
		}
	}
}

func TestDecodeAgentStreamIncompleteArgsFail(t *testing.T) {
	_, err := collectAgentEventsToolsErr(t, nil,
		mcpToolCallStartedFrame(t, "call-bad", "read", nil),
		mcpPartialArgsFrame(t, "call-bad", `{"path":`),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	if err == nil {
		t.Fatal("incomplete arguments finished as success")
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("err = %v, want StreamFailure", err)
	}
}

func TestDecodeAgentStreamZeroArgsIngressEncoders(t *testing.T) {
	events := collectAgentEvents(t, mcpExecFrame(t, "call-zero", "noop", nil))
	if got := openaiAssembledArgs(events); got != "{}" {
		t.Fatalf("cursor args = %q", got)
	}
	encodeAll := func(encode func(ir.StreamEvent, *sse.Writer) error) string {
		t.Helper()
		rec := httptest.NewRecorder()
		w, ok := sse.NewWriter(rec)
		if !ok {
			t.Fatal("recorder does not flush")
		}
		for _, ev := range events {
			if err := encode(ev, w); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Body.String()
	}
	check := func(name, body string) {
		t.Helper()
		if !strings.Contains(body, "{}") {
			t.Fatalf("%s encoded args missing {}: %s", name, body)
		}
		if strings.Contains(body, "{}{}") {
			t.Fatalf("%s duplicated empty args: %s", name, body)
		}
	}
	check("openai", encodeAll(openai.NewStreamEncoder("noop").Encode))
	check("anthropic", encodeAll(anthropic.NewStreamEncoder("noop").Encode))
	check("responses", encodeAll(responses.NewStreamEncoder("noop").Encode))
}

// openaiAssembledArgs concatenates EventToolCallDelta fragments the way
// OpenAI/Anthropic/Responses encoders and the unary collector do.
func openaiAssembledArgs(events []ir.StreamEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallDelta {
			b.WriteString(ev.ArgsFrag)
		}
	}
	return b.String()
}

// mcpToolCallStartedFrame builds InteractionUpdate.tool_call_started with an
// McpToolCall. A nil args map produces the empty McpArgs that mcpArgsMapJSON
// renders as "{}".
func mcpToolCallStartedFrame(t *testing.T, callID, toolName string, args map[string]any) []byte {
	t.Helper()
	var argsMap []byte
	for k, v := range args {
		val := encodeField(3, wireLen, []byte(v.(string)))
		entry := concatBytes(encodeField(1, wireLen, k), encodeField(2, wireLen, val))
		argsMap = append(argsMap, encodeField(maArgs, wireLen, entry)...)
	}
	ma := concatBytes(
		encodeField(maName, wireLen, toolName),
		argsMap,
		encodeField(maCallID, wireLen, callID),
		encodeField(maToolName, wireLen, toolName),
	)
	mtc := encodeField(mtcArgs, wireLen, ma)
	tc := encodeField(tcMCPTOolCall, wireLen, mtc)
	inner := concatBytes(
		encodeField(tcsCallID, wireLen, callID),
		encodeField(tcsToolCall, wireLen, tc),
	)
	return interactionUpdateFrame(t, iuToolCallStarted, inner)
}

// mcpPartialArgsFrame builds InteractionUpdate.partial_tool_call with an
// args_text_delta fragment for an already-started call.
func mcpPartialArgsFrame(t *testing.T, callID, delta string) []byte {
	t.Helper()
	inner := concatBytes(
		encodeField(ptcCallID, wireLen, callID),
		encodeField(ptcArgsDelta, wireLen, delta),
	)
	return interactionUpdateFrame(t, iuPartialToolCall, inner)
}

func TestDecodeAgentStreamShellExecDoesNotEmitToolUse(t *testing.T) {
	args := concatBytes(
		encodeField(1, wireLen, "ls -la"),
		encodeField(4, wireLen, "call-sh"),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(5)),
		encodeField(esmExecID, wireLen, "exec-sh"),
		encodeField(2, wireLen, args), // shell_args
	)
	var writes [][]byte
	var events []ir.StreamEvent
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTextFrame(t, "still here"),
		agentTurnEndedFrame(t, 4, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("rejects = %d, want 1", len(writes))
	}
	_, payload, err := readFrame(bytes.NewReader(writes[0]))
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := decodeMessage(payload)
	ecm, ok := cm[2]
	if !ok {
		t.Fatal("reject is not an exec_client_message")
	}
	em, _ := decodeMessage(ecm[0].value)
	if _, ok := em[2]; !ok {
		t.Fatal("shell result field missing")
	}
	var text strings.Builder
	var stop ir.StopReason
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("shell exec emitted tool_use %s", ev.ToolName)
		}
		if ev.Kind == ir.EventTextDelta {
			text.WriteString(ev.Text)
		}
		if ev.Kind == ir.EventFinish {
			stop = ev.StopReason
		}
	}
	if text.String() != "still here" {
		t.Errorf("text = %q, want later text after rejected shell", text.String())
	}
	if stop != ir.StopEndTurn {
		t.Errorf("stop = %q, want end_turn", stop)
	}
}

func TestDecodeAgentStreamBuiltinResultShapes(t *testing.T) {
	reason := "not available; use the declared MCP tools"
	pathArgs := func(path string) []byte {
		return concatBytes(encodeField(1, wireLen, path), encodeField(2, wireLen, "call"))
	}
	commandArgs := func(command string) []byte {
		return concatBytes(encodeField(1, wireLen, command), encodeField(4, wireLen, "call"))
	}
	cases := []struct {
		name      string
		argField  int
		args      []byte
		result    int
		check     func(t *testing.T, body []byte)
		wantThrow bool
	}{
		{name: "shell", argField: 2, result: 2, args: commandArgs("ls -la"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 4, 1, 3, "ls -la", reason, wireLen)
		}},
		{name: "write", argField: 3, result: 3, args: pathArgs("/tmp/out.txt"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 6, 1, 2, "/tmp/out.txt", reason, wireLen)
		}},
		{name: "delete", argField: 4, result: 4, args: pathArgs("/tmp/old.txt"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 6, 1, 2, "/tmp/old.txt", reason, wireLen)
		}},
		{name: "grep", argField: 5, result: 5, args: concatBytes(encodeField(1, wireLen, "xai"), encodeField(2, wireLen, "/tmp")), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "read", argField: 7, result: 7, args: pathArgs("/etc/hostname"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 3, 1, 2, "/etc/hostname", reason, wireLen)
		}},
		{name: "ls", argField: 8, result: 8, args: pathArgs("/tmp"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 3, 1, 2, "/tmp", reason, wireLen)
		}},
		{name: "diagnostics", argField: 9, result: 9, args: pathArgs("/tmp/main.go"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 3, 1, 2, "/tmp/main.go", reason, wireLen)
		}},
		{name: "shell_stream", argField: 14, result: 14, args: commandArgs("ls -la"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 5, 1, 3, "ls -la", reason, wireLen)
		}},
		{name: "background_shell", argField: 16, result: 16, args: commandArgs("sleep 1"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 3, 1, 3, "sleep 1", reason, wireLen)
		}},
		{name: "list_mcp_resources", argField: 17, result: 17, args: encodeField(1, wireLen, "srv"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 3, 1, reason)
		}},
		{name: "read_mcp_resource", argField: 18, result: 18, args: concatBytes(
			encodeField(1, wireLen, "server-a"),
			encodeField(2, wireLen, "file://resource"),
			encodeField(3, wireLen, "/tmp/download"),
		), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 3, 1, 2, "file://resource", reason, wireLen)
		}},
		{name: "fetch", argField: 20, result: 20, args: encodeField(1, wireLen, "https://example.test"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 2, 1, 2, "https://example.test", reason, wireLen)
		}},
		{name: "record_screen", argField: 21, result: 21, args: encodeField(2, wireLen, "call-rec"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 4, 1, reason)
		}},
		{name: "computer_use", argField: 22, result: 22, args: encodeField(1, wireLen, "call-cu"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "write_shell_stdin", argField: 23, result: 23, args: encodeField(1, wireVarint, uint64(9)), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "execute_hook", argField: 27, args: encodeField(1, wireLen, []byte{}), wantThrow: true},
		{name: "subagent", argField: 28, result: 28, args: encodeField(1, wireLen, "call-sub"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 2, reason)
		}},
		{name: "redacted_read", argField: 29, result: 29, args: pathArgs("/secret"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 3, 1, 2, "/secret", reason, wireLen)
		}},
		{name: "force_background_shell", argField: 30, result: 30, args: encodeField(1, wireLen, "call-fbs"), check: func(t *testing.T, body []byte) {
			assertEnum(t, body, 1, 2)
		}},
		{name: "force_background_subagent", argField: 31, result: 31, args: encodeField(1, wireLen, "call-fba"), check: func(t *testing.T, body []byte) {
			assertEnum(t, body, 1, 2)
		}},
		{name: "mcp_state", argField: 36, result: 36, args: []byte{}, check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 3, 1, reason)
		}},
		{name: "subagent_await", argField: 37, result: 37, args: encodeField(1, wireLen, "agent-1"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 4, 2, reason)
		}},
		{name: "smart_mode", argField: 38, result: 38, args: encodeField(1, wireLen, "call-sm"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "canvas", argField: 40, result: 40, args: pathArgs("/tmp/canvas.html"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 2, 1, 2, "/tmp/canvas.html", reason, wireLen)
		}},
		{name: "shell_allowlist", argField: 41, result: 41, args: commandArgs("ls"), check: func(t *testing.T, body []byte) {
			assertEnum(t, body, 1, 0)
		}},
		{name: "mcp_allowlist", argField: 42, result: 42, args: encodeField(2, wireLen, "tool"), check: func(t *testing.T, body []byte) {
			assertEnum(t, body, 1, 0)
		}},
		{name: "web_fetch_allowlist", argField: 43, result: 43, args: encodeField(1, wireLen, "https://example.test"), check: func(t *testing.T, body []byte) {
			assertEnum(t, body, 1, 0)
		}},
		{name: "git_diff", argField: 44, args: encodeField(1, wireLen, "HEAD"), wantThrow: true},
		{name: "pi_read", argField: 45, result: 46, args: pathArgs("/tmp/a"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "pi_bash", argField: 46, result: 47, args: commandArgs("uname -a"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "pi_edit", argField: 47, result: 48, args: pathArgs("/tmp/a"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 3, 1, reason)
		}},
		{name: "pi_write", argField: 48, result: 49, args: pathArgs("/tmp/a"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 3, 1, reason)
		}},
		{name: "pi_grep", argField: 49, result: 50, args: encodeField(1, wireLen, "x"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "pi_find", argField: 50, result: 51, args: encodeField(1, wireLen, "x"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "pi_ls", argField: 51, result: 52, args: pathArgs("/tmp"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "conversation_search", argField: 53, result: 53, args: encodeField(1, wireLen, "query"), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "agent_store_conflict", argField: 54, result: 54, args: encodeField(2, wireVarint, uint64(1)), check: func(t *testing.T, body []byte) {
			assertReasonOnly(t, body, 2, 1, reason)
		}},
		{name: "mini_swe", argField: 52, result: 55, args: commandArgs("pwd"), check: func(t *testing.T, body []byte) {
			assertNested(t, body, 4, 1, 3, "pwd", reason, wireLen)
		}},
		{name: "adopt", argField: 56, result: 56, args: encodeField(1, wireLen, "agent-9"), check: func(t *testing.T, body []byte) {
			m, err := decodeMessage(body)
			if err != nil {
				t.Fatal(err)
			}
			fs := m[5]
			if len(fs) != 1 || fs[0].wireType != wireLen || string(fs[0].value) != reason {
				t.Fatalf("adopt error = %+v", fs)
			}
			if _, ok := m[4]; ok {
				t.Fatal("adopt rejection invented success")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := concatBytes(
				encodeField(esmID, wireVarint, uint64(21)),
				encodeField(esmExecID, wireLen, "exec-"+tc.name),
				encodeField(esmMachineID, wireLen, "machine-1"),
				encodeField(esmSpanContext, wireLen, []byte{0x01}),
				encodeField(esmAcceptHookAdditionalContext, wireVarint, uint64(1)),
				encodeField(tc.argField, wireLen, tc.args),
			)
			var writes [][]byte
			var events []ir.StreamEvent
			err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
				agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
				agentTextFrame(t, "after "+tc.name),
				mcpExecFrame(t, "call-mcp", "bash", map[string]any{"command": "true"}),
			}, nil)), func(frame []byte) error {
				writes = append(writes, frame)
				return nil
			}, func(ev ir.StreamEvent) error {
				events = append(events, ev)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(writes) != 2 {
				t.Fatalf("writes = %d, want rejection plus MCP ack", len(writes))
			}
			_, payload, err := readFrame(bytes.NewReader(writes[0]))
			if err != nil {
				t.Fatal(err)
			}
			cm, _ := decodeMessage(payload)
			if tc.wantThrow {
				assertExecThrow(t, cm, 21, reason)
			} else {
				if _, ok := cm[acmExecClientControl]; ok {
					t.Fatal("recognized exec used throw")
				}
				em := mustExecClient(t, cm)
				if id, _ := varintField(em, ecmID); id != 21 {
					t.Fatalf("id = %d", id)
				}
				if eid, _ := stringField(em, ecmExecID); eid != "exec-"+tc.name {
					t.Fatalf("exec_id = %q", eid)
				}
				if _, ok := em[45]; ok {
					t.Fatal("client field 45 must stay hook context, not a result")
				}
				body := mustField(t, em, tc.result)
				tc.check(t, body)
			}
			var names []string
			var text strings.Builder
			for _, ev := range events {
				if ev.Kind == ir.EventToolCallStart {
					names = append(names, ev.ToolName)
				}
				if ev.Kind == ir.EventTextDelta {
					text.WriteString(ev.Text)
				}
			}
			if text.String() != "after "+tc.name {
				t.Fatalf("text = %q", text.String())
			}
			if len(names) != 1 || names[0] != "bash" {
				t.Fatalf("tools = %v, want later MCP bash only", names)
			}
		})
	}
}

func mustField(t *testing.T, m map[int][]field, num int) []byte {
	t.Helper()
	fs, ok := m[num]
	if !ok || len(fs) == 0 {
		t.Fatalf("field %d missing", num)
	}
	return fs[0].value
}

func mustExecClient(t *testing.T, cm map[int][]field) map[int][]field {
	t.Helper()
	fs, ok := cm[acmExecClientMessage]
	if !ok || len(fs) == 0 {
		t.Fatal("exec_client_message missing")
	}
	em, err := decodeMessage(fs[0].value)
	if err != nil {
		t.Fatal(err)
	}
	return em
}

func assertExecThrow(t *testing.T, cm map[int][]field, id uint64, reason string) {
	t.Helper()
	if _, ok := cm[acmExecClientMessage]; ok {
		t.Fatal("throw path wrote an exec result")
	}
	fs, ok := cm[acmExecClientControl]
	if !ok || len(fs) == 0 {
		t.Fatal("exec_client_control_message missing")
	}
	control, _ := decodeMessage(fs[0].value)
	throwBody := mustField(t, control, eccThrow)
	throwMsg, _ := decodeMessage(throwBody)
	if got, _ := varintField(throwMsg, ectID); got != id {
		t.Fatalf("throw id = %d, want %d", got, id)
	}
	if got, _ := stringField(throwMsg, ectError); got != reason {
		t.Fatalf("throw error = %q", got)
	}
}

func assertNested(t *testing.T, body []byte, outer, identityField, reasonField int, identity, reason string, identityWire int) {
	t.Helper()
	m, err := decodeMessage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("result fields = %v, want only %d", fieldNums(m), outer)
	}
	fs := m[outer]
	if len(fs) != 1 || fs[0].wireType != wireLen {
		t.Fatalf("outer %d = %+v, want one length-delimited field", outer, fs)
	}
	inner, err := decodeMessage(fs[0].value)
	if err != nil {
		t.Fatal(err)
	}
	id := inner[identityField]
	if len(id) != 1 || id[0].wireType != identityWire || string(id[0].value) != identity {
		t.Fatalf("identity field %d = %+v, want %q", identityField, id, identity)
	}
	rs := inner[reasonField]
	if len(rs) != 1 || rs[0].wireType != wireLen || string(rs[0].value) != reason {
		t.Fatalf("reason field %d = %+v, want %q", reasonField, rs, reason)
	}
}

func assertReasonOnly(t *testing.T, body []byte, outer, reasonField int, reason string) {
	t.Helper()
	m, err := decodeMessage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("result fields = %v, want only %d", fieldNums(m), outer)
	}
	fs := m[outer]
	if len(fs) != 1 || fs[0].wireType != wireLen {
		t.Fatalf("outer %d wire = %v", outer, fs)
	}
	inner, err := decodeMessage(fs[0].value)
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) != 1 {
		t.Fatalf("inner fields = %v, want only reason %d", fieldNums(inner), reasonField)
	}
	rs := inner[reasonField]
	if len(rs) != 1 || rs[0].wireType != wireLen || string(rs[0].value) != reason {
		t.Fatalf("reason = %+v", rs)
	}
}

func assertEnum(t *testing.T, body []byte, fieldNum int, want uint64) {
	t.Helper()
	m, err := decodeMessage(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("result fields = %v, want only %d", fieldNums(m), fieldNum)
	}
	fs := m[fieldNum]
	if len(fs) != 1 || fs[0].wireType != wireVarint {
		t.Fatalf("field %d = %+v, want varint", fieldNum, fs)
	}
	got, ok := varintField(m, fieldNum)
	if !ok || got != want {
		t.Fatalf("enum = %d ok=%v, want %d", got, ok, want)
	}
}

func TestDecodeAgentStreamKnownExecWinsOverUnknownFields(t *testing.T) {
	args := concatBytes(encodeField(1, wireLen, "ls -la"), encodeField(4, wireLen, "call-sh"))
	for i := 0; i < 20; i++ {
		ex := concatBytes(
			encodeField(esmID, wireVarint, uint64(21)),
			encodeField(esmExecID, wireLen, "exec-known"),
			encodeField(99, wireLen, encodeField(1, wireLen, "future")),
			encodeField(80, wireLen, encodeField(1, wireLen, "other")),
			encodeField(2, wireLen, args),
		)
		var writes [][]byte
		err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
			agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
			agentTextFrame(t, "after"),
			agentTurnEndedFrame(t, 1, 1),
			connectEndStreamFrame(t, []byte(`{}`)),
		}, nil)), func(frame []byte) error {
			writes = append(writes, frame)
			return nil
		}, func(ev ir.StreamEvent) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if len(writes) != 1 {
			t.Fatalf("writes = %d", len(writes))
		}
		_, payload, err := readFrame(bytes.NewReader(writes[0]))
		if err != nil {
			t.Fatal(err)
		}
		cm, err := decodeMessage(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cm[acmExecClientControl]; ok {
			t.Fatal("known shell was replaced by unknown-field throw")
		}
		em := mustExecClient(t, cm)
		body := mustField(t, em, 2)
		assertNested(t, body, 4, 1, 3, "ls -la", "not available; use the declared MCP tools", wireLen)
	}
}

func TestDecodeAgentStreamControlOnlyDoesNotReject(t *testing.T) {
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(4)),
		encodeField(esmExecID, wireLen, "exec-meta"),
		encodeField(esmSpanContext, wireLen, []byte{0x01}),
		encodeField(esmMachineID, wireLen, "machine"),
		encodeField(esmAcceptHookAdditionalContext, wireVarint, uint64(1)),
	)
	var writes int
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTextFrame(t, "kept"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatal("metadata exec surfaced a tool")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("writes = %d, want no rejection", writes)
	}
}

func TestDecodeAgentStreamUnknownOnlyThrowsDeterministically(t *testing.T) {
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(11)),
		encodeField(90, wireLen, encodeField(1, wireLen, "a")),
		encodeField(70, wireLen, encodeField(1, wireLen, "b")),
	)
	var writes [][]byte
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTextFrame(t, "continued"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %d", len(writes))
	}
	_, payload, err := readFrame(bytes.NewReader(writes[0]))
	if err != nil {
		t.Fatal(err)
	}
	cm, err := decodeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	assertExecThrow(t, cm, 11, "not available; use the declared MCP tools")
	if _, ok := cm[70]; ok || len(cm) != 1 {
		t.Fatalf("throw invented a result field: %v", fieldNums(cm))
	}
}

func TestDecodeAgentStreamUnknownExecOneofRejected(t *testing.T) {
	// Field 99 is not a known control field. It is still a built-in exec:
	// reject it and do not invent exec_99.
	args := encodeField(1, wireLen, "payload")
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(11)),
		encodeField(esmExecID, wireLen, "exec-99"),
		encodeField(99, wireLen, args),
	)
	var writes [][]byte
	events := []ir.StreamEvent{}
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTextFrame(t, "continued"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func(frame []byte) error {
		writes = append(writes, frame)
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("rejects = %d, want 1", len(writes))
	}
	_, payload, err := readFrame(bytes.NewReader(writes[0]))
	if err != nil {
		t.Fatal(err)
	}
	cm, _ := decodeMessage(payload)
	assertExecThrow(t, cm, 11, "not available; use the declared MCP tools")
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("unknown exec surfaced as %s", ev.ToolName)
		}
	}
	var text strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "continued" {
		t.Errorf("text = %q, want continued", text.String())
	}
}

func TestDecodeAgentStreamBuiltinUpdateDoesNotSurface(t *testing.T) {
	var writes int
	var sawTextAfter bool
	var finished bool
	err := DecodeAgentStreamTools([]ir.Tool{{Name: "bash"}, {Name: "read"}}, bytes.NewReader(bytes.Join([][]byte{
		agentTextFrame(t, "Checking what is left to build."),
		builtinToolUpdateFrame(t, 5, "call-grep", "/tmp", "**/xai*"),
		agentTextFrame(t, "continued after builtin"),
		agentTurnEndedFrame(t, 2, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("builtin tool update emitted %s", ev.ToolName)
		}
		if ev.Kind == ir.EventTextDelta && ev.Text == "continued after builtin" {
			sawTextAfter = true
		}
		if ev.Kind == ir.EventFinish {
			finished = true
			if ev.StopReason != ir.StopEndTurn {
				t.Errorf("stop = %q, want end_turn", ev.StopReason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("rejects = %d, want 0 (tool updates have no exec envelope)", writes)
	}
	if !finished {
		t.Fatal("builtin tool update stalled the stream")
	}
	if !sawTextAfter {
		t.Fatal("decoder stopped before text after the builtin tool update")
	}
}

func TestDecodeAgentStreamBuiltinPartialUpdateDoesNotSurface(t *testing.T) {
	var writes int
	var sawTextAfter bool
	var finished bool
	err := DecodeAgentStreamTools([]ir.Tool{{Name: "bash"}}, bytes.NewReader(bytes.Join([][]byte{
		agentTextFrame(t, "Checking what is left to build."),
		builtinPartialToolUpdateFrame(t, "call-grep", "/tmp", "**/xai*"),
		agentTextFrame(t, "continued after partial"),
		agentTurnEndedFrame(t, 2, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("builtin partial update emitted %s", ev.ToolName)
		}
		if ev.Kind == ir.EventTextDelta && ev.Text == "continued after partial" {
			sawTextAfter = true
		}
		if ev.Kind == ir.EventFinish {
			finished = true
			if ev.StopReason != ir.StopEndTurn {
				t.Errorf("stop = %q, want end_turn", ev.StopReason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("rejects = %d, want 0", writes)
	}
	if !finished {
		t.Fatal("builtin partial update stalled the stream")
	}
	if !sawTextAfter {
		t.Fatal("decoder stopped before text after the builtin partial update")
	}
}

func builtinToolUpdateFrame(t *testing.T, resultField int, callID, path, pattern string) []byte {
	t.Helper()
	args := concatBytes(
		encodeField(1, wireLen, pattern),
		encodeField(2, wireLen, path),
		encodeField(3, wireLen, callID),
	)
	var tc []byte
	if resultField != 0 {
		tc = append(tc, encodeField(resultField, wireLen, args)...)
	}
	inner := concatBytes(
		encodeField(tcsCallID, wireLen, callID),
		encodeField(tcsToolCall, wireLen, tc),
	)
	return interactionUpdateFrame(t, iuToolCallStarted, inner)
}

func builtinPartialToolUpdateFrame(t *testing.T, callID, path, pattern string) []byte {
	t.Helper()
	args := concatBytes(
		encodeField(1, wireLen, pattern),
		encodeField(2, wireLen, path),
		encodeField(3, wireLen, callID),
	)
	inner := concatBytes(
		encodeField(ptcCallID, wireLen, callID),
		encodeField(ptcToolCall, wireLen, encodeField(5, wireLen, args)),
	)
	return interactionUpdateFrame(t, iuPartialToolCall, inner)
}

func TestDecodeAgentStreamReadToolCallStartedDoesNotSurface(t *testing.T) {
	readArgs := concatBytes(
		encodeField(1, wireLen, "/etc/hostname"),
		encodeField(2, wireLen, "call-rd"),
	)
	tc := encodeField(8, wireLen, // read_tool_call
		encodeField(1, wireLen, readArgs))
	inner := concatBytes(
		encodeField(tcsCallID, wireLen, "call-rd"),
		encodeField(tcsToolCall, wireLen, tc),
	)
	events := collectAgentEvents(t,
		interactionUpdateFrame(t, iuToolCallStarted, inner),
		agentTextFrame(t, "after read"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	var text strings.Builder
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("read tool_call_started emitted %s", ev.ToolName)
		}
		if ev.Kind == ir.EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "after read" {
		t.Errorf("text = %q, want after read", text.String())
	}
}

func TestDecodeAgentStreamErrorFrameBeforeContent(t *testing.T) {
	errFrame := wrapConnectFrame([]byte(`{"error":{"code":"resource_exhausted","message":"Error","details":[{"debug":{"details":{"title":"Named models unavailable","detail":"Free plans can only use Auto."}}}]}}`), false)
	err := DecodeAgentStream(bytes.NewReader(errFrame), nil, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Named models unavailable") {
		t.Errorf("error = %v, want detail title", err)
	}
}

// TestDecodeAgentStreamTruncatedHeader verifies that a stream cut mid-frame
// header is reported as an error instead of ending cleanly; masking it as
// io.EOF would fabricate a successful finish over partial content.
func TestDecodeAgentStreamTruncatedHeader(t *testing.T) {
	// One valid text-bearing frame, then 3 bytes of a next 5-byte header.
	good := wrapConnectFrame(encodeField(asmInteractionUpdate, wireLen,
		encodeField(iuTextDelta, wireLen,
			encodeField(tdText, wireLen, []byte("Hello world")))), false)

	var sawFinish bool
	err := DecodeAgentStreamTools(nil, bytes.NewReader(append(append([]byte{}, good...), 1, 2, 3)), nil, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			sawFinish = true
		}
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("DecodeAgentStreamTools: got %v, want wrapped io.ErrUnexpectedEOF for truncated header", err)
	}
	if sawFinish {
		t.Error("DecodeAgentStreamTools fabricated a Finish event on a truncated stream")
	}
}

func TestDecodeAgentStreamOversizedFrame(t *testing.T) {
	valid := agentTextFrame(t, "visible")
	for _, tc := range []struct {
		name  string
		input []byte
	}{
		{name: "before output", input: oversizedConnectHeader()},
		{name: "after text", input: append(append([]byte{}, valid...), oversizedConnectHeader()...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []ir.StreamEvent
			err := DecodeAgentStreamTools(nil, bytes.NewReader(tc.input), nil, func(ev ir.StreamEvent) error {
				events = append(events, ev)
				return nil
			})
			if !errors.Is(err, errFrameTooLarge) {
				t.Fatalf("got %v, want frame size error", err)
			}
			for _, ev := range events {
				if ev.Kind == ir.EventFinish {
					t.Fatal("oversized frame fabricated EventFinish")
				}
			}
			if tc.name == "before output" && len(events) != 0 {
				t.Fatalf("events = %+v, want none before output", events)
			}
		})
	}
}

func TestDecodeAgentStreamDecompressedPayloadTooLarge(t *testing.T) {
	frame := gzipConnectFrame(t, maxDecompressedPayloadBytes+1)
	var events []ir.StreamEvent
	err := DecodeAgentStreamTools(nil, bytes.NewReader(frame), nil, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if !errors.Is(err, errDecompressedPayloadTooLarge) {
		t.Fatalf("got %v, want decompressed size error", err)
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none from the oversized frame", events)
	}
}

func oversizedConnectHeader() []byte {
	hdr := make([]byte, 5)
	hdr[0] = flagNone
	binary.BigEndian.PutUint32(hdr[1:5], ^uint32(0))
	return hdr
}

func gzipConnectFrame(t *testing.T, n int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	chunk := bytes.Repeat([]byte{'a'}, 64<<10)
	var written int64
	for written < n {
		step := int64(len(chunk))
		if remain := n - written; remain < step {
			step = remain
		}
		if _, err := w.Write(chunk[:step]); err != nil {
			t.Fatal(err)
		}
		written += step
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5+buf.Len())
	frame[0] = flagGzip
	binary.BigEndian.PutUint32(frame[1:5], uint32(buf.Len()))
	copy(frame[5:], buf.Bytes())
	return frame
}

func TestDecodeAgentStreamEmptyStreamFinishes(t *testing.T) {
	events, err := collectAgentEventsToolsErr(t, nil)
	if err == nil {
		t.Fatal("empty stream finished as success")
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("err = %v, want StreamFailure", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			t.Fatal("empty stream emitted Finish")
		}
	}
}

func TestDecodeAgentStreamTextOnlyEOFFails(t *testing.T) {
	events, err := collectAgentEventsToolsErr(t, nil, agentTextFrame(t, "partial"))
	if err == nil {
		t.Fatal("text-only EOF finished as success")
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("err = %v, want StreamFailure", err)
	}
	var sawText, sawFinish bool
	for _, ev := range events {
		if ev.Kind == ir.EventTextDelta && ev.Text == "partial" {
			sawText = true
		}
		if ev.Kind == ir.EventFinish {
			sawFinish = true
		}
	}
	if !sawText || sawFinish {
		t.Fatalf("text=%v finish=%v, want text without finish", sawText, sawFinish)
	}
}

func TestDecodeAgentStreamMalformedNestedPayloadsFail(t *testing.T) {
	badLen := []byte{0x0a, 0x05, 0x01}
	cases := []struct {
		name  string
		frame []byte
	}{
		{name: "turn ended", frame: interactionUpdateFrame(t, iuTurnEnded, badLen)},
		{name: "turn ended wrong wire", frame: interactionUpdateFrame(t, iuTurnEnded, encodeField(teInputTokens, wireLen, "12"))},
		{name: "exec", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, badLen))},
		{name: "mcp args", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(5)),
			encodeField(esmMCPArgs, wireLen, badLen),
		)))},
		{name: "text", frame: interactionUpdateFrame(t, iuTextDelta, badLen)},
		{name: "partial", frame: interactionUpdateFrame(t, iuPartialToolCall, badLen)},
		{name: "tool start", frame: interactionUpdateFrame(t, iuToolCallStarted, badLen)},
		{name: "context", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(3)),
			encodeField(esmRequestContextArgs, wireLen, badLen),
		)))},
		{name: "kv", frame: agentFrame(t, encodeField(asmKVServerMessage, wireLen, badLen))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := collectAgentEventsToolsErr(t, nil, agentTextFrame(t, "before"), tc.frame)
			if err == nil {
				t.Fatal("malformed payload was ignored")
			}
			if _, ok := ir.AsStreamFailure(err); !ok {
				t.Fatalf("err = %v, want StreamFailure", err)
			}
			for _, ev := range events {
				if ev.Kind == ir.EventFinish {
					t.Fatal("malformed payload emitted Finish")
				}
			}
		})
	}
}

func TestDecodeAgentStreamMalformedProtobufFails(t *testing.T) {
	// A truncated length-delimited field is not a Connect JSON end-stream.
	bad := agentFrame(t, []byte{0x0a, 0x05, 0x01})
	events, err := collectAgentEventsToolsErr(t, nil, agentTextFrame(t, "before"), bad)
	if err == nil {
		t.Fatal("malformed protobuf was skipped")
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("err = %v, want StreamFailure", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventFinish {
			t.Fatal("malformed protobuf emitted Finish")
		}
	}
}

func TestDecodeAgentStreamEndStreamJSONIsNotProtobuf(t *testing.T) {
	var reads int
	r := &countReader{r: bytes.NewReader(bytes.Join([][]byte{
		agentTextFrame(t, "done"),
		agentTurnEndedFrame(t, 2, 1),
		connectEndStreamFrame(t, []byte(`{"metadata":{"grpc-status":["0"]}}`)),
	}, nil)), n: &reads}
	var events []ir.StreamEvent
	err := DecodeAgentStream(r, nil, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopEndTurn || last.InputTokens != 2 || last.OutputTokens != 1 {
		t.Fatalf("finish = %+v", last)
	}
	if reads == 0 {
		t.Fatal("decoder returned before reading the trailer")
	}
}

type countReader struct {
	r io.Reader
	n *int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += n
	return n, err
}

func TestDecodeAgentStreamTrailerFailures(t *testing.T) {
	good := agentTextFrame(t, "done")
	turn := agentTurnEndedFrame(t, 3, 1)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	comp := make([]byte, 5+buf.Len())
	comp[0] = flagGzipTrailer
	binary.BigEndian.PutUint32(comp[1:5], uint32(buf.Len()))
	copy(comp[5:], buf.Bytes())
	cases := []struct {
		name   string
		frames [][]byte
	}{
		{name: "missing", frames: [][]byte{good, turn}},
		{name: "truncated", frames: [][]byte{good, turn, {flagTrailer, 0, 0, 0, 4, '{', '}'}}},
		{name: "malformed", frames: [][]byte{good, turn, connectEndStreamFrame(t, []byte(`{`))}},
		{name: "array", frames: [][]byte{good, turn, connectEndStreamFrame(t, []byte(`[]`))}},
		{name: "bad metadata", frames: [][]byte{good, turn, connectEndStreamFrame(t, []byte(`{"metadata":{"k":"v"}}`))}},
		{name: "trailer without turn", frames: [][]byte{good, connectEndStreamFrame(t, []byte(`{}`))}},
		{name: "extra envelope", frames: [][]byte{good, turn, connectEndStreamFrame(t, []byte(`{}`)), agentTextFrame(t, "late")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := collectAgentEventsToolsErr(t, nil, tc.frames...)
			if err == nil {
				t.Fatal("invalid transport finished as success")
			}
			for _, ev := range events {
				if ev.Kind == ir.EventFinish {
					t.Fatal("invalid transport emitted Finish")
				}
			}
		})
	}
	t.Run("compressed success", func(t *testing.T) {
		events := collectAgentEvents(t, good, turn, comp)
		last := events[len(events)-1]
		if last.Kind != ir.EventFinish || last.InputTokens != 3 || last.OutputTokens != 1 {
			t.Fatalf("finish = %+v", last)
		}
	})
	t.Run("late error", func(t *testing.T) {
		raw := []byte(`{"error":{"code":"not_found","message":"nope"}}`)
		events, err := collectAgentEventsToolsErr(t, nil, good, turn, connectEndStreamFrame(t, raw))
		if err == nil || !strings.Contains(err.Error(), "cursor: nope") {
			t.Fatalf("err = %v", err)
		}
		for _, ev := range events {
			if ev.Kind == ir.EventFinish {
				t.Fatal("error trailer emitted Finish")
			}
		}
	})
}

func TestDecodeAgentStreamKVAfterTurnEndedBeforeTrailer(t *testing.T) {
	var writes int
	var events []ir.StreamEvent
	err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
		agentTextFrame(t, "done"),
		agentTurnEndedFrame(t, 4, 2),
		kvServerFrame(t, kvsGetBlobArgs),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("kv replies = %d, want 1 after turnEnded", writes)
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.InputTokens != 4 || last.OutputTokens != 2 {
		t.Fatalf("finish = %+v", last)
	}
}

func TestDecodeAgentStreamMCPHandoffDoesNotWaitForTrailer(t *testing.T) {
	blocked := &blockingAfterReader{r: bytes.NewReader(mcpExecFrame(t, "call-1", "read", map[string]any{"path": "a"}))}
	done := make(chan error, 1)
	go func() {
		done <- DecodeAgentStream(blocked, func([]byte) error { return nil }, func(ir.StreamEvent) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("MCP handoff waited for a later frame")
	}
}

type blockingAfterReader struct {
	r    io.Reader
	done bool
}

func (b *blockingAfterReader) Read(p []byte) (int, error) {
	if b.done {
		select {}
	}
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.done = true
		err = nil
	}
	return n, err
}

func TestDecodeAgentStreamValidTurnEndedAndMCP(t *testing.T) {
	textEvents := collectAgentEvents(t, agentTextFrame(t, "ok"), agentTurnEndedFrame(t, 4, 2),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	if textEvents[len(textEvents)-1].Kind != ir.EventFinish {
		t.Fatal("turn_ended did not finish")
	}
	mcpEvents := collectAgentEvents(t, mcpExecFrame(t, "call-1", "noop", nil))
	if mcpEvents[len(mcpEvents)-1].StopReason != ir.StopToolUse {
		t.Fatal("MCP handoff did not finish as tool_use")
	}
}

// TestDecodeAgentStreamWebSearchDoesNotSurface ignores interaction_query
// web search. Built-ins are not client tool calls, and the stream continues.
func TestDecodeAgentStreamWebSearchDoesNotSurface(t *testing.T) {
	events := collectAgentEvents(t,
		webSearchQueryFrame(t, "call-ws", "SPUS"),
		agentTextFrame(t, "after search"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "after search")
}

func TestDecodeAgentStreamWebFetchDoesNotSurface(t *testing.T) {
	args := concatBytes(
		encodeField(iqArgPrimary, wireLen, "https://example.com"),
		encodeField(iqArgCallID, wireLen, "call-wf"),
	)
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(iqWebFetch, wireLen,
			encodeField(iqQueryArgs, wireLen, args))))
	events := collectAgentEvents(t, frame, agentTextFrame(t, "after fetch"), agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "after fetch")
}

func assertNoToolUseContinues(t *testing.T, events []ir.StreamEvent, wantText string) {
	t.Helper()
	var text strings.Builder
	var stop ir.StopReason
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart || ev.Kind == ir.EventToolCallDelta {
			t.Fatalf("built-in surfaced as %v/%s", ev.Kind, ev.ToolName)
		}
		if ev.Kind == ir.EventTextDelta {
			text.WriteString(ev.Text)
		}
		if ev.Kind == ir.EventFinish {
			stop = ev.StopReason
		}
	}
	if text.String() != wantText {
		t.Errorf("text = %q, want %q", text.String(), wantText)
	}
	if stop != ir.StopEndTurn {
		t.Errorf("stop = %q, want end_turn", stop)
	}
}

func assertInteractionToolUse(t *testing.T, events []ir.StreamEvent, id, name, wantArgs string) {
	t.Helper()
	var start, delta *ir.StreamEvent
	var nDelta int
	for i, ev := range events {
		switch ev.Kind {
		case ir.EventToolCallStart:
			start = &events[i]
		case ir.EventToolCallDelta:
			nDelta++
			delta = &events[i]
		}
	}
	if start == nil {
		t.Fatal("no tool call start")
	}
	if start.ToolID != id || start.ToolName != name {
		t.Errorf("tool = %s/%s, want %s/%s", start.ToolID, start.ToolName, id, name)
	}
	if start.ArgsFrag != "" {
		t.Errorf("start ArgsFrag = %q, want empty", start.ArgsFrag)
	}
	if nDelta != 1 || delta == nil {
		t.Fatalf("deltas = %d, want 1", nDelta)
	}
	var got json.RawMessage
	if err := json.Unmarshal([]byte(delta.ArgsFrag), &got); err != nil {
		t.Fatalf("args not JSON: %q (%v)", delta.ArgsFrag, err)
	}
	if string(got) != wantArgs {
		t.Errorf("args = %s, want %s", got, wantArgs)
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v, want tool_use", last)
	}
}

func webSearchQueryFrame(t *testing.T, callID, term string) []byte {
	t.Helper()
	args := concatBytes(
		encodeField(iqArgPrimary, wireLen, term),
		encodeField(iqArgCallID, wireLen, callID),
	)
	return agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(iqWebSearch, wireLen,
			encodeField(iqQueryArgs, wireLen, args))))
}

func webSearchStartedFrame(t *testing.T, callID string) []byte {
	t.Helper()
	// ToolCall{18: WebSearchToolCall{}} with empty args — identity only.
	tc := encodeField(18, wireLen, []byte{})
	inner := concatBytes(
		encodeField(tcsCallID, wireLen, callID),
		encodeField(tcsToolCall, wireLen, tc),
	)
	return interactionUpdateFrame(t, iuToolCallStarted, inner)
}

func TestDecodeAgentStreamWebSearchStartedThenQueryContinues(t *testing.T) {
	events := collectAgentEvents(t,
		webSearchStartedFrame(t, "call-ws"),
		webSearchQueryFrame(t, "call-ws", "SPUS"),
		agentTextFrame(t, "after query"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "after query")
}

func TestDecodeAgentStreamMCPStartedDoesNotBecomeTool15(t *testing.T) {
	// Incomplete MCP ToolCall (field 15, empty args) must not surface as tool_15.
	tc := encodeField(tcMCPTOolCall, wireLen, []byte{})
	inner := concatBytes(
		encodeField(tcsCallID, wireLen, "call-mcp"),
		encodeField(tcsToolCall, wireLen, tc),
	)
	events := collectAgentEvents(t,
		interactionUpdateFrame(t, iuToolCallStarted, inner),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("unexpected tool start %s/%s", ev.ToolID, ev.ToolName)
		}
	}
}

func TestDecodeAgentStreamWebSearchDoesNotRemapToDeclaredTool(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "websearch",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}}
	events := collectAgentEventsTools(t, tools,
		webSearchQueryFrame(t, "call-ws", "SPUS"),
		agentTextFrame(t, "no remap"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "no remap")
}

func TestDecodeAgentStreamWebSearchIgnoredWhenToolsDeclared(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "bash",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}}
	var writes int
	var sawTextAfter bool
	var finished bool
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join([][]byte{
		webSearchQueryFrame(t, "call-ws", "SPUS"),
		agentTextFrame(t, "continued after query"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("interaction emitted %+v", ev)
		}
		if ev.Kind == ir.EventTextDelta && ev.Text == "continued after query" {
			sawTextAfter = true
		}
		if ev.Kind == ir.EventFinish {
			finished = true
			if ev.StopReason != ir.StopEndTurn {
				t.Errorf("stop = %q, want end_turn", ev.StopReason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("rejects = %d, want 0", writes)
	}
	if !finished {
		t.Fatal("interaction query stalled the stream")
	}
	if !sawTextAfter {
		t.Fatal("decoder stopped before text after the interaction query")
	}
}

func TestDecodeAgentStreamShellDoesNotMatchBash(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "bash",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}}
	args := concatBytes(
		encodeField(1, wireLen, "ls"),
		encodeField(4, wireLen, "call-sh"),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(5)),
		encodeField(2, wireLen, args),
	)
	var writes int
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("shell remapped to %s", ev.ToolName)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("rejects = %d, want 1", writes)
	}
}

func TestDecodeAgentStreamShellDoesNotMatchDeclaredShell(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "shell",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}}
	args := concatBytes(
		encodeField(1, wireLen, "ls"),
		encodeField(4, wireLen, "call-sh"),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(5)),
		encodeField(2, wireLen, args),
	)
	var writes int
	events := []ir.StreamEvent{}
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTextFrame(t, "after shell"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("rejects = %d, want 1", writes)
	}
	assertNoToolUseContinues(t, events, "after shell")
}

func TestDecodeAgentStreamAskQuestionDoesNotSurface(t *testing.T) {
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(3, wireLen, []byte{}))) // ask_question_interaction_query
	events := collectAgentEvents(t, frame, agentTextFrame(t, "after ask"), agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "after ask")
}

func TestDecodeAgentStreamAskQuestionIgnoredWithTools(t *testing.T) {
	tools := []ir.Tool{{Name: "bash"}}
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(3, wireLen, []byte{})))
	var writes int
	var finished bool
	var sawText bool
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join([][]byte{
		frame,
		agentTextFrame(t, "continued after ask"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("interaction emitted %+v", ev)
		}
		if ev.Kind == ir.EventTextDelta && ev.Text == "continued after ask" {
			sawText = true
		}
		if ev.Kind == ir.EventFinish {
			finished = true
			if ev.StopReason != ir.StopEndTurn {
				t.Errorf("stop = %q, want end_turn", ev.StopReason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("rejects = %d, want 0", writes)
	}
	if !finished || !sawText {
		t.Fatalf("finished=%v sawText=%v, want both", finished, sawText)
	}
}

func TestDecodeAgentStreamUnknownInteractionIgnored(t *testing.T) {
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(99, wireLen, []byte{})))
	events := collectAgentEvents(t, frame, agentTextFrame(t, "after unknown"), agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "after unknown")
}

func TestDecodeAgentStreamPiBashRejectedNotRemapped(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "bash",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}}
	args := concatBytes(
		encodeField(1, wireLen, "uname -a"),
		encodeField(4, wireLen, "call-pi"),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(5)),
		encodeField(46, wireLen, args), // pi_bash_args
	)
	var writes int
	events := []ir.StreamEvent{}
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join([][]byte{
		agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
		agentTextFrame(t, "after pi"),
		agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	}, nil)), func([]byte) error {
		writes++
		return nil
	}, func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("rejects = %d, want 1", writes)
	}
	assertNoToolUseContinues(t, events, "after pi")
}

func TestParseCursorErrorPrefersDetailTitle(t *testing.T) {
	raw := []byte(`{"error":{"code":"resource_exhausted","message":"Error","details":[{"debug":{"details":{"title":"Update Required","detail":"Your version of Cursor is no longer supported."}}}]}}`)
	err := parseCursorError(raw)
	if err == nil || !strings.Contains(err.Error(), "Update Required") {
		t.Errorf("error = %v, want detail title surfaced", err)
	}
}

func TestParseCursorErrorGeneric(t *testing.T) {
	raw := []byte(`{"error":{"code":"not_found","message":"nope"}}`)
	err := parseCursorError(raw)
	if err == nil || !strings.Contains(err.Error(), "cursor: nope") {
		t.Errorf("error = %v", err)
	}
}

func TestDecloakToolName(t *testing.T) {
	if got := decloakToolName("mcp_custom_my_tool"); got != "my_tool" {
		t.Errorf("decloak = %q", got)
	}
	if got := decloakToolName("plain"); got != "plain" {
		t.Errorf("decloak = %q", got)
	}
}

func TestDecodeAgentStreamOfficialBuiltinMetadataDoesNotFail(t *testing.T) {
	// Minimal known-valid frame: flags 0, length 39, read_tool_call field 8
	// empty, metadata tool_call_id 57 and started_at_ms 59.
	minimal := []byte{
		0x00, 0x00, 0x00, 0x00, 0x27,
		0x0a, 0x25, 0x12, 0x23, 0x0a, 0x09, 0x63, 0x61, 0x6c, 0x6c, 0x2d, 0x72, 0x65, 0x61, 0x64,
		0x12, 0x16, 0x42, 0x00, 0xca, 0x03, 0x09, 0x63, 0x61, 0x6c, 0x6c, 0x2d, 0x72, 0x65, 0x61, 0x64,
		0xd8, 0x03, 0x80, 0xd0, 0x95, 0xff, 0xbc, 0x31,
	}
	hook := encodeField(54, wireLen, concatBytes(
		encodeField(1, wireLen, "preToolUse"),
		encodeField(2, wireLen, "ctx"),
	))
	completed := encodeField(60, wireVarint, uint64(1700000000001))
	unknownScalar := encodeField(99, wireVarint, uint64(7))
	unknownBytes := encodeField(100, wireLen, []byte{0xff, 0x00, 0x01})
	readArgs := concatBytes(encodeField(1, wireLen, "/tmp/a"), encodeField(2, wireLen, "call-read"))
	shellArgs := concatBytes(encodeField(1, wireLen, "ls"), encodeField(4, wireLen, "call-shell"))
	readTool := concatBytes(
		encodeField(8, wireLen, encodeField(1, wireLen, readArgs)),
		hook, unknownScalar, unknownBytes,
		encodeField(57, wireLen, "call-read"),
		encodeField(59, wireVarint, uint64(1700000000000)),
		completed,
	)
	shellTool := concatBytes(
		encodeField(1, wireLen, encodeField(1, wireLen, shellArgs)),
		encodeField(57, wireLen, "call-shell"),
		encodeField(59, wireVarint, uint64(1700000000000)),
	)
	tools := []ir.Tool{{Name: "read"}, {Name: "shell"}}
	frames := [][]byte{
		minimal,
		interactionUpdateFrame(t, iuToolCallStarted, concatBytes(
			encodeField(tcsCallID, wireLen, "call-read"),
			encodeField(tcsToolCall, wireLen, readTool),
		)),
		interactionUpdateFrame(t, iuPartialToolCall, concatBytes(
			encodeField(ptcCallID, wireLen, "call-shell"),
			encodeField(ptcToolCall, wireLen, shellTool),
			encodeField(ptcArgsDelta, wireLen, "{"),
		)),
		agentTextFrame(t, "after builtins"),
		mcpToolCallStartedFrame(t, "call-mcp", "read", map[string]any{"path": "a"}),
	}
	var sawMCP bool
	err := DecodeAgentStreamTools(tools, bytes.NewReader(bytes.Join(frames, nil)), nil, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			if ev.ToolName != "read" || ev.ToolID != "call-mcp" {
				t.Fatalf("surfaced %s id=%s", ev.ToolName, ev.ToolID)
			}
			sawMCP = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawMCP {
		t.Fatal("later MCP call was not delivered")
	}
}

func TestDecodeAgentStreamMalformedKnownToolOneofFails(t *testing.T) {
	// Field 8 is the known read_tool_call oneof. A varint is not a message.
	tool := encodeField(8, wireVarint, uint64(1))
	frame := interactionUpdateFrame(t, iuToolCallStarted, concatBytes(
		encodeField(tcsCallID, wireLen, "call-bad"),
		encodeField(tcsToolCall, wireLen, tool),
	))
	_, err := collectAgentEventsToolsErr(t, []ir.Tool{{Name: "read"}}, frame)
	if err == nil {
		t.Fatal("malformed known tool oneof was accepted")
	}
	if _, ok := ir.AsStreamFailure(err); !ok {
		t.Fatalf("err = %v, want StreamFailure", err)
	}
}

func TestDecodeAgentStreamMalformedIdentityStringsFail(t *testing.T) {
	badVarint := encodeField(1, wireVarint, uint64(7))
	cases := []struct {
		name  string
		frame []byte
	}{
		{name: "text", frame: interactionUpdateFrame(t, iuTextDelta, badVarint)},
		{name: "exec id", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(1)),
			encodeField(esmExecID, wireVarint, uint64(9)),
		)))},
		{name: "mcp name", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(1)),
			encodeField(esmMCPArgs, wireLen, encodeField(maName, wireVarint, uint64(1))),
		)))},
		{name: "mcp tool name", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(1)),
			encodeField(esmMCPArgs, wireLen, encodeField(maToolName, wireVarint, uint64(1))),
		)))},
		{name: "mcp call id", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(1)),
			encodeField(esmMCPArgs, wireLen, encodeField(maCallID, wireFixed64, 1.0)),
		)))},
		{name: "tool start call id", frame: interactionUpdateFrame(t, iuToolCallStarted, encodeField(tcsCallID, wireVarint, uint64(1)))},
		{name: "partial delta", frame: interactionUpdateFrame(t, iuPartialToolCall, concatBytes([]byte{0x1d}, []byte{1, 2, 3, 4}))},
		{name: "map key", frame: agentFrame(t, encodeField(asmExecServerMessage, wireLen, concatBytes(
			encodeField(esmID, wireVarint, uint64(1)),
			encodeField(esmMCPArgs, wireLen, concatBytes(
				encodeField(maName, wireLen, "read"),
				encodeField(maCallID, wireLen, "c1"),
				encodeField(maArgs, wireLen, concatBytes(
					encodeField(1, wireVarint, uint64(1)),
					encodeField(2, wireLen, encodeField(3, wireLen, "x")),
				)),
			)),
		)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectAgentEventsToolsErr(t, nil, tc.frame)
			if err == nil {
				t.Fatal("malformed string was accepted")
			}
			if _, ok := ir.AsStreamFailure(err); !ok {
				t.Fatalf("err = %v, want StreamFailure", err)
			}
		})
	}
}

func TestDecodeAgentStreamScalarBuiltinArgsStillRejected(t *testing.T) {
	reason := "not available; use the declared MCP tools"
	for _, tc := range []struct {
		name     string
		argField int
		args     []byte
		result   int
	}{
		{name: "record", argField: 21, result: 21, args: concatBytes(
			encodeField(1, wireVarint, uint64(1)),
			encodeField(2, wireLen, "call-rec"),
		)},
		{name: "stdin", argField: 23, result: 23, args: concatBytes(
			encodeField(1, wireVarint, uint64(9)),
			encodeField(2, wireLen, "abc"),
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := concatBytes(
				encodeField(esmID, wireVarint, uint64(4)),
				encodeField(esmExecID, wireLen, "exec-"+tc.name),
				encodeField(tc.argField, wireLen, tc.args),
			)
			var writes [][]byte
			err := DecodeAgentStream(bytes.NewReader(bytes.Join([][]byte{
				agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)),
				agentTextFrame(t, "after"),
				agentTurnEndedFrame(t, 1, 1),
				connectEndStreamFrame(t, []byte(`{}`)),
			}, nil)), func(frame []byte) error {
				writes = append(writes, frame)
				return nil
			}, func(ir.StreamEvent) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(writes) != 1 {
				t.Fatalf("writes = %d", len(writes))
			}
			_, payload, err := readFrame(bytes.NewReader(writes[0]))
			if err != nil {
				t.Fatal(err)
			}
			cm, err := decodeMessage(payload)
			if err != nil {
				t.Fatal(err)
			}
			em := mustExecClient(t, cm)
			body := mustField(t, em, tc.result)
			assertReasonOnly(t, body, map[int]int{21: 4, 23: 2}[tc.result], 1, reason)
		})
	}
}

func TestEndStreamValidation(t *testing.T) {
	good := agentTextFrame(t, "done")
	turn := agentTurnEndedFrame(t, 3, 1)
	success := []string{
		`{}`,
		`{"error":null}`,
		`{"error": null}`,
		"{\n\t\"error\" : null\n}",
		`{"metadata":{"error":["not-a-failure"],"grpc-status":[]}}`,
	}
	for _, raw := range success {
		t.Run("success "+raw, func(t *testing.T) {
			var reads int
			r := &countReader{r: bytes.NewReader(bytes.Join([][]byte{
				good, turn, connectEndStreamFrame(t, []byte(raw)),
			}, nil)), n: &reads}
			var finished bool
			err := DecodeAgentStream(r, nil, func(ev ir.StreamEvent) error {
				if ev.Kind == ir.EventFinish {
					finished = true
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !finished {
				t.Fatal("valid end-stream did not finish")
			}
			if reads == 0 {
				t.Fatal("decoder returned before reading the trailer")
			}
		})
	}
	invalid := []string{
		`null`,
		`"x"`,
		`1`,
		`[]`,
		`{"metadata":null}`,
		`{"metadata":[]}`,
		`{"metadata":1}`,
		`{"metadata":{"k":null}}`,
		`{"metadata":{"k":[null]}}`,
		`{"metadata":{"k":["ok",1]}}`,
		`{"error":null,"metadata":null}`,
	}
	for _, raw := range invalid {
		t.Run("invalid "+raw, func(t *testing.T) {
			events, err := collectAgentEventsToolsErr(t, nil, good, turn, connectEndStreamFrame(t, []byte(raw)))
			if err == nil {
				t.Fatal("invalid end-stream finished")
			}
			for _, ev := range events {
				if ev.Kind == ir.EventFinish {
					t.Fatal("invalid end-stream emitted Finish")
				}
			}
		})
	}
	t.Run("late error", func(t *testing.T) {
		raw := []byte(`{"error":{"code":"resource_exhausted","message":"Error","details":[{"type":"t","value":"YQ==","debug":{"details":{"title":"Named models unavailable","detail":"later"}}}]}}`)
		_, err := collectAgentEventsToolsErr(t, nil, good, turn, connectEndStreamFrame(t, raw))
		if err == nil || !strings.Contains(err.Error(), "Named models unavailable") {
			t.Fatalf("err = %v", err)
		}
		sf, ok := ir.AsStreamFailure(err)
		if !ok || sf.Code != "resource_exhausted" {
			t.Fatalf("failure = %+v", err)
		}
	})
}

func TestDecodeAgentStreamMCPArgsMapValues(t *testing.T) {
	stringValue := func(s string) []byte { return encodeField(3, wireLen, s) }
	nullValue := encodeField(1, wireVarint, uint64(0))
	entry := func(key string, value []byte) []byte {
		return encodeField(maArgs, wireLen, concatBytes(
			encodeField(1, wireLen, key),
			encodeField(2, wireLen, value),
		))
	}
	nested := encodeField(5, wireLen, encodeField(1, wireLen, concatBytes(
		encodeField(1, wireLen, ""),
		encodeField(2, wireLen, stringValue("empty-key")),
	)))
	missingKey := encodeField(5, wireLen, encodeField(1, wireLen, concatBytes(
		encodeField(2, wireLen, stringValue("missing-key")),
	)))
	list := encodeField(6, wireLen, encodeField(1, wireLen, concatBytes(
		encodeField(3, wireLen, "item"),
		encodeField(99, wireVarint, uint64(1)),
	)))
	ma := concatBytes(
		encodeField(maToolName, wireLen, "read"),
		encodeField(maCallID, wireLen, "call-map"),
		entry("", stringValue("root-empty")),
		entry("n", encodeField(2, wireFixed64, 1.5)),
		entry("b", encodeField(4, wireVarint, uint64(1))),
		entry("z", nullValue),
		entry("s", nested),
		entry("m", missingKey),
		entry("l", list),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(8)),
		encodeField(esmExecID, wireLen, "exec-map"),
		encodeField(esmMCPArgs, wireLen, ma),
	)
	var args string
	err := DecodeAgentStream(bytes.NewReader(mcpFrameFromExec(t, ex)), func([]byte) error { return nil }, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallDelta {
			args = ev.ArgsFrag
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(args), &got); err != nil {
		t.Fatal(err)
	}
	if got[""] != "root-empty" || got["n"].(float64) != 1.5 || got["b"] != true || got["z"] != nil {
		t.Fatalf("args = %s", args)
	}
	nestedGot, ok := got["s"].(map[string]any)
	if !ok || nestedGot[""] != "empty-key" {
		t.Fatalf("empty key = %#v", got["s"])
	}
	missingGot, ok := got["m"].(map[string]any)
	if !ok || missingGot[""] != "missing-key" {
		t.Fatalf("missing key = %#v", got["m"])
	}
	listGot, ok := got["l"].([]any)
	if !ok || len(listGot) != 1 || listGot[0] != "item" {
		t.Fatalf("list = %#v", got["l"])
	}
}

func TestDecodeAgentStreamMalformedMCPValuesFail(t *testing.T) {
	entry := func(key string, value []byte, includeValue bool) []byte {
		parts := []byte(encodeField(1, wireLen, key))
		if includeValue {
			parts = append(parts, encodeField(2, wireLen, value)...)
		}
		return encodeField(maArgs, wireLen, parts)
	}
	nan := encodeField(2, wireFixed64, math.NaN())
	inf := encodeField(2, wireFixed64, math.Inf(1))
	emptyValue := []byte{}
	unknownOnly := encodeField(99, wireLen, []byte{0x01})
	badNull := encodeField(1, wireVarint, uint64(1))
	nestedBad := encodeField(5, wireLen, encodeField(1, wireLen, concatBytes(
		encodeField(1, wireLen, "k"),
		encodeField(2, wireLen, emptyValue),
	)))
	for _, tc := range []struct {
		name string
		args []byte
	}{
		{name: "missing value", args: entry("k", nil, false)},
		{name: "empty value", args: entry("k", emptyValue, true)},
		{name: "unknown only", args: entry("k", unknownOnly, true)},
		{name: "bad null", args: entry("k", badNull, true)},
		{name: "nan", args: entry("k", nan, true)},
		{name: "inf", args: entry("k", inf, true)},
		{name: "nested", args: entry("k", nestedBad, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ma := concatBytes(
				encodeField(maName, wireLen, "read"),
				encodeField(maCallID, wireLen, "c"),
				tc.args,
			)
			ex := concatBytes(
				encodeField(esmID, wireVarint, uint64(1)),
				encodeField(esmMCPArgs, wireLen, ma),
			)
			_, err := collectAgentEventsToolsErr(t, nil, agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)))
			if err == nil {
				t.Fatal("malformed value was accepted")
			}
			if _, ok := ir.AsStreamFailure(err); !ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestInteractionQueryUnknownFieldsIgnored(t *testing.T) {
	query := concatBytes(
		encodeField(1, wireVarint, uint64(4)),
		encodeField(90, wireVarint, uint64(1)),
		encodeField(91, wireLen, []byte{0xff, 0x01}),
		encodeField(2, wireLen, encodeField(1, wireLen, "query")),
	)
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen, query))
	events := collectAgentEvents(t, frame, agentTextFrame(t, "after query"), agentTurnEndedFrame(t, 1, 1),
		connectEndStreamFrame(t, []byte(`{}`)),
	)
	assertNoToolUseContinues(t, events, "after query")
}

func mcpFrameFromExec(t *testing.T, ex []byte) []byte {
	t.Helper()
	return agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex))
}
