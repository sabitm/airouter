package claudecode

import (
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

// DecodeResponse parses an Anthropic Messages response and strips the cloak
// suffix from tool_use names so the client sees its original tool names. It
// wraps anthropic.DecodeResponse; everything else passes through unchanged.
func DecodeResponse(body []byte) (*ir.Response, error) {
	resp, err := anthropic.DecodeResponse(body)
	if err != nil {
		return nil, err
	}
	for i := range resp.Content {
		if resp.Content[i].Type == ir.BlockToolUse {
			resp.Content[i].ToolName = DecloakName(resp.Content[i].ToolName)
		}
	}
	return resp, nil
}
