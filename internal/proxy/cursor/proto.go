package cursor

import "fmt"

// encodeVarint writes a base-128 varint (little-endian, 7 bits per byte, MSB
// continuation). Matches the protobuf wire varint encoding.
func encodeVarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// decodeVarint reads a varint at off, returning the value and the new offset.
func decodeVarint(b []byte, off int) (uint64, int, error) {
	var result uint64
	var shift uint
	for {
		if off >= len(b) {
			return 0, off, fmt.Errorf("cursor: varint truncated")
		}
		cur := b[off]
		off++
		result |= uint64(cur&0x7f) << shift
		if cur < 0x80 {
			return result, off, nil
		}
		shift += 7
		if shift > 63 {
			return 0, off, fmt.Errorf("cursor: varint overflow")
		}
	}
}

// encodeField encodes one tagged field. value is a string, []byte, or uint64.
// For wireLen types the length prefix is emitted; for wireVarint the value is
// the varint itself.
func encodeField(fieldNum, wireType int, value any) []byte {
	tag := uint64(fieldNum)<<3 | uint64(wireType)
	tagBytes := encodeVarint(tag)
	switch v := value.(type) {
	case string:
		data := []byte(v)
		return concatBytes(tagBytes, encodeVarint(uint64(len(data))), data)
	case []byte:
		return concatBytes(tagBytes, encodeVarint(uint64(len(v))), v)
	case uint64:
		return concatBytes(tagBytes, encodeVarint(v))
	case int:
		return concatBytes(tagBytes, encodeVarint(uint64(v)))
	default:
		return tagBytes
	}
}

// concatBytes joins byte slices without allocation chaining overhead concerns.
func concatBytes(parts ...[]byte) []byte {
	var n int
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// field holds one decoded field occurrence.
type field struct {
	wireType int
	value    []byte // for LEN; for VARINT the raw varint bytes (decodeVarint on demand)
}

// decodeMessage parses a length-delimited protobuf message into a field-number
// -> []field map. Unknown fields are retained (schema drift tolerance).
func decodeMessage(data []byte) (map[int][]field, error) {
	out := map[int][]field{}
	off := 0
	for off < len(data) {
		tag, n, err := decodeVarint(data, off)
		if err != nil {
			return nil, err
		}
		off = n
		fieldNum := int(tag >> 3)
		wt := int(tag & 0x07)
		switch wt {
		case wireVarint:
			val, n2, err := decodeVarint(data, off)
			if err != nil {
				return nil, err
			}
			off = n2
			out[fieldNum] = append(out[fieldNum], field{wireType: wt, value: encodeVarint(val)})
		case wireLen:
			ln, n2, err := decodeVarint(data, off)
			if err != nil {
				return nil, err
			}
			off = n2
			if off+int(ln) > len(data) {
				return nil, fmt.Errorf("cursor: field %d length %d exceeds buffer", fieldNum, ln)
			}
			out[fieldNum] = append(out[fieldNum], field{wireType: wt, value: data[off : off+int(ln)]})
			off += int(ln)
		case wireFixed64:
			if off+8 > len(data) {
				return nil, fmt.Errorf("cursor: fixed64 truncated")
			}
			out[fieldNum] = append(out[fieldNum], field{wireType: wt, value: data[off : off+8]})
			off += 8
		case wireFixed32:
			if off+4 > len(data) {
				return nil, fmt.Errorf("cursor: fixed32 truncated")
			}
			out[fieldNum] = append(out[fieldNum], field{wireType: wt, value: data[off : off+4]})
			off += 4
		default:
			return nil, fmt.Errorf("cursor: unsupported wire type %d", wt)
		}
	}
	return out, nil
}

// stringField returns the first LEN field's bytes as a string, ok=false when
// absent.
func stringField(m map[int][]field, num int) (string, bool) {
	f, ok := m[num]
	if !ok || len(f) == 0 {
		return "", false
	}
	return string(f[0].value), true
}

// varintField returns the first varint field's value.
func varintField(m map[int][]field, num int) (uint64, bool) {
	f, ok := m[num]
	if !ok || len(f) == 0 || f[0].wireType != wireVarint {
		return 0, false
	}
	v, _, err := decodeVarint(f[0].value, 0)
	if err != nil {
		return 0, false
	}
	return v, true
}
