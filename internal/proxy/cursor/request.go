package cursor

import (
	"strings"

	"airouter/internal/proxy/ir"
)

// toolResultText flattens a tool-result block's content blocks to text. An
// all-empty or media-only result degrades to a placeholder so transcript lines
// stay informative.
func toolResultText(b ir.ContentBlock) string {
	var parts []string
	for _, c := range b.ToolResult {
		if c.Type == ir.BlockText && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	s := strings.Join(parts, "\n")
	if s == "" {
		if b.IsError {
			return "error"
		}
		return "[empty tool result]"
	}
	return s
}
