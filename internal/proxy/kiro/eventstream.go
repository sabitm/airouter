package kiro

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// AWS EventStream binary framing (vnd.amazon.eventstream). Each message:
//
//	[totalLen uint32][headersLen uint32][preludeCRC uint32]
//	[headers ...][payload ...][messageCRC uint32]      (all big-endian)
//
// preludeCRC covers the first 8 bytes (both length fields); messageCRC covers
// every byte from totalLen through the end of the payload.

// esMessage is one decoded EventStream frame.
type esMessage struct {
	headers map[string]string
	payload []byte
}

const esPreludeLen = 12 // totalLen(4) + headersLen(4) + preludeCRC(4)

// esHeaderStringType is the header value type for a UTF-8 string (type 7), the
// only type CodeWhisperer uses for the Smithy system headers we read.
const esHeaderStringType = 7

// readEventStreamMessage reads and validates one frame from r. It returns io.EOF
// when the stream is cleanly exhausted at a frame boundary.
func readEventStreamMessage(r io.Reader) (*esMessage, error) {
	prelude := make([]byte, esPreludeLen)
	if _, err := io.ReadFull(r, prelude); err != nil {
		// EOF at a boundary is the clean end of stream; propagate it verbatim so
		// the caller can stop. A partial prelude means the stream was truncated
		// mid-frame; masking it as io.EOF would fabricate a clean Finish.
		if err == io.EOF {
			return nil, io.EOF
		}
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("kiro: eventstream truncated prelude: %w", err)
		}
		return nil, err
	}
	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	if crc32.ChecksumIEEE(prelude[0:8]) != preludeCRC {
		return nil, fmt.Errorf("kiro: eventstream prelude CRC mismatch")
	}
	if totalLen < esPreludeLen+4 || headersLen > totalLen-esPreludeLen-4 {
		return nil, fmt.Errorf("kiro: eventstream frame length invalid (total=%d headers=%d)", totalLen, headersLen)
	}

	// Read the remainder of the frame (everything after the prelude).
	rest := make([]byte, totalLen-esPreludeLen)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("kiro: eventstream short read: %w", err)
	}

	// messageCRC covers total_len..end_of_payload, i.e. the whole frame except the
	// trailing 4 CRC bytes. Reassemble prelude+rest[:-4] to check it.
	msgCRC := binary.BigEndian.Uint32(rest[len(rest)-4:])
	sum := crc32.NewIEEE()
	sum.Write(prelude)
	sum.Write(rest[:len(rest)-4])
	if sum.Sum32() != msgCRC {
		return nil, fmt.Errorf("kiro: eventstream message CRC mismatch")
	}

	headerBytes := rest[:headersLen]
	payload := rest[headersLen : len(rest)-4]
	headers, err := parseHeaders(headerBytes)
	if err != nil {
		return nil, err
	}
	return &esMessage{headers: headers, payload: payload}, nil
}

// parseHeaders decodes the header block. Each header is:
//
//	[nameLen uint8][name][valueType uint8][valueLen uint16][value]
//
// Only string-typed values (type 7) are decoded into the returned map; other
// types are skipped by length (none are needed for the events Kiro sends).
func parseHeaders(b []byte) (map[string]string, error) {
	headers := map[string]string{}
	i := 0
	for i < len(b) {
		if i+1 > len(b) {
			return nil, fmt.Errorf("kiro: eventstream header truncated")
		}
		nameLen := int(b[i])
		i++
		if i+nameLen+1 > len(b) {
			return nil, fmt.Errorf("kiro: eventstream header name truncated")
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		valueType := b[i]
		i++
		if valueType != esHeaderStringType {
			// Non-string header: values carry a 2-byte length prefix for the variable
			// types we might encounter; bail rather than guess, since Kiro only sends
			// string headers.
			return nil, fmt.Errorf("kiro: eventstream unsupported header type %d", valueType)
		}
		if i+2 > len(b) {
			return nil, fmt.Errorf("kiro: eventstream header value length truncated")
		}
		valueLen := int(binary.BigEndian.Uint16(b[i : i+2]))
		i += 2
		if i+valueLen > len(b) {
			return nil, fmt.Errorf("kiro: eventstream header value truncated")
		}
		headers[name] = string(b[i : i+valueLen])
		i += valueLen
	}
	return headers, nil
}
