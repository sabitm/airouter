package usage

import (
	"context"
	"encoding/binary"
	"math"
	"net/http"
	"time"
)

// gRPC-web frame decoder for xAI GetGrokCreditsConfig
// (grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig).
//
// Response shape:
//   top-level field 1 (length-delimited) — nested credits info
//     subfield 1  (fixed32 float)       — usage ratio 0..1
//     subfield 5  (Timestamp)          — credit-pool reset time
//       seconds field 1, nanos field 2
//
// A fixed32 ratio is the live wire type; fixed64 is tolerated because it still
// carries a valid float in little-endian order.

const grpcWebEmptyFrame = "\x00\x00\x00\x00\x00"

type grokCreditsDecoded struct {
	percentUsed float64
	resetAt     *time.Time
}

func (s *Service) fetchGrokCredits(ctx context.Context, token string) (grokCreditsDecoded, bool) {
	headers := map[string]string{
		"Content-Type": "application/grpc-web+proto",
		"X-Grpc-Web":   "1",
		"Accept":       "application/grpc-web+proto",
	}
	res, err := s.doRequest(ctx, http.MethodPost, GrokGrpcCreditsURL, token, headers, []byte(grpcWebEmptyFrame))
	if err != nil || res.Status != http.StatusOK {
		return grokCreditsDecoded{}, false
	}
	decoded, ok := decodeGrokCreditsFrame(res.Body)
	if !ok {
		return grokCreditsDecoded{}, false
	}
	return decoded, true
}

const (
	gtagTopCredits     = 1
	gtagUsageRatio     = 1
	gtagResetTimestamp = 5
	gtagTimestampSec   = 1
	gtagTimestampNanos = 2
)

const (
	gwireVarint          = 0
	gwireFixed64         = 1
	gwireLengthDelimited = 2
	gwireFixed32         = 5
)

const grpcTrailerBit = 0x80

func readUvarint(b []byte, offset int) (uint64, int, bool) {
	if offset < 0 || offset >= len(b) {
		return 0, offset, false
	}
	value, n := binary.Uvarint(b[offset:])
	if n <= 0 {
		return 0, offset, false
	}
	return value, offset + n, true
}

func probeGrokFrame(b []byte, offset int) (flag byte, payloadStart, payloadLen int, ok bool) {
	if offset < 0 || len(b)-offset < 5 {
		return 0, 0, 0, false
	}
	flag = b[offset]
	if flag != 0x00 && flag != 0x01 && flag != 0x80 && flag != 0x81 {
		return 0, 0, 0, false
	}
	payloadLen = int(binary.BigEndian.Uint32(b[offset+1:]))
	payloadStart = offset + 5
	if payloadLen > len(b)-payloadStart {
		return 0, 0, 0, false
	}
	return flag, payloadStart, payloadLen, true
}

// findGrokDataPayload walks gRPC-web frames, returning the first data-frame
// payload. Trailer frames (flag & 0x80) are skipped.
func findGrokDataPayload(b []byte) []byte {
	offset := 0
	for offset < len(b) {
		flag, start, length, ok := probeGrokFrame(b, offset)
		if !ok {
			return nil
		}
		frameEnd := start + length
		if flag&grpcTrailerBit == 0 {
			return b[start:frameEnd]
		}
		offset = frameEnd
	}
	return nil
}

type grokField struct {
	number   int
	wireType int
	value    uint64
	bytes    []byte
}

func readGrokField(b []byte, offset int) (grokField, int, bool) {
	tag, next, ok := readUvarint(b, offset)
	if !ok {
		return grokField{}, offset, false
	}
	fieldNumber := int(tag >> 3)
	wireType := int(tag & 0x7)
	if fieldNumber == 0 {
		return grokField{}, offset, false
	}
	switch wireType {
	case gwireVarint:
		v, after, ok := readUvarint(b, next)
		if !ok {
			return grokField{}, offset, false
		}
		return grokField{fieldNumber, gwireVarint, v, nil}, after, true
	case gwireFixed64:
		if next+8 > len(b) {
			return grokField{}, offset, false
		}
		return grokField{fieldNumber, gwireFixed64, 0, b[next : next+8]}, next + 8, true
	case gwireFixed32:
		if next+4 > len(b) {
			return grokField{}, offset, false
		}
		return grokField{fieldNumber, gwireFixed32, 0, b[next : next+4]}, next + 4, true
	case gwireLengthDelimited:
		length, bodyStart, ok := readUvarint(b, next)
		if !ok {
			return grokField{}, offset, false
		}
		if length > uint64(len(b)-bodyStart) {
			return grokField{}, offset, false
		}
		end := bodyStart + int(length)
		return grokField{fieldNumber, gwireLengthDelimited, 0, b[bodyStart:end]}, end, true
	default:
		return grokField{}, offset, false
	}
}

func decodeGrokFields(b []byte) map[int]grokField {
	fields := map[int]grokField{}
	offset := 0
	for offset < len(b) {
		f, next, ok := readGrokField(b, offset)
		if !ok {
			return nil
		}
		fields[f.number] = f
		offset = next
	}
	return fields
}

func extractNestedGrok(f grokField, ok bool) map[int]grokField {
	if !ok || f.wireType != gwireLengthDelimited {
		return nil
	}
	return decodeGrokFields(f.bytes)
}

func extractGrokRatio(f grokField, ok bool) (float64, bool) {
	if !ok {
		return 0, true // proto3 omission => 0%
	}
	switch f.wireType {
	case gwireFixed32:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(f.bytes))), true
	case gwireFixed64:
		return math.Float64frombits(binary.LittleEndian.Uint64(f.bytes)), true
	default:
		return 0, false
	}
}

func extractGrokResetAt(f grokField, ok bool) *time.Time {
	if !ok || f.wireType != gwireLengthDelimited {
		return nil
	}
	ts := decodeGrokFields(f.bytes)
	if ts == nil {
		return nil
	}
	var seconds, nanos int64
	if sf, ok := ts[gtagTimestampSec]; ok && sf.wireType == gwireVarint {
		seconds = int64(sf.value)
	}
	if nf, ok := ts[gtagTimestampNanos]; ok && nf.wireType == gwireVarint {
		nanos = int64(nf.value)
	}
	if seconds == 0 && nanos == 0 {
		return nil
	}
	if nanos < 0 || nanos >= int64(time.Second) {
		return nil
	}
	millis := seconds*1000 + (nanos+int64(time.Millisecond)/2)/int64(time.Millisecond)
	t := time.UnixMilli(millis).UTC()
	return &t
}

// decodeGrokCreditsFrame decodes GetGrokCreditsConfig into percent-used/0..100
// plus reset time, or fails open (ok=false) on any malformed input.
func decodeGrokCreditsFrame(b []byte) (grokCreditsDecoded, bool) {
	if len(b) == 0 {
		return grokCreditsDecoded{}, false
	}

	payload := b
	if _, _, _, framed := probeGrokFrame(b, 0); framed {
		if payload = findGrokDataPayload(b); payload == nil {
			return grokCreditsDecoded{}, false
		}
	}

	top := decodeGrokFields(payload)
	if top == nil {
		return grokCreditsDecoded{}, false
	}

	credits := extractNestedGrok(top[gtagTopCredits], true)
	if credits == nil {
		return grokCreditsDecoded{}, false
	}

	ratioF, ratioOK := credits[gtagUsageRatio]
	ratio, ok := extractGrokRatio(ratioF, ratioOK)
	if !ok {
		return grokCreditsDecoded{}, false
	}
	// Omitted ratio is proto3 zero (0% used); negative is malformed.
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
		return grokCreditsDecoded{}, false
	}
	if ratio > 1 {
		ratio = 1
	}
	var resetAt *time.Time
	if rt := extractGrokResetAt(credits[gtagResetTimestamp], true); rt != nil {
		resetAt = rt
	}

	return grokCreditsDecoded{percentUsed: ratio * 100, resetAt: resetAt}, true
}
