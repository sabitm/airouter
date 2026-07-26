package claudecode

import (
	"io"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

// DecodeStream reads an Anthropic Messages SSE stream and strips the cloak
// suffix from tool names on the way out. It wraps anthropic.DecodeStream; only
// EventToolCallStart carries a tool name, so decloaking there covers both
// translated and stream-collected paths. Text/usage/finish events pass through.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return anthropic.DecodeStream(r, func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventToolCallStart {
			ev.ToolName = DecloakName(ev.ToolName)
		}
		return emit(ev)
	})
}
