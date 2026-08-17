package cursor

// agentstream.go decodes the agent.v1.AgentService/Run response stream and
// services the server's mid-stream control requests (KV blob storage and
// request-context queries) over the still-open request body. Unlike the retired
// ChatService stream, this endpoint is a bidi Connect stream: the decoder needs
// a write callback alongside the reader.

import (
	"encoding/json"
	"io"
	"math"
	"strings"

	"airouter/internal/proxy/ir"
)

// DecodeAgentStream reads AgentService Connect frames and emits IR events.
// writeFrame sends one AgentClientMessage payload (unframed protobuf; this
// function applies the Connect frame wrapper) back upstream; it may be nil,
// in which case control requests are ignored and the stream may stall.
func DecodeAgentStream(r io.Reader, writeFrame func([]byte) error, emit func(ir.StreamEvent) error) error {
	started := false
	msgID := ""

	type tcall struct {
		index   int
		id      string
		name    string
		args    strings.Builder
		started bool
	}
	toolCalls := map[string]*tcall{}
	toolOrder := []string{}
	var stopReason ir.StopReason = ir.StopEndTurn
	var inTok, outTok int

	emitStart := func() error {
		if started {
			return nil
		}
		started = true
		if msgID == "" {
			msgID = ir.NewID("msg_")
		}
		return emit(ir.StreamEvent{Kind: ir.EventMessageStart, ID: msgID})
	}

	emitFinish := func() error {
		// Finalize tool calls that never completed (stream cut short).
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if !tc.started {
				tc.started = true
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.index, ToolID: tc.id, ToolName: tc.name}); err != nil {
					return err
				}
				if tc.args.Len() > 0 {
					if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: tc.index, ToolID: tc.id, ToolName: tc.name, ArgsFrag: tc.args.String()}); err != nil {
						return err
					}
				}
			}
		}
		if len(toolOrder) > 0 {
			stopReason = ir.StopToolUse
		}
		if err := emitStart(); err != nil {
			return err
		}
		return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason, InputTokens: inTok, OutputTokens: outTok})
	}

	// startToolCall registers (or looks up) a call and emits its Start event.
	startToolCall := func(id, name, argsJSON string) error {
		if id == "" || name == "" {
			return nil
		}
		tc, seen := toolCalls[id]
		if !seen {
			tc = &tcall{index: len(toolOrder), id: id, name: name}
			toolCalls[id] = tc
			toolOrder = append(toolOrder, id)
		}
		if argsJSON != "" {
			tc.args.WriteString(argsJSON)
		}
		if !tc.started {
			tc.started = true
			if err := emitStart(); err != nil {
				return err
			}
			return emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.index, ToolID: tc.id, ToolName: tc.name, ArgsFrag: tc.args.String()})
		}
		return nil
	}

	for {
		flags, payload, err := readFrame(r)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if payload == nil {
			continue
		}
		data := decompressPayload(payload, flags)

		if len(data) > 0 && data[0] == 0x7b && isCursorError(data) {
			return parseCursorError(data)
		}

		top, derr := decodeMessage(data)
		if derr != nil {
			continue
		}

		// kv_server_message: reply with empty blob results. Cursor stores
		// conversation state blobs here; without a reply the run stalls. The
		// proxy is stateless across requests, so nothing is persisted.
		if kvs, ok := top[asmKVServerMessage]; ok && len(kvs) > 0 && writeFrame != nil {
			if reply := encodeKVReply(kvs[0].value); reply != nil {
				if err := writeFrame(reply); err != nil {
					return err
				}
			}
		}

		// exec_server_message: request-context queries get an empty context.
		// MCP tool calls are surfaced to the client as IR tool_use (the client
		// executes them; the result returns with the next request's history).
		// Any other exec request is an IDE tool the proxy cannot service.
		if exs, ok := top[asmExecServerMessage]; ok && len(exs) > 0 {
			if done, err := handleExecServerMessage(exs[0].value, writeFrame, startToolCall, emitStart); err != nil {
				return err
			} else if done {
				// An MCP tool call was surfaced: end the turn so the client can
				// execute it; the abandoned upstream session is by design.
				return emitFinish()
			}
		}

		// interaction_update: the actual content stream.
		if ius, ok := top[asmInteractionUpdate]; ok {
			for _, iu := range ius {
				update, err := decodeMessage(iu.value)
				if err != nil {
					continue
				}
				if err := emitStart(); err != nil {
					return err
				}
				// text_delta
				if tds, ok := update[iuTextDelta]; ok && len(tds) > 0 {
					if text, ok := stringField(decodeOrEmpty(tds[0].value), tdText); ok && text != "" {
						if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: text}); err != nil {
							return err
						}
					}
				}
				// thinking_delta: dropped. Cursor reasoning carries no
				// cryptographic signature, so strict thinking consumers
				// (Anthropic clients) would reject or stall on it.
				// tool_call_started / partial_tool_call: client-visible MCP calls.
				if tcss, ok := update[iuToolCallStarted]; ok && len(tcss) > 0 {
					if id, name, args, ok := extractMCPToolCall(tcss[0].value); ok {
						if err := startToolCall(id, name, args); err != nil {
							return err
						}
					}
				}
				if ptcs, ok := update[iuPartialToolCall]; ok && len(ptcs) > 0 {
					p, _ := decodeMessage(ptcs[0].value)
					id, _ := stringField(p, ptcCallID)
					if delta, ok := stringField(p, ptcArgsDelta); ok && delta != "" && id != "" {
						if tc, seen := toolCalls[id]; seen {
							tc.args.WriteString(delta)
							if err := emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: tc.index, ToolID: tc.id, ToolName: tc.name, ArgsFrag: delta}); err != nil {
								return err
							}
						}
					}
					if id == "" || toolCalls[id] == nil {
						if cid, name, args, ok := extractMCPToolCall(ptcs[0].value); ok {
							if err := startToolCall(cid, name, args); err != nil {
								return err
							}
						}
					}
				}
				// token_delta: running output count; authoritative usage arrives
				// with turn_ended.
				// turn_ended: final usage and stop.
				if tes, ok := update[iuTurnEnded]; ok && len(tes) > 0 {
					te, _ := decodeMessage(tes[0].value)
					if v, ok := varintField(te, teInputTokens); ok {
						inTok = int(v)
					}
					if v, ok := varintField(te, teOutputTokens); ok {
						outTok = int(v)
					}
					return emitFinish()
				}
			}
		}
	}

	return emitFinish()
}

func decodeOrEmpty(b []byte) map[int][]field {
	m, err := decodeMessage(b)
	if err != nil {
		return map[int][]field{}
	}
	return m
}

// encodeKVReply builds the KvClientMessage for one KvServerMessage: get ->
// empty GetBlobResult (blob not found), set -> empty SetBlobResult (success).
func encodeKVReply(server []byte) []byte {
	m, err := decodeMessage(server)
	if err != nil {
		return nil
	}
	id, _ := varintField(m, kvsID)
	switch {
	case m[kvsGetBlobArgs] != nil:
		client := concatBytes(
			encodeField(kvcID, wireVarint, id),
			encodeField(kvcGetBlobRes, wireLen, []byte{}),
		)
		return wrapConnectFrame(encodeField(3, wireLen, client), false)
	case m[kvsSetBlobArgs] != nil:
		client := concatBytes(
			encodeField(kvcID, wireVarint, id),
			encodeField(kvcSetBlobRes, wireLen, []byte{}),
		)
		return wrapConnectFrame(encodeField(3, wireLen, client), false)
	default:
		return nil
	}
}

// handleExecServerMessage services one ExecServerMessage. Returns done=true
// when the turn must end after surfacing an MCP tool call to the client.
func handleExecServerMessage(server []byte, writeFrame func([]byte) error, startToolCall func(id, name, args string) error, emitStart func() error) (bool, error) {
	m, err := decodeMessage(server)
	if err != nil {
		return false, nil
	}
	id, _ := varintField(m, esmID)
	execID, _ := stringField(m, esmExecID)

	if m[esmRequestContextArgs] != nil {
		if writeFrame == nil {
			return false, nil
		}
		// ExecClientMessage{1: id, 15: exec_id, 10: RequestContextResult{
		// 1: RequestContextSuccess{}}} — empty context, like the CLI on a
		// context-less run.
		result := encodeField(ecmRequestContextRes, wireLen,
			encodeField(1, wireLen, []byte{}))
		client := concatBytes(
			encodeField(ecmID, wireVarint, id),
			encodeField(ecmExecID, wireLen, execID),
			result,
		)
		if err := writeFrame(wrapConnectFrame(encodeField(2, wireLen, client), false)); err != nil {
			return false, err
		}
		return false, nil
	}

	if args, ok := m[esmMCPArgs]; ok && len(args) > 0 {
		am, err := decodeMessage(args[0].value)
		if err != nil {
			return false, nil
		}
		callID, _ := stringField(am, maCallID)
		name, _ := stringField(am, maToolName)
		if name == "" {
			name, _ = stringField(am, maName)
		}
		argsJSON := mcpArgsMapJSON(am)
		if callID != "" && name != "" {
			if err := startToolCall(callID, decloakToolName(name), argsJSON); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	// Any other exec request (shell, read, edit, ...) is an IDE tool this
	// proxy cannot execute. Fail the turn with a clear message rather than
	// hanging or narrating protocol state as assistant text.
	return false, &ir.StreamFailure{
		Message: "cursor: model requested an IDE tool the proxy cannot execute; rephrase without requiring local tools",
	}
}

// extractMCPToolCall pulls the MCP variant out of a ToolCallStartedUpdate:
// {1: call_id, 2: ToolCall{15: McpToolCall{1: McpArgs{...}}}, 3: model_call_id}.
func extractMCPToolCall(update []byte) (id, name, argsJSON string, ok bool) {
	m, err := decodeMessage(update)
	if err != nil {
		return "", "", "", false
	}
	callID, _ := stringField(m, tcsCallID)
	if tcs, ok := m[tcsToolCall]; ok && len(tcs) > 0 {
		tc, err := decodeMessage(tcs[0].value)
		if err != nil {
			return "", "", "", false
		}
		if mtcs, ok := tc[tcMCPTOolCall]; ok && len(mtcs) > 0 {
			mtc, err := decodeMessage(mtcs[0].value)
			if err != nil {
				return "", "", "", false
			}
			if mas, ok := mtc[mtcArgs]; ok && len(mas) > 0 {
				am, err := decodeMessage(mas[0].value)
				if err == nil {
					if n, ok := stringField(am, maToolName); ok && n != "" {
						name = n
					} else if n, ok := stringField(am, maName); ok {
						name = n
					}
					if c, ok := stringField(am, maCallID); ok && c != "" {
						callID = c
					}
					argsJSON = mcpArgsMapJSON(am)
				}
			}
		}
	}
	if callID == "" || name == "" {
		return "", "", "", false
	}
	return callID, decloakToolName(name), argsJSON, true
}

// mcpArgsMapJSON renders McpArgs.args (field 2, map<string, google.protobuf.
// Value>) as a JSON object string. Empty map renders as "{}".
func mcpArgsMapJSON(am map[int][]field) string {
	entries, ok := am[maArgs]
	if !ok || len(entries) == 0 {
		return "{}"
	}
	var sb strings.Builder
	sb.WriteByte('{')
	first := true
	for _, e := range entries {
		entry, err := decodeMessage(e.value)
		if err != nil {
			continue
		}
		key, _ := stringField(entry, 1)
		if key == "" {
			continue
		}
		var val any
		if vs, ok := entry[2]; ok && len(vs) > 0 {
			val = protoValueToGo(vs[0].value)
		}
		encoded, err := json.Marshal(val)
		if err != nil {
			continue
		}
		kb, _ := json.Marshal(key)
		if !first {
			sb.WriteByte(',')
		}
		first = false
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(encoded)
	}
	sb.WriteByte('}')
	return sb.String()
}

// protoValueToGo converts a google.protobuf.Value message to a Go value that
// json.Marshal can render. Unknown/absent kind marshals as null.
func protoValueToGo(b []byte) any {
	m, err := decodeMessage(b)
	if err != nil {
		return nil
	}
	if f, ok := m[3]; ok && len(f) > 0 { // string_value
		return string(f[0].value)
	}
	if f, ok := m[4]; ok && len(f) > 0 { // bool_value
		return len(f[0].value) > 0 && f[0].value[0] != 0
	}
	if f, ok := m[2]; ok && len(f) > 0 { // number_value (fixed64 double)
		if len(f[0].value) == 8 {
			bits := uint64(f[0].value[0]) | uint64(f[0].value[1])<<8 | uint64(f[0].value[2])<<16 | uint64(f[0].value[3])<<24 |
				uint64(f[0].value[4])<<32 | uint64(f[0].value[5])<<40 | uint64(f[0].value[6])<<48 | uint64(f[0].value[7])<<56
			return float64FromBits(bits)
		}
		return nil
	}
	if f, ok := m[5]; ok && len(f) > 0 { // struct_value
		return protoStructToGo(f[0].value)
	}
	if f, ok := m[6]; ok && len(f) > 0 { // list_value
		lm, err := decodeMessage(f[0].value)
		if err != nil {
			return nil
		}
		var out []any
		for _, e := range lm[1] {
			out = append(out, protoValueToGo(e.value))
		}
		if out == nil {
			return []any{}
		}
		return out
	}
	return nil // null_value
}

func protoStructToGo(b []byte) map[string]any {
	m, err := decodeMessage(b)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for _, e := range m[1] {
		entry, err := decodeMessage(e.value)
		if err != nil {
			continue
		}
		key, _ := stringField(entry, 1)
		if key == "" {
			continue
		}
		if vs, ok := entry[2]; ok && len(vs) > 0 {
			out[key] = protoValueToGo(vs[0].value)
		} else {
			out[key] = nil
		}
	}
	return out
}

func float64FromBits(bits uint64) float64 {
	return math.Float64frombits(bits)
}
