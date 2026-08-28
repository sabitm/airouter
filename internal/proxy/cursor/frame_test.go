package cursor

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
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
	// 3 bytes < 5-byte header -> io.ErrUnexpectedEOF surfaced as an error, not
	// mapped to io.EOF (which would mask a truncated stream as a clean end).
	_, _, err := readFrame(bytes.NewReader([]byte{1, 2, 3}))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("got %v, want wrapped io.ErrUnexpectedEOF for partial header", err)
	}
}

func TestDecompressPayloadZlibFallback(t *testing.T) {
	payload := []byte("zlib compressed payload")
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(payload)
	zw.Close()
	// Pass flagGzip so gzip reader is tried first and fails, then zlib succeeds.
	out := decompressPayload(buf.Bytes(), flagGzip)
	if !bytes.Equal(out, payload) {
		t.Errorf("zlib fallback: got %q, want %q", out, payload)
	}
}

func TestDecompressPayloadRawDeflateFallback(t *testing.T) {
	payload := []byte("raw deflate payload")
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(payload)
	zw.Close()
	// Strip the 2-byte zlib header but keep the adler32 trailer: this is the
	// raw-deflate shape inflateRaw expects (zlib body without its header).
	stripped := buf.Bytes()[2:]
	// Raw deflate: gzip and zlib readers both fail, falling through to inflateRaw.
	out := decompressPayload(stripped, flagGzip)
	if !bytes.Equal(out, payload) {
		t.Errorf("raw deflate fallback: got %q, want %q", out, payload)
	}
}

func TestDecompressPayloadGarbageReturnsAsIs(t *testing.T) {
	garbage := []byte{0xff, 0xfe, 0xfd, 0x00, 0x01}
	out := decompressPayload(garbage, flagGzip)
	if !bytes.Equal(out, garbage) {
		t.Errorf("garbage: got %q, want passthrough %q", out, garbage)
	}
}

func TestInflateRaw(t *testing.T) {
	t.Run("zlib body without header decompresses", func(t *testing.T) {
		payload := []byte("inflate raw test")
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		zw.Write(payload)
		zw.Close()
		// inflateRaw expects a zlib stream with its 2-byte header stripped,
		// keeping the adler32 trailer.
		stripped := buf.Bytes()[2:]
		out, err := inflateRaw(stripped)
		if err != nil {
			t.Fatalf("inflateRaw: %v", err)
		}
		if !bytes.Equal(out, payload) {
			t.Errorf("got %q, want %q", out, payload)
		}
	})

	t.Run("invalid deflate returns error", func(t *testing.T) {
		if _, err := inflateRaw([]byte{0xff, 0xff, 0xff}); err == nil {
			t.Error("got nil error, want error for invalid deflate stream")
		}
	})
}
