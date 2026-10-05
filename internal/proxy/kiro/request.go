package kiro

import (
	"encoding/json"
	"strings"

	"airouter/internal/proxy/ir"
)

// EncodeRequest renders the IR as a CodeWhisperer GenerateAssistantResponse
// body with no profile ARN. The proxy injects the provider's ARN afterward via
// InjectProfileArn, mirroring how the Codex backend injects its cache key, so
// the codec's encodeRequest signature stays uniform (no provider access).
func EncodeRequest(req *ir.Request) ([]byte, error) {
	return EncodeRequestWithProfile(req, "")
}

// EncodeRuntimeRequest renders the IR as a Kiro Runtime
// GenerateAssistantResponse body. It does not emit inferenceConfig: that field
// is absent from the official request schema. Profile and agent mode stay absent
// here and are injected only from explicit provider config at preparation.
//
// systemPrompt is optional and is not used. Official extraction is gated by
// system_field_injection, whose default is false, so the system text is folded
// into the first user turn. Temperature, max tokens, and thinking are not mapped
// into additionalModelRequestFields because this pass has no live model schema.
// Continuation IDs and reasoningContent need trusted binding this IR does not
// carry, so they stay omitted.
func EncodeRuntimeRequest(req *ir.Request) ([]byte, error) {
	return encodeRuntimeRequest(req, "", "")
}

// InjectRuntimeAgentMode sets agentMode on an already-encoded Runtime body when
// mode is one of the explicit IDE values. A blank or unknown mode stays absent.
// The same value is sent as the agent-mode header. Legacy bodies are not passed
// here.
func InjectRuntimeAgentMode(body []byte, mode string) []byte {
	mode = agentMode(mode)
	if mode == "" {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	raw, err := json.Marshal(mode)
	if err != nil {
		return body
	}
	m["agentMode"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// InjectProfileArn sets profileArn on an already-encoded Kiro request body. A
// blank arn is left absent (never a shared default: a wrong-account default ARN
// yields a 403). Returns the body unchanged if it is not a JSON object.
func InjectProfileArn(body []byte, arn string) []byte {
	if arn == "" {
		return body
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return body
	}
	raw, err := json.Marshal(arn)
	if err != nil {
		return body
	}
	m["profileArn"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// EncodeRequestWithProfile renders the IR into a Kiro request, injecting the
// given CodeWhisperer profile ARN. An empty profileArn is emitted as an omitted
// field, which is correct for the auth methods this MVP supports (no shared
// default ARN is ever substituted, since a wrong-account default yields 403).
func EncodeRequestWithProfile(req *ir.Request, profileArn string) ([]byte, error) {
	// The catalog is local to this encode and is not written back onto req.
	// History uses the same map, so repeated encodes cannot accumulate suffixes.
	catalog := buildToolCatalog(req.Tools)
	msgs := prepareMessages(req.Messages, catalog)
	turns := buildTurns(msgs, req.System)
	state := cwConversationState{
		ChatTriggerType: "MANUAL",
		ConversationID:  ir.NewID("conv_"),
	}

	// The last user turn is the current message; everything before it is history.
	// A trailing assistant turn has no place in the CodeWhisperer shape, so a
	// non-empty current user turn is synthesized instead of dropping the history.
	curIdx := lastUserTurn(turns)
	if curIdx < 0 || curIdx != len(turns)-1 {
		cur := &cwUserInputMessage{Content: emptyUserContent, Origin: "AI_EDITOR", ModelID: req.Model}
		applyToolCatalog(cur, catalog)
		state.CurrentMessage = cwMessage{UserInputMessage: cur}
		for i := 0; i < len(turns); i++ {
			state.History = append(state.History, turns[i].history())
		}
	} else {
		for i := 0; i < curIdx; i++ {
			state.History = append(state.History, turns[i].history())
		}
		cur := turns[curIdx].user
		cur.Origin = "AI_EDITOR"
		cur.ModelID = req.Model
		applyToolCatalog(cur, catalog)
		state.CurrentMessage = cwMessage{UserInputMessage: cur}
	}
	ensureCurrentContent(state.CurrentMessage.UserInputMessage)

	out := cwRequest{
		ConversationState: state,
		ProfileArn:        profileArn,
		InferenceConfig: cwInferenceConfig{
			MaxTokens:   DefaultMaxTokens,
			Temperature: req.Temperature,
			TopP:        req.TopP,
		},
	}
	return json.Marshal(out)
}

// encodeRuntimeRequest shares the legacy turn construction, then stamps model
// and origin on every user turn. A fresh conversation id is also used as
// rootConversationId, matching the observed fresh-conversation builder. No
// cross-request identity is stored.
func encodeRuntimeRequest(req *ir.Request, profileArn, agentModeValue string) ([]byte, error) {
	catalog := buildToolCatalog(req.Tools)
	msgs := prepareMessages(req.Messages, catalog)
	turns := buildTurns(msgs, req.System)
	conversationID := ir.NewID("conv_")
	state := rtConversationState{
		ChatTriggerType:    "MANUAL",
		ConversationID:     conversationID,
		RootConversationID: conversationID,
	}
	curIdx := lastUserTurn(turns)
	if curIdx < 0 || curIdx != len(turns)-1 {
		cur := &cwUserInputMessage{Content: emptyUserContent, Origin: runtimeOrigin, ModelID: req.Model}
		applyToolCatalog(cur, catalog)
		state.CurrentMessage = cwMessage{UserInputMessage: cur}
		for i := 0; i < len(turns); i++ {
			state.History = append(state.History, stampRuntimeHistory(turns[i], req.Model))
		}
	} else {
		for i := 0; i < curIdx; i++ {
			state.History = append(state.History, stampRuntimeHistory(turns[i], req.Model))
		}
		cur := turns[curIdx].user
		cur.Origin = runtimeOrigin
		cur.ModelID = req.Model
		applyToolCatalog(cur, catalog)
		state.CurrentMessage = cwMessage{UserInputMessage: cur}
	}
	ensureCurrentContent(state.CurrentMessage.UserInputMessage)
	out := rtRequest{
		ConversationState: state,
		ProfileArn:        profileArn,
		AgentMode:         agentMode(agentModeValue),
	}
	return json.Marshal(out)
}

func stampRuntimeHistory(t turn, model string) cwHistory {
	h := t.history()
	if h.UserInputMessage != nil {
		h.UserInputMessage.ModelID = model
		h.UserInputMessage.Origin = runtimeOrigin
	}
	return h
}

// turn is one merged conversation turn in the IR order, already converted to the
// CodeWhisperer message shape. Exactly one of user/assistant is set.
type turn struct {
	user      *cwUserInputMessage
	assistant *cwAssistantResponseMessage
}

func (t turn) history() cwHistory {
	if t.user != nil {
		return cwHistory{UserInputMessage: t.user}
	}
	return cwHistory{AssistantResponseMessage: t.assistant}
}

// buildTurns converts IR messages to merged CodeWhisperer turns, prepending the
// system prompt to the first user turn's content (matching the claude-to-kiro
// direct route). Consecutive same-role messages are merged so history alternates.
func buildTurns(msgs []ir.Message, system string) []turn {
	var turns []turn
	systemPending := strings.TrimSpace(system)

	for _, m := range msgs {
		if m.Role == ir.RoleAssistant {
			content, toolUses := buildAssistant(m.Content)
			if n := len(turns); n > 0 && turns[n-1].assistant != nil {
				merge := turns[n-1].assistant
				merge.Content = joinContent(merge.Content, content)
				merge.ToolUses = append(merge.ToolUses, toolUses...)
				continue
			}
			turns = append(turns, turn{assistant: &cwAssistantResponseMessage{Content: content, ToolUses: toolUses}})
			continue
		}
		content, images, toolResults := buildUser(m.Content)
		if systemPending != "" {
			content = joinContent(systemPending, content)
			systemPending = ""
		}
		if n := len(turns); n > 0 && turns[n-1].user != nil {
			merge := turns[n-1].user
			merge.Content = joinContent(merge.Content, content)
			merge.Images = append(merge.Images, images...)
			if len(toolResults) > 0 {
				if merge.UserInputMessageContext == nil {
					merge.UserInputMessageContext = &cwUserInputMessageContext{}
				}
				merge.UserInputMessageContext.ToolResults = append(merge.UserInputMessageContext.ToolResults, toolResults...)
			}
			continue
		}
		u := &cwUserInputMessage{Content: content, Images: images}
		if len(toolResults) > 0 {
			u.UserInputMessageContext = &cwUserInputMessageContext{ToolResults: toolResults}
		}
		turns = append(turns, turn{user: u})
	}

	// System prompt with no user turn to attach to: emit a lone user turn so it is
	// not lost. An assistant-only history therefore starts with that user turn.
	if systemPending != "" {
		if len(turns) == 0 || turns[0].user == nil {
			turns = append([]turn{{user: &cwUserInputMessage{Content: systemPending}}}, turns...)
		} else {
			turns[0].user.Content = joinContent(systemPending, turns[0].user.Content)
		}
	}
	for i := range turns {
		if turns[i].user != nil {
			ensureUserContent(turns[i].user)
		}
	}
	return turns
}

func lastUserTurn(turns []turn) int {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].user != nil {
			return i
		}
	}
	return -1
}

// buildUser flattens a user message's blocks into content text, inline images,
// and tool results. Remote image URLs must be materialized before encode; the
// proxy preflight skips Kiro when materialization cannot produce inline bytes.
// File blocks are not representable and are ignored here (preflight rejects).
func buildUser(blocks []ir.ContentBlock) (content string, images []cwImage, toolResults []cwToolResult) {
	var text []string
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Text != "" {
				text = append(text, b.Text)
			}
		case ir.BlockImage:
			if b.Image == nil {
				continue
			}
			if b.Image.Data != "" {
				images = append(images, cwImage{Format: imageFormat(b.Image.MediaType), Source: cwImageSource{Bytes: b.Image.Data}})
			}
			// URL-only images are left out; preflight + materialization handle them.
		case ir.BlockToolResult:
			toolResults = append(toolResults, cwToolResult{
				ToolUseID: b.ToolUseID,
				Status:    toolResultStatus(b.IsError),
				Content:   []cwToolResultText{{Text: toolResultText(b)}},
			})
		}
	}
	return strings.Join(text, "\n"), images, toolResults
}

func buildAssistant(blocks []ir.ContentBlock) (content string, toolUses []cwToolUse) {
	var text []string
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			if b.Text != "" {
				text = append(text, b.Text)
			}
		case ir.BlockToolUse:
			input := b.ToolInput
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			toolUses = append(toolUses, cwToolUse{ToolUseID: b.ToolID, Name: b.ToolName, Input: input})
		}
	}
	content = strings.Join(text, "\n")
	if strings.TrimSpace(content) == "" {
		content = emptyAssistantContent
	}
	return content, toolUses
}

func applyToolCatalog(cur *cwUserInputMessage, catalog toolCatalog) {
	if cur == nil || catalog.empty() {
		return
	}
	ctx := cur.UserInputMessageContext
	if ctx == nil {
		ctx = &cwUserInputMessageContext{}
	}
	ctx.Tools = catalog.tools
	cur.UserInputMessageContext = ctx
}

func ensureUserContent(msg *cwUserInputMessage) {
	if msg == nil || strings.TrimSpace(msg.Content) != "" {
		return
	}
	if msg.UserInputMessageContext != nil && len(msg.UserInputMessageContext.ToolResults) > 0 {
		msg.Content = toolResultsPlaceholder
		return
	}
	msg.Content = emptyUserContent
}

func ensureCurrentContent(cur *cwUserInputMessage) {
	ensureUserContent(cur)
}

// toolResultText collapses a tool_result's block content into plain text, the
// only form CodeWhisperer accepts for a tool result.
func toolResultText(b ir.ContentBlock) string {
	var parts []string
	for _, rb := range b.ToolResult {
		if rb.Type == ir.BlockText && rb.Text != "" {
			parts = append(parts, rb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func toolResultStatus(isError bool) string {
	if isError {
		return "error"
	}
	return "success"
}

func imageFormat(mediaType string) string {
	if i := strings.LastIndex(mediaType, "/"); i >= 0 {
		if f := mediaType[i+1:]; f != "" {
			return f
		}
	}
	return "png"
}

func joinContent(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}
