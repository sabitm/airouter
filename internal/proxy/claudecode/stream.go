package claudecode

import (
	"io"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

// DecodeStream reads an Anthropic Messages SSE stream without renaming tools.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return anthropic.DecodeStream(r, emit)
}
