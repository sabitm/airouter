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
	"strconv"
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
	sawTurnEnded := false
	mcpHandoff := false

	type tcall struct {
		index        int
		id           string
		name         string
		args         strings.Builder
		started      bool
		provisional  bool
		emittedArgs  bool
		pendingFrags strings.Builder
	}
	toolCalls := map[string]*tcall{}
	toolOrder := []string{}
	seenExec := map[string]bool{}
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

	emitArgsOnce := func(tc *tcall, argsJSON string) error {
		if tc.emittedArgs || argsJSON == "" {
			return nil
		}
		tc.emittedArgs = true
		tc.args.Reset()
		tc.args.WriteString(argsJSON)
		return emit(ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: tc.index, ToolID: tc.id, ToolName: tc.name, ArgsFrag: argsJSON})
	}

	emitFinish := func() error {
		// Finalize tool calls that never completed (stream cut short).
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if tc.provisional || (tc.pendingFrags.Len() > 0 && !json.Valid([]byte(tc.pendingFrags.String()))) {
				return ir.ProtocolError("cursor: incomplete tool arguments")
			}
			if !tc.started {
				tc.started = true
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.index, ToolID: tc.id, ToolName: tc.name}); err != nil {
					return err
				}
			}
			if !tc.emittedArgs && tc.pendingFrags.Len() > 0 {
				if err := emitArgsOnce(tc, tc.pendingFrags.String()); err != nil {
					return err
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
	// does not make this true. Provisional "{}" is not complete.
	clientToolsReady := func() bool {
		if len(toolOrder) == 0 {
			return false
		}
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if tc.provisional {
				return false
			}
			raw := tc.args.String()
			if raw == "" || !json.Valid([]byte(raw)) {
				return false
			}
		}
		return true
	}

	// ensureToolCall registers a call and emits identity-only Start. "{}" from
	// tool_call_started is provisional: Cursor sends that empty map before
	// argument fragments. An authoritative exec snapshot may carry the same
	// bytes as a real no-arg call, and that "{}" must be emitted once.
	ensureToolCall := func(id, name string, provisional bool) (*tcall, error) {
		if id == "" || name == "" {
			return nil, nil
		}
		tc, seen := toolCalls[id]
		if !seen {
			tc = &tcall{index: len(toolOrder), id: id, name: name, provisional: provisional}
			toolCalls[id] = tc
			toolOrder = append(toolOrder, id)
		}
		if !tc.started {
			tc.started = true
			if err := emitStart(); err != nil {
				return nil, err
			}
			if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.index, ToolID: tc.id, ToolName: tc.name}); err != nil {
				return nil, err
			}
		}
		return tc, nil
	}

	// acceptToolArgs applies one argument snapshot. Authoritative snapshots
	// replace a provisional placeholder. Fragments stay buffered until the
	// joined value is valid JSON, then emit exactly once.
	acceptToolArgs := func(id, name, argsJSON string, authoritative bool) error {
		tc, err := ensureToolCall(id, name, !authoritative && (argsJSON == "" || argsJSON == "{}"))
		if err != nil || tc == nil {
			return err
		}
		if authoritative {
			if !json.Valid([]byte(argsJSON)) {
				return ir.ProtocolError("cursor: invalid tool arguments")
			}
			tc.provisional = false
			tc.pendingFrags.Reset()
			return emitArgsOnce(tc, argsJSON)
		}
		if tc.emittedArgs {
			return nil
		}
		if argsJSON == "" || argsJSON == "{}" {
			tc.provisional = true
			return nil
		}
		tc.pendingFrags.WriteString(argsJSON)
		joined := tc.pendingFrags.String()
		if !json.Valid([]byte(joined)) {
			tc.provisional = true
			return nil
		}
		tc.provisional = false
		return emitArgsOnce(tc, joined)
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

		// A flagged end-stream is validated before any JSON error scan. Official
		// success is {} or {"error":null}; a metadata key named "error" is not a
		// failure. Unflagged historical Cursor JSON error frames still use the
		// error parser. Bodies are not logged.
		if flags&flagTrailer != 0 {
			if err := endStreamError(data); err != nil {
				return err
			}
			if !sawTurnEnded {
				return ir.ProtocolError("cursor: stream ended without turnEnded")
			}
			// EndStreamResponse ends the Connect stream. Another envelope is a
			// protocol error, so success is not emitted until that check passes.
			if _, _, err := readFrame(r); err != io.EOF {
				if err == nil {
					return ir.ProtocolError("cursor: extra envelope after end-stream")
				}
				return err
			}
			return emitFinish()
		}
		if len(data) > 0 && data[0] == 0x7b && isCursorError(data) {
			return parseCursorError(data)
		}
		top, derr := decodeMessage(data)
		if derr != nil {
			return ir.ProtocolError("cursor: malformed protobuf message")
		}

		// interaction_query asks the client to run a Cursor built-in.
		// Built-ins are not surfaced. The query has no ExecServerMessage
		// envelope, so it cannot be rejected here. Ignore the payload and keep
		// reading: ending the turn drops later text, and a missing reply is
		// heartbeats rather than a stream failure. A query that names an MCP
		// tool already in toolOrder ends the turn once that call's args are
		// complete (the client's result returns on the next request).
		if iqs, ok := top[asmInteractionQuery]; ok && len(iqs) > 0 {
			if iqs[0].wireType != wireLen {
				return ir.ProtocolError("cursor: malformed interaction query")
			}
			name, recognized, qerr := interactionQueryNameOf(iqs[0].value)
			if qerr != nil {
				return qerr
			}
			if recognized && clientToolsReady() {
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
			if kvs[0].wireType != wireLen {
				return ir.ProtocolError("cursor: malformed kv request")
			}
			reply, kerr := encodeKVReply(kvs[0].value)
			if kerr != nil {
				return kerr
			}
			if reply != nil {
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
			if exs[0].wireType != wireLen {
				return ir.ProtocolError("cursor: malformed exec request")
			}
			execKey, keyErr := execSeenKey(exs[0].value)
			if keyErr != nil {
				return keyErr
			}
			if execKey != "" {
				if seenExec[execKey] {
					continue
				}
				seenExec[execKey] = true
			}
			outcome, server, err := handleExecServerMessage(exs[0].value, writeFrame, acceptToolArgs)
			if err != nil {
				return err
			}
			switch outcome {
			case execBuiltinRejected:
				if err := rejectUnmatchedExec(server, writeFrame); err != nil {
					return err
				}
				continue
			case execMCPHandoff:
				// The empty ack closes a matched MCP exec. It is not the tool
				// output, so Cursor does not send turn_ended. Reading further only
				// receives heartbeats and holds the client until timeout.
				mcpHandoff = true
				return emitFinish()
			}
		}

		// interaction_update: the actual content stream.
		if ius, ok := top[asmInteractionUpdate]; ok {
			for _, iu := range ius {
				update, err := decodeMessage(iu.value)
				if err != nil {
					return ir.ProtocolError("cursor: malformed protobuf message")
				}
				if err := emitStart(); err != nil {
					return err
				}
				// text_delta
				if tds, ok := update[iuTextDelta]; ok && len(tds) > 0 {
					text, terr := textDeltaOf(tds[0])
					if terr != nil {
						return terr
					}
					if text != "" {
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
					id, name, args, ok, terr := extractMCPToolCall(tcss[0])
					if terr != nil {
						return terr
					}
					if ok {
						if err := acceptToolArgs(id, name, args, false); err != nil {
							return err
						}
					}
				}
				if ptcs, ok := update[iuPartialToolCall]; ok && len(ptcs) > 0 {
					if ptcs[0].wireType != wireLen {
						return ir.ProtocolError("cursor: malformed partial tool call")
					}
					p, perr := decodeMessage(ptcs[0].value)
					if perr != nil {
						return ir.ProtocolError("cursor: malformed partial tool call")
					}
					id, idOK := stringField(p, ptcCallID)
					if !idOK && len(p[ptcCallID]) > 0 {
						return ir.ProtocolError("cursor: malformed partial tool call")
					}
					delta, deltaOK := stringField(p, ptcArgsDelta)
					if !deltaOK && len(p[ptcArgsDelta]) > 0 {
						return ir.ProtocolError("cursor: malformed partial tool call")
					}
					if delta != "" && id != "" {
						if tc, seen := toolCalls[id]; seen && !tc.emittedArgs {
							if err := acceptToolArgs(tc.id, tc.name, delta, false); err != nil {
								return err
							}
						}
					}
					if id == "" || toolCalls[id] == nil {
						cid, name, args, ok, terr := extractMCPToolCall(ptcs[0])
						if terr != nil {
							return terr
						}
						if ok {
							if err := acceptToolArgs(cid, name, args, false); err != nil {
								return err
							}
						}
					}
				}
				// token_delta: running output count; authoritative usage arrives
				// with turn_ended. inputTokens is already the inclusive prompt
				// total; cache read/write partition it and must not be added.
				if tes, ok := update[iuTurnEnded]; ok && len(tes) > 0 {
					in, out, read, write, terr := turnEndedUsage(tes[0])
					if terr != nil {
						return terr
					}
					inTok, outTok, cacheRead, cacheWrite = in, out, read, write
					sawTurnEnded = true
					continue
				}
			}
			// A client-visible tool call can arrive as an interaction update
			// before, or without, the exec message that used to end the turn.
			// Leaving the stream open after the arguments are complete only
			// receives heartbeats. The client's real result returns on the next
			// request, so this MCP handoff ends here. Text-only turns still wait
			// for the Connect end-stream frame.
			if len(toolOrder) > 0 && clientToolsReady() {
				mcpHandoff = true
				return emitFinish()
			}
		}
	}

	if mcpHandoff {
		return emitFinish()
	}
	if !sawTurnEnded {
		return ir.ProtocolError("cursor: stream ended without turnEnded")
	}
	return ir.ProtocolError("cursor: missing end-stream frame")
}

func textDeltaOf(f field) (string, error) {
	if f.wireType != wireLen {
		return "", ir.ProtocolError("cursor: malformed text delta")
	}
	m, err := decodeMessage(f.value)
	if err != nil {
		return "", ir.ProtocolError("cursor: malformed text delta")
	}
	text, ok := stringField(m, tdText)
	if !ok && len(m[tdText]) > 0 {
		return "", ir.ProtocolError("cursor: malformed text delta")
	}
	return text, nil
}

func turnEndedUsage(f field) (inTok, outTok, cacheRead, cacheWrite int, err error) {
	if f.wireType != wireLen {
		return 0, 0, 0, 0, ir.ProtocolError("cursor: malformed turn ended")
	}
	te, err := decodeMessage(f.value)
	if err != nil {
		return 0, 0, 0, 0, ir.ProtocolError("cursor: malformed turn ended")
	}
	read := func(num int) (int, error) {
		fs := te[num]
		if len(fs) == 0 {
			return 0, nil
		}
		if fs[0].wireType != wireVarint {
			return 0, ir.ProtocolError("cursor: malformed turn ended")
		}
		v, ok := varintField(te, num)
		if !ok {
			return 0, ir.ProtocolError("cursor: malformed turn ended")
		}
		return usageInt(v), nil
	}
	if inTok, err = read(teInputTokens); err != nil {
		return 0, 0, 0, 0, err
	}
	if outTok, err = read(teOutputTokens); err != nil {
		return 0, 0, 0, 0, err
	}
	if cacheRead, err = read(teCacheReadTokens); err != nil {
		return 0, 0, 0, 0, err
	}
	if cacheWrite, err = read(teCacheWriteTokens); err != nil {
		return 0, 0, 0, 0, err
	}
	return inTok, outTok, cacheRead, cacheWrite, nil
}

// encodeKVReply builds the KvClientMessage for one KvServerMessage: get ->
// empty GetBlobResult (blob not found), set -> empty SetBlobResult (success).
// A malformed known envelope is an error. An unrecognized variant is ignored.
func encodeKVReply(server []byte) ([]byte, error) {
	m, err := decodeMessage(server)
	if err != nil {
		return nil, ir.ProtocolError("cursor: malformed kv request")
	}
	id, idOK := varintField(m, kvsID)
	if !idOK && len(m[kvsID]) > 0 {
		return nil, ir.ProtocolError("cursor: malformed kv request")
	}
	for _, num := range []int{kvsGetBlobArgs, kvsSetBlobArgs} {
		if len(m[num]) > 0 && m[num][0].wireType != wireLen {
			return nil, ir.ProtocolError("cursor: malformed kv request")
		}
	}
	switch {
	case m[kvsGetBlobArgs] != nil:
		client := concatBytes(
			encodeField(kvcID, wireVarint, id),
			encodeField(kvcGetBlobRes, wireLen, []byte{}),
		)
		return wrapConnectFrame(encodeField(acmKVClientMessage, wireLen, client), false), nil
	case m[kvsSetBlobArgs] != nil:
		client := concatBytes(
			encodeField(kvcID, wireVarint, id),
			encodeField(kvcSetBlobRes, wireLen, []byte{}),
		)
		return wrapConnectFrame(encodeField(acmKVClientMessage, wireLen, client), false), nil
	default:
		return nil, nil
	}
}

func execSeenKey(server []byte) (string, error) {
	m, err := decodeMessage(server)
	if err != nil {
		return "", ir.ProtocolError("cursor: malformed exec request")
	}
	if len(m[esmExecID]) > 0 {
		id, ok := stringField(m, esmExecID)
		if !ok {
			return "", ir.ProtocolError("cursor: malformed exec request")
		}
		if id != "" {
			return "e:" + id, nil
		}
	}
	if len(m[esmID]) > 0 {
		id, ok := varintField(m, esmID)
		if !ok {
			return "", ir.ProtocolError("cursor: malformed exec request")
		}
		return "i:" + strconv.FormatUint(id, 10), nil
	}
	return "", nil
}

// execOutcome is the explicit result of one ExecServerMessage. Control
// replies continue the stream. A built-in rejection is not an MCP handoff.
type execOutcome int

const (
	execControlHandled execOutcome = iota
	execBuiltinRejected
	execMCPHandoff
)

// handleExecServerMessage services one ExecServerMessage.
func handleExecServerMessage(server []byte, writeFrame func([]byte) error, acceptMCP func(id, name, args string, authoritative bool) error) (execOutcome, map[int][]field, error) {
	m, err := decodeMessage(server)
	if err != nil {
		return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
	}
	id, idOK := varintField(m, esmID)
	if !idOK && len(m[esmID]) > 0 {
		return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
	}
	execID, execOK := stringField(m, esmExecID)
	if !execOK && len(m[esmExecID]) > 0 {
		return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
	}

	if ctxArgs := m[esmRequestContextArgs]; len(ctxArgs) > 0 {
		if ctxArgs[0].wireType != wireLen {
			return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
		}
		if _, err := decodeMessage(ctxArgs[0].value); err != nil {
			return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
		}
		if writeFrame == nil {
			return execControlHandled, m, nil
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
		if err := writeFrame(wrapConnectFrame(encodeField(acmExecClientMessage, wireLen, client), false)); err != nil {
			return execControlHandled, m, err
		}
		return execControlHandled, m, nil
	}

	if args, ok := m[esmMCPArgs]; ok && len(args) > 0 {
		if args[0].wireType != wireLen {
			return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
		}
		am, err := decodeMessage(args[0].value)
		if err != nil {
			return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
		}
		callID, callOK := stringField(am, maCallID)
		if !callOK && len(am[maCallID]) > 0 {
			return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
		}
		name, nameOK := stringField(am, maToolName)
		if !nameOK && len(am[maToolName]) > 0 {
			return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
		}
		if name == "" {
			alt, altOK := stringField(am, maName)
			if !altOK && len(am[maName]) > 0 {
				return execControlHandled, nil, ir.ProtocolError("cursor: malformed exec request")
			}
			name = alt
		}
		argsJSON, aerr := mcpArgsMapJSON(am)
		if aerr != nil {
			return execControlHandled, nil, aerr
		}
		if callID != "" && name != "" {
			// The exec snapshot is authoritative, including a real no-arg "{}".
			// It replaces a provisional tool_call_started placeholder for the
			// same id instead of concatenating onto it.
			if err := acceptMCP(callID, decloakToolName(name), argsJSON, true); err != nil {
				return execControlHandled, m, err
			}
			// Empty McpSuccess closes the exec. The client's real result is not
			// available yet; it is replayed on the next request. A missing ack
			// leaves AgentService on heartbeats until the stream times out.
			if writeFrame != nil {
				if err := writeFrame(encodeMCPAck(id, execID)); err != nil {
					return execControlHandled, m, err
				}
			}
			return execMCPHandoff, m, nil
		}
		return execControlHandled, m, nil
	}

	// Any other exec args oneof (shell, read, grep, ...) is a Cursor
	// built-in. Do not surface it. The caller writes the rejection and keeps
	// reading.
	if execArgField(m) != 0 {
		return execBuiltinRejected, m, nil
	}
	return execControlHandled, m, nil
}

// rejectUnmatchedExec writes the official failure variant for one recognized
// built-in exec. A recognized exec with no failure variant gets
// ExecClientThrow. Unknown fields are never copied into a fabricated result.
// Cursor can then call a declared MCP tool instead of waiting on a tool the
// client cannot execute.
func rejectUnmatchedExec(server map[int][]field, writeFrame func([]byte) error) error {
	if writeFrame == nil {
		return nil
	}
	argField := execArgField(server)
	if argField == 0 {
		return nil
	}
	id, _ := varintField(server, esmID)
	execID, _ := stringField(server, esmExecID)
	resultField, ok := execArgResult[argField]
	if !ok {
		return writeFrame(encodeExecThrow(id, execUnavailableReason))
	}
	path, command, url, aerr := execArgText(server, argField)
	if aerr != nil {
		return aerr
	}
	body, ok := encodeExecRejection(resultField, path, command, url)
	if !ok {
		return writeFrame(encodeExecThrow(id, execUnavailableReason))
	}
	client := concatBytes(
		encodeField(ecmID, wireVarint, id),
		encodeField(ecmExecID, wireLen, execID),
		encodeField(resultField, wireLen, body),
	)
	return writeFrame(wrapConnectFrame(encodeField(acmExecClientMessage, wireLen, client), false))
}

func execArgField(server map[int][]field) int {
	// A known args oneof wins even when the same message also carries an
	// unrecognized field. Map iteration is not ordered, so the known winner
	// is the lowest known field number. Unknown-only messages use the lowest
	// unknown field so the throw id stays deterministic.
	known := 0
	unknown := 0
	for num := range server {
		if execControlFields[num] {
			continue
		}
		if _, ok := execArgResult[num]; ok {
			if known == 0 || num < known {
				known = num
			}
			continue
		}
		if unknown == 0 || num < unknown {
			unknown = num
		}
	}
	if known != 0 {
		return known
	}
	return unknown
}

// execIdentityField is the schema-defined string a rejection result copies.
// kind is path, command, or url. num 0 means the result spec does not need an
// args string, so field 1 is not read: RecordScreenArgs.mode and
// WriteShellStdinArgs.shell_id are scalars, and AgentStoreConflictArgs field 1
// is a message.
func execIdentityField(argField int) (num int, kind string) {
	switch argField {
	case 2, 14, 16, 46, 52:
		return 1, "command"
	case 5:
		return 2, "path"
	case 18:
		return 2, "path"
	case 20, 43:
		return 1, "url"
	case 3, 4, 7, 8, 9, 29, 40, 45, 51:
		return 1, "path"
	default:
		return 0, ""
	}
}

func execArgText(server map[int][]field, argField int) (path, command, url string, err error) {
	num, kind := execIdentityField(argField)
	if num == 0 {
		return "", "", "", nil
	}
	fs := server[argField]
	if len(fs) == 0 {
		return "", "", "", nil
	}
	if fs[0].wireType != wireLen {
		return "", "", "", ir.ProtocolError("cursor: malformed exec request")
	}
	args, derr := decodeMessage(fs[0].value)
	if derr != nil {
		return "", "", "", ir.ProtocolError("cursor: malformed exec request")
	}
	if len(args[num]) == 0 {
		return "", "", "", nil
	}
	got, ok := stringField(args, num)
	if !ok {
		return "", "", "", ir.ProtocolError("cursor: malformed exec request")
	}
	switch kind {
	case "command":
		command = got
	case "url":
		url = got
	default:
		path = got
	}
	return path, command, url, nil
}

func encodeExecRejection(resultField int, path, command, url string) ([]byte, bool) {
	spec, ok := execResultSpecs[resultField]
	if !ok {
		return nil, false
	}
	reason := execUnavailableReason
	if spec.enum {
		return encodeField(spec.outer, wireVarint, spec.enumVal), true
	}
	if spec.reason == 0 {
		return encodeField(spec.outer, wireLen, reason), true
	}
	identity := path
	switch {
	case spec.command:
		identity = command
	case spec.url:
		identity = url
	}
	var inner []byte
	if spec.path != 0 {
		inner = append(inner, encodeField(spec.path, wireLen, identity)...)
	}
	inner = append(inner, encodeField(spec.reason, wireLen, reason)...)
	return encodeField(spec.outer, wireLen, inner), true
}

func encodeExecThrow(id uint64, reason string) []byte {
	throw := concatBytes(
		encodeField(ectID, wireVarint, id),
		encodeField(ectError, wireLen, reason),
	)
	control := encodeField(eccThrow, wireLen, throw)
	return wrapConnectFrame(encodeField(acmExecClientControl, wireLen, control), false)
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
// ok is false when the message has no named payload. Unknown fields are
// ignored regardless of wire type. Only a named variant is validated as a
// length-delimited message. Args are not decoded: the query is not a client
// tool call.
func interactionQueryNameOf(query []byte) (name string, ok bool, err error) {
	m, derr := decodeMessage(query)
	if derr != nil {
		return "", false, ir.ProtocolError("cursor: malformed interaction query")
	}
	for num, fs := range m {
		named := interactionQueryName[num]
		if named == "" || len(fs) == 0 {
			continue
		}
		if fs[0].wireType != wireLen {
			return "", false, ir.ProtocolError("cursor: malformed interaction query")
		}
		if _, derr := decodeMessage(fs[0].value); derr != nil {
			return "", false, ir.ProtocolError("cursor: malformed interaction query")
		}
		return named, true, nil
	}
	return "", false, nil
}

// toolCallOneofFields are the official ToolCall oneof field numbers. Fields
// outside this set are unknown and are skipped before any wire-type check.
// Metadata on the same message is not a oneof and is not nested protobuf.
var toolCallOneofFields = map[int]bool{
	1: true, 3: true, 4: true, 5: true, 8: true, 9: true, 10: true, 12: true,
	13: true, 14: true, 15: true, 16: true, 17: true, 18: true, 19: true,
	20: true, 21: true, 22: true, 23: true, 24: true, 25: true, 28: true,
	29: true, 30: true, 31: true, 32: true, 33: true, 34: true, 35: true,
	36: true, 37: true, 38: true, 39: true, 40: true, 41: true, 42: true,
	43: true, 44: true, 45: true, 46: true, 48: true, 49: true, 50: true,
	51: true, 52: true, 53: true, 55: true, 56: true, 58: true, 61: true,
	62: true, 63: true, 64: true, 65: true, 66: true, 67: true, 68: true,
	69: true, 70: true, 71: true, 72: true, 73: true, 74: true, 75: true,
	76: true, 77: true, 78: true, 79: true, 80: true,
}

// toolCallMetaFields are ToolCall metadata, not oneof variants. Field 54 is
// repeated HookAdditionalContext. Field 57 is an optional string. Fields 59
// and 60 are optional uint64 timestamps.
var toolCallMetaFields = map[int]int{
	54: wireLen,
	57: wireLen,
	59: wireVarint,
	60: wireVarint,
}

// extractMCPToolCall pulls the MCP variant out of a ToolCallStartedUpdate:
// {1: call_id, 2: ToolCall{15: McpToolCall{1: McpArgs{...}}}, 3: model_call_id}.
func extractMCPToolCall(update field) (id, name, argsJSON string, ok bool, err error) {
	if update.wireType != wireLen {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	m, derr := decodeMessage(update.value)
	if derr != nil {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	callID, callOK := stringField(m, tcsCallID)
	if !callOK && len(m[tcsCallID]) > 0 {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	tcs := m[tcsToolCall]
	if len(tcs) == 0 {
		return "", "", "", false, nil
	}
	if tcs[0].wireType != wireLen {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	tc, derr := decodeMessage(tcs[0].value)
	if derr != nil {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	if err := validateToolCallFields(tc); err != nil {
		return "", "", "", false, err
	}
	mtcs := tc[tcMCPTOolCall]
	if len(mtcs) == 0 {
		// A non-MCP tool oneof is a Cursor built-in. Ignore it, including when
		// its nested tool name matches a declared MCP tool.
		return "", "", "", false, nil
	}
	if mtcs[0].wireType != wireLen {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	mtc, derr := decodeMessage(mtcs[0].value)
	if derr != nil {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	mas := mtc[mtcArgs]
	if len(mas) == 0 {
		return "", "", "", false, nil
	}
	if mas[0].wireType != wireLen {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	am, derr := decodeMessage(mas[0].value)
	if derr != nil {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	}
	if n, nOK := stringField(am, maToolName); !nOK && len(am[maToolName]) > 0 {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	} else if n != "" {
		name = n
	}
	if name == "" {
		if n, nOK := stringField(am, maName); !nOK && len(am[maName]) > 0 {
			return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
		} else {
			name = n
		}
	}
	if c, cOK := stringField(am, maCallID); !cOK && len(am[maCallID]) > 0 {
		return "", "", "", false, ir.ProtocolError("cursor: malformed tool call")
	} else if c != "" {
		callID = c
	}
	argsJSON, err = mcpArgsMapJSON(am)
	if err != nil {
		return "", "", "", false, err
	}
	if callID == "" || name == "" {
		return "", "", "", false, nil
	}
	return callID, decloakToolName(name), argsJSON, true, nil
}

// validateToolCallFields checks known oneof and metadata fields. Unknown
// fields are skipped before any assumed wire type is checked. A known oneof
// must be a nested message. Metadata keeps its official wire type: string
// and hook context are length-delimited, timestamps are varints.
func validateToolCallFields(tc map[int][]field) error {
	for num, fs := range tc {
		if len(fs) == 0 {
			continue
		}
		want, meta := toolCallMetaFields[num]
		switch {
		case toolCallOneofFields[num]:
			if fs[0].wireType != wireLen {
				return ir.ProtocolError("cursor: malformed tool call")
			}
			if _, err := decodeMessage(fs[0].value); err != nil {
				return ir.ProtocolError("cursor: malformed tool call")
			}
		case meta:
			for _, f := range fs {
				if f.wireType != want {
					return ir.ProtocolError("cursor: malformed tool call")
				}
				if num == 54 {
					if _, err := decodeMessage(f.value); err != nil {
						return ir.ProtocolError("cursor: malformed tool call")
					}
				}
			}
		}
	}
	return nil
}

// mcpArgsMapJSON renders McpArgs.args (field 2, map<string, google.protobuf.
// Value>) as a JSON object string. An absent map renders as "{}". An empty
// string key is valid and is preserved when the entry has a value.
func mcpArgsMapJSON(am map[int][]field) (string, error) {
	entries := am[maArgs]
	if len(entries) == 0 {
		return "{}", nil
	}
	out := map[string]any{}
	for _, e := range entries {
		key, val, err := protoMapEntry(e)
		if err != nil {
			return "", err
		}
		out[key] = val
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", ir.ProtocolError("cursor: malformed tool arguments")
	}
	return string(encoded), nil
}

func protoMapEntry(e field) (string, any, error) {
	if e.wireType != wireLen {
		return "", nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	entry, err := decodeMessage(e.value)
	if err != nil {
		return "", nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	key, ok := stringField(entry, 1)
	if !ok && len(entry[1]) > 0 {
		return "", nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	vs, ok := entry[2]
	if !ok || len(vs) == 0 {
		return "", nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	if vs[0].wireType != wireLen {
		return "", nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	val, err := protoValueToGo(vs[0].value)
	if err != nil {
		return "", nil, err
	}
	return key, val, nil
}

// protoValueToGo converts a google.protobuf.Value message. Official Value
// requires one selected kind. Explicit null_value (field 1, enum 0) is JSON
// null. An empty or unknown-only Value is invalid. Unknown fields beside a
// valid kind are ignored. NaN and Inf are rejected.
func protoValueToGo(b []byte) (any, error) {
	m, err := decodeMessage(b)
	if err != nil {
		return nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	if f, ok := m[1]; ok && len(f) > 0 { // null_value
		if f[0].wireType != wireVarint {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		v, ok := varintField(m, 1)
		if !ok || v != 0 {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		return nil, nil
	}
	if f, ok := m[3]; ok && len(f) > 0 { // string_value
		if f[0].wireType != wireLen {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		return string(f[0].value), nil
	}
	if f, ok := m[4]; ok && len(f) > 0 { // bool_value
		if f[0].wireType != wireVarint {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		v, ok := varintField(m, 4)
		if !ok {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		return v != 0, nil
	}
	if f, ok := m[2]; ok && len(f) > 0 { // number_value (fixed64 double)
		if f[0].wireType != wireFixed64 || len(f[0].value) != 8 {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		bits := uint64(f[0].value[0]) | uint64(f[0].value[1])<<8 | uint64(f[0].value[2])<<16 | uint64(f[0].value[3])<<24 |
			uint64(f[0].value[4])<<32 | uint64(f[0].value[5])<<40 | uint64(f[0].value[6])<<48 | uint64(f[0].value[7])<<56
		n := float64FromBits(bits)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		return n, nil
	}
	if f, ok := m[5]; ok && len(f) > 0 { // struct_value
		if f[0].wireType != wireLen {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		return protoStructToGo(f[0].value)
	}
	if f, ok := m[6]; ok && len(f) > 0 { // list_value
		if f[0].wireType != wireLen {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		lm, err := decodeMessage(f[0].value)
		if err != nil {
			return nil, ir.ProtocolError("cursor: malformed tool arguments")
		}
		out := []any{}
		for _, e := range lm[1] {
			if e.wireType != wireLen {
				return nil, ir.ProtocolError("cursor: malformed tool arguments")
			}
			item, err := protoValueToGo(e.value)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	}
	return nil, ir.ProtocolError("cursor: malformed tool arguments")
}

func protoStructToGo(b []byte) (map[string]any, error) {
	m, err := decodeMessage(b)
	if err != nil {
		return nil, ir.ProtocolError("cursor: malformed tool arguments")
	}
	out := map[string]any{}
	for _, e := range m[1] {
		key, val, err := protoMapEntry(e)
		if err != nil {
			return nil, err
		}
		out[key] = val
	}
	return out, nil
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
