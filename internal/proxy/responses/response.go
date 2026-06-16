package responses

import (
	"encoding/json"

	"airouter/internal/proxy/ir"
)

// EncodeResponse renders the IR response as an OpenAI Responses object.
func EncodeResponse(resp *ir.Response) ([]byte, error) {
	id := resp.ID
	if id == "" {
		id = ir.NewID("resp_")
	}
	output := buildOutput(resp.Content)

	status := "completed"
	var incomplete any
	if resp.StopReason == ir.StopMaxTokens {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}

	out := map[string]any{
		"id":                 id,
		"object":             "response",
		"status":             status,
		"model":              resp.Model,
		"output":             output,
		"incomplete_details": incomplete,
		"usage": map[string]any{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
			"total_tokens":  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

// buildOutput groups text blocks into a single assistant message item and emits
// each tool_use block as a function_call item, preserving order.
func buildOutput(blocks []ir.ContentBlock) []map[string]any {
	var output []map[string]any
	var textParts []map[string]any
	for _, b := range blocks {
		if b.Type == ir.BlockText {
			textParts = append(textParts, map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}})
		}
	}
	if len(textParts) > 0 {
		output = append(output, map[string]any{
			"type":    "message",
			"id":      ir.NewID("msg_"),
			"status":  "completed",
			"role":    "assistant",
			"content": textParts,
		})
	}
	for _, b := range blocks {
		if b.Type == ir.BlockToolUse {
			args := string(b.ToolInput)
			if args == "" {
				args = "{}"
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        ir.NewID("fc_"),
				"status":    "completed",
				"call_id":   b.ToolID,
				"name":      b.ToolName,
				"arguments": args,
			})
		}
	}
	return output
}
