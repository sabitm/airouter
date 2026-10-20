package claudecode

import (
	"airouter/internal/proxy/anthropic"
	"airouter/internal/proxy/ir"
)

// DecodeResponse parses an Anthropic Messages response. Tool names are not
// rewritten. The 2.1.303 CLI does not append _ide, and a name that already
// ends in _ide is a caller name.
func DecodeResponse(body []byte) (*ir.Response, error) {
	return anthropic.DecodeResponse(body)
}
