package grokbuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"airouter/internal/proxy/ir"
	"airouter/internal/proxy/responses"
	"airouter/internal/proxy/thinking"
)

const encryptedInclude = "reasoning.encrypted_content"

// EncodeRequest renders a Responses body for the subscription proxy.
// It reuses the shared Responses input and tool encoding. It does not apply
// Codex or OpenAI effort defaults. store is forced false because the official
// sampler sets that when the field is absent, and this proxy cannot resolve
// server-stored item ids. stream is forced true because the backend is
// stream-only. reasoning.summary is not added: the official sampler adds it
// only from model config this pass does not have.
func EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, errors.New("grok-build: empty request")
	}
	raw, err := responses.EncodeRequest(req)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["stream"] = true
	doc["store"] = false
	if err := applyEffort(doc, req.Thinking); err != nil {
		return nil, err
	}
	ensureInclude(doc, encryptedInclude)
	return json.Marshal(doc)
}

func applyEffort(doc map[string]any, t *ir.Thinking) error {
	level, ok, err := effortFromThinking(t)
	if err != nil {
		return err
	}
	if !ok {
		delete(doc, "reasoning")
		return nil
	}
	doc["reasoning"] = map[string]any{"effort": level}
	return nil
}

// effortFromThinking returns a recognized effort. Absent intent and "auto" omit
// the field so the account's server default applies. An explicit unknown level
// fails instead of being rewritten to a local default.
func effortFromThinking(t *ir.Thinking) (string, bool, error) {
	cfg := thinking.FromIR(t)
	if cfg == nil {
		return "", false, nil
	}
	if cfg.Mode == thinking.ModeNone || cfg.Enable == thinking.EnableDisabled {
		return "none", true, nil
	}
	if cfg.Mode == thinking.ModeAuto {
		return "", false, nil
	}
	level := strings.ToLower(strings.TrimSpace(thinking.LevelFor(cfg)))
	if level == "" {
		level = strings.ToLower(strings.TrimSpace(cfg.Effort))
	}
	if level == "" || level == "auto" {
		return "", false, nil
	}
	if !recognizedEffort(level) {
		return "", false, errors.New("grok-build: reasoning effort is not a recognized level")
	}
	return level, true, nil
}

func ensureInclude(doc map[string]any, want string) {
	var items []string
	if raw, ok := doc["include"].([]any); ok {
		for _, item := range raw {
			s, ok := item.(string)
			if ok && s != "" {
				items = append(items, s)
			}
		}
	}
	for _, s := range items {
		if s == want {
			doc["include"] = items
			return
		}
	}
	doc["include"] = append(items, want)
}

// RejectUnsupportedIngress fails closed when the original ingress body asks for
// a feature this pass cannot represent under store=false. Call it on the
// accepted ingress bytes before a lossy decode. Hosted tools, custom tools,
// server item references, and previous_response_id are dropped by the shared
// Responses decoder, so they must be rejected before that drop.
func RejectUnsupportedIngress(body []byte) error {
	body = bytes.TrimLeft(body, " \t\r\n")
	if len(body) == 0 || body[0] != '{' {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return nil
	}
	if raw, ok := root["previous_response_id"]; ok && nonemptyJSONValue(raw) {
		return errors.New("grok-build: previous_response_id is not supported when store is false")
	}
	if raw, ok := root["tools"]; ok && len(bytes.TrimSpace(raw)) > 0 && raw[0] == '[' {
		var tools []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &tools) == nil {
			for _, tool := range tools {
				typ := strings.TrimSpace(tool.Type)
				if typ != "" && typ != "function" {
					return errors.New("grok-build: hosted or custom tools are not supported")
				}
			}
		}
	}
	raw, ok := root["input"]
	if !ok || len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	for _, item := range items {
		item = bytes.TrimLeft(item, " \t\r\n")
		if len(item) == 0 {
			continue
		}
		if item[0] == '"' {
			var s string
			if json.Unmarshal(item, &s) == nil && strings.TrimSpace(s) != "" {
				return errors.New("grok-build: server-stored item references are not supported when store is false")
			}
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &head) != nil {
			continue
		}
		if unsupportedInputItem(head.Type) {
			return errors.New("grok-build: server-stored or custom tool items are not supported")
		}
	}
	return nil
}

func nonemptyJSONValue(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return true
		}
		return strings.TrimSpace(s) != ""
	}
	return true
}

func unsupportedInputItem(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "item_reference", "custom_tool_call", "custom_tool_call_output",
		"web_search_call", "file_search_call", "computer_call", "computer_call_output",
		"mcp_call", "mcp_call_output", "mcp_list_tools", "mcp_approval_request",
		"mcp_approval_response", "image_generation_call", "code_interpreter_call",
		"local_shell_call", "shell_call", "apply_patch_call", "tool_search_call":
		return true
	default:
		return false
	}
}
