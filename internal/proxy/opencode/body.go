package opencode

import (
	"encoding/json"
	"strings"
)

// museSparkMinOutputTokens is the upstream's hard floor for
// max_output_tokens on the muse-spark Responses endpoint.
const museSparkMinOutputTokens = 16

// maxEffortForMuseSpark clamps reasoning effort: muse-spark accepts up to
// xhigh; max/ultra are rejected by the upstream.
func clampMuseSparkEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "max", "ultra":
		return "xhigh"
	default:
		return strings.ToLower(strings.TrimSpace(effort))
	}
}

// PrepareMuseSparkResponse normalizes a muse-spark Responses body after the
// generic reasoning finalizer: clamps max/ultra to xhigh, removes an explicit
// none (the upstream rejects reasoning.effort=none with this model), defaults
// summary to auto, and lifts max_output_tokens to the upstream floor.
func PrepareMuseSparkResponse(body []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		if e, isStr := r["effort"].(string); isStr {
			if eff := clampMuseSparkEffort(e); eff == "" || eff == "none" {
				delete(r, "effort")
			} else {
				r["effort"] = eff
			}
		}
		if _, has := r["summary"]; !has && len(r) > 0 {
			r["summary"] = "auto"
		}
		if len(r) > 0 {
			m["reasoning"] = r
		} else {
			delete(m, "reasoning")
		}
	}
	if mot, ok := m["max_output_tokens"].(float64); ok && mot < museSparkMinOutputTokens {
		m["max_output_tokens"] = museSparkMinOutputTokens
	}
	return json.Marshal(m)
}

// needsReasoningEcho reports whether the model's Chat Completions upstream
// validates that reasoning_content is echoed back on assistant turns.
// DeepSeek rejects any assistant turn without it; Kimi requires it only on
// tool-call turns (mirroring the 9router scopes).
func needsReasoningEcho(model string) (bool, bool) {
	m := strings.ToLower(model)
	if strings.Contains(m, "deepseek") {
		return true, false
	}
	if strings.Contains(m, "kimi") {
		return true, true
	}
	return false, false
}

// InjectReasoningEcho patches a Chat Completions body for models whose upstream
// rejects follow-up requests when assistant messages lack reasoning_content: a
// non-empty placeholder satisfies the validation when the client did not echo
// one. toolCallOnly narrows to assistant turns carrying tool_calls.
func InjectReasoningEcho(body []byte, model string) ([]byte, error) {
	need, toolCallOnly := needsReasoningEcho(model)
	if !need {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(envelope["messages"], &messages); err != nil {
		return body, nil
	}
	changed := false
	for i, raw := range messages {
		var msg struct {
			Role             string          `json:"role"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if msg.Role != "assistant" || msg.ReasoningContent != "" {
			continue
		}
		if toolCallOnly {
			var calls []any
			if len(msg.ToolCalls) == 0 || json.Unmarshal(msg.ToolCalls, &calls) != nil || len(calls) == 0 {
				continue
			}
		}
		var mm map[string]any
		if json.Unmarshal(raw, &mm) != nil {
			continue
		}
		mm["reasoning_content"] = " "
		out, err := json.Marshal(mm)
		if err != nil {
			continue
		}
		messages[i] = out
		changed = true
	}
	if !changed {
		return body, nil
	}
	patchedMessages, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	envelope["messages"] = patchedMessages
	return json.Marshal(envelope)
}

// sessionTranscriptCap bounds the assistant text folded into the session
// hash: long enough to distinguish real conversations, cheap to compute.
const sessionTranscriptCap = 512

// AccumulateAssistantText folds assistant text content from either wire family
// (messages[].content or input[] items) into one string, capped. It keys the
// conversation-stable session id: only turns that already produced output
// distinguish conversations; a first user prompt intentionally shares the
// per-account session.
func AccumulateAssistantText(body []byte) string {
	var m struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input []struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, msg := range m.Messages {
		if msg.Role != "assistant" {
			continue
		}
		appendContent(&sb, msg.Content)
		if sb.Len() >= sessionTranscriptCap {
			return sb.String()
		}
	}
	for _, item := range m.Input {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		appendContent(&sb, item.Content)
		if sb.Len() >= sessionTranscriptCap {
			return sb.String()
		}
	}
	return sb.String()
}

// appendContent extracts string/array content text into sb.
func appendContent(sb *strings.Builder, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		sb.WriteString(s)
		return
	}
	var parts []struct {
		Text   string `json:"text"`
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		for _, p := range parts {
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
			if p.Output != "" {
				sb.WriteString(p.Output)
			}
		}
	}
}
