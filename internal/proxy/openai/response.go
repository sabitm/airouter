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
		if c.Message.Content != nil && *c.Message.Content != "" {
			out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockText, Text: *c.Message.Content})
		}
		for _, tc := range c.Message.ToolCalls {
			out.Content = append(out.Content, ir.ContentBlock{
				Type:      ir.BlockToolUse,
				ToolID:    tc.ID,
				ToolName:  tc.Function.Name,
				ToolInput: rawOrNull(tc.Function.Arguments),
			})
		}
		out.StopReason = stopReasonFromFinish(c.FinishReason)
	}
	if resp.Usage != nil {
		out.Usage = ir.Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	}
	return out, nil
}

// EncodeResponse renders the IR as an OpenAI Chat Completions response. Used
// when OpenAI is the ingress format.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	id := resp.ID
	if id == "" {
		id = ir.NewID("chatcmpl-")
	}
	msg := chatRespMessage{Role: "assistant"}
	var text strings.Builder
	for _, b := range resp.Content {
		switch b.Type {
		case ir.BlockText:
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
		}
	}
	if t := text.String(); t != "" {
		msg.Content = &t
	}
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
		Usage: &chatUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

func stopReasonFromFinish(fr string) ir.StopReason {
	switch fr {
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	default: // "stop" and anything else
		return ir.StopEndTurn
	}
}

func finishFromStopReason(sr ir.StopReason) string {
	switch sr {
	case ir.StopMaxTokens:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	default: // end_turn, stop_sequence
		return "stop"
	}
}
