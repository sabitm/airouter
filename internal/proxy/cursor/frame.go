package cursor

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

// wrapConnectFrame wraps a protobuf payload in a Connect-RPC 5-byte envelope.
// Cursor never compresses requests, so compress is false on the send path.
func wrapConnectFrame(payload []byte, compress bool) []byte {
	flags := byte(flagNone)
	out := payload
	if compress {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(payload)
		_ = gz.Close()
		out = buf.Bytes()
		flags = flagGzip
	}
	frame := make([]byte, 5+len(out))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(out)))
	copy(frame[5:], out)
	return frame
}

// readFrame reads one Connect-RPC frame from r. Returns the flags byte and the
// (still-compressed) payload. Returns io.EOF when the stream is exhausted
// cleanly at a frame boundary.
func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		// Only a clean EOF at a frame boundary ends the stream. A partial header
		// means the stream was truncated mid-frame; masking it as io.EOF would
		// fabricate a clean finish over partial content.
		if err == io.EOF {
			return 0, nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return 0, nil, fmt.Errorf("cursor: truncated frame header: %w", err)
		}
		return 0, nil, err
	}
	flags := header[0]
	length := binary.BigEndian.Uint32(header[1:5])
	if length == 0 {
		return flags, nil, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("cursor: frame body read: %w", err)
	}
	return flags, payload, nil
}

// decompressPayload decompresses a frame payload when flagged gzip. Cursor
// responses may use gzip, raw zlib, or (rarely) raw deflate; try each in turn.
// JSON error frames (payload starts with '{') are returned unchanged so the
// caller can parse them as errors.
func decompressPayload(payload []byte, flags byte) []byte {
	if len(payload) > 0 && payload[0] == 0x7b { // '{' — JSON error frame
		return payload
	}
	if flags&(flagGzip|flagGzipTrailer) == 0 && flags&flagTrailer == 0 {
		return payload
	}
	// gzip first (standard gzip header 1f 8b).
	if gz, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
		if out, derr := io.ReadAll(gz); derr == nil {
			return out
		}
		_ = gz.Close()
	}
	// zlib (RFC 1950) then raw deflate (RFC 1951) fallbacks: some Cursor frames
	// use the raw deflate stream without a zlib wrapper.
	if zr, err := zlib.NewReader(bytes.NewReader(payload)); err == nil {
		if out, derr := io.ReadAll(zr); derr == nil {
			return out
		}
	}
	if out, err := inflateRaw(payload); err == nil {
		return out
	}
	// Last resort: return as-is so decode attempts a best-effort parse.
	return payload
}

// inflateRaw decompresses a raw deflate stream (no zlib header) via zlib's
// flush-mode reader seeded with a synthetic zlib header.
func inflateRaw(payload []byte) ([]byte, error) {
	r := bytes.NewReader(payload)
	zr, err := zlib.NewReader(io.MultiReader(bytes.NewReader([]byte{0x78, 0x01}), r))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
