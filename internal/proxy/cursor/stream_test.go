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

// TestDecodeAgentStreamMCPToolCallEmptyThenDeltas covers incremental args:
// tool_call_started with an empty McpArgs map ("{}") must not emit a "{}"
// Delta, or later ptcArgsDelta fragments would concatenate to invalid JSON.
func TestDecodeAgentStreamMCPToolCallEmptyThenDeltas(t *testing.T) {
	events := collectAgentEvents(t,
		mcpToolCallStartedFrame(t, "call-2", "websearch", nil),
		mcpPartialArgsFrame(t, "call-2", `{"query":`),
		mcpPartialArgsFrame(t, "call-2", `"SPUS"}`),
		agentTurnEndedFrame(t, 1, 1),
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

func TestDecodeAgentStreamShellExecEmitsToolUse(t *testing.T) {
	args := concatBytes(
		encodeField(1, wireLen, "ls -la"),
		encodeField(4, wireLen, "call-sh"),
	)
	ex := concatBytes(
		encodeField(esmID, wireVarint, uint64(5)),
		encodeField(2, wireLen, args), // shell_args
	)
	events := collectAgentEvents(t, agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)))
	assertInteractionToolUse(t, events, "call-sh", "shell", `{"command":"ls -la"}`)
}

func TestDecodeAgentStreamUnknownExecOneofStillSurfaces(t *testing.T) {
	// Field 99 is not in execToolName; the decoder must still emit a tool
	// call (name exec_99) instead of fail-closing.
	args := encodeField(1, wireLen, "payload")
	ex := encodeField(99, wireLen, args)
	events := collectAgentEvents(t, agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)))
	var start *ir.StreamEvent
	for i, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			start = &events[i]
		}
	}
	if start == nil {
		t.Fatal("no tool call start")
	}
	if start.ToolName != "exec_99" {
		t.Errorf("name = %q, want exec_99", start.ToolName)
	}
	last := events[len(events)-1]
	if last.Kind != ir.EventFinish || last.StopReason != ir.StopToolUse {
		t.Fatalf("finish = %+v, want tool_use", last)
	}
}

func TestDecodeAgentStreamReadToolCallStarted(t *testing.T) {
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
		agentTurnEndedFrame(t, 1, 1),
	)
	assertInteractionToolUse(t, events, "call-rd", "read", `{"path":"/etc/hostname"}`)
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

// TestDecodeAgentStreamWebSearchEmitsToolUse surfaces interaction_query web
// search as IR tool_use under Cursor's native name, then finishes the turn.
func TestDecodeAgentStreamWebSearchEmitsToolUse(t *testing.T) {
	args := concatBytes(
		encodeField(iqArgPrimary, wireLen, "SPUS"),
		encodeField(iqArgCallID, wireLen, "call-ws"),
	)
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(iqWebSearch, wireLen,
			encodeField(iqQueryArgs, wireLen, args))))
	events := collectAgentEvents(t, frame)
	assertInteractionToolUse(t, events, "call-ws", "web_search", `{"search_term":"SPUS"}`)
}

// TestDecodeAgentStreamWebFetchEmitsToolUse is the web_fetch counterpart.
func TestDecodeAgentStreamWebFetchEmitsToolUse(t *testing.T) {
	args := concatBytes(
		encodeField(iqArgPrimary, wireLen, "https://example.com"),
		encodeField(iqArgCallID, wireLen, "call-wf"),
	)
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(iqWebFetch, wireLen,
			encodeField(iqQueryArgs, wireLen, args))))
	events := collectAgentEvents(t, frame)
	assertInteractionToolUse(t, events, "call-wf", "web_fetch", `{"url":"https://example.com"}`)
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

func TestDecodeAgentStreamWebSearchStartedThenQueryFlushesArgs(t *testing.T) {
	events := collectAgentEvents(t,
		webSearchStartedFrame(t, "call-ws"),
		webSearchQueryFrame(t, "call-ws", "SPUS"),
	)
	assertInteractionToolUse(t, events, "call-ws", "web_search", `{"search_term":"SPUS"}`)
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
	)
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("unexpected tool start %s/%s", ev.ToolID, ev.ToolName)
		}
	}
}

func TestDecodeAgentStreamWebSearchRemapsToDeclaredWebsearch(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "websearch",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}}
	events := collectAgentEventsTools(t, tools, webSearchQueryFrame(t, "call-ws", "SPUS"))
	assertInteractionToolUse(t, events, "call-ws", "websearch", `{"query":"SPUS"}`)
}

func TestDecodeAgentStreamWebSearchUnmatchedDropped(t *testing.T) {
	tools := []ir.Tool{{
		Name:       "bash",
		Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
	}}
	events, err := collectAgentEventsToolsErr(t, tools, webSearchQueryFrame(t, "call-ws", "SPUS"))
	ub, ok := AsUnmatchedBuiltin(err)
	if !ok || ub.Name != "web_search" {
		t.Fatalf("err = %v, want unmatched web_search", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart || ev.Kind == ir.EventFinish {
			t.Fatalf("unmatched built-in emitted %+v", ev)
		}
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
	events, err := collectAgentEventsToolsErr(t, tools, agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)))
	if _, ok := AsUnmatchedBuiltin(err); !ok {
		t.Fatalf("err = %v, want unmatched builtin", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			t.Fatalf("shell remapped to %s", ev.ToolName)
		}
	}
}

func TestDecodeAgentStreamShellMatchesDeclaredShell(t *testing.T) {
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
	events := collectAgentEventsTools(t, tools, agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)))
	assertInteractionToolUse(t, events, "call-sh", "shell", `{"command":"ls"}`)
}

func TestDecodeAgentStreamAskQuestionPassThroughWithoutTools(t *testing.T) {
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(3, wireLen, []byte{}))) // ask_question_interaction_query
	events := collectAgentEvents(t, frame)
	var start *ir.StreamEvent
	for i, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			start = &events[i]
		}
	}
	if start == nil || start.ToolName != "ask_question" {
		t.Fatalf("tool = %+v, want ask_question", start)
	}
}

func TestDecodeAgentStreamAskQuestionUnmatched(t *testing.T) {
	tools := []ir.Tool{{Name: "bash"}}
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(3, wireLen, []byte{})))
	events, err := collectAgentEventsToolsErr(t, tools, frame)
	ub, ok := AsUnmatchedBuiltin(err)
	if !ok || ub.Name != "ask_question" {
		t.Fatalf("err = %v, want unmatched ask_question", err)
	}
	for _, ev := range events {
		if ev.Kind == ir.EventToolCallStart || ev.Kind == ir.EventFinish {
			t.Fatalf("unmatched interaction emitted %+v", ev)
		}
	}
}

func TestDecodeAgentStreamUnknownInteractionNamed(t *testing.T) {
	frame := agentFrame(t, encodeField(asmInteractionQuery, wireLen,
		encodeField(99, wireLen, []byte{})))
	events := collectAgentEvents(t, frame)
	var start *ir.StreamEvent
	for i, ev := range events {
		if ev.Kind == ir.EventToolCallStart {
			start = &events[i]
		}
	}
	if start == nil || start.ToolName != "interaction_99" {
		t.Fatalf("tool = %+v, want interaction_99", start)
	}
}

func TestDecodeAgentStreamPiBashRemapsToBash(t *testing.T) {
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
	events := collectAgentEventsTools(t, tools, agentFrame(t, encodeField(asmExecServerMessage, wireLen, ex)))
	assertInteractionToolUse(t, events, "call-pi", "bash", `{"command":"uname -a"}`)
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
