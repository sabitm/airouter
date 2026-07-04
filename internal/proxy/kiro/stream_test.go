package kiro

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
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
