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

// InjectProfileArn sets profileArn on an already-encoded Kiro request body. A
// blank arn is left absent (never a shared default: a wrong-account default ARN
// yields a 403). Returns the body unchanged if it is not a JSON object.
func InjectProfileArn(body []byte, arn string) []byte {
	if arn == "" {
		return body
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	m["profileArn"] = arn
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
	// Guards must run before conversion: they rewrite tool blocks into plain text
	// to avoid Kiro's HTTP 400 on inconsistent tool state.
	msgs := reconcileOrphanedToolResults(req.Messages)
	if len(req.Tools) == 0 {
		msgs = flattenToolInteractions(msgs)
	}

	turns := buildTurns(msgs, req.System)
	state := cwConversationState{
		ChatTriggerType: "MANUAL",
		ConversationID:  ir.NewID("conv_"),
	}

	// The last user turn is the current message; everything before it is history.
	// A trailing assistant turn (prefill) has no place in the CodeWhisperer shape
	// and is dropped.
	curIdx := lastUserTurn(turns)
	if curIdx < 0 {
		// No user turn at all: synthesize an empty current message so the request
		// is still well-formed.
		state.CurrentMessage = cwMessage{UserInputMessage: &cwUserInputMessage{Content: "", Origin: "AI_EDITOR"}}
	} else {
		for i := 0; i < curIdx; i++ {
			state.History = append(state.History, turns[i].history())
		}
		cur := turns[curIdx].user
		cur.Origin = "AI_EDITOR"
		cur.ModelID = req.Model
		if len(req.Tools) > 0 {
			ctx := cur.UserInputMessageContext
			if ctx == nil {
				ctx = &cwUserInputMessageContext{}
			}
			ctx.Tools = encodeTools(req.Tools)
			cur.UserInputMessageContext = ctx
		}
		state.CurrentMessage = cwMessage{UserInputMessage: cur}
	}

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
	// not lost.
	if systemPending != "" {
		turns = append(turns, turn{user: &cwUserInputMessage{Content: systemPending}})
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
	return strings.Join(text, "\n"), toolUses
}

func encodeTools(tools []ir.Tool) []cwTool {
	out := make([]cwTool, 0, len(tools))
	for _, t := range tools {
		schema := json.RawMessage(t.Parameters)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, cwTool{ToolSpecification: cwToolSpecification{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: cwInputSchema{JSON: schema},
		}})
	}
	return out
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
