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
// clientTools, when non-empty, is the ingress-declared tool list used to
// resolve Cursor built-in names onto a tool the client can run.
func DecodeAgentStream(r io.Reader, writeFrame func([]byte) error, emit func(ir.StreamEvent) error) error {
	return DecodeAgentStreamTools(nil, r, writeFrame, emit)
}

// DecodeAgentStreamTools is DecodeAgentStream with the ingress tool list.
func DecodeAgentStreamTools(clientTools []ir.Tool, r io.Reader, writeFrame func([]byte) error, emit func(ir.StreamEvent) error) error {
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
	var inTok, outTok, cacheRead, cacheWrite int

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
		cacheRead, cacheWrite = ir.ClampCacheTokens(inTok, cacheRead, cacheWrite)
		return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason, InputTokens: inTok, OutputTokens: outTok, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite})
	}

	// clientToolsReady is true when every surfaced call has complete JSON
	// arguments. A name without arguments is not ready: Cursor often sends
	// the name first and the arguments in later updates. A partial fragment
	// is not ready either. An unmatched built-in is not in toolOrder, so it
	// does not make this true.
	clientToolsReady := func() bool {
		if len(toolOrder) == 0 {
			return false
		}
		for _, id := range toolOrder {
			raw := toolCalls[id].args.String()
			if raw == "" || !json.Valid([]byte(raw)) {
				return false
			}
		}
		return true
	}

	// startToolCall registers (or looks up) a call and emits identity-only
	// Start. Ingress encoders read arguments only from EventToolCallDelta, so
	// a one-shot McpArgs snapshot must go out as a Delta. "{}" is the empty
	// map placeholder from mcpArgsMapJSON and must not be emitted: incremental
	// tool_call_started + ptcArgsDelta would otherwise become "{}"+fragments.
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
		substantive := argsJSON != "" && argsJSON != "{}"
		if !tc.started {
			tc.started = true
			if substantive {
				tc.args.WriteString(argsJSON)
			}
			if err := emitStart(); err != nil {
				return err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.index, ToolID: tc.id, ToolName: tc.name}); err != nil {
				return err
			}
			if substantive {
				return emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: tc.index, ToolID: tc.id, ToolName: tc.name, ArgsFrag: argsJSON})
			}
			return nil
		}
		// tool_call_started often precedes the frame that carries args.
		// A second startToolCall for the same id must flush those args as a
		// Delta; dropping them is how ingress clients assembled "{}".
		if substantive && tc.args.Len() == 0 {
			tc.args.WriteString(argsJSON)
			return emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: tc.index, ToolID: tc.id, ToolName: tc.name, ArgsFrag: argsJSON})
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
		data, err := decompressPayload(payload, flags)
		if err != nil {
			return err
		}

		if len(data) > 0 && data[0] == 0x7b && isCursorError(data) {
			return parseCursorError(data)
		}

		top, derr := decodeMessage(data)
		if derr != nil {
			continue
		}

		// interaction_query asks the client to run a Cursor built-in.
		// Built-ins are not surfaced. The query has no ExecServerMessage
		// envelope, so it cannot be rejected here. Ignore the payload and keep
		// reading: ending the turn drops later text, and a missing reply is
		// heartbeats rather than a stream failure. A query that names an MCP
		// tool already in toolOrder ends the turn once that call's args are
		// complete (the client's result returns on the next request).
		if iqs, ok := top[asmInteractionQuery]; ok && len(iqs) > 0 {
			if name, ok := interactionQueryNameOf(iqs[0].value); ok && clientToolsReady() {
				want := decloakToolName(name)
				for _, id := range toolOrder {
					if toolCalls[id].name == want {
						return emitFinish()
					}
				}
			}
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
		// Only MCP exec args are surfaced as IR tool_use. Every other exec
		// oneof is a Cursor built-in: reject it and keep reading so a later
		// declared MCP call or text delta can still be delivered. A matched
		// MCP exec ends this turn. Waiting for turn_ended deadlocks: Cursor
		// heartbeats until the exec is answered, and the answer is the client's
		// next request, not a frame on this stream.
		if exs, ok := top[asmExecServerMessage]; ok && len(exs) > 0 {
			before := len(toolOrder)
			if done, err := handleExecServerMessage(exs[0].value, writeFrame, startToolCall); err != nil {
				return err
			} else if done && len(toolOrder) == before {
				server, _ := decodeMessage(exs[0].value)
				if err := rejectUnmatchedExec(server, writeFrame); err != nil {
					return err
				}
				continue
			} else if done {
				// The empty ack closes a matched MCP exec. It is not the tool
				// output, so Cursor does not send turn_ended. Reading further only
				// receives heartbeats and holds the client until timeout.
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
				// tool_call_started / partial_tool_call: only the MCP ToolCall
				// oneof is client-visible. Built-in oneofs are not surfaced; the
				// matching exec message is rejected and the stream continues.
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
				// with turn_ended. inputTokens is already the inclusive prompt
				// total; cache read/write partition it and must not be added.
				if tes, ok := update[iuTurnEnded]; ok && len(tes) > 0 {
					te, _ := decodeMessage(tes[0].value)
					if v, ok := varintField(te, teInputTokens); ok {
						inTok = usageInt(v)
					}
					if v, ok := varintField(te, teOutputTokens); ok {
						outTok = usageInt(v)
					}
					if v, ok := varintField(te, teCacheReadTokens); ok {
						cacheRead = usageInt(v)
					}
					if v, ok := varintField(te, teCacheWriteTokens); ok {
						cacheWrite = usageInt(v)
					}
					return emitFinish()
				}
			}
			// A client-visible tool call can arrive as an interaction update
			// before, or without, the exec message that used to end the turn.
			// Leaving the stream open after the arguments are complete only
			// receives heartbeats. The client's real result returns on the next
			// request, so this turn ends here.
			if clientToolsReady() {
				return emitFinish()
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
// when the message was an MCP exec (surface and end the turn) or a built-in
// exec that must be rejected without changing toolOrder. done=false means
// the message was a control ack (request context) and the stream continues.
func handleExecServerMessage(server []byte, writeFrame func([]byte) error, startMCP func(id, name, args string) error) (bool, error) {
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
			if err := startMCP(callID, decloakToolName(name), argsJSON); err != nil {
				return false, err
			}
			// Empty McpSuccess closes the exec. The client's real result is not
			// available yet; it is replayed on the next request. A missing ack
			// leaves AgentService on heartbeats until the stream times out.
			if writeFrame != nil {
				if err := writeFrame(encodeMCPAck(id, execID)); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		return false, nil
	}

	// Any other exec args oneof (shell, read, grep, ...) is a Cursor
	// built-in. Do not surface it. done=true with no toolOrder change makes
	// the caller reject the exec and keep reading.
	if execResultField(m) != 0 {
		return true, nil
	}
	return false, nil
}

// rejectUnmatchedExec writes ExecClientMessage with the built-in result's
// rejected variant. Cursor can then call a declared MCP tool instead of
// waiting, or ending the run, on a tool the client cannot execute.
func rejectUnmatchedExec(server map[int][]field, writeFrame func([]byte) error) error {
	if writeFrame == nil {
		return nil
	}
	fieldNum := execResultField(server)
	if fieldNum == 0 {
		return nil
	}
	id, _ := varintField(server, esmID)
	execID, _ := stringField(server, esmExecID)
	rejected := encodeField(execResultRejected, wireLen,
		encodeField(execRejectedError, wireLen, []byte("not available; use the declared MCP tools")))
	client := concatBytes(
		encodeField(ecmID, wireVarint, id),
		encodeField(ecmExecID, wireLen, execID),
		encodeField(fieldNum, wireLen, rejected),
	)
	return writeFrame(wrapConnectFrame(encodeField(2, wireLen, client), false))
}

func execResultField(server map[int][]field) int {
	for num := range server {
		if execControlFields[num] {
			continue
		}
		return num
	}
	return 0
}

// encodeMCPAck is ExecClientMessage{1: id, 15: exec_id, 11: McpResult{1: McpSuccess{}}}.
// The empty success tells AgentService the exec was accepted. It is not the
// tool output; that arrives with the client's next request.
func encodeMCPAck(id uint64, execID string) []byte {
	result := encodeField(ecmMCPResult, wireLen,
		encodeField(mcpResultSuccess, wireLen, []byte{}))
	client := concatBytes(
		encodeField(ecmID, wireVarint, id),
		encodeField(ecmExecID, wireLen, execID),
		result,
	)
	return wrapConnectFrame(encodeField(2, wireLen, client), false)
}

// interactionQueryName is the MCP-comparable name for each InteractionQuery
// oneof (agent.v1). Unknown field numbers are ignored: a built-in query is
// never surfaced, and only a name match against an already-started MCP call
// can end the turn.
var interactionQueryName = map[int]string{
	2:  "web_search",
	3:  "ask_question",
	4:  "switch_mode",
	7:  "create_plan",
	8:  "setup_vm_environment",
	9:  "web_fetch",
	10: "pr_management",
	11: "mcp_auth",
	12: "generate_image",
	13: "replace_env",
	14: "connect_scm",
}

// interactionQueryNameOf returns the named oneof of an InteractionQuery.
// ok is false when the message has no named payload. Args are not decoded:
// the query is not a client tool call.
func interactionQueryNameOf(query []byte) (name string, ok bool) {
	m := decodeOrEmpty(query)
	for num, fs := range m {
		if num == iqID || len(fs) == 0 || fs[0].wireType != wireLen {
			continue
		}
		name = interactionQueryName[num]
		if name == "" {
			continue
		}
		return name, true
	}
	return "", false
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

func usageInt(v uint64) int {
	if v > math.MaxInt {
		return math.MaxInt
	}
	return int(v)
}
