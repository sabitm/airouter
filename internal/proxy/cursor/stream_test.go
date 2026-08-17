package cursor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
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
	var out []ir.StreamEvent
	err := DecodeAgentStream(bytes.NewReader(bytes.Join(frames, nil)), nil, func(ev ir.StreamEvent) error {
		out = append(out, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("DecodeAgentStream: %v", err)
	}
	return out
}

func TestDecodeAgentStreamTextDeltas(t *testing.T) {
	events := collectAgentEvents(t,
		agentTextFrame(t, "Hello"),
		agentTextFrame(t, " world"),
		agentTurnEndedFrame(t, 12, 3),
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
	if events[0].Kind != ir.EventMessageStart {
		t.Errorf("first event = %+v, want message start", events[0])
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
	var toolStart *ir.StreamEvent
	for i, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			toolStart = &events[i]
		}
	}
	if toolStart == nil {
		t.Fatal("no tool call start event")
	}
	if toolStart.ToolID != "call-1" || toolStart.ToolName != "get_weather" {
		t.Errorf("tool call = %s/%s", toolStart.ToolID, toolStart.ToolName)
	}
	var args json.RawMessage
	if err := json.Unmarshal([]byte(toolStart.ArgsFrag), &args); err != nil {
		t.Fatalf("args not JSON: %q (%v)", toolStart.ArgsFrag, err)
	}
	if string(args) != `{"city":"Tokyo"}` {
		t.Errorf("args = %s", args)
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v, want tool_use", last)
	}
}

func TestDecodeAgentStreamUnsupportedExecToolFails(t *testing.T) {
	// shell_args (field 2) — an IDE tool the proxy cannot execute.
	ex := encodeField(2, wireLen, []byte{})
	frame := agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex))
	err := DecodeAgentStream(bytes.NewReader(frame), nil, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected unsupported-tool error")
	}
	if !strings.Contains(err.Error(), "cannot execute") {
		t.Errorf("error = %v", err)
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

func TestDecodeAgentStreamEmptyStreamFinishes(t *testing.T) {
	events := collectAgentEvents(t)
	if len(events) != 2 || events[0].Kind != ir.EventMessageStart || events[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v, want start+finish", events)
	}
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
