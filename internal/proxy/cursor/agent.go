package cursor

// agent.go implements the agent.v1.AgentService/Run wire format: the endpoint
// Cursor's own CLI and current IDE use for chat (ChatService is retired for
// non-Pro accounts). Field numbers are ported from the Cursor CLI's bundled
// proto definitions (agent.v1) and cross-checked against 9router's executor.
//
// Request envelope (AgentClientMessage):
//
//	1: AgentRunRequest{
//	    1: conversation_state: empty (a fresh session per proxy request; history
//	       is replayed inline so the proxy stays stateless)
//	    2: ConversationAction{ 1: UserMessageAction{
//	        1: UserMessage{ 1: text, 2: message_id }
//	        2: RequestContext{}        // empty: no IDE context to offer
//	        7: ConversationHistory{ 1: repeated history messages } } }
//	    4: McpTools{ 1: repeated McpToolDefinition } (registered client tools)
//	    8: custom_system_prompt
//	    9: RequestedModel{ 1: model_id, 7: built_in_model=true } }

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
	runMCPTools          = 4
	runCustomSystem      = 8
	runRequestedModel    = 9
)

// ConversationAction and UserMessageAction.
const (
	convUserMessageAction  = 1
	umaUserMessage         = 1
	umaRequestContext      = 2
	umaConversationHistory = 7
)

// UserMessage.
const (
	umText      = 1
	umMessageID = 2
)

// ConversationHistory*Content oneofs and the shared text part.
const (
	hcText = 1
	tpText = 1
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
	// interaction (web search, web fetch, ask question, plan, VM setup). A
	// proxy cannot execute these, and ignoring the query stalls the turn with
	// heartbeats forever — DecodeAgentStream fails the turn instead.
	asmInteractionQuery = 7

	// InteractionQuery variants. Only the discriminator matters; every variant
	// is client-executed.
	iqWebSearch = 2
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

// ExecClientMessage replies.
const (
	ecmID                = 1
	ecmExecID            = 15
	ecmRequestContextRes = 10
	ecmMCPResult         = 11
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
// agent.v1.AgentClientMessage carrying an AgentRunRequest. Tool results in the
// IR history are replayed as native ConversationHistoryToolMessage entries (not
// flattened XML, which was the retired ChatService path).
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

	// The deployed AgentService ignores UserMessageAction.conversation_history
	// (verified live: input token counts are identical with and without it);
	// only checkpoint resume carries state. A stateless proxy therefore folds
	// prior turns into the current message as a transcript, the same approach
	// the retired ChatService path used for tool results.
	transcript := renderHistoryTranscript(req.Messages[:currentIdx], toolNames)

	current := req.Messages[currentIdx]
	userText := renderCurrentMessage(current, toolNames)
	if userText == "" {
		userText = "Continue."
	}
	if transcript != "" {
		userText = transcript + "\n\n[Current Message]\n" + userText
	}
	// custom_system_prompt (field 8) is rejected by the deployed server as an
	// unknown CLI option, so the system prompt is folded into the current user
	// message instead, matching the CLI's own prompt layout.
	if sys := strings.TrimSpace(req.System); sys != "" {
		userText = "[System Instructions]\n" + sys + "\n\n" + userText
	}

	userMessage := concatBytes(
		encodeField(umText, wireLen, userText),
		encodeField(umMessageID, wireLen, uuid.NewString()),
	)
	userAction := concatBytes(
		encodeField(umaUserMessage, wireLen, userMessage),
		encodeField(umaRequestContext, wireLen, []byte{}),
	)

	run := concatBytes(
		encodeField(runConversationState, wireLen, []byte{}),
		encodeField(runAction, wireLen,
			encodeField(convUserMessageAction, wireLen, userAction)),
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

// renderHistoryTranscript folds prior turns into a labeled transcript block.
// Assistant tool calls are rendered inline (name + arguments) so the following
// tool-result lines have context. toolNames maps call ids to names for results
// whose caller message was dropped by a client.
func renderHistoryTranscript(messages []ir.Message, toolNames map[string]string) string {
	var sb strings.Builder
	for _, m := range messages {
		switch m.Role {
		case ir.RoleUser:
			var text strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockText:
					text.WriteString(b.Text)
				case ir.BlockToolResult:
					name := toolNames[b.ToolUseID]
					if name == "" {
						name = "tool"
					}
					sb.WriteString("Tool result (" + name + "): " + toolResultText(b) + "\n")
				}
			}
			if s := strings.TrimSpace(text.String()); s != "" {
				sb.WriteString("User: " + s + "\n")
			}
		case ir.RoleAssistant:
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockText:
					if s := strings.TrimSpace(b.Text); s != "" {
						sb.WriteString("Assistant: " + s + "\n")
					}
				case ir.BlockToolUse:
					sb.WriteString("Assistant (tool call): " + b.ToolName + " " + string(b.ToolInput) + "\n")
				}
			}
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return ""
	}
	return "[Conversation History]\n" + out
}

// encodeAgentMCPTools builds McpTools{1: repeated McpToolDefinition}. The
// schema is sent as input_schema_json (string) — the CLI's own field — which
// avoids encoding a google.protobuf.Value tree.
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
		def = append(def, encodeField(mcpDefInputSchemaJSON, wireLen, []byte(params))...)
		out = append(out, encodeField(mcpDefsName, wireLen, def)...)
	}
	return out
}

// renderCurrentMessage renders the current turn's blocks: tool results first
// (labeled like the transcript), then any text. Tool-result-only turns (the
// client answered a tool call) must keep their results or the model re-issues
// the call.
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
