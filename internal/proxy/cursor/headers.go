package cursor

import (
	"net/http"

	"github.com/google/uuid"
)

// BuildHeaders constructs the Cursor CLI identity header set for an upstream
// request. token is the raw pasted access token (any "::" prefix is stripped);
// machineID is the machine id (falls back to a token-derived hash when empty,
// so the header is well-formed even without a paste). ghost toggles
// x-ghost-mode.
func BuildHeaders(token, machineID string, ghost bool) http.Header {
	clean := stripColonPrefix(token)
	mid := machineID
	if mid == "" {
		mid = machineIDFallback(clean)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+clean)
	h.Set("Connect-Accept-Encoding", "gzip")
	h.Set("Connect-Protocol-Version", "1")
	h.Set("Content-Type", ConnectContentType)
	h.Set("User-Agent", UserAgent)
	h.Set("X-Client-Key", clientKey(clean))
	h.Set("X-Cursor-Checksum", generateChecksum(mid))
	h.Set("X-Cursor-Client-Type", ClientType)
	h.Set("X-Cursor-Client-Version", ClientVersion)
	h.Set("X-Ghost-Mode", ghostModeStr(ghost))
	h.Set("X-Request-Id", uuid.NewString())
	h.Set("X-Session-Id", sessionID(clean))
	return h
}

// BuildModelsHeaders is like BuildHeaders but for the unary GetUsableModels
// call, which uses unframed application/proto: it drops the connect framing
// headers (connect-accept-encoding, connect-protocol-version) and sets the
// unframed accept/content-type.
func BuildModelsHeaders(token, machineID string, ghost bool) http.Header {
	h := BuildHeaders(token, machineID, ghost)
	h.Del("Connect-Accept-Encoding")
	h.Del("Connect-Protocol-Version")
	h.Set("Accept", ProtoContentType)
	h.Set("Content-Type", ProtoContentType)
	return h
}

func ghostModeStr(ghost bool) string {
	if ghost {
		return "true"
	}
	return "false"
}
