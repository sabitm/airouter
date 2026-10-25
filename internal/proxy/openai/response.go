package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"airouter/internal/proxy/ir"
)

// DecodeResponse parses an OpenAI Chat Completions response into the IR. Used
// when OpenAI is the backend format.
func DecodeResponse(body []byte) (*ir.Response, error) {
	var resp chatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	out := &ir.Response{ID: resp.ID, Model: resp.Model, StopReason: ir.StopEndTurn}
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		if reasoning := chatReasoningText(c.Message.ReasoningContent, c.Message.Reasoning, c.Message.ReasoningDetails); reasoning != "" {
			out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockReasoning, Text: reasoning})
		}
		if c.Message.Content != nil && *c.Message.Content != "" {
			out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockText, Text: *c.Message.Content})
		}
		if c.Message.Refusal != nil {
			// Keep the native field independent of content. Do not duplicate an
			// equal refusal that already arrived as a content part. A content-filter
			// finish is truncation, not a completed refusal.
			if c.FinishReason != "content_filter" && c.FinishReason != "length" && c.FinishReason != "tool_calls" && c.FinishReason != "function_call" {
				out.StopReason = ir.StopRefusal
			}
			dup := false
			for _, b := range out.Content {
				if b.Type == ir.BlockText && b.Refusal && b.Text == *c.Message.Refusal {
					dup = true
					break
				}
			}
			if !dup {
				out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockText, Text: *c.Message.Refusal, Refusal: true})
			}
		}
		for _, tc := range c.Message.ToolCalls {
			out.Content = append(out.Content, ir.ContentBlock{
				Type:      ir.BlockToolUse,
				ToolID:    tc.ID,
				ToolName:  tc.Function.Name,
				ToolInput: rawOrNull(tc.Function.Arguments),
			})
		}
		// A native refusal uses finish_reason stop. Stronger terminal reasons stay
		// stronger; stop alone must not erase the refusal selected above.
		finishStop := stopReasonFromFinish(c.FinishReason)
		if c.FinishReason != "stop" || out.StopReason != ir.StopRefusal {
			out.StopReason = finishStop
		}
	}
	if resp.Usage != nil {
		out.Usage = usageFromWire(resp.Usage)
	}
	return out, nil
}

func usageFromWire(u *chatUsage) ir.Usage {
	out := ir.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		out.CacheWriteTokens = u.PromptTokensDetails.CacheWriteTokens
	}
	return out.Clamped()
}

func usageToWire(u ir.Usage) *chatUsage {
	u = u.Clamped()
	out := &chatUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		out.PromptTokensDetails = &chatPromptTokensDetails{
			CachedTokens:     u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens,
		}
	}
	return out
}

// EncodeResponse renders the IR as an OpenAI Chat Completions response. Used
// when OpenAI is the ingress format.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	id := resp.ID
	if id == "" {
		id = ir.NewID("chatcmpl-")
	}
	msg := chatRespMessage{Role: "assistant"}
	var text, reasoning, refusalText strings.Builder
	refusalSet := false
	// A known unary Anthropic refusal has StopRefusal and no marked text. Project
	// that local copy only. Mixed marked segments keep their own markers.
	content := ir.ProjectUnaryRefusal(resp)
	for _, b := range content {
		switch b.Type {
		case ir.BlockReasoning:
			// Anthropic signatures are not OpenAI encrypted_content. Only readable
			// reasoning text is projected. The proxy logs the omission once.
			reasoning.WriteString(b.Text)
		case ir.BlockRedactedReasoning:
			// Redacted data is omitted. It is not emitted as text.
		case ir.BlockText:
			if b.Refusal {
				// Chat keeps one refusal field, so marked segments stay in that field
				// in order. Ordinary content stays independent. This is not a claim
				// that the two fields can preserve exact cross-field interleaving.
				// The first marked segment, including an explicit empty one, sets the field.
				refusalSet = true
				refusalText.WriteString(b.Text)
				continue
			}
			text.WriteString(b.Text)
		case ir.BlockToolUse:
			msg.ToolCalls = append(msg.ToolCalls, chatToolCall{
				ID:   b.ToolID,
				Type: "function",
				Function: chatFunctionCall{
					Name:      b.ToolName,
					Arguments: string(rawOrNull(string(b.ToolInput))),
				},
			})
		case ir.BlockServer:
			// A provider-owned server block has no Chat Completions projection.
			// Returning an empty successful message would hide the loss.
			return nil, ir.IncompatibleResponse()
		}
	}
	if t := text.String(); t != "" {
		msg.Content = &t
	}
	// Marked segments use the native refusal field. Ordinary content stays in
	// content. A known unary Anthropic refusal is projected above without
	// guessing from phrases or mutating the shared response.
	if refusalSet {
		v := refusalText.String()
		msg.Refusal = &v
	}
	msg.ReasoningContent = reasoning.String()
	out := chatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishFromStopReason(resp.StopReason),
		}},
		Usage: usageToWire(resp.Usage),
	}
	return json.Marshal(out)
}

func stopReasonFromFinish(fr string) ir.StopReason {
	switch fr {
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	default: // "stop" and anything else
		return ir.StopEndTurn
	}
}

func strongerChatStop(current, next ir.StopReason) ir.StopReason {
	rank := func(sr ir.StopReason) int {
		switch sr {
		case ir.StopContentFilter:
			return 4
		case ir.StopMaxTokens:
			return 3
		case ir.StopToolUse:
			return 2
		default:
			return 1
		}
	}
	if rank(next) >= rank(current) {
		return next
	}
	return current
}

func finishFromStopReason(sr ir.StopReason) string {
	switch sr {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopContentFilter:
		return "content_filter"
	default: // end_turn, stop_sequence, refusal
		return "stop"
	}
}
