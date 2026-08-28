package kiro

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"

	"airouter/internal/proxy/ir"
)

// buildFrame encodes one AWS EventStream message with a single :event-type
// string header and the given JSON payload, matching the wire format the decoder
// parses. Used to synthesize upstream frames in tests.
func buildFrame(eventType string, payload []byte) []byte {
	// Header: [nameLen][name][type=7][valueLen uint16][value]
	name := ":event-type"
	var hdr bytes.Buffer
	hdr.WriteByte(byte(len(name)))
	hdr.WriteString(name)
	hdr.WriteByte(esHeaderStringType)
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(eventType)))
	hdr.Write(vl[:])
	hdr.WriteString(eventType)
	headers := hdr.Bytes()

	totalLen := uint32(esPreludeLen + len(headers) + len(payload) + 4)
	var prelude [esPreludeLen]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))

	var msg bytes.Buffer
	msg.Write(prelude[:])
	msg.Write(headers)
	msg.Write(payload)
	var msgCRC [4]byte
	binary.BigEndian.PutUint32(msgCRC[:], crc32.ChecksumIEEE(msg.Bytes()))
	msg.Write(msgCRC[:])
	return msg.Bytes()
}

func collect(t *testing.T, frames []byte) []ir.StreamEvent {
	t.Helper()
	var events []ir.StreamEvent
	err := DecodeStream(bytes.NewReader(frames), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	return events
}

// TestDecodeStreamTruncatedPrelude verifies that a stream cut mid-prelude is
// reported as an error instead of ending cleanly. Masking it as io.EOF would
// fabricate a successful finish over partial content.
func TestDecodeStreamTruncatedPrelude(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildFrame("assistantResponseEvent", []byte(`{"content":"Hello "}`)))
	buf.Write(buildFrame("assistantResponseEvent", []byte(`{"content":"world"}`)))
	// 7 bytes of the next 12-byte prelude: the upstream died mid-frame.
	buf.Write([]byte{0, 0, 0, 0, 0, 0, 0})

	var sawFinish bool
	err := DecodeStream(bytes.NewReader(buf.Bytes()), func(ev ir.StreamEvent) error {
		if ev.Kind == ir.EventFinish {
			sawFinish = true
		}
		return nil
	})
	if err == nil {
		t.Fatal("DecodeStream: expected truncation error, got nil (clean finish)")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error %v does not wrap io.ErrUnexpectedEOF", err)
	}
	if sawFinish {
		t.Error("DecodeStream fabricated a Finish event on a truncated stream")
	}
}

func TestDecodeStreamText(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildFrame("assistantResponseEvent", []byte(`{"content":"Hello "}`)))
	buf.Write(buildFrame("assistantResponseEvent", []byte(`{"content":"world"}`)))
	buf.Write(buildFrame("metricsEvent", []byte(`{"inputTokens":11,"outputTokens":5}`)))
	buf.Write(buildFrame("messageStopEvent", []byte(`{}`)))

	events := collect(t, buf.Bytes())
	if len(events) < 2 || events[0].Kind != ir.EventMessageStart {
		t.Fatalf("expected message start first, got %+v", events)
	}
	var text string
	var finish *ir.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case ir.EventTextDelta:
			text += events[i].Text
		case ir.EventFinish:
			finish = &events[i]
		}
	}
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	if finish == nil || finish.InputTokens != 11 || finish.OutputTokens != 5 {
		t.Errorf("finish usage = %+v", finish)
	}
	if finish.StopReason != ir.StopEndTurn {
		t.Errorf("stop = %v", finish.StopReason)
	}
}

func TestDecodeStreamMetricsCacheAccounting(t *testing.T) {
	cases := []struct {
		name    string
		metrics string
		wantIn  int
		wantOut int
	}{
		{"base only", `{"inputTokens":11,"outputTokens":5}`, 11, 5},
		{"cache read camel", `{"inputTokens":11,"outputTokens":5,"cacheReadInputTokens":4}`, 15, 5},
		{"cache read snake", `{"inputTokens":11,"outputTokens":5,"cache_read_input_tokens":4}`, 15, 5},
		{"cache creation camel", `{"inputTokens":11,"outputTokens":5,"cacheCreationInputTokens":7}`, 18, 5},
		{"cache creation snake", `{"inputTokens":11,"outputTokens":5,"cache_creation_input_tokens":7}`, 18, 5},
		{"both cache fields", `{"inputTokens":11,"outputTokens":5,"cacheReadInputTokens":4,"cacheCreationInputTokens":7}`, 22, 5},
		{"camel preferred over snake", `{"inputTokens":11,"outputTokens":5,"cacheReadInputTokens":4,"cache_read_input_tokens":99,"cacheCreationInputTokens":7,"cache_creation_input_tokens":88}`, 22, 5},
		{"missing fields", `{}`, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			buf.Write(buildFrame("metricsEvent", []byte(tc.metrics)))
			buf.Write(buildFrame("messageStopEvent", []byte(`{}`)))
			events := collect(t, buf.Bytes())
			if len(events) != 2 || events[0].Kind != ir.EventMessageStart || events[1].Kind != ir.EventFinish {
				t.Fatalf("events = %+v, want start and finish", events)
			}
			finish := events[1]
			if finish.InputTokens != tc.wantIn || finish.OutputTokens != tc.wantOut {
				t.Errorf("usage = %d/%d, want %d/%d", finish.InputTokens, finish.OutputTokens, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func TestDecodeStreamStopOnlyEmitsFinish(t *testing.T) {
	events := collect(t, buildFrame("messageStopEvent", []byte(`{}`)))
	if len(events) != 2 || events[0].Kind != ir.EventMessageStart || events[1].Kind != ir.EventFinish {
		t.Fatalf("events = %+v, want start and finish", events)
	}
}

func TestDecodeStreamStripsThinkingTags(t *testing.T) {
	frame := buildFrame("assistantResponseEvent", []byte(`{"content":"<thinking>ignore</thinking>kept"}`))
	events := collect(t, frame)
	var text string
	for _, e := range events {
		if e.Kind == ir.EventTextDelta {
			text += e.Text
		}
	}
	if text != "ignorekept" {
		t.Errorf("text = %q (tags should be stripped, enclosed text kept)", text)
	}
}

func TestDecodeStreamToolUse(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildFrame("toolUseEvent", []byte(`{"toolUseId":"call_1","name":"get_weather","input":"{\"ci"}`)))
	buf.Write(buildFrame("toolUseEvent", []byte(`{"toolUseId":"call_1","input":"ty\":\"NYC\"}"}`)))
	buf.Write(buildFrame("messageStopEvent", []byte(`{}`)))

	events := collect(t, buf.Bytes())
	var start *ir.StreamEvent
	var args string
	var finish *ir.StreamEvent
	for i := range events {
		switch events[i].Kind {
		case ir.EventToolCallStart:
			start = &events[i]
		case ir.EventToolCallDelta:
			args += events[i].ArgsFrag
		case ir.EventFinish:
			finish = &events[i]
		}
	}
	if start == nil || start.ToolID != "call_1" || start.ToolName != "get_weather" {
		t.Fatalf("tool start = %+v", start)
	}
	if args != `{"city":"NYC"}` {
		t.Errorf("assembled args = %q", args)
	}
	if finish == nil || finish.StopReason != ir.StopToolUse {
		t.Errorf("finish stop = %+v", finish)
	}
}

func TestDecodeStreamCRCMismatch(t *testing.T) {
	frame := buildFrame("assistantResponseEvent", []byte(`{"content":"x"}`))
	frame[len(frame)-1] ^= 0xff // corrupt the message CRC
	err := DecodeStream(bytes.NewReader(frame), func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected CRC mismatch error")
	}
}

func TestDecodeStreamEmpty(t *testing.T) {
	// A clean empty stream emits nothing and no error.
	events := collect(t, nil)
	if len(events) != 0 {
		t.Errorf("expected no events, got %+v", events)
	}
}

// buildPrelude constructs the 12-byte prelude (totalLen, headersLen, preludeCRC).
func buildPrelude(totalLen, headersLen uint32) []byte {
	var p [esPreludeLen]byte
	binary.BigEndian.PutUint32(p[0:4], totalLen)
	binary.BigEndian.PutUint32(p[4:8], headersLen)
	binary.BigEndian.PutUint32(p[8:12], crc32.ChecksumIEEE(p[0:8]))
	return p[:]
}

func TestReadEventStreamMessage(t *testing.T) {
	t.Run("clean EOF at frame boundary", func(t *testing.T) {
		_, err := readEventStreamMessage(bytes.NewReader(nil))
		if err != io.EOF {
			t.Errorf("got %v, want io.EOF", err)
		}
	})

	t.Run("partial prelude is a truncation error", func(t *testing.T) {
		// 5 bytes < 12-byte prelude -> io.ErrUnexpectedEOF, surfaced as an error.
		_, err := readEventStreamMessage(bytes.NewReader([]byte{1, 2, 3, 4, 5}))
		if err == nil || err == io.EOF || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("got %v, want wrapped io.ErrUnexpectedEOF for partial prelude", err)
		}
	})

	t.Run("valid frame with single header", func(t *testing.T) {
		header := encodeHeader(":event-type", "tool")
		payload := []byte(`{"x":1}`)
		totalLen := uint32(esPreludeLen + len(header) + len(payload) + 4)
		var buf bytes.Buffer
		buf.Write(buildPrelude(totalLen, uint32(len(header))))
		buf.Write(header)
		buf.Write(payload)
		sum := crc32.NewIEEE()
		sum.Write(buf.Bytes())
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], sum.Sum32())
		buf.Write(crc[:])

		msg, err := readEventStreamMessage(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("got %v, want nil", err)
		}
		if msg.headers[":event-type"] != "tool" {
			t.Errorf("header = %q", msg.headers[":event-type"])
		}
		if string(msg.payload) != `{"x":1}` {
			t.Errorf("payload = %q", msg.payload)
		}
	})

	t.Run("prelude CRC mismatch", func(t *testing.T) {
		var p [esPreludeLen]byte
		binary.BigEndian.PutUint32(p[0:4], 100)
		binary.BigEndian.PutUint32(p[4:8], 0)
		// Skip preludeCRC computation -> guaranteed mismatch (it stays 0).
		_, err := readEventStreamMessage(bytes.NewReader(p[:]))
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("prelude CRC mismatch")) {
			t.Errorf("got %v, want prelude CRC mismatch error", err)
		}
	})

	t.Run("frame length invalid totalLen too small", func(t *testing.T) {
		// totalLen=10 < esPreludeLen(12)+4=16 -> invalid.
		_, err := readEventStreamMessage(bytes.NewReader(buildPrelude(10, 0)))
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("frame length invalid")) {
			t.Errorf("got %v, want frame length invalid error", err)
		}
	})

	t.Run("headersLen exceeds frame", func(t *testing.T) {
		// totalLen=20, headersLen=100 (> totalLen-esPreludeLen-4=4).
		_, err := readEventStreamMessage(bytes.NewReader(buildPrelude(20, 100)))
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("frame length invalid")) {
			t.Errorf("got %v, want frame length invalid error", err)
		}
	})

	t.Run("message CRC mismatch", func(t *testing.T) {
		frame := buildFrame("assistantResponseEvent", []byte(`{"content":"x"}`))
		frame[len(frame)-1] ^= 0xff // corrupt trailing message CRC
		_, err := readEventStreamMessage(bytes.NewReader(frame))
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("message CRC mismatch")) {
			t.Errorf("got %v, want message CRC mismatch error", err)
		}
	})

	t.Run("short read on frame body", func(t *testing.T) {
		// Valid prelude claiming totalLen=50 but provide no body bytes.
		_, err := readEventStreamMessage(bytes.NewReader(buildPrelude(50, 0)))
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("short read")) {
			t.Errorf("got %v, want short read error", err)
		}
	})
}

// encodeHeader builds a single string-typed (type 7) header entry.
func encodeHeader(name, value string) []byte {
	var h bytes.Buffer
	h.WriteByte(byte(len(name)))
	h.WriteString(name)
	h.WriteByte(esHeaderStringType)
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(value)))
	h.Write(vl[:])
	h.WriteString(value)
	return h.Bytes()
}

func TestParseHeaders(t *testing.T) {
	t.Run("single string header", func(t *testing.T) {
		h, err := parseHeaders(encodeHeader(":event-type", "tool"))
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if h[":event-type"] != "tool" {
			t.Errorf("got %q", h[":event-type"])
		}
	})

	t.Run("multiple string headers", func(t *testing.T) {
		b := append(encodeHeader(":a", "1"), encodeHeader(":b", "2")...)
		h, err := parseHeaders(b)
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if h[":a"] != "1" || h[":b"] != "2" {
			t.Errorf("got %+v", h)
		}
	})

	t.Run("empty header block returns empty map", func(t *testing.T) {
		h, err := parseHeaders(nil)
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if len(h) != 0 {
			t.Errorf("got %+v, want empty", h)
		}
	})

	t.Run("truncated header name length", func(t *testing.T) {
		// nameLen byte claims a name but no bytes follow.
		_, err := parseHeaders([]byte{5})
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("header name truncated")) {
			t.Errorf("got %v, want header name truncated", err)
		}
	})

	t.Run("non-string value type rejected", func(t *testing.T) {
		// Build a header with valueType=3 (not 7) after the name.
		var h bytes.Buffer
		name := ":event-type"
		h.WriteByte(byte(len(name)))
		h.WriteString(name)
		h.WriteByte(3) // non-string type
		_, err := parseHeaders(h.Bytes())
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("unsupported header type")) {
			t.Errorf("got %v, want unsupported header type error", err)
		}
	})

	t.Run("truncated value length", func(t *testing.T) {
		// Header with name + type=7 but only 1 byte for the uint16 value length.
		var h bytes.Buffer
		name := ":event-type"
		h.WriteByte(byte(len(name)))
		h.WriteString(name)
		h.WriteByte(esHeaderStringType)
		h.WriteByte(0) // only 1 byte; need 2 for uint16
		_, err := parseHeaders(h.Bytes())
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("value length truncated")) {
			t.Errorf("got %v, want value length truncated", err)
		}
	})

	t.Run("truncated value", func(t *testing.T) {
		// Header claims valueLen=10 but provides only 1 byte.
		var h bytes.Buffer
		name := ":event-type"
		h.WriteByte(byte(len(name)))
		h.WriteString(name)
		h.WriteByte(esHeaderStringType)
		var vl [2]byte
		binary.BigEndian.PutUint16(vl[:], 10)
		h.Write(vl[:])
		h.WriteString("x") // only 1 byte
		_, err := parseHeaders(h.Bytes())
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte("value truncated")) {
			t.Errorf("got %v, want value truncated", err)
		}
	})
}

func TestToolInputFragment(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"empty", json.RawMessage(``), ""},
		{"json string", json.RawMessage(`"hello"`), "hello"},
		{"json object", json.RawMessage(`{"k":"v"}`), `{"k":"v"}`},
		{"json number", json.RawMessage(`42`), "42"},
		{"json array", json.RawMessage(`[1,2]`), "[1,2]"},
		{"json bool", json.RawMessage(`true`), "true"},
		{"json null", json.RawMessage(`null`), "null"},
		{"raw non-json bytes", json.RawMessage(`notjson`), "notjson"},
		{"empty json string", json.RawMessage(`""`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolInputFragment(tc.raw); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// buildExceptionFrame encodes an EventStream exception with :message-type and payload.
func buildExceptionFrame(exceptionType, payload string) []byte {
	headers := map[string]string{
		":message-type":   "exception",
		":exception-type": exceptionType,
		":event-type":     exceptionType,
	}
	var hdr bytes.Buffer
	// Deterministic order for test stability.
	order := []string{":message-type", ":exception-type", ":event-type"}
	for _, name := range order {
		val := headers[name]
		hdr.WriteByte(byte(len(name)))
		hdr.WriteString(name)
		hdr.WriteByte(esHeaderStringType)
		var vl [2]byte
		binary.BigEndian.PutUint16(vl[:], uint16(len(val)))
		hdr.Write(vl[:])
		hdr.WriteString(val)
	}
	hb := hdr.Bytes()
	pl := []byte(payload)
	totalLen := uint32(esPreludeLen + len(hb) + len(pl) + 4)
	var prelude [esPreludeLen]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(hb)))
	binary.BigEndian.PutUint32(prelude[8:12], crc32.ChecksumIEEE(prelude[0:8]))
	var msg bytes.Buffer
	msg.Write(prelude[:])
	msg.Write(hb)
	msg.Write(pl)
	var msgCRC [4]byte
	binary.BigEndian.PutUint32(msgCRC[:], crc32.ChecksumIEEE(msg.Bytes()))
	msg.Write(msgCRC[:])
	return msg.Bytes()
}

func TestDecodeStreamExceptionFrame(t *testing.T) {
	frame := buildExceptionFrame("ModelStreamErrorException", `{"message":"model overloaded"}`)
	var events []ir.StreamEvent
	err := DecodeStream(bytes.NewReader(frame), func(ev ir.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	sf, ok := ir.AsStreamFailure(err)
	if !ok {
		t.Fatalf("want StreamFailure, got %v", err)
	}
	if !strings.Contains(sf.Message, "overloaded") {
		t.Errorf("message = %q", sf.Message)
	}
	if sf.Type != "ModelStreamErrorException" {
		t.Errorf("type = %q", sf.Type)
	}
	if len(events) != 0 {
		t.Fatalf("want no events, got %+v", events)
	}
}
