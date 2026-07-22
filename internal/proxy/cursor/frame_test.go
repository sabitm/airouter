package cursor

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func TestWrapConnectFrameNoCompress(t *testing.T) {
	payload := []byte{1, 2, 3, 4}
	frame := wrapConnectFrame(payload, false)
	if frame[0] != flagNone {
		t.Errorf("flags = %d, want 0", frame[0])
	}
	if int(frame[1])<<24|int(frame[2])<<16|int(frame[3])<<8|int(frame[4]) != len(payload) {
		t.Errorf("length header wrong: %v", frame[1:5])
	}
	if !bytes.Equal(frame[5:], payload) {
		t.Errorf("payload mismatch")
	}
}

func TestWrapConnectFrameGzip(t *testing.T) {
	payload := bytes.Repeat([]byte("hello world "), 50)
	frame := wrapConnectFrame(payload, true)
	if frame[0] != flagGzip {
		t.Errorf("flags = %d, want %d", frame[0], flagGzip)
	}
	// Round-trip via decompressPayload.
	flags, body, err := readFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	out := decompressPayload(body, flags)
	if !bytes.Equal(out, payload) {
		t.Errorf("gzip roundtrip mismatch: got %d bytes want %d", len(out), len(payload))
	}
}

func TestReadFrameJSONErrorNotDecompressed(t *testing.T) {
	jsonErr := []byte(`{"error":{"code":"resource_exhausted"}}`)
	// A JSON error frame from Cursor is uncompressed (flags=0); build it raw.
	raw := make([]byte, 5+len(jsonErr))
	raw[0] = flagNone
	raw[1], raw[2], raw[3], raw[4] = 0, 0, 0, byte(len(jsonErr))
	copy(raw[5:], jsonErr)
	flags, body, err := readFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := decompressPayload(body, flags)
	if !bytes.Equal(out, jsonErr) {
		t.Errorf("JSON error frame should pass through, got %q", out)
	}
}

func TestReadFrameGzipManual(t *testing.T) {
	payload := []byte("manual gzip payload")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(payload)
	gz.Close()
	frame := make([]byte, 5+buf.Len())
	frame[0] = flagGzip
	frame[1], frame[2], frame[3], frame[4] = byte(buf.Len()>>24), byte(buf.Len()>>16), byte(buf.Len()>>8), byte(buf.Len())
	copy(frame[5:], buf.Bytes())
	flags, body, err := readFrame(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	out := decompressPayload(body, flags)
	if !bytes.Equal(out, payload) {
		t.Errorf("manual gzip roundtrip: %q want %q", out, payload)
	}
}

func TestReadFrameEOF(t *testing.T) {
	_, _, err := readFrame(bytes.NewReader(nil))
	if err == nil {
		t.Error("empty reader should yield EOF")
	}
}

func TestReadFramePartial(t *testing.T) {
	// Only 3 header bytes -> io.ErrUnexpectedEOF maps to EOF via readFrame.
	_, _, err := readFrame(bytes.NewReader([]byte{1, 2, 3}))
	if err == nil {
		t.Error("partial header should error")
	}
}
