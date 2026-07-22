package cursor

import (
	"net/http"
	"runtime"
	"time"

	"github.com/google/uuid"
)

// BuildHeaders constructs the full Cursor identity header set for an upstream
// request. token is the raw pasted access token (any "::" prefix is stripped);
// machineID is the IDE machine id (falls back to a token-derived hash when
// empty, so the header is well-formed even without a paste). ghost toggles
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
	h.Set("X-Amzn-Trace-Id", "Root="+uuid.NewString())
	h.Set("X-Client-Key", clientKey(clean))
	h.Set("X-Cursor-Checksum", generateChecksum(mid))
	h.Set("X-Cursor-Client-Version", ClientVersion)
	h.Set("X-Cursor-Client-Commit", ClientCommit)
	h.Set("X-Cursor-Client-Type", "ide")
	h.Set("X-Cursor-Client-OS", osName())
	h.Set("X-Cursor-Client-Arch", archName())
	h.Set("X-Cursor-Client-Device-Type", "desktop")
	h.Set("X-Cursor-Config-Version", uuid.NewString())
	h.Set("X-Cursor-Timezone", timezoneName())
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

func osName() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func archName() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	default:
		return "x86_64"
	}
}

func timezoneName() string {
	// Cursor sends an IANA zone; fall back to UTC when the local zone name is
	// empty or the generic "Local" placeholder (minimal containers).
	if z := time.Local.String(); z != "" && z != "Local" {
		return z
	}
	return "UTC"
}

func ghostModeStr(ghost bool) string {
	if ghost {
		return "true"
	}
	return "false"
}
