package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"airouter/internal/proxy/ir"
)

// requestServerBlockTypes is the source-backed request content-block set that
// this codec cannot project as text or a custom tool. qH is the official
// server-type array plus mid_conv_system; search_result is modeled separately
// in the request block union.
var requestServerBlockTypes = map[string]bool{
	"server_tool_use":                        true,
	"web_search_tool_result":                 true,
	"web_fetch_tool_result":                  true,
	"advisor_tool_result":                    true,
	"code_execution_tool_result":             true,
	"bash_code_execution_tool_result":        true,
	"text_editor_code_execution_tool_result": true,
	"tool_search_tool_result":                true,
	"mcp_tool_use":                           true,
	"mcp_tool_result":                        true,
	"container_upload":                       true,
	"compaction":                             true,
	"fallback":                               true,
	"mid_conv_system":                        true,
	"search_result":                          true,
	"tool_reference":                         true,
	"server_tool_result":                     true,
}

// responseServerBlockTypes is the source-backed response block set. It is the
// official server-type array without request-only mid_conv_system.
var responseServerBlockTypes = map[string]bool{
	"server_tool_use":                        true,
	"web_search_tool_result":                 true,
	"web_fetch_tool_result":                  true,
	"advisor_tool_result":                    true,
	"code_execution_tool_result":             true,
	"bash_code_execution_tool_result":        true,
	"text_editor_code_execution_tool_result": true,
	"tool_search_tool_result":                true,
	"mcp_tool_use":                           true,
	"mcp_tool_result":                        true,
	"container_upload":                       true,
	"compaction":                             true,
	"fallback":                               true,
}

func knownServerBlockType(typ string) bool {
	return requestServerBlockTypes[typ] || responseServerBlockTypes[typ]
}

func knownResponseServerBlockType(typ string) bool {
	return responseServerBlockTypes[typ]
}

// decodeCitations validates a text-block citations array. Each element must be
// a JSON object with a non-empty string type. Null and absent become nil. A
// non-array value fails; the search_result {enabled} object is never routed
// here because that block stays a preserved server object.
func decodeCitations(raw json.RawMessage) ([]ir.Citation, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullJSON) {
		return nil, nil
	}
	if trimmed[0] != '[' || !json.Valid(trimmed) {
		return nil, errors.New("citations must be an array")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(trimmed, &raws); err != nil {
		return nil, errors.New("citations must be an array")
	}
	return decodeCitationItems(raws)
}

func decodeCitationItems(raws []json.RawMessage) ([]ir.Citation, error) {
	if len(raws) == 0 {
		return nil, nil
	}
	out := make([]ir.Citation, 0, len(raws))
	for i, raw := range raws {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return nil, fmt.Errorf("citation %d must be an object", i)
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(trimmed, &head); err != nil || head.Type == "" {
			return nil, fmt.Errorf("citation %d must have a type", i)
		}
		out = append(out, ir.Citation{Type: head.Type, Raw: append(json.RawMessage(nil), trimmed...)})
	}
	return out, nil
}

func encodeCitations(citations []ir.Citation) (json.RawMessage, error) {
	if len(citations) == 0 {
		return nil, nil
	}
	out := make([]json.RawMessage, 0, len(citations))
	for i, c := range citations {
		trimmed := bytes.TrimSpace(c.Raw)
		if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
			return nil, ir.InvalidRequest(fmt.Sprintf("citation %d is malformed", i))
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(trimmed, &head); err != nil || head.Type == "" {
			return nil, ir.InvalidRequest(fmt.Sprintf("citation %d has no type", i))
		}
		out = append(out, append(json.RawMessage(nil), trimmed...))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// validateOptionalObject returns nil for absent, null, or an empty object, and
// the original bytes for a validated non-empty JSON object.
func validateOptionalObject(raw json.RawMessage, name string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullJSON) {
		return nil, nil
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, errors.New(name + " must be an object")
	}
	if bytes.Equal(trimmed, []byte("{}")) {
		return nil, nil
	}
	return append(json.RawMessage(nil), trimmed...), nil
}

// contextManagementHasEdits reports whether a validated context_management
// value carries at least one edit. Null, an empty object, and an empty edits
// array are not provider-owned state.
func contextManagementHasEdits(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullJSON) || bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	var obj struct {
		Edits []json.RawMessage `json:"edits"`
	}
	if json.Unmarshal(trimmed, &obj) != nil {
		return true
	}
	return len(obj.Edits) > 0
}

// validateTaskBudget validates the verified {type:"tokens", total, remaining?}
// shape. Total is required; remaining keeps an explicit zero.
func validateTaskBudget(wire *anthTaskBudget) (*ir.AnthropicTaskBudget, error) {
	if wire == nil {
		return nil, nil
	}
	if wire.Type != "tokens" {
		return nil, errors.New(`output_config.task_budget.type must be "tokens"`)
	}
	if wire.Total == nil {
		return nil, errors.New("output_config.task_budget.total is required")
	}
	if *wire.Total < 0 || (wire.Remaining != nil && *wire.Remaining < 0) {
		return nil, errors.New("output_config.task_budget values must not be negative")
	}
	budget := &ir.AnthropicTaskBudget{Total: *wire.Total}
	if wire.Remaining != nil {
		v := *wire.Remaining
		budget.Remaining = &v
	}
	return budget, nil
}

func encodeTaskBudget(b *ir.AnthropicTaskBudget) *anthTaskBudget {
	if b == nil {
		return nil
	}
	total := b.Total
	out := &anthTaskBudget{Type: "tokens", Total: &total}
	if b.Remaining != nil {
		v := *b.Remaining
		out.Remaining = &v
	}
	return out
}

// validateFallbacks accepts the verified server-side fallback selector: the
// string "default" or a non-empty array of objects with a non-empty model.
func validateFallbacks(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullJSON) {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) != nil || s != "default" {
			return nil, errors.New(`fallbacks must be "default" or an array`)
		}
		return append(json.RawMessage(nil), trimmed...), nil
	}
	if trimmed[0] != '[' || !json.Valid(trimmed) {
		return nil, errors.New("fallbacks must be an array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil || len(entries) == 0 {
		return nil, errors.New("fallbacks must be a non-empty array")
	}
	for i, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 || entry[0] != '{' {
			return nil, fmt.Errorf("fallbacks[%d] must be an object", i)
		}
		var obj struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(entry, &obj) != nil || obj.Model == "" {
			return nil, fmt.Errorf("fallbacks[%d].model is required", i)
		}
	}
	return append(json.RawMessage(nil), trimmed...), nil
}

// validateContainer accepts the verified code-execution container selector: a
// string id or an object with an optional string id. Other object fields are
// provider-owned and preserved.
func validateContainer(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullJSON) {
		return nil, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) != nil || s == "" {
			return nil, errors.New("container must be a non-empty string or object")
		}
		return append(json.RawMessage(nil), trimmed...), nil
	case '{':
		if !json.Valid(trimmed) {
			return nil, errors.New("container must be an object")
		}
		var obj struct {
			ID *string `json:"id"`
		}
		if json.Unmarshal(trimmed, &obj) != nil {
			return nil, errors.New("container is malformed")
		}
		if obj.ID != nil && *obj.ID == "" {
			return nil, errors.New("container.id must not be empty")
		}
		return append(json.RawMessage(nil), trimmed...), nil
	default:
		return nil, errors.New("container must be a string or object")
	}
}

// validateResponseState validates the response continuity fields. Container,
// context_management, and stop_details are objects; input_transformations is an
// array. Null and empty values are omitted.
func validateResponseState(container, contextManagement, stopDetails, inputTransformations json.RawMessage) (*ir.AnthropicResponseState, error) {
	c, err := validateOptionalObject(container, "container")
	if err != nil {
		return nil, err
	}
	cm, err := validateOptionalObject(contextManagement, "context_management")
	if err != nil {
		return nil, err
	}
	sd, err := validateOptionalObject(stopDetails, "stop_details")
	if err != nil {
		return nil, err
	}
	it, err := validateOptionalArray(inputTransformations, "input_transformations")
	if err != nil {
		return nil, err
	}
	if c == nil && cm == nil && sd == nil && it == nil {
		return nil, nil
	}
	return &ir.AnthropicResponseState{Container: c, ContextManagement: cm, StopDetails: sd, InputTransformations: it}, nil
}

func validateOptionalArray(raw json.RawMessage, name string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, nullJSON) {
		return nil, nil
	}
	if trimmed[0] != '[' || !json.Valid(trimmed) {
		return nil, errors.New(name + " must be an array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, errors.New(name + " must be an array")
	}
	for i, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 || entry[0] != '{' {
			return nil, fmt.Errorf("%s[%d] must be an object", name, i)
		}
	}
	return append(json.RawMessage(nil), trimmed...), nil
}
