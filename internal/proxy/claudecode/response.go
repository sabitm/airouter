package claudecode

import (
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

// DecodeResponse parses an Anthropic Messages response. The proxy preserves
// caller tool names and does not apply the CLI's conditional MCP name mapping.
func DecodeResponse(body []byte) (*ir.Response, error) {
	return anthropic.DecodeResponse(body)
}
