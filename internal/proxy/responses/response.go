package responses

import (
	"bytes"
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
	Error             *respErrorObject `json:"error"`
}

// respErrorObject is the nested error on a failed Responses object or SSE frame.
// Optional metadata is decoded tolerantly so a numeric code or unsupported
// value cannot invalidate a recognized failure.
type respErrorObject struct {
	Type    string
	Code    string
	Message string
}

// errorScalar decodes optional error metadata without failing the surrounding
// object. Strings are preserved, numbers keep their original JSON text, and
// null or unsupported values become empty.
type errorScalar string

func (s *errorScalar) UnmarshalJSON(data []byte) error {
	*s = errorScalar(normalizeErrorScalar(data))
	return nil
}

func normalizeErrorScalar(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return ""
	}
	switch data[0] {
	case '"':
		var s string
		if json.Unmarshal(data, &s) != nil {
			return ""
		}
		return s
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return string(data)
	default:
		return ""
	}
}

func (e *respErrorObject) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" || data[0] != '{' {
		*e = respErrorObject{}
		return nil
	}
	var raw struct {
		Type    errorScalar `json:"type"`
		Code    errorScalar `json:"code"`
		Message errorScalar `json:"message"`
	}
	if json.Unmarshal(data, &raw) != nil {
		*e = respErrorObject{}
		return nil
	}
	*e = respErrorObject{Type: string(raw.Type), Code: string(raw.Code), Message: string(raw.Message)}
	return nil
}

type respIncomplete struct {
	Reason string `json:"reason"`
}

type respInputTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type respUsage struct {
	InputTokens        int                     `json:"input_tokens"`
	OutputTokens       int                     `json:"output_tokens"`
	InputTokensDetails *respInputTokensDetails `json:"input_tokens_details,omitempty"`
}

type respOutputItem struct {
	ID         string              `json:"id"`
	Type       string              `json:"type"` // message | reasoning | function_call
	Content    []respOutputContent `json:"content"`
	Summary    []reasoningSummary  `json:"summary"`
	SummarySet bool                `json:"-"`
	CallID     string              `json:"call_id"`
	Name       string              `json:"name"`
	Arguments  string              `json:"arguments"`
	// EncryptedContent is provider-owned opaque state. A missing or null field
	// and an explicit empty string stay distinct through the pointer.
	EncryptedContent *string `json:"encrypted_content"`
	Status           string  `json:"status"`
}

// UnmarshalJSON records whether the summary array was present and non-null so a
// complete terminal item can tell an absent placeholder from an explicit array.
// Content presence is read directly: an explicit empty array decodes non-nil
// while an absent or null field stays nil.
func (it *respOutputItem) UnmarshalJSON(data []byte) error {
	type plain respOutputItem
	var raw struct {
		plain
		Summary json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*it = respOutputItem(raw.plain)
	it.SummarySet = !isNullish(raw.Summary)
	if it.SummarySet {
		if err := json.Unmarshal(raw.Summary, &it.Summary); err != nil {
			return err
		}
	}
	return nil
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
	if resp.Status == "failed" {
		fail := streamFailureFrom(nil, &resp)
		if fail == nil {
			fail = &ir.StreamFailure{Message: "upstream response failed"}
		}
		return nil, fail
	}
	out := &ir.Response{ID: resp.ID, Model: resp.Model, StopReason: ir.StopEndTurn}
	sawTool := false
	for _, it := range resp.Output {
		switch it.Type {
		case "reasoning":
			// One structured block per item, in output order, even when the
			// readable projection is empty. Encrypted bytes stay in the envelope.
			out.Content = append(out.Content, reasoningBlock(it.ID, reasoningSummaryParts(it.Summary), it.EncryptedContent, it.Status))
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
		out.Usage = usageFromWire(resp.Usage)
	}
	return out, nil
}

func usageFromWire(u *respUsage) ir.Usage {
	out := ir.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
		out.CacheWriteTokens = u.InputTokensDetails.CacheWriteTokens
	}
	return out.Clamped()
}

func usageToWire(u ir.Usage) map[string]any {
	u = u.Clamped()
	wire := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		wire["input_tokens_details"] = map[string]any{
			"cached_tokens":      u.CacheReadTokens,
			"cache_write_tokens": u.CacheWriteTokens,
		}
	}
	return wire
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
		"usage":              usageToWire(resp.Usage),
	}
	return json.Marshal(out)
}

// buildOutput renders IR content blocks as Responses output items. Relative
// block order is preserved: consecutive text blocks form one message item, and
// a reasoning or function_call block flushes the pending message first. Exactly
// one reasoning item is emitted per structured reasoning block; readable
// summaries come only from Summary (or the legacy Text field), never from
// opaque provider state.
func buildOutput(blocks []ir.ContentBlock) []map[string]any {
	var output []map[string]any
	var textParts []map[string]any
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		output = append(output, map[string]any{
			"type":    "message",
			"id":      ir.NewID("msg_"),
			"status":  "completed",
			"role":    "assistant",
			"content": textParts,
		})
		textParts = nil
	}
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockReasoning:
			if b.ResponsesReasoning != nil {
				flushText()
				output = append(output, reasoningOutputItem(b.ResponsesReasoning, b.Text, true))
				continue
			}
			if b.Text != "" {
				// Readable summary only. Do not copy Anthropic signatures into
				// encrypted_content or invent a signature for unsigned reasoning.
				flushText()
				output = append(output, reasoningOutputItem(nil, b.Text, true))
			}
		case ir.BlockText:
			textParts = append(textParts, map[string]any{"type": "output_text", "text": b.Text, "annotations": []any{}})
		case ir.BlockToolUse:
			flushText()
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
		// BlockRedactedReasoning is omitted. Its data is not summary text.
	}
	flushText()
	return output
}
