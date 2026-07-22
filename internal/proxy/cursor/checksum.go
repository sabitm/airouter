package cursor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

// nowMillis is overridable in tests to pin the timestamp for golden vectors.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// checksumAt computes the x-cursor-checksum header value for a given timestamp
// and machine id, using the jyh cipher from cursorChecksum.js (authoritative;
// the older cipher in oauth/services/cursor.js omits the +i%256 term and must
// not be used). ts is unix milliseconds / 1e6, encoded as 6 big-endian bytes.
// The cipher: key starts at 165; for each byte i,
//
//	b = (b ^ key + i%256) & 0xff; key = b
//
// then URL-safe base64 (no padding) is appended with the machine id verbatim.
func checksumAt(ts int64, machineID string) string {
	v := uint64(ts)
	var six [6]byte
	six[0] = byte(v >> 40)
	six[1] = byte(v >> 32)
	six[2] = byte(v >> 24)
	six[3] = byte(v >> 16)
	six[4] = byte(v >> 8)
	six[5] = byte(v)

	key := byte(165)
	for i := 0; i < 6; i++ {
		six[i] = (six[i] ^ key + byte(i%256)) & 0xff
		key = six[i]
	}
	return base64.RawURLEncoding.EncodeToString(six[:]) + machineID
}

// generateChecksum computes the header value for the current time.
func generateChecksum(machineID string) string {
	return checksumAt(nowMillis()/1e6, machineID)
}

// clientKey is the x-client-key header: sha256 hex of the (clean) access token.
func clientKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sessionID is the x-session-id header: UUID v5 (DNS namespace) of the token.
func sessionID(token string) string {
	return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(token)).String()
}

// stripColonPrefix removes a "::"-prefixed segment, e.g. "sess::tok" -> "tok".
// Cursor tokens sometimes carry a session prefix; the bearer credential is the
// part after the last "::".
func stripColonPrefix(token string) string {
	for i := len(token) - 1; i > 0; i-- {
		if token[i] == ':' && token[i-1] == ':' {
			return token[i+1:]
		}
	}
	return token
}

// machineIDFallback derives a stable machine id from the token when the user did
// not paste one (generateHashed64Hex(token, "machineId") in 9router).
func machineIDFallback(token string) string {
	h := sha256.New()
	h.Write([]byte(token))
	h.Write([]byte("machineId"))
	return hex.EncodeToString(h.Sum(nil))
}
