package claudecode

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
)

// Seeded xxHash64 primes and the Claude Code 2.1.318 CCH seed. The low 20
// bits of the digest are the five checksum digits. This is not the cc_version
// fingerprint.
const (
	cchSeed uint64 = 0x4D659218E32A3268
	cchP1   uint64 = 0x9E3779B185EBCA87
	cchP2   uint64 = 0xC2B2AE3D27D4EB4F
	cchP3   uint64 = 0x165667B19E3779F9
	cchP4   uint64 = 0x85EBCA77C2B2AE63
	cchP5   uint64 = 0x27D4EB2F165667C5
)

const cchWindow = 300

// Native marker lengths are part of the scan. Do not trim or JSON-parse them.
var (
	cchSystem      = []byte(`"system":[`)
	cchPlaceholder = []byte("cch=00000")
	cchModel       = []byte(`"model":"`)
	cchFallbacks   = []byte(`"fallbacks":[`)
	cchCredit      = []byte(`"fallback_credit_token":"`)
	cchMaxTokens   = []byte(`"max_tokens":`)
	cchURLNeedle   = "/v1/messages"
)

// ApplyCCH returns body with one in-window cch=00000 replaced by the checksum
// of the raw included bytes. upstreamURL is the resolved send URL. Activation
// is a case-sensitive substring check for /v1/messages, then the first compact
// system marker, then the first cch=00000 that fits in the next 300 bytes.
//
// The exclusion scan is not a JSON parser. A backslash does not escape a
// quote. A comma before a field is dropped only when that field has no
// trailing comma. Model values, integer max_tokens, fallbacks arrays, and
// fallback_credit_token values are the only exclusions.
//
// The input buffer is never written. A no-op returns that same buffer. One
// call changes one placeholder. A later call can patch the next cch=00000 in
// the same window, so the result is not an idempotent transform. A span that
// does not advance returns the input unchanged; a prepared body does not hit
// that path, and the proxy must not loop.
//
// This is the plaintext fragment rule only. It does not model gzip, random
// padding, HTTP/2, or provider acceptance.
func ApplyCCH(upstreamURL string, body []byte) []byte {
	at := cchActivate(upstreamURL, body)
	if at < 0 || at+len(cchPlaceholder) > len(body) {
		return body
	}
	included, ok := cchIncluded(body)
	if !ok {
		return body
	}
	out := bytes.Clone(body)
	copy(out[at+4:at+9], cchDigits(cchXXH64(included)))
	return out
}

func cchDigits(digest uint64) []byte {
	return []byte(fmt.Sprintf("%05x", digest&0xFFFFF))
}

func cchActivate(upstreamURL string, body []byte) int {
	if len(upstreamURL) <= 11 || !bytes.Contains([]byte(upstreamURL), []byte(cchURLNeedle)) {
		return -1
	}
	if len(body) < len(cchSystem) {
		return -1
	}
	marker := bytes.Index(body, cchSystem)
	if marker < 0 {
		return -1
	}
	windowEnd := marker + cchWindow
	if windowEnd > len(body) {
		windowEnd = len(body)
	}
	return cchFind(body, cchPlaceholder, marker, windowEnd)
}

func cchFind(body, needle []byte, start, end int) int {
	if start < 0 {
		start = 0
	}
	if end > len(body) {
		end = len(body)
	}
	if start >= end || len(needle) == 0 || end-start < len(needle) {
		return -1
	}
	rel := bytes.Index(body[start:end], needle)
	if rel < 0 {
		return -1
	}
	return start + rel
}

func cchFindFrom(body, needle []byte, start int) int {
	if start < 0 {
		start = 0
	}
	if start > len(body) || len(needle) == 0 {
		return -1
	}
	rel := bytes.Index(body[start:], needle)
	if rel < 0 {
		return -1
	}
	return start + rel
}

type cchSpan struct {
	until  int
	resume int
}

func cchSpanFrom(body []byte, cursor, marker, contentEnd int) cchSpan {
	// A trailing comma suppresses the leading-comma adjustment. The native
	// helper jumps over that check after consuming the trailing comma.
	if contentEnd < len(body) && body[contentEnd] == ',' {
		return cchSpan{until: marker, resume: contentEnd + 1}
	}
	until := marker
	if marker > cursor && body[marker-1] == ',' {
		until = marker - 1
	}
	return cchSpan{until: until, resume: contentEnd}
}

func cchScanBrackets(body []byte, index int) (int, bool) {
	depth := 1
	inString := false
	for index < len(body) {
		b := body[index]
		if inString {
			if b == '"' {
				inString = false
				index++
				continue
			}
			// One escaped byte does not end the string. This is not a JSON parser.
			if b == '\\' && index+1 < len(body) {
				index += 2
				continue
			}
			index++
			continue
		}
		switch b {
		case '"':
			inString = true
			index++
		case '[':
			depth++
			index++
		case ']':
			depth--
			if depth == 0 {
				return index, true
			}
			index++
		default:
			index++
		}
	}
	return 0, false
}

func cchExclusion(body []byte, cursor int) (cchSpan, bool) {
	size := len(body)
	if cursor >= size || size-cursor < 13 {
		return cchSpan{}, false
	}
	var best cchSpan
	found := false
	if size-cursor > 12 {
		if marker := cchFindFrom(body, cchFallbacks, cursor); marker >= 0 && marker+13 < size {
			if end, ok := cchScanBrackets(body, marker+13); ok {
				best = cchSpanFrom(body, cursor, marker, end+1)
				found = true
			}
		}
	}
	if size-cursor >= 25 {
		if marker := cchFindFrom(body, cchCredit, cursor); marker >= 0 {
			if quote := cchFindFrom(body, []byte{'"'}, marker+25); quote >= 0 {
				span := cchSpanFrom(body, cursor, marker, quote+1)
				if !found || span.until < best.until {
					best = span
					found = true
				}
			}
		}
	}
	search := cursor
	for size-search >= 13 {
		marker := cchFindFrom(body, cchMaxTokens, search)
		if marker < 0 || marker+13 >= size {
			break
		}
		index := marker + 13
		for index < size && body[index] >= '0' && body[index] <= '9' {
			index++
		}
		if index == marker+13 {
			search = marker + 13
			continue
		}
		span := cchSpanFrom(body, cursor, marker, index)
		if !found || span.until < best.until {
			best = span
			found = true
		}
		break
	}
	if !found {
		return cchSpan{}, false
	}
	return best, true
}

func cchIncluded(body []byte) ([]byte, bool) {
	size := len(body)
	cursor := 0
	parts := make([][]byte, 0, 4)
	for steps := 0; ; steps++ {
		if steps > size+2 {
			return nil, false
		}
		span, found := cchExclusion(body, cursor)
		model := -1
		if size-cursor >= len(cchModel) {
			if marker := cchFindFrom(body, cchModel, cursor); marker >= 0 {
				model = marker
			}
		}
		if found && !(model >= 0 && span.until >= model) {
			if span.resume <= cursor || span.until < cursor || span.until > size || span.resume > size {
				return nil, false
			}
			parts = append(parts, body[cursor:span.until])
			cursor = span.resume
			continue
		}
		if model < 0 {
			parts = append(parts, body[cursor:])
			break
		}
		// Model skip stops at the first raw quote. It does not treat backslash as an escape.
		quote := cchFindFrom(body, []byte{'"'}, model+len(cchModel))
		if quote < 0 {
			parts = append(parts, body[cursor:])
			break
		}
		parts = append(parts, body[cursor:model+len(cchModel)])
		cursor = quote
	}
	return bytes.Join(parts, nil), true
}

func cchRound(acc, lane uint64) uint64 {
	acc += lane * cchP2
	acc = bits.RotateLeft64(acc, 31)
	return acc * cchP1
}

func cchXXH64(data []byte) uint64 {
	size := len(data)
	var digest uint64
	index := 0
	if size >= 32 {
		// Add in variables. Constant uint64 expressions do not wrap.
		var accs [4]uint64
		accs[0] = cchSeed
		accs[0] += cchP1
		accs[0] += cchP2
		accs[1] = cchSeed
		accs[1] += cchP2
		accs[2] = cchSeed
		accs[3] = cchSeed
		accs[3] -= cchP1
		for index+32 <= size {
			for lane := 0; lane < 4; lane++ {
				start := index + 8*lane
				accs[lane] = cchRound(accs[lane], binary.LittleEndian.Uint64(data[start:start+8]))
			}
			index += 32
		}
		digest = bits.RotateLeft64(accs[0], 1) + bits.RotateLeft64(accs[1], 7) + bits.RotateLeft64(accs[2], 12) + bits.RotateLeft64(accs[3], 18)
		for _, acc := range accs {
			digest ^= cchRound(0, acc)
			digest = digest*cchP1 + cchP4
		}
		index = (size / 32) * 32
	} else {
		digest = cchSeed + cchP5
	}
	digest += uint64(size)
	for index+8 <= size {
		lane := binary.LittleEndian.Uint64(data[index : index+8])
		digest ^= cchRound(0, lane)
		digest = bits.RotateLeft64(digest, 27)*cchP1 + cchP4
		index += 8
	}
	if index+4 <= size {
		lane := uint64(binary.LittleEndian.Uint32(data[index : index+4]))
		digest ^= lane * cchP1
		digest = bits.RotateLeft64(digest, 23)*cchP2 + cchP3
		index += 4
	}
	for index < size {
		digest ^= uint64(data[index]) * cchP5
		digest = bits.RotateLeft64(digest, 11) * cchP1
		index++
	}
	digest ^= digest >> 33
	digest *= cchP2
	digest ^= digest >> 29
	digest *= cchP3
	digest ^= digest >> 32
	return digest
}
