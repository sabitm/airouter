package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"airouter/internal/proxy/ir"
)

// --- response (decode) wire types ---

type respObject struct {
	ID                string           `json:"id"`
	Model             string           `json:"model"`
	Status            string           `json:"status"`
	Output            []respOutputItem `json:"output"`
	IncompleteDetails *respIncomplete  `json:"incomplete_details"`
	Usage             *respUsage       `json:"usage"`
}

type respIncomplete struct {
	Reason string `json:"reason"`
}

type respUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type respOutputItem struct {
	Type      string              `json:"type"` // message | function_call
	Content   []respOutputContent `json:"content"`
	CallID    string              `json:"call_id"`
	Name      string              `json:"name"`
	Arguments string              `json:"arguments"`
}

type respOutputContent struct {
	Type string `json:"type"` // output_text | text
	Text string `json:"text"`
}

// DecodeResponse parses an OpenAI Responses object into the IR. Used when
// Responses is the backend format.
func DecodeResponse(body []byte) (*ir.Response, error) {
	var resp respObject
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("responses: decode response: %w", err)
	}
	out := &ir.Response{ID: resp.ID, Model: resp.Model, StopReason: ir.StopEndTurn}
	sawTool := false
	for _, it := range resp.Output {
		switch it.Type {
		case "message":
			var text strings.Builder
			for _, c := range it.Content {
				if c.Type == "output_text" || c.Type == "text" {
					text.WriteString(c.Text)
				}
			}
			if t := text.String(); t != "" {
				out.Content = append(out.Content, ir.ContentBlock{Type: ir.BlockText, Text: t})
			}
		case "function_call":
			sawTool = true
			out.Content = append(out.Content, ir.ContentBlock{
				Type: ir.BlockToolUse, ToolID: it.CallID, ToolName: it.Name, ToolInput: rawArgs(it.Arguments),
			})
		}
	}
	out.StopReason = responsesStopReason(resp.Status, sawTool)
	if resp.Usage != nil {
		out.Usage = ir.Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}
	return out, nil
}

// responsesStopReason maps a Responses status to an IR stop reason. Truncation
// (status "incomplete", reason max_output_tokens) takes priority; otherwise a
// present tool call means the model is waiting on a tool result.
func responsesStopReason(status string, sawTool bool) ir.StopReason {
	if status == "incomplete" {
		return ir.StopMaxTokens
	}
	if sawTool {
		return ir.StopToolUse
	}
	return ir.StopEndTurn
}

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
