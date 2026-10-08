package cursor

import (
	"encoding/json"
	"fmt"
	"strings"

	"airouter/internal/proxy/ir"
)

// Error-frame parsing shared by the AgentService stream decoder. Cursor's
// Connect end-stream errors arrive as JSON in a trailer-flagged frame; the
// details array carries the human title/detail (e.g. "Free plans can only use
// Auto"). parseCursorError always returns a Go error, including after content
// has streamed, so the proxy can emit an ingress error frame without
// fabricating a Finish.

// cursorErrorEnvelope is the shape of Cursor's JSON error frames.
type cursorErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Debug struct {
				Details struct {
					Title  string `json:"title"`
					Detail string `json:"detail"`
					Error  string `json:"error"`
				} `json:"details"`
			} `json:"debug"`
		} `json:"details"`
	} `json:"error"`
}

func isCursorError(data []byte) bool {
	// Historical unflagged Cursor JSON error frames start with an object that
	// carries a non-null error. A flagged end-stream is validated separately:
	// {"error":null} and a metadata key named "error" are not failures.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	errRaw, ok := raw["error"]
	if !ok || isJSONNull(errRaw) {
		return false
	}
	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(errRaw, &errObj); err != nil {
		return false
	}
	return true
}

// endStreamError validates one Connect EndStreamResponse. Official transport
// requires a non-null JSON object, not an array or primitive. Metadata, when
// present, is a non-null object whose values are arrays of strings. An absent
// or null error is success. A non-null error is an object and is forwarded
// through the existing Cursor error parser. The payload is never logged.
func endStreamError(data []byte) error {
	if len(data) == 0 {
		return ir.ProtocolError("cursor: missing end-stream frame")
	}
	var top any
	if err := json.Unmarshal(data, &top); err != nil {
		return ir.ProtocolError("cursor: malformed end-stream frame")
	}
	raw, ok := top.(map[string]any)
	if !ok || raw == nil {
		return ir.ProtocolError("cursor: malformed end-stream frame")
	}
	if meta, ok := raw["metadata"]; ok {
		headers, ok := meta.(map[string]any)
		if !ok || headers == nil {
			return ir.ProtocolError("cursor: malformed end-stream frame")
		}
		for _, values := range headers {
			list, ok := values.([]any)
			if !ok {
				return ir.ProtocolError("cursor: malformed end-stream frame")
			}
			for _, value := range list {
				if _, ok := value.(string); !ok {
					return ir.ProtocolError("cursor: malformed end-stream frame")
				}
			}
		}
	}
	errVal, ok := raw["error"]
	if !ok || errVal == nil {
		return nil
	}
	errObj, ok := errVal.(map[string]any)
	if !ok || errObj == nil {
		return ir.ProtocolError("cursor: malformed end-stream frame")
	}
	if msg, ok := errObj["message"]; ok && msg != nil {
		if _, ok := msg.(string); !ok {
			return ir.ProtocolError("cursor: malformed end-stream frame")
		}
	}
	if code, ok := errObj["code"]; ok && code != nil {
		if _, ok := code.(string); !ok {
			return ir.ProtocolError("cursor: malformed end-stream frame")
		}
	}
	if details, ok := errObj["details"]; ok && details != nil {
		items, ok := details.([]any)
		if !ok {
			return ir.ProtocolError("cursor: malformed end-stream frame")
		}
		for _, item := range items {
			detail, ok := item.(map[string]any)
			if !ok || detail == nil {
				return ir.ProtocolError("cursor: malformed end-stream frame")
			}
			if _, ok := detail["type"].(string); !ok {
				return ir.ProtocolError("cursor: malformed end-stream frame")
			}
			if _, ok := detail["value"].(string); !ok {
				return ir.ProtocolError("cursor: malformed end-stream frame")
			}
		}
	}
	return parseCursorError(data)
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func parseCursorError(data []byte) error {
	var env cursorErrorEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Do not embed the raw frame body; terminal logs must stay metadata-only.
		return &ir.StreamFailure{Message: "upstream stream failed"}
	}
	msg := env.Error.Message
	if len(env.Error.Details) > 0 {
		d := env.Error.Details[0].Debug.Details
		if msg == "" || msg == "Error" {
			msg = d.Title
			if msg == "" {
				msg = d.Detail
			}
		}
	}
	sf := &ir.StreamFailure{Code: env.Error.Code, Message: msg}
	if env.Error.Code == "resource_exhausted" {
		sf.Type = "resource_exhausted"
		if sf.Message == "" {
			sf.Message = "rate limit exceeded"
		}
		return sf
	}
	if sf.Message == "" {
		sf.Message = "upstream stream failed"
	} else {
		sf.Message = fmt.Sprintf("cursor: %s", sf.Message)
	}
	return sf
}

// decloakToolName strips the "mcp_custom_" prefix Cursor may add to MCP tool
// names so the IR (and the client) sees the original tool name.
func decloakToolName(name string) string {
	if strings.HasPrefix(name, "mcp_custom_") {
		return name[len("mcp_custom_"):]
	}
	return name
}
