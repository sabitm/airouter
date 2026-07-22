package cursor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"airouter/internal/proxy/ir"
)

// DecodeStream reads Cursor ChatService Connect-RPC frames and emits IR stream
// events. Each frame holds either a ClientSideToolV2Call (field 1) or a
// StreamUnifiedChatResponse (field 2). Tool-call argument fragments are
// reassembled by tool id; composer models emit the post-</think> slice of the
// thinking field as visible text. JSON error frames (resource_exhausted /
// rate-limit) surface as errors but, if content already streamed, terminate
// cleanly rather than overwrite a partial response.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	started := false
	msgID := ""
	sawTools := false

	// tool-call reassembly by id: index, name, accumulated args, started flag.
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

	// Composer models embed their visible answer in the thinking field after the
	// last </think> tag. Buffer thinking and emit the post-tag slice incrementally;
	// the extraction is a no-op when no </think> tag appears (non-composer streams).
	var thinkingBuf strings.Builder
	emittedComposer := 0

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
		// Finalize any tool calls that never saw is_last (stream ended early).
		needTools := false
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if !tc.started {
				tc.started = true
				if err := emit(ir.StreamEvent{Kind: ir.EventToolCallStart, Index: tc.index, ToolID: tc.id, ToolName: tc.name}); err != nil {
					return err
				}
			}
			needTools = true
		}
		if needTools {
			stopReason = ir.StopToolUse
		}
		if err := emitStart(); err != nil {
			return err
		}
		return emit(ir.StreamEvent{Kind: ir.EventFinish, StopReason: stopReason})
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

		// JSON error frame: starts with '{'. Surface as error; if content
		// already streamed, finish cleanly instead of overwriting it.
		if len(data) > 0 && data[0] == 0x7b {
			if isCursorError(data) {
				if started || sawTools || len(toolOrder) > 0 {
					return emitFinish()
				}
				return parseCursorError(data)
			}
		}

		top, derr := decodeMessage(data)
		if derr != nil {
			// Skip undecodable frames (schema drift) rather than abort a stream.
			continue
		}
		if err := emitStart(); err != nil {
			return err
		}

		// Field 1: ClientSideToolV2Call.
		if calls, ok := top[respToolCall]; ok {
			for _, cf := range calls {
				tc, ok := extractToolCall(cf.value)
				if !ok {
					continue
				}
				sawTools = true
				existing, seen := toolCalls[tc.id]
				if !seen {
					existing = &tcall{index: len(toolOrder), id: tc.id, name: tc.name}
					toolCalls[tc.id] = existing
					toolOrder = append(toolOrder, tc.id)
				}
				if tc.args != "" {
					existing.args.WriteString(tc.args)
				}
				if !existing.started {
					existing.started = true
					if err := emit(ir.StreamEvent{
						Kind: ir.EventToolCallStart, Index: existing.index,
						ToolID: existing.id, ToolName: existing.name,
					}); err != nil {
						return err
					}
					// Emit the args accumulated so far as the first delta.
					if existing.args.Len() > 0 {
						if err := emit(ir.StreamEvent{
							Kind: ir.EventToolCallDelta, Index: existing.index,
							ToolID: existing.id, ToolName: existing.name,
							ArgsFrag: existing.args.String(),
						}); err != nil {
							return err
						}
					}
				} else if tc.args != "" {
					// Subsequent fragment: emit only the new piece.
					if err := emit(ir.StreamEvent{
						Kind: ir.EventToolCallDelta, Index: existing.index,
						ToolID: existing.id, ToolName: existing.name,
						ArgsFrag: tc.args,
					}); err != nil {
						return err
					}
				}
			}
		}

		// Field 2: StreamUnifiedChatResponse.
		if resps, ok := top[respResponse]; ok {
			for _, rf := range resps {
				text, thinking := extractTextAndThinking(rf.value)
				if text != "" {
					if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: text}); err != nil {
						return err
					}
				}
				if thinking != "" {
					thinkingBuf.WriteString(thinking)
					visible := visibleComposerContentFromThinking(thinkingBuf.String())
					if len(visible) > emittedComposer {
						delta := visible[emittedComposer:]
						emittedComposer = len(visible)
						if err := emit(ir.StreamEvent{Kind: ir.EventTextDelta, Text: delta}); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	return emitFinish()
}

// toolCallInfo is a decoded tool call fragment.
type toolCallInfo struct {
	id   string
	name string
	args string
}

// extractToolCall decodes a ClientSideToolV2Call. The tool id is the first line
// of field 3 (Cursor appends a model_call_id after "\n"). The tool name prefers
// the nested mcp_params.tools[0].name (the real tool name) over the flat field 9.
// Args prefer mcp_params.tools[0].params over field 10 (raw_args).
func extractToolCall(data []byte) (toolCallInfo, bool) {
	m, err := decodeMessage(data)
	if err != nil {
		return toolCallInfo{}, false
	}
	id, _ := stringField(m, toolID)
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	name, _ := stringField(m, toolName)
	args, _ := stringField(m, toolRawArgs)

	if mp, ok := m[toolMCPParams]; ok && len(mp) > 0 {
		params, err := decodeMessage(mp[0].value)
		if err == nil {
			if list, ok := params[mcpToolsList]; ok && len(list) > 0 {
				tool, err := decodeMessage(list[0].value)
				if err == nil {
					if n, ok := stringField(tool, mcpNestedName); ok && n != "" {
						name = n
					}
					if p, ok := stringField(tool, mcpNestedParams); ok && p != "" {
						args = p
					}
				}
			}
		}
	}
	if id == "" || name == "" {
		return toolCallInfo{}, false
	}
	return toolCallInfo{id: id, name: decloakToolName(name), args: args}, true
}

// extractTextAndThinking pulls text (field 1) and thinking (field 25 -> field 1)
// from a StreamUnifiedChatResponse.
func extractTextAndThinking(data []byte) (text, thinking string) {
	m, err := decodeMessage(data)
	if err != nil {
		return "", ""
	}
	if t, ok := stringField(m, respText); ok {
		text = t
	}
	if th, ok := m[respThinking]; ok && len(th) > 0 {
		tm, err := decodeMessage(th[0].value)
		if err == nil {
			if t, ok := stringField(tm, thinkingText); ok {
				thinking = t
			}
		}
	}
	return text, thinking
}

// decloakToolName strips the "mcp_custom_" prefix Cursor adds on encode so the
// IR (and the client) sees the original tool name.
func decloakToolName(name string) string {
	if strings.HasPrefix(name, "mcp_custom_") {
		return name[len("mcp_custom_"):]
	}
	return name
}

// cursorErrorEnvelope is the shape of Cursor's JSON error frames.
type cursorErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Debug struct {
				Details struct {
					Title  string `json:"title"`
					Detail string `json:"detail"`
					Error  string `json:"error"`
				} `json:"details"`
			} `json:"debug"`
		} `json:"details"`
	} `json:"error"`
}

func isCursorError(data []byte) bool {
	// Cheap check before a full JSON unmarshal: must contain "error".
	return len(data) > 10 && strings.Contains(string(data), "\"error\"")
}

func parseCursorError(data []byte) error {
	var env cursorErrorEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("cursor: upstream error: %s", strings.TrimSpace(string(data)))
	}
	msg := env.Error.Message
	if msg == "" && len(env.Error.Details) > 0 {
		d := env.Error.Details[0].Debug.Details
		msg = d.Title
		if msg == "" {
			msg = d.Detail
		}
	}
	if env.Error.Code == "resource_exhausted" {
		if msg == "" {
			msg = "rate limit exceeded"
		}
		return fmt.Errorf("cursor: rate limited: %s", msg)
	}
	if msg == "" {
		return fmt.Errorf("cursor: upstream error: %s", strings.TrimSpace(string(data)))
	}
	return fmt.Errorf("cursor: %s", msg)
}
