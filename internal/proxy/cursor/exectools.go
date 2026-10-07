package cursor

import (
	"strings"

	"airouter/internal/proxy/ir"
)

// ExecServerMessage fields that are not a client-visible tool oneof.
const (
	esmSpanContext                 = 19
	esmAcceptHookAdditionalContext = 55
)

// execControlFields are protocol/control oneofs, not tools to reject.
// Any other field on an ExecServerMessage is a Cursor built-in exec. Those
// are rejected and never surfaced as IR tool_use.
var execControlFields = map[int]bool{
	esmID:                          true,
	esmExecID:                      true,
	esmRequestContextArgs:          true,
	esmMCPArgs:                     true,
	esmSpanContext:                 true,
	esmAcceptHookAdditionalContext: true,
}

// MCPAvailabilityNote tells the model that only the declared MCP tools exist.
// Names come from the current request, not a hardcoded catalog.
func MCPAvailabilityNote(tools []ir.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "You have no Cursor built-in tools in this session. Your only tools are the MCP tools: " +
		strings.Join(names, ", ") + ". Always call those by name."
}
