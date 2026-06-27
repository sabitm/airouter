package oauth

import (
	"encoding/json"
	"io"
)

// readLimited reads up to 1 MiB from r. Token responses are small; the cap
// guards against a misbehaving or hostile token endpoint.
func readLimited(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, 1<<20))
}

// jsonUnmarshal decodes b into v, wrapping the error with context at call sites.
func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
