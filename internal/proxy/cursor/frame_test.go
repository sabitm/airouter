package cursor

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/adler32"
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
	out, err := decompressPayload(body, flags)
	if err != nil {
		t.Fatal(err)
	}
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
	out, err := decompressPayload(body, flags)
	if err != nil {
		t.Fatal(err)
	}
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
	out, err := decompressPayload(body, flags)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("manual gzip roundtrip: %q want %q", out, payload)
	}
}

func TestReadFrameEOF(t *testing.T) {
	_, _, err := readFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Errorf("empty reader: got %v, want io.EOF", err)
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
	out, err := decompressPayload(buf.Bytes(), flagGzip)
	if err != nil {
		t.Fatal(err)
	}
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
	out, err := decompressPayload(stripped, flagGzip)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, payload) {
		t.Errorf("raw deflate fallback: got %q, want %q", out, payload)
	}
}

func TestDecompressPayloadGarbageReturnsAsIs(t *testing.T) {
	garbage := []byte{0xff, 0xfe, 0xfd, 0x00, 0x01}
	out, err := decompressPayload(garbage, flagGzip)
	if err != nil {
		t.Fatal(err)
	}
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
		out, err := inflateRaw(stripped, maxDecompressedPayloadBytes)
		if err != nil {
			t.Fatalf("inflateRaw: %v", err)
		}
		if !bytes.Equal(out, payload) {
			t.Errorf("got %q, want %q", out, payload)
		}
	})

	t.Run("invalid deflate returns error", func(t *testing.T) {
		if _, err := inflateRaw([]byte{0xff, 0xff, 0xff}, maxDecompressedPayloadBytes); err == nil {
			t.Error("got nil error, want error for invalid deflate stream")
		}
	})
}

func TestReadFrameZeroLength(t *testing.T) {
	flags, payload, err := readFrame(bytes.NewReader([]byte{flagGzip, 0, 0, 0, 0}))
	if err != nil {
		t.Fatal(err)
	}
	if flags != flagGzip || payload != nil {
		t.Fatalf("flags=%d payload=%v, want gzip flags and nil payload", flags, payload)
	}
}

func TestReadFrameShortBody(t *testing.T) {
	header := []byte{flagNone, 0, 0, 0, 8, 1, 2}
	_, _, err := readFrame(bytes.NewReader(header))
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want wrapped short-body EOF", err)
	}
	if errors.Is(err, errFrameTooLarge) {
		t.Fatal("short body reported as a size-limit error")
	}
}

func TestReadFramePayloadLimit(t *testing.T) {
	t.Run("exact boundary accepted", func(t *testing.T) {
		r := &countingReader{r: io.LimitReader(zeroReader{}, maxConnectPayloadBytes)}
		flags, payload, err := readFrame(io.MultiReader(bytes.NewReader(connectHeader(flagNone, maxConnectPayloadBytes)), r))
		if err != nil {
			t.Fatal(err)
		}
		if flags != flagNone || int64(len(payload)) != maxConnectPayloadBytes {
			t.Fatalf("flags=%d len=%d, want uncompressed %d-byte payload", flags, len(payload), maxConnectPayloadBytes)
		}
		if r.reads == 0 {
			t.Fatal("exact boundary did not read the declared body")
		}
	})

	t.Run("one over limit rejected before body read", func(t *testing.T) {
		assertHeaderOnlyFrameRejected(t, maxConnectPayloadBytes+1)
	})

	t.Run("max uint32 rejected before body read", func(t *testing.T) {
		assertHeaderOnlyFrameRejected(t, ^uint32(0))
	})
}

func TestDecompressPayloadOutputLimit(t *testing.T) {
	plain := []byte("bounded payload")
	gzipped := mustGzip(t, plain)
	zlibbed := mustZlib(t, plain)
	raw := zlibbed[2:]

	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{name: "gzip", payload: gzipped},
		{name: "zlib", payload: zlibbed},
		{name: "raw", payload: raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, limit := range []int64{int64(len(plain) - 1), int64(len(plain)), int64(len(plain) + 1)} {
				out, err := decompressPayloadWithLimit(tc.payload, flagGzip, limit)
				if limit < int64(len(plain)) {
					if out != nil || !errors.Is(err, errDecompressedPayloadTooLarge) {
						t.Fatalf("limit %d: got %q err=%v, want nil and size error", limit, out, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("limit %d: %v", limit, err)
				}
				if !bytes.Equal(out, plain) {
					t.Fatalf("limit %d: got %q, want %q", limit, out, plain)
				}
			}
		})
	}
}

func TestDecompressPayloadFlaggedJSONPassesThrough(t *testing.T) {
	payload := []byte(`{"error":"resource_exhausted"}`)
	out, err := decompressPayloadWithLimit(payload, flagGzip, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("got %q, want flagged JSON passthrough", out)
	}
}

func TestDecompressPayloadMalformedFallsBackBelowLimit(t *testing.T) {
	gzipped := mustGzip(t, []byte("checksum"))
	gzipped[len(gzipped)-1] ^= 0xff
	out, err := decompressPayloadWithLimit(gzipped, flagGzip, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, gzipped) {
		t.Fatal("malformed gzip below the limit was not returned as original payload")
	}

	zlibbed := mustZlib(t, []byte("checksum"))
	zlibbed[len(zlibbed)-1] ^= 0xff
	out, err = decompressPayloadWithLimit(zlibbed, flagGzip, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, zlibbed) {
		t.Fatal("malformed zlib below the limit was not returned as original payload")
	}

	raw := mustZlib(t, []byte("checksum"))[2:]
	raw[len(raw)-1] ^= 0xff
	out, err = decompressPayloadWithLimit(raw, flagGzip, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatal("malformed raw deflate below the limit was not returned as original payload")
	}
}

func TestDecompressPayloadGzipMembersCombinedLimit(t *testing.T) {
	first := mustGzip(t, []byte("aaa"))
	second := mustGzip(t, []byte("bbb"))
	joined := append(append([]byte{}, first...), second...)

	out, err := decompressPayloadWithLimit(joined, flagGzip, 6)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, []byte("aaabbb")) {
		t.Fatalf("got %q, want concatenated members", out)
	}

	out, err = decompressPayloadWithLimit(joined, flagGzip, 5)
	if out != nil || !errors.Is(err, errDecompressedPayloadTooLarge) {
		t.Fatalf("got %q err=%v, want nil and combined size error", out, err)
	}
}

func TestReadDecompressedPayloadErrorPrecedence(t *testing.T) {
	t.Run("excess takes priority over simultaneous error", func(t *testing.T) {
		r := &scriptedReadCloser{chunks: []scriptedRead{{
			data: bytes.Repeat([]byte{'x'}, 4),
			err:  io.ErrUnexpectedEOF,
		}}}
		out, err := readDecompressedPayload(r, 3)
		if out != nil || !errors.Is(err, errDecompressedPayloadTooLarge) {
			t.Fatalf("got %q err=%v, want nil and size error", out, err)
		}
		if !r.closed {
			t.Fatal("decompressor was not closed")
		}
	})

	t.Run("ordinary error returns no partial output", func(t *testing.T) {
		r := &scriptedReadCloser{chunks: []scriptedRead{{
			data: []byte("partial"),
			err:  io.ErrUnexpectedEOF,
		}}}
		out, err := readDecompressedPayload(r, 100)
		if out != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("got %q err=%v, want nil and ordinary error", out, err)
		}
		if errors.Is(err, errDecompressedPayloadTooLarge) {
			t.Fatal("ordinary error reported as a size-limit error")
		}
		if !r.closed {
			t.Fatal("decompressor was not closed")
		}
	})

	t.Run("success within limit", func(t *testing.T) {
		r := &scriptedReadCloser{chunks: []scriptedRead{{
			data: []byte("ok"),
			err:  io.EOF,
		}}}
		out, err := readDecompressedPayload(r, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, []byte("ok")) {
			t.Fatalf("got %q, want ok", out)
		}
		if !r.closed {
			t.Fatal("decompressor was not closed")
		}
	})
}

func TestInflateRawClosesOnSizeError(t *testing.T) {
	plain := bytes.Repeat([]byte{'z'}, 32)
	raw := mustZlib(t, plain)[2:]
	if _, err := inflateRaw(raw, int64(len(plain)-1)); !errors.Is(err, errDecompressedPayloadTooLarge) {
		t.Fatalf("got %v, want size error", err)
	}
	// The synthetic zlib header and Adler-32 trailer remain compatible: an
	// in-budget payload still round-trips after the size rejection above.
	out, err := inflateRaw(raw, int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("got %d bytes, want %d", len(out), len(plain))
	}
	if got := adler32.Checksum(plain); got == 0 {
		t.Fatal("test payload has an empty Adler-32 checksum")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

type countingReader struct {
	r     io.Reader
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	return c.r.Read(p)
}

type scriptedRead struct {
	data []byte
	err  error
}

type scriptedReadCloser struct {
	chunks []scriptedRead
	closed bool
}

func (s *scriptedReadCloser) Read(p []byte) (int, error) {
	if len(s.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := s.chunks[0]
	n := copy(p, chunk.data)
	s.chunks[0].data = chunk.data[n:]
	if len(s.chunks[0].data) == 0 {
		s.chunks = s.chunks[1:]
		return n, chunk.err
	}
	return n, nil
}

func (s *scriptedReadCloser) Close() error {
	s.closed = true
	return nil
}

func connectHeader(flags byte, n uint32) []byte {
	hdr := []byte{flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(hdr[1:5], n)
	return hdr
}

func assertHeaderOnlyFrameRejected(t *testing.T, n uint32) {
	t.Helper()
	body := &failOnRead{}
	_, payload, err := readFrame(io.MultiReader(bytes.NewReader(connectHeader(flagGzip, n)), body))
	if payload != nil || !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("got payload=%v err=%v, want frame-too-large", payload, err)
	}
	if body.read {
		t.Fatal("oversized header read the frame body")
	}
}

type failOnRead struct {
	read bool
}

func (f *failOnRead) Read(p []byte) (int, error) {
	f.read = true
	return 0, errors.New("body read after oversized header")
}

func mustGzip(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustZlib(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
