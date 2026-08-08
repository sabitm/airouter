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

	recordID := stableChatRecordID(modelKey, req.Messages, tools, maxTokens)
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

func stableChatRecordID(model string, msgs []ir.Message, tools []map[string]any, maxTokens int) string {
	h := sha256.New()
	h.Write([]byte("qoder-record\x00"))
	h.Write([]byte(model))
	for _, m := range msgs {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		for _, b := range m.Content {
			if b.Type == ir.BlockText {
				h.Write([]byte{0})
				h.Write([]byte(b.Text))
			}
		}
	}
	if len(tools) > 0 {
		raw, _ := json.Marshal(tools)
		h.Write([]byte{0})
		h.Write(raw)
	}
	fmt.Fprintf(h, "\x00mt=%d", maxTokens)
	return hex.EncodeToString(h.Sum(nil))[:16]
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
