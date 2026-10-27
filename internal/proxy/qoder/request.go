package qoder

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"airouter/internal/proxy/ir"
)

// EncodeRequest maps IR to Qoder's plaintext JSON envelope. model_config is
// left empty; prepareUpstreamRequest injects the live catalog block and WAF-
// encodes before COSY signs the wire bytes.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("qoder: nil request")
	}
	modelKey := strings.TrimPrefix(strings.TrimSpace(req.Model), "qoder/")
	if modelKey == "" {
		return nil, fmt.Errorf("qoder: empty model")
	}

	msgs, lastUser := encodeMessages(req.Messages)
	tools := encodeTools(req.Tools)

	maxTokens := DefaultMaxTokens
	if req.MaxTokens > 0 && req.MaxTokens < maxTokens {
		maxTokens = req.MaxTokens
	}

	recordID, err := stableChatRecordID(modelKey, req.System, msgs, tools, maxTokens, requestedReasoningIdentity(req.Thinking))
	if err != nil {
		return nil, err
	}
	// Placeholder only: the encoder cannot access the provider. PrepareWireBody
	// replaces this with the account-scoped stableHash("qoder-session",
	// userID, modelKey).
	sessionID := stableHash("qoder-session", modelKey)
	now := time.Now().UnixMilli()
	bizName := lastUser
	if len(bizName) > 30 {
		bizName = bizName[:30]
	}

	out := qoderRequest{
		RequestID:    newUUID(),
		RequestSetID: recordID,
		ChatRecordID: recordID,
		SessionID:    sessionID,
		Stream:       true,
		ChatTask:     "FREE_INPUT",
		IsReply:      true,
		IsRetry:      false,
		Source:       1,
		Version:      "3",
		SessionType:  "qodercli",
		AgentID:      "agent_common",
		TaskID:       "common",
		System:       req.System,
		Messages:     msgs,
		Tools:        tools,
		Parameters:   qoderParameters{MaxTokens: maxTokens},
		ChatContext: qoderChatContext{
			ChatPrompt: "",
			ImageURLs:  nil,
			Extra: map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"key":          modelKey,
					"is_reasoning": false,
				},
				"originalContent": lastUser,
			},
			Features: []any{},
			Text:     lastUser,
		},
		ModelConfig: nil,
		Business: qoderBusiness{
			Product: "cli",
			Version: "1.0.0",
			Type:    "agent",
			Stage:   "start",
			ID:      newUUID(),
			Name:    bizName,
			BeginAt: now,
		},
	}
	// Embed model key for inject lookup without a second pass over IR.
	// inject reads top-level model from chat_context.extra.modelConfig.key.
	return json.Marshal(out)
}

func encodeMessages(msgs []ir.Message) (out []map[string]any, lastUser string) {
	for _, m := range msgs {
		switch m.Role {
		case ir.RoleAssistant:
			msg := map[string]any{"role": "assistant"}
			var text strings.Builder
			var toolCalls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockText:
					text.WriteString(b.Text)
				case ir.BlockToolUse:
					args := string(b.ToolInput)
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   b.ToolID,
						"type": "function",
						"function": map[string]any{
							"name":      b.ToolName,
							"arguments": args,
						},
					})
				}
			}
			if t := text.String(); t != "" {
				msg["content"] = t
			} else {
				msg["content"] = ""
			}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			out = append(out, msg)
		default: // user (+ tool_result blocks)
			// BlockImage/BlockFile are intentionally unhandled: attachment preflight
			// skips this backend before encode so media never reaches here.
			var textParts []string
			for _, b := range m.Content {
				switch b.Type {
				case ir.BlockToolResult:
					content := toolResultPlain(b)
					out = append(out, map[string]any{
						"role":         "tool",
						"tool_call_id": b.ToolUseID,
						"content":      content,
					})
				case ir.BlockText:
					textParts = append(textParts, b.Text)
				}
			}
			if len(textParts) > 0 {
				joined := strings.Join(textParts, "\n")
				out = append(out, map[string]any{"role": "user", "content": joined})
				lastUser = joined
			}
		}
	}
	return out, lastUser
}

func encodeTools(tools []ir.Tool) []map[string]any {
	if len(tools) == 0 {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		params := json.RawMessage(t.Parameters)
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

func toolResultPlain(b ir.ContentBlock) string {
	var parts []string
	for _, c := range b.ToolResult {
		if c.Type == ir.BlockText && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	s := strings.Join(parts, "\n")
	if b.IsError && s == "" {
		return "error"
	}
	return s
}

func stableHash(prefix string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(prefix))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// qoderRecordNamespace is the fixed hash domain for chat_record_id and
// request_set_id. Bump the trailing version only when the identity inputs
// intentionally change, because that invalidates every previously derived
// record id.
const qoderRecordNamespace = "qoder-record-v2"

// recordIdentity is the deterministic projection of the exact content Qoder
// receives. Struct field order fixes the input boundaries; encoding/json sorts
// the keys of the map values inside Messages and Tools, so identical encoded
// content always serializes to identical bytes. Only encoder-selected values
// belong here: randomized request/business ids, timestamps, account session id,
// credentials, provider identity, and the live model_config are excluded. The
// effective reasoning wire controls belong here because prepare writes them
// from the live catalog before the ids are refreshed.
type recordIdentity struct {
	Model     string           `json:"model"`
	System    string           `json:"system"`
	Messages  []map[string]any `json:"messages"`
	Tools     []map[string]any `json:"tools"`
	MaxTokens int              `json:"max_tokens"`
	Reasoning recordReasoning  `json:"reasoning"`
}

// recordReasoning projects the effective reasoning wire controls. State ""
// means no reasoning fields are written, "on" is enabled with Effort, and
// "off" is an explicit disable. none/off spellings and equivalent catalog
// remaps collapse to one value, so equal effective settings keep equal record
// ids while different effective settings never share one.
type recordReasoning struct {
	State  string `json:"state"`
	Effort string `json:"effort"`
}

// stableChatRecordID hashes the already-encoded Qoder content. It takes the
// encoder output, not IR, so it cannot drift from what Qoder actually receives.
func stableChatRecordID(model, system string, msgs, tools []map[string]any, maxTokens int, reasoning recordReasoning) (string, error) {
	payload, err := json.Marshal(recordIdentity{
		Model:     model,
		System:    system,
		Messages:  msgs,
		Tools:     tools,
		MaxTokens: maxTokens,
		Reasoning: reasoning,
	})
	if err != nil {
		return "", fmt.Errorf("qoder: record identity: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(qoderRecordNamespace))
	h.Write([]byte{0})
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// requestedReasoningIdentity projects the requested (not yet catalog-mapped)
// intent for the placeholder record id. Preparation recomputes the identity
// from the final body after the live catalog selects the effective controls.
func requestedReasoningIdentity(t *ir.Thinking) recordReasoning {
	if t == nil {
		return recordReasoning{}
	}
	switch t.Mode {
	case ir.ThinkingNone:
		return recordReasoning{State: "off"}
	case ir.ThinkingLevel:
		lvl := strings.ToLower(strings.TrimSpace(t.Level))
		if lvl == "" {
			return recordReasoning{}
		}
		return recordReasoning{State: "on", Effort: lvl}
	default:
		// auto states no intensity; budget and unknown modes never reach the
		// wire (budget is rejected at prepare).
		return recordReasoning{}
	}
}

// reasoningIdentityFromParams reconstructs the effective wire controls from
// the prepared body so the refreshed identity always describes exactly what
// Qoder receives.
func reasoningIdentityFromParams(params map[string]json.RawMessage) recordReasoning {
	effort := ""
	if raw, ok := params["reasoning_effort"]; ok && len(raw) > 0 {
		_ = json.Unmarshal(raw, &effort)
	}
	enabled, enablePresent := false, false
	if raw, ok := params["enable_thinking"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &enabled); err == nil {
			enablePresent = true
		}
	}
	switch {
	case effort == "none":
		return recordReasoning{State: "off"}
	case !enablePresent:
		if effort == "" {
			return recordReasoning{}
		}
		return recordReasoning{State: "on", Effort: effort}
	case enabled:
		return recordReasoning{State: "on", Effort: effort}
	default:
		return recordReasoning{State: "off"}
	}
}

// ModelKeyFromBody extracts the model key embedded by EncodeRequest.
func ModelKeyFromBody(body []byte) string {
	var env struct {
		ChatContext struct {
			Extra struct {
				ModelConfig struct {
					Key string `json:"key"`
				} `json:"modelConfig"`
			} `json:"extra"`
		} `json:"chat_context"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	return env.ChatContext.Extra.ModelConfig.Key
}
