package claudecode

import (
	"io"

	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

// DecodeStream reads a Claude Code Messages SSE stream without renaming tools.
// Strict completion rejects a truncated stream instead of synthesizing success.
// Reported usage merges start and delta counters. These options are
// independent. They are bounded proxy rules, not full CLI or provider conformance.
func DecodeStream(r io.Reader, emit func(ir.StreamEvent) error) error {
	return anthropic.DecodeStreamWithOptions(r, emit, anthropic.DecodeStreamOptions{
		StrictCompletion: true,
		ReportedUsage:    true,
	})
}
