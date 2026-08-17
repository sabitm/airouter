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
	// Cheap check before a full JSON unmarshal: must contain "error".
	return len(data) > 10 && strings.Contains(string(data), "\"error\"")
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
