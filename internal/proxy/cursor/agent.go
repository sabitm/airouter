package cursor

// agent.go implements the agent.v1.AgentService/Run wire format: the endpoint
// Cursor's own CLI and current IDE use for chat (ChatService is retired for
// non-Pro accounts). Field numbers are ported from the Cursor IDE agent.v1
// definitions and cross-checked against 9router's AgentService executor.
//
// Request envelope (AgentClientMessage):
//
//	1: AgentRunRequest{
//	    1: conversation_state: empty (a fresh session per proxy request)
//	    2: ConversationAction{ 1: UserMessageAction{
//	        1: UserMessage{ 1: text, 2: message_id, 3: selected_context, 4: mode=1 }
//	        7: ConversationHistory{ 1: repeated history messages } } }
//	    3: ModelDetails{ 1: model, 3: model, 4: model }
//	    4: McpTools{ 1: repeated McpToolDefinition }
//	    9: RequestedModel{ 1: model_id, 7: built_in_model=true } }
//
// custom_system_prompt (field 8) is rejected by the deployed server, so the
// system prompt is folded into the current user message. Prior turns go out
// as structured conversation_history, not a transcript inside that text.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"airouter/internal/proxy/ir"
)

// AgentClientMessage field numbers.
const (
	acmRunRequest = 1
)

// AgentRunRequest field numbers.
const (
	runConversationState = 1
	runAction            = 2
	runModelDetails      = 3
	runMCPTools          = 4
	runCustomSystem      = 8
	runRequestedModel    = 9
)

// ModelDetails. Fields 1, 3, and 4 all carry the model id. Thinking variants
// return an empty turn when only RequestedModel (field 9) is set.
const (
	mdModelID    = 1
	mdModelIDAlt = 3
	mdDisplay    = 4
)

// ConversationAction and UserMessageAction.
const (
	convUserMessageAction  = 1
	umaUserMessage         = 1
	umaRequestContext      = 2
	umaConversationHistory = 7
)

// UserMessage. selected_context (3) and mode=1 (4) match cursor-agent; without
// them the server may accept the RPC and stream an empty turn.
const (
	umText            = 1
	umMessageID       = 2
	umSelectedContext = 3
	umMode            = 4
	umModeAgent       = 1
)

// ConversationHistoryMessage oneofs and nested content.
const (
	chMessages = 1

	chmUser      = 1
	chmAssistant = 2
	chmTool      = 3

	chuContent = 1
	chaContent = 1

	hcText     = 1
	hcToolCall = 4

	tpText = 1

	chtcID       = 1
	chtcName     = 2
	chtcArgsJSON = 3

	chtCallID  = 1
	chtName    = 2
	chtContent = 3
	chtIsError = 4
	chtcText   = 1
)

// McpTools / McpToolDefinition.
const (
	mcpDefsName            = 1
	mcpDefName             = 1
	mcpDefDescription      = 2
	mcpDefInputSchemaValue = 3
	mcpDefInputSchemaJSON  = 6
	mcpDefProviderID       = 4
	mcpDefToolName         = 5
)

// google.protobuf.Value / Struct / ListValue.
const (
	pvNull   = 1
	pvNumber = 2
	pvString = 3
	pvBool   = 4
	pvStruct = 5
	pvList   = 6

	psFields = 1
	plValues = 1
	pmKey    = 1
	pmValue  = 2
)

// RequestedModel.
const (
	rmModelID      = 1
	rmBuiltInModel = 7
)

// AgentServerMessage field numbers.
const (
	asmInteractionUpdate = 1
	asmExecServerMessage = 2
	asmKVServerMessage   = 4
	// asmInteractionQuery (7): the server asks the CLIENT to run a built-in
	// interaction. Built-ins are not surfaced. An unmatched query is ignored
	// so the stream can continue; ending the turn here drops later text.
	asmInteractionQuery = 7

	iqID        = 1
	iqWebSearch = 2
	iqWebFetch  = 9

	// WebSearchRequestQuery / WebFetchRequestQuery wrap typed args at field 1.
	iqQueryArgs = 1
	// WebSearchArgs / WebFetchArgs share field numbers: 1 = primary value
	// (search_term or url), 2 = tool_call_id.
	iqArgPrimary = 1
	iqArgCallID  = 2
)

// KvServerMessage / KvClientMessage and blob args/results.
const (
	kvsID          = 1
	kvsGetBlobArgs = 2
	kvsSetBlobArgs = 3
	kvcID          = 1
	kvcGetBlobRes  = 2
	kvcSetBlobRes  = 3
)

// AgentClientMessage reply envelopes. ExecClientMessage is field 2.
// ExecClientControlMessage is field 5; its throw oneof is field 2.
const (
	acmExecClientMessage = 2
	acmKVClientMessage   = 3
	acmExecClientControl = 5
)

// ExecClientMessage replies. Field 45 is hook_additional_contexts, not a
// result. Result field numbers come from execArgResult.
const (
	ecmID                = 1
	ecmExecID            = 15
	ecmRequestContextRes = 10
	ecmMCPResult         = 11
)

// ExecClientControlMessage / ExecClientThrow. The official handler uses
// throw when a recognized exec has no handler. id is the exec id; error is
// the human message. stack_trace and error_code stay unset for this rejection.
const (
	eccThrow = 2
	ectID    = 1
	ectError = 2
)

// McpResult oneof. Field 1 is McpSuccess. The proxy acks with an empty
// success so AgentService can close the exec. The real tool result returns
// on the next request's history. An empty success does not produce
// turn_ended.
const (
	mcpResultSuccess = 1
)

// InteractionUpdate field numbers (oneof message).
const (
	iuTextDelta        = 1
	iuToolCallStarted  = 2
	iuToolCallComplete = 3
	iuThinkingDelta    = 4
	iuPartialToolCall  = 7
	iuTokenDelta       = 8
	iuHeartbeat        = 13
	iuTurnEnded        = 14
)

// TextDeltaUpdate / ThinkingDeltaUpdate / TokenDeltaUpdate / TurnEndedUpdate.
const (
	tdText             = 1
	thdText            = 1
	tokdTokens         = 1
	teInputTokens      = 1
	teOutputTokens     = 2
	teCacheReadTokens  = 3
	teCacheWriteTokens = 4
	teReasoningTokens  = 5
)

// ToolCallStartedUpdate / ToolCallCompletedUpdate / PartialToolCallUpdate.
const (
	tcsCallID    = 1
	tcsToolCall  = 2
	tcsModelCall = 3
	ptcCallID    = 1
	ptcToolCall  = 2
	ptcArgsDelta = 3
	ptcModelCall = 4
)

// ToolCall oneof variants (client-visible tool kinds).
const (
	tcMCPTOolCall = 15
)

// McpToolCall / McpArgs.
const (
	mtcArgs    = 1
	maName     = 1
	maArgs     = 2
	maCallID   = 3
	maToolName = 5
)

// ExecServerMessage field numbers.
const (
	esmID                 = 1
	esmExecID             = 15
	esmRequestContextArgs = 10
	esmMCPArgs            = 11
)

// EncodeAgentRequest maps an IR request to a Connect-framed
// agent.v1.AgentClientMessage carrying an AgentRunRequest. Prior turns are
// replayed as ConversationHistory on UserMessageAction; the current turn stays
// in UserMessage text.
func EncodeAgentRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor: nil request")
	}
	model := strings.TrimPrefix(strings.TrimSpace(req.Model), "cursor/")
	if model == "" {
		return nil, fmt.Errorf("cursor: empty model")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("cursor: empty messages")
	}

	// The current turn is the trailing user message (the one the model answers);
	// everything before it is replayable history. Mirrors the CLI's split.
	currentIdx := -1
	for i, m := range req.Messages {
		if m.Role == ir.RoleUser {
			currentIdx = i
		}
	}
	if currentIdx < 0 {
		// No user turn at all: treat the final message as current so the request
		// still carries one user_message (AgentService requires it).
		currentIdx = len(req.Messages) - 1
	}

	toolNames := map[string]string{}
	for _, m := range req.Messages {
		if m.Role != ir.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.ToolID != "" {
				toolNames[b.ToolID] = b.ToolName
			}
		}
	}

	current := req.Messages[currentIdx]
	userText := renderCurrentMessage(current, toolNames)
	if userText == "" {
		userText = "Continue."
	}
	// custom_system_prompt (field 8) is rejected by the deployed server as an
	// unknown CLI option, so the system prompt is folded into the current user
	// message instead, matching the CLI's own prompt layout.
	sys := strings.TrimSpace(req.System)
	if note := MCPAvailabilityNote(req.Tools); note != "" {
		if sys != "" {
			sys = sys + "\n\n" + note
		} else {
			sys = note
		}
	}
	if sys != "" {
		userText = "[System Instructions]\n" + sys + "\n\n" + userText
	}

	userMessage := concatBytes(
		encodeField(umText, wireLen, userText),
		encodeField(umMessageID, wireLen, uuid.NewString()),
		encodeField(umSelectedContext, wireLen, []byte{}),
		encodeField(umMode, wireVarint, uint64(umModeAgent)),
	)
	userAction := encodeField(umaUserMessage, wireLen, userMessage)
	if hist := encodeConversationHistory(req.Messages[:currentIdx], toolNames); len(hist) > 0 {
		userAction = append(userAction, encodeField(umaConversationHistory, wireLen, hist)...)
	}

	run := concatBytes(
		encodeField(runConversationState, wireLen, []byte{}),
		encodeField(runAction, wireLen,
			encodeField(convUserMessageAction, wireLen, userAction)),
		encodeField(runModelDetails, wireLen, encodeModelDetails(model)),
	)
	if len(req.Tools) > 0 {
		run = append(run, encodeField(runMCPTools, wireLen, encodeAgentMCPTools(req.Tools))...)
	}
	run = append(run, encodeField(runRequestedModel, wireLen, concatBytes(
		encodeField(rmModelID, wireLen, model),
		encodeField(rmBuiltInModel, wireVarint, uint64(1)),
	))...)

	clientMessage := encodeField(acmRunRequest, wireLen, run)
	return wrapConnectFrame(clientMessage, false), nil
}

// encodeModelDetails is ModelDetails{1: model, 3: model, 4: model}.
func encodeModelDetails(model string) []byte {
	return concatBytes(
		encodeField(mdModelID, wireLen, model),
		encodeField(mdModelIDAlt, wireLen, model),
		encodeField(mdDisplay, wireLen, model),
	)
}

// encodeConversationHistory builds ConversationHistory{1: repeated
// ConversationHistoryMessage} for turns before the current user message.
// An empty history is omitted by the caller (nil).
func encodeConversationHistory(messages []ir.Message, toolNames map[string]string) []byte {
	var out []byte
	for _, m := range messages {
		switch m.Role {
		case ir.RoleUser:
			out = append(out, encodeHistoryUser(m, toolNames)...)
		case ir.RoleAssistant:
			out = append(out, encodeHistoryAssistant(m)...)
		}
	}
	return out
}

// encodeHistoryUser groups contiguous text parts into one user
// HistoryMessage. ConversationHistoryUserMessage.content is repeated, so a
// prior user turn with several text blocks stays one turn. A tool result is
// a separate structured message and splits the surrounding text groups.
// Empty text is skipped. Tool-result-only turns still produce tool messages
// so the model sees the prior call's output.
func encodeHistoryUser(m ir.Message, toolNames map[string]string) []byte {
	var out []byte
	var textParts []byte
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		user := encodeField(chmUser, wireLen, textParts)
		out = append(out, encodeField(chMessages, wireLen, user)...)
		textParts = nil
	}
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockText:
			if b.Text == "" {
				continue
			}
			part := encodeField(hcText, wireLen, encodeField(tpText, wireLen, b.Text))
			textParts = append(textParts, encodeField(chuContent, wireLen, part)...)
		case ir.BlockToolResult:
			flushText()
			if msg := encodeHistoryToolResult(b, toolNames); len(msg) > 0 {
				out = append(out, encodeField(chMessages, wireLen, msg)...)
			}
		}
	}
	flushText()
	return out
}

func encodeHistoryToolResult(b ir.ContentBlock, toolNames map[string]string) []byte {
	name := toolNames[b.ToolUseID]
	if name == "" {
		name = "tool"
	}
	text := toolResultText(b)
	content := encodeField(chtContent, wireLen, encodeField(chtcText, wireLen, text))
	tool := concatBytes(
		encodeField(chtCallID, wireLen, b.ToolUseID),
		encodeField(chtName, wireLen, name),
		content,
	)
	if b.IsError {
		tool = append(tool, encodeField(chtIsError, wireVarint, uint64(1))...)
	}
	return encodeField(chmTool, wireLen, tool)
}

// encodeHistoryAssistant emits one assistant HistoryMessage whose content
// repeats text parts and tool_call parts in block order. Empty text is
// skipped. A message with no encodable parts is omitted.
func encodeHistoryAssistant(m ir.Message) []byte {
	var content []byte
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockText:
			if b.Text == "" {
				continue
			}
			part := encodeField(hcText, wireLen, encodeField(tpText, wireLen, b.Text))
			content = append(content, encodeField(chaContent, wireLen, part)...)
		case ir.BlockToolUse:
			args := string(b.ToolInput)
			if args == "" {
				args = "{}"
			}
			call := concatBytes(
				encodeField(chtcID, wireLen, b.ToolID),
				encodeField(chtcName, wireLen, b.ToolName),
				encodeField(chtcArgsJSON, wireLen, args),
			)
			part := encodeField(hcToolCall, wireLen, call)
			content = append(content, encodeField(chaContent, wireLen, part)...)
		}
	}
	if len(content) == 0 {
		return nil
	}
	return encodeField(chMessages, wireLen, encodeField(chmAssistant, wireLen, content))
}

// encodeAgentMCPTools builds McpTools{1: repeated McpToolDefinition}. The
// schema is sent both as a google.protobuf.Value (field 3, IDE/9router) and
// as input_schema_json (field 6, CLI). Identity fields are required: the
// provider rejects two or more definitions that omit them.
func encodeAgentMCPTools(tools []ir.Tool) []byte {
	var out []byte
	for _, t := range tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		def := concatBytes(
			encodeField(mcpDefName, wireLen, t.Name),
			encodeField(mcpDefProviderID, wireLen, []byte("airouter")),
			encodeField(mcpDefToolName, wireLen, t.Name),
		)
		if t.Description != "" {
			def = append(def, encodeField(mcpDefDescription, wireLen, t.Description)...)
		}
		def = append(def, concatBytes(
			encodeField(mcpDefInputSchemaValue, wireLen, encodeProtoValue(jsonValue(params))),
			encodeField(mcpDefInputSchemaJSON, wireLen, []byte(params)),
		)...)
		out = append(out, encodeField(mcpDefsName, wireLen, def)...)
	}
	return out
}

// jsonValue parses a JSON schema. Invalid or empty input becomes the default
// object schema so the Value field is never omitted.
func jsonValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return v
}

// encodeProtoValue encodes a Go value as a google.protobuf.Value oneof body.
// null is field 1 varint 0, number is field 2 fixed64 LE double, string is
// field 3, bool is field 4, object is field 5 Struct, array is field 6 ListValue.
func encodeProtoValue(v any) []byte {
	switch x := v.(type) {
	case nil:
		return encodeField(pvNull, wireVarint, uint64(0))
	case bool:
		n := uint64(0)
		if x {
			n = 1
		}
		return encodeField(pvBool, wireVarint, n)
	case float64:
		return encodeField(pvNumber, wireFixed64, x)
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return encodeField(pvString, wireLen, x.String())
		}
		return encodeField(pvNumber, wireFixed64, f)
	case string:
		return encodeField(pvString, wireLen, x)
	case []any:
		var items []byte
		for _, item := range x {
			items = append(items, encodeField(plValues, wireLen, encodeProtoValue(item))...)
		}
		return encodeField(pvList, wireLen, items)
	case map[string]any:
		var entries []byte
		for k, val := range x {
			entry := concatBytes(
				encodeField(pmKey, wireLen, k),
				encodeField(pmValue, wireLen, encodeProtoValue(val)),
			)
			entries = append(entries, encodeField(psFields, wireLen, entry)...)
		}
		return encodeField(pvStruct, wireLen, entries)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return encodeField(pvNull, wireVarint, uint64(0))
		}
		return encodeField(pvString, wireLen, string(b))
	}
}

// renderCurrentMessage renders the current turn's blocks: tool results first
// (labeled), then any text. Tool-result-only turns (the client answered a
// tool call) must keep their results or the model re-issues the call.
func renderCurrentMessage(m ir.Message, toolNames map[string]string) string {
	var results, text strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case ir.BlockText:
			text.WriteString(b.Text)
		case ir.BlockToolResult:
			name := toolNames[b.ToolUseID]
			if name == "" {
				name = "tool"
			}
			results.WriteString("Tool result (" + name + "): " + toolResultText(b) + "\n")
		}
	}
	out := strings.TrimSpace(results.String() + text.String())
	if out == "" {
		return ""
	}
	if results.Len() > 0 && text.Len() > 0 {
		return results.String() + "\n" + strings.TrimSpace(text.String())
	}
	return out
}
