package cursor

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeVarint(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 16383, 16384, 1<<32 - 1, 1<<63 - 1}
	for _, v := range cases {
		enc := encodeVarint(v)
		got, n, err := decodeVarint(enc, 0)
		if err != nil {
			t.Fatalf("decode %d: %v", v, err)
		}
		if got != v {
			t.Errorf("roundtrip %d -> %d", v, got)
		}
		if n != len(enc) {
			t.Errorf("consumed %d, want %d for %d", n, len(enc), v)
		}
	}
}

func TestEncodeFieldString(t *testing.T) {
	b := encodeField(5, wireLen, "hello")
	// tag = (5<<3)|2 = 42, len=5, then bytes
	want := []byte{42, 5, 'h', 'e', 'l', 'l', 'o'}
	if !bytes.Equal(b, want) {
		t.Errorf("got %v want %v", b, want)
	}
}

func TestEncodeFieldVarint(t *testing.T) {
	b := encodeField(27, wireVarint, uint64(1))
	// tag = (27<<3)|0 = 216 -> varint [216, 1]; value 1 -> [1]
	want := []byte{216, 1, 1}
	if !bytes.Equal(b, want) {
		t.Errorf("got %v want %v", b, want)
	}
}

func TestDecodeMessageNested(t *testing.T) {
	// inner: field 1 = "hi"
	inner := encodeField(1, wireLen, "hi")
	// outer: field 2 (LEN) = inner; field 3 (VARINT) = 7
	outer := concatBytes(encodeField(2, wireLen, inner), encodeField(3, wireVarint, uint64(7)))
	m, err := decodeMessage(outer)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := stringField(m, 2); !ok || s != string(inner) {
		t.Errorf("field 2 = %q ok=%v", s, ok)
	}
	if v, ok := varintField(m, 3); !ok || v != 7 {
		t.Errorf("field 3 = %d ok=%v", v, ok)
	}
	// recurse into the inner message
	innerMap, err := decodeMessage([]byte(safeString(m, 2)))
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := stringField(innerMap, 1); !ok || s != "hi" {
		t.Errorf("inner field 1 = %q ok=%v", s, ok)
	}
}

func safeString(m map[int][]field, num int) string {
	s, _ := stringField(m, num)
	return s
}

func TestDecodeMessageUnknownFieldRetained(t *testing.T) {
	// A message with an unknown field 99 (LEN) plus a known one.
	data := concatBytes(encodeField(99, wireLen, "x"), encodeField(1, wireLen, "known"))
	m, err := decodeMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m[99]; !ok {
		t.Error("unknown field 99 should be retained")
	}
	if s, ok := stringField(m, 1); !ok || s != "known" {
		t.Errorf("field 1 = %q", s)
	}
}
